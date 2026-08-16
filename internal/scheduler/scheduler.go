// Package scheduler 是 30s 节拍调度内核(docs/03 §2):
//
//	只调度,不解题:每 tick 读黑板态势 → 确定性决策 → 经 Dispatcher 派活。
//	Worker 类型:operator(侦察/挖掘/分析三阶段合一,Phase 区分)/ synthesizer(收尾)。
//	stale 判定:无进展超时(默认 1h)才允许换向/停题。
package scheduler

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"scopeforge/internal/blackboard"
	"scopeforge/internal/breach"
	"scopeforge/internal/config"
	"scopeforge/internal/constraint"
	"scopeforge/internal/conversation"
	"scopeforge/internal/coverage"
	"scopeforge/internal/dispatcher"
	"scopeforge/internal/event"
	"scopeforge/internal/executor"
	"scopeforge/internal/guard"
	"scopeforge/internal/kb"
	"scopeforge/internal/reasonix/provider"
	"scopeforge/internal/reasonix/skill"
	"scopeforge/internal/reasonix/tool"
	"scopeforge/internal/report"
)

// Worker 类型常量(07 合并:Scout/Executor/Analyst 三阶段合一为 Operator)。
const (
	WorkerOperator    = "operator"    // 操作员:侦察(scout)/挖掘(executor)/分析(analyst)三阶段合一(Phase 区分)
	WorkerSynthesizer = "synthesizer" // 汇总者:收尾汇总,生成报告
)

// Config 是调度配置。
type Config struct {
	TickInterval    time.Duration  // 全局节拍,默认 30s
	StaleTimeout    time.Duration  // 无进展判定,默认 1h
	MaxConcurrency  int            // 并行 Worker 上限,默认 2
	ReasonLeaseTTL  time.Duration  // reason 互斥租约,默认 10m
	MinIntentWeight float64        // 派活的最低意图权重,默认 0.5
	MaxTicks        int            // 单题最大节拍数(时间预算),默认 120
	WorkerTurns     map[string]int // 各阶段最大轮数(键: recon/explore/reason/synthesizer)
	ReportDir       string         // 报告输出目录(默认 workdir/reports)
}

// DefaultConfig 返回默认配置。
func DefaultConfig() Config {
	return Config{
		TickInterval:    30 * time.Second,
		StaleTimeout:    time.Hour,
		MaxConcurrency:  2,
		ReasonLeaseTTL:  10 * time.Minute,
		MinIntentWeight: 0.5,
		MaxTicks:        120,
		WorkerTurns: map[string]int{
			string(PhaseRecon):   15,
			string(PhaseExplore): 30,
			string(PhaseReason):  15,
			WorkerSynthesizer:    10,
		},
	}
}

// Options 是 Scheduler 构造参数。
type Options struct {
	Cfg        Config
	Board      *blackboard.Blackboard
	Dispatcher *dispatcher.Dispatcher
	Hub        *constraint.TerminationHub
	Providers  map[string]provider.Provider
	RoleMap    map[string]string // phase → provider name(recon/explore/reason/synthesizer;空 = 默认)
	Registry   *tool.Registry    // worker 基础工具集(不含账本工具)
	// SkillStore skill 卡仓库(04 §3 层 3;nil = worker 无 run_skill/技能索引)。
	// 此前从未接入调度器:run_skill 只在集成测试注册、SkillIndex 无赋值方,
	// 提示词指路成死路(docs/phase2/11 §2.4)。
	SkillStore *skill.Store
	Gate       *executor.Gate
	Sink       event.Sink
	Ledger     *constraint.CostLedger
	Pricing    map[string]*provider.Pricing // provider name → 单价
	ConvStore  *conversation.Store
	CompactCfg conversation.CompactConfig
	WorkDir    string
	PromptDir  string // 提示词物化根目录(空 = SCOPEFORGE_HOME > ~/.scopeforge)
	// M2 能力层(docs/04;nil = 不启用)
	KB    *kb.Index   // 漏洞知识库(vulnerability_search)
	Guard *guard.Hook // 确定性安全 Hook
	// M3 观测与交互(docs/05;nil = 不启用)
	Reports *report.Generator // 报告生成(任务终止后自动生成,§5.3)
	// phase2 覆盖矩阵(切片 3;nil = 不启用,退回阶段一调度)
	Coverage *coverage.Matrix
	// phase2 breach 状态空间(切片 4;goal_shape=breach 时启用)
	Breach *breach.Space
	// phase2 任务描述注入(切片 5,04 §1):description 进提示词,
	// constraints.targets 进 Gate 白名单,skills 注入 skill 卡清单。
	TaskProfile config.TaskProfileConfig
}

