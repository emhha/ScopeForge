package testutil

import (
	"path/filepath"
	"testing"

	"scopeforge/internal/store"
)

// NewTestDB 为测试创建内存数据库并完成迁移。
func NewTestDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.OpenInMemory()
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// TempDBPath 返回一个临时文件数据库路径(崩溃恢复测试用)。
func TempDBPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "scopeforge.db")
}
