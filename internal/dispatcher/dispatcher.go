// Package dispatcher 是黑板协议唯一写入者(docs/03 §3):
//
//	一切"状态变更"的仲裁点:Worker 成果回写、黑板更新、漏洞账本。
//	Agent 只返回结构化 JSON,Dispatcher 负责落库。
//
// 关键语义:
//   - seq 冲突:ReportFindings 携带 worker 读取时的 asOfSeq,黑板已前进 → ErrConflict
//   - 幂等:同内容 fact 重复上报 → NO_CHANGE(不产生重复)
//   - UpdateFact 保守四档:no_change > update > delete > add
package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"scopeforge/internal/blackboard"
	"scopeforge/internal/coverage"
	"scopeforge/internal/cwe"
	"scopeforge/internal/event"
)

// ErrConflict 是黑板 seq 冲突(他人已写入,需合并重报)。
var ErrConflict = errors.New("dispatcher: blackboard seq conflict, re-report after merge")

// Finding 是 Worker 的一项成果(docs/07 §6.1 + phase2/03 §1.1 混合结构化)。
// 结构化字段(cwe/asset/endpoint/severity)全部可选——能字段化的获得
// 确定性去重键,不能字段化的退回自由文本(幂等键轨 2)。
type Finding struct {
	Prefix      string  `json:"prefix"`
	Text        string  `json:"text"`
	Weight      float64 `json:"weight"`
	EvidenceRef string  `json:"evidence_ref"`
	// phase2 结构化键:
	CWE      string `json:"cwe,omitempty"`
	Asset    string `json:"asset,omitempty"`
	Endpoint string `json:"endpoint,omitempty"`
	Severity string `json:"severity,omitempty"`
	// AuthGained 声明"已获得登录态/有效凭据"(agent 实际执行登录后的判断,
	// 含认证绕过/万能密码/会话劫持等非明文凭据场景——文本无法可靠表达,
	// 故用结构化布尔;服务端据此重开"缺登录态"跳过的方向)。
	AuthGained bool `json:"auth_gained,omitempty"`
	// Privilege 声明获得的能力/权限形态(2.23,breach 能力维度通用通道):
	// "shell" / "domain_user" / "authenticated" 等,可与 auth_gained 并存
	// (能力累加合并);服务端据此在状态图上解锁转移候选——攻击路径不参与
	// 映射,能力声明是唯一入口(路径无限,能力有界)。
	Privilege string `json:"privilege,omitempty"`
}

// IntentIn 是 Worker 回报的新意图(docs/07 §6.1 + phase2/03 §1.3)。
type IntentIn struct {
	Text     string  `json:"text"`
	Weight   float64 `json:"weight"`
	Target   string  `json:"target,omitempty"`
	Approach string  `json:"approach,omitempty"`
}

// AttackSurfaceItem 是 Scout 输出的攻击面条目(docs/phase2/03 §2.2)。
type AttackSurfaceItem struct {
	Asset    string   `json:"asset"`    // 域名/IP(归一化后)
	Endpoint string   `json:"endpoint"` // 路径
	Params   []string `json:"params,omitempty"`
	Auth     string   `json:"auth,omitempty"`
	Tech     string   `json:"tech,omitempty"`
	Notes    string   `json:"notes,omitempty"`
}

// AttackSurfaceList 兼容 attack_surface 的数组与单对象两种输出。
// 实测:Scout 常把单个端点输出为对象而非单元素数组,严格数组反序列化
// 会使整个 Worker 契约 parse 失败(unparseable),攻击面矩阵完全不落地。
type AttackSurfaceList []AttackSurfaceItem

// UnmarshalJSON 先按数组解析,失败回退单对象(包装为单元素数组)。
func (l *AttackSurfaceList) UnmarshalJSON(b []byte) error {
	var arr []AttackSurfaceItem
	if err := json.Unmarshal(b, &arr); err == nil {
		*l = arr
		return nil
	}
	var one AttackSurfaceItem
	if err := json.Unmarshal(b, &one); err == nil {
		*l = AttackSurfaceList{one}
		return nil
	}
	return fmt.Errorf("attack_surface: 期望数组或对象")
}