// runningWorker 是内存中的 Worker 运行时状态。
type runningWorker struct {
	record    *blackboard.Worker
	session   *conversation.Session
	intentID  int64
	launchSeq int64 // 派活时的黑板 seq(asOfSeq)
	cancel    context.CancelFunc
	done      chan struct{}
	mu        sync.Mutex
	steer     []provider.Message // 待注入的 steer 消息队列
	// phase2 S6:conclude 穷尽声明是否被采信(exhausted=true 且 evidence 非空)。
	// 注意:仅 runWorker goroutine 访问(applyContract 写入,runWorker 尾部读取,
	// 同一 goroutine 顺序执行),Abort/tick 等其他路径不得读取,防引入 race。
	exhaustedOK bool
	// phase2 breach:关联的状态转移边(applyContract 确认/失败联动)
	edgeID string
	// phase2 07:Worker 运行阶段(Recon/Explore/Reason)——模板选择和未穷尽判定的依据
	phase Phase
}

// Result 是单题编排结果。
type Result struct {
	ChallengeID string
	Concluded   bool
	Reason      constraint.Reason
	Terminated  bool
	Facts       []blackboard.Fact
	Vulns       []blackboard.Vulnerability // phase2 漏洞账本(报告数据源)
	Turns       int
	CostUSD     float64
	ReportPath  string
}

// Scheduler 是调度内核。
type Scheduler struct {
	cfg       Config
	board     *blackboard.Blackboard
	disp      *dispatcher.Dispatcher
	hub       *constraint.TerminationHub
	providers map[string]provider.Provider
	roleMap   map[string]string
	registry  *tool.Registry
	gate      *executor.Gate
	sink      event.Sink
	ledger    *constraint.CostLedger
	pricing   map[string]*provider.Pricing
	compact   conversation.CompactConfig
	convSt    *conversation.Store
	workDir   string
	reportDir string
	promptDir string // 提示词物化根(空 = SCOPEFORGE_HOME > ~/.scopeforge)
	// M2 能力层
	kb    *kb.Index
	guard *guard.Hook
	// 层 3 skill 卡(04 §3):run_skill 注册 + 技能索引(一次性构建缓存)
	skills         *skill.Store
	skillIndexOnce sync.Once
	skillIndexStr  string
	// M3
	reports *report.Generator
	// phase2 覆盖矩阵
	cov *coverage.Matrix
	// phase2 breach 状态空间
	bspace *breach.Space
	// 06.15 任务控制:暂停/恢复(节拍级,worker 不打断)
	paused atomic.Bool

	mu               sync.Mutex
	running          map[string]*runningWorker
	tickCount        int
	concluding       bool
	concluded        bool
	termReason       constraint.Reason
	target           string                   // 目标描述(提示词用)
	taskProfile      config.TaskProfileConfig // 任务形态/描述/约束/skill 卡(切片 5)
	focusCached      *focusTarget             // 聚焦目标缓存(阶段 2.21;nil = 未启用)
	focusParsed      bool                     // focusCached 已解析(惰性)
	unparseable      map[string]int           // provider → 连续解析失败次数
	unExhaustedCount map[int64]int            // intentID → 未穷尽重试次数(03 §3.3 防空转)
	exhaustRejects   int                      // 穷尽声明连续被拒次数(达上限强制收尾,防空转)
	reasonDispatches int                      // reason worker 派发计数(达 maxReasonDispatches 让位收尾)
	traceID          string                   // 分布式追踪 ID(Phase 4 M4: tick 生成,worker 继承)
}

