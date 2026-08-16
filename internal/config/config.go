// Package config 加载 scopeforge.yaml 聚合配置(docs/02 §8.2)。
// 密钥只进 .env / 环境变量,配置内只允许 ${ENV} 引用(防密钥入仓)。
package config

import (
	"fmt"
	"os"
	"regexp"

	"gopkg.in/yaml.v3"
)

// Config 是 scopeforge.yaml 顶层结构(docs/02 §8.2)。
type Config struct {
	Providers   []ProviderConfig `yaml:"providers"`
	Agent       AgentConfig      `yaml:"agent"`
	Tools       ToolsConfig      `yaml:"tools"`
	Skills      SkillsConfig     `yaml:"skills"`
	MCP         MCPConfig        `yaml:"mcp"`
	Memory      MemoryConfig     `yaml:"memory"`
	Platform    PlatformConfig   `yaml:"platform"`
	Constraints Constraints      `yaml:"constraints"`
	Scheduler   SchedulerConfig  `yaml:"scheduler"`
	Models      ModelsConfig     `yaml:"models"`
	Serve       ServeConfig      `yaml:"serve"`
	Capability  CapabilityConfig `yaml:"capability"` // M2 能力层(docs/04)
}

// ProviderConfig 是一个 provider 实例。
type ProviderConfig struct {
	Name      string         `yaml:"name"`
	Kind      string         `yaml:"kind"` // openai | anthropic
	BaseURL   string         `yaml:"base_url"`
	Model     string         `yaml:"model"`
	APIKeyEnv string         `yaml:"api_key_env"`
	Pricing   PricingConfig  `yaml:"pricing,omitempty"`
	Extra     map[string]any `yaml:",inline"`
}

// PricingConfig 是每百万 token 美元价。
type PricingConfig struct {
	CacheHit float64 `yaml:"cache_hit"`
	Input    float64 `yaml:"input"`
	Output   float64 `yaml:"output"`
	Currency string  `yaml:"currency"`
}

// AgentConfig 是 agent 运行参数。
type AgentConfig struct {
	MaxTurns               int           `yaml:"max_turns"`
	ContextWindow          int           `yaml:"context_window"`
	Compact                CompactConfig `yaml:"compact"`
	MaxSubagentDepth       int           `yaml:"max_subagent_depth"`
	MaxSubagentConcurrency int           `yaml:"max_subagent_concurrency"`
	MaxParallelWriters     int           `yaml:"max_parallel_writers"`
}

// CompactConfig 是压缩阈值。
type CompactConfig struct {
	Soft    float64 `yaml:"soft"`
	Snip    float64 `yaml:"snip"`
	Compact float64 `yaml:"compact"`
	Force   float64 `yaml:"force"`
	Tail    int     `yaml:"tail"`
}

// ToolsConfig 是工具与权限配置。
type ToolsConfig struct {
	Permissions     string   `yaml:"permissions"` // auto | yolo(ask 已移除,06.14)
	Blacklist       []string `yaml:"blacklist"`
	DomainAllowlist []string `yaml:"domain_allowlist"`
}

// SkillsConfig 是技能配置。
type SkillsConfig struct {
	Dir      string   `yaml:"dir"`      // 内置技能 materialize 目录(可编辑;默认 "skills")
	Paths    []string `yaml:"paths"`    // 额外自定义技能目录
	Disabled []string `yaml:"disabled"` // 禁用的技能名
}

// MCPConfig 是 MCP 服务器列表。
type MCPConfig struct {
	Servers []MCPServerConfig `yaml:"servers"`
}

// MCPServerConfig 是一个 MCP 服务器。
type MCPServerConfig struct {
	Name        string            `yaml:"name"`
	Command     string            `yaml:"command,omitempty"`
	Args        []string          `yaml:"args,omitempty"`
	Env         []string          `yaml:"env,omitempty"`
	URL         string            `yaml:"url,omitempty"`
	Headers     map[string]string `yaml:"headers,omitempty"`
	Dir         string            `yaml:"dir,omitempty"`
	LowPriority bool              `yaml:"low_priority,omitempty"`
	Disabled    bool              `yaml:"disabled,omitempty"`
	Visibility  []string          `yaml:"visibility,omitempty"`
}