// UnmarshalJSON 容错:真实 LLM 对 params 的输出格式多变——字符串("q")、
// 数组(["q"])、对象({"q":"描述"})(阶段 2.21 实测三种都出现过),
// 一律宽容解析,防整条契约丢弃重试。
func (a *AttackSurfaceItem) UnmarshalJSON(data []byte) error {
	type alias AttackSurfaceItem // 防递归
	var raw struct {
		alias
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*a = AttackSurfaceItem(raw.alias)
	switch {
	case len(raw.Params) == 0 || string(raw.Params) == "null":
		// 无 params 字段
	default:
		a.Params = tolerantParams(raw.Params)
	}
	return nil
}

// tolerantParams 把任意 JSON 值提取为参数名列表:
// "q" → [q]; ["q","limit"] → 逐元素; {"q":"描述"} → 取 keys;
// 数字/布尔 → 转字符串;无法解析 → 空(不丢弃契约)。
func tolerantParams(raw json.RawMessage) []string {
	var arr []string
	if err := json.Unmarshal(raw, &arr); err == nil {
		return arr
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []string{s}
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err == nil {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		return keys
	}
	var v any
	if err := json.Unmarshal(raw, &v); err == nil {
		if num, ok := v.(float64); ok {
			return []string{fmt.Sprintf("%v", num)}
		}
		if b, ok := v.(bool); ok {
			return []string{fmt.Sprintf("%v", b)}
		}
	}
	return nil
}

// Handoff 是派活载荷(docs/03 §2.4:≤900 字符)。
type Handoff struct {
	ChallengeID  string
	IntentID     int64
	Phase        string   // operator 阶段(recon/explore/reason);synthesizer 为空
	SnapshotText string   // 黑板快照 YAML 增量文本
	IntentText   string   // 当前 intent 文本(独立槽位;不占已完成事项,不承受契约截断)
	Forbidden    []string // 禁止重复项
	// phase2 breach:关联的状态转移边(派活前固定,构造期写入,无运行期竞争)
	EdgeID string
}

// Steer 是 Observer 纠偏通道(docs/03 §4.3)。
type Steer struct {
	TargetWorker string `json:"target_worker"`
	Message      string `json:"message"`
	Priority     string `json:"priority"` // high | medium | low
}

// Mutation 是黑板维护操作(保守四档)。
type Mutation struct {
	Action  string  `json:"action"` // no_change | update | delete | add
	FactID  int64   `json:"fact_id"`
	NewText string  `json:"new_text"`
	Weight  float64 `json:"weight"`
}

// SchedulerHooks 是 Dispatcher 对调度内核的委托接口(解耦)。
type SchedulerHooks interface {
	Launch(ctx context.Context, workerType string, handoff Handoff) (workerID string, err error)
	Abort(ctx context.Context, workerID string, reason string) error
	Steer(ctx context.Context, workerID string, message string, priority string) error
}

// Dispatcher 是唯一写入者。
type Dispatcher struct {
	board    *blackboard.Blackboard
	sink     event.Sink
	mu       sync.Mutex
	hooks    map[string]SchedulerHooks // challengeID → 调度内核(默认键 "" 兼容单任务)
	coverage *coverage.Matrix          // phase2 覆盖矩阵(nil = 不启用,向后兼容)
}

// New 构建 Dispatcher。
func New(board *blackboard.Blackboard, sink event.Sink, hooks SchedulerHooks) *Dispatcher {
	if sink == nil {
		sink = event.Discard
	}
	d := &Dispatcher{board: board, sink: sink, hooks: map[string]SchedulerHooks{}}
	if hooks != nil {
		d.hooks[""] = hooks
	}
	return d
}

// SetCoverage 注入覆盖矩阵(phase2 切片 3;nil 可随时清空关闭)。
func (d *Dispatcher) SetCoverage(m *coverage.Matrix) { d.coverage = m }

// SetHooks 注入调度内核委托(默认键;单任务场景兼容)。
func (d *Dispatcher) SetHooks(hooks SchedulerHooks) { d.RegisterHooks("", hooks) }

// RegisterHooks 按 challengeID 注册调度内核委托(循环依赖解耦)。
// 并发多任务(serve 多 run)下各任务路由到自己的调度器,防 hooks 覆盖串台。
func (d *Dispatcher) RegisterHooks(challengeID string, hooks SchedulerHooks) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.hooks[challengeID] = hooks
}

// UnregisterHooks 任务结束时注销(goroutine 防泄漏)。
func (d *Dispatcher) UnregisterHooks(challengeID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.hooks, challengeID)
}