// New 构建调度器。
func New(opts Options) *Scheduler {
	cfg := opts.Cfg
	if cfg.TickInterval <= 0 {
		cfg.TickInterval = 30 * time.Second
	}
	if cfg.StaleTimeout <= 0 {
		cfg.StaleTimeout = time.Hour
	}
	if cfg.MaxConcurrency <= 0 {
		cfg.MaxConcurrency = 2
	}
	if cfg.ReasonLeaseTTL <= 0 {
		cfg.ReasonLeaseTTL = 10 * time.Minute
	}
	if cfg.MinIntentWeight <= 0 {
		cfg.MinIntentWeight = 0.5
	}
	if cfg.MaxTicks <= 0 {
		cfg.MaxTicks = 120
	}
	if cfg.WorkerTurns == nil {
		cfg.WorkerTurns = DefaultConfig().WorkerTurns
	}
	if opts.Sink == nil {
		opts.Sink = event.Discard
	}
	reportDir := opts.Cfg.ReportDir
	if reportDir == "" {
		reportDir = filepath.Join(opts.WorkDir, "reports")
	}
	return &Scheduler{
		cfg:              cfg,
		board:            opts.Board,
		disp:             opts.Dispatcher,
		hub:              opts.Hub,
		providers:        opts.Providers,
		roleMap:          opts.RoleMap,
		registry:         opts.Registry,
		gate:             opts.Gate,
		sink:             opts.Sink,
		ledger:           opts.Ledger,
		pricing:          opts.Pricing,
		compact:          opts.CompactCfg,
		convSt:           opts.ConvStore,
		workDir:          opts.WorkDir,
		reportDir:        reportDir,
		promptDir:        opts.PromptDir,
		running:          map[string]*runningWorker{},
		unparseable:      map[string]int{},
		unExhaustedCount: map[int64]int{},
		kb:               opts.KB,
		guard:            opts.Guard,
		skills:           opts.SkillStore,
		reports:          opts.Reports,
		cov:              opts.Coverage,
		bspace:           opts.Breach,
		taskProfile:      opts.TaskProfile,
	}
}

// ------------------------------------------------------------------ 主循环

// Run 运行单题编排直到三路 OR 终止,返回报告数据。
func (s *Scheduler) Run(ctx context.Context, challengeID string) (*Result, error) {
	// 1. 清理上次崩溃残留的 running worker(DB 即协议:黑板/attempts 不丢)
	orphans, err := s.board.Workers(challengeID, "running")
	if err != nil {
		return nil, err
	}
	for _, w := range orphans {
		if _, ok := s.running[w.ID]; !ok {
			s.logErr(challengeID, "orphan cleanup", s.board.MarkWorkerFinished(w.ID, "orphaned"))
			s.sink.Emit(event.Event{Kind: event.KindWorkerAbort, ChallengeID: challengeID,
				Payload: map[string]any{"worker": w.ID, "reason": "orphaned (crash resume)"}})
		}
	}

	// 2. 启动实例(获取目标);目标注入 Gate 白名单(ScopeGate 接线,docs/03 §6.3)
	// 切片 5(04 §1):目标来自 task_profile constraints.targets(配置声明),
	// 无平台对接;exclusions 与禁止项进任务描述提示词(层 2 纪律)。
	s.setTarget(challengeID)
	// Phase 4 M4: 分布式追踪——每次 Run 生成唯一 trace ID,贯穿全部 tick/worker 事件。
	s.traceID = fmt.Sprintf("trace-%s-%d", challengeID, time.Now().UnixNano())
	if s.gate != nil && len(s.taskProfile.Constraints.Targets) > 0 {
		if err := s.gate.AddTargets(s.taskProfile.Constraints.Targets); err != nil {
			s.sink.Emit(event.Event{Kind: event.KindError, ChallengeID: challengeID,
				Payload: map[string]any{"error": fmt.Sprintf("scope gate add targets: %v", err)}})
		}
	}
	if s.focus() != nil && s.cov != nil && s.hub != nil {
		s.hub.SetFocused(s.cov)
		if s.cfg.MaxConcurrency > 1 {
			s.cfg.MaxConcurrency = 1
		}
	}

	// 3. 节拍循环
	for {
		select {
		case <-ctx.Done():
			// 外部 stop/取消:立即回收运行中的 worker(防停止后仍产出事件),
			// 然后返回(Result.Reason 由调用方按 interrupted 语义处理)。
			s.abortRunning(ctx, challengeID)
			return s.buildResult(challengeID), ctx.Err()
		default:
		}
		s.tick(ctx, challengeID)
		if s.isConcluded() {
			break
		}
		select {
		case <-ctx.Done():
			s.abortRunning(ctx, challengeID)
			return s.buildResult(challengeID), ctx.Err()
		case <-time.After(s.cfg.TickInterval):
		}
	}
	// M3 §5.3:任务终止(三路 OR 任一)后自动生成正式报告(markdown)
	if s.reports != nil && s.isConcluded() {
		if rep, err := s.reports.Generate(ctx, challengeID, true); err == nil && rep.Path != "" {
			s.sink.Emit(event.Event{Kind: event.KindReport, ChallengeID: challengeID,
				Payload: map[string]any{"challenge_id": challengeID, "path": rep.Path, "redacted": true}})
		}
	}
	return s.buildResult(challengeID), nil
}

