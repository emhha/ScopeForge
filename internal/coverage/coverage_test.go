package coverage

import (
	"context"
	"testing"

	"scopeforge/internal/constraint"
	"scopeforge/internal/testutil"
)

// TestMatrixLifecycle 验收:矩阵状态机 open → claimed → confirmed/dead。
func TestMatrixLifecycle(t *testing.T) {
	db := testutil.NewTestDB(t)
	m := New(db)
	if err := m.EnsureOpen("c1", "", "shop.example.com", "/login", FormParam); err != nil {
		t.Fatal(err)
	}
	if err := m.EnsureOpen("c1", "CWE-89", "shop.example.com", "/login", FormParam); err != nil {
		t.Fatal(err)
	}
	// 幂等:同键重复落地不动
	if err := m.EnsureOpen("c1", "", "shop.example.com", "/login", FormParam); err != nil {
		t.Fatal(err)
	}
	cells, _ := m.Cells("c1")
	if len(cells) != 2 {
		t.Fatalf("cells = %d, want 2", len(cells))
	}
	// claimed:端点级认领只标空 CWE 格;精确格用 MarkClaimedCell
	if err := m.MarkClaimed("c1", "", "/login"); err != nil {
		t.Fatal(err)
	}
	if err := m.MarkClaimedCell("c1", "CWE-89", "shop.example.com", "/login"); err != nil {
		t.Fatal(err)
	}
	cells, _ = m.Cells("c1")
	claimed := 0
	for _, c := range cells {
		if c.Status == StatusClaimed {
			claimed++
		}
	}
	if claimed != 2 {
		t.Errorf("claimed = %d, want 2 (empty-CWE + exact cell)", claimed)
	}
	// confirmed
	if err := m.MarkConfirmed("c1", "CWE-89", "shop.example.com", "/login"); err != nil {
		t.Fatal(err)
	}
	cells, _ = m.Cells("c1")
	confirmed := 0
	for _, c := range cells {
		if c.Status == StatusConfirmed {
			confirmed++
		}
	}
	if confirmed != 1 {
		t.Errorf("confirmed = %d, want 1", confirmed)
	}
	// dead
	if err := m.MarkDead("c1", "shop.example.com", "/logout"); err != nil {
		t.Fatal(err)
	}
	// skipped 带理由
	if err := m.MarkSkipped("c1", "shop.example.com", "/admin", "需登录态"); err != nil {
		t.Fatal(err)
	}
	// 跨 challenge 隔离
	cells2, _ := m.Cells("c2")
	if len(cells2) != 0 {
		t.Errorf("c2 cells = %d, want 0", len(cells2))
	}
}

// TestMarkConfirmedMaterializesMissingCell 验收:攻击面清单未落地时,
// 已确认漏洞仍必须把精确格子补进覆盖矩阵(不能静默 0 行)。
func TestMarkConfirmedMaterializesMissingCell(t *testing.T) {
	db := testutil.NewTestDB(t)
	m := New(db)

	if err := m.MarkConfirmed("c1", "CWE-89", "192.168.81.167", "/rest/products/search"); err != nil {
		t.Fatal(err)
	}
	cells, err := m.Cells("c1")
	if err != nil {
		t.Fatal(err)
	}
	if len(cells) != 1 {
		t.Fatalf("cells = %d, want 1(缺格时补建确认)", len(cells))
	}
	c := cells[0]
	if c.CWE != "CWE-89" || c.Asset != "192.168.81.167" || c.Endpoint != "/rest/products/search" || c.Status != StatusConfirmed {
		t.Fatalf("cell = %+v, want CWE-89@192.168.81.167/rest/products/search confirmed", c)
	}
	// 幂等:再次确认不产生第二格
	if err := m.MarkConfirmed("c1", "CWE-89", "192.168.81.167", "/rest/products/search"); err != nil {
		t.Fatal(err)
	}
	cells, _ = m.Cells("c1")
	if len(cells) != 1 {
		t.Fatalf("after second MarkConfirmed: cells = %d, want 1", len(cells))
	}
}

