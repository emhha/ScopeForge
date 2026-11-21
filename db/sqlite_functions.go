package db

import (
	"bytes"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"math/big"
	"net/netip"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	sqlite3 "github.com/mattn/go-sqlite3"
)

func decodeJSON(value driver.Value) any {
	var b []byte
	switch v := value.(type) {
	case string:
		b = []byte(v)
	case []byte:
		b = v
	case nil:
		return nil
	case int64:
		return json.Number(strconv.FormatInt(v, 10))
	case float64:
		return json.Number(strconv.FormatFloat(v, 'g', -1, 64))
	default:
		return v
	}
	var result any
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.UseNumber()
	if decoder.Decode(&result) != nil {
		return string(b)
	}
	return result
}
func encodeJSON(value any) (driver.Value, error) {
	b, err := json.Marshal(value)
	return string(b), err
}
func jsonContains(container, want any) bool {
	switch w := want.(type) {
	case map[string]any:
		c, ok := container.(map[string]any)
		if !ok {
			return false
		}
		for k, v := range w {
			got, ok := c[k]
			if !ok || !jsonContains(got, v) {
				return false
			}
		}
		return true
	case []any:
		c, ok := container.([]any)
		if !ok {
			return false
		}
		for _, v := range w {
			found := false
			for _, got := range c {
				if jsonContains(got, v) {
					found = true
					break
				}
			}
			if !found {
				return false
			}
		}
		return true
	default:
		return equalJSON(container, want)
	}
}
func jsonArray(v driver.Value) []any {
	a, _ := decodeJSON(v).([]any)
	if a == nil {
		return []any{}
	}
	return a
}
func jsonUnion(a, b []any) []any {
	out := append([]any{}, a...)
	for _, v := range b {
		found := false
		for _, old := range out {
			if equalJSON(old, v) {
				found = true
				break
			}
		}
		if !found {
			out = append(out, v)
		}
	}
	return out
}
func prefix(v driver.Value) (netip.Prefix, bool) {
	if v == nil {
		return netip.Prefix{}, false
	}
	s := fmt.Sprint(v)
	if p, err := netip.ParsePrefix(s); err == nil {
		return p, true
	}
	if a, err := netip.ParseAddr(s); err == nil {
		return netip.PrefixFrom(a, a.BitLen()), true
	}
	return netip.Prefix{}, false
}

const BusinessSQLiteDriver = "scopeforge-sqlite3"

func init() {
	sql.Register(BusinessSQLiteDriver, &sqlite3.SQLiteDriver{ConnectHook: registerBusinessFunctions})
}

