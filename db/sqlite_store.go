package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"modernc.org/sqlite"
)

// Tx reserves the SQLite writer at BEGIN IMMEDIATE. All ownership checks and
// writes in the transaction see the same snapshot and cannot lose a race to a
// second task, archive worker or process between SELECT and UPDATE.
type Tx struct {
	*sql.Tx
	columnTypes map[string]map[string]string
}

func sqliteArgs(args []any) []any {
	out := append([]any(nil), args...)
	for i, v := range out {
		if v == nil {
			continue
		}
		switch value := v.(type) {
		case time.Time:
			out[i] = value.UTC().Format("2006-01-02 15:04:05.000000000")
			continue
		case *time.Time:
			if value == nil {
				out[i] = nil
			} else {
				out[i] = value.UTC().Format("2006-01-02 15:04:05.000000000")
			}
			continue
		case sql.NullTime:
			if !value.Valid {
				out[i] = nil
			} else {
				out[i] = value.Time.UTC().Format("2006-01-02 15:04:05.000000000")
			}
			continue

		case json.RawMessage:
			if value == nil {
				out[i] = nil
			} else {
				out[i] = string(value)
			}
			continue
		case []byte:
			if value == nil {
				out[i] = nil
			} else {
				out[i] = string(value)
			}
			continue
		}
		t := reflect.TypeOf(v)
		if t.Kind() == reflect.Slice && t.Elem().Kind() != reflect.Uint8 {
			if reflect.ValueOf(v).IsNil() {
				out[i] = "[]"
				continue
			}
			b, err := json.Marshal(v)
			if err == nil {
				out[i] = string(b)
			}
		}
	}
	return out
}
func (d *DB) writePool() *sql.DB {
	if d.writer != nil {
		return d.writer
	}
	return d.DB
}
func (d *DB) Exec(q string, args ...any) (sql.Result, error) {
	return d.ExecContext(context.Background(), q, args...)
}
func (d *DB) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	return d.writePool().ExecContext(ctx, q, sqliteArgs(args)...)
}
func (d *DB) Query(q string, args ...any) (*Rows, error) {
	return d.QueryContext(context.Background(), q, args...)
}
func (d *DB) QueryContext(ctx context.Context, q string, args ...any) (*Rows, error) {
	return wrapRows(d.queryPool(q).QueryContext(ctx, q, sqliteArgs(args)...))
}
func (d *DB) QueryRow(q string, args ...any) *Row {
	return d.QueryRowContext(context.Background(), q, args...)
}
func (d *DB) QueryRowContext(ctx context.Context, q string, args ...any) *Row {
	return &Row{d.queryPool(q).QueryRowContext(ctx, q, sqliteArgs(args)...)}
}
func (d *DB) Begin() (*Tx, error) { return d.BeginTx(context.Background(), nil) }
func (d *DB) BeginTx(ctx context.Context, opts *sql.TxOptions) (*Tx, error) {
	pool := d.writePool()
	if opts != nil && opts.ReadOnly {
		pool = d.DB
	}
	t, err := pool.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &Tx{Tx: t}, nil
}
func (t *Tx) Exec(q string, args ...any) (sql.Result, error) {
	return t.ExecContext(context.Background(), q, args...)
}
func (t *Tx) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	return t.Tx.ExecContext(ctx, q, sqliteArgs(args)...)
}
func (t *Tx) Query(q string, args ...any) (*Rows, error) {
	return t.QueryContext(context.Background(), q, args...)
}
func (t *Tx) QueryContext(ctx context.Context, q string, args ...any) (*Rows, error) {
	return wrapRows(t.Tx.QueryContext(ctx, q, sqliteArgs(args)...))
}
func (t *Tx) QueryRow(q string, args ...any) *Row {
	return t.QueryRowContext(context.Background(), q, args...)
}
func (t *Tx) QueryRowContext(ctx context.Context, q string, args ...any) *Row {
	return &Row{t.Tx.QueryRowContext(ctx, q, sqliteArgs(args)...)}
}
func (d *DB) Close() error {
	var err error
	if d.writer != nil {
		err = d.writer.Close()
	}
	return errors.Join(err, d.DB.Close())
}
func IsUniqueViolation(err error) bool     { return sqliteCode(err) == 2067 || sqliteCode(err) == 1555 }
func IsForeignKeyViolation(err error) bool { return sqliteCode(err) == 787 }
func sqliteCode(err error) int {
	var e *sqlite.Error
	if errors.As(err, &e) {
		return e.Code()
	}
	return 0
}

