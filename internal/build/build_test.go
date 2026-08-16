package build

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"scopeforge/internal/config"
	"scopeforge/internal/constraint"
)

func TestTaskPolicyDefaultsToCoverage(t *testing.T) {
	// 未配置 task_profile → coverage 语义(默认形态,阶段 2.11)
	p := taskPolicy(config.Config{})
	if p.GoalShape != constraint.GoalShapeCoverage {
		t.Fatalf("default policy = %+v, want coverage", p)
	}
}

func TestTaskPolicyCoverage(t *testing.T) {
	cfg := config.Config{}
	cfg.Platform.TaskProfile = config.TaskProfileConfig{
		GoalShape:      constraint.GoalShapeCoverage,
		InterestMinSev: "high",
		MaxConvergence: 5,
	}
	p := taskPolicy(cfg)
	if p.GoalShape != constraint.GoalShapeCoverage {
		t.Fatalf("coverage policy = %+v", p)
	}
	if p.Interest.SeverityMin != "high" || p.MaxConvergence != 5 {
		t.Fatalf("interest/convergence = %+v", p)
	}
}

func TestTaskPolicyBreach(t *testing.T) {
	cfg := config.Config{}
	cfg.Platform.TaskProfile = config.TaskProfileConfig{GoalShape: constraint.GoalShapeBreach}
	p := taskPolicy(cfg)
	if p.GoalShape != constraint.GoalShapeBreach {
		t.Fatalf("breach policy = %+v", p)
	}
}

// TestValidateTaskProfile 验收(04 §4):非法组合启动即拒绝——
// coverage+goal_scope / coverage+goal_node / 非法 goal_shape / 非法 goal_scope /
// specific 无 goal_node / 非法 interest_min_severity;合法组合放行。
func TestValidateTaskProfile(t *testing.T) {
	tp := func(mut func(*config.TaskProfileConfig)) config.Config {
		cfg := config.Default()
		mut(&cfg.Platform.TaskProfile)
		return cfg
	}
	cases := []struct {
		name    string
		cfg     config.Config
		wantErr bool
	}{
		{"默认 coverage 合法", tp(func(p *config.TaskProfileConfig) {}), false},
		{"coverage+goal_scope 非法", tp(func(p *config.TaskProfileConfig) {
			p.GoalScope = "specific"
		}), true},
		{"coverage+goal_node 非法", tp(func(p *config.TaskProfileConfig) {
			p.GoalNode = "state@dc"
		}), true},
		{"breach 合法", tp(func(p *config.TaskProfileConfig) {
			p.GoalShape = "breach"
			p.GoalNode = "state@dc"
		}), false},
		{"breach+specific 无 goal 非法", tp(func(p *config.TaskProfileConfig) {
			p.GoalShape = "breach"
			p.GoalScope = "specific"
		}), true},
		{"非法 goal_shape", tp(func(p *config.TaskProfileConfig) {
			p.GoalShape = "ctf"
		}), true},
		{"非法 goal_scope", tp(func(p *config.TaskProfileConfig) {
			p.GoalShape = "breach"
			p.GoalScope = "deep"
		}), true},
		{"非法 interest_min_severity", tp(func(p *config.TaskProfileConfig) {
			p.InterestMinSev = "urgent"
		}), true},
		{"合法 interest", tp(func(p *config.TaskProfileConfig) {
			p.InterestMinSev = "high"
		}), false},
	}
	for _, c := range cases {
		err := validateTaskProfile(c.cfg)
		if (err != nil) != c.wantErr {
			t.Errorf("%s: err = %v, wantErr %v", c.name, err, c.wantErr)
		}
	}
}

// ------------------------------------------------------------------ RunTask 黑板编排(阶段 2.15)

// runTaskLLM 是 scripted mock LLM(按角色路由契约,记录 system 提示词)。
type runTaskLLM struct {
	mu         sync.Mutex
	srv        *httptest.Server
	sysPrompts []string
	// scoutBadOnce:首次 bootstrap 输出非法文本(模拟真实 LLM 契约格式错误),
	// 验证 unparseable → failed → 重试链路。
	scoutBadOnce bool
	// scoutBadAll:所有 bootstrap 都输出非法文本(重试耗尽 → focus_seed 兜底)。
	scoutBadAll bool
}

