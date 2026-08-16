package scheduler

import (
	"bytes"
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

	"scopeforge/internal/blackboard"
	"scopeforge/internal/breach"
	"scopeforge/internal/config"
	"scopeforge/internal/constraint"
	"scopeforge/internal/conversation"
	"scopeforge/internal/coverage"
	"scopeforge/internal/dispatcher"
	"scopeforge/internal/event"
	"scopeforge/internal/executor"
	"scopeforge/internal/reasonix/provider"
	"scopeforge/internal/reasonix/skill"
	"scopeforge/internal/reasonix/tool"
	"scopeforge/internal/store"
	"scopeforge/internal/testutil"

	_ "scopeforge/internal/reasonix/provider/openai"
)

// ------------------------------------------------------------------ 智能 mock LLM

// smartLLM 按 worker 类型路由行为(依据系统提示角色标记 + 最后消息 role):
//   - bootstrap: 首轮 bash 侦察,次轮契约(findings+intents)
//   - explore:   首轮 submit_flag(正确 flag),次轮契约(stop_reason=conclude)
//   - reason:    直接契约(hyp + intents)
//   - conclude:  契约(汇总 findings)
type smartLLM struct {
	*httptest.Server
	flag        string
	neverFinish bool          // 每轮都调工具(永不结束,熔断测试用)
	delay       time.Duration // 每次响应延迟(竞态测试用)
	// phase2 S6:conclude 契约定制(空 = 默认无穷尽声明)
	concludeContract string
	// phase2:explore 空手而归(无 findings,stop_reason=exhausted → 方向耗尽场景)
	exploreEmpty bool
	// phase2:explore 空手且自认完成(无 findings/new_intents,stop_reason=intent_done →
	// 未穷尽语义场景,03 §3.3)
	exploreEmptyDone bool
	// phase2:bootstrap 直接输出 attack_surface 契约(非空时覆盖默认 bootstrap 行为)
	bootstrapSurface string
	// phase2:explore 确认队列(CWE@endpoint,依次消费,末项重复)——驱动矩阵 confirmed 收敛
	confirmQueue []string
	// 回归:explore 以 submit_vulnerability 工具参数格式输出漏洞 finding
	// (无 prefix)→ 验证契约归一化补 vuln: 后 intent done/任务收尾
	exploreVulnToolFormat bool

	mu       sync.Mutex
	requests []string // 收到的请求消息尾部(调试/断言用)
}

func (m *smartLLM) lastRequestTails() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.requests))
	for _, b := range m.requests {
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal([]byte(b), &req)
		var parts []string
		for _, msg := range req.Messages {
			parts = append(parts, msg.Role+":"+msg.Content)
		}
		out = append(out, strings.Join(parts, "\n"))
	}
	return out
}

func newSmartLLM(t *testing.T, flag string) *smartLLM {
	t.Helper()
	m := &smartLLM{flag: flag}
	mux := http.NewServeMux()
	handler := func(w http.ResponseWriter, r *http.Request) {
		if m.delay > 0 {
			time.Sleep(m.delay)
		}
		bodyBytes, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(bodyBytes, &req)
		m.mu.Lock()
		m.requests = append(m.requests, string(bodyBytes))
		m.mu.Unlock()
		lastRole := ""
		sys := ""
		for i, msg := range req.Messages {
			if i == 0 {
				sys = msg.Content
			}
			if msg.Role == "user" || msg.Role == "tool" || msg.Role == "assistant" {
				lastRole = msg.Role
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl := w.(http.Flusher)
		write := func(v any) {
			data, _ := json.Marshal(v)
			fmt.Fprintf(w, "data: %s\n\n", data)
			fl.Flush()
		}
		usage := map[string]any{"prompt_tokens": 100, "completion_tokens": 20, "total_tokens": 120}
		if m.neverFinish {
			write(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{
				"tool_calls": []any{map[string]any{"index": 0, "id": "c1", "type": "function", "function": map[string]any{"name": "bash", "arguments": `{"command":"echo loop"}`}}},
			}}}})
			write(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": "tool_calls"}}, "usage": usage})
			fmt.Fprint(w, "data: [DONE]\n\n")
			fl.Flush()
			return
		}
		contract := ""
		switch {
		case strings.Contains(sys, "Scout。") || strings.Contains(sys, "Scout（"):
			// Scout 模板(聚焦模式用全角括号 "Scout（聚焦模式）")
			if m.bootstrapSurface != "" {
				contract = m.bootstrapSurface
			} else if lastRole == "tool" {
				contract = `{"accepted":true,"findings":[{"prefix":"obs:","text":"端口 80 开放","weight":0.8,"evidence_ref":"exec-b1"}],"new_intents":[{"text":"弱口令测试","weight":0.9}],"dead_ends":[],"stop_reason":"intent_done"}`
			} else {
				writeToolCall(w, fl, "bash", `{"command":"curl -s http://127.0.0.1/ | head -5"}`, usage)
				return
			}
		case strings.Contains(sys, "Executor。"):
			if m.exploreVulnToolFormat {
				// 实测回归:模型用 submit_vulnerability 工具参数格式输出漏洞契约
				// (title/description/evidence,无 prefix/stop_reason 语义)
				contract = `{"findings":[{"title":"SQL Injection in q","cwe":"CWE-89","asset":"shop.example.com","endpoint":"/rest/products/search?q=","severity":"high","description":"q 拼接进 SQL LIKE"}],"new_intents":[],"dead_ends":[],"stop_reason":"intent_done"}`
			} else if len(m.confirmQueue) > 0 {
				// 确认型 finding:按队列依次确认(驱动矩阵 confirmed)
				item := m.confirmQueue[0]
				if len(m.confirmQueue) > 1 {
					m.confirmQueue = m.confirmQueue[1:]
				}
				parts := strings.SplitN(item, "@", 2)
				cweID, endpoint := parts[0], parts[1]
				contract = `{"accepted":true,"findings":[{"prefix":"vuln:","text":"确认漏洞","weight":0.9,"evidence_ref":"exec-e1","cwe":"` + cweID + `","asset":"shop.example.com","endpoint":"` + endpoint + `","severity":"high"}],"new_intents":[],"dead_ends":[],"stop_reason":"intent_done"}`
			} else if m.exploreEmptyDone {
				contract = `{"accepted":true,"findings":[],"new_intents":[],"dead_ends":[],"stop_reason":"intent_done"}`
			} else if m.exploreEmpty {
				contract = `{"accepted":true,"findings":[],"new_intents":[],"dead_ends":[],"stop_reason":"exhausted"}`
			} else if lastRole == "tool" {
				// 临时实验:旧 flag 语义
				contract = `{"accepted":true,"findings":[{"prefix":"flag:","text":"` + m.flag + `","weight":1.0,"evidence_ref":"exec-e1"}],"new_intents":[],"dead_ends":[],"stop_reason":"conclude"}`
			} else {
				writeToolCall(w, fl, "bash", `{"command":"curl -s http://127.0.0.1/probe"}`, usage)
				return
			}
		case strings.Contains(sys, "Synthesizer。"):
			contract = `{"accepted":true,"findings":[{"prefix":"obs:","text":"最终确认:目标已完成","weight":0.9}],"new_intents":[],"dead_ends":[],"stop_reason":"conclude"}`
			if m.concludeContract != "" {
				contract = m.concludeContract
			}
		default: // reason
			contract = `{"accepted":true,"findings":[{"prefix":"hyp:","text":"疑似存在弱口令,待验证","weight":0.7}],"new_intents":[{"text":"验证弱口令","weight":0.8}],"dead_ends":[],"stop_reason":"intent_done"}`
		}
		write(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": contract}}}})
		write(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": "stop"}}, "usage": usage})
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	}
	mux.HandleFunc("/v1/chat/completions", handler)
	mux.HandleFunc("/chat/completions", handler)
	m.Server = httptest.NewServer(mux)
	t.Cleanup(m.Close)
	return m
}

func writeToolCall(w http.ResponseWriter, fl http.Flusher, name, args string, usage map[string]any) {
	data, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{
		"tool_calls": []any{map[string]any{"index": 0, "id": "c1", "type": "function", "function": map[string]any{"name": name, "arguments": args}}},
	}}}})
	fmt.Fprintf(w, "data: %s\n\n", data)
	fl.Flush()
	data2, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": "tool_calls"}}, "usage": usage})
	fmt.Fprintf(w, "data: %s\n\n", data2)
	fl.Flush()
	fmt.Fprint(w, "data: [DONE]\n\n")
	fl.Flush()
}

// ------------------------------------------------------------------ 装配

