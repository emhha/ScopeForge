package breach

import (
	"context"
	"testing"

	"scopeforge/internal/testutil"
)

// TestTransitionsGenerated 验收:确认状态后生成受限转移候选(非全组合)。
func TestTransitionsGenerated(t *testing.T) {
	db := testutil.NewTestDB(t)
	s := New(db)
	// 确认 web 入口 → 生成 {注入RCE, 上传RCE, SSRF内网}
	created, err := s.ConfirmNode("c1", "web@shop", KindWeb, "shop.example.com", "")
	if err != nil || !created {
		t.Fatalf("confirm node: %v %v", created, err)
	}
	if err := s.AddTransitions("c1", "web@shop"); err != nil {
		t.Fatal(err)
	}
	edges, _ := s.Edges("c1")
	if len(edges) != 3 {
		t.Fatalf("edges = %d, want 3 (受限候选,非全组合)", len(edges))
	}
	want := map[string]bool{ActionInjectRCE: true, ActionUploadRCE: true, ActionSSRFInner: true}
	for _, e := range edges {
		if !want[e.Action] {
			t.Errorf("unexpected edge %s", e.Action)
		}
		delete(want, e.Action)
	}
	// 幂等:重复生成不动
	if err := s.AddTransitions("c1", "web@shop"); err != nil {
		t.Fatal(err)
	}
	edges, _ = s.Edges("c1")
	if len(edges) != 3 {
		t.Fatalf("edges after re-add = %d, want 3", len(edges))
	}
	// 未确认节点不生成
	s2 := New(db)
	if err := s2.AddTransitions("c2", "ghost@x"); err != nil {
		t.Fatal(err)
	}
	edges2, _ := s2.Edges("c2")
	if len(edges2) != 0 {
		t.Errorf("unconfirmed node must not generate edges")
	}
}

// TestSpaceClosure 验收:所有边终态 → space_closed(02 §2.3)。
func TestSpaceClosure(t *testing.T) {
	db := testutil.NewTestDB(t)
	s := New(db)
	ctx := context.Background()
	_, _ = s.ConfirmNode("c1", "shell@a", KindShell, "host-a", "")
	_ = s.AddTransitions("c1", "shell@a")
	// 有 open 边 → 不闭合
	ok, why := s.IsSpaceClosed(ctx, "c1")
	if ok {
		t.Fatalf("open edges must not close: %s", why)
	}
	// 全部 dead → 闭合
	edges, _ := s.Edges("c1")
	for _, e := range edges {
		_ = s.MarkEdgeDead(e.ID)
	}
	ok, why = s.IsSpaceClosed(ctx, "c1")
	if !ok || why != "space_closed" {
		t.Fatalf("all dead: ok=%v why=%s", ok, why)
	}
	// 空状态空间 → 不闭合(还没展开)
	s2 := New(db)
	ok, why = s2.IsSpaceClosed(ctx, "c2")
	if ok {
		t.Fatalf("empty space must not close: %s", why)
	}
}

// TestGoalReached 验收:goal 断言独立验证器确认(节点 confirmed)才终止。
func TestGoalReached(t *testing.T) {
	db := testutil.NewTestDB(t)
	s := New(db)
	ctx := context.Background()
	s.SetGoal("shell@host-a")
	// 未确认 → 不达成
	ok, _ := s.IsGoalReached(ctx, "c1")
	if ok {
		t.Fatal("unconfirmed goal must not be reached")
	}
	// 独立验证器确认 → 达成
	if _, err := s.ConfirmNode("c1", "shell@host-a", KindShell, "host-a", ""); err != nil {
		t.Fatal(err)
	}
	ok, goalID := s.IsGoalReached(ctx, "c1")
	if !ok || goalID != "shell@host-a" {
		t.Fatalf("goal reached: ok=%v id=%s", ok, goalID)
	}
}