func newRunTaskLLM(t *testing.T) *runTaskLLM {
	t.Helper()
	m := &runTaskLLM{}
	mux := http.NewServeMux()
	handler := func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(body, &req)
		sys := ""
		for _, msg := range req.Messages {
			if sys == "" {
				sys = msg.Content
			}
		}
		m.mu.Lock()
		m.sysPrompts = append(m.sysPrompts, sys)
		m.mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl := w.(http.Flusher)
		write := func(v any) {
			data, _ := json.Marshal(v)
			fmt.Fprintf(w, "data: %s\n\n", data)
			fl.Flush()
		}
		usage := map[string]any{"prompt_tokens": 100, "completion_tokens": 20, "total_tokens": 120}
		contract := `{"accepted":true,"findings":[],"new_intents":[],"dead_ends":[],"stop_reason":"conclude"}`
		switch {
		case strings.Contains(sys, "Scout（聚焦模式）"):
			m.mu.Lock()
			bad := m.scoutBadOnce || m.scoutBadAll
			m.scoutBadOnce = false
			m.mu.Unlock()
			if bad {
				contract = `这是模型首次输出的非契约文本(应触发 unparseable → failed 重试)`
				break
			}
			// 聚焦侦察:产出聚焦条目 + 一条越界条目(聚焦模式应丢弃)
			contract = `{"accepted":true,"findings":[],"new_intents":[{"text":"对 /rest/products/search 做 SQL 注入测试","weight":0.9,"target":"/rest/products/search","approach":"probe"},{"text":"越界:注册用户拿 IDOR 面","weight":0.8,"target":"/api/Users","approach":"IDOR"}],"dead_ends":[],"stop_reason":"intent_done","attack_surface":[{"asset":"192.168.81.167","endpoint":"/rest/products/search","params":["q"],"tech":"express","notes":"任务指定端点"},{"asset":"shop.example.com","endpoint":"/api/Users","params":[],"tech":"express","notes":"越界条目"}]}`
		case strings.Contains(sys, "Scout。"):
			m.mu.Lock()
			bad := m.scoutBadOnce || m.scoutBadAll
			m.scoutBadOnce = false
			m.mu.Unlock()
			if bad {
				contract = `这是模型首次输出的非契约文本(应触发 unparseable → failed 重试)`
				break
			}
			// scout:依任务描述建攻击面——一条聚焦条目(目标主机/端点)
			// + 一条越界条目(其他主机/端点,聚焦模式应丢弃)
			contract = `{"accepted":true,"findings":[],"new_intents":[{"text":"对 /rest/products/search 做 SQL 注入测试","weight":0.9,"target":"/rest/products/search","approach":"probe"},{"text":"越界:注册用户拿 IDOR 面","weight":0.8,"target":"/api/Users","approach":"IDOR"}],"dead_ends":[],"stop_reason":"intent_done","attack_surface":[{"asset":"192.168.81.167","endpoint":"/rest/products/search","params":["q"],"tech":"express","notes":"任务指定端点"},{"asset":"shop.example.com","endpoint":"/api/Users","params":[],"tech":"express","notes":"越界条目"}]}`
		case strings.Contains(sys, "Executor。"):
			// executor:直接返回 vuln finding 合约(不做工具调用,聚焦模式即停)
			contract = `{"accepted":true,"findings":[{"prefix":"vuln:","text":"q 参数 SQL 注入(UNION 提取 Users 表)","weight":0.95,"evidence_ref":"exec-e1","cwe":"CWE-89","asset":"192.168.81.167","endpoint":"/rest/products/search","severity":"high"}],"new_intents":[],"dead_ends":[],"stop_reason":"intent_done"}`
		case strings.Contains(sys, "Synthesizer。"):
			contract = `{"accepted":true,"findings":[],"new_intents":[],"dead_ends":[],"stop_reason":"conclude","exhausted":true}`
		}
		write(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": contract}}}})
		write(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": "stop"}}, "usage": usage})
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	}
	mux.HandleFunc("/v1/chat/completions", handler)
	mux.HandleFunc("/chat/completions", handler)
	m.srv = httptest.NewServer(mux)
	t.Cleanup(m.srv.Close)
	return m
}

func (m *runTaskLLM) URL() string { return m.srv.URL }

// firstSystemFor 取最近一次含 role 的 system 提示词(断言任务描述注入)。
func (m *runTaskLLM) firstSystemFor(t *testing.T, role string) string {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := len(m.sysPrompts) - 1; i >= 0; i-- {
		if strings.Contains(m.sysPrompts[i], role) {
			return m.sysPrompts[i]
		}
	}
	t.Fatalf("未找到 %q 角色的 system 提示词", role)
	return ""
}

func TestEnsureEnvFile(t *testing.T) {
	dir := t.TempDir()
	if err := EnsureEnvFile(dir); err != nil {
		t.Fatalf("EnsureEnvFile: %v", err)
	}
	path := filepath.Join(dir, ".env")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "DEEPSEEK_API_KEY=") || !strings.Contains(string(data), "SCOPEFORGE_TOKEN=") {
		t.Errorf("generated .env missing key template:\n%s", data)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf(".env mode = %o, want 600", perm)
	}
}

