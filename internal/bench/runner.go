package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"scopeforge/internal/blackboard"
	"scopeforge/internal/breach"
	"scopeforge/internal/config"
	"scopeforge/internal/constraint"
	"scopeforge/internal/conversation"
	"scopeforge/internal/coverage"
	"scopeforge/internal/cwe"
	"scopeforge/internal/dispatcher"
	"scopeforge/internal/event"
	"scopeforge/internal/executor"
	"scopeforge/internal/reasonix/provider"
	_ "scopeforge/internal/reasonix/provider/openai"
	"scopeforge/internal/reasonix/tool"
	"scopeforge/internal/scheduler"
	"scopeforge/internal/store"
)

// RunnerConfig 是评测 runner 参数(docs/phase2/02 §3.2 benchmark 改造)。
type RunnerConfig struct {
	SetName string // src(mock SRC 靶场) | juice(本地 Juice Shop 真实靶场,11 §3-④)

	Target    *SRCTarget // 自定义靶场(空 = 按 SetName 取默认)
	TargetURL string     // juice 集目标地址(空 = http://127.0.0.1:3000)

	LLMBase   string // 真实 LLM 端点(空 = 内置 scripted mock LLM,离线闭环)
	LLMModel  string // 真实 LLM 模型名(LLMBase 非空时生效)
	LLMKeyEnv string // 真实 LLM API key 环境变量名

	MaxCostUSD float64 // 成本熔断(0 = 默认:mock 不限,real 3.0 防跑飞)

	OutDir      string // 输出目录(JSONL 结果)
	DBPath      string // 评测落库路径(空 = 临时文件)
	WorkDir     string // workdir(报告等,空 = OutDir)
	ChallengeID string // 任务 id(空 = bench-<时间戳>)

	Tick     time.Duration // 调度节拍(默认 50ms,测试加速)
	MaxTicks int           // 调度 tick 上限(默认 2000)
}

// DefaultRunnerConfig 返回默认评测配置。
func DefaultRunnerConfig() RunnerConfig {
	return RunnerConfig{
		SetName: "src",
		Tick:    50 * time.Millisecond,
	}
}

// RunResult 是单次评测结果。
type RunResult struct {
	RunID       string            `json:"run_id"`
	SetName     string            `json:"set_name"`
	ChallengeID string            `json:"challenge_id"`
	LLMMode     string            `json:"llm_mode"` // mock | real(11 §3-④:与 mock 分开记账)
	Model       string            `json:"model"`
	Terminated  bool              `json:"terminated"`
	Concluded   bool              `json:"concluded"`
	Reason      string            `json:"reason"`
	Metrics     Metrics           `json:"metrics"`
	Result      *scheduler.Result `json:"-"`
}

// Runner 执行一次评测:装配调度器 → 跑完整任务 → 回执推进 → 指标计算 → JSONL。
type Runner struct {
	cfg     RunnerConfig
	runID   string
	chID    string
	target  *SRCTarget
	judger  *Judger
	board   *blackboard.Blackboard
	disp    *dispatcher.Dispatcher
	cov     *coverage.Matrix
	bspace  *breach.Space
	db      *store.DB
	outPath string

	// 任务结束快照(评测报告/测试断言用;Run 结束后 db 已关闭,需先取数)
	LastCells []coverage.Cell            `json:"-"`
	LastVulns []blackboard.Vulnerability `json:"-"`
}