// MemoryConfig 是记忆配置。
type MemoryConfig struct {
	Dir string `yaml:"dir"`
}

// PlatformConfig 是任务形态配置(docs/phase2/04 §1/§2)。
// 阶段 2.11(阶段二取代阶段一):平台网关配置(base_url/token/限频/实例数)删除,
// 获取目标/提交由 skill 卡驱动,目标范围在 constraints.targets 声明。
type PlatformConfig struct {
	// phase2 任务形态配置(docs/phase2/04 §1/§2;零值 = coverage 语义)
	TaskProfile TaskProfileConfig `yaml:"task_profile"`
}

// TaskProfileConfig 是任务形态声明式配置(层 2,数据非代码)。
// 与 constraint.Policy 一一对应(docs/phase2/02 §2.5)。
// 任务描述/约束/skill 卡属 04 §1 任务描述注入:输入目标 = 配置声明,
// 不做平台对接(阶段 2.3 定案:真实靶场获取目标/提交语义一律 skill 驱动)。
type TaskProfileConfig struct {
	GoalShape      string   `yaml:"goal_shape"`            // coverage(默认) | breach
	GoalScope      string   `yaml:"goal_scope"`            // breach: specific(只朝指定目标收敛)| maximize(默认,深广混合)
	InterestMinSev string   `yaml:"interest_min_severity"` // 覆盖收敛过滤(如 high)
	InterestCWEs   []string `yaml:"interest_cwes"`         // 覆盖收敛过滤 CWE 集合
	MaxConvergence int      `yaml:"max_convergence"`       // 覆盖度收敛检查次数上限
	GoalNode       string   `yaml:"goal_node"`             // breach 目标状态节点(如 shell@dc,独立验证器判定)
	// 任务描述注入(04 §1;输入目标):进 scout/synthesizer 提示词。
	Description string `yaml:"description"` // 任务描述(范围/关注点/禁止项/提交语义,自然语言)
	// 聚焦目标(任务入口解析;非空 → 聚焦模式):任务指定目标 URL,
	// 攻击面/意图只保留该 host:port + 路径前缀(防 scout 全站测绘扩散)。
	FocusTarget string `yaml:"focus_target"`
	// 约束注入(04 §1):targets 进 Gate 外发白名单,exclusions 进黑名单。
	Constraints TaskConstraints `yaml:"constraints"`
	// Skill 卡清单(层 3 热插拔):任务相关知识包名字(平台 API/提交语义/打法),
	// 注入提示词由 agent 用 run_skill 自行加载;零代码接入靶场。
	Skills []string `yaml:"skills"`
}

// TaskConstraints 是任务级边界(04 §1 Constraints → ScopeGate + guard)。
type TaskConstraints struct {
	Targets    []string `yaml:"targets"`    // 白名单(域名/IP/CIDR,同 ScopeGate 语法)
	Exclusions []string `yaml:"exclusions"` // 黑名单(白名单内再排除)
}

// SchedulerConfig 是调度内核配置(docs/03 §2)。
type SchedulerConfig struct {
	TickInterval   string         `yaml:"tick_interval"`   // 如 "30s"
	StaleTimeout   string         `yaml:"stale_timeout"`   // 如 "1h"
	MaxConcurrency int            `yaml:"max_concurrency"` // 默认 2
	ReasonLeaseTTL string         `yaml:"reason_lease_ttl"`
	MaxTicks       int            `yaml:"max_ticks"`
	ObserverEvery  int            `yaml:"observer_every_n_turns"`
	WorkerTurns    map[string]int `yaml:"worker_turns"` // 键: scout/executor/analyst/synthesizer
}

// ModelsConfig 是多模型策略(docs/03 §7,本阶段启用路由与成本护栏)。
type ModelsConfig struct {
	RoleMap  map[string]string `yaml:"role_map"` // phase → provider name(scout/executor/analyst/synthesizer)
	Race     bool              `yaml:"race"`     // 异构模型竞争(默认关)
	Gate     ModelGateConfig   `yaml:"gate"`     // 端点级并发闸门
	Failover []string          `yaml:"failover"` // 主 provider 失败时切换的备份名(M4,§4.1)
}