// tick 单次节拍(docs/03 §2.2 决策规则,纯确定性)。
func (s *Scheduler) tick(ctx context.Context, challengeID string) {
	// 暂停(06.15):跳过全部调度逻辑——不派发新 worker、不做回收;
	// 正在执行的 worker 自然完成后不再补位。resume 后恢复。
	if s.paused.Load() {
		s.mu.Lock()
		runningCount := len(s.running)
		tickCount := s.tickCount
		s.mu.Unlock()
		s.sink.Emit(event.Event{Kind: event.KindTick, ChallengeID: challengeID,
			Payload: map[string]any{"tick": tickCount, "running": runningCount, "paused": true}})
		return
	}
	s.mu.Lock()
	s.tickCount++
	runningCount := len(s.running)
	s.mu.Unlock()
	s.sink.Emit(event.Event{Kind: event.KindTick, ChallengeID: challengeID,
		Payload: map[string]any{"tick": s.tickCount, "running": runningCount}})

	// 1. 终止优先(三路 OR)
	stop, reason, err := s.hub.ShouldStop(ctx, challengeID)
	if err != nil {
		s.sink.Emit(event.Event{Kind: event.KindError, ChallengeID: challengeID,
			Payload: map[string]any{"error": err.Error()}})
	}
	if stop {
		s.enterConclude(ctx, challengeID, reason)
		return
	}

	// 2. stale 换向:最老 Worker 无进展超时且无正确提交 → abort
	if w := s.oldestActive(); w != nil {
		now := time.Now().Unix()
		if w.record.LastProgressAt > 0 && int64(s.cfg.StaleTimeout.Seconds()) <= now-w.record.LastProgressAt && !w.record.HasCorrectSubmission {
			s.logErr(challengeID, "stale abort", s.Abort(ctx, w.record.ID, "stale"))
		}
	}

	// 3. 派活(并发槽检查)
	if !s.slotsAvailable() || s.isConcluding() {
		return
	}
	view, err := s.board.SnapshotForScheduler(challengeID, s.cfg.MinIntentWeight)
	if err != nil {
		s.sink.Emit(event.Event{Kind: event.KindError, ChallengeID: challengeID,
			Payload: map[string]any{"error": err.Error()}})
		return
	}
	switch {
	case view.FactCount == 0 && s.shouldBootstrap(challengeID):
		// 首次侦察;失败重试(真实 LLM 首次可能未输出契约 JSON 就耗尽轮次)
		s.dispatch(ctx, challengeID, WorkerOperator, PhaseRecon, "", 0)
	case view.FactCount == 0 && s.seedFocus(challengeID):
		// 阶段 2.21 兜底:bootstrap 重试耗尽(≤2 次)后由聚焦目标确定性建格,
		// 下个 tick 经正常认领派 explore(不依赖 LLM 契约格式)
	case s.isBreachMode() && s.bspace != nil:
		// phase2 breach 认领(03 §3.2 breach 分支):open 转移边按距 goal 启发式
		// 排序认领;无 open 边 → conclude(space_closed 由 hub 判定)
		if s.claimBreachEdge(ctx, challengeID) {
			// 边已认领派活
		} else {
			s.concludeWhenIdle(challengeID)
		}
	case s.cov != nil && len(view.OpenIntents) > 0:
		// phase2 coverage 认领(03 §3.2):过滤已覆盖/在跑方向 → weight 降序取第一;
		// 全被过滤但矩阵有 open 格 → 补格 intent
		if it := s.claimIntentCoverage(challengeID, view.OpenIntents); it != nil {
			if err := s.board.UpdateIntentState(it.ID, blackboard.StateClaimed, ""); err != nil {
				s.logErr(challengeID, "claim intent", err)
			} else {
				s.logErr(challengeID, "cov mark claimed", s.cov.MarkClaimed(challengeID, "", it.Target))
				s.dispatch(ctx, challengeID, WorkerOperator, PhaseExplore, it.Text, it.ID)
			}
		} else if s.dispatchGapFiller(ctx, challengeID) {
			// 补格 intent 已派活
		} else {
			s.concludeWhenIdle(challengeID)
		}
	case len(view.OpenIntents) > 0:
		// 沿最高权重未认领 intent 探索(阶段一/无矩阵)
		it := view.OpenIntents[0]
		if err := s.board.UpdateIntentState(it.ID, blackboard.StateClaimed, ""); err != nil {
			s.logErr(challengeID, "claim intent", err)
		} else {
			s.dispatch(ctx, challengeID, WorkerOperator, PhaseExplore, it.Text, it.ID)
		}
	case view.UnresolvedHypotheses > 0 && s.reasonDispatches < maxReasonDispatches:
		// 综合分析(reason-lease 互斥)。护栏:模型持续产出未验证 hyp 时
		// reason 会无限空转(lease 同 holder 可续期),达上限后让位收尾/预算
		// (阶段 2.11 删除平台完成态后该护栏成为唯一兜底)。
		ok, _ := s.board.AcquireLease("reason-"+challengeID, "scheduler", s.cfg.ReasonLeaseTTL)
		if ok {
			s.reasonDispatches++
			s.dispatch(ctx, challengeID, WorkerOperator, PhaseReason, "", 0)
		}
	case s.cov != nil && s.hasOpenCells(challengeID):
		// phase2 补格(03 §3.2 第 4 条):无可用 intent 但矩阵有 open 格 → 按格子派活
		if !s.dispatchGapFiller(ctx, challengeID) {
			s.concludeWhenIdle(challengeID)
		}
	case view.DeadEnds > 0 && s.tickCount < s.cfg.MaxTicks:
		// 死路多但时间预算剩:重试被 stale 遗留的已认领 intent
		if it := s.nextBestIntent(challengeID); it != nil {
			s.dispatch(ctx, challengeID, WorkerOperator, PhaseExplore, it.Text, it.ID)
		} else {
			s.concludeWhenIdle(challengeID)
		}
	default:
		s.concludeWhenIdle(challengeID)
	}
}