// TestReopenSkippedAuth 验收:获取凭据后重开"缺登录态"跳过格子(登录后攻击面闭环)。
// 只重开登录类理由的 skipped;穷尽声明排除(无登录理由)不受影响;跨 challenge 隔离。
func TestReopenSkippedAuth(t *testing.T) {
	db := testutil.NewTestDB(t)
	m := New(db)

	// 三个 skipped 格子:两个登录类理由,一个穷尽排除
	_ = m.EnsureOpen("c1", "", "shop.example.com", "/admin", FormUnknown)
	_ = m.MarkSkipped("c1", "shop.example.com", "/admin", "需登录态")
	_ = m.EnsureOpen("c1", "", "shop.example.com", "/orders", FormUnknown)
	_ = m.MarkSkipped("c1", "shop.example.com", "/orders", "login required")
	_ = m.EnsureOpen("c1", "", "shop.example.com", "/legacy", FormUnknown)
	_ = m.MarkSkipped("c1", "shop.example.com", "/legacy", "穷尽声明排除:已确认无漏洞")

	n, err := m.ReopenSkippedAuth("c1", "")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("reopened = %d, want 2(仅登录类理由)", n)
	}

	// 状态断言:登录类格子 → open 且 skip_reason 清空;穷尽排除保持 skipped
	statuses := map[string]string{}
	cells, _ := m.Cells("c1")
	for _, c := range cells {
		statuses[c.Endpoint] = c.Status
	}
	if statuses["/admin"] != StatusOpen || statuses["/orders"] != StatusOpen {
		t.Errorf("auth-skipped cells not reopened: %v", statuses)
	}
	if statuses["/legacy"] != StatusSkipped {
		t.Errorf("exhaustion-skipped cell must stay skipped: %v", statuses)
	}

	// asset 过滤 + 幂等:再次重开(无 skipped 登录格)→ 0
	if n2, _ := m.ReopenSkippedAuth("c1", "shop.example.com"); n2 != 0 {
		t.Errorf("second reopen = %d, want 0(幂等)", n2)
	}
	// 跨 challenge 隔离
	if n3, _ := m.ReopenSkippedAuth("c2", ""); n3 != 0 {
		t.Errorf("c2 reopen = %d, want 0", n3)
	}
}

// TestIsConverged 验收:覆盖度收敛判定(02 §2.3)。
// 无攻击面 → 不收敛;有 open 格 → 不收敛;全终态 → 收敛。
func TestIsConverged(t *testing.T) {
	db := testutil.NewTestDB(t)
	m := New(db)
	ctx := context.Background()

	// 攻击面都没测绘 → no_attack_surface
	ok, why := m.IsConverged(ctx, "c1", constraint.Interest{})
	if ok {
		t.Fatalf("empty matrix must not converge: %s", why)
	}
	// open 格子存在 → 不收敛
	_ = m.EnsureOpen("c1", "CWE-89", "shop.example.com", "/login", FormParam)
	ok, why = m.IsConverged(ctx, "c1", constraint.Interest{})
	if ok {
		t.Fatalf("open cell must not converge: %s", why)
	}
	// 全终态 → 收敛
	_ = m.MarkConfirmed("c1", "CWE-89", "shop.example.com", "/login")
	ok, why = m.IsConverged(ctx, "c1", constraint.Interest{})
	if !ok || why != "coverage_converged" {
		t.Fatalf("all terminal: ok=%v why=%s", ok, why)
	}
	// interest 过滤:低危格子不参与收敛(open 但 interest 不命中)
	_ = m.EnsureOpen("c1", "CWE-200", "shop.example.com", "/info", FormFile)
	hi := constraint.Interest{SeverityMin: "high"}
	ok, _ = m.IsConverged(ctx, "c1", hi)
	if !ok {
		t.Fatal("low-sev open cell must be filtered by interest=high")
	}
	ok, _ = m.IsConverged(ctx, "c1", constraint.Interest{})
	if ok {
		t.Fatal("without interest filter the open low cell must not converge")
	}
}

// TestCandidateCWEs 验收:接力候选受限子集,非全 CWE(03 §3.4 反笛卡尔积)。
func TestCandidateCWEs(t *testing.T) {
	if got := CandidateCWEs(FormParam); len(got) != 3 || got[0] != "CWE-89" {
		t.Errorf("param candidates = %v", got)
	}
	if got := CandidateCWEs(FormUpload); len(got) != 2 || got[0] != "CWE-434" {
		t.Errorf("upload candidates = %v", got)
	}
	if got := CandidateCWEs(FormStatic); got != nil {
		t.Errorf("static must have no relay candidates: %v", got)
	}
	if got := CandidateCWEs(FormUnknown); got != nil {
		t.Errorf("unknown form must not guess: %v", got)
	}
}

