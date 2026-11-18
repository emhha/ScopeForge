package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSQLitePathPrecedenceAndLegacyProtection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	t.Setenv("ARTEX_CONFIG", path)
	t.Setenv("ARTEX_DB_PATH", "")
	t.Setenv("ARTEX_PG_DSN", "")
	got, _, err := SQLitePath(filepath.Join(dir, "data"))
	if err != nil || got != filepath.Join(dir, "data", "artex.sqlite") {
		t.Fatalf("default=%s %v", got, err)
	}
	if err = os.WriteFile(path, []byte(`{"database":{"path":"local/store.sqlite","driver":"sqlite"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	got, _, err = SQLitePath("")
	if err != nil || got != filepath.Join(dir, "local", "store.sqlite") {
		t.Fatalf("configured=%s %v", got, err)
	}
	t.Setenv("ARTEX_DB_PATH", filepath.Join(dir, "override.sqlite"))
	got, _, err = SQLitePath("")
	if err != nil || got != filepath.Join(dir, "override.sqlite") {
		t.Fatalf("env=%s %v", got, err)
	}
	t.Setenv("ARTEX_DB_PATH", "")
	if err = os.WriteFile(path, []byte(`{"database":{"host":"localhost","user":"old"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err = SQLitePath(""); err == nil {
		t.Fatal("legacy PostgreSQL configuration silently created an empty SQLite database")
	}
}
