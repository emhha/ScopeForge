package report

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"scopeforge/internal/blackboard"
	"scopeforge/internal/breach"
	"scopeforge/internal/constraint"
	"scopeforge/internal/coverage"
	"scopeforge/internal/event"
	"scopeforge/internal/testutil"
)

func TestValidateEvidence(t *testing.T) {
	events := []event.Event{
		{Seq: 1, Kind: event.KindTurnStart},
		{Seq: 5, Kind: event.KindTraffic, Payload: map[string]any{"flow_ref": "flow-3"}},
	}
	ok, rejected := ValidateEvidence(events, []string{"ev-1", "ev-5", "flow-3", "ev-99", "flow-404", "ev-abc"})
	if ok != 3 {
		t.Errorf("ok=%d want 3", ok)
	}
	if len(rejected) != 3 {
		t.Errorf("rejected=%v", rejected)
	}
	for _, r := range rejected {
		if r == "ev-1" || r == "ev-5" || r == "flow-3" {
			t.Errorf("valid ref rejected: %s", r)
		}
	}
}

func TestExtractRefs(t *testing.T) {
	md := "evidence ev-42 and flow-7, also ev-42 again and bogus ev-xyz"
	refs := extractRefs(md)
	if len(refs) != 2 || refs[0] != "ev-42" || refs[1] != "flow-7" {
		t.Errorf("refs=%v", refs)
	}
}

func TestRedact(t *testing.T) {
	md := "target 10.10.0.5:8080, internal 192.168.1.10, key sk-abcdef123456789012"
	out := Redact(md)
	if strings.Contains(out, "10.10.0.5") || strings.Contains(out, "192.168.1.10") {
		t.Errorf("internal IP not redacted: %q", out)
	}
	if strings.Contains(out, "sk-abcdef123456789012") {
		t.Errorf("credential not redacted: %q", out)
	}
	if !strings.Contains(out, "[internal-ip]") {
		t.Errorf("redaction marker missing: %q", out)
	}
	// 公网 IP 不脱敏
	out2 := Redact("8.8.8.8 and 1.1.1.1")
	if !strings.Contains(out2, "8.8.8.8") {
		t.Error("public IP should not be redacted")
	}
}

func TestEventSummary(t *testing.T) {
	cases := []struct {
		e    event.Event
		want string
	}{
		{event.Event{Kind: event.KindTurnStart}, "turn start"},
		{event.Event{Kind: event.KindToolCallStart, Payload: map[string]any{"name": "bash"}}, "tool: bash"},
		{event.Event{Kind: event.KindToolCallResult, Payload: map[string]any{"name": "bash", "error": true}}, "result: bash (error)"},
		{event.Event{Kind: event.KindSubmission, Payload: map[string]any{"result": "correct"}}, "submission → correct"},
		{event.Event{Kind: event.KindTraffic, Payload: map[string]any{"method": "GET", "host": "t", "path": "/x"}}, "traffic GET t/x"},
		{event.Event{Kind: event.KindSteer}, "steer"},
		{event.Event{Kind: event.KindTextDelta}, ""},
	}
	for _, c := range cases {
		if got := eventSummary(c.e); got != c.want {
			t.Errorf("eventSummary(%s)=%q want %q", c.e.Kind, got, c.want)
		}
	}
}

// ------------------------------------------------------------------ phase2 切片5:报告 goal_shape 分叉(05 §2)

