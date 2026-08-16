package store

import (
	"path/filepath"
	"testing"
)

// tempDBPath 返回临时文件数据库路径。
func tempDBPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "scopeforge.db")
}

func TestMigrateAndReopen(t *testing.T) {
	path := tempDBPath(t)
	db, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// 验证核心表存在
	for _, table := range []string{"sessions", "events", "ledger", "subagent_transcripts", "vulnerability_ledger"} {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n); err != nil {
			t.Fatalf("check %s: %v", table, err)
		}
		if n != 1 {
			t.Fatalf("table %s missing", table)
		}
	}

	// 幂等:重复迁移不报错、不重复应用
	if err := db.Migrate(); err != nil {
		t.Fatalf("re-migrate: %v", err)
	}
	var versions int
	if err := db.QueryRow(`SELECT count(*) FROM schema_migrations`).Scan(&versions); err != nil {
		t.Fatalf("versions: %v", err)
	}
	if versions != 11 {
		t.Fatalf("expected 11 applied migrations, got %d", versions)
	}

	// 关闭后重开,数据仍在(WAL 持久化)
	db.Close()
	db2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()
	if _, err := db2.Exec(`INSERT INTO events (ts, kind, payload) VALUES (1, 'test', NULL)`); err != nil {
		t.Fatalf("insert after reopen: %v", err)
	}
	var seq int64
	if err := db2.QueryRow(`SELECT max(seq) FROM events`).Scan(&seq); err != nil {
		t.Fatalf("read after reopen: %v", err)
	}
	if seq != 1 {
		t.Fatalf("expected seq 1, got %d", seq)
	}
}