// hooksFor 按 challengeID 路由,回退默认键(单任务/旧测试)。
func (d *Dispatcher) hooksFor(challengeID string) SchedulerHooks {
	d.mu.Lock()
	defer d.mu.Unlock()
	if h, ok := d.hooks[challengeID]; ok {
		return h
	}
	return d.hooks[""]
}

// ------------------------------------------------------------------ Worker 成果回写

// ReportFindings 回写 findings(唯一入口,携带 asOfSeq)。
// seq 冲突返回 ErrConflict(调用方合并后重报);幂等重复静默跳过。
// 冲突检测与写入同事务(防并发穿透)。
// phase2(03 §1.1):结构化字段归一化——asset/endpoint 归一化,
// cwe 白名单校验(错值拒字段不拒整条:置空退回逐字幂等,事件层提示)。
func (d *Dispatcher) ReportFindings(ctx context.Context, workerID, challengeID string, asOfSeq int64, findings []Finding) error {
	if len(findings) == 0 {
		_ = d.board.WorkerHeartbeat(workerID, false)
		return nil
	}
	items := make([]blackboard.FactIn, 0, len(findings))
	hasCorrect := false
	for _, f := range findings {
		if f.Prefix == "" {
			f.Prefix = blackboard.PrefixObs
		}
		state := blackboard.StateConfirmed
		if f.Prefix == blackboard.PrefixHyp {
			state = blackboard.StateCandidate
		}
		// 结构化归一化(03 §1.1/§1.2)
		st, hint := normalizeFinding(f)
		if hint != "" {
			d.sink.Emit(event.Event{Kind: event.KindFinding, ChallengeID: challengeID,
				Payload: map[string]any{"worker": workerID, "normalize_hint": hint, "text": f.Text}})
		}
		items = append(items, blackboard.FactIn{
			Prefix: f.Prefix, Text: f.Text, Weight: f.Weight, State: state,
			CreatedBy: workerID, EvidenceRef: f.EvidenceRef,
			CWE: st.CWE, Asset: st.Asset, Endpoint: st.Endpoint, Severity: st.Severity,
		})
		if f.Prefix == blackboard.PrefixFlag && f.Weight >= 1.0 {
			hasCorrect = true
		}
	}
	_, err := d.board.AddFactsAtomically(challengeID, asOfSeq, items)
	if err != nil {
		if errors.Is(err, blackboard.ErrSeqConflict) {
			return ErrConflict
		}
		return err
	}
	for _, f := range findings {
		if f.Prefix == "" {
			f.Prefix = blackboard.PrefixObs
		}
		// 事件层记录归一化后的结构化值(与库内一致),原始值差异见 normalize_hint
		st, _ := normalizeFinding(f)
		d.sink.Emit(event.Event{Kind: event.KindFinding, ChallengeID: challengeID,
			Payload: map[string]any{"worker": workerID, "prefix": f.Prefix, "text": f.Text, "weight": f.Weight,
				"cwe": st.CWE, "asset": st.Asset, "endpoint": st.Endpoint, "severity": st.Severity}})
	}
	_ = d.board.WorkerHeartbeat(workerID, hasCorrect)
	return nil
}

