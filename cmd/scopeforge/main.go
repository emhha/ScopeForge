// Command scopeforge 是 ScopeForge 入口(docs/02 §8.3):
//
//	scopeforge serve   # chi 路由 + SSE 骨架(M3 填前端;本期 /health /api/v1/events)
//	scopeforge run     # 单次任务:加载配置 → 建会话 → 跑 Executor → 输出结果
//	scopeforge doctor  # 环境自检(provider 连通、docker、网络)
//	scopeforge bench   # M4 实现,本期占位
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"scopeforge/configs"
	"scopeforge/internal/bench"
	"scopeforge/internal/build"
	"scopeforge/internal/config"
	"scopeforge/internal/event"
	"scopeforge/internal/executor"
	"scopeforge/internal/reasonix/provider"
)

var version = "0.1.0-m0"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = cmdServe(os.Args[2:])
	case "run":
		err = cmdRun(os.Args[2:])
	case "doctor":
		err = cmdDoctor(os.Args[2:])
	case "bench":
		err = cmdBench(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Println("scopeforge", version)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "scopeforge: unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "scopeforge:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Print(`ScopeForge — 安全渗透智能体平台

用法:
  scopeforge serve [--config DIR] [--addr :8080]
  scopeforge run   [--config DIR] [--task TEXT]
                 [--permission-mode auto|yolo]
  scopeforge doctor [--config DIR]
  scopeforge bench  [--set src|juice] [--llm-base URL --llm-model NAME --llm-key-env ENV]
                  [--target-url URL] [--max-cost USD] [--out-dir DIR] [--challenge-id ID]
  scopeforge version

--config DIR  配置文件夹:配置(scopeforge.yaml)/密钥(.env)/数据库(scopeforge.db)
              统一落此目录;默认 SCOPEFORGE_HOME > ~/.scopeforge。
              首次启动自动创建 scopeforge.yaml 与 .env(密钥填 ~/.scopeforge/.env)。

任务模式(--task TEXT):
  任务描述注入调度器(04 §1),黑板编排 bootstrap→explore→conclude;
  结果进黑板/账本/报告(与 serve + API 的 mode=task 等价)。

评测模式(bench,阶段二 02 §3 + 11 §3-④):
  --set src     mock SRC 靶场(预埋漏洞)+ 内置 scripted mock LLM,离线验证管道(默认)
  --set juice   本地 Juice Shop 真实靶场,必须配真实 LLM(--llm-base 等),
                recall 度量真实挖掘能力;JSONL 记账带 llm_mode(mock/real)分开统计
`)
}

// dataDirFor 解析运行时数据目录(配置文件夹):--config 显式指定优先,
// 否则 SCOPEFORGE_HOME > ~/.scopeforge。
func dataDirFor(explicit string) string {
	if explicit != "" {
		return explicit
	}
	return build.DefaultDataDir()
}

// resolveConfigPath 解析配置文件路径:配置文件夹下固定为 scopeforge.yaml;
// 默认(无 --config)时按序查找 <dataDir>/scopeforge.yaml → configs/scopeforge.yaml
// → ./scopeforge.yaml,开箱即用(无需手动 --config)。找不到返回空串(调用方按无配置处理)。
func resolveConfigPath(dataDir string) string {
	if p := filepath.Join(dataDir, "scopeforge.yaml"); fileExists(p) {
		return p
	}
	if dataDir == build.DefaultDataDir() { // 仅默认目录回退源码样例(只读沙箱/未初始化)
		for _, c := range []string{"configs/scopeforge.yaml", "scopeforge.yaml"} {
			if fileExists(c) {
				return c
			}
		}
	}
	return ""
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// ensureEnvFile 首次初始化时在数据目录创建 .env(默认即 ~/.scopeforge/.env)。
// 实现统一放在 internal/build,保证 CLI 与直接装配 build.New 的行为一致。
func ensureEnvFile(dataDir string) error {
	return build.EnsureEnvFile(dataDir)
}

// ensureConfig 首次启动在数据目录(配置文件夹)生成 scopeforge.yaml 与 .env:
// 源码环境优先拷贝 configs/(开发迭代即时生效);单二进制部署目录无 configs/
// 时用内嵌样例(configs/embed.go,go:embed,分发无需携带源码),最后兜底
// 内置默认值(Default 含 deepseek-main provider,保证 serve 可启动)。
// 已存在不覆盖(用户改过的配置/密钥保留)。
func ensureConfig(dataDir string) error {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return err
	}
	if err := ensureEnvFile(dataDir); err != nil {
		return err
	}

	target := filepath.Join(dataDir, "scopeforge.yaml")
	if fileExists(target) {
		return nil // 已存在,不覆盖
	}
	for _, sample := range []string{"configs/scopeforge.yaml", "configs/scopeforge.yaml.example"} {
		if data, err := os.ReadFile(sample); err == nil {
			return os.WriteFile(target, data, 0o644)
		}
	}
	if len(configs.Example) > 0 {
		return os.WriteFile(target, configs.Example, 0o644)
	}
	data, err := yaml.Marshal(config.Default())
	if err != nil {
		return err
	}
	return os.WriteFile(target, data, 0o644)
}

