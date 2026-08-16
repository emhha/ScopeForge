// Package observer 是旁路 Observer(docs/03 §4):
//
//	异步审查最近轮次 → 维护黑板质量 → 低频 steer 纠偏。
//	不执行工具、不提交 flag、不在执行热路径上(A3)。
//
// 纪律(docs/03 §4.4):
//   - 优先级硬编码:NO_CHANGE > update > delete > add(对抗行动偏差)
//   - 解释不能单独作为证据
//   - 板体积硬上限:超限先压缩再添加
//   - Observer 计入成本账本(便宜模型)
package observer

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"scopeforge/internal/blackboard"
	"scopeforge/internal/constraint"
	"scopeforge/internal/conversation"
	"scopeforge/internal/coverage"
	"scopeforge/internal/dispatcher"
	"scopeforge/internal/event"
	"scopeforge/internal/executor"
	"scopeforge/internal/reasonix/provider"
	"scopeforge/internal/reasonix/tool"
	"scopeforge/internal/store"
)

// Config 是 Observer 配置。
type Config struct {
	EveryNTurns int // 每 N 轮审查一次,默认 3
	MaxTurns    int // 审查会话最大轮数,默认 10
	WindowLimit int // 单次审查的事件窗口,默认 200
}

// DefaultConfig 返回默认配置。
func DefaultConfig() Config {
	return Config{EveryNTurns: 3, MaxTurns: 10, WindowLimit: 200}
}

// Options 是构造参数。
type Options struct {
	Cfg        Config
	Board      *blackboard.Blackboard
	Dispatcher *dispatcher.Dispatcher
	Provider   provider.Provider
	Sink       event.Sink
	Ledger     *constraint.CostLedger
	Pricing    *provider.Pricing
	CompactCfg conversation.CompactConfig
	DB         *store.DB
	// ActiveWorkers 返回当前可 steer 的 worker id(空 = 无)。
	ActiveWorkers func() []string
	// phase2 覆盖矩阵(03 §3.5;nil = 不启用覆盖审查)
	Coverage *coverage.Matrix
	// TaskDescription 任务描述:Observer 依此理解用户意图
	// (聚焦测试 vs SRC 全面扫描 vs 企业渗透拿靶标),
	// 自主判断应建议收尾/扩展/深入,而非硬编码规则。
	TaskDescription string
	// GoalShape 任务形态(coverage/breach),Observer 据此调整引导策略。
	GoalShape string
	// PromptDir 提示词物化根目录(空 = SCOPEFORGE_HOME > ~/.scopeforge)。
	PromptDir string
}

// Review 是审查输出契约(docs/03 §4.3 / docs/07 §6.2)。
type Review struct {
	BoardUpdates []BoardUpdate `json:"board_updates"`
	Steer        *SteerMsg     `json:"steer"`
	// phase2 覆盖审查(03 §3.5):建议补探索方向
	CoverageSuggestions []CoverageSuggestion `json:"coverage_suggestions"`
}

// CoverageSuggestion 是覆盖矩阵补格建议(经 Dispatcher 转新 intent)。
type CoverageSuggestion struct {
	Direction string `json:"direction"` // 如 CWE-22@/download
	Reason    string `json:"reason"`    // 如 "矩阵中唯一未探索的高危格子"
}

// BoardUpdate 是板维护建议(保守四档)。
type BoardUpdate struct {
	Action  string  `json:"action"` // no_change | update | delete | add
	FactID  int64   `json:"fact_id"`
	NewText string  `json:"new_text"`
	Weight  float64 `json:"weight"`
}

// SteerMsg 是纠偏消息。
type SteerMsg struct {
	TargetWorker string `json:"target_worker"`
	Message      string `json:"message"`
	Priority     string `json:"priority"`
}

