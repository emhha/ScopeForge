package kb

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuiltinIndexLoad(t *testing.T) {
	ix, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	b, p := ix.Count()
	if b < 5 {
		t.Errorf("builtin entries=%d want >=5", b)
	}
	if p != 0 {
		t.Errorf("plugins=%d", p)
	}
}

func TestSearchByProduct(t *testing.T) {
	ix, _ := New("")
	hits := ix.Search("tomcat", "", "")
	if !HasProduct(hits, "tomcat") {
		t.Fatalf("tomcat hits=%+v", hits)
	}
	// 版本过滤
	hits = ix.Search("tomcat", "8.5.20", "")
	if len(hits) == 0 {
		t.Error("tomcat 8.5.x should hit")
	}
	// 版本不匹配
	hits = ix.Search("tomcat", "10.1.99", "")
	for _, v := range hits {
		if len(v.Versions) > 0 {
			t.Errorf("unexpected hit for 10.1.99: %s", v.CVE)
		}
	}
}

func TestSearchKeywords(t *testing.T) {
	ix, _ := New("")
	hits := ix.Search("log4j", "", "jndi")
	if len(hits) == 0 {
		t.Fatal("log4j jndi should hit")
	}
	if hits[0].Severity != "critical" || !hits[0].ExploitAvailable {
		t.Errorf("hits[0]=%+v", hits[0])
	}
	// 空结果(弱反证语义)
	hits = ix.Search("nonexistent-product-xyz", "", "")
	if len(hits) != 0 {
		t.Error("should be empty")
	}
}

func TestFormatWeakEvidence(t *testing.T) {
	out := Format(nil)
	if !strings.Contains(out, "弱反证") {
		t.Errorf("empty format should carry weak-evidence note: %q", out)
	}
	hits := []Vuln{{CVE: "CVE-2021-44228", Severity: "critical", Summary: "x", Product: "log4j", Source: "NVD", Verified: false}}
	out = Format(hits)
	if !strings.Contains(out, "[hypothesis]") {
		t.Errorf("unverified should be flagged: %q", out)
	}
}

func TestPluginsHotReload(t *testing.T) {
	dir := t.TempDir()
	ix, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	// 写入插件
	plugin := `{"cve":"CVE-2099-0001","severity":"high","summary":"测试插件漏洞","product":"testapp","source":"local","verified":false}`
	if err := os.WriteFile(filepath.Join(dir, "test.json"), []byte(plugin), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := ix.ReloadPlugins()
	if err != nil || changed != 1 {
		t.Fatalf("changed=%d err=%v", changed, err)
	}
	hits := ix.Search("testapp", "", "")
	if len(hits) != 1 || hits[0].CVE != "CVE-2099-0001" {
		t.Fatalf("hits=%+v", hits)
	}
	// 增删不重启生效:删除插件
	os.Remove(filepath.Join(dir, "test.json"))
	changed, err = ix.ReloadPlugins()
	if err != nil || changed != 1 {
		t.Fatalf("changed=%d err=%v", changed, err)
	}
	if len(ix.Search("testapp", "", "")) != 0 {
		t.Error("plugin should be gone after delete")
	}
	// 无变化不报变更
	changed, _ = ix.ReloadPlugins()
	if changed != 0 {
		t.Errorf("no-change should report 0, got %d", changed)
	}
}

func TestPluginYAMLAndArray(t *testing.T) {
	dir := t.TempDir()
	yamlDoc := "- cve: CVE-2099-0002\n  severity: medium\n  summary: yaml 条目\n  product: yamltest\n  source: local\n"
	if err := os.WriteFile(filepath.Join(dir, "a.yaml"), []byte(yamlDoc), 0o644); err != nil {
		t.Fatal(err)
	}
	jsonArr := `[{"cve":"CVE-2099-0003","severity":"low","summary":"arr","product":"arrtest","source":"local"}]`
	if err := os.WriteFile(filepath.Join(dir, "b.json"), []byte(jsonArr), 0o644); err != nil {
		t.Fatal(err)
	}
	ix, _ := New(dir)
	if len(ix.Search("yamltest", "", "")) != 1 || len(ix.Search("arrtest", "", "")) != 1 {
		t.Error("yaml/array plugins not loaded")
	}
}

func TestValidateBadEntries(t *testing.T) {
	bad := []Vuln{{CVE: "CVE-X", Severity: "urgent", Summary: "x", Product: "p", Source: "s"}}
	if err := Validate(bad); err == nil {
		t.Error("bad severity should fail")
	}
	bad = []Vuln{{CVE: "", Severity: "high", Summary: "x", Product: "p", Source: "s"}}
	if err := Validate(bad); err == nil {
		t.Error("missing cve should fail")
	}
	bad = []Vuln{{CVE: "CVE-1", Severity: "high", Summary: "x", Product: "p", Source: "s", PocURL: "ftp://bad"}}
	if err := Validate(bad); err == nil {
		t.Error("broken poc url should fail")
	}
}

func TestSearchTool(t *testing.T) {
	ix, _ := New("")
	st := &SearchTool{Index: ix}
	if st.Name() != "vulnerability_search" || !st.ReadOnly() {
		t.Error("SearchTool contract mismatch")
	}
	out, err := st.Execute(nil, []byte(`{"product":"tomcat","version":"8.5.20"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "CVE-2020-1938") && !strings.Contains(out, "CVE-2017-12615") {
		t.Errorf("out=%q", out)
	}
	if _, err := st.Execute(nil, []byte(`{}`)); err == nil {
		t.Error("missing product should error")
	}
}