// concludeWhenIdle 无方向或时间预算耗尽时收尾(防过早:仅在无运行 Worker 时)。
// reason 语义:时间预算耗尽 → time_budget;方向耗尽 → no_more_work
// (与 enterConclude 事件层兜底一致,Result.Reason 不为空,docs/03 §2.4)。
func (s *Scheduler) concludeWhenIdle(challengeID string) {
	s.mu.Lock()
	idle := len(s.running) == 0
	s.mu.Unlock()
	if !idle {
		return
	}
	reason := constraint.Reason("no_more_work")
	if s.tickCount >= s.cfg.MaxTicks {
		reason = "time_budget"
	}
	s.enterConclude(context.Background(), challengeID, reason)
}

// enterConclude 进入收尾:abort 其他 Worker,派 conclude。
// skipWorkerID: 调用者自身的 worker ID(如 focusedConfirmed 由 explore 触发),
// 该 worker 会在 applyContract 返回后自行完成,不在 abort 中等待(防自我 abort 死锁)。
func (s *Scheduler) enterConclude(ctx context.Context, challengeID string, reason constraint.Reason, skipWorkerID ...string) {
	s.mu.Lock()
	if s.concluding {
		s.mu.Unlock()
		return
	}
	s.concluding = true
	if reason == "" {
		reason = "no_more_work"
	}
	s.termReason = reason
	s.mu.Unlock()
	s.sink.Emit(event.Event{Kind: event.KindTermination, ChallengeID: challengeID,
		Payload: map[string]any{"challenge_id": challengeID, "reason": string(reason)}})

	// abort 全部运行中的非 conclude worker(跳过调用者自身,防死锁)
	skip := ""
	if len(skipWorkerID) > 0 {
		skip = skipWorkerID[0]
	}
	s.mu.Lock()
	var toAbort []string
	for id, rw := range s.running {
		if rw.record.WorkerType != WorkerSynthesizer && id != skip {
			toAbort = append(toAbort, id)
		}
	}
	s.mu.Unlock()
	for _, id := range toAbort {
		s.logErr(challengeID, "termination abort", s.Abort(ctx, id, "termination:"+string(reason)))
	}
	// 派 conclude(等并发槽)
	s.dispatch(ctx, challengeID, WorkerSynthesizer, "", "", 0)
}