// normalizeFinding 结构化字段归一化(03 §1.1):
//   - cwe:白名单校验(NormalizeCWE),不合法 → 置空 + 提示(拒字段不拒整条)
//   - asset/endpoint:归一化(小写去 www./去 query 尾部斜杠)
//   - severity:仅接受 critical|high|medium|low|info,否则置空
//
// 纪律:拒字段不拒整条——cwe 错值只丢 cwe,不丢 finding(退回幂等键轨 2)。
func normalizeFinding(f Finding) (blackboard.FactIn, string) {
	st := blackboard.FactIn{Severity: f.Severity}
	if f.Asset != "" {
		st.Asset = cwe.NormalizeAsset(f.Asset)
	}
	if f.Endpoint != "" {
		st.Endpoint = cwe.NormalizeEndpoint(f.Endpoint)
	}
	hint := ""
	if f.CWE != "" {
		if norm, ok := cwe.NormalizeCWE(f.CWE); ok {
			st.CWE = norm
		} else {
			hint = fmt.Sprintf("cwe %q 不在白名单(参考 skills/cwe-reference),已置空退回逐字去重", f.CWE)
		}
	}
	switch f.Severity {
	case "critical", "high", "medium", "low", "info":
	default:
		st.Severity = ""
	}
	return st, hint
}

// ReportIntent 回写新意图(带权重 + 可选 target/approach 结构化键)。
// target 归一化(NormalizeEndpoint:去 query/尾部斜杠),与 facts 键同规,
// 保证 "/login" 与 "/login/" 防抖一致。
func (d *Dispatcher) ReportIntent(ctx context.Context, workerID, challengeID string, it IntentIn) error {
	if it.Weight <= 0 {
		it.Weight = 0.5
	}
	if it.Target != "" {
		it.Target = cwe.NormalizeEndpoint(it.Target)
	}
	_, err := d.board.AddIntent(challengeID, it.Text, it.Weight, workerID,
		blackboard.IntentIn{Target: it.Target, Approach: it.Approach})
	if err != nil && !errors.Is(err, blackboard.ErrNoChange) {
		return err
	}
	_ = d.board.WorkerHeartbeat(workerID, false)
	return nil
}

// ReportDeadEnd 回写死路(防止其他 Worker 重复)。
func (d *Dispatcher) ReportDeadEnd(ctx context.Context, workerID, challengeID, text, evidenceRef string) error {
	_, err := d.board.AddFact(challengeID, blackboard.PrefixDead, text, 0.8, blackboard.StateConfirmed, workerID, evidenceRef)
	if err != nil && !errors.Is(err, blackboard.ErrNoChange) {
		return err
	}
	_ = d.board.WorkerHeartbeat(workerID, false)
	return nil
}

// ReportVulnerability 写入漏洞账本(docs/phase2/02 §1,唯一入口)。
// 只增不改:产生一条 submitted 记录;回执状态经 UpdateVulnerabilityReceipt 推进
// (阶段 2.9 定案:不做平台对接,提交语义由 skill 卡承载,回执由人工/外部确认)。
// coverage/breach 形态下 accepted 不触发终止(02 §2.2)。
// 去重:同 challenge 已存在归一化键 (cwe, asset, endpoint) 一致的记录
// (false_positive/rejected 除外——方向仍可重试)时不再新增,返回该记录并
// 以 duplicate 状态提示 worker(实测:同一漏洞被重复提交 5 次,账本噪音)。
func (d *Dispatcher) ReportVulnerability(ctx context.Context, workerID, challengeID string, v blackboard.VulnerabilityIn) (*blackboard.Vulnerability, error) {
	if dup, err := d.duplicateVulnerability(challengeID, v); err == nil && dup != nil {
		_ = d.board.WorkerHeartbeat(workerID, false)
		return dup, nil
	}
	entry, err := d.board.AddVulnerability(challengeID, v)
	if err != nil {
		return nil, err
	}
	d.sink.Emit(event.Event{Kind: event.KindFinding, ChallengeID: challengeID,
		Payload: map[string]any{"worker": workerID, "ledger": true, "vuln_id": entry.ID,
			"cwe": entry.CWE, "asset": entry.Asset, "endpoint": entry.Endpoint,
			"severity": entry.Severity, "title": entry.Title, "status": entry.Status}})
	_ = d.board.WorkerHeartbeat(workerID, false)
	return entry, nil
}

