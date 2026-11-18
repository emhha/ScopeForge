package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "artex-agent-sqlite-")
	if err != nil {
		panic(err)
	}
	os.Setenv("ARTEX_DB_PATH", filepath.Join(dir, "test.sqlite"))
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