func TestRenderCoverageReport(t *testing.T) {
	db := testutil.NewTestDB(t)
	board := blackboard.New(db)
	// 账本:accepted + submitted 各一
	if _, err := board.AddVulnerability("t1", blackboard.VulnerabilityIn{
		CWE: "CWE-89", Asset: "shop.example.com", Endpoint: "/login", Severity: "high", Title: "SQLi", EvidenceRef: "ev-5",
	}); err != nil {
		t.Fatal(err)
	}
	vulns, _ := board.Vulnerabilities("t1")
	if err := board.UpdateVulnerabilityStatus(vulns[0].ID, blackboard.LedgerAccepted, "r1"); err != nil {
		t.Fatal(err)
	}
	if _, err := board.AddVulnerability("t1", blackboard.VulnerabilityIn{
		CWE: "CWE-79", Asset: "shop.example.com", Endpoint: "/search", Severity: "medium", Title: "XSS",
	}); err != nil {
		t.Fatal(err)
	}
	// 覆盖矩阵:confirmed + open 各一
	cov := coverage.New(db)
	_ = cov.EnsureOpen("t1", "CWE-89", "shop.example.com", "/login", coverage.FormParam)
	_ = cov.MarkConfirmed("t1", "CWE-89", "shop.example.com", "/login")
	_ = cov.EnsureOpen("t1", "", "shop.example.com", "/api", coverage.FormParam)
	// 剩余风险:open intent(未认领方向,任务终止时仍未覆盖)
	_, _ = board.AddIntent("t1", "未知未覆盖: /api 需登录态", 0.5, "w")
	if _, err := board.Intents("t1", nil, 0); err != nil {
		t.Fatal(err)
	}
	// 复现步骤事件
	events := []event.Event{
		{Seq: 1, Kind: event.KindToolCallStart, Payload: map[string]any{"name": "bash", "args": `{"command":"curl -s http://shop/login"}`}},
		{Seq: 5, Kind: event.KindToolCallStart, Payload: map[string]any{"name": "bash", "args": `{"command":"sqlmap -u http://shop/login"}`}},
	}

	g := &Generator{Board: board, Coverage: cov}
	md := g.renderCoverage("t1", nil, events, constraint.Spend{CostUSD: 1.2, Turns: 10})
	for _, want := range []string{
		"# ScopeForge 漏洞报告(coverage)",
		"## 2. 漏洞清单",
		"CWE-89", "shop.example.com", "/login", "high", "accepted",
		"## 3. 覆盖矩阵", "confirmed", "open",
		"## 4. 复现步骤", "sqlmap",
		"## 5. 剩余风险", "/api 需登录态",
		"## 附录",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("coverage report missing %q\n%s", want, md)
		}
	}
}

func TestRenderBreachReport(t *testing.T) {
	db := testutil.NewTestDB(t)
	board := blackboard.New(db)
	bspace := breach.New(db)
	bspace.SetGoal("state@dc")
	// 状态:web 入口 → shell 已确认;dc 未达成
	_, _ = bspace.ConfirmNode("t1", "state@web", "web", "web-host", "")
	_ = bspace.AddTransitions("t1", "state@web")
	if err := bspace.ClaimEdge(bspaceEdges(t, bspace, "t1")[0].ID); err != nil {
		t.Fatal(err)
	}
	// 确认注入→RCE 边与 shell 节点
	edges := bspaceEdges(t, bspace, "t1")
	found := false
	for _, e := range edges {
		if e.Action == breach.ActionInjectRCE {
			_ = bspace.ConfirmEdge("t1", e.ID, "state@shell")
			found = true
		}
	}
	if !found {
		t.Fatal("注入RCE edge not generated")
	}
	// 一条 dead 边(未达成路径)
	for _, e := range edges {
		if e.Action == breach.ActionSSRFInner {
			_ = bspace.MarkEdgeDead(e.ID)
		}
	}
	// 账本一条
	if _, err := board.AddVulnerability("t1", blackboard.VulnerabilityIn{
		CWE: "CWE-89", Asset: "web-host", Endpoint: "/", Severity: "high", Title: "SQLi→RCE",
	}); err != nil {
		t.Fatal(err)
	}

	g := &Generator{Board: board, Breach: bspace, GoalShape: constraint.GoalShapeBreach}
	md := g.renderBreach("t1", nil, nil, constraint.Spend{CostUSD: 0.5, Turns: 8})
	for _, want := range []string{
		"# ScopeForge 渗透测试报告(breach)",
		"## 2. 攻击链",
		"state@web", "state@shell",
		"## 3. 权限证明",
		"## 4. 漏洞清单", "SQLi→RCE",
		"## 5. 未达成路径", "SSRF",
		"## 6. 处置建议", "优先修复入口",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("breach report missing %q\n%s", want, md)
		}
	}
}

func bspaceEdges(t *testing.T, s *breach.Space, challengeID string) []breach.Edge {
	t.Helper()
	edges, err := s.Edges(challengeID)
	if err != nil {
		t.Fatal(err)
	}
	return edges
}

