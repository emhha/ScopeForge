package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Agent.MaxTurns != 200 || cfg.Tools.Permissions != "auto" {
		t.Errorf("defaults = %+v", cfg.Agent)
	}
	if cfg.Agent.Compact.Tail != 16384 || cfg.Agent.ContextWindow != 65536 {
		t.Errorf("compact defaults = %+v", cfg.Agent.Compact)
	}
	// 回归:Default 必须带一个 provider 实例,否则单二进制首次启动生成的
	// scopeforge.yaml 为 providers: [],serve fail-fast 拒绝启动。
	if len(cfg.Providers) != 1 || cfg.Providers[0].Name != "deepseek-main" {
		t.Errorf("default providers = %+v, want exactly deepseek-main", cfg.Providers)
	}
}

func TestLoadYAMLAndEnvExpand(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TEST_BASE_URL", "https://mock.example.com/v1")
	path := filepath.Join(dir, "scopeforge.yaml")
	content := `
providers:
  - name: deepseek-main
    kind: openai
    base_url: ${TEST_BASE_URL}
    model: deepseek-reasoner
    api_key_env: DEEPSEEK_API_KEY
agent:
  max_turns: 42
tools:
  permissions: yolo
  blacklist: ["rm -rf /"]
  domain_allowlist: ["*.example.com"]
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Providers[0].BaseURL != "https://mock.example.com/v1" {
		t.Errorf("env not expanded: %q", cfg.Providers[0].BaseURL)
	}
	if cfg.Agent.MaxTurns != 42 || cfg.Tools.Permissions != "yolo" {
		t.Errorf("overrides = %+v %+v", cfg.Agent, cfg.Tools)
	}
	if len(cfg.Tools.Blacklist) != 1 || len(cfg.Tools.DomainAllowlist) != 1 {
		t.Errorf("lists = %v %v", cfg.Tools.Blacklist, cfg.Tools.DomainAllowlist)
	}
}

func TestLoadErrors(t *testing.T) {
	dir := t.TempDir()
	// 非法 kind
	path := filepath.Join(dir, "bad1.yaml")
	os.WriteFile(path, []byte("providers:\n  - name: x\n    kind: nope\n    base_url: http://a\n    model: m\n"), 0o644)
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "kind") {
		t.Errorf("bad kind should error with location: %v", err)
	}
	// 非法权限模式
	path2 := filepath.Join(dir, "bad2.yaml")
	os.WriteFile(path2, []byte("tools:\n  permissions: crazy\n"), 0o644)
	_, err = Load(path2)
	if err == nil || !strings.Contains(err.Error(), "permissions") {
		t.Errorf("bad permissions should error: %v", err)
	}
	// 不存在文件
	if _, err := Load(filepath.Join(dir, "nope.yaml")); err == nil {
		t.Error("missing file should error")
	}
}

func TestExpandEnv(t *testing.T) {
	t.Setenv("A", "value-a")
	cases := map[string]string{
		"${A}":            "value-a",
		"x${A}y":          "xvalue-ay",
		"${MISSING}":      "",
		"${MISSING:-def}": "def",
		"${A:-def}":       "value-a",
		"no-var":          "no-var",
	}
	for in, want := range cases {
		if got := ExpandEnv(in); got != want {
			t.Errorf("ExpandEnv(%q) = %q, want %q", in, got, want)
		}
	}
}
