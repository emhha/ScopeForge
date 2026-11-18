package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// archiveRows serializes one row at a time. Declared types distinguish JSON
// documents from text containing JSON, and retain the existing archive format.
type archiveRows struct {
	*Rows
	columns []string
	types   []*sql.ColumnType
}

func newArchiveRows(rows *Rows) (*archiveRows, error) {
	columns, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	types, err := rows.ColumnTypes()
	return &archiveRows{rows, columns, types}, err
}
func (r *archiveRows) Scan(dest ...any) error {
	values := make([]any, len(r.columns))
	targets := make([]any, len(values))
	for i := range values {
		targets[i] = &values[i]
	}
	if err := r.Rows.Scan(targets...); err != nil {
		return err
	}
	obj := make(map[string]any, len(values))
	for i, v := range values {
		typ := strings.ToUpper(r.types[i].DatabaseTypeName())
		if v != nil {
			switch typ {
			case "JSON", "JSON_TEXT":
				var raw []byte
				switch x := v.(type) {
				case string:
					raw = []byte(x)
				case []byte:
					raw = x
				}
				if !json.Valid(raw) {
					return fmt.Errorf("invalid JSON in %s", r.columns[i])
				}
				v = json.RawMessage(raw)
			case "BOOLEAN":
				if n, ok := v.(int64); ok {
					v = n != 0
				}
			default:
				if b, ok := v.([]byte); ok {
					v = string(b)
				}
			}
		}
		// Computed JSON aggregates have no declared type.
		if typ == "" && (r.columns[i] == "agents" || r.columns[i] == "vulnclasses") {
			if s, ok := v.(string); ok && json.Valid([]byte(s)) {
				v = json.RawMessage(s)
			}
		}
		if typ == "" && r.columns[i] == "last_used" && v != nil {
			var ts time.Time
			if err := (timeDestination{&ts}).Scan(v); err != nil {
				return err
			}
			v = ts
		}
		obj[r.columns[i]] = v
	}
	raw, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	if len(dest) != 1 {
		return fmt.Errorf("archive row expects one destination")
	}
	switch p := dest[0].(type) {
	case *[]byte:
		*p = raw
	case *json.RawMessage:
		*p = raw
	default:
		return fmt.Errorf("invalid archive destination %T", p)
	}
	return nil
}

var sqliteIdentifier = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

func tableColumnTypes(tx *Tx, table string) (map[string]string, error) {
	if types, ok := tx.columnTypes[table]; ok {
		return types, nil
	}
	if !sqliteIdentifier.MatchString(table) {
		return nil, fmt.Errorf("invalid table %q", table)
	}
	rows, err := tx.Query(`PRAGMA table_info("` + table + `")`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	types := map[string]string{}
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var def any
		if err := rows.Scan(&cid, &name, &typ, &notnull, &def, &pk); err != nil {
			return nil, err
		}
		types[name] = strings.ToUpper(typ)
		if types[name] == "JSON_TEXT" {
			types[name] = "JSON"
		}
	}
	if len(types) == 0 {
		return nil, fmt.Errorf("unknown table %q", table)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if tx.columnTypes == nil {
		tx.columnTypes = map[string]map[string]string{}
	}
	tx.columnTypes[table] = types
	return types, nil
}
func archiveSQLValue(value any, typ string) (any, error) {
	if value == nil {
		return nil, nil
	}
	if typ == "JSON" {
		b, e := json.Marshal(value)
		return string(b), e
	}
	if typ == "TIMESTAMP" {
		switch x := value.(type) {
		case string:
			if t, e := time.Parse(time.RFC3339Nano, x); e == nil {
				return t.UTC().Format("2006-01-02 15:04:05.000000000"), nil
			}
		case time.Time:
			return x.UTC().Format("2006-01-02 15:04:05.000000000"), nil
		}
	}
	if n, ok := value.(json.Number); ok {
		if i, e := n.Int64(); e == nil {
			return i, nil
		}
		return n.Float64()
	}
	return value, nil
}
func insertSQLiteArchiveRow(tx *Tx, table string, row map[string]any, conflict string) (sql.Result, error) {
	types, err := tableColumnTypes(tx, table)
	if err != nil {
		return nil, err
	}
	keys := []string{}
	for key := range row {
		if _, ok := types[key]; ok {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return nil, fmt.Errorf("empty archived %s row", table)
	}
	args := make([]any, len(keys))
	marks := make([]string, len(keys))
	cols := make([]string, len(keys))
	for i, key := range keys {
		args[i], err = archiveSQLValue(row[key], types[key])
		if err != nil {
			return nil, err
		}
		marks[i] = "?"
		cols[i] = `"` + key + `"`
	}
	return tx.Exec(`INSERT INTO "`+table+`" (`+strings.Join(cols, ",")+`) VALUES (`+strings.Join(marks, ",")+`) `+conflict, args...)
}
func updateSQLiteArchiveRow(tx *Tx, table string, id int64, row map[string]any) error {
	types, err := tableColumnTypes(tx, table)
	if err != nil {
		return err
	}
	keys := []string{}
	for k := range row {
		if k != "id" {
			if _, ok := types[k]; ok {
				keys = append(keys, k)
			}
		}
	}
	sort.Strings(keys)
	sets := []string{}
	args := []any{}
	for _, k := range keys {
		v, e := archiveSQLValue(row[k], types[k])
		if e != nil {
			return e
		}
		sets = append(sets, `"`+k+`"=?`)
		args = append(args, v)
	}
	args = append(args, id)
	_, err = tx.Exec(`UPDATE "`+table+`" SET `+strings.Join(sets, ",")+` WHERE id=?`, args...)
	return err
}

// Explicit projection keeps snapshots typed without PostgreSQL composite records.
func sqliteRowJSON(tx *Tx, table, alias string) (string, error) {
	if !sqliteIdentifier.MatchString(alias) {
		return "", fmt.Errorf("invalid alias")
	}
	types, err := tableColumnTypes(tx, table)
	if err != nil {
		return "", err
	}
	keys := []string{}
	for k := range types {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fields := []string{}
	for _, k := range keys {
		v := alias + `."` + k + `"`
		switch types[k] {
		case "JSON", "JSON_TEXT":
			v = "json(" + v + ")"
		case "BOOLEAN":
			v = "json(CASE WHEN " + v + " IS NULL THEN 'null' WHEN " + v + " THEN 'true' ELSE 'false' END)"
		case "TIMESTAMP":
			v = "CASE WHEN " + v + " IS NULL THEN NULL ELSE strftime('%Y-%m-%dT%H:%M:%fZ'," + v + ") END"
		}
		fields = append(fields, "'"+k+"'", v)
	}
	return "json_object(" + strings.Join(fields, ",") + ")", nil
}