// NewRunner 构建评测 runner。
func NewRunner(cfg RunnerConfig) (*Runner, error) {
	if cfg.SetName == "" {
		cfg.SetName = "src"
	}
	switch cfg.SetName {
	case "src":
		if cfg.Target == nil {
			cfg.Target = DefaultSRCTarget()
		}
	case "juice":
		// 真实靶场集(11 §3-④):目标真实可达、工具真实执行——
		// 必须配真实 LLM(mock 只会复述种子,度量不了真实能力)。
		if cfg.LLMBase == "" {
			return nil, fmt.Errorf("bench: --set juice 需要真实 LLM(--llm-base/--llm-model/--llm-key-env)")
		}
		if cfg.TargetURL == "" {
			cfg.TargetURL = "http://127.0.0.1:3000"
		}
		if cfg.Target == nil {
			cfg.Target = JuiceShopTarget(cfg.TargetURL)
		}
	default:
		return nil, fmt.Errorf("bench: 未知评测集 %q(src | juice)", cfg.SetName)
	}
	if cfg.Tick == 0 {
		cfg.Tick = 50 * time.Millisecond
	}
	if cfg.MaxTicks == 0 {
		cfg.MaxTicks = 2000
	}
	if cfg.WorkDir == "" {
		cfg.WorkDir = cfg.OutDir
	}
	r := &Runner{
		cfg:    cfg,
		runID:  fmt.Sprintf("bench-%d", time.Now().UnixNano()),
		chID:   cfg.ChallengeID,
		target: cfg.Target,
		judger: NewJudger(cfg.Target),
	}
	if r.chID == "" {
		r.chID = r.runID
	}
	if cfg.OutDir != "" {
		r.outPath = filepath.Join(cfg.OutDir, "bench-runs.jsonl")
		if err := os.MkdirAll(cfg.OutDir, 0o755); err != nil {
			return nil, fmt.Errorf("bench: outdir: %w", err)
		}
	}
	return r, nil
}

// llmMode 返回记账用 LLM 模式(11 §3-④:mock 全绿 ≠ 真实有效,两者分开记账)。
func (r *Runner) llmMode() string {
	if r.cfg.LLMBase != "" {
		return "real"
	}
	return "mock"
}

