package constraint

import (
	"context"
	"testing"

	"scopeforge/internal/blackboard"
	"scopeforge/internal/reasonix/provider"
	"scopeforge/internal/testutil"
)

// ------------------------------------------------------------------ ScopeGate

func TestScopeGateDomain(t *testing.T) {
	g, err := NewScopeGate([]string{"https://target.example.com", "internal.test"})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		target string
		allow  bool
	}{
		{"http://target.example.com/", true},
		{"https://target.example.com:8443/admin", true},
		{"https://sub.target.example.com/x", true}, // 子域
		{"https://evil.com", false},
		{"https://example.com", false}, // 裸域不匹配(规则是 target.example.com)
		{"internal.test", true},
		{"a.b.internal.test", true},
		{"internal.test.evil.com", false},
	}
	for _, c := range cases {
		ok, reason := g.CheckTarget(c.target)
		if ok != c.allow {
			t.Errorf("CheckTarget(%q) = %v (%s), want %v", c.target, ok, reason, c.allow)
		}
	}
}

func TestScopeGateWildcard(t *testing.T) {
	g, err := NewScopeGate([]string{"*.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := g.CheckTarget("https://a.example.com"); !ok {
		t.Error("wildcard subdomain should allow")
	}
	if ok, _ := g.CheckTarget("https://example.com"); ok {
		t.Error("bare domain should not match *.example.com")
	}
}

func TestScopeGateIPAndCIDR(t *testing.T) {
	g, err := NewScopeGate([]string{"10.10.0.0/24", "172.16.5.10"})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		target string
		allow  bool
	}{
		{"http://10.10.0.7:8080", true},
		{"10.10.0.1", true},
		{"10.10.1.1", false},
		{"http://172.16.5.10/", true},
		{"172.16.5.11", false},
		{"http://192.168.1.1", false},
	}
	for _, c := range cases {
		ok, reason := g.CheckTarget(c.target)
		if ok != c.allow {
			t.Errorf("CheckTarget(%q) = %v (%s), want %v", c.target, ok, reason, c.allow)
		}
	}
}

