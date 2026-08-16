// Package build 是运行时装配:配置 → Provider → 工具注册表 → 权限门 → Executor。
package build

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"scopeforge/internal/api"
	"scopeforge/internal/blackboard"
	"scopeforge/internal/breach"
	"scopeforge/internal/builtinskills"
	"scopeforge/internal/config"
	"scopeforge/internal/constraint"
	"scopeforge/internal/conversation"
	"scopeforge/internal/coverage"
	"scopeforge/internal/dispatcher"
	"scopeforge/internal/event"
	"scopeforge/internal/executor"
	"scopeforge/internal/failover"
	"scopeforge/internal/guard"
	"scopeforge/internal/kb"
	"scopeforge/internal/listener"
	"scopeforge/internal/observer"
	"scopeforge/internal/planner"
	"scopeforge/internal/reasonix/memory"
	"scopeforge/internal/reasonix/plugin"
	"scopeforge/internal/reasonix/provider"
	"scopeforge/internal/reasonix/skill"
	"scopeforge/internal/reasonix/tool"
	"scopeforge/internal/reasonix/tool/builtin"
	"scopeforge/internal/report"
	"scopeforge/internal/route"
	"scopeforge/internal/sandbox"
	"scopeforge/internal/scheduler"
	"scopeforge/internal/store"
	"scopeforge/internal/tmux"
	"scopeforge/internal/traffic"

	// init 注册 provider kinds
	_ "scopeforge/internal/reasonix/provider/anthropic"
	_ "scopeforge/internal/reasonix/provider/openai"
)

// App 是装配后的运行时。
type App struct {
	// 06.15 运行控制:runID → 取消函数(停止)
	runMu      sync.Mutex
	runs       map[string]context.CancelFunc
	CfgPath    string // 配置文件路径(M3 config PUT 落盘)
	Cfg        config.Config
	DB         *store.DB
	Providers  map[string]provider.Provider
	Registry   *tool.Registry
	Gate       *executor.Gate
	ConvStore  *conversation.Store
	TransStore *planner.TranscriptStore
	Sink       event.Sink
	SkillStore *skill.Store
	Memory     memory.Store
	WorkDir    string
	DataDir    string // 运行时数据目录(配置/数据库/提示词/技能/报告/记忆/插件根)

	// M1 编排组件
	Blackboard *blackboard.Blackboard
	Coverage   *coverage.Matrix // phase2 覆盖矩阵(切片 3)
	Breach     *breach.Space    // phase2 breach 状态空间(切片 4)
	Ledger     *constraint.CostLedger
	Meter      *constraint.Meter
	Hub        *constraint.TerminationHub
	Dispatcher *dispatcher.Dispatcher

	// M2 能力层(docs/04)
	TmuxMgr     *tmux.Manager
	ListenerMgr *listener.Manager
	RouteMgr    *route.Manager
	TrafficRec  *traffic.Recorder
	KB          *kb.Index
	GuardHook   *guard.Hook
	Sandbox     *sandbox.Docker

	// M3 观测与交互(docs/05)
	Broadcaster *event.Broadcaster
	Reports     *report.Generator
}

// envFileName 是 ScopeForge 密钥文件名(与 reasonix 的 ~/.reasonix/.env 同风格,
// ScopeForge 默认落在 ~/.scopeforge/.env;SCOPEFORGE_HOME 或 --config DIR 可覆盖)。
const envFileName = ".env"

// DefaultEnvExample 是初始化生成的 .env 模板(单二进制分发内置;
// 源码环境会优先拷贝仓库根目录 .env.example)。
const DefaultEnvExample = `# ScopeForge 密钥文件(与 reasonix 的 ~/.reasonix/.env 同风格)。
# 默认路径: ~/.scopeforge/.env(SCOPEFORGE_HOME 可覆盖;--config DIR 时为 DIR/.env)。
# 进程环境变量优先;此文件仅作兜底,请勿提交到版本库。

# LLM provider key(对应 scopeforge.yaml 中 providers[].api_key_env)
DEEPSEEK_API_KEY=sk-your-deepseek-key

# Web 写操作鉴权 token(对应 serve.auth.token_env;看板操作/配置保存/终端写需要)
SCOPEFORGE_TOKEN=change-me-to-a-long-random-token
`

// EnsureEnvFile 初始化 ScopeForge 数据目录时创建 .env(默认即 ~/.scopeforge/.env)。
// 已存在不覆盖(用户填过的真实密钥保留);密钥文件权限设为 0600。
func EnsureEnvFile(dataDir string) error {
	target := filepath.Join(dataDir, envFileName)
	if _, err := os.Stat(target); err == nil {
		return nil
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return err
	}
	data := []byte(DefaultEnvExample)
	if b, err := os.ReadFile(".env.example"); err == nil && len(b) > 0 {
		data = b
	}
	return os.WriteFile(target, data, 0o600)
}