// Run 执行一次完整评测任务(02 §3.2)。
func (r *Runner) Run(ctx context.Context) (*RunResult, error) {
	// --- 落库 ---
	dbPath := r.cfg.DBPath
	if dbPath == "" {
		dbPath = filepath.Join(os.TempDir(), r.runID+".db")
	}
	db, err := store.Open(dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	if r.cfg.DBPath == "" {
		defer os.Remove(dbPath) // 临时库清理
	}
	if err := db.Migrate(); err != nil {
		return nil, err
	}
	r.db = db

	// --- LLM(内置 scripted mock 或真实端点)---
	llmURL := r.cfg.LLMBase
	var scripted *scriptedLLM
	if llmURL == "" {
		scripted = newScriptedLLM(r.target)
		defer scripted.Close()
		llmURL = scripted.URL
	}
	model := r.cfg.LLMModel
	if model == "" {
		model = "mock-model"
	}
	apiKey := ""
	if r.cfg.LLMKeyEnv != "" {
		apiKey = os.Getenv(r.cfg.LLMKeyEnv)
	}
	p, err := provider.New("openai", provider.Config{
		Name: "bench", BaseURL: llmURL, Model: model, APIKey: apiKey,
	})
	if err != nil {
		return nil, fmt.Errorf("bench: provider: %w", err)
	}

	// --- 内核装配(与 scheduler_test 同规,阶段 2.11 无平台)---
	board := blackboard.New(db)
	ledger := constraint.NewCostLedger(db)
	// 成本熔断(11 §3-④):真实 LLM 默认 3.0 USD 防跑飞;mock 不计费无需熔断。
	budget := constraint.Budget{MaxCostUSD: r.cfg.MaxCostUSD}
	if r.cfg.MaxCostUSD == 0 && r.cfg.LLMBase != "" {
		budget.MaxCostUSD = 3.0
	}
	meter := constraint.NewMeter(db, budget)
	hub := constraint.NewTerminationHubWithPolicy(board, meter, nil,
		constraint.Policy{GoalShape: constraint.GoalShapeCoverage})
	disp := dispatcher.New(board, event.Discard, nil)
	covMat := coverage.New(db)
	disp.SetCoverage(covMat)
	hub.SetConvergence(covMat)
	bspace := breach.New(db)

	reg := tool.NewRegistry()
	if r.cfg.LLMBase != "" {
		reg.Add(&realBashTool{}) // 真实 LLM → 真实执行(端到端度量)
	} else {
		reg.Add(&benchBashTool{}) // mock → 固定输出(离线确定性)
	}
	gate, err := executor.NewGate(executor.ModeYolo, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("bench: gate: %w", err)
	}

	workDir := r.cfg.WorkDir
	if workDir == "" {
		workDir = os.TempDir()
	}
	s := scheduler.New(scheduler.Options{
		Cfg: scheduler.Config{
			TickInterval:   r.cfg.Tick,
			StaleTimeout:   time.Hour,
			MaxConcurrency: 1,
			MaxTicks:       r.cfg.MaxTicks,
		},
		Board:      board,
		Dispatcher: disp,
		Hub:        hub,
		Providers:  map[string]provider.Provider{"bench": p},
		Registry:   reg,
		Gate:       gate,
		Sink:       event.Discard,
		Ledger:     ledger,
		ConvStore:  conversation.NewStore(db),
		CompactCfg: conversation.DefaultCompactConfig(),
		WorkDir:    workDir,
		Coverage:   covMat,
		Breach:     bspace,
		TaskProfile: config.TaskProfileConfig{
			Description: r.taskDescription(),
		},
	})
	disp.SetHooks(s)
	r.board = board
	r.disp = disp
	r.cov = covMat
	r.bspace = bspace

	// --- 回执推进器(mock 平台回执,离线闭环)---
	judgeCtx, judgeCancel := context.WithCancel(ctx)
	var pumpWG sync.WaitGroup
	pumpWG.Add(1)
	go r.receiptPump(judgeCtx, &pumpWG)

	// --- 跑任务 ---
	res, err := s.Run(ctx, r.chID)
	if err != nil {
		judgeCancel()
		pumpWG.Wait()
		return nil, fmt.Errorf("bench: run: %w", err)
	}

	// --- 收尾:停 pump,同步判定剩余 submitted(确定性取数)---
	judgeCancel()
	pumpWG.Wait()
	r.judgeSubmitted(context.Background()) // Run 返回瞬间写入的条目兜底

	// --- 指标 ---
	cells, _ := covMat.Cells(r.chID)
	vulns, _ := board.Vulnerabilities(r.chID)
	r.LastCells = cells
	r.LastVulns = vulns
	// 正常收敛白名单:覆盖收敛 / 穷尽声明方向耗尽;预算/时间硬断不算(02 §2.2 ①路兜底)
	converged := res.Reason == constraint.ReasonCoverageConverged || res.Reason == reasonNoMoreWork
	metrics := Evaluate(r.target, vulns, cells, res.CostUSD, res.Turns, converged)

	out := &RunResult{
		RunID: r.runID, SetName: r.cfg.SetName, ChallengeID: r.chID,
		LLMMode: r.llmMode(), Model: model,
		Terminated: res.Terminated, Concluded: res.Concluded,
		Reason: string(res.Reason), Metrics: metrics, Result: res,
	}
	if err := r.appendJSONL(out); err != nil {
		return nil, err
	}
	return out, nil
}

// reasonNoMoreWork 是方向耗尽收敛的终止理由(scheduler 收尾路径,非预算硬断)。
const reasonNoMoreWork = "no_more_work"

// receiptPump 轮询账本:对 submitted 条目对照预埋集推进回执
// (命中→accepted/重复→duplicate/未命中→false_positive),驱动矩阵与接力。
func (r *Runner) receiptPump(ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.judgeSubmitted(ctx)
		}
	}
}