// duplicateVulnerability 按归一化键查重(cwe 归一化 + asset/endpoint 归一化)。
// 命中返回旧记录(Status 置 duplicate 供工具回显提示),未命中返回 nil。
func (d *Dispatcher) duplicateVulnerability(challengeID string, v blackboard.VulnerabilityIn) (*blackboard.Vulnerability, error) {
	vs, err := d.board.Vulnerabilities(challengeID)
	if err != nil {
		return nil, err
	}
	asset := cwe.NormalizeAsset(v.Asset)
	endpoint := cwe.NormalizeEndpoint(v.Endpoint)
	for i := range vs {
		old := &vs[i]
		if old.Status == blackboard.LedgerFalsePositive || old.Status == blackboard.LedgerRejected {
			continue // 误报/拒绝:方向可重试,不算重复
		}
		// cwe 双方均能归一化 → 按规范形式比较(容忍 cwe-89/CWE89/89 变体);
		// 任一方无法归一化 → 退回精确比较
		normOld, okOld := cwe.NormalizeCWE(old.CWE)
		normNew, okNew := cwe.NormalizeCWE(v.CWE)
		if okOld && okNew {
			if normOld != normNew {
				continue
			}
		} else if old.CWE != v.CWE {
			continue
		}
		if asset != "" && cwe.NormalizeAsset(old.Asset) != asset {
			continue
		}
		if endpoint != "" && cwe.NormalizeEndpoint(old.Endpoint) != endpoint {
			continue
		}
		dup := *old
		dup.Status = blackboard.LedgerDuplicate
		return &dup, nil
	}
	return nil, nil
}

// UpdateVulnerabilityReceipt 应用平台回执(02 §1.3 提交纪律)。
// accepted → 覆盖矩阵该格子 confirmed(03 §3.1;矩阵启用时)。
func (d *Dispatcher) UpdateVulnerabilityReceipt(ctx context.Context, workerID, challengeID string, vulnID int64, status, platformRef string) error {
	if err := d.board.UpdateVulnerabilityStatus(vulnID, status, platformRef); err != nil {
		return err
	}
	// 账本 → 覆盖矩阵(accepted 标记格子已覆盖)
	if d.coverage != nil && status == blackboard.LedgerAccepted {
		if v, err := d.board.VulnerabilityByID(vulnID); err == nil && v != nil {
			if err := d.coverage.MarkConfirmed(challengeID, v.CWE, v.Asset, v.Endpoint); err != nil {
				d.sink.Emit(event.Event{Kind: event.KindError, ChallengeID: challengeID,
					Payload: map[string]any{"error": fmt.Sprintf("cov mark confirmed(vuln): %v", err)}})
			}
			if err := d.coverage.GenerateRelay(challengeID, v.CWE, v.Asset, v.Endpoint); err != nil {
				d.sink.Emit(event.Event{Kind: event.KindError, ChallengeID: challengeID,
					Payload: map[string]any{"error": fmt.Sprintf("cov gen relay(vuln): %v", err)}})
			}
		}
	}
	d.sink.Emit(event.Event{Kind: event.KindSubmission, ChallengeID: challengeID,
		Payload: map[string]any{"vuln_id": vulnID, "status": status, "platform_ref": platformRef}})
	_ = d.board.WorkerHeartbeat(workerID, false)
	return nil
}