type fakeBashTool struct{}

func (f *fakeBashTool) Name() string        { return "bash" }
func (f *fakeBashTool) Description() string { return "执行命令" }
func (f *fakeBashTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}}}`)
}
func (f *fakeBashTool) ReadOnly() bool { return true }
func (f *fakeBashTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	return "recon output: open ports 80/443", nil
}

// newTestScheduler 装配完整测试调度器(阶段 2.11:无平台,coverage 语义)。
func newTestScheduler(t *testing.T, llmURL string, cfg Config) (*Scheduler, *blackboard.Blackboard, *store.DB) {
	t.Helper()
	db := testutil.NewTestDB(t)
	board := blackboard.New(db)

	p, err := provider.New("openai", provider.Config{
		Name: "mock", BaseURL: llmURL, Model: "mock-model", APIKey: "test",
	})
	if err != nil {
		t.Fatalf("provider: %v", err)
	}

	reg := tool.NewRegistry()
	reg.Add(&fakeBashTool{})

	ledger := constraint.NewCostLedger(db)
	meter := constraint.NewMeter(db, constraint.Budget{})
	hub := constraint.NewTerminationHubWithPolicy(board, meter, nil,
		constraint.Policy{GoalShape: constraint.GoalShapeCoverage})
	disp := dispatcher.New(board, event.Discard, nil)
	covMat := coverage.New(db)
	disp.SetCoverage(covMat)
	hub.SetConvergence(covMat)
	bspace := breach.New(db)
	convStore := conversation.NewStore(db)

	gate, err := executor.NewGate(executor.ModeYolo, nil, []string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}

	s := New(Options{
		Cfg:        cfg,
		Board:      board,
		Dispatcher: disp,
		Hub:        hub,
		Providers:  map[string]provider.Provider{"mock": p},
		Registry:   reg,
		Gate:       gate,
		Sink:       event.Discard,
		Ledger:     ledger,
		ConvStore:  convStore,
		CompactCfg: conversation.DefaultCompactConfig(),
		WorkDir:    t.TempDir(),
		Coverage:   covMat,
		Breach:     bspace,
	})
	disp.SetHooks(s)
	return s, board, db
}

func testCfg() Config {
	c := DefaultConfig()
	c.TickInterval = 50 * time.Millisecond
	c.StaleTimeout = time.Hour
	c.MaxConcurrency = 1
	c.MaxTicks = 200
	return c
}

// ------------------------------------------------------------------ 测试

// TestBudgetTripNeverFinish 永不完成题:预算熔断进入 conclude。
func TestBudgetTripNeverFinish(t *testing.T) {
	llm := newSmartLLM(t, "flag{never}")
	llm.neverFinish = true
	// turns 熔断:MaxTurnsPerChallenge=3(usage 每轮 1 行)
	cfg := testCfg()
	db := testutil.NewTestDB(t)
	board := blackboard.New(db)
	p, _ := provider.New("openai", provider.Config{Name: "mock", BaseURL: llm.URL, Model: "m", APIKey: "k"})
	reg := tool.NewRegistry()
	reg.Add(&fakeBashTool{})
	ledger := constraint.NewCostLedger(db)
	hub := constraint.NewTerminationHub(board, constraint.NewMeter(db, constraint.Budget{MaxTurnsPerChallenge: 3}), nil)
	disp := dispatcher.New(board, event.Discard, nil)
	gate, _ := executor.NewGate(executor.ModeYolo, nil, nil)
	s := New(Options{
		Cfg:        cfg,
		Board:      board,
		Dispatcher: disp,
		Hub:        hub,
		Providers:  map[string]provider.Provider{"mock": p},
		Registry:   reg,
		Gate:       gate,
		Sink:       event.Discard,
		Ledger:     ledger,
		ConvStore:  conversation.NewStore(db),
		CompactCfg: conversation.DefaultCompactConfig(),
		WorkDir:    t.TempDir(),
	})
	disp.SetHooks(s)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := s.Run(ctx, "easy-1")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Terminated {
		t.Fatalf("never-finish challenge not cut off: %+v", res)
	}
	if res.Reason != constraint.ReasonBudget {
		t.Errorf("reason = %s, want budget_exceeded", res.Reason)
	}
}

// TestHandoffTruncation handoff ≤900 字符截断。
func TestHandoffTruncation(t *testing.T) {
	llm := newSmartLLM(t, "flag{x}")
	s, board, _ := newTestScheduler(t, llm.URL, testCfg())
	// 塞入大量事实,迫使快照超长
	for i := 0; i < 50; i++ {
		_, _ = board.AddFact("easy-1", blackboard.PrefixObs, fmt.Sprintf("长事实内容 %d %s", i, strings.Repeat("x", 60)), 0.5, blackboard.StateConfirmed, "w", "")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := s.Run(ctx, "easy-1")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Terminated {
		t.Fatalf("not terminated")
	}
	// 校验 workers 表中 handoff 均 ≤900 字符
	ws, _ := board.Workers("easy-1")
	if len(ws) == 0 {
		t.Fatal("no workers recorded")
	}
	for _, w := range ws {
		if len([]rune(w.Handoff)) > 900 {
			t.Errorf("handoff %s = %d chars > 900", w.ID, len([]rune(w.Handoff)))
		}
	}
}

// TestContractInSystemPromptNotHandoff 输出契约经系统提示下发,不进 handoff
// (docs/phase2/11 §2.5:契约在 handoff 尾部会被 900 rune 截断连头切掉,
// 直接制造"契约解析失败";契约是静态指令,系统提示才是正确层)。
func TestContractInSystemPromptNotHandoff(t *testing.T) {
	// promptDir 置空目录 → 内置常量兜底,不读本机物化文件,测试密闭
	s := &Scheduler{promptDir: t.TempDir()}
	sys := s.buildSystem(WorkerOperator, "t.example.com", map[string]string{}, false, PhaseExplore)
	for _, want := range []string{"## CONTRACT(输出契约)", "不允许输出 JSON 以外的多余文本"} {
		if !strings.Contains(sys, want) {
			t.Errorf("system prompt missing contract part %q", want)
		}
	}

	llm := newSmartLLM(t, "flag{x}")
	s2, board, _ := newTestScheduler(t, llm.URL, testCfg())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := s2.Run(ctx, "easy-1"); err != nil {
		t.Fatalf("run: %v", err)
	}
	ws, _ := board.Workers("easy-1")
	if len(ws) == 0 {
		t.Fatal("no workers recorded")
	}
	for _, w := range ws {
		if strings.Contains(w.Handoff, "CONTRACT") {
			t.Errorf("handoff %s still carries contract", w.ID)
		}
	}
}

// TestHandoffIntentOwnSlot 当前 intent 走独立 "## 当前 Intent" 段,不再占用
// "已完成事项(不要重复)" 槽位;超长快照触发截断时 intent/completed 结构保留
// (docs/phase2/11 §2.5:intent 错放 completed 槽位既丢失真实已完成事项——
// 重复派发诱因,又让 worker 在"不要重复"标题下读到当前任务)。
func TestHandoffIntentOwnSlot(t *testing.T) {
	llm := newSmartLLM(t, "flag{x}")
	s, board, _ := newTestScheduler(t, llm.URL, testCfg())
	// 塞入大量事实,迫使快照超长触发截断
	for i := 0; i < 50; i++ {
		_, _ = board.AddFact("easy-1", blackboard.PrefixObs, fmt.Sprintf("长事实内容 %d %s", i, strings.Repeat("x", 60)), 0.5, blackboard.StateConfirmed, "w", "")
	}
	_, _ = board.AddIntent("easy-1", "独特方向标记-支付逻辑越权", 0.9, "w")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := s.Run(ctx, "easy-1"); err != nil {
		t.Fatalf("run: %v", err)
	}
	ws, _ := board.Workers("easy-1")
	exploreSeen := false
	for _, w := range ws {
		if w.WorkerType != WorkerOperator || w.Phase != string(PhaseExplore) {
			continue
		}
		exploreSeen = true
		if !strings.Contains(w.Handoff, "## 当前 Intent") || !strings.Contains(w.Handoff, "独特方向标记") {
			t.Errorf("explore %s handoff missing 当前 Intent section:\n%s", w.ID, w.Handoff)
		}
		if strings.Contains(w.Handoff, "当前 intent: ") {
			t.Errorf("explore %s handoff still uses completed slot for intent", w.ID)
		}
		if !strings.Contains(w.Handoff, "## 已完成事项") {
			t.Errorf("explore %s handoff lost completed section under truncation", w.ID)
		}
		if len([]rune(w.Handoff)) > maxHandoffRunes {
			t.Errorf("handoff %s = %d runes > %d", w.ID, len([]rune(w.Handoff)), maxHandoffRunes)
		}
	}
	if !exploreSeen {
		t.Fatal("no explore worker dispatched")
	}
}

// TestWorkerSkillLink 层 3 skill 卡链路回归(docs/phase2/11 §2.4):
// worker 注册表带 run_skill、卡体可真实加载、技能索引进系统前缀且只构建一次。
// 此前三处全断:run_skill 被 recursiveTools 排除/生产未注册、SkillIndex 无赋值方,
// 而提示词指示模型"用 run_skill 加载"——照做只会 tool not found。
func TestWorkerSkillLink(t *testing.T) {
	dir := t.TempDir()
	cardDir := filepath.Join(dir, "test-card")
	if err := os.MkdirAll(cardDir, 0o755); err != nil {
		t.Fatal(err)
	}
	card := "---\nname: test-card\ndescription: 测试技能卡\nrunas: inline\ninvocation: auto\n---\n\n# 测试卡体\n\n具体打法步骤。\n"
	if err := os.WriteFile(filepath.Join(cardDir, "SKILL.md"), []byte(card), 0o644); err != nil {
		t.Fatal(err)
	}
	store := skill.New(skill.Options{ProjectRoot: t.TempDir(), CustomPaths: []string{dir}})
	s := &Scheduler{skills: store, sink: event.Discard}

	// 1. worker 注册表带 run_skill
	reg := s.workerRegistryFor(&blackboard.Worker{ChallengeID: "c1", ID: "w1"})
	tl, ok := reg.Get("run_skill")
	if !ok {
		t.Fatal("worker registry missing run_skill")
	}
	// 2. 卡体可经工具真实加载(inline 卡直接返回卡内容)
	out, err := tl.Execute(context.Background(), json.RawMessage(`{"name":"test-card"}`))
	if err != nil {
		t.Fatalf("run_skill: %v", err)
	}
	if !strings.Contains(out, "具体打法步骤") {
		t.Errorf("run_skill did not return card body:\n%s", out)
	}
	// 3. 技能索引含卡名/描述,且缓存(两次调用同一字符串)
	idx := s.skillIndexBlock()
	if !strings.Contains(idx, "test-card") || !strings.Contains(idx, "测试技能卡") {
		t.Errorf("skill index missing card:\n%s", idx)
	}
	if again := s.skillIndexBlock(); again != idx {
		t.Error("skill index not cached")
	}
	// 4. 无 SkillStore 时:无 run_skill、索引为空(行为回退兼容)
	s2 := &Scheduler{sink: event.Discard}
	if _, ok := s2.workerRegistryFor(&blackboard.Worker{ChallengeID: "c1", ID: "w2"}).Get("run_skill"); ok {
		t.Error("run_skill present without SkillStore")
	}
	if s2.skillIndexBlock() != "" {
		t.Error("skill index non-empty without SkillStore")
	}
}

// TestWorkerRegistryNoDynamicToolWrappers 验收:Kali 攻击工具不封装为
// sqlmap_run / nmap_scan / ffuf_run 等动态工具,统一由容器 bash + skill 卡
// 说明调用;worker 注册表也不应再出现 tool_search。
func TestWorkerRegistryNoDynamicToolWrappers(t *testing.T) {
	s := &Scheduler{sink: event.Discard}
	reg := s.workerRegistryFor(&blackboard.Worker{ChallengeID: "c1", ID: "w1"})
	for _, name := range []string{"sqlmap_run", "nmap_scan", "ffuf_run", "hashcat_run", "john_run", "msfconsole_run", "tool_search"} {
		if _, ok := reg.Get(name); ok {
			t.Errorf("worker registry must not expose %s(Kali 工具经 bash + skill 卡调用)", name)
		}
	}
}

// TestConcurrencyLimit 并发上限生效(串行时同刻至多 1 个 running)。
func TestConcurrencyLimit(t *testing.T) {
	llm := newSmartLLM(t, "flag{x}")
	cfg := testCfg()
	cfg.MaxConcurrency = 1
	s, board, _ := newTestScheduler(t, llm.URL, cfg)
	// 两个高权重 intent → 但并发槽 1,只能顺序执行
	_, _ = board.AddFact("easy-1", blackboard.PrefixObs, "侦察完成", 0.5, blackboard.StateConfirmed, "w", "")
	_, _ = board.AddIntent("easy-1", "方向A", 0.9, "w")
	_, _ = board.AddIntent("easy-1", "方向B", 0.8, "w")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := s.Run(ctx, "easy-1")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	// explore 串行执行:worker 记录顺序不重叠(通过 done 状态验证)
	ws, _ := board.Workers("easy-1")
	explores := 0
	for _, w := range ws {
		if w.WorkerType == WorkerOperator && w.Phase == string(PhaseExplore) {
			explores++
			if w.Status != "done" {
				t.Errorf("explore %s status = %s", w.ID, w.Status)
			}
		}
	}
	if explores < 1 {
		t.Errorf("explores = %d", explores)
	}
}

// TestResumeOrphanRecovery 崩溃残留 worker 被标记 orphaned,黑板不丢。
func TestResumeOrphanRecovery(t *testing.T) {
	llm := newSmartLLM(t, "flag{easy-login-bypass}")
	s, board, _ := newTestScheduler(t, llm.URL, testCfg())
	// 模拟上次崩溃:残留 running worker + 黑板事实 + 已夺旗(flag fact)
	_ = board.CreateWorker(&blackboard.Worker{
		ID: "w-crash", ChallengeID: "easy-1", WorkerType: WorkerOperator, Phase: string(PhaseExplore),
		LastProgressAt: time.Now().Unix(),
	})
	_, _ = board.AddFact("easy-1", blackboard.PrefixObs, "崩溃前已发现登录口", 0.8, blackboard.StateConfirmed, "w-crash", "")
	_, _ = board.AddFact("easy-1", blackboard.PrefixFlag, "flag{easy-login-bypass}", 1.0, blackboard.StateConfirmed, "w-crash", "")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := s.Run(ctx, "easy-1")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Terminated {
		t.Fatalf("not terminated")
	}
	// 孤儿 worker 已标记 orphaned
	ws, _ := board.Workers("easy-1")
	orphaned := false
	for _, w := range ws {
		if w.ID == "w-crash" && w.Status == "orphaned" {
			orphaned = true
		}
	}
	if !orphaned {
		t.Errorf("crash worker not orphaned: %+v", ws)
	}
	// 黑板事实保留(含 flag)
	facts, _ := board.Facts("easy-1", 0)
	hasFlag := false
	for _, f := range facts {
		if f.Prefix == blackboard.PrefixFlag && f.State == blackboard.StateConfirmed {
			hasFlag = true
		}
	}
	if !hasFlag {
		t.Error("blackboard lost facts after resume")
	}
	// bootstrap 不再重复(有残留事实时板非 initial)
	bs := 0
	for _, w := range ws {
		if w.WorkerType == WorkerOperator && w.Phase == string(PhaseRecon) {
			bs++
		}
	}
	if bs > 1 {
		t.Errorf("bootstrap ran %d times", bs)
	}
}

// TestSteerInjection Observer steer 消息在下一轮生效。
func TestSteerInjection(t *testing.T) {
	llm := newSmartLLM(t, "flag{easy-login-bypass}")
	llm.delay = 300 * time.Millisecond
	s, board, _ := newTestScheduler(t, llm.URL, testCfg())
	// 直接派一个 explore worker 并注入 steer
	_, _ = board.AddFact("easy-1", blackboard.PrefixObs, "登录口已发现", 0.8, blackboard.StateConfirmed, "w", "")
	it, _ := board.AddIntent("easy-1", "弱口令测试", 0.9, "w")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	wid, err := s.Launch(ctx, WorkerOperator, dispatcher.Handoff{
		ChallengeID: "easy-1", IntentID: it.ID,
		Phase:        string(PhaseExplore),
		SnapshotText: "challenge: easy-1\n",
	})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	// 等 worker 进入 running(第一轮请求已发出)再注入 steer
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.mu.Lock()
		_, running := s.running[wid]
		s.mu.Unlock()
		if running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("worker did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := s.Steer(ctx, wid, "优先验证弱口令", "high"); err != nil {
		t.Fatalf("steer: %v", err)
	}
	// 等 worker 完成
	finishDeadline := time.Now().Add(10 * time.Second)
	for {
		s.mu.Lock()
		_, running := s.running[wid]
		s.mu.Unlock()
		if !running {
			break
		}
		if time.Now().After(finishDeadline) {
			t.Fatal("worker did not finish")
		}
		time.Sleep(50 * time.Millisecond)
	}
	// steer 注入成功:LLM 收到的请求中至少一条含 STEER 消息
	tails := llm.lastRequestTails()
	found := false
	for _, tail := range tails {
		if strings.Contains(tail, "[STEER") {
			found = true
		}
	}
	if !found {
		t.Error("steer message not injected into worker prompt")
	}
}

// TestReasonWorkerDispatched hyp candidate 存在 → reason worker 被调度
// (docs/03 §2.2 决策规则 + §1.2 hyp 状态机)。
func TestReasonWorkerDispatched(t *testing.T) {
	llm := newSmartLLM(t, "flag{easy-login-bypass}")
	s, board, _ := newTestScheduler(t, llm.URL, testCfg())
	// 预置:有事实 + 未验证假设(无 open intent)→ 应派 reason
	_, _ = board.AddFact("easy-1", blackboard.PrefixObs, "登录口已发现", 0.8, blackboard.StateConfirmed, "w", "")
	_, _ = board.AddFact("easy-1", blackboard.PrefixHyp, "疑似弱口令,待验证", 0.7, blackboard.StateCandidate, "w", "")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := s.Run(ctx, "easy-1")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Terminated {
		t.Fatalf("not terminated")
	}
	ws, _ := board.Workers("easy-1")
	hasReason := false
	for _, w := range ws {
		if w.WorkerType == WorkerOperator && w.Phase == string(PhaseReason) {
			hasReason = true
		}
	}
	if !hasReason {
		t.Errorf("workers = %+v, want reason dispatched (hyp candidate)", ws)
	}
}

// ------------------------------------------------------------------ phase2 S6:穷尽声明(docs/phase2/02 §2.4)

// TestExhaustionNoEvidenceResumes 验收:方向耗尽收尾时 conclude 无证据 → 不采信 → 调度继续
// (防懒收尾);连续被拒达上限(3 次)后强制收尾,不无限空转烧 token。
func TestExhaustionNoEvidenceResumes(t *testing.T) {
	llm := newSmartLLM(t, "flag{x}")
	// bootstrap 产出方向,explore 无成果收尾,conclude 无穷尽声明证据
	llm.exploreEmpty = true
	s, board, _ := newTestScheduler(t, llm.URL, testCfg())

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := s.Run(ctx, "easy-1")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Terminated {
		t.Fatalf("not terminated: %+v", res)
	}
	if res.Reason != "no_more_work" {
		t.Errorf("reason = %s, want no_more_work (forced conclude after %d rejections)", res.Reason, maxExhaustionRejects)
	}
	// conclude worker 数量 = maxExhaustionRejects(前 N-1 次被拒,第 N 次拒绝时强制收尾)
	ws, _ := board.Workers("easy-1")
	concludeCount := 0
	for _, w := range ws {
		if w.WorkerType == WorkerSynthesizer && w.Status == "done" {
			concludeCount++
		}
	}
	if concludeCount != maxExhaustionRejects {
		t.Errorf("conclude workers = %d, want %d (rejected %d times, forced conclude on last)", concludeCount, maxExhaustionRejects, maxExhaustionRejects)
	}
}

// TestExhaustionWithEvidenceStops 验收:穷尽声明带证据 → 采信 → 收尾。
func TestExhaustionWithEvidenceStops(t *testing.T) {
	llm := newSmartLLM(t, "flag{x}")
	llm.exploreEmpty = true
	llm.concludeContract = `{"accepted":true,"exhausted":true,
	  "coverage_evidence":[{"direction":"CWE-89@/login","tried":["error_based","union"],"excluded":"参数全参数化"}],
	  "findings":[{"prefix":"obs:","text":"最终确认:目标已完成","weight":0.9}],
	  "new_intents":[],"dead_ends":[],"stop_reason":"conclude"}`
	s, _, _ := newTestScheduler(t, llm.URL, testCfg())

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := s.Run(ctx, "easy-1")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Terminated || !res.Concluded {
		t.Fatalf("not concluded: %+v", res)
	}
	if res.Reason != "no_more_work" {
		t.Errorf("reason = %s, want no_more_work (evidence-backed exhaustion)", res.Reason)
	}
}

// ------------------------------------------------------------------ phase2 切片3:覆盖矩阵驱动(03 §2/§3)

// TestCoverageAttackSurfaceToConverge 验收闭环:攻击面清单 → 初始 intent 展开 →
// explore 逐个确认(队列) → 全终态收敛终止(coverage_converged),accepted 后接力格被补格派活。
func TestCoverageAttackSurfaceToConverge(t *testing.T) {
	llm := newSmartLLM(t, "flag{x}")
	// bootstrap 输出 2 个带参端点攻击面
	llm.bootstrapSurface = `{"accepted":true,"findings":[],"new_intents":[],"dead_ends":[],
	  "attack_surface":[{"asset":"shop.example.com","endpoint":"/login","params":["u","p"]},
	                    {"asset":"shop.example.com","endpoint":"/api/order","params":["id"]}],
	  "stop_reason":"intent_done"}`
	// explore 依次确认:/login 三个候选 CWE + /api/order 候选(队列末项重复防越界)
	llm.confirmQueue = []string{
		"CWE-89@/login", "CWE-79@/login", "CWE-639@/login",
		"CWE-89@/api/order", "CWE-79@/api/order", "CWE-639@/api/order",
	}
	s, board, db := newTestScheduler(t, llm.URL, testCfg())
	cov := coverage.New(db)

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	res, err := s.Run(ctx, "easy-1")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Terminated {
		t.Fatalf("not terminated: %+v", res)
	}
	if res.Reason != constraint.ReasonCoverageConverged {
		t.Errorf("reason = %s, want coverage_converged", res.Reason)
	}
	// 矩阵全终态(无 open 格)
	cells, _ := cov.Cells("easy-1")
	openCount := 0
	for _, c := range cells {
		if c.Status == "open" {
			openCount++
		}
	}
	if openCount != 0 {
		t.Errorf("open cells remain: %d (%+v)", openCount, cells)
	}
	_ = board
}

// TestClaimIntentCoverageFilters 验收:已 confirmed 方向不再被认领(03 §3.2)。
func TestClaimIntentCoverageFilters(t *testing.T) {
	db := testutil.NewTestDB(t)
	board := blackboard.New(db)
	// 两个 intent:目标 /login 与 /api/order
	if _, err := board.AddIntent("c1", "测 /login", 0.9, "w", blackboard.IntentIn{Target: "/login", Approach: "probe"}); err != nil {
		t.Fatal(err)
	}
	if _, err := board.AddIntent("c1", "测 /api/order", 0.8, "w", blackboard.IntentIn{Target: "/api/order", Approach: "probe"}); err != nil {
		t.Fatal(err)
	}
	cov := coverage.New(db)
	// /login 已 confirmed → 认领应跳过它,选 /api/order
	_ = cov.EnsureOpen("c1", "CWE-89", "shop.example.com", "/login", coverage.FormParam)
	_ = cov.MarkConfirmed("c1", "CWE-89", "shop.example.com", "/login")

	// 用最小 Scheduler(只测 claimIntentCoverage)
	s := &Scheduler{cov: cov}
	open, _ := board.Intents("c1", []string{blackboard.StateOpen}, 0)
	it := s.claimIntentCoverage("c1", open)
	if it == nil || it.Target != "/api/order" {
		t.Fatalf("claim = %+v, want /api/order (confirmed /login filtered)", it)
	}
	// 全部 confirmed → nil(走补格)
	_ = cov.EnsureOpen("c1", "CWE-89", "shop.example.com", "/api/order", coverage.FormParam)
	_ = cov.MarkConfirmed("c1", "CWE-89", "shop.example.com", "/api/order")
	open, _ = board.Intents("c1", []string{blackboard.StateOpen}, 0)
	if it := s.claimIntentCoverage("c1", open); it != nil {
		t.Fatalf("all confirmed must return nil, got %+v", it)
	}
}

// ------------------------------------------------------------------ phase2 切片4:breach 状态空间(03 §3.4.1)

// TestBreachSpaceExpansion 验收:breach 模式——确认状态 → 转移边展开 →
// 认领派活 → 目标达成(goal_reached)终止;空间闭合兜底(space_closed)。
func TestBreachSpaceExpansion(t *testing.T) {
	llm := newSmartLLM(t, "flag{x}")
	// bootstrap 确认 web 入口状态
	llm.bootstrapSurface = `{"accepted":true,"findings":[{"prefix":"vuln:","text":"web 入口可达","weight":0.9,"evidence_ref":"exec-b1","cwe":"CWE-89","asset":"web-host","endpoint":"/"}],"new_intents":[],"dead_ends":[],"stop_reason":"intent_done"}`
	// explore 沿转移边推进:确认注入→RCE(每条边都确认)
	llm.confirmQueue = []string{"CWE-89@web-host"}

	db := testutil.NewTestDB(t)
	board := blackboard.New(db)
	p, _ := provider.New("openai", provider.Config{Name: "mock", BaseURL: llm.URL, Model: "m", APIKey: "k"})
	reg := tool.NewRegistry()
	reg.Add(&fakeBashTool{})
	ledger := constraint.NewCostLedger(db)
	hub := constraint.NewTerminationHubWithPolicy(board, constraint.NewMeter(db, constraint.Budget{}), nil,
		constraint.Policy{GoalShape: constraint.GoalShapeBreach})
	bspace := breach.New(db)
	bspace.SetGoal("state@web-host")
	hub.SetGoalVerifier(bspace)
	disp := dispatcher.New(board, event.Discard, nil)
	cfg := testCfg()
	s := New(Options{
		Cfg: cfg, Board: board, Dispatcher: disp, Hub: hub,
		Providers: map[string]provider.Provider{"mock": p}, Registry: reg,
		Sink: event.Discard, Ledger: ledger,
		ConvStore: conversation.NewStore(db), CompactCfg: conversation.DefaultCompactConfig(),
		WorkDir: t.TempDir(), Breach: bspace,
	})
	disp.SetHooks(s)

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	res, err := s.Run(ctx, "easy-1")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Terminated {
		t.Fatalf("not terminated: %+v", res)
	}
	// goal_reached(web 入口即目标)或 space_closed 均为合法收尾
	if res.Reason != constraint.ReasonGoalReached && res.Reason != constraint.ReasonSpaceClosed {
		t.Errorf("reason = %s, want goal_reached|space_closed", res.Reason)
	}
	// 状态节点已确认
	nodes, _ := bspace.ConfirmedNodes("easy-1")
	if len(nodes) == 0 {
		t.Error("no confirmed state nodes")
	}
	// 转移边已展开(web → 注入RCE/上传RCE/SSRF)
	edges, _ := bspace.Edges("easy-1")
	if len(edges) == 0 {
		t.Error("no transfer edges generated")
	}
}

// ------------------------------------------------------------------ phase2 切片5:任务描述注入(04 §1)

func TestTaskContextBlock(t *testing.T) {
	// 空 profile → 空块(不污染提示词)
	if got := taskContextBlock(config.TaskProfileConfig{}); got != "" {
		t.Fatalf("empty profile = %q, want empty", got)
	}
	tp := config.TaskProfileConfig{
		Description: "企业 B SRC:范围 app.b.example.com,关注支付逻辑与越权",
		Constraints: config.TaskConstraints{Exclusions: []string{"admin.b.example.com"}},
		Skills:      []string{"enterprise-b-src"},
	}
	got := taskContextBlock(tp)
	for _, want := range []string{"任务描述", "企业 B SRC", "禁止范围", "admin.b.example.com", "可用知识包", "enterprise-b-src", "run_skill"} {
		if !strings.Contains(got, want) {
			t.Errorf("taskContextBlock missing %q:\n%s", want, got)
		}
	}
}

func TestBuildSystemTaskContextInjection(t *testing.T) {
	// 注入:task_context 出现
	vars := map[string]string{"{task_context}": "## 任务描述\n测试任务"}
	s := &Scheduler{}
	sys := s.buildSystem(WorkerOperator, "t.example.com", vars, false, PhaseRecon)
	if !strings.Contains(sys, "## 任务描述") || !strings.Contains(sys, "测试任务") {
		t.Errorf("task context not injected:\n%s", sys)
	}
	// 空:不留占位符残留
	sys2 := s.buildSystem(WorkerOperator, "t.example.com", map[string]string{}, false, PhaseRecon)
	if strings.Contains(sys2, "{task_context}") {
		t.Errorf("placeholder残留:\n%s", sys2)
	}
}

func TestRunInjectsConstraintsToGate(t *testing.T) {
	llm := newSmartLLM(t, "flag{x}")
	s, _, _ := newTestScheduler(t, llm.URL, testCfg())
	// 注入 task_profile:targets → Gate 白名单(替代原平台 StartChallenge 注入点)
	s.taskProfile = config.TaskProfileConfig{
		Constraints: config.TaskConstraints{Targets: []string{"app.b.example.com", "https://*.b.example.com"}},
	}
	gate, err := executor.NewGate(executor.ModeYolo, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.gate = gate
	// 直接调用注入段(与 Run 相同的代码路径)
	if len(s.taskProfile.Constraints.Targets) > 0 {
		if err := s.gate.AddTargets(s.taskProfile.Constraints.Targets); err != nil {
			t.Fatal(err)
		}
	}
	if d, reason := s.gate.Check("curl", false, map[string]any{"url": "https://app.b.example.com/login"}); d != executor.Allow {
		t.Errorf("whitelisted target denied: %v %s", d, reason)
	}
	if d, _ := s.gate.Check("curl", false, map[string]any{"url": "https://evil.example.com"}); d == executor.Allow {
		t.Error("out-of-scope target allowed")
	}
}

// ------------------------------------------------------------------ phase2 C1:未穷尽语义(03 §3.3)

// TestUnExhaustedJudgement 验收:explore 收尾 intent_done 但无 new_intent 且无
// vuln 产出 → 未穷尽(不 done);有 new_intent / 有 vuln 产出 / conclude 收尾 /
// 非 explore worker → 不算未穷尽。
func TestUnExhaustedJudgement(t *testing.T) {
	s := &Scheduler{}
	explore := &runningWorker{record: &blackboard.Worker{WorkerType: WorkerOperator}, phase: PhaseExplore}
	reason := &runningWorker{record: &blackboard.Worker{WorkerType: WorkerOperator}, phase: PhaseReason}

	cases := []struct {
		name     string
		rw       *runningWorker
		stop     string
		findings []dispatcher.Finding
		intents  []dispatcher.IntentIn
		want     bool
	}{
		{"explore 空手 intent_done → 未穷尽", explore, "intent_done", nil, nil, true},
		{"有 new_intent → 不算未穷尽", explore, "intent_done", nil,
			[]dispatcher.IntentIn{{Text: "继续"}}, false},
		{"有 vuln 产出 → 不算未穷尽", explore, "intent_done",
			[]dispatcher.Finding{{Prefix: blackboard.PrefixVuln, Text: "x"}}, nil, false},
		{"obs 产出不算 vuln → 未穷尽", explore, "intent_done",
			[]dispatcher.Finding{{Prefix: blackboard.PrefixObs, Text: "x"}}, nil, true},
		{"conclude 收尾 → 不算未穷尽", explore, "conclude", nil, nil, false},
		{"非 explore → 不算未穷尽", reason, "intent_done", nil, nil, false},
	}
	for _, c := range cases {
		contract := &dispatcher.WorkerContract{StopReason: c.stop, NewIntents: c.intents}
		if got := s.unExhausted(c.rw, contract, c.findings); got != c.want {
			t.Errorf("%s: unExhausted = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestExploreUnExhaustedKeepsIntentOpen 验收(03 §3.3):explore 无产出无 new_intent
// 时 intent 不标记 done——重试超限前转 open(可再认领),超限后 dead(防空转)。
func TestExploreUnExhaustedKeepsIntentOpen(t *testing.T) {
	llm := newSmartLLM(t, "flag{x}")
	llm.exploreEmptyDone = true // explore 每次都空手自认完成
	s, board, _ := newTestScheduler(t, llm.URL, testCfg())

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := s.Run(ctx, "easy-1")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Terminated {
		t.Fatalf("not terminated: %+v", res)
	}
	// bootstrap 产出 1 个初始 intent("弱口令测试");未穷尽重试超限后应为 dead,
	// 绝不允许直接 done(03 §3.3:无 new_intent 且方向未确认 → 不标记完成)。
	intents, err := board.Intents("easy-1", nil, 0)
	if err != nil {
		t.Fatalf("intents: %v", err)
	}
	if len(intents) == 0 {
		t.Fatal("no intents")
	}
	for _, it := range intents {
		if it.State == blackboard.StateDone {
			t.Errorf("intent %d 被标记 done,未穷尽语义要求不 done(03 §3.3): %+v", it.ID, it)
		}
	}
	// 重试计数达到上限(> maxUnExhaustedRetries)→ 至少一次 dead 落地
	dead := 0
	for _, it := range intents {
		if it.State == blackboard.StateDead {
			dead++
		}
	}
	if dead == 0 {
		t.Errorf("无 dead intent,未穷尽重试护栏未生效: %+v", intents)
	}
}

// TestNormalizeFindings 验收:模型以 submit_vulnerability 工具参数格式输出
// 漏洞 finding(无 prefix,title/description 字段)→ 归一化补 vuln: 前缀,
// 使 unExhausted/applyCoverage 正确判定(实测回归:缺失前缀导致同一 intent
// 被反复重派 4 次、任务不收敛)。
func TestNormalizeFindings(t *testing.T) {
	fs := []dispatcher.Finding{
		{Prefix: "", CWE: "CWE-89", Asset: "http://192.168.81.167:3000", Endpoint: "/rest/products/search?q=", Severity: "high"}, // 工具格式漏洞
		{Prefix: blackboard.PrefixObs, Text: "技术栈观测", CWE: "CWE-89"},                                                             // 已带前缀不动
		{Prefix: "", Text: "纯文本无结构化键"},                                                                                           // 无结构化键不动
		{Prefix: "", CWE: "CWE-79", Asset: "shop.example.com"},                                                                   // 仅 asset 也可
		{Prefix: "", CWE: "CWE-89", Endpoint: "/rest/products/search"},                                                           // 仅 endpoint 无 asset → 不提升(格子无法确认)
	}
	normalizeFindings(fs)
	if fs[0].Prefix != blackboard.PrefixVuln {
		t.Errorf("findings[0].prefix = %q, want vuln:", fs[0].Prefix)
	}
	if fs[0].Weight != 0.9 {
		t.Errorf("findings[0].weight = %v, want 0.9", fs[0].Weight)
	}
	if fs[1].Prefix != blackboard.PrefixObs {
		t.Errorf("findings[1].prefix 被误改: %q", fs[1].Prefix)
	}
	if fs[2].Prefix != "" {
		t.Errorf("findings[2].prefix = %q, want 空", fs[2].Prefix)
	}
	if fs[3].Prefix != blackboard.PrefixVuln {
		t.Errorf("findings[3].prefix = %q, want vuln:", fs[3].Prefix)
	}
	if fs[4].Prefix != "" {
		t.Errorf("findings[4](仅 endpoint)prefix = %q, want 空", fs[4].Prefix)
	}
}

// TestExploreVulnToolFormatContract 验收(实测回归):explore worker 以
// submit_vulnerability 工具参数格式输出漏洞契约(无 prefix)时,契约归一化
// 补 vuln: 前缀 → intent 标记 done(不再误判"未穷尽"反复重派)、聚焦模式
// "确认后即终止"触发、explore 只派发一次(修复前同一 intent 被派发 4 次、
// 漏洞重复提交 5 次)。
func TestExploreVulnToolFormatContract(t *testing.T) {
	llm := newSmartLLM(t, "flag{x}")
	llm.exploreVulnToolFormat = true
	// bootstrap 输出带 target 的初始 intent(聚焦过滤只保留带 target 的方向)
	llm.bootstrapSurface = `{"accepted":true,"findings":[{"prefix":"obs:","text":"端点可达","weight":0.8,"evidence_ref":"exec-b1"}],"new_intents":[{"text":"对 GET /rest/products/search?q= 执行 SQL 注入检测","weight":0.9,"target":"/rest/products/search","approach":"SQLi on q parameter"}],"dead_ends":[],"stop_reason":"intent_done"}`
	s, board, _ := newTestScheduler(t, llm.URL, testCfg())
	// 聚焦模式:任务指定目标端点(与实测任务形态一致)
	s.taskProfile = config.TaskProfileConfig{FocusTarget: "http://shop.example.com/rest/products/search?q="}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := s.Run(ctx, "easy-1")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Terminated {
		t.Fatalf("not terminated: %+v", res)
	}
	// intent 应为 done(漏洞产出被识别),绝不允许 dead/回到 open(修复前状态)
	intents, err := board.Intents("easy-1", nil, 0)
	if err != nil {
		t.Fatalf("intents: %v", err)
	}
	if len(intents) != 1 {
		ws, _ := board.Workers("easy-1")
		for _, w := range ws {
			t.Logf("worker: %s type=%s phase=%q status=%s", w.ID, w.WorkerType, w.Phase, w.Status)
		}
		for i, r := range llm.lastRequestTails() {
			t.Logf("req %d: %s", i, r)
		}
		t.Fatalf("intents = %d, want 1", len(intents))
	}
	if intents[0].State != blackboard.StateDone {
		t.Errorf("intent state = %s, want done(修复前:反复重派后 dead)", intents[0].State)
	}
	// explore 只派发一次
	ws, err := board.Workers("easy-1")
	if err != nil {
		t.Fatalf("workers: %v", err)
	}
	explore := 0
	for _, w := range ws {
		if w.WorkerType == WorkerOperator && w.Phase == string(PhaseExplore) {
			explore++
		}
	}
	if explore != 1 {
		t.Errorf("explore workers = %d, want 1(修复前:4)", explore)
	}
}

// TestPickBreachEdgeGoalScope 验收(03 §3.2):maximize 按距 goal 距离升序认领;
// specific 只认领可达 goal 的边;全不可达时回退(防死锁)。
func TestPickBreachEdgeGoalScope(t *testing.T) {
	db := testutil.NewTestDB(t)
	bspace := breach.New(db)
	bspace.SetGoal("state@dc")

	// 图:web →(注入RCE)→ shell(确认),shell →(横向)→ dc(确认)
	if _, err := bspace.ConfirmNode("t1", "state@web", "web", "web-host", ""); err != nil {
		t.Fatal(err)
	}
	_ = bspace.AddTransitions("t1", "state@web")
	webEdges, _ := bspace.Edges("t1")
	var rce *breach.Edge
	for i := range webEdges {
		if webEdges[i].Action == breach.ActionInjectRCE {
			rce = &webEdges[i]
		}
	}
	if rce == nil {
		t.Fatal("rce edge missing")
	}
	_ = bspace.ConfirmEdge("t1", rce.ID, "state@shell")
	_, _ = bspace.ConfirmNode("t1", "state@shell", "shell", "web-host", "root")
	_ = bspace.AddTransitions("t1", "state@shell")
	shellEdges, _ := bspace.Edges("t1")
	var lateral *breach.Edge
	for i := range shellEdges {
		if shellEdges[i].From == "state@shell" && shellEdges[i].Action == breach.ActionLateral {
			lateral = &shellEdges[i]
		}
	}
	if lateral == nil {
		t.Fatal("lateral edge missing")
	}
	_ = bspace.ConfirmEdge("t1", lateral.ID, "state@dc")

	open, _ := bspace.OpenEdges("t1")
	if len(open) < 2 {
		t.Fatalf("open edges = %d, want >= 2", len(open))
	}

	// maximize(默认):dist 升序 → 先认领距 dc 最近的(shell 的边 dist=1)
	sMax := &Scheduler{bspace: bspace, taskProfile: config.TaskProfileConfig{}}
	first := sMax.pickBreachEdge("t1", open)
	if first == nil || first.From != "state@shell" {
		t.Errorf("maximize first = %+v, want shell 的边(dist=1 优先)", first)
	}

	// specific:只认领可达 goal 的边(shell 的边);不可达边(web 其他 open 边)不派
	sSpec := &Scheduler{bspace: bspace, taskProfile: config.TaskProfileConfig{GoalScope: "specific"}}
	first = sSpec.pickBreachEdge("t1", open)
	if first == nil || first.From != "state@shell" {
		t.Errorf("specific first = %+v, want 可达 goal 的 shell 边", first)
	}

	// 全不可达:goal 改到孤立节点 → 回退最近候选(防死锁)
	bspace.SetGoal("state@nowhere")
	first = sSpec.pickBreachEdge("t1", open)
	if first == nil {
		t.Error("全不可达必须回退候选(防死锁)")
	}
}

// ------------------------------------------------------------------ 凭据 → 登录后攻击面闭环

// TestAuthGainedReopenSkippedAuth 验收:explore 契约声明 auth_gained:true
// (含万能密码/认证绕过等非明文凭据场景)后,"缺登录态"跳过的格子被确定性重开。
// 触发依据是结构化声明而非文本匹配——文本里没有"密码:xxx"也照样触发。
func TestAuthGainedReopenSkippedAuth(t *testing.T) {
	s, _, _ := newTestScheduler(t, "http://127.0.0.1:1", testCfg())
	// 预置:因缺登录态跳过的格子
	_ = s.cov.EnsureOpen("easy-1", "", "shop.example.com", "/admin", coverage.FormUnknown)
	_ = s.cov.MarkSkipped("easy-1", "shop.example.com", "/admin", "需登录态")

	rw := &runningWorker{
		record:    &blackboard.Worker{ID: "w-1", ChallengeID: "easy-1", WorkerType: WorkerOperator},
		phase:     PhaseExplore,
		launchSeq: 0,
	}
	// 万能密码登录成功:文本无"密码:xxx"赋值形式,但 auth_gained 声明触发重开
	res := &executor.Result{FinalText: `{"accepted":true,"findings":[{"prefix":"obs:","text":"万能密码 ' OR '1'='1 登录成功","weight":0.9,"evidence_ref":"exec-1","auth_gained":true}],"new_intents":[],"dead_ends":[],"stop_reason":"intent_done"}`}
	s.applyContract(context.Background(), rw, res, "done")

	cells, _ := s.cov.Cells("easy-1")
	if len(cells) != 1 || cells[0].Status != coverage.StatusOpen {
		t.Fatalf("auth_gained 声明后 /admin 格子应重开为 open, got %+v", cells)
	}
	if cells[0].SkipReason != "" {
		t.Errorf("重开格子 skip_reason 应清空: %q", cells[0].SkipReason)
	}
}

// TestNoAuthGainedKeepsSkipped 负向:契约未声明 auth_gained(即使文本含
// "密码"字样)时,登录态跳过格子保持 skipped——文本不参与判定。
func TestNoAuthGainedKeepsSkipped(t *testing.T) {
	s, _, _ := newTestScheduler(t, "http://127.0.0.1:1", testCfg())
	_ = s.cov.EnsureOpen("easy-1", "", "shop.example.com", "/admin", coverage.FormUnknown)
	_ = s.cov.MarkSkipped("easy-1", "shop.example.com", "/admin", "需登录态")

	rw := &runningWorker{
		record:    &blackboard.Worker{ID: "w-1", ChallengeID: "easy-1", WorkerType: WorkerOperator},
		phase:     PhaseExplore,
		launchSeq: 0,
	}
	// 文本含"密码"但未声明 auth_gained → 不触发(防探针/描述类文本误报)
	res := &executor.Result{FinalText: `{"accepted":true,"findings":[{"prefix":"obs:","text":"/login 探测:参数 password=abc 返回 200","weight":0.8,"evidence_ref":"exec-1"}],"new_intents":[],"dead_ends":[],"stop_reason":"intent_done"}`}
	s.applyContract(context.Background(), rw, res, "done")

	cells, _ := s.cov.Cells("easy-1")
	if len(cells) != 1 || cells[0].Status != coverage.StatusSkipped {
		t.Fatalf("未声明 auth_gained 时格子应保持 skipped, got %+v", cells)
	}
}

// ------------------------------------------------------------------ 2.22:breach 能力维度 + 第 3 层自由边

// newBreachTestScheduler 装配 goal_shape=breach 的测试调度器。
func newBreachTestScheduler(t *testing.T) (*Scheduler, *breach.Space, *store.DB) {
	t.Helper()
	db := testutil.NewTestDB(t)
	board := blackboard.New(db)
	p, _ := provider.New("openai", provider.Config{Name: "mock", BaseURL: "http://127.0.0.1:1", Model: "m", APIKey: "k"})
	reg := tool.NewRegistry()
	reg.Add(&fakeBashTool{})
	ledger := constraint.NewCostLedger(db)
	hub := constraint.NewTerminationHubWithPolicy(board, constraint.NewMeter(db, constraint.Budget{}), nil,
		constraint.Policy{GoalShape: constraint.GoalShapeBreach})
	bspace := breach.New(db)
	disp := dispatcher.New(board, event.Discard, nil)
	s := New(Options{
		Cfg: testCfg(), Board: board, Dispatcher: disp, Hub: hub,
		Providers: map[string]provider.Provider{"mock": p}, Registry: reg,
		Sink: event.Discard, Ledger: ledger,
		ConvStore: conversation.NewStore(db), CompactCfg: conversation.DefaultCompactConfig(),
		WorkDir: t.TempDir(), Breach: bspace,
	})
	disp.SetHooks(s)
	return s, bspace, db
}

// TestAuthGainedBreachPrivilegeNode 验收:breach 模式下 auth_gained 声明 →
// asset 节点 privilege=authenticated(能力维度,任意攻击路径汇聚),登录后
// 转移边展开,执行中边 confirmed(2.22)。
func TestAuthGainedBreachPrivilegeNode(t *testing.T) {
	s, bspace, _ := newBreachTestScheduler(t)
	// 预置:web 入口已确认 + 基线边 + 认领"注入→RCE"边
	if _, err := bspace.ConfirmNode("easy-1", "state@shop.example.com", "web", "shop.example.com", ""); err != nil {
		t.Fatal(err)
	}
	_ = bspace.AddTransitions("easy-1", "state@shop.example.com")
	edges, _ := bspace.Edges("easy-1")
	var rce *breach.Edge
	for i := range edges {
		if edges[i].Action == breach.ActionInjectRCE {
			rce = &edges[i]
		}
	}
	if rce == nil {
		t.Fatal("baseline edge missing")
	}
	_ = bspace.ClaimEdge(rce.ID)

	rw := &runningWorker{
		record:    &blackboard.Worker{ID: "w-1", ChallengeID: "easy-1", WorkerType: WorkerOperator},
		phase:     PhaseExplore,
		launchSeq: 0,
		edgeID:    rce.ID,
	}
	// 万能密码登录成功(非明文凭据场景),auth_gained 声明
	res := &executor.Result{FinalText: `{"accepted":true,"findings":[{"prefix":"obs:","text":"万能密码 ' OR '1'='1 登录成功","weight":0.9,"evidence_ref":"exec-1","asset":"shop.example.com","auth_gained":true}],"new_intents":[],"dead_ends":[],"stop_reason":"intent_done"}`}
	s.applyContract(context.Background(), rw, res, "done")

	// 节点能力:privilege=authenticated
	n, _ := bspace.NodeByID("state@shop.example.com")
	if n == nil || n.Privilege != breach.PrivilegeAuthenticated {
		t.Fatalf("node privilege = %+v, want authenticated", n)
	}
	// 登录后转移边展开(数据访问/管理功能/越权/凭据收集)
	edges, _ = bspace.Edges("easy-1")
	got := map[string]bool{}
	for _, e := range edges {
		got[e.Action] = true
	}
	for _, want := range []string{breach.ActionAuthData, breach.ActionAuthAdmin, breach.ActionAuthPrivEsc, breach.ActionCredGrab} {
		if !got[want] {
			t.Errorf("auth action %q missing", want)
		}
	}
	// 执行中边达成 → confirmed
	edge, _ := bspace.EdgeByID("easy-1", rce.ID)
	if edge == nil || edge.Status != breach.EdgeConfirmed || edge.To != "state@shop.example.com" {
		t.Errorf("executing edge should be confirmed to auth node, got %+v", edge)
	}
}

// TestBreachNewIntentBecomesAgentEdge 验收:breach 模式下 explore 回报的
// new_intent 落地为自由方向边(第 3 层,映射外路径有出口);无锚点(edgeID
// 为空)不建边(黑板仍留痕)。
func TestBreachNewIntentBecomesAgentEdge(t *testing.T) {
	s, bspace, _ := newBreachTestScheduler(t)
	if _, err := bspace.ConfirmNode("easy-1", "state@web", "web", "shop.example.com", ""); err != nil {
		t.Fatal(err)
	}
	_ = bspace.AddTransitions("easy-1", "state@web")
	edges, _ := bspace.Edges("easy-1")
	var rce *breach.Edge
	for i := range edges {
		if edges[i].Action == breach.ActionInjectRCE {
			rce = &edges[i]
		}
	}
	_ = bspace.ClaimEdge(rce.ID)
	rw := &runningWorker{
		record:    &blackboard.Worker{ID: "w-1", ChallengeID: "easy-1", WorkerType: WorkerOperator},
		phase:     PhaseExplore,
		launchSeq: 0,
		edgeID:    rce.ID,
	}
	// 映射外路径:JWT 伪造后直接构造 admin token(无确定性映射)
	res := &executor.Result{FinalText: `{"accepted":true,"findings":[],"new_intents":[{"text":"构造 admin JWT 提权","weight":0.8}],"dead_ends":[],"stop_reason":"intent_done"}`}
	s.applyContract(context.Background(), rw, res, "done")

	agentEdge, _ := bspace.EdgeByFromAction("easy-1", "state@web", "构造 admin JWT 提权")
	if agentEdge == nil || agentEdge.Status != breach.EdgeOpen {
		t.Fatalf("agent edge not created: %+v", agentEdge)
	}
	// 幂等:同方向重复回报不重复建边
	s.applyContract(context.Background(), rw, res, "done")
	edges2, _ := bspace.Edges("easy-1")
	n := 0
	for _, e := range edges2 {
		if e.Action == "构造 admin JWT 提权" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("agent edge duplicated: %d", n)
	}
	// 无锚点(new_intent 但无 edgeID):不建边(黑板仍留痕)
	rw2 := &runningWorker{
		record:    &blackboard.Worker{ID: "w-2", ChallengeID: "easy-1", WorkerType: WorkerOperator},
		phase:     PhaseExplore,
		launchSeq: 0,
	}
	res2 := &executor.Result{FinalText: `{"accepted":true,"findings":[],"new_intents":[{"text":"无锚点方向","weight":0.5}],"dead_ends":[],"stop_reason":"intent_done"}`}
	s.applyContract(context.Background(), rw2, res2, "done")
	if anchor, _ := bspace.EdgeByFromAction("easy-1", "state@web", "无锚点方向"); anchor != nil {
		t.Errorf("unanchored new_intent must not create edge: %+v", anchor)
	}
}

// TestAuthGainedNoBreachInCoverageMode 负向:coverage 模式下 auth_gained
// 不写 breach 状态空间(仅重开矩阵格子)。
func TestAuthGainedNoBreachInCoverageMode(t *testing.T) {
	s, _, _ := newTestScheduler(t, "http://127.0.0.1:1", testCfg())
	rw := &runningWorker{
		record:    &blackboard.Worker{ID: "w-1", ChallengeID: "easy-1", WorkerType: WorkerOperator},
		phase:     PhaseExplore,
		launchSeq: 0,
	}
	res := &executor.Result{FinalText: `{"accepted":true,"findings":[{"prefix":"obs:","text":"登录成功","weight":0.9,"evidence_ref":"exec-1","asset":"shop.example.com","auth_gained":true}],"new_intents":[],"dead_ends":[],"stop_reason":"intent_done"}`}
	s.applyContract(context.Background(), rw, res, "done")
	nodes, _ := s.bspace.ConfirmedNodes("easy-1")
	if len(nodes) != 0 {
		t.Errorf("coverage mode must not write breach nodes: %+v", nodes)
	}
}

// ------------------------------------------------------------------ 2.23:privilege 通用能力声明落地

// TestPrivilegeDeclarationUnlocksTransitions 验收:breach 模式下契约
// privilege:"shell" 声明 → 节点能力落地 + shell 转移候选解锁(2.23,
// 2.15 遗留"shell/域用户触发"落地)。
func TestPrivilegeDeclarationUnlocksTransitions(t *testing.T) {
	s, bspace, _ := newBreachTestScheduler(t)
	// 预置:web 入口已确认(无能力),认领注入→RCE 边
	if _, err := bspace.ConfirmNode("easy-1", "state@web-host", "web", "web-host", ""); err != nil {
		t.Fatal(err)
	}
	_ = bspace.AddTransitions("easy-1", "state@web-host")
	edges, _ := bspace.Edges("easy-1")
	var rce *breach.Edge
	for i := range edges {
		if edges[i].Action == breach.ActionInjectRCE {
			rce = &edges[i]
		}
	}
	_ = bspace.ClaimEdge(rce.ID)
	rw := &runningWorker{
		record:    &blackboard.Worker{ID: "w-1", ChallengeID: "easy-1", WorkerType: WorkerOperator},
		phase:     PhaseExplore,
		launchSeq: 0,
		edgeID:    rce.ID,
	}
	// getshell 成功:privilege:"shell" 声明(通用能力通道,非 auth_gained)
	res := &executor.Result{FinalText: `{"accepted":true,"findings":[{"prefix":"obs:","text":"RCE getshell 成功","weight":0.9,"evidence_ref":"exec-1","asset":"web-host","privilege":"shell"}],"new_intents":[],"dead_ends":[],"stop_reason":"intent_done"}`}
	s.applyContract(context.Background(), rw, res, "done")

	n, _ := bspace.NodeByID("state@web-host")
	if n == nil || n.Privilege != breach.PrivilegeShell {
		t.Fatalf("node privilege = %+v, want shell", n)
	}
	// shell 转移候选解锁(横向移动/提权/凭据收集)+ web 基线
	edges, _ = bspace.Edges("easy-1")
	got := map[string]bool{}
	for _, e := range edges {
		got[e.Action] = true
	}
	for _, want := range []string{breach.ActionLateral, breach.ActionPrivEsc, breach.ActionCredGrab, breach.ActionInjectRCE} {
		if !got[want] {
			t.Errorf("capability action %q missing", want)
		}
	}
	// 执行中边达成 → confirmed
	edge, _ := bspace.EdgeByID("easy-1", rce.ID)
	if edge == nil || edge.Status != breach.EdgeConfirmed || edge.To != "state@web-host" {
		t.Errorf("executing edge should be confirmed, got %+v", edge)
	}
}

// TestAuthGainedPlusPrivilegeMerge 验收:auth_gained 与 privilege 并存 →
// 能力累加合并(shell,authenticated),两类转移候选同时解锁。
func TestAuthGainedPlusPrivilegeMerge(t *testing.T) {
	s, bspace, _ := newBreachTestScheduler(t)
	rw := &runningWorker{
		record:    &blackboard.Worker{ID: "w-1", ChallengeID: "easy-1", WorkerType: WorkerOperator},
		phase:     PhaseExplore,
		launchSeq: 0,
	}
	// 登录成功 + 获得 shell:auth_gained 与 privilege 同时声明
	res := &executor.Result{FinalText: `{"accepted":true,"findings":[{"prefix":"obs:","text":"登录并 getshell","weight":0.9,"evidence_ref":"exec-1","asset":"web-host","auth_gained":true,"privilege":"shell"}],"new_intents":[],"dead_ends":[],"stop_reason":"intent_done"}`}
	s.applyContract(context.Background(), rw, res, "done")

	n, _ := bspace.NodeByID("state@web-host")
	if n == nil || n.Privilege != "shell,authenticated" {
		t.Fatalf("node privilege = %+v, want shell,authenticated(能力累加)", n)
	}
	edges, _ := bspace.Edges("easy-1")
	got := map[string]bool{}
	for _, e := range edges {
		got[e.Action] = true
	}
	// shell 动作 + 登录后动作同时解锁
	for _, want := range []string{breach.ActionLateral, breach.ActionAuthData, breach.ActionAuthAdmin, breach.ActionAuthPrivEsc} {
		if !got[want] {
			t.Errorf("merged capability action %q missing", want)
		}
	}
}

// TestPrivilegeNoBreachInCoverageMode 负向:coverage 模式下 privilege 声明
// 不写 breach 状态空间(与 auth_gained 同规则)。
func TestPrivilegeNoBreachInCoverageMode(t *testing.T) {
	s, _, _ := newTestScheduler(t, "http://127.0.0.1:1", testCfg())
	rw := &runningWorker{
		record:    &blackboard.Worker{ID: "w-1", ChallengeID: "easy-1", WorkerType: WorkerOperator},
		phase:     PhaseExplore,
		launchSeq: 0,
	}
	res := &executor.Result{FinalText: `{"accepted":true,"findings":[{"prefix":"obs:","text":"getshell","weight":0.9,"evidence_ref":"exec-1","asset":"web-host","privilege":"shell"}],"new_intents":[],"dead_ends":[],"stop_reason":"intent_done"}`}
	s.applyContract(context.Background(), rw, res, "done")
	nodes, _ := s.bspace.ConfirmedNodes("easy-1")
	if len(nodes) != 0 {
		t.Errorf("coverage mode must not write breach nodes: %+v", nodes)
	}
}

// TestContractTextDocumentsPrivilege 验收:契约说明与 explore 纪律包含
// privilege 声明指引(拿到权限不声明 = 后续方向不扩展)。
func TestContractTextDocumentsPrivilege(t *testing.T) {
	for _, want := range []string{`"privilege":"shell"`, "shell / domain_user", "横向移动/提权/域枚举/登录后方向"} {
		if !strings.Contains(contractText, want) {
			t.Errorf("contractText missing %q", want)
		}
	}
	if !strings.Contains(promptText("", "executor", executorTemplate), "privilege:") {
		t.Error("executorTemplate missing privilege 纪律")
	}
}
