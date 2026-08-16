package bench

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"scopeforge/internal/blackboard"
	"scopeforge/internal/constraint"
	"scopeforge/internal/dispatcher"
	"scopeforge/internal/event"
	"strings"

	"scopeforge/internal/testutil"
)

// TestMockSRCBenchFullLoop 验收(03 §5):预埋 3 漏洞 mock 靶场 → scripted LLM
// 探索 → 回执推进 → recall=3/3、fpr=0、覆盖收敛终止(coverage_converged)。
func TestMockSRCBenchFullLoop(t *testing.T) {
	cfg := DefaultRunnerConfig()
	cfg.OutDir = t.TempDir()
	r, err := NewRunner(cfg)
	if err != nil {
		t.Fatalf("new runner: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	out, err := r.Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if !out.Terminated {
		t.Fatalf("not terminated: %+v", out.Metrics)
	}
	if out.Reason != string(constraint.ReasonCoverageConverged) {
		t.Errorf("reason = %s, want coverage_converged", out.Reason)
	}
	m := out.Metrics
	if m.Recall != 1.0 {
		t.Errorf("recall = %v, want 1.0 (found=%v missed=%v)", m.Recall, m.Found, m.Missed)
	}
	if m.FPR != 0 {
		t.Errorf("fpr = %v, want 0", m.FPR)
	}
	if m.Coverage != 1.0 {
		t.Errorf("coverage = %v, want 1.0 (全格子终态)", m.Coverage)
	}
	if !m.Converged {
		t.Errorf("converged = false, want true (非预算硬断)")
	}
	if m.Accepted != 3 {
		t.Errorf("accepted = %d, want 3", m.Accepted)
	}
	recallOK, fprOK := m.Pass(DefaultThresholds())
	if !recallOK || !fprOK {
		t.Errorf("pass = %v/%v, want true/true (README 验收底线 1)", recallOK, fprOK)
	}
	// 记账:mock 模式标记(11 §3-④ 与 real 分开记账)
	if out.LLMMode != "mock" || out.Model != "mock-model" {
		t.Errorf("llm mode = %s/%s, want mock/mock-model", out.LLMMode, out.Model)
	}

	// JSONL 落盘可审计
	raw, err := os.ReadFile(filepath.Join(cfg.OutDir, "bench-runs.jsonl"))
	if err != nil {
		t.Fatalf("jsonl: %v", err)
	}
	var rec RunResult
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatalf("jsonl parse: %v", err)
	}
	if rec.Metrics.Recall != 1.0 {
		t.Errorf("jsonl recall = %v, want 1.0", rec.Metrics.Recall)
	}
	if rec.LLMMode != "mock" {
		t.Errorf("jsonl llm_mode = %s, want mock", rec.LLMMode)
	}
}

// TestJudgeSubmittedAllEntries 回归(review blocking):多条 submitted 同时存在时
// 全部判定,不允许漏判——旧实现用 ID 游标 + Vulnerabilities 倒序返回,
// 每轮只判最新一条,其余永久停留 submitted(recall/fpr 低估)。
func TestJudgeSubmittedAllEntries(t *testing.T) {
	db := testutil.NewTestDB(t)
	board := blackboard.New(db)
	disp := dispatcher.New(board, event.Discard, nil)
	r := &Runner{judger: NewJudger(DefaultSRCTarget()), board: board, disp: disp, chID: "c1"}

	ctx := context.Background()
	must := func(_ *blackboard.Vulnerability, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("report: %v", err)
		}
	}
	// 同批 3 条 submitted:两条命中预埋(v1/v3),一条未预埋
	must(disp.ReportVulnerability(ctx, "w1", "c1", blackboard.VulnerabilityIn{CWE: "CWE-89", Asset: "shop.example.com", Endpoint: "/login", Severity: "high", Title: "t1"}))
	must(disp.ReportVulnerability(ctx, "w2", "c1", blackboard.VulnerabilityIn{CWE: "CWE-639", Asset: "shop.example.com", Endpoint: "/api/order", Severity: "high", Title: "t3"}))
	must(disp.ReportVulnerability(ctx, "w3", "c1", blackboard.VulnerabilityIn{CWE: "CWE-22", Asset: "shop.example.com", Endpoint: "/x", Severity: "low", Title: "t-x"}))

	r.judgeSubmitted(ctx)

	vulns, err := board.Vulnerabilities("c1")
	if err != nil {
		t.Fatalf("vulns: %v", err)
	}
	counts := map[string]int{}
	for _, v := range vulns {
		counts[v.Status]++
	}
	if counts[blackboard.LedgerAccepted] != 2 {
		t.Errorf("accepted = %d, want 2 (全部命中条目判定,不漏判)", counts[blackboard.LedgerAccepted])
	}
	if counts[blackboard.LedgerFalsePositive] != 1 {
		t.Errorf("false_positive = %d, want 1", counts[blackboard.LedgerFalsePositive])
	}
	if counts[blackboard.LedgerSubmitted] != 0 {
		t.Errorf("submitted 残留 = %d, want 0 (游标漏判回归)", counts[blackboard.LedgerSubmitted])
	}
	// 幂等:重复调用不改变判定(accepted map 去重)
	r.judgeSubmitted(ctx)
	vulns, _ = board.Vulnerabilities("c1")
	for _, v := range vulns {
		if v.Status == blackboard.LedgerSubmitted {
			t.Fatalf("second judge: submitted 残留 %+v", v)
		}
	}
}