// Observer 是旁路审查者。
type Observer struct {
	cfg      Config
	board    *blackboard.Blackboard
	disp     *dispatcher.Dispatcher
	provider provider.Provider
	sink     event.Sink
	ledger   *constraint.CostLedger
	pricing  *provider.Pricing
	compact  conversation.CompactConfig
	db       *store.DB
	active   func() []string
	cov      *coverage.Matrix // phase2 覆盖矩阵(03 §3.5)
	taskDesc string           // 任务描述(Observer 上下文注入)
	goalShape string          // 任务形态(coverage/breach)
	promptDir string          // 提示词物化根(空 = SCOPEFORGE_HOME > ~/.scopeforge)

	mu              sync.Mutex
	lastReviewedSeq int64 // 上次审查后的事件 seq(增量窗口)
	turnCount       int   // 已见 turn_start 计数
}

// New 构建 Observer。
func New(opts Options) *Observer {
	cfg := opts.Cfg
	if cfg.EveryNTurns <= 0 {
		cfg.EveryNTurns = 3
	}
	if cfg.MaxTurns <= 0 {
		cfg.MaxTurns = 10
	}
	if cfg.WindowLimit <= 0 {
		cfg.WindowLimit = 200
	}
	if opts.Sink == nil {
		opts.Sink = event.Discard
	}
	return &Observer{
		cfg: cfg, board: opts.Board, disp: opts.Dispatcher,
		provider: opts.Provider, sink: opts.Sink, ledger: opts.Ledger,
		pricing: opts.Pricing, compact: opts.CompactCfg, db: opts.DB,
		active: opts.ActiveWorkers, cov: opts.Coverage,
		taskDesc: opts.TaskDescription, goalShape: opts.GoalShape,
		promptDir: opts.PromptDir,
	}
}

// observerGate 是 Observer 会话的权限门(空注册表 + yolo 已无工具可调)。
var observerGate, _ = executor.NewGate(executor.ModeYolo, nil, nil)

// Loop 运行审查循环(docs/03 §4.2):每 N 轮审查一次,ctx 取消即停。
func (o *Observer) Loop(ctx context.Context, challengeID string) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			due, err := o.due(challengeID)
			if err != nil {
				continue
			}
			if due {
				_ = o.ReviewOnce(ctx, challengeID)
			}
		}
	}
}

