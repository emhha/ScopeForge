package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
	"time"
)

// SQLite returns JSON as text. These scanners retain the existing domain DTOs
// while letting database/sql continue to own row and connection lifetimes.
type Rows struct{ *sql.Rows }
type Row struct{ *sql.Row }
type jsonDestination struct{ target any }

func (j jsonDestination) Scan(value any) error {
	var data []byte
	switch v := value.(type) {
	case nil:
		data = []byte("null")
	case string:
		data = []byte(v)
	case []byte:
		data = v
	default:
		return fmt.Errorf("invalid JSON column value %T", value)
	}
	if raw, ok := j.target.(*json.RawMessage); ok {
		*raw = append((*raw)[:0], data...)
		return nil
	}
	return json.Unmarshal(data, j.target)
}
func scanDestinations(dest []any) []any {
	out := append([]any(nil), dest...)
	for i, target := range out {
		switch target.(type) {
		case *time.Time, **time.Time, *sql.NullTime:
			out[i] = timeDestination{target}
			continue
		}

		if _, ok := target.(*json.RawMessage); ok {
			out[i] = jsonDestination{target}
			continue
		}
		t := reflect.TypeOf(target)
		if t != nil && t.Kind() == reflect.Pointer && t.Elem().Kind() == reflect.Slice && t.Elem().Elem().Kind() != reflect.Uint8 {
			out[i] = jsonDestination{target}
		}
	}
	return out
}
func (r *Rows) Scan(dest ...any) error { return r.Rows.Scan(scanDestinations(dest)...) }
func (r *Row) Scan(dest ...any) error  { return r.Row.Scan(scanDestinations(dest)...) }
func wrapRows(rows *sql.Rows, err error) (*Rows, error) {
	if err != nil {
		return nil, err
	}
	return &Rows{rows}, nil
}

type timeDestination struct{ target any }

func (t timeDestination) Scan(v any) error {
	var value time.Time
	if v != nil {
		switch x := v.(type) {
		case time.Time:
			value = x
		case string:
			var err error
			for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999-07:00", "2006-01-02 15:04:05.999999999Z07:00", "2006-01-02 15:04:05.999999999", "2006-01-02 15:04:05"} {
				value, err = time.Parse(layout, x)
				if err == nil {
					break
				}
			}
			if err != nil {
				return fmt.Errorf("invalid SQLite timestamp %q: %w", x, err)
			}
		default:
			return fmt.Errorf("invalid timestamp %T", v)
		}
	}
	switch dest := t.target.(type) {
	case *time.Time:
		*dest = value
	case **time.Time:
		if v == nil {
			*dest = nil
		} else {
			*dest = &value
		}
	case *sql.NullTime:
		dest.Time = value
		dest.Valid = v != nil
	}
	return nil
}