func TestScopeGateDenied(t *testing.T) {
	g, err := NewScopeGate([]string{"*"})
	if err == nil {
		// "*" 作为通配:处理成空通配,无实际放行
		t.Log("wildcard-only gate created")
	}
	g, err = NewScopeGate([]string{"target.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	g.AddDenied("api.deepseek.com", "platform.example.com")
	if ok, _ := g.CheckTarget("https://api.deepseek.com/v1"); ok {
		t.Error("LLM gateway must be denied")
	}
	if ok, _ := g.CheckTarget("https://platform.example.com/submit"); ok {
		t.Error("platform endpoint must be denied")
	}
	if ok, _ := g.CheckTarget("https://target.example.com"); !ok {
		t.Error("target must still be allowed")
	}
}

func TestScopeGateEmptyTargetsDeniesAll(t *testing.T) {
	g, err := NewScopeGate(nil)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := g.CheckTarget("http://anything.com"); ok {
		t.Error("default deny: empty allowlist must reject everything")
	}
}

// ------------------------------------------------------------------ CostLedger / BudgetMeter

func TestCostLedgerAndMeter(t *testing.T) {
	db := testutil.NewTestDB(t)
	ledger := NewCostLedger(db)
	pricing := &provider.Pricing{CacheHit: 0.1, Input: 1.0, Output: 2.0} // 每百万 token 美元
	u := &provider.Usage{PromptTokens: 1000, CacheHitTokens: 400, CacheMissTokens: 600, CompletionTokens: 200}

	if err := ledger.Record("s1", "c1", "recon", "m1", u, pricing); err != nil {
		t.Fatal(err)
	}
	// cache hit 价低于 miss 价:cost = (400*0.1 + 600*1.0 + 200*2.0)/1e6 = (40+600+400)/1e6 = 0.00104
	want := (400*0.1 + 600*1.0 + 200*2.0) / 1e6
	spend, err := ledger.Spend("c1")
	if err != nil {
		t.Fatal(err)
	}
	if spend.CostUSD != want {
		t.Errorf("cost = %v, want %v (cache hit cheaper than miss)", spend.CostUSD, want)
	}
	if spend.Turns != 1 || spend.PromptTokens != 1000 {
		t.Errorf("spend = %+v", spend)
	}
	// 其他 challenge 不受影响
	spend2, _ := ledger.Spend("c2")
	if spend2.CostUSD != 0 || spend2.Turns != 0 {
		t.Errorf("c2 spend = %+v, want zero", spend2)
	}
	// 无 pricing → cost 0
	if err := ledger.Record("s2", "c1", "explore", "m2", u, nil); err != nil {
		t.Fatal(err)
	}

	// 熔断:tokens 维度
	meter := NewMeter(db, Budget{MaxTokensPerChallenge: 1500, MaxTurnsPerChallenge: 10, MaxCostUSD: 100})
	if ok, reason := meter.Check("c1"); ok {
		t.Errorf("tokens budget should trip (spent 1000+1000 >= 1500), reason=%q", reason)
	}
	// turns 维度(3 行 usage = 3 turns > 2)
	meter2 := NewMeter(db, Budget{MaxTurnsPerChallenge: 2})
	if ok, reason := meter2.Check("c1"); ok {
		t.Errorf("turns budget should trip, reason=%q", reason)
	}
	// 不超限
	meter3 := NewMeter(db, Budget{MaxTokensPerChallenge: 100000, MaxTurnsPerChallenge: 100, MaxCostUSD: 100})
	if ok, reason := meter3.Check("c1"); !ok {
		t.Errorf("budget should allow: %q", reason)
	}
}

// ------------------------------------------------------------------ TerminationHub

func TestTerminationThreeWayOR(t *testing.T) {
	db := testutil.NewTestDB(t)
	board := blackboard.New(db)
	meter := NewMeter(db, Budget{})

	// ① 覆盖矩阵收敛(coverage 默认形态)
	conv := &fakeConvergence{converged: false}
	hub := NewTerminationHub(board, meter, nil)
	hub.SetConvergence(conv)
	stop, reason, err := hub.ShouldStop(context.Background(), "c1")
	if err != nil || stop {
		t.Fatalf("not converged must not stop: stop=%v reason=%v err=%v", stop, reason, err)
	}
	conv.converged = true
	stop, reason, _ = hub.ShouldStop(context.Background(), "c1")
	if !stop || reason != ReasonCoverageConverged {
		t.Fatalf("coverage converged: stop=%v reason=%v", stop, reason)
	}

	// ② 预算熔断
	u := &provider.Usage{PromptTokens: 100}
	if err := NewCostLedger(db).Record("s1", "c2", "explore", "m", u, nil); err != nil {
		t.Fatal(err)
	}
	hub2 := NewTerminationHub(board, NewMeter(db, Budget{MaxTurnsPerChallenge: 0, MaxTokensPerChallenge: 50}), nil)
	stop, reason, _ = hub2.ShouldStop(context.Background(), "c2")
	if !stop || reason != ReasonBudget {
		t.Fatalf("budget: stop=%v reason=%v", stop, reason)
	}

	// 模型谎称完成(无收敛、无熔断)不被接受:用无收敛器的 hub 判定
	hub3 := NewTerminationHub(board, meter, nil)
	board.AddFact("c3", blackboard.PrefixFlag, "flag{fake}", 0.9, blackboard.StateConfirmed, "w", "")
	stop, reason, _ = hub3.ShouldStop(context.Background(), "c3")
	if stop {
		t.Fatalf("fake flag must not stop (reason=%v)", reason)
	}
}

// ------------------------------------------------------------------ phase2 切片1:Policy 分叉(02 §2.2/§2.5)

func TestInterestMatch(t *testing.T) {
	all := Interest{}
	if !all.Match("low", "CWE-89") {
		t.Error("empty interest must match everything")
	}
	hi := Interest{SeverityMin: "high"}
	if !hi.Match("high", "CWE-89") || !hi.Match("critical", "") {
		t.Error("high+ must match")
	}
	if hi.Match("medium", "CWE-89") || hi.Match("info", "CWE-89") {
		t.Error("below high must not match")
	}
	cweSet := Interest{CWEs: []string{"CWE-89", "CWE-79"}}
	if !cweSet.Match("low", "CWE-79") {
		t.Error("cwe in set must match")
	}
	if cweSet.Match("low", "CWE-22") {
		t.Error("cwe out of set must not match")
	}
}

// fakeConvergence 模拟覆盖矩阵收敛判定器(切片 3 的真实实现)。
type fakeConvergence struct {
	converged bool
	why       string
	interest  Interest // 记录收到的 interest(断言过滤生效)
	callsN    int
}

func (f *fakeConvergence) IsConverged(ctx context.Context, challengeID string, interest Interest) (bool, string) {
	f.callsN++
	f.interest = interest
	return f.converged, f.why
}

func (f *fakeConvergence) calls() int { return f.callsN }

// fakeGoalVerifier 模拟 breach 目标验证器(切片 2/3 的真实实现)。
type fakeGoalVerifier struct {
	goalReached bool
	goalID      string
	spaceClosed bool
}

func (f *fakeGoalVerifier) IsGoalReached(ctx context.Context, taskID string) (bool, string) {
	return f.goalReached, f.goalID
}
func (f *fakeGoalVerifier) IsSpaceClosed(ctx context.Context, taskID string) (bool, string) {
	return f.spaceClosed, "space_closed"
}

// TestTerminationCoverageAcceptedNotStop 验收:coverage 形态下提交 accepted 只记账,不终止。
func TestTerminationCoverageAcceptedNotStop(t *testing.T) {
	db := testutil.NewTestDB(t)
	board := blackboard.New(db)
	hub := NewTerminationHubWithPolicy(board, NewMeter(db, Budget{}), nil,
		Policy{GoalShape: GoalShapeCoverage, Interest: Interest{SeverityMin: "high"}})
	conv := &fakeConvergence{converged: false}
	hub.SetConvergence(conv)

	// 账本写入 accepted 漏洞(模拟平台接受回执)
	v, err := board.AddVulnerability("c1", blackboard.VulnerabilityIn{
		CWE: "CWE-89", Asset: "shop.example.com", Endpoint: "/login", Severity: "high", Title: "SQLi",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := board.UpdateVulnerabilityStatus(v.ID, blackboard.LedgerAccepted, "receipt-1"); err != nil {
		t.Fatal(err)
	}
	stop, reason, err := hub.ShouldStop(context.Background(), "c1")
	if err != nil || stop {
		t.Fatalf("accepted must NOT stop in coverage mode: stop=%v reason=%v err=%v", stop, reason, err)
	}

	// 收敛后终止(interest=high 传入判定器)
	conv.converged = true
	stop, reason, _ = hub.ShouldStop(context.Background(), "c1")
	if !stop || reason != ReasonCoverageConverged {
		t.Fatalf("converged: stop=%v reason=%v", stop, reason)
	}
	if conv.interest.SeverityMin != "high" {
		t.Errorf("interest not propagated: %+v", conv.interest)
	}
}

// TestTerminationBreachGoalVerified 验收:breach goal 断言经独立验证器确认才终止,
// 模型自述不采信(账本/黑板上的 flag fact 不算)。
func TestTerminationBreachGoalVerified(t *testing.T) {
	db := testutil.NewTestDB(t)
	board := blackboard.New(db)
	hub := NewTerminationHubWithPolicy(board, NewMeter(db, Budget{}), nil,
		Policy{GoalShape: GoalShapeBreach})
	ver := &fakeGoalVerifier{}
	hub.SetGoalVerifier(ver)

	// 模型在黑板自述达成(不采信)
	if _, err := board.AddFact("c1", blackboard.PrefixObs, "我已经拿到主机 root", 0.9, blackboard.StateConfirmed, "w", ""); err != nil {
		t.Fatal(err)
	}
	stop, reason, _ := hub.ShouldStop(context.Background(), "c1")
	if stop {
		t.Fatalf("model self-claim must not stop (reason=%v)", reason)
	}

	// 独立验证器确认 goal
	ver.goalReached = true
	ver.goalID = "web-root"
	stop, reason, _ = hub.ShouldStop(context.Background(), "c1")
	if !stop || reason != ReasonGoalReached {
		t.Fatalf("goal reached: stop=%v reason=%v", stop, reason)
	}

	// 路径闭合终止
	ver.goalReached = false
	ver.spaceClosed = true
	stop, reason, _ = hub.ShouldStop(context.Background(), "c1")
	if !stop || reason != ReasonSpaceClosed {
		t.Fatalf("space closed: stop=%v reason=%v", stop, reason)
	}
}

// TestTerminationConvergenceLimit 收敛检查次数上限:达上限后停止检查,预算兜底。
func TestTerminationConvergenceLimit(t *testing.T) {
	db := testutil.NewTestDB(t)
	board := blackboard.New(db)
	hub := NewTerminationHubWithPolicy(board, NewMeter(db, Budget{}), nil,
		Policy{GoalShape: GoalShapeCoverage, MaxConvergence: 2})
	conv := &fakeConvergence{}
	hub.SetConvergence(conv)

	for i := 0; i < 2; i++ {
		stop, _, err := hub.ShouldStop(context.Background(), "c1")
		if err != nil || stop {
			t.Fatalf("tick %d: stop=%v err=%v", i, stop, err)
		}
	}
	// 第 3 次起不再调用判定器(预算/穷尽声明兜底)
	stop, _, _ := hub.ShouldStop(context.Background(), "c1")
	if stop {
		t.Fatal("must not stop after limit")
	}
	if conv.calls() != 2 {
		t.Errorf("convergence calls = %d, want 2", conv.calls())
	}
}