// TestEdgeLifecycle 边认领/达成/失败状态机。
func TestEdgeLifecycle(t *testing.T) {
	db := testutil.NewTestDB(t)
	s := New(db)
	_, _ = s.ConfirmNode("c1", "web@shop", KindWeb, "shop.example.com", "")
	_ = s.AddTransitions("c1", "web@shop")
	edge, _ := s.EdgeByFromAction("c1", "web@shop", ActionInjectRCE)
	if edge == nil {
		t.Fatal("edge not found")
	}
	if err := s.ClaimEdge(edge.ID); err != nil {
		t.Fatal(err)
	}
	// 达成 → confirmed + to_node
	if err := s.ConfirmEdge("c1", edge.ID, "shell@shop"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.EdgeByFromAction("c1", "web@shop", ActionInjectRCE)
	if got.Status != EdgeConfirmed || got.To != "shell@shop" {
		t.Fatalf("edge = %+v", got)
	}
}

// TestDistToGoal 验收(03 §3.2 距 goal 启发式):BFS 无权图距离——
// web→shell→dc 链路,dist(web)=2、dist(shell)=1;goal 未配置/不可达 → maxDist。
func TestDistToGoal(t *testing.T) {
	db := testutil.NewTestDB(t)
	s := New(db)
	s.SetGoal("state@dc")

	// web →(注入RCE)→ shell(确认)→(横向)→ dc(确认)
	if _, err := s.ConfirmNode("t1", "state@web", "web", "web-host", ""); err != nil {
		t.Fatal(err)
	}
	_ = s.AddTransitions("t1", "state@web")
	webEdges, _ := s.Edges("t1")
	var rce *Edge
	for i := range webEdges {
		if webEdges[i].Action == ActionInjectRCE {
			rce = &webEdges[i]
		}
	}
	if rce == nil {
		t.Fatal("inject_rce edge missing")
	}
	if err := s.ConfirmEdge("t1", rce.ID, "state@shell"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConfirmNode("t1", "state@shell", "shell", "web-host", "root"); err != nil {
		t.Fatal(err)
	}
	_ = s.AddTransitions("t1", "state@shell")
	shellEdges, _ := s.Edges("t1")
	var lateral *Edge
	for i := range shellEdges {
		if shellEdges[i].From == "state@shell" && shellEdges[i].Action == ActionLateral {
			lateral = &shellEdges[i]
		}
	}
	if lateral == nil {
		t.Fatal("lateral edge missing")
	}
	if err := s.ConfirmEdge("t1", lateral.ID, "state@dc"); err != nil {
		t.Fatal(err)
	}

	if d := s.DistToGoal("t1", "state@web"); d != 2 {
		t.Errorf("dist(web) = %d, want 2", d)
	}
	if d := s.DistToGoal("t1", "state@shell"); d != 1 {
		t.Errorf("dist(shell) = %d, want 1", d)
	}
	if d := s.DistToGoal("t1", "state@dc"); d != 0 {
		t.Errorf("dist(goal) = %d, want 0", d)
	}
	// 孤立节点不可达
	if _, err := s.ConfirmNode("t1", "state@isolated", "web", "other-host", ""); err != nil {
		t.Fatal(err)
	}
	if d := s.DistToGoal("t1", "state@isolated"); d != maxDist {
		t.Errorf("dist(isolated) = %d, want maxDist", d)
	}
	// goal 未配置 → maxDist
	s2 := New(db)
	if d := s2.DistToGoal("t1", "state@web"); d != maxDist {
		t.Errorf("no-goal dist = %d, want maxDist", d)
	}
}

// ------------------------------------------------------------------ 2.22 能力维度(privilege)

// TestConfirmNodePrivilegeMerge 验收:重复确认时 privilege 能力累加合并
// (不覆盖不重复),B2 能力维度落地基础。
func TestConfirmNodePrivilegeMerge(t *testing.T) {
	db := testutil.NewTestDB(t)
	s := New(db)
	if _, err := s.ConfirmNode("t1", "state@web", KindWeb, "shop.example.com", "shell"); err != nil {
		t.Fatal(err)
	}
	// 再次确认(如 auth_gained):privilege 累加 authenticated
	if _, err := s.ConfirmNode("t1", "state@web", KindWeb, "shop.example.com", PrivilegeAuthenticated); err != nil {
		t.Fatal(err)
	}
	n, _ := s.NodeByID("state@web")
	if n.Privilege != "shell,authenticated" {
		t.Errorf("privilege = %q, want shell,authenticated(能力累加,不覆盖)", n.Privilege)
	}
	// 重复声明同一能力不重复
	if _, err := s.ConfirmNode("t1", "state@web", KindWeb, "shop.example.com", PrivilegeAuthenticated); err != nil {
		t.Fatal(err)
	}
	n, _ = s.NodeByID("state@web")
	if n.Privilege != "shell,authenticated" {
		t.Errorf("privilege after repeat = %q, want 不变(去重)", n.Privilege)
	}
}

// TestCredentialTransitions 验收:凭据节点生成受限转移候选{登录, 凭据复用}。
func TestCredentialTransitions(t *testing.T) {
	db := testutil.NewTestDB(t)
	s := New(db)
	if _, err := s.ConfirmNode("t1", "cred@shop", KindCredential, "shop.example.com", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.AddTransitions("t1", "cred@shop"); err != nil {
		t.Fatal(err)
	}
	edges, _ := s.Edges("t1")
	if len(edges) != 2 {
		t.Fatalf("edges = %d, want 2 {登录, 凭据复用}", len(edges))
	}
	want := map[string]bool{ActionLogin: true, ActionCredReuse: true}
	for _, e := range edges {
		if !want[e.Action] {
			t.Errorf("unexpected edge %s", e.Action)
		}
		delete(want, e.Action)
	}
}

// TestPrivilegeUnlocksAuthActions 验收:privilege=authenticated 解锁登录后
// 攻击面动作(kind 基线 ∪ 能力动作,去重);能力升级后重复 AddTransitions
// 幂等补齐新边。
func TestPrivilegeUnlocksAuthActions(t *testing.T) {
	db := testutil.NewTestDB(t)
	s := New(db)
	// 无能力 web 节点:仅基线 3 条
	if _, err := s.ConfirmNode("t1", "state@web", KindWeb, "shop.example.com", ""); err != nil {
		t.Fatal(err)
	}
	_ = s.AddTransitions("t1", "state@web")
	edges, _ := s.Edges("t1")
	if len(edges) != 3 {
		t.Fatalf("baseline edges = %d, want 3", len(edges))
	}
	// 能力升级(auth_gained → authenticated):登录后动作补边
	if _, err := s.ConfirmNode("t1", "state@web", KindWeb, "shop.example.com", PrivilegeAuthenticated); err != nil {
		t.Fatal(err)
	}
	_ = s.AddTransitions("t1", "state@web")
	edges, _ = s.Edges("t1")
	if len(edges) != 7 {
		t.Fatalf("after auth edges = %d, want 7 (3 基线 + 4 登录后)", len(edges))
	}
	got := map[string]bool{}
	for _, e := range edges {
		got[e.Action] = true
	}
	for _, want := range []string{ActionAuthData, ActionAuthAdmin, ActionAuthPrivEsc, ActionCredGrab} {
		if !got[want] {
			t.Errorf("auth action %q missing", want)
		}
	}
	// shell + authenticated:凭据收集去重(基线含,能力也含 → 不重复)
	if _, err := s.ConfirmNode("t1", "state@shell", KindShell, "shop.example.com", PrivilegeAuthenticated); err != nil {
		t.Fatal(err)
	}
	_ = s.AddTransitions("t1", "state@shell")
	shellEdges, _ := s.Edges("t1")
	n := 0
	for _, e := range shellEdges {
		if e.From == "state@shell" {
			n++
		}
	}
	if n != 6 {
		t.Errorf("shell+auth edges = %d, want 6 (3 基线 + 4 能力 - 1 重复凭据收集)", n)
	}
	// 升级后幂等:重复 AddTransitions 不产生新边
	before, _ := s.Edges("t1")
	_ = s.AddTransitions("t1", "state@web")
	after, _ := s.Edges("t1")
	if len(after) != len(before) {
		t.Errorf("re-add changed edge count: %d → %d", len(before), len(after))
	}
}

// TestAgentEdge 验收:agent 补充方向落地自由边(第 3 层)——锚定来源节点、
// 幂等、未知来源不建、空方向不建。
func TestAgentEdge(t *testing.T) {
	db := testutil.NewTestDB(t)
	s := New(db)
	if _, err := s.ConfirmNode("t1", "state@web", KindWeb, "shop.example.com", ""); err != nil {
		t.Fatal(err)
	}
	// 建自由边
	if err := s.AddAgentEdge("t1", "state@web", "构造 admin JWT 提权"); err != nil {
		t.Fatal(err)
	}
	edges, _ := s.Edges("t1")
	if len(edges) != 1 || edges[0].Action != "构造 admin JWT 提权" || edges[0].Status != EdgeOpen {
		t.Fatalf("agent edge not created: %+v", edges)
	}
	// 幂等:重复建同方向不重复
	if err := s.AddAgentEdge("t1", "state@web", "构造 admin JWT 提权"); err != nil {
		t.Fatal(err)
	}
	edges, _ = s.Edges("t1")
	if len(edges) != 1 {
		t.Errorf("duplicate agent edge created: %d", len(edges))
	}
	// 未知来源节点不建(防脏数据)
	if err := s.AddAgentEdge("t1", "state@ghost", "任意方向"); err != nil {
		t.Fatal(err)
	}
	edges, _ = s.Edges("t1")
	if len(edges) != 1 {
		t.Errorf("edge anchored to unknown node created: %d", len(edges))
	}
	// 空方向不建
	if err := s.AddAgentEdge("t1", "state@web", ""); err != nil {
		t.Fatal(err)
	}
	edges, _ = s.Edges("t1")
	if len(edges) != 1 {
		t.Errorf("empty-action edge created: %d", len(edges))
	}
}

// ------------------------------------------------------------------ 2.23:privilege 通用能力声明

// TestPrivilegeShellDomainUserUnlocks 验收:privilege 能力解锁转移候选
// (2.23 通用声明通道)——web 节点声明 shell/domain_user 后,候选 = kind
// 基线 ∪ 能力动作;kind 存量分支兼容,重复动作去重。
func TestPrivilegeShellDomainUserUnlocks(t *testing.T) {
	db := testutil.NewTestDB(t)
	s := New(db)
	// web 入口 + privilege="shell,domain_user"(agent 契约声明)
	if _, err := s.ConfirmNode("t1", "state@web", KindWeb, "web-host", "shell,domain_user"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddTransitions("t1", "state@web"); err != nil {
		t.Fatal(err)
	}
	edges, _ := s.Edges("t1")
	got := map[string]bool{}
	for _, e := range edges {
		got[e.Action] = true
	}
	// web 基线
	for _, want := range []string{ActionInjectRCE, ActionUploadRCE, ActionSSRFInner} {
		if !got[want] {
			t.Errorf("web baseline %q missing", want)
		}
	}
	// shell 能力
	for _, want := range []string{ActionLateral, ActionPrivEsc, ActionCredGrab} {
		if !got[want] {
			t.Errorf("shell capability %q missing", want)
		}
	}
	// domain_user 能力
	for _, want := range []string{ActionDomainEnum, ActionDelegate, ActionSpray} {
		if !got[want] {
			t.Errorf("domain_user capability %q missing", want)
		}
	}
	if len(edges) != 9 {
		t.Errorf("edges = %d, want 9 (3 基线 + 3 shell + 3 域用户,无重复)", len(edges))
	}
	// kind 存量兼容:kind=shell 且 privilege=shell → 不重复生成
	if _, err := s.ConfirmNode("t1", "state@shell", KindShell, "web-host", PrivilegeShell); err != nil {
		t.Fatal(err)
	}
	if err := s.AddTransitions("t1", "state@shell"); err != nil {
		t.Fatal(err)
	}
	shellEdges, _ := s.Edges("t1")
	n := 0
	for _, e := range shellEdges {
		if e.From == "state@shell" {
			n++
		}
	}
	if n != 3 {
		t.Errorf("kind=shell + privilege=shell edges = %d, want 3(去重,不重复生成)", n)
	}
}