// ------------------------------------------------------------------ phase2 B:导出/签名/脱敏 fail-closed(05 §3/§5)

// TestRedactPII 验收(05 §3):个人数据(邮箱/手机/身份证)强制打码。
func TestRedactPII(t *testing.T) {
	md := "contact alice@example.com or 13812345678, id 110101199003078654"
	out := Redact(md)
	for _, bad := range []string{"alice@example.com", "13812345678", "110101199003078654"} {
		if strings.Contains(out, bad) {
			t.Errorf("PII not redacted: %q in %q", bad, out)
		}
	}
	for _, want := range []string{"[email]", "[phone]"} {
		if !strings.Contains(out, want) {
			t.Errorf("marker %s missing: %q", want, out)
		}
	}
	// 身份证可能被凭据脱敏器先行打码(****)或 PII 正则([id-card]),值必须隐藏
	if strings.Contains(out, "110101199003078654") {
		t.Errorf("id-card not hidden: %q", out)
	}
}

// TestGenerateFailClosed 验收(05 §3):无脱敏配置 → 未脱敏生成被拒(fail-closed);
// 显式允许后放行,且未脱敏版独立文件留痕。
func TestGenerateFailClosed(t *testing.T) {
	dir := t.TempDir()
	db := testutil.NewTestDB(t)
	board := blackboard.New(db)
	_, _ = board.AddVulnerability("t1", blackboard.VulnerabilityIn{
		CWE: "CWE-89", Asset: "shop.example.com", Endpoint: "/login", Severity: "high", Title: "SQLi",
	})
	g := &Generator{Board: board, DB: db, WorkDir: dir}
	// 默认:未脱敏被拒
	if _, err := g.Generate(nil, "t1", false); err == nil {
		t.Fatal("unredacted generation must be rejected (fail-closed)")
	}
	// 脱敏生成正常
	rep, err := g.Generate(nil, "t1", true)
	if err != nil {
		t.Fatalf("redacted: %v", err)
	}
	if rep.Path == "" || !strings.HasSuffix(rep.Path, "t1.md") {
		t.Errorf("path = %q", rep.Path)
	}
	// 显式允许后未脱敏放行(独立文件 -unredacted.md)
	g.AllowUnredacted = true
	rep2, err := g.Generate(nil, "t1", false)
	if err != nil {
		t.Fatalf("unredacted with allow: %v", err)
	}
	if !strings.HasSuffix(rep2.Path, "t1-unredacted.md") {
		t.Errorf("unredacted path = %q, want t1-unredacted.md", rep2.Path)
	}
}

// TestExportJSON 验收(05 §5):JSON 导出(机器可读,含证据引用)落盘 reports/<id>.json。
func TestExportJSON(t *testing.T) {
	dir := t.TempDir()
	db := testutil.NewTestDB(t)
	board := blackboard.New(db)
	_, _ = board.AddVulnerability("t1", blackboard.VulnerabilityIn{
		CWE: "CWE-89", Asset: "shop.example.com", Endpoint: "/login",
		Severity: "high", Title: "SQLi", EvidenceRef: "ev-5",
	})
	cov := coverage.New(db)
	_ = cov.EnsureOpen("t1", "CWE-89", "shop.example.com", "/login", coverage.FormParam)
	_ = cov.MarkConfirmed("t1", "CWE-89", "shop.example.com", "/login")
	g := &Generator{Board: board, DB: db, Coverage: cov, WorkDir: dir}

	out, err := g.ExportJSON(nil, "t1")
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if out.ChallengeID != "t1" || out.GoalShape != constraint.GoalShapeCoverage {
		t.Errorf("meta = %+v", out)
	}
	if len(out.Vulns) != 1 || out.Vulns[0].CWE != "CWE-89" {
		t.Errorf("vulns = %+v", out.Vulns)
	}
	if len(out.EvidenceRefs) != 1 || out.EvidenceRefs[0] != "ev-5" {
		t.Errorf("evidence refs = %v", out.EvidenceRefs)
	}
	if len(out.Coverage) != 1 || out.Coverage[0].Status != coverage.StatusConfirmed {
		t.Errorf("coverage = %+v", out.Coverage)
	}
	// 落盘校验
	raw, err := os.ReadFile(filepath.Join(dir, "reports", "t1.json"))
	if err != nil {
		t.Fatalf("json file: %v", err)
	}
	if !strings.Contains(string(raw), `"challenge_id": "t1"`) {
		t.Errorf("json content: %s", raw)
	}
}