// scopeforgeEnvPaths 返回按优先级排列的 .env 候选路径:
// ~/.scopeforge/.env(或 $SCOPEFORGE_HOME/.env)> <dataDir>/.env > 当前目录 .env。
// 进程环境变量仍高于所有这些文件(由调用方先查 os.Getenv / os.LookupEnv)。
func scopeforgeEnvPaths(dataDirs ...string) []string {
	var paths []string
	seen := map[string]bool{}
	add := func(p string) {
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		paths = append(paths, p)
	}
	add(filepath.Join(DefaultDataDir(), envFileName))
	for _, dir := range dataDirs {
		if dir != "" {
			add(filepath.Join(dir, envFileName))
		}
	}
	if wd, err := os.Getwd(); err == nil {
		add(filepath.Join(wd, envFileName))
	}
	return paths
}

// dotEnvValue 从单个 .env 文件读取变量。解析规则:
// 忽略空行/注释,支持 KEY=value 与 KEY="value"。
func dotEnvValue(path, envName string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if ok && strings.TrimSpace(k) == envName {
			return strings.Trim(strings.TrimSpace(v), `"'`), true
		}
	}
	return "", false
}

// apiKeyFor 解析 provider API key:进程环境变量优先,缺失或为空时依次兜底
// 读取 ~/.scopeforge/.env 与当前目录 .env。
// apiKeyForDirs 额外支持显式数据目录(serve/run 的 --config DIR)。
func apiKeyFor(envName string) string {
	return apiKeyForDirs(envName)
}

// apiKeyForDirs 解析 provider API key:进程环境变量优先,缺失或为空时依次兜底
// 读取 ~/.scopeforge/.env、<dataDir>/.env、当前目录 .env
// (serve/run 未 source .env 时不再 401;key 只进 .env,不进配置)。
func apiKeyForDirs(envName string, dataDirs ...string) string {
	if envName == "" {
		return ""
	}
	if v := os.Getenv(envName); v != "" {
		return v
	}
	for _, path := range scopeforgeEnvPaths(dataDirs...) {
		if v, ok := dotEnvValue(path, envName); ok && v != "" {
			return v
		}
	}
	return ""
}

// loadDotEnv 读取 .env 文件并导入进程环境(真实环境变量优先;值为空时允许
// 低优先级文件补齐)。优先级:
//
//	进程环境变量 > ~/.scopeforge/.env(SCOPEFORGE_HOME 可覆盖)> <dataDir>/.env > 当前目录 .env
//
// 默认 dataDir 就是 ~/.scopeforge,因此 serve/run 免参数开箱即用;
// serve --config /data 时仍可把部署密钥放在 /data/.env。
func loadDotEnv(dataDir string) {
	for _, path := range scopeforgeEnvPaths(dataDir) {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			k, v, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			k = strings.TrimSpace(k)
			v = strings.Trim(strings.TrimSpace(v), `"'`)
			if k == "" {
				continue
			}
			if cur, exists := os.LookupEnv(k); !exists || cur == "" {
				_ = os.Setenv(k, v)
			}
		}
	}
}

// LoadDotEnv 导出 loadDotEnv 给不需要完整 build.New 装配的入口使用
// (例如 bench 只需解析 --llm-key-env 时,也要能读到 ~/.scopeforge/.env)。
func LoadDotEnv(dataDir string) {
	loadDotEnv(dataDir)
}

// urlInTextRe 提取任务文本中的第一个 http(s) URL(中英文混排时截断)。
var urlInTextRe = regexp.MustCompile(`https?://[^\s"'<>]+`)

// firstURLInText 从任务描述提取目标 URL(入口未显式给 platform_url 时兜底,
// 阶段 2.21 聚焦模式)。返回空串 = 未发现(不启用聚焦)。
func firstURLInText(text string) string {
	m := urlInTextRe.FindString(text)
	if m == "" {
		return ""
	}
	// 截断:非 ASCII(中文)与常见标点处截断(URL 后紧跟中文/标点时)
	for i, r := range m {
		if r > 127 || strings.ContainsRune("。，,；;：！？、）】", r) {
			m = m[:i]
			break
		}
	}
	return strings.TrimRight(m, ".,;:!?")
}