// TestGenerateRelay 验收:accepted 后接力生成——
// 同端点其他 CWE 进矩阵 open 格(不直接派活);同 CWE 同形态其他端点展开;
// 已 confirmed 的 CWE 不重复生成(幂等)。
func TestGenerateRelay(t *testing.T) {
	db := testutil.NewTestDB(t)
	m := New(db)
	// 攻击面:/login(带参) + /api/search(带参) + /static/x.js(静态)
	_ = m.EnsureOpen("c1", "", "shop.example.com", "/login", FormParam)
	_ = m.EnsureOpen("c1", "", "shop.example.com", "/api/search", FormParam)
	_ = m.EnsureOpen("c1", "", "shop.example.com", "/static/x.js", FormStatic)
	// CWE-89@/login confirmed → 接力
	_ = m.MarkConfirmed("c1", "CWE-89", "shop.example.com", "/login")
	if err := m.GenerateRelay("c1", "CWE-89", "shop.example.com", "/login"); err != nil {
		t.Fatal(err)
	}
	cells, _ := m.Cells("c1")
	// 断言:/login 出现 CWE-79/CWE-639 接力格;/api/search 出现 CWE-89 同型格;
	// /static/x.js 无注入类接力。
	byKey := map[string]string{}
	for _, c := range cells {
		byKey[c.CWE+"|"+c.Endpoint] = c.Status
	}
	if byKey["CWE-79|/login"] != StatusOpen {
		t.Errorf("/login CWE-79 relay missing: %v", byKey)
	}
	if byKey["CWE-639|/login"] != StatusOpen {
		t.Errorf("/login CWE-639 relay missing: %v", byKey)
	}
	if byKey["CWE-89|/api/search"] != StatusOpen {
		t.Errorf("/api/search same-cwe relay missing: %v", byKey)
	}
	for k := range byKey {
		if k == "CWE-89|/static/x.js" || k == "CWE-79|/static/x.js" {
			t.Errorf("static endpoint must not get injection relay: %s", k)
		}
	}
	// 幂等:重复接力不产生新格
	before := len(cells)
	_ = m.GenerateRelay("c1", "CWE-89", "shop.example.com", "/login")
	after, _ := m.Cells("c1")
	if len(after) != before {
		t.Errorf("relay not idempotent: %d → %d", before, len(after))
	}
	// 反例:同端点已 confirmed 的 CWE 不重复生成(CWE-89 不会在 /login 再开格)
	for _, c := range after {
		if c.CWE == "CWE-89" && c.Endpoint == "/login" && c.Status != StatusConfirmed {
			t.Errorf("confirmed cwe duplicated as %s", c.Status)
		}
	}
}

// TestClassifyForm 端点形态分类。
func TestClassifyForm(t *testing.T) {
	cases := []struct {
		endpoint string
		want     string
	}{
		{"/login", FormAuth},
		{"/api/auth/signin", FormAuth},
		{"/upload", FormUpload},
		{"/download", FormFile},
		{"/static/x.js", FormFile},
		{"/api/order", FormUnknown},
	}
	for _, c := range cases {
		if got := ClassifyForm(c.endpoint); got != c.want {
			t.Errorf("ClassifyForm(%q) = %q, want %q", c.endpoint, got, c.want)
		}
	}
}

// TestIsConvergedEmptyCWECellNotFiltered 回归:空 CWE 攻击面格永远参与收敛判定
// (CWE 集合过滤不得跳过端点级格子,否则零探索提前终止)。
func TestIsConvergedEmptyCWECellNotFiltered(t *testing.T) {
	db := testutil.NewTestDB(t)
	m := New(db)
	ctx := context.Background()
	// 攻击面格(空 CWE)open + 一个 CWE 候选格 confirmed
	_ = m.EnsureOpen("c1", "", "shop.example.com", "/login", FormParam)
	_ = m.EnsureOpen("c1", "CWE-89", "shop.example.com", "/login", FormParam)
	_ = m.MarkConfirmed("c1", "CWE-89", "shop.example.com", "/login")
	// interest 只关心 CWE-79:空 CWE 格仍参与 → 不收敛
	interest := constraint.Interest{CWEs: []string{"CWE-79"}}
	ok, _ := m.IsConverged(ctx, "c1", interest)
	if ok {
		t.Fatal("empty-CWE open cell must keep matrix unconverged even under cwe-set interest")
	}
	// 空 CWE 格终态后收敛(CWE-89 格被兴趣过滤)
	_ = m.MarkClaimed("c1", "", "/login")
	ok, why := m.IsConverged(ctx, "c1", interest)
	if !ok {
		t.Fatalf("all relevant cells terminal: %s", why)
	}
}