// TestSign 验收(05 §5):复核签名(报告 hash + 复核人/时间戳)→ meta.json(审计留痕)。
func TestSign(t *testing.T) {
	dir := t.TempDir()
	db := testutil.NewTestDB(t)
	board := blackboard.New(db)
	_, _ = board.AddVulnerability("t1", blackboard.VulnerabilityIn{
		CWE: "CWE-89", Asset: "shop.example.com", Endpoint: "/login", Severity: "high", Title: "SQLi",
	})
	g := &Generator{Board: board, DB: db, WorkDir: dir}

	sig, err := g.Sign(nil, "t1", "alice")
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if sig.Reviewer != "alice" || sig.ReportHash == "" || sig.SignedAt == 0 {
		t.Errorf("sig = %+v", sig)
	}
	if len(sig.ReportHash) != 64 {
		t.Errorf("hash = %q, want sha256 hex (64 chars)", sig.ReportHash)
	}
	// 签名缺复核人拒绝
	if _, err := g.Sign(nil, "t1", ""); err == nil {
		t.Fatal("sign without reviewer must fail")
	}
	// 二次签名累积(复核链)
	sig2, _ := g.Sign(nil, "t1", "bob")
	if sig2.ReportHash != sig.ReportHash {
		t.Errorf("hash changed across signs: %s vs %s", sig.ReportHash, sig2.ReportHash)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "reports", "t1.meta.json"))
	if err != nil {
		t.Fatalf("meta.json: %v", err)
	}
	var sigs []Signature
	if err := json.Unmarshal(raw, &sigs); err != nil {
		t.Fatalf("meta parse: %v", err)
	}
	if len(sigs) != 2 || sigs[0].Reviewer != "alice" || sigs[1].Reviewer != "bob" {
		t.Errorf("meta sigs = %+v", sigs)
	}
}

// TestRenderCoverageInterestFilter 验收(04 §5.2):interest=high 时低危漏洞
// 只记账不进交付报告(摘要/清单只含 high;附录审计仍完整)。
func TestRenderCoverageInterestFilter(t *testing.T) {
	db := testutil.NewTestDB(t)
	board := blackboard.New(db)
	_, _ = board.AddVulnerability("t1", blackboard.VulnerabilityIn{
		CWE: "CWE-89", Asset: "shop.example.com", Endpoint: "/login", Severity: "high", Title: "SQLi",
	})
	_, _ = board.AddVulnerability("t1", blackboard.VulnerabilityIn{
		CWE: "CWE-79", Asset: "shop.example.com", Endpoint: "/search", Severity: "low", Title: "低危 XSS",
	})

	g := &Generator{Board: board, Interest: constraint.Interest{SeverityMin: "high"}}
	md := g.renderCoverage("t1", nil, nil, constraint.Spend{})
	// 漏洞清单节(2. 漏洞清单 → 3. 覆盖矩阵之间)不得含低危
	section := md[strings.Index(md, "## 2. 漏洞清单"):strings.Index(md, "## 3. 覆盖矩阵")]
	if strings.Contains(section, "低危 XSS") {
		t.Errorf("低危漏洞进入交付清单:\n%s", section)
	}
	if !strings.Contains(section, "SQLi") {
		t.Errorf("高危漏洞丢失:\n%s", section)
	}
	if !strings.Contains(md, "interest 过滤") {
		t.Errorf("缺少 interest 过滤说明:\n%s", md)
	}
	// 附录账本提交记录完整(审计留痕:低危仍在)
	if !strings.Contains(md, "低危 XSS") {
		t.Errorf("附录账本记录缺失(审计不完整):\n%s", md)
	}
	// 不过滤时全部交付(零值 Interest)
	g0 := &Generator{Board: board}
	md0 := g0.renderCoverage("t1", nil, nil, constraint.Spend{})
	if !strings.Contains(md0, "低危 XSS") {
		t.Errorf("零值 Interest 应全量交付:\n%s", md0)
	}
}