// DefaultDataDir 返回 ScopeForge 运行时数据目录(单二进制分发:配置/数据库/
// 提示词/技能/报告/记忆/插件统一落在用户 HOME,不随工作目录漂移)。
// 优先级: SCOPEFORGE_HOME 环境变量 > ~/.scopeforge。
func DefaultDataDir() string {
	if d := os.Getenv("SCOPEFORGE_HOME"); d != "" {
		return d
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".scopeforge")
	}
	return ".scopeforge"
}

// Options 是装配选项。
// 平台相关选项已移除(阶段 2.11 定案:阶段二取代阶段一,
// 平台抽象层与 ctf 模式删除;获取目标/提交由 skill 卡驱动,
// 账本/覆盖矩阵/breach 语义保留)。
type Options struct {
	ConfigPath string
	DBPath     string
	WorkDir    string
	DataDir    string // 运行时数据目录(空 = DefaultDataDir())
	// Sink 覆盖(默认 SQLite 落库)。
	Sink event.Sink
}

// New 装配应用。
func New(opts Options) (*App, error) {
	cfg, err := config.Load(opts.ConfigPath)
	if err != nil {
		return nil, err
	}
	// task_profile 非法组合启动即拒绝(04 §4 验收)
	if err := validateTaskProfile(cfg); err != nil {
		return nil, err
	}
	// 运行时数据目录(配置/数据库/提示词/技能/报告/记忆/插件统一落此)
	dataDir := opts.DataDir
	if dataDir == "" {
		dataDir = DefaultDataDir()
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	// 初始化密钥文件(默认 ~/.scopeforge/.env),再导入 .env
	// (PROVIDER key / SCOPEFORGE_TOKEN),然后构造 providers 与 API 鉴权。
	if err := EnsureEnvFile(dataDir); err != nil {
		return nil, err
	}
	loadDotEnv(dataDir)
	dbPath := opts.DBPath
	if dbPath == "" {
		dbPath = filepath.Join(dataDir, "scopeforge.db")
	}
	db, err := store.Open(dbPath)
	if err != nil {
		return nil, err
	}
	if err := db.Migrate(); err != nil {
		db.Close()
		return nil, err
	}

	workDir := opts.WorkDir
	if workDir == "" {
		if wd, err := os.Getwd(); err == nil {
			workDir = wd
		}
	}

	// Providers(端点级并发闸门包装,docs/03 §7 成本护栏)
	gateMax := cfg.Models.Gate.MaxConcurrent
	var gateMin time.Duration
	if d, err := time.ParseDuration(cfg.Models.Gate.MinInterval); err == nil {
		gateMin = d
	}
	providers := map[string]provider.Provider{}
	for _, pc := range cfg.Providers {
		p, err := provider.New(pc.Kind, provider.Config{
			Name:    pc.Name,
			BaseURL: config.ExpandEnv(pc.BaseURL),
			Model:   pc.Model,
			APIKey:  apiKeyForDirs(pc.APIKeyEnv, dataDir),
			Extra:   pc.Extra,
		})
		if err != nil {
			db.Close()
			return nil, fmt.Errorf("provider %s: %w", pc.Name, err)
		}
		if gateMax > 0 || gateMin > 0 {
			p = constraint.NewEndpointGate(p, gateMax, gateMin)
		}
		providers[pc.Name] = p
	}

	// M4 §4.1:Provider failover(主失败自动切备份,LLM 网关故障自愈;
	// 健康探测先行,主端点不可达立即切换)
	if len(cfg.Models.Failover) > 0 {
		for name, p := range providers {
			var backups []provider.Provider
			for _, bn := range cfg.Models.Failover {
				if bp, ok := providers[bn]; ok && bn != name {
					backups = append(backups, bp)
				}
			}
			if len(backups) > 0 {
				probeURL := ""
				for _, pc := range cfg.Providers {
					if pc.Name == name {
						probeURL = pc.BaseURL
						break
					}
				}
				providers[name] = failover.New(p, backups, probeURL)
			}
		}
	}

	// 工具注册表
	reg := tool.NewRegistry()
	ws := builtin.Workspace{Dir: workDir}
	for _, tl := range ws.Tools() {
		reg.Add(tl)
	}

	// MCP 服务器
	if len(cfg.MCP.Servers) > 0 {
		var specs []plugin.Spec
		for _, s := range cfg.MCP.Servers {
			specs = append(specs, plugin.Spec{
				Name:        s.Name,
				Type:        "stdio",
				Command:     s.Command,
				Args:        s.Args,
				Env:         envMap(s.Env),
				URL:         s.URL,
				Headers:     s.Headers,
				Dir:         s.Dir,
				LowPriority: s.LowPriority,
			})
		}
		_, mcpTools, err := plugin.StartAll(context.Background(), specs)
		if err != nil {
			db.Close()
			return nil, fmt.Errorf("mcp: %w", err)
		}
		for _, tl := range mcpTools {
			reg.Add(tl)
		}
	}

	// 权限门
	gate, err := executor.NewGate(
		executor.PermissionMode(cfg.Tools.Permissions),
		cfg.Tools.Blacklist,
		cfg.Tools.DomainAllowlist,
	)
	if err != nil {
		db.Close()
		return nil, err
	}
	// 任务禁区(04 §1 Constraints.exclusions → Gate 域黑名单确定性拒绝)
	if err := gate.AddDenied(cfg.Platform.TaskProfile.Constraints.Exclusions); err != nil {
		db.Close()
		return nil, err
	}

	// 技能 + 记忆
	// 06.14:内置技能从 reasonix/builtincontent 移出,首次启动 materialize
	// 到 dataDir/skills(单二进制分发,随 ~/.scopeforge 统一)。仅当目标文件
	// 不存在时写入,重启不覆盖;用户可直接编辑。
	skillDir := cfg.Skills.Dir
	if skillDir == "" {
		skillDir = filepath.Join(dataDir, "skills")
	}
	if _, err := builtinskills.Materialize(skillDir); err != nil {
		return nil, err
	}
	// 内置提示词首次启动物化到 <dataDir>/prompts/(开箱即用,用户可直接编辑;
	// promptText/observer.buildPrompt 优先读这些文件,Go 常量兜底)。
	if _, err := scheduler.MaterializePrompts(dataDir); err != nil {
		return nil, err
	}
	if err := observer.MaterializePrompt(dataDir); err != nil {
		return nil, err
	}
	skillStore := skill.New(skill.Options{
		ProjectRoot:   workDir,
		CustomPaths:   append([]string{skillDir}, cfg.Skills.Paths...),
		DisabledNames: append(cfg.Skills.Disabled, "reasonix-guide"), // 上游技能,ScopeForge 不加载
	})
	memDir := cfg.Memory.Dir
	if memDir == "" {
		memDir = filepath.Join(dataDir, "memory")
	}
	memStore := memory.StoreFor(memDir, workDir)

	convStore := conversation.NewStore(db)
	// M3:事件广播器(SSE 实时推送,替代轮询;慢消费者断开靠 after 补拉)
	bc := event.NewBroadcaster()
	sink := event.NewBroadcastSink(db, bc)

	// M2 确定性安全 Hook(§5.4/§7;denied 事件入 events)
	var guardHook *guard.Hook
	if cfg.Capability.Guard.Enabled {
		guardHook, err = guard.NewHook(sink, "")
		if err != nil {
			db.Close()
			return nil, err
		}
		if err := guardHook.AddDenyPatterns(cfg.Capability.Guard.DenyPatterns...); err != nil {
			db.Close()
			return nil, err
		}
		if err := guardHook.AddExfilPatterns(cfg.Capability.Guard.ExfilPatterns...); err != nil {
			db.Close()
			return nil, err
		}
	}

	// M2 能力层(docs/04):tmux / 反连监听 / 路由 / 流量审计 / 动态池 / 知识库 / 沙箱
	tmuxMgr := tmux.NewManager(nil, cfg.Capability.Tmux.SessionPrefix)
	if cfg.Capability.Tmux.CaptureMaxLines > 0 {
		tmuxMgr.MaxLines = cfg.Capability.Tmux.CaptureMaxLines
	}
	listenerMgr := listener.NewManager(sink, "", cfg.Capability.Listener.BasePort)
	trafficRec := traffic.NewRecorder(sink, "")
	routeMgr := route.NewManager(route.Options{Sink: sink, Recorder: trafficRec})
	kbDir := cfg.Capability.Knowledge.PluginsDir
	if kbDir == "" {
		kbDir = filepath.Join(dataDir, "plugins")
	}
	kbIndex, err := kb.New(kbDir)
	if err != nil {
		db.Close()
		return nil, err
	}
	sandboxMgr := sandbox.New(nil)
	if cfg.Capability.Sandbox.Image != "" {
		sandboxMgr.Image = cfg.Capability.Sandbox.Image
	}
	if cfg.Capability.Sandbox.MemoryMB > 0 {
		sandboxMgr.MemoryMB = cfg.Capability.Sandbox.MemoryMB
	}
	if cfg.Capability.Sandbox.Pids > 0 {
		sandboxMgr.Pids = cfg.Capability.Sandbox.Pids
	}
	if cfg.Capability.Sandbox.Network != "" {
		sandboxMgr.Net = cfg.Capability.Sandbox.Network
	}
	if cfg.Capability.Sandbox.TmpfsSize != "" {
		sandboxMgr.TmpfsSize = cfg.Capability.Sandbox.TmpfsSize
	}
	// 容器模式(Cairn 会话级容器化,06.14 阶段 C-3):工具面整体切换 —
	// bash + 文件工具(ls/glob/grep/read_file/cat/edit_file/write_file/
	// delete_range/move_file)+ web_fetch 全部替换为容器内实现,
	// 宿主机对应工具 RemovePrefix 关闭("开启容器时禁用本地工具执行"。
	// 保留宿主机:code_index/complete_step/todo_write 等编排工具)。
	if cfg.Capability.Sandbox.Enabled {
		if err := installContainerBash(reg, sandboxMgr, cfg.Capability.Sandbox); err != nil {
			db.Close()
			return nil, err
		}
		// 文件工具 + web_fetch 容器化(同名同 Schema 替换)
		for _, tl := range (&sandbox.ContainerFileTools{Mgr: sandboxMgr}).Tools() {
			reg.Add(tl)
		}
	}

	// M2 常驻工具注册进主 registry(worker 复制时携带;动态池经 ApplyTo 注入)
	if cfg.Capability.Tmux.Enabled {
		for _, tl := range tmux.NewTools(tmuxMgr) {
			reg.Add(tl)
		}
	}
	if cfg.Capability.Listener.Enabled {
		for _, tl := range listener.NewTools(listenerMgr) {
			reg.Add(tl)
		}
	}
	if cfg.Capability.Route.Enabled {
		for _, tl := range route.NewTools(routeMgr) {
			reg.Add(tl)
		}
	}

	// M1 编排组件:黑板 / 账本 / 熔断 / 终止判定 / Dispatcher
	board := blackboard.New(db)
	ledger := constraint.NewCostLedger(db)
	meter := constraint.NewMeter(db, constraint.Budget{
		MaxTokensPerChallenge: cfg.Constraints.MaxTokensPerChallenge,
		MaxTurnsPerChallenge:  cfg.Constraints.MaxTurnsPerChallenge,
		MaxCostUSD:            cfg.Constraints.MaxCostUSD,
	})
	hub := constraint.NewTerminationHubWithPolicy(board, meter, sink, taskPolicy(cfg))
	disp := dispatcher.New(board, sink, nil)
	// phase2 覆盖矩阵(切片 3):装配到 Dispatcher(攻击面落地/回执联动)与
	// TerminationHub(coverage 收敛终止,02 §2.3)。
	covMat := coverage.New(db)
	disp.SetCoverage(covMat)
	hub.SetConvergence(covMat)
	// phase2 平台 v2 装配已移除(阶段 2.9 定案:不做平台对接适配器,
	// 获取目标/提交由 skill 卡驱动;账本/覆盖矩阵/breach 语义保留)。
	// phase2 breach 状态空间(goal_shape=breach 时启用):
	// 目标节点经独立验证器确认(模型自述不采信,02 §2.3)。
	bspace := breach.New(db)
	policy := taskPolicy(cfg)
	if policy.GoalShape == constraint.GoalShapeBreach {
		if cfg.Platform.TaskProfile.GoalNode != "" {
			bspace.SetGoal(cfg.Platform.TaskProfile.GoalNode)
		}
		hub.SetGoalVerifier(bspace)
	}
	// 报告生成器(§5)
	// 切片 5 报告分叉(05 §2):goal_shape 决定报告结构(coverage 清单版/breach 攻击链版)
	tp := taskPolicy(cfg)
	reports := &report.Generator{DB: db, Board: board, Ledger: ledger, Sink: sink, WorkDir: workDir,
		GoalShape: tp.GoalShape, Breach: bspace, Coverage: covMat, Interest: tp.Interest}

	app := &App{
		CfgPath:     opts.ConfigPath,
		Cfg:         cfg,
		DB:          db,
		Providers:   providers,
		Registry:    reg,
		Gate:        gate,
		ConvStore:   convStore,
		TransStore:  planner.NewTranscriptStore(db),
		Sink:        sink,
		SkillStore:  skillStore,
		Memory:      memStore,
		WorkDir:     workDir,
		DataDir:     dataDir,
		Blackboard:  board,
		Coverage:    covMat,
		Breach:      bspace,
		Ledger:      ledger,
		Meter:       meter,
		Hub:         hub,
		Dispatcher:  disp,
		TmuxMgr:     tmuxMgr,
		ListenerMgr: listenerMgr,
		RouteMgr:    routeMgr,
		TrafficRec:  trafficRec,
		KB:          kbIndex,
		GuardHook:   guardHook,
		Sandbox:     sandboxMgr,
		Broadcaster: bc,
		Reports:     reports,
	}
	// 06.17 孤儿任务收尾:进程重启后 run_done 丢失(goroutine 随进程消失),
	// 历史任务会永远显示 running。启动时扫描 run_started 无 run_done 的
	// 任务,统一发 run_done(interrupted) —— 前端立即显示"已中断",
	// 无需等待 stale 窗口,也不再出现"暂停/停止返回 404"的假运行任务。
	if db != nil {
		rows, err := db.Query(`SELECT challenge_id FROM events WHERE kind = 'run_started'
			AND challenge_id NOT IN (SELECT challenge_id FROM events WHERE kind = 'run_done')`)
		if err == nil {
			var orphans []string
			for rows.Next() {
				var id string
				if rows.Scan(&id) == nil {
					orphans = append(orphans, id)
				}
			}
			rows.Close()
			for _, id := range orphans {
				sink.Emit(event.Event{Kind: event.KindRunDone, ChallengeID: id,
					Payload: map[string]any{"interrupted": true, "reason": "process restart (orphan)"}})
			}
			if len(orphans) > 0 {
				fmt.Printf("scopeforge: 收尾 %d 个孤儿任务(进程重启残留)\n", len(orphans))
			}
		}
	}
	return app, nil
}

// Close 关闭数据库(不等待运行中任务;测试用)。
// 生产环境请用 Shutdown(ctx) 确保 goroutine 安全退出。
func (a *App) Close() {
	if a.DB != nil {
		a.DB.Close()
	}
}

// Shutdown 优雅关闭:取消所有运行中任务,等待完成,关闭数据库。
// Phase 4 H3: 统一 goroutine 生命周期管理,防 "sql: database is closed" 日志噪音。
func (a *App) Shutdown(ctx context.Context) error {
	// 1. 取消全部运行中任务
	a.runMu.Lock()
	for id, cancel := range a.runs {
		if cancel != nil {
			cancel()
		}
		delete(a.runs, id)
	}
	a.runMu.Unlock()
	// 2. 给 goroutine 短暂时间收尾
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(200 * time.Millisecond):
	}
	// 3. 关闭数据库
	a.Close()
	return nil
}