func openSQLite(path string) (*DB, error) {
	if strings.HasPrefix(path, "postgres:") || strings.HasPrefix(path, "postgresql:") || strings.Contains(path, "host=") {
		return nil, fmt.Errorf("SQLite requires a file path; PostgreSQL connections are not supported")
	}
	if path == "" {
		return nil, fmt.Errorf("empty SQLite database path")
	}
	if path == ":memory:" {
		return nil, fmt.Errorf("use a temporary SQLite file for pooled storage")
	}
	var uri string
	if strings.HasPrefix(path, "file:") {
		uri = path
	} else {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(absolute), 0700); err != nil {
			return nil, err
		}
		file, err := os.OpenFile(absolute, os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			return nil, err
		}
		if err := file.Close(); err != nil {
			return nil, err
		}
		uri = sqliteFileURI(absolute)
	}
	sep := "?"
	if strings.Contains(uri, "?") {
		sep = "&"
	}
	base := uri + sep + "_pragma=busy_timeout(10000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_time_format=sqlite"
	writer, err := sql.Open("sqlite", base+"&_txlock=immediate")
	if err != nil {
		return nil, err
	}
	writer.SetMaxOpenConns(1)
	writer.SetMaxIdleConns(1)
	if err := writer.Ping(); err != nil {
		writer.Close()
		return nil, err
	}
	if err := initializeSQLite(writer); err != nil {
		writer.Close()
		return nil, fmt.Errorf("initialize SQLite schema: %w", err)
	}
	reader, err := sql.Open("sqlite", base+"&_txlock=deferred&_pragma=query_only(1)")
	if err != nil {
		writer.Close()
		return nil, err
	}
	reader.SetMaxOpenConns(8)
	reader.SetMaxIdleConns(8)
	d := &DB{DB: reader, writer: writer}
	if err := d.seedBuiltins(); err != nil {
		d.Close()
		return nil, fmt.Errorf("seed SQLite: %w", err)
	}
	return d, nil
}

const sqliteSchemaVersion = 3
const sqliteApplicationID = 0x41525458 // Stable SQLite format identifier; retained for existing databases.
func initializeSQLite(writer *sql.DB) error {
	tx, err := writer.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version, application int
	if err = tx.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if err = tx.QueryRow(`PRAGMA application_id`).Scan(&application); err != nil {
		return err
	}
	if version > sqliteSchemaVersion {
		return fmt.Errorf("SQLite schema %d is newer than supported %d", version, sqliteSchemaVersion)
	}
	if application != 0 && application != sqliteApplicationID {
		return errors.New("SQLite file belongs to another application")
	}
	if version == 0 {
		var tables int
		if err = tx.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`).Scan(&tables); err != nil {
			return err
		}
		if tables != 0 {
			return errors.New("refusing to initialize an unrecognized nonempty SQLite database")
		}
		if _, err = tx.Exec(schemaSQL); err != nil {
			return err
		}
		if _, err = tx.Exec(fmt.Sprintf(`PRAGMA application_id=%d`, sqliteApplicationID)); err != nil {
			return err
		}
		if _, err = tx.Exec(fmt.Sprintf(`PRAGMA user_version=%d`, sqliteSchemaVersion)); err != nil {
			return err
		}
	} else if application != sqliteApplicationID {
		return errors.New("SQLite database application ID is missing")
	}
	if version < 2 {
		if _, err = tx.Exec(`DROP TABLE IF EXISTS side_question_requests; DROP TABLE IF EXISTS side_question_sessions; PRAGMA user_version=2`); err != nil {
			return err
		}
	}
	if version < 3 {
		if _, err = tx.Exec(`-- Host-authored links from actual domain writes to their persisted activity.
CREATE TABLE IF NOT EXISTS node_sources (
    node_id INTEGER NOT NULL REFERENCES exploration_nodes(id) ON DELETE CASCADE,
    activity_id INTEGER NOT NULL REFERENCES activity(id) ON DELETE CASCADE,
    relation TEXT NOT NULL CHECK (relation IN ('created','produced','verified')),
    PRIMARY KEY (node_id, activity_id, relation)
);
CREATE INDEX IF NOT EXISTS node_sources_activity ON node_sources(activity_id);

PRAGMA user_version=3`); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (d *DB) queryPool(q string) *sql.DB {
	text := strings.ToUpper(strings.TrimSpace(q))
	if strings.HasPrefix(text, "INSERT") || strings.HasPrefix(text, "UPDATE") || strings.HasPrefix(text, "DELETE") {
		return d.writePool()
	}
	return d.DB
}

func sqliteFileURI(path string) string {
	path = filepath.ToSlash(path)
	// A Windows drive belongs in the URI path, never in its authority.
	if len(path) > 1 && path[1] == ':' {
		path = "/" + path
	}
	return (&url.URL{Scheme: "file", Path: path}).String()
}