// ReportAttackSurface 落地攻击面清单(docs/phase2/03 §2.3 展开规则):
//   - 每个条目建立覆盖矩阵 open 格子(cwe 初始"未指定",form 按端点推断)
//   - 每个条目展开为一个初始 intent(key=(asset, endpoint) 去重)
//   - 带参数端点:初始 intent 附带候选 CWE 接力格(注入/XSS/越权)
//
// 攻击面 N 个条目 → N 个方向,而非 1 个;同 key 去重由 EnsureOpen/AddIntent 双保险。
func (d *Dispatcher) ReportAttackSurface(ctx context.Context, workerID, challengeID string, items []AttackSurfaceItem) error {
	if len(items) == 0 {
		return nil
	}
	for _, it := range items {
		asset := cwe.NormalizeAsset(it.Asset)
		endpoint := cwe.NormalizeEndpoint(it.Endpoint)
		if asset == "" {
			continue
		}
		form := coverage.ClassifyForm(endpoint)
		if len(it.Params) > 0 {
			form = coverage.FormParam // 有参数 → 带参端点(候选注入类)
		}
		if d.coverage != nil {
			if err := d.coverage.EnsureOpen(challengeID, "", asset, endpoint, form); err != nil {
				return err
			}
			// 带参端点:初始接力候选格(注入/XSS/越权)
			if form == coverage.FormParam {
				for _, c := range coverage.CandidateCWEs(form) {
					if err := d.coverage.EnsureOpen(challengeID, c, asset, endpoint, form); err != nil {
						return err
					}
				}
			}
		}
		text := fmt.Sprintf("测试 %s(%s) 的注入/越权/XSS", endpoint, asset)
		if it.Notes != "" {
			text = fmt.Sprintf("测试 %s(%s): %s", endpoint, asset, it.Notes)
		}
		// 初始 intent:key=(asset,endpoint) 去重(AddIntent 逐字防抖 + target 键)
		if err := d.ReportIntent(ctx, workerID, challengeID, IntentIn{
			Text: text, Weight: 0.7, Target: endpoint, Approach: "probe",
		}); err != nil {
			return err
		}
	}
	d.sink.Emit(event.Event{Kind: event.KindFinding, ChallengeID: challengeID,
		Payload: map[string]any{"worker": workerID, "attack_surface": len(items)}})
	_ = d.board.WorkerHeartbeat(workerID, false)
	return nil
}

// ------------------------------------------------------------------ 调度命令

// Launch 启动 Worker(委托调度内核)。
func (d *Dispatcher) Launch(ctx context.Context, workerType string, handoff Handoff) (string, error) {
	h := d.hooksFor(handoff.ChallengeID)
	if h == nil {
		return "", errors.New("dispatcher: scheduler hooks not configured")
	}
	return h.Launch(ctx, workerType, handoff)
}

// Abort 中止 Worker。
// Abort 中止 Worker(无生产调用方;调度内核内部经 s.Abort 直调)。
// 多任务并发时按 challengeID 路由(workerID 不携带任务信息)。
func (d *Dispatcher) Abort(ctx context.Context, challengeID, workerID, reason string) error {
	h := d.hooksFor(challengeID)
	if h == nil {
		return errors.New("dispatcher: scheduler hooks not configured")
	}
	return h.Abort(ctx, workerID, reason)
}

// Steer 注入 Observer 纠偏消息(不打断工具执行,下一轮生效)。
// challengeID 用于路由到该任务的调度内核(并发多任务防串台)。
func (d *Dispatcher) Steer(ctx context.Context, challengeID, workerID string, steer Steer) error {
	h := d.hooksFor(challengeID)
	if h == nil {
		return errors.New("dispatcher: scheduler hooks not configured")
	}
	if steer.Message == "" {
		return nil
	}
	if err := h.Steer(ctx, workerID, steer.Message, steer.Priority); err != nil {
		return err
	}
	d.sink.Emit(event.Event{Kind: event.KindSteer, Payload: map[string]any{
		"target_worker": steer.TargetWorker, "message": steer.Message, "priority": steer.Priority}})
	return nil
}

// ------------------------------------------------------------------ 黑板维护

// UpdateFact 保守四档维护(Observer 专用, docs/03 §4.3)。
func (d *Dispatcher) UpdateFact(ctx context.Context, challengeID string, m Mutation) error {
	switch m.Action {
	case "no_change", "":
		return nil
	case "update":
		if m.FactID <= 0 {
			return errors.New("dispatcher: update requires fact_id")
		}
		return d.board.SupersedeFact(challengeID, m.FactID, m.NewText, m.Weight, false, "observer")
	case "delete":
		if m.FactID <= 0 {
			return errors.New("dispatcher: delete requires fact_id")
		}
		return d.board.SupersedeFact(challengeID, m.FactID, "", 0, true, "observer")
	case "add":
		_, err := d.board.AddFact(challengeID, blackboard.PrefixObs, m.NewText, m.Weight, blackboard.StateConfirmed, "observer", "")
		if err != nil && !errors.Is(err, blackboard.ErrNoChange) {
			return err
		}
		return nil
	default:
		return fmt.Errorf("dispatcher: unknown mutation action %q", m.Action)
	}
}