// validateTaskProfile 校验 task_profile 非法组合(04 §4 验收:启动即拒绝)。
func validateTaskProfile(cfg config.Config) error {
	tp := cfg.Platform.TaskProfile
	switch tp.GoalShape {
	case "", "coverage":
		if tp.GoalNode != "" {
			return fmt.Errorf("task_profile: goal_shape=coverage 不允许 goal_node(%q,breach 专用)", tp.GoalNode)
		}
		if tp.GoalScope != "" {
			return fmt.Errorf("task_profile: goal_shape=coverage 不允许 goal_scope(%q,breach 专用)", tp.GoalScope)
		}
	case "breach":
		// goal_node 可空(space_closed 兜底);specific 必须有目标
		if tp.GoalScope == "specific" && tp.GoalNode == "" {
			return fmt.Errorf("task_profile: goal_scope=specific 需要 goal_node(朝指定目标收敛)")
		}
	default:
		return fmt.Errorf("task_profile: 非法 goal_shape %q(coverage|breach)", tp.GoalShape)
	}
	switch tp.GoalScope {
	case "", "specific", "maximize":
	default:
		return fmt.Errorf("task_profile: 非法 goal_scope %q(specific|maximize)", tp.GoalScope)
	}
	if tp.InterestMinSev != "" {
		if _, ok := constraint.SeverityRank(tp.InterestMinSev); !ok {
			return fmt.Errorf("task_profile: 非法 interest_min_severity %q(critical|high|medium|low|info)", tp.InterestMinSev)
		}
	}
	return nil
}

