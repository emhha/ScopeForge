package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestSQLiteWriterIsolationAndWALReaders(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	other, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err = d.SetSetting("wal-test", "committed"); err != nil {
		t.Fatal(err)
	}
	tx, err := d.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`UPDATE settings SET value='pending' WHERE key='wal-test'`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	var value string
	if err = other.QueryRowContext(ctx, `SELECT value FROM settings WHERE key='wal-test'`).Scan(&value); err != nil || value != "committed" {
		t.Fatalf("WAL read=%q err=%v", value, err)
	}
	done := make(chan error, 1)
	go func() { done <- other.SetSetting("wal-test", "next") }()
	select {
	case err = <-done:
		t.Fatalf("second writer escaped: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("writer did not resume")
	}
	if value, _, err = other.GetSetting("wal-test"); err != nil || value != "next" {
		t.Fatalf("committed value=%q err=%v", value, err)
	}
	var mode string
	var foreignKeys int
	if err = d.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("mode=%s err=%v", mode, err)
	}
	if err = d.QueryRow(`PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil || foreignKeys != 1 {
		t.Fatal("foreign keys disabled", err)
	}
}
func TestSQLiteRejectsForeignAndFutureDatabases(t *testing.T) {
	for _, test := range []struct{ name, ddl string }{{"unrecognized", `CREATE TABLE unrelated(id INTEGER)`}, {"future", `PRAGMA application_id=1095914584; PRAGMA user_version=999`}} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "store.sqlite")
			raw, err := sql.Open(BusinessSQLiteDriver, path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = raw.Exec(test.ddl); err != nil {
				t.Fatal(err)
			}
			raw.Close()
			if d, err := Open(path); err == nil {
				d.Close()
				t.Fatal("unrecognized schema accepted")
			}
		})
	}
}
func TestSQLiteContextCancelsQueuedWriter(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "store.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	tx, err := d.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if _, err = d.ExecContext(ctx, `INSERT INTO settings(key,value)VALUES('cancelled','no')`); err == nil {
		t.Fatal("queued writer ignored cancellation")
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = d.QueryRow(`SELECT count(*) FROM settings WHERE key='cancelled'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("cancelled write persisted: %d %v", count, err)
	}
}
func TestSQLiteJSONRetainsNumericPrecisionAndValueEquality(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "store.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for _, value := range []string{`123456789012345678901234567890`, `1.00000000000000000001`, `1e300`, `null`, `{"nested":[1,2]}`} {
		var id int64
		if err = d.QueryRow(`INSERT INTO assets(type,domain,extra)VALUES('root_domain',?,?) RETURNING id`, value, []byte(value)).Scan(&id); err != nil {
			t.Fatal(err)
		}
		var text, kind string
		if err = d.QueryRow(`SELECT extra,typeof(extra) FROM assets WHERE id=?`, id).Scan(&text, &kind); err != nil || text != value || kind != "text" {
			t.Fatalf("JSON changed: %s -> %s (%s): %v", value, text, kind, err)
		}
		if _, _, err = queryArchiveRows(d, `SELECT * FROM assets WHERE id=?`, id); err != nil {
			t.Fatal(err)
		}
	}
	var equal, contains bool
	if err = d.QueryRow(`SELECT json_equal('{"n":1.0}','{"n":1}'),json_contains('[1e2]','[100]')`).Scan(&equal, &contains); err != nil || !equal || !contains {
		t.Fatalf("numeric equality=%v containment=%v err=%v", equal, contains, err)
	}
}

func TestSQLiteAssetMergePreservesNullAndLearnedService(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "store.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var merged string
	if err = d.QueryRow(`SELECT json_object_merge('{"nullable":1,"nested":{"a":1}}','{"nullable":null,"nested":{"b":2}}')`).Scan(&merged); err != nil {
		t.Fatal(err)
	}
	var equal bool
	if err = d.QueryRow(`SELECT json_equal(?, '{"nullable":null,"nested":{"b":2}}')`, merged).Scan(&equal); err != nil || !equal {
		t.Fatalf("shallow merge=%s: %v", merged, err)
	}
	id, err := d.Assets().UpsertIP(UpsertIPReq{IP: "192.0.2.53", OpenPorts: []PortService{{Port: 53}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.Assets().UpsertIP(UpsertIPReq{IP: "192.0.2.53", OpenPorts: []PortService{{Port: 53, Service: "dns"}}}); err != nil {
		t.Fatal(err)
	}
	var ports string
	if err = d.QueryRow(`SELECT open_ports FROM assets WHERE id=?`, id).Scan(&ports); err != nil {
		t.Fatal(err)
	}
	if err = d.QueryRow(`SELECT json_equal(?, '[{"port":53,"service":"dns"}]')`, ports).Scan(&equal); err != nil || !equal {
		t.Fatalf("service lost: %s %v", ports, err)
	}
}

func TestSQLiteFileURIWindowsDriveAndEscaping(t *testing.T) {
	for _, test := range []struct{ path, want string }{
		{"C:/Users/Test/data/scopeforge.sqlite", "file:///C:/Users/Test/data/scopeforge.sqlite"},
		{"/home/test/data/scopeforge.sqlite", "file:///home/test/data/scopeforge.sqlite"},
		{"/home/test/data/a b?.sqlite", "file:///home/test/data/a%20b%3F.sqlite"},
	} {
		if got := sqliteFileURI(test.path); got != test.want {
			t.Fatalf("URI %q = %q, want %q", test.path, got, test.want)
		}
	}
}
