package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const nativeSessionArchiveFile = "native-sessions.sqlite"

// Archive the native SQLite tables as SQLite, without translating session events,
// reconstructing model history, or replacing the SDK's session implementation.
var nativeSessionTables = []string{"session_states", "session_events", "session_track_events", "session_summaries"}

func (m *Manager) exportNativeSessions(ctx context.Context, destination string, explorationID int64) (int64, error) {
	// A recovered staging job must create a fresh snapshot rather than reuse a
	// partial previous export. This destination belongs to the private staging dir.
	if err := os.Remove(destination); err != nil && !os.IsNotExist(err) {
		return 0, err
	}
	source, err := sql.Open("sqlite3", filepath.Join(m.dir, "sessions.sqlite")+"?_busy_timeout=10000")
	if err != nil {
		return 0, err
	}
	defer source.Close()
	if _, err = source.ExecContext(ctx, "VACUUM INTO ?", destination); err != nil {
		return 0, err
	}
	if err = os.Chmod(destination, archiveFileMode); err != nil {
		return 0, err
	}
	archive, err := sql.Open("sqlite3", destination+"?_busy_timeout=10000&_secure_delete=on")
	if err != nil {
		return 0, err
	}
	defer archive.Close()
	prefix := fmt.Sprintf("exp%d-", explorationID)
	tx, err := archive.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	for _, table := range nativeSessionTables {
		if _, err = tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE NOT (app_name=? AND user_id=? AND substr(session_id,1,length(?))=?)", "scopeforge", "operator", prefix, prefix); err != nil {
			return 0, err
		}
	}
	// App/user state is installation-wide and must never be exported with one task.
	for _, table := range []string{"app_states", "user_states"} {
		if _, err = tx.ExecContext(ctx, "DELETE FROM "+table); err != nil {
			return 0, err
		}
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	// Purge free pages as well: another task's deleted text must not remain in the package.
	if _, err = archive.ExecContext(ctx, "VACUUM"); err != nil {
		return 0, err
	}
	var count int64
	err = archive.QueryRowContext(ctx, "SELECT count(*) FROM session_states").Scan(&count)
	return count, err
}

func sessionColumns(ctx context.Context, conn *sql.Conn, schema, table string) ([]string, error) {
	rows, err := conn.QueryContext(ctx, "PRAGMA "+schema+".table_info("+table+")")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var id, notnull, pk int
		var name, typ string
		var value any
		if err = rows.Scan(&id, &name, &typ, &notnull, &value, &pk); err != nil {
			return nil, err
		}
		if name != "id" {
			columns = append(columns, `"`+strings.ReplaceAll(name, `"`, `""`)+`"`)
		}
	}
	if len(columns) == 0 {
		return nil, fmt.Errorf("native session table missing: %s", table)
	}
	return columns, rows.Err()
}

func (m *Manager) restoreNativeSessions(ctx context.Context, source string, explorationID int64, expected int64) (func(), error) {
	noop := func() {}
	if _, err := os.Stat(source); err != nil {
		return noop, err
	}
	store, err := sql.Open("sqlite3", filepath.Join(m.dir, "sessions.sqlite")+"?_busy_timeout=10000&_journal_mode=WAL&_synchronous=FULL")
	if err != nil {
		return noop, err
	}
	defer store.Close()
	conn, err := store.Conn(ctx)
	if err != nil {
		return noop, err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "ATTACH DATABASE ? AS archived", source); err != nil {
		return noop, err
	}
	defer conn.ExecContext(context.Background(), "DETACH DATABASE archived")
	prefix := fmt.Sprintf("exp%d-", explorationID)
	var invalid int
	if err = conn.QueryRowContext(ctx, "SELECT count(*) FROM archived.sqlite_master WHERE type IN ('trigger','view')").Scan(&invalid); err != nil || invalid > 0 {
		return noop, errors.New("invalid native session archive schema")
	}
	var count int64
	if err = conn.QueryRowContext(ctx, "SELECT count(*) FROM archived.session_states").Scan(&count); err != nil {
		return noop, err
	}
	if count != expected {
		return noop, errors.New("native session archive count mismatch")
	}
	columns := make(map[string]string)
	for _, table := range nativeSessionTables {
		mainCols, e := sessionColumns(ctx, conn, "main", table)
		if e != nil {
			return noop, e
		}
		archivedCols, e := sessionColumns(ctx, conn, "archived", table)
		if e != nil {
			return noop, e
		}
		if strings.Join(mainCols, ",") != strings.Join(archivedCols, ",") {
			return noop, errors.New("native session archive schema version mismatch")
		}
		columns[table] = strings.Join(mainCols, ",")
		if err = conn.QueryRowContext(ctx, "SELECT count(*) FROM archived."+table+" WHERE NOT (app_name=? AND user_id=? AND substr(session_id,1,length(?))=?)", "scopeforge", "operator", prefix, prefix).Scan(&invalid); err != nil {
			return noop, err
		}
		if invalid > 0 {
			return noop, errors.New("native session archive contains another scope")
		}
	}
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return noop, err
	}
	committed := false
	defer func() {
		if !committed {
			conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	for _, table := range nativeSessionTables {
		if _, err = conn.ExecContext(ctx, "DELETE FROM main."+table+" WHERE app_name=? AND user_id=? AND substr(session_id,1,length(?))=?", "scopeforge", "operator", prefix, prefix); err != nil {
			return noop, err
		}
		if _, err = conn.ExecContext(ctx, "INSERT INTO main."+table+" ("+columns[table]+") SELECT "+columns[table]+" FROM archived."+table); err != nil {
			return noop, err
		}
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return noop, err
	}
	committed = true
	rollback := func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		// Native deletion also removes summaries and tracks, scoped to this task.
		m.forgetSessionsMatching(cleanup, func(id string) bool { return strings.HasPrefix(id, prefix) })
	}
	return rollback, nil
}