// ModelGateConfig 是端点并发闸门配置(⚑ LingXi `_EndpointGate`)。
type ModelGateConfig struct {
	MaxConcurrent int    `yaml:"max_concurrent"`
	MinInterval   string `yaml:"min_interval"` // 如 "200ms"
}

// Constraints 是约束平面配置(M1 启用)。
type Constraints struct {
	MaxCostUSD            float64 `yaml:"max_cost_usd"`
	MaxTokensPerChallenge int64   `yaml:"max_tokens_per_challenge"`
	MaxTurnsPerChallenge  int     `yaml:"max_turns_per_challenge"`
}

// ServeConfig 是 HTTP 服务配置。
type ServeConfig struct {
	Addr string `yaml:"addr"`
	Auth struct {
		TokenEnv string `yaml:"token_env"`
	} `yaml:"auth"`
}

// CapabilityConfig 是 M2 能力层配置(docs/04)。
type CapabilityConfig struct {
	Tmux      TmuxConfig      `yaml:"tmux"`
	Route     RouteConfig     `yaml:"route"`
	Listener  ListenerConfig  `yaml:"listener"`
	Traffic   TrafficConfig   `yaml:"traffic"`
	Sandbox   SandboxConfig   `yaml:"sandbox"`
	Knowledge KnowledgeConfig `yaml:"knowledge"`
	Guard     GuardConfig     `yaml:"guard"`
}

// TmuxConfig 是 tmux 配置(docs/04 §2)。
type TmuxConfig struct {
	Enabled         bool   `yaml:"enabled"`           // 默认 true
	SessionPrefix   string `yaml:"session_prefix"`    // 会话命名空间前缀
	CaptureMaxLines int    `yaml:"capture_max_lines"` // capture 截断行数(默认 80)
}

// RouteConfig 是路由配置(docs/04 §3)。
type RouteConfig struct {
	Enabled bool `yaml:"enabled"` // 默认 true
}

// ListenerConfig 是反连监听器配置(docs/04 §3.4)。
type ListenerConfig struct {
	Enabled  bool `yaml:"enabled"`   // 默认 true
	BasePort int  `yaml:"base_port"` // 自动分配起点(默认 18000)
}

// TrafficConfig 是流量捕获配置(docs/04 §3.3)。
type TrafficConfig struct {
	Enabled bool `yaml:"enabled"` // 默认 true(内置代理 + 脱敏落库)
}

// SandboxConfig 是容器沙箱配置(docs/04 §5;Cairn 模式工具容器化)。
// 容器模型:容器内 root + Docker 默认 capabilities(攻击工具全功能,
// 如 nmap -sS 需 NET_RAW);显式禁用 --privileged(特权模式红线)。
type SandboxConfig struct {
	Enabled    bool   `yaml:"enabled"`    // 默认 false(编排模式可选开启)
	Image      string `yaml:"image"`      // 默认 scopeforge-attacker
	MemoryMB   int    `yaml:"memory_mb"`  // 默认 512
	Pids       int    `yaml:"pids"`       // 默认 512
	Network    string `yaml:"network"`    // 默认 bridge;host = Cairn 评测模式(容器共享宿主网络访问靶场)
	TmpfsSize  string `yaml:"tmpfs_size"` // 默认 128m
	Dockerfile string `yaml:"dockerfile"` // 镜像缺失时自动构建的 Dockerfile(默认 deploy/executor/Dockerfile)
}

// KnowledgeConfig 是漏洞知识库配置(docs/04 §6)。
type KnowledgeConfig struct {
	Enabled    bool   `yaml:"enabled"`     // 默认 true
	PluginsDir string `yaml:"plugins_dir"` // 插件目录(热插拔)
}

// GuardConfig 是确定性 Hook 配置(docs/04 §5.4/§7)。
type GuardConfig struct {
	Enabled       bool     `yaml:"enabled"`        // 默认 true
	DenyPatterns  []string `yaml:"deny_patterns"`  // 追加硬拦截模式
	ExfilPatterns []string `yaml:"exfil_patterns"` // 追加外泄检测模式
}