// cmdServe 启动 HTTP 服务。
func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	cfgDir := fs.String("config", "", "配置文件夹路径(空 = SCOPEFORGE_HOME > ~/.scopeforge)")
	addr := fs.String("addr", "", "监听地址(默认读配置,再默认 :8080)")
	fs.Parse(args)

	dataDir := dataDirFor(*cfgDir)
	// 首次启动生成 <dataDir>/scopeforge.yaml(配置)等运行时数据
	if err := ensureConfig(dataDir); err != nil {
		return err
	}
	app, err := build.New(build.Options{
		DataDir:    dataDir,
		ConfigPath: filepath.Join(dataDir, "scopeforge.yaml"),
	})
	if err != nil {
		return err
	}
	defer app.Close()

	// fail-fast:无 provider 时启动即报错(否则任务发起时才 500,
	// 且用户难以定位——通常 serve 未带 --config 加载 providers 段)。
	if len(app.Providers) == 0 {
		return fmt.Errorf("serve: 配置缺少 providers 段(在 %s/scopeforge.yaml 的 providers 添加至少一个实例,或在 Web 配置页保存 provider)",
			dataDir)
	}

	// 孤儿攻击容器清理:进程强杀时 RemoveForChallenge 的 defer 不执行,
	// 残留容器在此收敛(一题一容器,环境重置语义)。
	if app.Sandbox != nil {
		if n := app.Sandbox.CleanupOrphans(context.Background()); n > 0 {
			fmt.Printf("scopeforge serve: 清理 %d 个残留攻击容器\n", n)
		}
	}

	listen := *addr
	if listen == "" {
		listen = app.Cfg.Serve.Addr
	}
	srv := &http.Server{
		Addr:    listen,
		Handler: app.APIHandler().Handler(),
	}
	// 优雅关闭
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	fmt.Printf("scopeforge serve: listening on %s (home=%s)\n", listen, dataDir)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// cmdRun 无头执行:任务文本入口(阶段 2.15 起走黑板编排,
// 与 serve + API 的 mode=task 等价;等 run_done 后输出结果)。
func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	cfgDir := fs.String("config", "", "配置文件夹路径(空 = SCOPEFORGE_HOME > ~/.scopeforge)")
	task := fs.String("task", "", "任务描述(输入目标;注入 bootstrap 提示词)")
	perm := fs.String("permission-mode", "", "权限模式覆盖:auto|yolo")
	fs.Parse(args)

	if *task == "" {
		return fmt.Errorf("run: --task 必填(任务描述;黑板编排入口)")
	}
	dataDir := dataDirFor(*cfgDir)
	if err := ensureConfig(dataDir); err != nil {
		return err
	}
	app, err := build.New(build.Options{
		DataDir:    dataDir,
		ConfigPath: filepath.Join(dataDir, "scopeforge.yaml"),
	})
	if err != nil {
		return err
	}
	defer app.Close()

	if len(app.Providers) == 0 {
		return fmt.Errorf("run: 配置中没有 providers(见 docs/02 §1.3)")
	}
	if *perm != "" {
		switch executor.PermissionMode(*perm) {
		case executor.ModeAuto, executor.ModeYolo:
			app.Gate.Mode = executor.PermissionMode(*perm)
		default:
			return fmt.Errorf("run: 非法权限模式 %q(auto|yolo;ask 已移除,06.14)", *perm)
		}
	}

	// 捕获 run_done(透传全部事件,不吞其他任务/会话事件)
	done := make(chan event.Event, 1)
	base := app.Sink
	app.Sink = event.FuncSink(func(e event.Event) {
		if e.Kind == event.KindRunDone {
			select {
			case done <- e:
			default:
			}
		}
		if base != nil {
			base.Emit(e)
		}
	})

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	runID, err := app.RunTask(ctx, *task) // 聚焦目标从任务文本提取(CLI 无 platform_url 字段)
	if err != nil {
		return err
	}
	fmt.Printf("任务已启动: %s(mode=task;事件流透传)\n", runID)

	select {
	case e := <-done:
		p, _ := e.Payload.(map[string]any)
		fmt.Println("=== 任务结果 ===")
		fmt.Printf("mode=%v terminated=%v reason=%v turns=%v\n",
			p["mode"], p["terminated"], p["reason"], p["turns"])
		if rp, ok := p["report_path"].(string); ok && rp != "" {
			fmt.Printf("report=%s\n", rp)
		}
		if em, ok := p["error"].(string); ok && em != "" {
			return fmt.Errorf("任务失败: %s", em)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// cmdBench 阶段二评测(docs/phase2/02 §3.2 + 11 §3-④):
//
//	scopeforge bench --set src   (mock 靶场 + 内置 scripted mock LLM,离线验证管道)
//	scopeforge bench --set juice --llm-base URL --llm-model NAME --llm-key-env ENV
//	               [--target-url URL] [--max-cost USD]
//	               [--out-dir DIR] [--challenge-id ID]
//
// --set src 默认 mock LLM 离线闭环(验证机制);--set juice 打本地 Juice Shop
// 真实靶场,必须真实 LLM——recall 度量真实挖掘能力(README 验收底线 1:
// recall ≥ 60% / fpr ≤ 20%)。JSONL 记账带 llm_mode(mock/real)分开统计。
func cmdBench(args []string) error {
	fs := flag.NewFlagSet("bench", flag.ExitOnError)
	setName := fs.String("set", "src", "评测集:src(mock SRC 靶场) | juice(本地 Juice Shop 真实靶场,需真实 LLM)")
	llmBase := fs.String("llm-base", "", "真实 LLM 端点(空 = 内置 scripted mock LLM,仅 --set src)")
	llmModel := fs.String("llm-model", "", "真实 LLM 模型名")
	llmKeyEnv := fs.String("llm-key-env", "", "真实 LLM API key 环境变量名")
	targetURL := fs.String("target-url", "", "juice 集目标地址(空 = http://127.0.0.1:3000)")
	maxCost := fs.Float64("max-cost", 0, "成本熔断 USD(0 = 默认:mock 不限,real 3.0)")
	outDir := fs.String("out-dir", "", "输出目录(bench-runs.jsonl;空 = 不落盘)")
	challengeID := fs.String("challenge-id", "", "任务 id(空 = bench-<时间戳>)")
	fs.Parse(args)

	// bench 不走 build.New;真实 LLM key 也允许从 ~/.scopeforge/.env 解析。
	build.LoadDotEnv(build.DefaultDataDir())

	r, err := bench.NewRunner(bench.RunnerConfig{
		SetName:     *setName,
		TargetURL:   *targetURL,
		LLMBase:     *llmBase,
		LLMModel:    *llmModel,
		LLMKeyEnv:   *llmKeyEnv,
		MaxCostUSD:  *maxCost,
		OutDir:      *outDir,
		ChallengeID: *challengeID,
	})
	if err != nil {
		return err
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	out, err := r.Run(ctx)
	if err != nil {
		return err
	}

	m := out.Metrics
	recallOK, fprOK := m.Pass(bench.DefaultThresholds())
	fmt.Printf("=== bench %s (%s) llm=%s/%s ===\n", out.SetName, out.ChallengeID, out.LLMMode, out.Model)
	fmt.Printf("terminated=%v reason=%s concluded=%v turns=%d cost=%.4f USD\n",
		out.Terminated, out.Reason, out.Concluded, m.Turns, m.CostUSD)
	fmt.Printf("recall=%.0f%% (%d/%d) found=%v missed=%v\n",
		m.Recall*100, len(m.Found), len(m.Found)+len(m.Missed), m.Found, m.Missed)
	fmt.Printf("fpr=%.1f%% (%d/%d) accepted=%d duplicate=%d false_positive=%d\n",
		m.FPR*100, m.FalsePositive, m.TotalSubmissions, m.Accepted, m.Duplicate, m.FalsePositive)
	fmt.Printf("coverage=%.0f%% converged=%v efficiency=%.4f USD/漏洞\n",
		m.Coverage*100, m.Converged, m.Efficiency)
	fmt.Printf("判定: recall>=%.0f%%? %v | fpr<=%.0f%%? %v\n",
		bench.DefaultThresholds().RecallMin*100, recallOK,
		bench.DefaultThresholds().FPRMax*100, fprOK)
	return nil
}

// cmdDoctor 环境自检(docs/04 §4.3 幽灵工具防线 + 工具后端检测)。
func cmdDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	cfgDir := fs.String("config", "", "配置文件夹路径(空 = SCOPEFORGE_HOME > ~/.scopeforge)")
	fs.Parse(args)

	ok := true
	check := func(name string, err error) {
		if err != nil {
			ok = false
			fmt.Printf("[FAIL] %s: %v\n", name, err)
		} else {
			fmt.Printf("[ OK ] %s\n", name)
		}
	}

	// 配置加载
	dataDir := dataDirFor(*cfgDir)
	cfg, err := loadConfigForDoctor(resolveConfigPath(dataDir))
	check("配置加载", err)
	if err == nil {
		fmt.Printf("       providers: %d\n", len(cfg.Providers))
	}

	// provider 注册表
	kinds := provider.Kinds()
	fmt.Printf("       provider kinds: %v\n", kinds)

	// docker 可用性
	check("docker", checkDocker())

	// 宿主机能力工具(docs/04 §2/§3;缺失时相关工具降级或明确报错)
	for _, bin := range []string{"tmux", "chisel", "stowaway", "proxychains4", "mitmdump"} {
		check("binary: "+bin, checkBinary(bin))
	}

	// 攻击工具在 scopeforge-attacker(Kali)镜像内,不检查宿主机 PATH;
	// 检查镜像是否存在,避免 skill 卡声称的工具面落空。
	if err == nil && cfg.Capability.Sandbox.Enabled {
		image := cfg.Capability.Sandbox.Image
		if image == "" {
			image = "scopeforge-attacker"
		}
		check("attack-image: "+image, checkDockerImage(image))
	}
	_ = ok
	return nil
}

// checkBinary 检查二进制可用性。
func checkBinary(name string) error {
	if _, err := exec.LookPath(name); err != nil {
		return fmt.Errorf("未找到 %s(相关工具将降级或明确报错)", name)
	}
	return nil
}

// loadConfigForDoctor 加载配置(doctor 用)。
func loadConfigForDoctor(path string) (config.Config, error) {
	return config.Load(path)
}

// checkDocker 检查 docker CLI 可用性。
func checkDocker() error {
	cmd := exec.Command("docker", "version", "--format", "{{.Server.Version}}")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker CLI 不可用: %w", err)
	}
	return nil
}

// checkDockerImage 检查攻击镜像是否已构建(kali 工具面由镜像提供)。
func checkDockerImage(image string) error {
	cmd := exec.Command("docker", "image", "inspect", image)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("镜像 %s 不存在(构建: docker build -f deploy/executor/Dockerfile -t %s .)", image, image)
	}
	return nil
}