// judgeSubmitted 对账本中全部 submitted 条目判定回执(幂等:judger.accepted 去重;
// 状态更新后条目不再 submitted,失败条目下轮重试)。
// 注意:blackboard.Vulnerabilities 按提交时间倒序返回,不能用 ID 游标跳过
// (会系统性漏判非最新条目);全量遍历成本可控(账本规模小)。
func (r *Runner) judgeSubmitted(ctx context.Context) {
	vulns, err := r.board.Vulnerabilities(r.chID)
	if err != nil {
		return
	}
	for _, v := range vulns {
		if v.Status != blackboard.LedgerSubmitted {
			continue
		}
		status, ref := r.judger.Judge(v)
		_ = r.disp.UpdateVulnerabilityReceipt(ctx, "bench-judge", r.chID, v.ID, status, ref)
	}
}

// appendJSONL 追加一行结果(OutDir 非空时)。
func (r *Runner) appendJSONL(out *RunResult) error {
	if r.outPath == "" {
		return nil
	}
	f, err := os.OpenFile(r.outPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	return enc.Encode(out)
}

// taskDescription 返回注入调度器的任务描述(04 §1 任务描述注入)。
// mock 集不需要(scripted LLM 不读提示词);juice 集必须告诉真实模型
// 目标、范围与提交语义——否则端到端跑的是"无头任务"。
func (r *Runner) taskDescription() string {
	if r.cfg.SetName != "juice" {
		return ""
	}
	asset := cwe.NormalizeAsset(r.cfg.TargetURL)
	return fmt.Sprintf(`对本地 OWASP Juice Shop 靶场(%s)做 SRC 式漏洞挖掘(本地授权靶场)。
目标:发现尽可能多的真实漏洞(SQL 注入/越权/信息泄露/XSS 等),每个确认漏洞经 submit_vulnerability 登记。
提交纪律:必须带 cwe/asset/endpoint 结构化字段——asset 填 %q,endpoint 填漏洞所在路径(去 query),cwe 用白名单编号,不确定留空。
已知攻击面参考:/rest/products/search(商品搜索,注入高发)、/rest/basket/{id}(购物车,越权高发)、/#/search(反射型 XSS)、/api/*(REST 接口)。
工作方式:用 bash + curl 直接探测(如 curl -s "%s/rest/products/search?q=test"),先枚举攻击面再逐类验证。
范围红线:仅 %s 一个主机,禁止扫描/攻击其他任何主机。`, r.cfg.TargetURL, asset, r.cfg.TargetURL, asset)
}

// benchBashTool 是评测用最小 bash 工具(mock 环境,不真正执行)。
type benchBashTool struct{}

func (b *benchBashTool) Name() string        { return "bash" }
func (b *benchBashTool) Description() string { return "执行命令" }
func (b *benchBashTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}}}`)
}
func (b *benchBashTool) ReadOnly() bool { return true }
func (b *benchBashTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	return "scout output: mock 靶场端口 80/443 开放", nil
}

// realBashTool 是真实执行 bash 工具(--set juice 等真实靶场评测用,11 §3-④):
// 宿主机真实执行命令(curl/sqlmap 等),输出截断 8KB,单命令 2 分钟超时。
type realBashTool struct{}

func (b *realBashTool) Name() string { return "bash" }
func (b *realBashTool) Description() string {
	return "执行命令(curl/nmap 等;输出截断 8KB,单命令 2 分钟超时)"
}
func (b *realBashTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"command":{"type":"string","description":"要执行的 shell 命令"}},"required":["command"]}`)
}
func (b *realBashTool) ReadOnly() bool { return false }
func (b *realBashTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var p struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(args, &p); err != nil || p.Command == "" {
		return "", fmt.Errorf("bash: 缺少 command 参数")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "bash", "-c", p.Command).CombinedOutput()
	s := string(out)
	if len(s) > 8*1024 {
		s = s[:8*1024] + "\n...[truncated]..."
	}
	if ctx.Err() == context.DeadlineExceeded {
		return s + "\n...[timed out 2m]", nil
	}
	if err != nil {
		return s, fmt.Errorf("bash: %v", err)
	}
	return s, nil
}