// taskPolicy 从配置构造终止策略(docs/phase2/02 §2.5 + 04 §4)。
// 零值(未配置 task_profile)→ coverage 语义(默认形态)。
func taskPolicy(cfg config.Config) constraint.Policy {
	tp := cfg.Platform.TaskProfile
	if tp.GoalShape == "" {
		return constraint.Policy{GoalShape: constraint.GoalShapeCoverage}
	}
	p := constraint.Policy{
		GoalShape: tp.GoalShape,
		Interest: constraint.Interest{
			SeverityMin: tp.InterestMinSev,
			CWEs:        tp.InterestCWEs,
		},
		MaxConvergence: tp.MaxConvergence,
	}
	return p
}

// installContainerBash 用容器版 bash 替换注册表中的宿主机 bash(Cairn 模式)。
// 前置检查:docker 可用 + 镜像存在(缺失时按配置 Dockerfile 自动构建)。
func installContainerBash(reg *tool.Registry, mgr *sandbox.Docker, cfg config.SandboxConfig) error {
	ctx := context.Background()
	if !mgr.Available(ctx) {
		return fmt.Errorf("capability.sandbox.enabled 但 docker 不可用(工具执行无法容器化)")
	}
	if !mgr.ImagePresent(ctx) {
		df := cfg.Dockerfile
		if df == "" {
			df = "deploy/executor/Dockerfile"
		}
		if err := mgr.BuildImage(ctx, df); err != nil {
			return fmt.Errorf("capability.sandbox: 镜像 %s 缺失且自动构建失败(%s): %v", mgr.Image, df, err)
		}
	}
	reg.RemovePrefix("bash")
	reg.Add(sandbox.ContainerBash{Mgr: mgr})
	return nil
}

