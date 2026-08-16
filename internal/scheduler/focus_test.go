package scheduler

import (
	"testing"

	"scopeforge/internal/dispatcher"
	"scopeforge/internal/event"
)

func TestParseFocusTarget(t *testing.T) {
	ft, err := parseFocusTarget("http://192.168.81.167:3000/rest/products/search?q=")
	if err != nil {
		t.Fatal(err)
	}
	if ft.Host != "192.168.81.167:3000" || ft.HostOnly != "192.168.81.167" ||
		ft.PathPrefix != "/rest/products/search" {
		t.Fatalf("parse = %+v", ft)
	}
	// 根路径 → "/"(只聚焦主机)
	ft, err = parseFocusTarget("https://target.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if ft.PathPrefix != "/" || ft.HostOnly != "target.example.com" {
		t.Fatalf("root = %+v", ft)
	}
	// 空 → nil(聚焦关闭)
	ft, err = parseFocusTarget("   ")
	if err != nil || ft != nil {
		t.Fatalf("empty: ft=%v err=%v, want nil", ft, err)
	}
}

func TestFocusMatchEndpoint(t *testing.T) {
	ft, _ := parseFocusTarget("http://192.168.81.167:3000/rest/products/search")
	cases := []struct {
		ep   string
		want bool
	}{
		{"/rest/products/search", true},
		{"/rest/products/search?q=", true},          // query 归一化去除
		{"/rest/products/search/1/reviews", true},   // 同前缀子路径放行
		{"/api/Users", false},
		{"/rest/user/login", false},
		{"/", false},
		{"", true}, // 无端点信息保底放行
	}
	for _, c := range cases {
		if got := ft.matchEndpoint(c.ep); got != c.want {
			t.Errorf("matchEndpoint(%q) = %v, want %v", c.ep, got, c.want)
		}
	}
	// 根路径聚焦 = 只限主机不限路径
	ftRoot, _ := parseFocusTarget("http://192.168.81.167:3000")
	if !ftRoot.matchEndpoint("/api/Users") {
		t.Error("根路径聚焦应放行任意端点")
	}
}

// TestFocusTrailingSlash 回归:聚焦目标带尾部斜杠时 PathPrefix 必须归一化
// (否则 matchEndpoint 的 HasPrefix 永不命中,聚焦模式系统性误杀 findings)。
func TestFocusTrailingSlash(t *testing.T) {
	ft, err := parseFocusTarget("http://192.168.81.167:3000/rest/products/search/?q=")
	if err != nil {
		t.Fatal(err)
	}
	if ft.PathPrefix != "/rest/products/search" {
		t.Fatalf("PathPrefix = %q, want /rest/products/search(归一化去尾斜杠)", ft.PathPrefix)
	}
	if !ft.matchEndpoint("/rest/products/search?q=") {
		t.Error("带尾斜杠聚焦目标应匹配归一化端点")
	}
	if !ft.matchEndpoint("GET http://192.168.81.167:3000/rest/products/search?q=") {
		t.Error("带尾斜杠聚焦目标应匹配方法前缀+完整 URL 形态")
	}
}

func TestFocusMatchAsset(t *testing.T) {
	ft, _ := parseFocusTarget("http://192.168.81.167:3000/rest/products/search")
	cases := []struct {
		asset string
		want  bool
	}{
		{"192.168.81.167", true},
		{"192.168.81.167:3000", true},
		{"http://192.168.81.167:3000", true},
		{"shop.example.com", false},
		{"192.168.81.168", false},
		{"", true}, // 无资产信息保底放行
	}
	for _, c := range cases {
		if got := ft.matchAsset(c.asset); got != c.want {
			t.Errorf("matchAsset(%q) = %v, want %v", c.asset, got, c.want)
		}
	}
}

// TestFocusFilter 验收(阶段 2.21):越界攻击面/意图/发现确定性丢弃,
// 聚焦条目保留;未启用聚焦时原样透传。
func TestFocusFilter(t *testing.T) {
	ft, _ := parseFocusTarget("http://192.168.81.167:3000/rest/products/search")
	s := &Scheduler{sink: eventDiscardSink{}}
	contract := &dispatcher.WorkerContract{
		AttackSurface: []dispatcher.AttackSurfaceItem{
			{Asset: "192.168.81.167", Endpoint: "/rest/products/search", Params: []string{"q"}},
			{Asset: "192.168.81.167", Endpoint: "/api/Users"},     // 越界端点
			{Asset: "shop.example.com", Endpoint: "/rest/products/search"}, // 越界主机
		},
		NewIntents: []dispatcher.IntentIn{
			{Text: "测注入", Target: "/rest/products/search", Approach: "probe"},
			{Text: "越界意图", Target: "/api/Users", Approach: "IDOR"},
		},
		Findings: []dispatcher.Finding{
			{Prefix: "vuln:", Text: "聚焦漏洞", Endpoint: "/rest/products/search", Asset: "192.168.81.167"},
			{Prefix: "vuln:", Text: "越界漏洞", Endpoint: "/api/Users", Asset: "192.168.81.167"},
		},
	}
	out := s.focusFilter(contract, ft, "c1")
	if len(out.AttackSurface) != 1 || out.AttackSurface[0].Endpoint != "/rest/products/search" {
		t.Fatalf("attack_surface = %+v, want 仅聚焦条目", out.AttackSurface)
	}
	if len(out.NewIntents) != 1 || out.NewIntents[0].Target != "/rest/products/search" {
		t.Fatalf("new_intents = %+v, want 仅聚焦意图", out.NewIntents)
	}
	if len(out.Findings) != 1 || out.Findings[0].Endpoint != "/rest/products/search" {
		t.Fatalf("findings = %+v, want 仅聚焦发现", out.Findings)
	}
	// 原契约不被修改(返回副本)
	if len(contract.AttackSurface) != 3 {
		t.Fatalf("原契约被修改: %d", len(contract.AttackSurface))
	}
	// 未启用聚焦 → 原样
	out2 := s.focusFilter(contract, nil, "c1")
	if out2 != contract {
		t.Fatal("聚焦关闭时应返回原契约")
	}
}

// eventDiscardSink 测试用空 sink。
type eventDiscardSink struct{}

func (eventDiscardSink) Emit(event.Event) {}
