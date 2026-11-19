package server

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "scopeforge-server-sqlite-")
	if err != nil {
		panic(err)
	}
	// Each Manager uses its own t.TempDir data directory. Do not share profiles,
	// settings or queues across unrelated HTTP fixtures.
	os.Unsetenv("SCOPEFORGE_DB_PATH")
	os.Unsetenv("SCOPEFORGE_PG_DSN")
	os.Setenv("SCOPEFORGE_CONFIG", filepath.Join(dir, "missing-config.json"))
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