// Default 返回默认配置。
func Default() Config {
	return Config{
		// 默认带一个 deepseek-main 实例(openai 兼容协议),与
		// configs/scopeforge.yaml.example 保持一致。否则单二进制首次启动
		// 生成的 scopeforge.yaml 为 providers: [],serve 直接拒绝启动。
		Providers: []ProviderConfig{
			{
				Name:      "deepseek-main",
				Kind:      "openai",
				BaseURL:   "https://api.deepseek.com/v1",
				Model:     "deepseek-chat",
				APIKeyEnv: "DEEPSEEK_API_KEY",
				Pricing:   PricingConfig{Input: 0.27, Output: 1.10, CacheHit: 0.027},
			},
		},
		Agent: AgentConfig{
			MaxTurns:               200,
			ContextWindow:          65536,
			Compact:                CompactConfig{Soft: 0.5, Snip: 0.6, Compact: 0.8, Force: 0.9, Tail: 16384},
			MaxSubagentDepth:       2,
			MaxSubagentConcurrency: 6,
			MaxParallelWriters:     3,
		},
		Tools:       ToolsConfig{Permissions: "auto"},
		Memory:      MemoryConfig{Dir: "data/memory"},
		Platform:    PlatformConfig{},
		Constraints: Constraints{MaxCostUSD: 10, MaxTurnsPerChallenge: 300},
		Scheduler: SchedulerConfig{
			TickInterval: "30s", StaleTimeout: "1h", MaxConcurrency: 2,
			ReasonLeaseTTL: "10m", MaxTicks: 120, ObserverEvery: 3,
		},
		Serve: ServeConfig{Addr: ":8080"},
		Capability: CapabilityConfig{
			Tmux:      TmuxConfig{Enabled: true, SessionPrefix: "pf", CaptureMaxLines: 80},
			Route:     RouteConfig{Enabled: true},
			Listener:  ListenerConfig{Enabled: true, BasePort: 18000},
			Traffic:   TrafficConfig{Enabled: true},
			Sandbox:   SandboxConfig{Enabled: false, Image: "scopeforge-attacker", MemoryMB: 512, Pids: 512, Network: "bridge", TmpfsSize: "128m", Dockerfile: "deploy/executor/Dockerfile"},
			Knowledge: KnowledgeConfig{Enabled: true, PluginsDir: "plugins"},
			Guard:     GuardConfig{Enabled: true},
		},
	}
}

var envRefRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(:-([^}]*))?\}`)

// ExpandEnv 展开 ${VAR} 与 ${VAR:-default} 引用(未设且无默认 → 空串)。
func ExpandEnv(s string) string {
	return envRefRe.ReplaceAllStringFunc(s, func(m string) string {
		sub := envRefRe.FindStringSubmatch(m)
		name := sub[1]
		if v, ok := os.LookupEnv(name); ok {
			return v
		}
		return sub[3]
	})
}

// Load 加载配置:先默认值,再合并 scopeforge.yaml(路径为空则跳过)。
// 返回错误含文件定位。
func Load(path string) (Config, error) {
	cfg := Default()
	if path == "" {
		return cfg, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("config %s: %w", path, err)
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("config %s: %w", path, err)
	}
	// ${ENV} 展开(密钥引用与 URL 等)
	for i := range cfg.Providers {
		cfg.Providers[i].BaseURL = ExpandEnv(cfg.Providers[i].BaseURL)
		cfg.Providers[i].APIKeyEnv = ExpandEnv(cfg.Providers[i].APIKeyEnv)
	}
	cfg.Serve.Addr = ExpandEnv(cfg.Serve.Addr)
	// 校验
	for i, p := range cfg.Providers {
		if p.Name == "" || p.Kind == "" || p.BaseURL == "" || p.Model == "" {
			return cfg, fmt.Errorf("config %s: providers[%d] requires name/kind/base_url/model", path, i)
		}
		switch p.Kind {
		case "openai", "anthropic":
		default:
			return cfg, fmt.Errorf("config %s: providers[%d].kind %q unknown (openai|anthropic)", path, i, p.Kind)
		}
	}
	switch cfg.Tools.Permissions {
	case "auto", "yolo":
	default:
		return cfg, fmt.Errorf("config %s: tools.permissions %q invalid (auto|yolo;ask 已移除,06.14)", path, cfg.Tools.Permissions)
	}
	return cfg, nil
}