// AcquireLease 获取互斥租约(reason 读图 / 提交互斥)。
func (d *Dispatcher) AcquireLease(ctx context.Context, resource, holder string, ttl time.Duration) (bool, error) {
	return d.board.AcquireLease(resource, holder, ttl)
}

// ReleaseLease 释放租约。
func (d *Dispatcher) ReleaseLease(ctx context.Context, resource, holder string) error {
	return d.board.ReleaseLease(resource, holder)
}

// CoverageEvidence 是穷尽声明的方向证据(docs/phase2/02 §2.4)。
type CoverageEvidence struct {
	Direction string   `json:"direction"` // 如 CWE-89@/login
	Tried     []string `json:"tried"`     // 已尝试方法(error_based/union/boolean_blind...)
	Excluded  string   `json:"excluded"`  // 排除理由(参数全参数化,无注入点)
}

// CoverageEvidenceList 兼容 coverage_evidence 的数组与单对象两种输出。
// 实测:模型把 coverage_evidence 输出为对象时,严格数组反序列化使整个契约
// 解析失败(unparseable)→ conclude worker 反复重试,任务无法收尾。
type CoverageEvidenceList []CoverageEvidence

// UnmarshalJSON 先按数组解析,失败回退单对象(包装为单元素数组)。
func (l *CoverageEvidenceList) UnmarshalJSON(b []byte) error {
	var arr []CoverageEvidence
	if err := json.Unmarshal(b, &arr); err == nil {
		*l = arr
		return nil
	}
	var one CoverageEvidence
	if err := json.Unmarshal(b, &one); err == nil {
		*l = CoverageEvidenceList{one}
		return nil
	}
	return fmt.Errorf("coverage_evidence: 期望数组或对象")
}

// WorkerContract 是 Worker 输出契约(docs/07 §6.1 + phase2/02 §2.4 穷尽声明)。
type WorkerContract struct {
	Accepted   bool       `json:"accepted"`
	Findings   []Finding  `json:"findings"`
	NewIntents []IntentIn `json:"new_intents"`
	DeadEnds   []string   `json:"dead_ends"`
	StopReason string     `json:"stop_reason"` // exhausted | intent_done | conclude
	// phase2 攻击面清单(Scout 输出,03 §2.2;兼容数组与单对象):
	AttackSurface AttackSurfaceList `json:"attack_surface"`
	// phase2 S6 穷尽声明(conclude worker 输出):
	Exhausted        bool                 `json:"exhausted"`         // 声明"无更多可探索"
	CoverageEvidence CoverageEvidenceList `json:"coverage_evidence"` // 带证据(空 = 不采信;兼容单对象)
	RemainingRisks   []string             `json:"remaining_risks"`   // 已知未覆盖(转 intents 或进报告)
}

// ParseContract 解析 Worker 输出契约(失败返回错误)。
func ParseContract(text string) (*WorkerContract, error) {
	trimmed := trimJSON(text)
	if trimmed == "" {
		return nil, errors.New("dispatcher: no JSON block in worker output")
	}
	var c WorkerContract
	if err := json.Unmarshal([]byte(trimmed), &c); err != nil {
		return nil, fmt.Errorf("dispatcher: contract parse: %w", err)
	}
	return &c, nil
}

// trimJSON 提取第一个合法 JSON 块(去代码围栏与前后文本)。
func trimJSON(text string) string {
	// 尝试整体
	if json.Valid([]byte(text)) {
		return text
	}
	// 代码围栏 ```json ... ```
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
	// 扫描大括号配对(支持嵌套),取第一段合法 JSON
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