// SchedulerCfg 从配置构建调度配置。
func (a *App) SchedulerCfg() scheduler.Config {
	sc := scheduler.DefaultConfig()
	if d, err := time.ParseDuration(a.Cfg.Scheduler.TickInterval); err == nil && d > 0 {
		sc.TickInterval = d
	}
	if d, err := time.ParseDuration(a.Cfg.Scheduler.StaleTimeout); err == nil && d > 0 {
		sc.StaleTimeout = d
	}
	if d, err := time.ParseDuration(a.Cfg.Scheduler.ReasonLeaseTTL); err == nil && d > 0 {
		sc.ReasonLeaseTTL = d
	}
	if a.Cfg.Scheduler.MaxConcurrency > 0 {
		sc.MaxConcurrency = a.Cfg.Scheduler.MaxConcurrency
	}
	if a.Cfg.Scheduler.MaxTicks > 0 {
		sc.MaxTicks = a.Cfg.Scheduler.MaxTicks
	}
	if len(a.Cfg.Scheduler.WorkerTurns) > 0 {
		for k, v := range a.Cfg.Scheduler.WorkerTurns {
			if v > 0 {
				sc.WorkerTurns[k] = v
			}
		}
	}
	sc.ReportDir = filepath.Join(a.DataDir, "reports")
	return sc
}

