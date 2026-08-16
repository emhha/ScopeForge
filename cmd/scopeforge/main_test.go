package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"scopeforge/internal/config"
)

// 回归:单二进制部署(工作目录无 configs/)时,首次启动生成的
// scopeforge.yaml 必须含 provider 实例且能通过 config.Load,
// 否则 serve fail-fast 报"配置缺少 providers 段"。
func TestEnsureConfigGeneratesProvider(t *testing.T) {
	dir := t.TempDir()
	if err := ensureConfig(dir); err != nil {
		t.Fatalf("ensureConfig: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "scopeforge.yaml"))
	if err != nil {
		t.Fatalf("read generated config: %v", err)
	}
	if strings.Contains(string(data), "providers: []") {
		t.Errorf("generated config has empty providers list:\n%s", data)
	}
	cfg, err := config.Load(filepath.Join(dir, "scopeforge.yaml"))
	if err != nil {
		t.Fatalf("generated config fails Load: %v", err)
	}
	if len(cfg.Providers) == 0 {
		t.Errorf("generated config has no providers:\n%s", data)
	}
}

// 初始化时必须创建 <dataDir>/.env(默认 dataDir = ~/.scopeforge),
// 内容为密钥模板且权限 0600。
func TestEnsureConfigGeneratesEnv(t *testing.T) {
	dir := t.TempDir()
	if err := ensureConfig(dir); err != nil {
		t.Fatalf("ensureConfig: %v", err)
	}
	path := filepath.Join(dir, ".env")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read generated .env: %v", err)
	}
	if !strings.Contains(string(data), "DEEPSEEK_API_KEY=") || !strings.Contains(string(data), "SCOPEFORGE_TOKEN=") {
		t.Errorf("generated .env missing key template:\n%s", data)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf(".env mode = %o, want 600", perm)
	}
}

// 已存在的 .env 不被覆盖(用户填过的真实密钥保留)。
func TestEnsureEnvFileKeepsExisting(t *testing.T) {
	dir := t.TempDir()
	existing := "DEEPSEEK_API_KEY=sk-real-key\n"
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ensureConfig(dir); err != nil {
		t.Fatalf("ensureConfig: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != existing {
		t.Errorf("existing .env was overwritten: %q", data)
	}
}

// 已存在的配置不被覆盖(用户手改的 providers 保留)。
func TestEnsureConfigKeepsExisting(t *testing.T) {
	dir := t.TempDir()
	existing := "providers: []\n"
	if err := os.WriteFile(filepath.Join(dir, "scopeforge.yaml"), []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ensureConfig(dir); err != nil {
		t.Fatalf("ensureConfig: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "scopeforge.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != existing {
		t.Errorf("existing config was overwritten: %q", data)
	}
}