// ------------------------------------------------------------------ SchedulerHooks

// Launch 启动 Worker(docs/03 §3.2 调度命令)。
func (s *Scheduler) Launch(ctx context.Context, workerType string, handoff dispatcher.Handoff) (string, error) {
	s.mu.Lock()
	if len(s.running) >= s.cfg.MaxConcurrency && workerType != WorkerSynthesizer {
		s.mu.Unlock()
		return "", fmt.Errorf("scheduler: concurrency limit %d reached", s.cfg.MaxConcurrency)
	}
	if workerType == WorkerSynthesizer && s.concluded {
		s.mu.Unlock()
		return "", fmt.Errorf("scheduler: already concluded")
	}
	s.mu.Unlock()

	// 选 provider(role_map 路由,按 phase)
	p := s.providerFor(workerType, Phase(handoff.Phase))
	if p == nil {
		return "", fmt.Errorf("scheduler: no provider for worker type %s", workerType)
	}

	// 会话与 handoff
	challengeID := handoff.ChallengeID
	snap, err := s.board.SnapshotForWorker(challengeID)
	if err != nil {
		return "", err
	}
	asOfSeq := snap.AsOfSeq
	completed := s.completedSummary(challengeID)
	forbidden := s.forbiddenSummary(challengeID)
	snapshot := handoff.SnapshotText
	handoffText := s.handoffFor(snapshot, handoff.IntentText, completed, forbidden)
	if len([]rune(handoffText)) > maxHandoffRunes {
		// 契约已移入系统提示,handoff 只含动态段;超限时优先只裁剪快照
		// (唯一可压缩段),保住 当前 Intent/已完成事项/禁止重复项 结构完整;
		// 其余段自身超限时整体硬截兜底(docs/phase2/11 §2.5)。
		fixed := len([]rune(s.handoffFor("", handoff.IntentText, completed, forbidden)))
		if budget := maxHandoffRunes - fixed; budget > 0 {
			if snapRunes := []rune(snapshot); len(snapRunes) > budget {
				snapshot = string(snapRunes[:budget])
			}
		} else {
			snapshot = ""
		}
		handoffText = s.handoffFor(snapshot, handoff.IntentText, completed, forbidden)
		if len([]rune(handoffText)) > maxHandoffRunes {
			handoffText = truncateRunes(handoffText, maxHandoffRunes)
		}
	}

	wid := fmt.Sprintf("w-%d", time.Now().UnixNano())
	sess := conversation.New(wid, conversation.KindWorker)
	sess.ChallengeID = challengeID
	sess.Provider = p.Name()
	sess.Add(provider.Message{Role: provider.RoleUser, Content: handoffText})

	// 注册表:基础工具 + 平台工具(runWorker 内构造,见 workerRegistryFor)

	// 登记 worker
	rec := &blackboard.Worker{
		ID: wid, ChallengeID: challengeID, WorkerType: workerType, Phase: handoff.Phase,
		Provider: p.Name(), Model: p.Name(), SessionID: wid,
		Handoff: handoffText, IntentID: intentIDOf(handoff),
		LastProgressAt: time.Now().Unix(),
	}
	if err := s.board.CreateWorker(rec); err != nil {
		return "", err
	}
	rec.IntentID = intentIDOf(handoff)

	rw := &runningWorker{
		record: rec, session: sess, intentID: intentIDOf(handoff), launchSeq: asOfSeq,
		edgeID: handoff.EdgeID, // breach 边关联:构造期固定,无运行期竞争
		phase:  Phase(handoff.Phase),
	}
	workerCtx, cancel := context.WithCancel(ctx)
	rw.cancel = cancel
	rw.done = make(chan struct{})

	s.mu.Lock()
	s.running[wid] = rw
	s.mu.Unlock()

	s.sink.Emit(event.Event{Kind: event.KindWorkerLaunch, ChallengeID: challengeID,
		Payload: map[string]any{"worker": wid, "type": workerType, "phase": handoff.Phase, "provider": p.Name(), "intent_id": intentIDOf(handoff)}})

	go s.runWorker(workerCtx, rw)
	return wid, nil
}