// SchedulerFor 构造单题调度器(Dispatcher hooks 自动注入)。
func (a *App) SchedulerFor(challengeID string) *scheduler.Scheduler {
	return a.SchedulerForCfg(challengeID, a.SchedulerCfg())
}

// SchedulerForCfg 按给定配置构造调度器(任务形态取静态配置)。
func (a *App) SchedulerForCfg(challengeID string, sc scheduler.Config) *scheduler.Scheduler {
	return a.SchedulerForProfile(challengeID, sc, a.Cfg.Platform.TaskProfile)
}

// SchedulerForProfile 按给定配置与任务形态构造调度器(04 §1 任务描述注入:
// task 入口可覆盖 Description 后传入,description/skills/constraints 随调度器生效)。
func (a *App) SchedulerForProfile(challengeID string, sc scheduler.Config, tp config.TaskProfileConfig) *scheduler.Scheduler {
	sch := scheduler.New(scheduler.Options{
		Cfg:        sc,
		Board:      a.Blackboard,
		Dispatcher: a.Dispatcher,
		Hub:        a.Hub,
		Providers:  a.Providers,
		RoleMap:    a.Cfg.Models.RoleMap,
		Registry:   a.Registry,
		Gate:       a.Gate,
		Sink:       a.Sink,
		Ledger:     a.Ledger,
		Pricing:    a.ProviderPricing(),
		ConvStore:  a.ConvStore,
		CompactCfg: a.CompactConfig(),
		WorkDir:    a.WorkDir,
		PromptDir:  a.DataDir,
		SkillStore: a.SkillStore,
		KB:         a.KB,
		Guard:      a.GuardHook,
		Reports:    a.Reports,
		Coverage:   a.Coverage,
		Breach:     a.Breach,
		// 切片 5 任务描述注入(04 §1):description/skills/constraints 随调度器生效
		TaskProfile: tp,
	})
	a.Dispatcher.RegisterHooks(challengeID, sch)
	return sch
}

// ObserverFor 构造旁路审查者。
func (a *App) ObserverFor(challengeID string) *observer.Observer {
	p := a.firstProvider()
	cfg := observer.DefaultConfig()
	if a.Cfg.Scheduler.ObserverEvery > 0 {
		cfg.EveryNTurns = a.Cfg.Scheduler.ObserverEvery
	}
	return observer.New(observer.Options{
		Cfg:             cfg,
		Board:           a.Blackboard,
		Dispatcher:      a.Dispatcher,
		Provider:        p,
		Sink:            a.Sink,
		Ledger:          a.Ledger,
		Pricing:         a.ProviderPricing()[p.Name()],
		CompactCfg:      a.CompactConfig(),
		DB:              a.DB,
		ActiveWorkers:   func() []string { return a.activeWorkerIDs(challengeID) },
		Coverage:        a.Coverage,
		TaskDescription: a.Cfg.Platform.TaskProfile.Description,
		GoalShape:       a.Cfg.Platform.TaskProfile.GoalShape,
		PromptDir:       a.DataDir,
	})
}

