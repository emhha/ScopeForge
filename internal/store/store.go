// Package store 提供 SQLite 连接与迁移管理。
//
// 设计契约: docs/01 §3.1 — SQLite(WAL + busy_timeout),modernc.org/sqlite 纯 Go 驱动免 CGO。
// 全部持久化状态(会话、事件、账本、转录)落同一库,文件系统只存工作区产物。
package store

import (
	"database/sql"
	"embed"
	"fmt"
	"sort"
	"strconv"
	"strings"

	_ "modernc.org/sqlite"
)

//go:embed migrations
var migrationsFS embed.FS

// DB 封装 *sql.DB,附加 WAL 语义。
type DB struct {
	*sql.DB
}

// Open 打开(或创建)SQLite 数据库,启用 WAL 模式与 busy_timeout。
func Open(path string) (*DB, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)&_pragma=synchronous(NORMAL)", path)
	sqldb, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	sqldb.SetMaxOpenConns(1) // SQLite 单写者;避免并发写锁竞争
	db := &DB{DB: sqldb}
	if err := db.Ping(); err != nil {
		sqldb.Close()
		return nil, fmt.Errorf("store: ping %s: %w", path, err)
	}
	return db, nil
}

// OpenInMemory 打开内存数据库(测试用)。
func OpenInMemory() (*DB, error) {
	return Open(":memory:")
}

// Migrate 按文件名顺序执行 migrations/ 下的 *.sql,记录已应用版本。
func (db *DB) Migrate() error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY,
		applied_at INTEGER NOT NULL
	)`); err != nil {
		return fmt.Errorf("store: create schema_migrations: %w", err)
	}

	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("store: read migrations dir: %w", err)
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		version, err := strconv.Atoi(strings.TrimSuffix(name, ".sql")[:4])
		if err != nil {
			return fmt.Errorf("store: bad migration name %q: %w", name, err)
		}
		var applied int
		err = db.QueryRow(`SELECT version FROM schema_migrations WHERE version = ?`, version).Scan(&applied)
		switch {
		case err == sql.ErrNoRows:
			// 未应用,继续
		case err != nil:
			return fmt.Errorf("store: check migration %s: %w", name, err)
		default:
			continue // 已应用
		}

		sqlBytes, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("store: read migration %s: %w", name, err)
		}
		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("store: begin migration %s: %w", name, err)
		}
		if _, err := tx.Exec(string(sqlBytes)); err != nil {
			tx.Rollback()
			return fmt.Errorf("store: apply migration %s: %w", name, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
			version, nowUnix()); err != nil {
			tx.Rollback()
			return fmt.Errorf("store: record migration %s: %w", name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("store: commit migration %s: %w", name, err)
		}
	}
	return nil
}

// Close 关闭数据库。
func (db *DB) Close() error { return db.DB.Close() }