func TestLoadDotEnvDataDir(t *testing.T) {
	// 新语义(reasonix 风格):进程环境 > ~/.scopeforge/.env(SCOPEFORGE_HOME)
	// > <dataDir>/.env > 当前目录 .env。
	home := t.TempDir()
	t.Setenv("SCOPEFORGE_HOME", home)
	if err := os.WriteFile(filepath.Join(home, ".env"), []byte("HOME_KEY=home-win\nDEEPSEEK_API_KEY=sk-home\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("DEEPSEEK_API_KEY=sk-data-dir\nSCOPEFORGE_TOKEN=tok-data-dir\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("EXISTING_KEY", "env-win")
	if err := os.WriteFile(".env", []byte("EXISTING_KEY=cwd\nDEEPSEEK_API_KEY=sk-cwd\nCWD_KEY=cwd-win\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(".env") })
	unsetEnv := func(k string) { _ = os.Unsetenv(k) }
	for _, k := range []string{"HOME_KEY", "DEEPSEEK_API_KEY", "SCOPEFORGE_TOKEN", "EXISTING_KEY", "CWD_KEY"} {
		key := k
		t.Cleanup(func() { unsetEnv(key) })
	}

	loadDotEnv(dir)
	if got := os.Getenv("DEEPSEEK_API_KEY"); got != "sk-home" {
		t.Fatalf("DEEPSEEK_API_KEY = %q, want ~/.scopeforge/.env 优先", got)
	}
	if got := os.Getenv("HOME_KEY"); got != "home-win" {
		t.Fatalf("HOME_KEY = %q, want 默认 home .env 生效", got)
	}
	if got := os.Getenv("SCOPEFORGE_TOKEN"); got != "tok-data-dir" {
		t.Fatalf("SCOPEFORGE_TOKEN = %q, want 数据目录 .env 兜底", got)
	}
	if got := os.Getenv("CWD_KEY"); got != "cwd-win" {
		t.Fatalf("CWD_KEY = %q, want 当前目录 .env 最后兜底", got)
	}
	if got := os.Getenv("EXISTING_KEY"); got != "env-win" {
		t.Fatalf("EXISTING_KEY = %q, want 真实环境变量最高优先", got)
	}
}

// TestRunTaskBoardOrchestration 验收(阶段 2.15):RunTask 走黑板编排——
// task 文本注入 scout 提示词(04 §1 任务描述注入),bootstrap 建攻击面
// 格子,explore 认领执行并登记漏洞(账本/facts),任务终止 + 报告生成。
// 单 agent 直跑(executor.Run)在 task 入口已删除。
func TestRunTaskBoardOrchestration(t *testing.T) {
	llm := newRunTaskLLM(t)
	t.Setenv("SCOPEFORGE_LLM_KEY", "test")
	tmp := t.TempDir()
	t.Setenv("SCOPEFORGE_HOME", tmp) // skill 物化到测试临时目录
	cfgText := fmt.Sprintf(`providers:
  - name: mock
    kind: openai
    base_url: %s
    model: mock-model
    api_key_env: SCOPEFORGE_LLM_KEY
scheduler:
  tick_interval: 20ms
  max_ticks: 80
  max_concurrency: 2
  observer_every_n_turns: 0
  worker_turns:
    recon: 3
    explore: 3
    reason: 3
    synthesizer: 3
tools:
  permissions: yolo
  blacklist: []
  domain_allowlist: ["127.0.0.1"]
skills:
  dir: %s/skills
memory:
  dir: %s/memory
capability:
  knowledge:
    plugins_dir: %s/kb
`, llm.URL(), tmp, tmp, tmp)
	cfgPath := filepath.Join(tmp, "scopeforge.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfgText), 0o644); err != nil {
		t.Fatal(err)
	}

	app, err := New(Options{ConfigPath: cfgPath, DBPath: filepath.Join(tmp, "test.db"), WorkDir: tmp})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	app.Sandbox = nil // 测试禁用容器(worker 契约不依赖 sandbox)

	taskText := "对 http://192.168.81.167:3000/rest/products/search 进行 SQL 注入检测"
	runID, err := app.RunTask(context.Background(), taskText)
	if err != nil {
		t.Fatal(err)
	}

	// 等 run_done(事件落库;30s 上限,正常 <5s)
	deadline := time.Now().Add(30 * time.Second)
	var payloadStr string
	for time.Now().Before(deadline) {
		err := app.DB.QueryRow(
			`SELECT payload FROM events WHERE kind='run_done' AND challenge_id=? ORDER BY seq DESC LIMIT 1`,
			runID).Scan(&payloadStr)
		if err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if payloadStr == "" {
		t.Fatalf("run_done 未在 30s 内到达(challenge_id=%s)", runID)
	}
	var p map[string]any
	if err := json.Unmarshal([]byte(payloadStr), &p); err != nil {
		t.Fatalf("run_done payload: %v", err)
	}
	if p["terminated"] != true {
		t.Fatalf("terminated = %v, want true(reason=%v)", p["terminated"], p["reason"])
	}
	rp, _ := p["report_path"].(string)
	if rp == "" {
		t.Fatal("report_path 为空(黑板编排应生成报告)")
	}
	if _, err := os.Stat(rp); err != nil {
		t.Fatalf("报告文件不存在: %s", rp)
	}

	// worker 轨迹:scout(侦察)一次 + executor(挖掘)认领执行(单 agent 直跑无此轨迹)
	rows, err := app.DB.Query(`SELECT worker_type, phase, status FROM workers WHERE challenge_id=?`, runID)
	if err != nil {
		t.Fatal(err)
	}
	recons := 0
	explores := 0
	running := []string{}
	for rows.Next() {
		var wt, ph, st string
		if err := rows.Scan(&wt, &ph, &st); err != nil {
			t.Fatal(err)
		}
		if wt == "operator" && ph == "scout" {
			recons++
		}
		if wt == "operator" && ph == "executor" {
			explores++
		}
		if st == "running" {
			running = append(running, wt)
		}
	}
	rows.Close()
	if len(running) > 0 {
		t.Fatalf("存在未收尾 worker: %v", running)
	}
	if recons != 1 {
		t.Fatalf("scout workers = %d, want 1", recons)
	}
	if explores < 1 {
		t.Fatalf("executor workers = %d, want >=1(executor 应认领攻击面方向)", explores)
	}

	// 漏洞账本(executor 产出经 Dispatcher 唯一写入;
	// 模拟聚焦直接产出 vuln finding,不经 submit_vulnerability 工具)
	var vulnCWE, vulnEP, vulnStatus string
	if err := app.DB.QueryRow(`SELECT cwe, endpoint, status FROM vulnerability_ledger WHERE challenge_id=? ORDER BY id DESC LIMIT 1`,
		runID).Scan(&vulnCWE, &vulnEP, &vulnStatus); err == nil {
		if vulnCWE != "CWE-89" || !strings.Contains(vulnEP, "/rest/products/search") {
			t.Fatalf("账本 cwe=%s endpoint=%s, want CWE-89@/rest/products/search", vulnCWE, vulnEP)
		}
	}

	// 黑板 facts 有 vuln: 前缀
	var n int
	if err := app.DB.QueryRow(`SELECT COUNT(*) FROM facts WHERE challenge_id=? AND prefix='vuln:'`, runID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n < 1 {
		t.Fatal("facts 无 vuln: 前缀事实")
	}

	// 攻击面 intent(scout 产出 → executor 认领)
	var itState string
	err = app.DB.QueryRow(`SELECT state FROM intents WHERE challenge_id=? AND target LIKE '%products/search%' ORDER BY id LIMIT 1`,
		runID).Scan(&itState)
	if err != nil {
		t.Fatalf("intents 无攻击面方向: %v", err)
	}

	// 聚焦模式(阶段 2.21):任务文本含目标 URL → FocusTarget 自动提取,
	// 越界意图(/api/Users,其他主机)必须被 focusFilter 丢弃
	var outOfScope int
	if err := app.DB.QueryRow(`SELECT COUNT(*) FROM intents WHERE challenge_id=? AND target LIKE '%Users%'`, runID).Scan(&outOfScope); err != nil {
		t.Fatal(err)
	}
	if outOfScope > 0 {
		t.Fatalf("聚焦模式未过滤越界意图: /api/Users intents = %d", outOfScope)
	}
	var hint string
	if err := app.DB.QueryRow(`SELECT CAST(payload AS TEXT) FROM events WHERE challenge_id=? AND CAST(payload AS TEXT) LIKE '%focus_filter%' ORDER BY seq DESC LIMIT 1`, runID).Scan(&hint); err != nil {
		t.Fatalf("focus_filter hint 事件缺失: %v", err)
	}

	// 任务描述注入验证(04 §1):scout 提示词含用户 task 文本
	sys := llm.firstSystemFor(t, "Scout")
	if !strings.Contains(sys, taskText) {
		t.Fatal("scout 提示词未注入任务描述(04 §1)")
	}
}

// TestAPIKeyFor 验收:API key 解析优先级(环境变量 > .env 兜底)与 .env 语法
// (注释/空值/引号)。serve/run 未 source .env 时不再 401。
func TestAPIKeyFor(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SCOPEFORGE_HOME", t.TempDir()) // 隔离真实 ~/.scopeforge
	t.Chdir(dir)
	if err := os.WriteFile(".env", []byte("# comment\nEMPTY=\nQUOTED=\"abc123\"\nPLAIN=xyz789\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := apiKeyFor("MISSING"); got != "" {
		t.Fatalf("apiKeyFor(MISSING) = %q, want 空", got)
	}
	if got := apiKeyFor("QUOTED"); got != "abc123" {
		t.Fatalf("apiKeyFor(QUOTED) = %q, want abc123(去引号)", got)
	}
	if got := apiKeyFor("PLAIN"); got != "xyz789" {
		t.Fatalf("apiKeyFor(PLAIN) = %q, want xyz789", got)
	}
	// 环境变量优先于 .env
	t.Setenv("QUOTED", "env-win")
	if got := apiKeyFor("QUOTED"); got != "env-win" {
		t.Fatalf("apiKeyFor(QUOTED) = %q, want env-win(环境变量优先)", got)
	}
	// 无 .env 文件时静默空
	empty := t.TempDir()
	t.Chdir(empty)
	if got := apiKeyFor("PLAIN"); got != "" {
		t.Fatalf("无 .env 时 apiKeyFor = %q, want 空", got)
	}
	// 切走环境变量后仍能回退 .env
	t.Setenv("QUOTED", "")
	t.Chdir(dir)
	if got := apiKeyFor("QUOTED"); got != "abc123" {
		t.Fatalf("空环境变量回退 .env: got %q, want abc123", got)
	}
}

// TestAPIKeyForHomeFirst 验收 reasonix 风格密钥位置:
// ~/.scopeforge/.env(SCOPEFORGE_HOME)> <dataDir>/.env > 当前目录 .env。
func TestAPIKeyForHomeFirst(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SCOPEFORGE_HOME", home)
	if err := os.WriteFile(filepath.Join(home, ".env"), []byte("QUOTED=home-win\nHOME_ONLY=home-key\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	dataDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dataDir, ".env"), []byte("QUOTED=data-loses\nDATA_ONLY=data-key\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.WriteFile(".env", []byte("QUOTED=cwd-loses\nCWD_ONLY=cwd-key\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := apiKeyForDirs("QUOTED", dataDir); got != "home-win" {
		t.Fatalf("apiKeyForDirs(QUOTED, dataDir) = %q, want home .env 优先", got)
	}
	if got := apiKeyForDirs("HOME_ONLY", dataDir); got != "home-key" {
		t.Fatalf("apiKeyForDirs(HOME_ONLY) = %q, want 读 ~/.scopeforge/.env", got)
	}
	if got := apiKeyForDirs("DATA_ONLY", dataDir); got != "data-key" {
		t.Fatalf("apiKeyForDirs(DATA_ONLY) = %q, want 数据目录 .env 兜底", got)
	}
	if got := apiKeyForDirs("CWD_ONLY", dataDir); got != "cwd-key" {
		t.Fatalf("apiKeyForDirs(CWD_ONLY) = %q, want 当前目录 .env 最后兜底", got)
	}
}

// TestFirstURLInText 验收(阶段 2.21):任务文本提取聚焦目标 URL(中英文混排截断)。
func TestFirstURLInText(t *testing.T) {
	cases := []struct {
		text string
		want string
	}{
		{"对 http://192.168.81.167:3000/rest/products/search 做sql注入检测", "http://192.168.81.167:3000/rest/products/search"},
		{"目标 https://target.example.com/login,关注支付", "https://target.example.com/login"},
		{"没有 URL 的任务", ""},
	}
	for _, c := range cases {
		if got := firstURLInText(c.text); got != c.want {
			t.Errorf("firstURLInText(%q) = %q, want %q", c.text, got, c.want)
		}
	}
}

// TestRunTaskBootstrapUnparseableRetries 验收(阶段 2.21):bootstrap 契约
// 解析失败(unparseable)→ 标记 failed(而非 done)→ shouldBootstrap 重试
// → 重试成功落地格子 → explore 照常执行(此前 unparseable 被当 done,
// 黑板空 → no_more_work 提前收尾)。
func TestRunTaskBootstrapUnparseableRetries(t *testing.T) {
	llm := newRunTaskLLM(t)
	llm.scoutBadOnce = true
	t.Setenv("SCOPEFORGE_LLM_KEY", "test")
	tmp := t.TempDir()
	t.Setenv("SCOPEFORGE_HOME", tmp) // skill 物化到测试临时目录
	cfgText := fmt.Sprintf(`providers:
  - name: mock
    kind: openai
    base_url: %s
    model: mock-model
    api_key_env: SCOPEFORGE_LLM_KEY
scheduler:
  tick_interval: 20ms
  max_ticks: 80
  max_concurrency: 2
  observer_every_n_turns: 0
  worker_turns:
    recon: 3
    explore: 3
    reason: 3
    synthesizer: 3
tools:
  permissions: yolo
  blacklist: []
  domain_allowlist: ["127.0.0.1"]
skills:
  dir: %s/skills
memory:
  dir: %s/memory
capability:
  knowledge:
    plugins_dir: %s/kb
`, llm.URL(), tmp, tmp, tmp)
	cfgPath := filepath.Join(tmp, "scopeforge.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfgText), 0o644); err != nil {
		t.Fatal(err)
	}
	app, err := New(Options{ConfigPath: cfgPath, DBPath: filepath.Join(tmp, "test.db"), WorkDir: tmp})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	app.Sandbox = nil

	runID, err := app.RunTask(context.Background(), "对 http://192.168.81.167:3000/rest/products/search 进行 SQL 注入检测")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := app.DB.QueryRow(`SELECT COUNT(*) FROM intents WHERE challenge_id=?`, runID).Scan(&n); err == nil && n > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	// scout:1 failed(unparseable)+ 1 done(重试成功)
	var failed, done int
	if err := app.DB.QueryRow(`SELECT COUNT(*) FROM workers WHERE challenge_id=? AND worker_type='operator' AND phase='scout' AND status='failed'`, runID).Scan(&failed); err != nil {
		t.Fatal(err)
	}
	if err := app.DB.QueryRow(`SELECT COUNT(*) FROM workers WHERE challenge_id=? AND worker_type='operator' AND phase='scout' AND status='done'`, runID).Scan(&done); err != nil {
		t.Fatal(err)
	}
	if failed != 1 || done != 1 {
		t.Fatalf("bootstrap failed=%d done=%d, want 1/1(unparseable 应标记 failed 并重试)", failed, done)
	}
	// 重试成功后格子落地(不提前 no_more_work)
	var itN int
	if err := app.DB.QueryRow(`SELECT COUNT(*) FROM intents WHERE challenge_id=? AND target LIKE '%products/search%'`, runID).Scan(&itN); err != nil {
		t.Fatal(err)
	}
	if itN < 1 {
		t.Fatal("重试后无聚焦 intent(黑板空提前收尾)")
	}
}

// TestRunTaskSeedFallback 验收(阶段 2.21 兜底):bootstrap 全部契约失败
// (重试耗尽 ≤2 次)后,聚焦目标确定性建种子 → explore 照常认领执行,
// 漏洞进账本——任务不再因 LLM 输出格式问题空转/无产出。
func TestRunTaskSeedFallback(t *testing.T) {
	llm := newRunTaskLLM(t)
	llm.scoutBadAll = true // 3 次 bootstrap(1+2 重试)全失败
	t.Setenv("SCOPEFORGE_LLM_KEY", "test")
	tmp := t.TempDir()
	t.Setenv("SCOPEFORGE_HOME", tmp) // skill 物化到测试临时目录
	cfgText := fmt.Sprintf(`providers:
  - name: mock
    kind: openai
    base_url: %s
    model: mock-model
    api_key_env: SCOPEFORGE_LLM_KEY
scheduler:
  tick_interval: 20ms
  max_ticks: 120
  max_concurrency: 2
  observer_every_n_turns: 0
  worker_turns:
    recon: 3
    explore: 3
    reason: 3
    synthesizer: 3
tools:
  permissions: yolo
  blacklist: []
  domain_allowlist: ["127.0.0.1"]
skills:
  dir: %s/skills
memory:
  dir: %s/memory
capability:
  knowledge:
    plugins_dir: %s/kb
`, llm.URL(), tmp, tmp, tmp)
	cfgPath := filepath.Join(tmp, "scopeforge.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfgText), 0o644); err != nil {
		t.Fatal(err)
	}
	app, err := New(Options{ConfigPath: cfgPath, DBPath: filepath.Join(tmp, "test.db"), WorkDir: tmp})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	app.Sandbox = nil

	runID, err := app.RunTask(context.Background(), "对 http://192.168.81.167:3000/rest/products/search 进行 SQL 注入检测")
	if err != nil {
		t.Fatal(err)
	}
	// 等 explore 认领种子方向(30s 上限)
	deadline := time.Now().Add(30 * time.Second)
	var executorN int
	for time.Now().Before(deadline) {
		if err := app.DB.QueryRow(`SELECT COUNT(*) FROM workers WHERE challenge_id=? AND worker_type='operator' AND phase='executor'`, runID).Scan(&executorN); err == nil && executorN > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if executorN < 1 {
		t.Fatal("focus_seed 兜底未生效:executor 未派发(种子方向应被认领)")
	}
	// 种子 intent 已落地(确定性建格)
	var itN int
	if err := app.DB.QueryRow(`SELECT COUNT(*) FROM intents WHERE challenge_id=? AND target LIKE '%products/search%'`, runID).Scan(&itN); err != nil {
		t.Fatal(err)
	}
	if itN < 1 {
		t.Fatal("focus_seed 未建种子 intent")
	}
	// focus_seed 事件存在
	var hint string
	if err := app.DB.QueryRow(`SELECT CAST(payload AS TEXT) FROM events WHERE challenge_id=? AND CAST(payload AS TEXT) LIKE '%focus_seed%' ORDER BY seq DESC LIMIT 1`, runID).Scan(&hint); err != nil {
		t.Fatalf("focus_seed 事件缺失: %v", err)
	}
}