// firstProvider 返回第一个可用 provider。
func (a *App) firstProvider() provider.Provider {
	for _, p := range a.Providers {
		return p
	}
	return nil
}

// activeWorkerIDs 查询运行中的 worker(challenge 维度)。
func (a *App) activeWorkerIDs(challengeID string) []string {
	ws, err := a.Blackboard.Workers(challengeID, "running")
	if err != nil {
		return nil
	}
	var ids []string
	for _, w := range ws {
		ids = append(ids, w.ID)
	}
	return ids
}

// CompactConfig 从配置构建压缩配置。
func (a *App) CompactConfig() conversation.CompactConfig {
	cc := conversation.CompactConfig{
		Soft: a.Cfg.Agent.Compact.Soft, Snip: a.Cfg.Agent.Compact.Snip,
		Compact: a.Cfg.Agent.Compact.Compact, Force: a.Cfg.Agent.Compact.Force,
		Tail: a.Cfg.Agent.Compact.Tail, ContextWindow: a.Cfg.Agent.ContextWindow,
	}
	if cc.ContextWindow <= 0 {
		cc.ContextWindow = 65536
	}
	if cc.Tail <= 0 {
		cc.Tail = 16384
	}
	return cc
}

// ProviderPricing 构建 provider name → 单价 映射。
func (a *App) ProviderPricing() map[string]*provider.Pricing {
	out := map[string]*provider.Pricing{}
	for _, pc := range a.Cfg.Providers {
		if pc.Pricing.Currency == "" && pc.Pricing.Input == 0 && pc.Pricing.Output == 0 {
			continue
		}
		out[pc.Name] = &provider.Pricing{
			CacheHit: pc.Pricing.CacheHit,
			Input:    pc.Pricing.Input,
			Output:   pc.Pricing.Output,
			Currency: pc.Pricing.Currency,
		}
	}
	return out
}

// RegisterAgentTools 注册依赖运行时上下文的工具(task/run_skill/记忆工具)。
func (a *App) RegisterAgentTools(p provider.Provider, semaphore chan struct{}) {
	taskTool := &planner.TaskTool{
		Provider:   p,
		Registry:   a.Registry,
		ConvStore:  a.ConvStore,
		TransStore: a.TransStore,
		Gate:       a.Gate,
		Sink:       a.Sink,
		WorkDir:    a.WorkDir,
		CompactCfg: conversation.DefaultCompactConfig(),
		MaxDepth:   a.Cfg.Agent.MaxSubagentDepth,
		Semaphore:  semaphore,
	}
	if taskTool.MaxDepth <= 0 {
		taskTool.MaxDepth = 2
	}
	a.Registry.Add(taskTool)
	// 技能工具
	a.Registry.Add(skill.NewRunSkillTool(a.SkillStore, nil))
	// 记忆工具(简化:remember/forget/recall 以 skill 或 todo 形式,M1 强化)
	a.Registry.Add(&memoryTool{store: a.Memory})
}

func envMap(env []string) map[string]string {
	out := map[string]string{}
	for _, kv := range env {
		for i := 0; i < len(kv); i++ {
			if kv[i] == '=' {
				out[kv[:i]] = kv[i+1:]
				break
			}
		}
	}
	return out
}

// APIHandler 装配 M3 API 服务器(docs/05 §1.1):全组件依赖注入。
func (a *App) APIHandler() *api.Server {
	return api.NewServer(api.Deps{
		DB:          a.DB,
		Broadcaster: a.Broadcaster,
		Board:       a.Blackboard,
		Ledger:      a.Ledger,
		Meter:       a.Meter,
		Tmux:        a.TmuxMgr,
		Route:       a.RouteMgr,
		Listener:    a.ListenerMgr,
		KB:          a.KB,
		Sandbox:     a.Sandbox,
		Providers:   a.Providers,
		Cfg:         a.Cfg,
		Reports:     a.Reports,
		WorkDir:     a.WorkDir,
		ConfigPath:  a.CfgPath,
		AuthToken:   a.APIToken(),
		Runner:      a,
	})
}

// APIToken serve 鉴权 token(serve.auth.token_env 环境变量)。
func (a *App) APIToken() string {
	if env := a.Cfg.Serve.Auth.TokenEnv; env != "" {
		return os.Getenv(env)
	}
	return ""
}