// due 判断是否到达审查节奏(基于 turn_start 事件计数)。
func (o *Observer) due(challengeID string) (bool, error) {
	turns, seq, err := countTurns(o.db, challengeID, o.lastReviewedSeq)
	if err != nil {
		return false, err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if turns == o.turnCount {
		return false, nil // 无新轮次
	}
	o.turnCount = turns
	o.lastReviewedSeq = seq
	return turns%o.cfg.EveryNTurns == 0, nil
}

// ReviewOnce 执行一次审查:读事件 → LLM 审查 → 保守四档应用 + steer。
func (o *Observer) ReviewOnce(ctx context.Context, challengeID string) error {
	// 1. 事件窗口
	events, err := event.Query(o.db, "", o.lastReviewedSeq, 0, o.cfg.WindowLimit)
	if err != nil {
		return fmt.Errorf("observer: events: %w", err)
	}
	var relevant []event.Event
	for _, e := range events {
		if e.ChallengeID == challengeID {
			relevant = append(relevant, e)
		}
	}
	// 2. 黑板快照
	snap, err := o.board.SnapshotForWorker(challengeID)
	if err != nil {
		return err
	}
	// 3. 审查会话(独立 LLM,便宜模型;无工具注册表 = 不执行工具)
	prompt := o.buildPrompt(challengeID, relevant, snap)
	// 会话 id 固定 "observer":审查事件(turn_start/思考增量/工具拒绝等)统一归口
	// 该 SessionID,看板 observer 卡片(created_by="observer")按 SessionID 过滤
	// 即可见完整审查时间线(此前 obs-<nano> 每次不同,永远匹配不上)。
	sess := conversation.New("observer", conversation.KindObserver)
	sess.ChallengeID = challengeID
	sess.Provider = o.provider.Name()
	sess.Add(provider.Message{Role: provider.RoleUser, Content: prompt})

	opts := &executor.Options{
		Provider: o.provider,
		Registry: tool.NewRegistry(), // 空注册表:Observer 不执行工具
		Session:  sess,
		Store:    nil,
		MaxTurns: o.cfg.MaxTurns,
		Gate:     observerGate,
		Sink: event.FuncSink(func(e event.Event) {
			// 补 ChallengeID:executor 内部事件不带 challenge_id,
			// 前端按任务分桶会丢 observer 事件(与 runWorker 同规)。
			if e.ChallengeID == "" {
				e.ChallengeID = challengeID
			}
			o.sink.Emit(e)
		}),
		CompactCfg: o.compact,
		Ledger:     o.ledger,
		Role:       "observer",
		Pricing:    o.pricing,
	}
	res, err := executor.Run(ctx, opts)
	if err != nil {
		return fmt.Errorf("observer: review: %w", err)
	}
	// 4. 解析契约
	review, err := parseReview(res.FinalText)
	if err != nil {
		o.sink.Emit(event.Event{Kind: event.KindError, ChallengeID: challengeID, SessionID: "observer",
			Payload: map[string]any{"error": fmt.Sprintf("observer: contract: %v", err)}})
		return err
	}
	// 5. 应用(保守四档经 Dispatcher)
	o.apply(ctx, challengeID, review)
	o.sink.Emit(event.Event{Kind: event.KindCheckpoint, ChallengeID: challengeID, SessionID: "observer",
		Payload: map[string]any{"action": "observer_review", "updates": len(review.BoardUpdates), "steer": review.Steer != nil}})
	return nil
}

// apply 应用审查结果。
func (o *Observer) apply(ctx context.Context, challengeID string, review *Review) {
	for _, u := range review.BoardUpdates {
		m := dispatcher.Mutation{Action: u.Action, FactID: u.FactID, NewText: u.NewText, Weight: u.Weight}
		if err := o.disp.UpdateFact(ctx, challengeID, m); err != nil {
			o.sink.Emit(event.Event{Kind: event.KindError, ChallengeID: challengeID, SessionID: "observer",
				Payload: map[string]any{"error": fmt.Sprintf("observer: update fact: %v", err)}})
		}
	}
	// phase2 覆盖审查建议(03 §3.5):direction 如 CWE-22@/download → 新 intent
	for _, s := range review.CoverageSuggestions {
		if s.Direction == "" {
			continue
		}
		text := fmt.Sprintf("Observer 建议:%s(%s)", s.Direction, s.Reason)
		if err := o.disp.ReportIntent(ctx, "observer", challengeID,
			dispatcher.IntentIn{Text: text, Weight: 0.6, Target: s.Direction, Approach: "observer"}); err != nil {
			o.sink.Emit(event.Event{Kind: event.KindError, ChallengeID: challengeID, SessionID: "observer",
				Payload: map[string]any{"error": fmt.Sprintf("observer: coverage suggestion: %v", err)}})
		}
	}
	if review.Steer != nil && review.Steer.Message != "" {
		target := review.Steer.TargetWorker
		if target == "" {
			if ws := o.activeWorkers(); len(ws) > 0 {
				target = ws[0]
			}
		}
		if target == "" {
			return // 无活动 worker,steer 无处投递
		}
		_ = o.disp.Steer(ctx, challengeID, target, dispatcher.Steer{
			TargetWorker: target, Message: review.Steer.Message, Priority: review.Steer.Priority,
		})
	}
}

func (o *Observer) activeWorkers() []string {
	if o.active == nil {
		return nil
	}
	return o.active()
}

// observerPrompt 是 Observer 审查提示词 fallback(Go 常量;embed 优先)。
const observerPrompt = `你是旁路 Observer,不是 solver。你不执行工具、不提交 flag。
任务:审查最近轮次事件与黑板,输出板维护建议。
## 用户任务(权威输入)
{task_description}
任务形态: {goal_shape}
理解用户意图:若任务仅要求检测单一漏洞/端点 → 完成检测后应建议终止;
若任务要求尽可能多找漏洞(SRC) → 应建议探索新端点/新漏洞类型;
若任务是渗透拿靶标(breach) → 已有立足点时应建议深入内网而非扩展外网。

优先级硬编码:NO_CHANGE > update existing > delete superseded > add new。
纪律:
- 解释性内容(模型自述)不能单独成为证据
- 黑板体积超限时先压缩再添加(facts ≤10 / intents ≤8)
- 重复/低质量事实应 update 或 delete,不要 add

## 黑板快照(challenge {challenge_id})
facts:
{snapshot_facts}
intents:
{snapshot_intents}

## 最近事件
{recent_events}

{coverage_section}

## 登录后攻击面审查
若最近事件或黑板显示已获得登录态/有效凭据(含认证绕过、万能密码、
会话劫持等,不限于明文密码形式),必须提出登录后攻击面扩展:
- 若覆盖矩阵存在 skipped(需登录态)格子 → 输出 coverage_suggestions 提出登录后重测(direction=该格子方向,reason 注明已获得登录态)
- 若发现已知但未覆盖的登录后方向 → 输出 coverage_suggestions(自由文本方向)或 steer 提示当前 worker 使用登录态

## 能力获得审查(breach 形态)
若最近事件或黑板显示已获得更高权限/能力(不限于明文形式),必须提出对应解锁方向:
- 获得 shell(WebShell/反弹 shell/RCE 落地/命令执行/任意文件写+执行等)→ 提出横向移动/提权/凭据收集方向
- 获得域用户(域凭据/票据/Kerberoast/委派利用等)→ 提出域枚举/委派攻击/密码喷洒方向
- 能力→转移候选映射:shell→{横向移动,提权,凭据收集};domain_user→{域枚举,委派攻击,密码喷洒};登录态→登录后攻击面(见上段)
- 输出形式:coverage_suggestions(自由文本方向)或 steer;此通道覆盖 explore 忘记在 finding 声明 privilege/auth_gained 的漏报

## 输出契约(严格 JSON)
{"board_updates":[{"action":"no_change|update|delete|add","fact_id":12,"new_text":"...","weight":0.9}],"steer":{"target_worker":"w-3","message":"...","priority":"high"},"coverage_suggestions":[{"direction":"CWE-22@/download","reason":"矩阵中唯一未探索的高危格子"}]}
- coverage_suggestions 可选:矩阵存在未探索格(尤其高危/interest 命中)或已获得登录态存在登录后方向时给出补探索建议,方向格式 CWE-N@/endpoint 或自由文本(如 '登录后测试 /admin')
无必要修改时输出 {"board_updates":[]} 或 {"board_updates":[{"action":"no_change"}]}。`

// promptDir 返回 Observer 提示词物化根目录(与 scheduler.scopeforgeDir 同规):
// SCOPEFORGE_HOME 优先,否则 ~/.scopeforge。
func promptDir() string {
	if d := os.Getenv("SCOPEFORGE_HOME"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".scopeforge")
}

// MaterializePrompt 把 Observer 审查提示词物化到 <promptDir>/prompts/observer.md,
// promptDir 为空时默认 SCOPEFORGE_HOME > ~/.scopeforge;仅当目标文件不存在时写入
// (用户改过的不覆盖)。
func MaterializePrompt(dir string) error {
	if dir == "" {
		dir = promptDir()
	}
	target := filepath.Join(dir, "prompts", "observer.md")
	if _, err := os.Stat(target); err == nil {
		return nil // 已存在,不覆盖
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	return os.WriteFile(target, []byte(observerPrompt), 0o644)
}

// buildPrompt 构造审查提示词(docs/07 §3.5)。
// 模板从 <promptDir>/prompts/observer.md 加载(Go 常量兜底),
// 运行时数据(snapshot/events/coverage)动态注入占位符。
func (o *Observer) buildPrompt(challengeID string, events []event.Event, snap *blackboard.Snapshot) string {
	tmpl := observerPrompt
	if o.promptDir != "" {
		if data, err := os.ReadFile(filepath.Join(o.promptDir, "prompts", "observer.md")); err == nil {
			tmpl = string(data)
		}
	} else if data, err := os.ReadFile(filepath.Join(promptDir(), "prompts", "observer.md")); err == nil {
		tmpl = string(data)
	}

	// 快照事实
	var factLines []string
	for _, f := range snap.Facts {
		factLines = append(factLines, fmt.Sprintf("- id=%d [%s] %s (weight %.2f, %s)", f.ID, f.Prefix, f.Text, f.Weight, f.State))
	}
	// 快照意图
	var intentLines []string
	for _, it := range snap.Intents {
		intentLines = append(intentLines, fmt.Sprintf("- id=%d %s (weight %.2f, %s)", it.ID, it.Text, it.Weight, it.State))
	}
	// 最近事件
	var eventLines []string
	for _, e := range events {
		payload, _ := json.Marshal(e.Payload)
		eventLines = append(eventLines, fmt.Sprintf("- [%s] %s", e.Kind, truncate(string(payload), 200)))
	}
	// 覆盖矩阵
	var covSection string
	if o.cov != nil {
		if cells, err := o.cov.Cells(challengeID); err == nil && len(cells) > 0 {
			var lines []string
			for _, c := range cells {
				extra := ""
				if c.SkipReason != "" {
					extra = " (" + c.SkipReason + ")"
				}
				lines = append(lines, fmt.Sprintf("- %s@%s%s [%s]%s", c.CWE, c.Asset, c.Endpoint, c.Status, extra))
			}
			covSection = "## 覆盖矩阵(phase2)\n" + strings.Join(lines, "\n")
		}
	}

	// 占位符替换
	tmpl = strings.ReplaceAll(tmpl, "{task_description}", o.taskDesc)
	tmpl = strings.ReplaceAll(tmpl, "{goal_shape}", o.goalShape)
	tmpl = strings.ReplaceAll(tmpl, "{challenge_id}", challengeID)
	tmpl = strings.ReplaceAll(tmpl, "{snapshot_facts}", strings.Join(factLines, "\n"))
	tmpl = strings.ReplaceAll(tmpl, "{snapshot_intents}", strings.Join(intentLines, "\n"))
	tmpl = strings.ReplaceAll(tmpl, "{recent_events}", strings.Join(eventLines, "\n"))
	tmpl = strings.ReplaceAll(tmpl, "{coverage_section}", covSection)
	return tmpl
}

// parseReview 解析审查契约(复用 JSON 块提取兜底)。
func parseReview(text string) (*Review, error) {
	trimmed := trimJSON(text)
	if trimmed == "" {
		return nil, fmt.Errorf("no JSON block")
	}
	var r Review
	if err := json.Unmarshal([]byte(trimmed), &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// trimJSON 提取第一段合法 JSON(与 dispatcher 同款兜底,独立实现避免包依赖)。
func trimJSON(text string) string {
	if json.Valid([]byte(text)) {
		return text
	}
	start := 0
	for _, marker := range []string{"```json", "```"} {
		if i := strings.Index(text, marker); i >= 0 {
			start = i + len(marker)
			break
		}
	}
	if end := strings.Index(text[start:], "```"); end >= 0 {
		return strings.TrimSpace(text[start : start+end])
	}
	depth := 0
	for i := 0; i < len(text); i++ {
		switch text[i] {
		case '{':
			if depth == 0 {
				start = i
			}
			depth++
		case '}':
			depth--
			if depth == 0 && i > start {
				cand := text[start : i+1]
				if json.Valid([]byte(cand)) {
					return cand
				}
			}
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// countTurns 统计 challenge 的 turn_start 事件数(增量窗口)。
func countTurns(db *store.DB, challengeID string, after int64) (int, int64, error) {
	var n int
	var maxSeq int64
	err := db.QueryRow(`SELECT COUNT(*), COALESCE(MAX(seq),0) FROM events WHERE challenge_id=? AND kind='turn_start' AND seq > ?`,
		challengeID, after).Scan(&n, &maxSeq)
	if err != nil {
		return 0, 0, err
	}
	return n, maxSeq, nil
}