// These functions belong to the business schema. Register them on every business
// connection using mattn's API; native tRPC stores use the ordinary sqlite3 driver.
func registerBusinessFunctions(conn *sqlite3.SQLiteConn) error {
	var registrationErr error
	scalar := func(name string, n int, fn func([]driver.Value) (driver.Value, error)) {
		if registrationErr != nil {
			return
		}
		var impl any
		switch n {
		case 1:
			impl = func(a any) (any, error) { return fn([]driver.Value{a}) }
		case 2:
			impl = func(a, b any) (any, error) { return fn([]driver.Value{a, b}) }
		default:
			impl = func(args ...any) (any, error) {
				if n >= 0 && len(args) != n {
					return nil, fmt.Errorf("%s requires %d arguments", name, n)
				}
				values := make([]driver.Value, len(args))
				for i, v := range args {
					values[i] = v
				}
				return fn(values)
			}
		}
		registrationErr = conn.RegisterFunc(name, impl, true)
	}
	registrationErr = conn.RegisterFunc("utc_now", func() string { return time.Now().UTC().Format("2006-01-02 15:04:05.000000000") }, false)

	scalar("regexp", 2, func(a []driver.Value) (driver.Value, error) {
		if a[0] == nil || a[1] == nil {
			return int64(0), nil
		}
		r, err := regexp.Compile(fmt.Sprint(a[0]))
		if err != nil {
			return nil, err
		}
		return boolValue(r.MatchString(fmt.Sprint(a[1]))), nil
	})
	scalar("regexp_replace", -1, func(a []driver.Value) (driver.Value, error) {
		if len(a) < 3 {
			return nil, fmt.Errorf("regexp_replace requires three arguments")
		}
		r, err := regexp.Compile(fmt.Sprint(a[1]))
		if err != nil {
			return nil, err
		}
		return r.ReplaceAllString(fmt.Sprint(a[0]), fmt.Sprint(a[2])), nil
	})
	scalar("concat_ws", -1, func(a []driver.Value) (driver.Value, error) {
		var parts []string
		for _, v := range a[1:] {
			if v != nil {
				parts = append(parts, fmt.Sprint(v))
			}
		}
		return strings.Join(parts, fmt.Sprint(a[0])), nil
	})
	scalar("json_object_merge", 2, func(a []driver.Value) (driver.Value, error) {
		left, ok := decodeJSON(a[0]).(map[string]any)
		if !ok {
			return nil, fmt.Errorf("JSON object required")
		}
		right, ok := decodeJSON(a[1]).(map[string]any)
		if !ok {
			return nil, fmt.Errorf("JSON object required")
		}
		for k, v := range right {
			left[k] = v
		}
		return encodeJSON(left)
	})
	scalar("json_contains", 2, func(a []driver.Value) (driver.Value, error) {
		return boolValue(jsonContains(decodeJSON(a[0]), decodeJSON(a[1]))), nil
	})
	scalar("json_equal", 2, func(a []driver.Value) (driver.Value, error) {
		return boolValue(equalJSON(decodeJSON(a[0]), decodeJSON(a[1]))), nil
	})
	scalar("json_overlaps", 2, func(a []driver.Value) (driver.Value, error) {
		for _, x := range jsonArray(a[0]) {
			for _, y := range jsonArray(a[1]) {
				if equalJSON(x, y) {
					return int64(1), nil
				}
			}
		}
		return int64(0), nil
	})
	scalar("json_array_union", 2, func(a []driver.Value) (driver.Value, error) {
		return encodeJSON(jsonUnion(jsonArray(a[0]), jsonArray(a[1])))
	})
	scalar("json_array_add", 2, func(a []driver.Value) (driver.Value, error) {
		return encodeJSON(append(jsonArray(a[0]), decodeJSON(a[1])))
	})
	scalar("json_array_remove", 2, func(a []driver.Value) (driver.Value, error) {
		var result = []any{}
		for _, v := range jsonArray(a[0]) {
			if !equalJSON(v, decodeJSON(a[1])) {
				result = append(result, v)
			}
		}
		return encodeJSON(result)
	})
	scalar("json_array_join", 2, func(a []driver.Value) (driver.Value, error) {
		var result []string
		for _, v := range jsonArray(a[0]) {
			result = append(result, fmt.Sprint(v))
		}
		return strings.Join(result, fmt.Sprint(a[1])), nil
	})
	scalar("json_merge_ports", 2, func(a []driver.Value) (driver.Value, error) {
		result := []any{}
		positions := map[string]int{}
		for _, v := range append(jsonArray(a[0]), jsonArray(a[1])...) {
			m, ok := v.(map[string]any)
			if !ok {
				continue
			}
			key := fmt.Sprint(m["port"])
			if i, ok := positions[key]; ok {
				old := result[i].(map[string]any)
				if (old["service"] == nil || old["service"] == "") && m["service"] != nil && m["service"] != "" {
					result[i] = m
				}
			} else {
				positions[key] = len(result)
				result = append(result, m)
			}
		}
		return encodeJSON(result)
	})
	scalar("json_merge_params", 2, func(a []driver.Value) (driver.Value, error) {
		values := map[string]any{}
		encoded := map[string]string{}
		for _, v := range append(jsonArray(a[0]), jsonArray(a[1])...) {
			m, ok := v.(map[string]any)
			if !ok {
				continue
			}
			key := fmt.Sprint(m["location"]) + "\x00" + fmt.Sprint(m["name"])
			raw, err := json.Marshal(v)
			if err != nil {
				return nil, err
			}
			if previous, ok := encoded[key]; !ok || string(raw) > previous {
				encoded[key] = string(raw)
				values[key] = v
			}
		}
		keys := make([]string, 0, len(values))
		for k := range values {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		result := make([]any, 0, len(keys))
		for _, k := range keys {
			result = append(result, values[k])
		}
		return encodeJSON(result)
	})
	scalar("try_inet", 1, func(a []driver.Value) (driver.Value, error) {
		p, ok := prefix(a[0])
		if !ok {
			return nil, nil
		}
		return p.String(), nil
	})
	scalar("masklen", 1, func(a []driver.Value) (driver.Value, error) {
		p, ok := prefix(a[0])
		if !ok {
			return nil, nil
		}
		return int64(p.Bits()), nil
	})
	scalar("cidr_contains", 2, func(a []driver.Value) (driver.Value, error) {
		p, ok := prefix(a[0])
		q, valid := prefix(a[1])
		return boolValue(ok && valid && p.Bits() <= q.Bits() && p.Contains(q.Addr())), nil
	})

	return registrationErr
}

func boolValue(v bool) int64 {
	if v {
		return 1
	}
	return 0
}

// JSON numbers compare by value, as in PostgreSQL jsonb, without float64 loss.
func equalJSON(a, b any) bool {
	if left, ok := a.(json.Number); ok {
		right, ok := b.(json.Number)
		if !ok {
			return false
		}
		x, ok := new(big.Rat).SetString(string(left))
		if !ok {
			return false
		}
		y, ok := new(big.Rat).SetString(string(right))
		return ok && x.Cmp(y) == 0
	}
	switch x := a.(type) {
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, v := range x {
			w, ok := y[k]
			if !ok || !equalJSON(v, w) {
				return false
			}
		}
		return true
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !equalJSON(x[i], y[i]) {
				return false
			}
		}
		return true
	default:
		return reflect.DeepEqual(a, b)
	}
}