// 不影响 recall;经 LLMBase 注入路径(真实端点模式)验证 runner 装配。
// TestMockSRCBenchFalsePositive 验收:未预埋提交 → false_positive 回执计入 fpr,
// 不影响 recall;经 LLMBase 注入路径(真实端点模式)验证 runner 装配。
func TestMockSRCBenchFalsePositive(t *testing.T) {
	llm := newScriptedLLM(DefaultSRCTarget())
	defer llm.Close()
	// 向队列注入一个未预埋条目(提交后 judge → false_positive)
	llm.mu.Lock()
	llm.queue = append(llm.queue, Seed{CWE: "CWE-22", Asset: "shop.example.com", Endpoint: "/download", Severity: "low", Title: "未预埋的路径穿越(误报)"})
	llm.mu.Unlock()

	cfg := DefaultRunnerConfig()
	cfg.LLMBase = llm.URL
	cfg.LLMModel = "mock-model"
	cfg.OutDir = t.TempDir()
	r, err := NewRunner(cfg)
	if err != nil {
		t.Fatalf("new runner: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	out, err := r.Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	m := out.Metrics
	if m.Recall != 1.0 {
		t.Errorf("recall = %v, want 1.0 (误报不影响 recall)", m.Recall)
	}
	if m.FalsePositive != 1 {
		t.Errorf("false_positive = %d, want 1", m.FalsePositive)
	}
	if m.FPR <= 0 {
		t.Errorf("fpr = %v, want > 0 (误报计入)", m.FPR)
	}
	// 账本终态检查:存在 false_positive 条目(快照在 db 关闭前取数)
	hasFP := false
	for _, v := range r.LastVulns {
		if v.Status == blackboard.LedgerFalsePositive {
			hasFP = true
		}
	}
	if !hasFP {
		t.Error("ledger: no false_positive entry")
	}
}

// TestJuiceSetConfig juice 集装配回归(11 §3-④ 真实 LLM 端到端):
// 必须真实 LLM(mock 只会复述种子,度量不了真实能力);默认目标地址;
// 种子归一化键与提交侧同规(asset 保留端口、endpoint 去 query)。
func TestJuiceSetConfig(t *testing.T) {
	// 无真实 LLM → fail-fast
	if _, err := NewRunner(RunnerConfig{SetName: "juice"}); err == nil {
		t.Error("juice set without real LLM must fail")
	}
	// 配真实 LLM → 默认目标地址 + juice-shop 预埋集 + real 记账
	r, err := NewRunner(RunnerConfig{SetName: "juice", LLMBase: "http://127.0.0.1:9/v1", LLMModel: "m"})
	if err != nil {
		t.Fatalf("new runner: %v", err)
	}
	if r.cfg.TargetURL != "http://127.0.0.1:3000" {
		t.Errorf("target url = %q, want default", r.cfg.TargetURL)
	}
	if r.llmMode() != "real" {
		t.Errorf("llm mode = %s, want real", r.llmMode())
	}
	if len(r.target.Seeds) != 3 {
		t.Fatalf("juice seeds = %d, want 3", len(r.target.Seeds))
	}
	// 匹配键归一化:模型提交 URL 形态(asset 带协议/endpoint 带 query)也能命中
	if s := r.target.MatchSeed("cwe-89", "http://127.0.0.1:3000/", "/rest/products/search?q=xyz"); s == nil {
		t.Error("CWE-89 @ /rest/products/search must match normalized")
	}
	if s := r.target.MatchSeed("CWE-639", "127.0.0.1:3000", "/rest/basket"); s == nil {
		t.Error("CWE-639 @ /rest/basket must match")
	}
	// 任务描述必须告诉真实模型目标与提交语义(否则端到端跑的是无头任务)
	desc := r.taskDescription()
	if !strings.Contains(desc, "127.0.0.1:3000") || !strings.Contains(desc, "submit_vulnerability") {
		t.Errorf("task description missing target/semantics: %q", desc)
	}
	// mock 集无任务描述(scripted LLM 不读提示词)
	r2, err := NewRunner(RunnerConfig{SetName: "src"})
	if err != nil {
		t.Fatal(err)
	}
	if r2.taskDescription() != "" {
		t.Error("src set must not inject task description")
	}
}

// TestRealBashTool 真实执行回归:命令真实跑、缺参报错。
func TestRealBashTool(t *testing.T) {
	b := &realBashTool{}
	out, err := b.Execute(context.Background(), json.RawMessage(`{"command":"echo bench-real-ok"}`))
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if !strings.Contains(out, "bench-real-ok") {
		t.Errorf("out = %q", out)
	}
	if _, err := b.Execute(context.Background(), json.RawMessage(`{"command":""}`)); err == nil {
		t.Error("empty command must error")
	}
}
