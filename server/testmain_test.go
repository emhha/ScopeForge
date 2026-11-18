package server

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "artex-server-sqlite-")
	if err != nil {
		panic(err)
	}
	// Each Manager uses its own t.TempDir data directory. Do not share profiles,
	// settings or queues across unrelated HTTP fixtures.
	os.Unsetenv("ARTEX_DB_PATH")
	os.Unsetenv("ARTEX_PG_DSN")
	os.Setenv("ARTEX_CONFIG", filepath.Join(dir, "missing-config.json"))
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
