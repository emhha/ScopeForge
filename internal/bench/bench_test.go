package bench

import (
	"testing"

	"scopeforge/internal/blackboard"
	"scopeforge/internal/coverage"
)

// TestSeedKeyNormalization 验收:种子匹配走归一化键(03 §1.2 同规)。
func TestSeedKeyNormalization(t *testing.T) {
	target := DefaultSRCTarget()
	cases := []struct {
		cwe, asset, endpoint string
		wantID               string
	}{
		{"CWE-89", "shop.example.com", "/login", "v1"},
		{"cwe-89", "Shop.Example.COM", "/login", "v1"},          // 大小写归一
		{"89", "www.shop.example.com", "/login", "v1"},          // 裸编号 + www 归一
		{"CWE-89", "https://shop.example.com/", "/login", "v1"}, // 协议/尾部斜杠归一
		{"CWE-79", "shop.example.com", "/login", "v2"},
		{"CWE-639", "shop.example.com", "/api/order", "v3"},
		{"CWE-22", "shop.example.com", "/login", ""}, // 未预埋
	}
	for _, c := range cases {
		s := target.MatchSeed(c.cwe, c.asset, c.endpoint)
		if c.wantID == "" {
			if s != nil {
				t.Errorf("MatchSeed(%s,%s,%s) = %s, want nil", c.cwe, c.asset, c.endpoint, s.ID)
			}
			continue
		}
		if s == nil || s.ID != c.wantID {
			t.Errorf("MatchSeed(%s,%s,%s) = %+v, want %s", c.cwe, c.asset, c.endpoint, s, c.wantID)
		}
	}
}

// TestJudgerAcceptedDuplicateFalsePositive 验收:回执纪律(02 §1.3)。
func TestJudgerAcceptedDuplicateFalsePositive(t *testing.T) {
	j := NewJudger(DefaultSRCTarget())
	v := func(cweID, asset, endpoint string) blackboard.Vulnerability {
		return blackboard.Vulnerability{CWE: cweID, Asset: asset, Endpoint: endpoint, Status: blackboard.LedgerSubmitted}
	}
	// 首次命中 → accepted
	st, ref := j.Judge(v("CWE-89", "shop.example.com", "/login"))
	if st != blackboard.LedgerAccepted || ref != "mock-src" {
		t.Fatalf("first hit = %s/%s, want accepted/mock-src", st, ref)
	}
	// 重复命中同一种子 → duplicate(幂等拦截语义)
	st, _ = j.Judge(v("CWE-89", "shop.example.com", "/login"))
	if st != blackboard.LedgerDuplicate {
		t.Fatalf("repeat hit = %s, want duplicate", st)
	}
	// 未预埋 → false_positive(fpr 计入)
	st, _ = j.Judge(v("CWE-22", "shop.example.com", "/download"))
	if st != blackboard.LedgerFalsePositive {
		t.Fatalf("miss = %s, want false_positive", st)
	}
	// 不同种子 → accepted
	st, _ = j.Judge(v("CWE-79", "shop.example.com", "/login"))
	if st != blackboard.LedgerAccepted {
		t.Fatalf("second seed = %s, want accepted", st)
	}
}

// TestFoundMissedSeeds 验收:found/missed 划分。
func TestFoundMissedSeeds(t *testing.T) {
	target := DefaultSRCTarget()
	j := NewJudger(target)
	vulns := []blackboard.Vulnerability{
		{CWE: "CWE-89", Asset: "shop.example.com", Endpoint: "/login", Status: blackboard.LedgerAccepted},
		{CWE: "CWE-89", Asset: "shop.example.com", Endpoint: "/login", Status: blackboard.LedgerDuplicate}, // 重复不算新发现
		{CWE: "CWE-639", Asset: "shop.example.com", Endpoint: "/api/order", Status: blackboard.LedgerAccepted},
		{CWE: "CWE-22", Asset: "shop.example.com", Endpoint: "/x", Status: blackboard.LedgerFalsePositive},
	}
	found := j.FoundSeeds(vulns)
	if len(found) != 2 || found[0] != "v1" || found[1] != "v3" {
		t.Fatalf("found = %v, want [v1 v3]", found)
	}
	missed := MissedSeeds(target, found)
	if len(missed) != 1 || missed[0] != "v2" {
		t.Fatalf("missed = %v, want [v2]", missed)
	}
}

// TestEvaluateRecallFPRCoverage 验收:指标计算(02 §3.1)。
func TestEvaluateRecallFPRCoverage(t *testing.T) {
	target := DefaultSRCTarget()
	vulns := []blackboard.Vulnerability{
		{CWE: "CWE-89", Asset: "shop.example.com", Endpoint: "/login", Status: blackboard.LedgerAccepted},
		{CWE: "CWE-79", Asset: "shop.example.com", Endpoint: "/login", Status: blackboard.LedgerAccepted},
		{CWE: "CWE-22", Asset: "shop.example.com", Endpoint: "/download", Status: blackboard.LedgerFalsePositive},
		{CWE: "CWE-22", Asset: "shop.example.com", Endpoint: "/download", Status: blackboard.LedgerFalsePositive},
	}
	cells := []coverage.Cell{
		{Status: coverage.StatusConfirmed}, {Status: coverage.StatusDead}, {Status: coverage.StatusSkipped},
		{Status: coverage.StatusOpen},
	}
	m := Evaluate(target, vulns, cells, 2.0, 10, true)
	if m.Recall != 2.0/3.0 {
		t.Errorf("recall = %v, want 2/3", m.Recall)
	}
	if m.FPR != 0.5 { // 2 误报 / 4 提交
		t.Errorf("fpr = %v, want 0.5", m.FPR)
	}
	if m.Coverage != 0.75 {
		t.Errorf("coverage = %v, want 0.75", m.Coverage)
	}
	if m.Efficiency != 1.0 { // 2 USD / 2 召回
		t.Errorf("efficiency = %v, want 1.0", m.Efficiency)
	}
	if !m.Converged || m.TotalSubmissions != 4 || m.Accepted != 2 || m.FalsePositive != 2 {
		t.Errorf("misc = %+v", m)
	}
	recallOK, fprOK := m.Pass(DefaultThresholds())
	if !recallOK || fprOK {
		t.Errorf("pass = %v/%v, want true/false (fpr 0.5 > 0.2)", recallOK, fprOK)
	}
}

// TestEvaluateMissedAndThresholds 验收:recall 不足时判定失败 + 空账本不除零。
func TestEvaluateMissedAndThresholds(t *testing.T) {
	target := DefaultSRCTarget()
	m := Evaluate(target, nil, nil, 0, 0, false)
	if m.Recall != 0 || m.FPR != 0 || m.Coverage != 0 {
		t.Fatalf("empty eval = %+v", m)
	}
	if len(m.Missed) != 3 {
		t.Fatalf("missed = %v, want all 3 seeds", m.Missed)
	}
	recallOK, fprOK := m.Pass(DefaultThresholds())
	if recallOK || !fprOK {
		t.Errorf("pass = %v/%v, want false/true", recallOK, fprOK)
	}
}
