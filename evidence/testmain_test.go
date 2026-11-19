package evidence

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "scopeforge-evidence-sqlite-")
	if err != nil {
		panic(err)
	}
	os.Setenv("SCOPEFORGE_DB_PATH", filepath.Join(dir, "test.sqlite"))
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