// Abort 中止 Worker(取消上下文 + 标记)。
func (s *Scheduler) Abort(ctx context.Context, workerID, reason string) error {
	s.mu.Lock()
	rw, ok := s.running[workerID]
	s.mu.Unlock()
	if !ok {
		// 可能已结束,幂等
		return nil
	}
	s.sink.Emit(event.Event{Kind: event.KindWorkerAbort, ChallengeID: rw.record.ChallengeID,
		Payload: map[string]any{"worker": workerID, "reason": reason}})
	rw.cancel()
	select {
	case <-rw.done:
	case <-time.After(30 * time.Second):
	}
	s.mu.Lock()
	delete(s.running, workerID)
	s.mu.Unlock()
	// 终态条件更新:仅当仍为 running 时标记 aborted(runWorker 的 done/failed 优先)
	s.logErr(rw.record.ChallengeID, "abort finish", s.board.MarkWorkerFinishedIfRunning(workerID, "aborted"))
	return nil
}

// Steer 注入 Observer 纠偏消息(下一轮生效,不打断工具执行)。
func (s *Scheduler) Steer(ctx context.Context, workerID, message, priority string) error {
	s.mu.Lock()
	rw, ok := s.running[workerID]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("scheduler: unknown worker %s", workerID)
	}
	content := "[STEER" + priorityTag(priority) + "] " + message
	rw.mu.Lock()
	rw.steer = append(rw.steer, provider.Message{Role: provider.RoleUser, Content: content})
	rw.mu.Unlock()
	return nil
}

func (s *Scheduler) slotsAvailable() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.running) < s.cfg.MaxConcurrency
}

func (s *Scheduler) oldestActive() *runningWorker {
	s.mu.Lock()
	defer s.mu.Unlock()
	var oldest *runningWorker
	for _, rw := range s.running {
		if oldest == nil || rw.record.LastProgressAt < oldest.record.LastProgressAt {
			oldest = rw
		}
	}
	return oldest
}

// abortRunning 取消全部运行中 worker(stop/外部取消时快速回收,
// 防"停止后测试仍在继续"——worker 事件不再流出)。并发 abort,
// stop 总耗时 ≈ 单个 worker 清理耗时(Abort 内部有 30s 兜底超时)。
func (s *Scheduler) abortRunning(ctx context.Context, challengeID string) {
	s.mu.Lock()
	ids := make([]string, 0, len(s.running))
	for id := range s.running {
		ids = append(ids, id)
	}
	s.mu.Unlock()
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func(wid string) {
			defer wg.Done()
			s.logErr(challengeID, "run stopped, abort worker", s.Abort(ctx, wid, "run_stopped"))
		}(id)
	}
	wg.Wait()
}

// maxBootstrapRetries bootstrap 失败重试上限(真实 LLM 首次可能不收尾;防死循环)。
const maxBootstrapRetries = 2

// convSt 是会话存储(由 Options 注入)。

// isConcluded 线程安全读 concluded(docs/03 终止状态;修复 M1 遗留无锁读 race)。
func (s *Scheduler) isConcluded() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.concluded
}

// isConcluding 线程安全读 concluding(与终止状态同锁)。
func (s *Scheduler) isConcluding() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.concluding
}

// setTarget / target 线程安全读写(worker goroutine 读提示词目标,§3)。
func (s *Scheduler) setTarget(t string) {
	s.mu.Lock()
	s.target = t
	s.mu.Unlock()
}

func (s *Scheduler) targetText() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.target
}

// logErr 统一错误日志(Phase 2)。
func (s *Scheduler) logErr(challengeID, op string, err error) {
	if err != nil {
		s.sink.Emit(event.Event{Kind: event.KindError, ChallengeID: challengeID,
			TraceID: s.traceID, Level: event.LevelError,
			Payload: map[string]any{"error": fmt.Sprintf("%s: %v", op, err)}})
	}
}

// emit 发送带 trace ID 与默认 info 级别的事件(Phase 4 M4)。
func (s *Scheduler) emit(e event.Event) {
	if e.TraceID == "" {
		e.TraceID = s.traceID
	}
	if e.Level == "" {
		e.Level = event.LevelInfo
	}
	s.sink.Emit(e)
}

// Pause 暂停调度(06.15 任务控制:暂停派发,worker 不打断)。
func (s *Scheduler) Pause() { s.paused.Store(true) }

// Resume 恢复调度。
func (s *Scheduler) Resume() { s.paused.Store(false) }

// Paused 是否暂停中。
func (s *Scheduler) Paused() bool { return s.paused.Load() }
