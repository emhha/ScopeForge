package learning

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"unicode"

	"trpc.group/trpc-go/trpc-agent-go/knowledge"
	"trpc.group/trpc-go/trpc-agent-go/knowledge/document"
	"trpc.group/trpc-go/trpc-agent-go/knowledge/searchfilter"
)

const lexicalSchema = `CREATE TABLE IF NOT EXISTS chunks(id TEXT PRIMARY KEY,document_id TEXT NOT NULL,document_json TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS chunks_document ON chunks(document_id);
CREATE VIRTUAL TABLE IF NOT EXISTS chunk_terms USING fts5(id UNINDEXED,terms,tokenize='unicode61');
CREATE TABLE IF NOT EXISTS lexical_state(id INTEGER PRIMARY KEY CHECK(id=1),revision INTEGER NOT NULL DEFAULT 0,synced_revision INTEGER NOT NULL DEFAULT -1,generation TEXT NOT NULL DEFAULT '');
INSERT OR IGNORE INTO lexical_state(id) VALUES(1);`

// Explicit Han n-grams make Chinese short terms searchable with stock FTS5;
// english identifiers are retained as words. No SQLite extension is required.
func lexicalTerms(text string) []string {
	seen := map[string]bool{}
	out := []string{}
	add := func(s string) {
		s = strings.ToLower(s)
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	var word, han []rune
	flushWord := func() { add(string(word)); word = nil }
	flushHan := func() {
		for i, r := range han {
			add(string(r))
			if i+1 < len(han) {
				add(string(han[i : i+2]))
			}
			if i+2 < len(han) {
				add(string(han[i : i+3]))
			}
		}
		han = nil
	}
	for _, r := range text {
		if unicode.Is(unicode.Han, r) {
			flushWord()
			han = append(han, r)
		} else {
			flushHan()
			if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
				word = append(word, r)
			} else {
				flushWord()
			}
		}
	}
	flushWord()
	flushHan()
	return out
}
func lexicalQuery(text string) string {
	terms := lexicalTerms(text)
	selected := []string{}
	for _, term := range terms {
		if len([]rune(term)) == 1 && len(terms) > 1 {
			continue
		}
		switch term {
		case "如何", "什么", "是否", "可以", "一个", "进行", "的", "how", "what", "the", "to", "is":
			continue
		}
		selected = append(selected, `"`+strings.ReplaceAll(term, `"`, `""`)+`"`)
		if len(selected) == 48 {
			break
		}
	}
	if len(selected) == 0 && len(terms) > 0 {
		selected = append(selected, `"`+terms[0]+`"`)
	}
	return strings.Join(selected, " OR ")
}
func replaceChunks(ctx context.Context, conn *sql.DB, id string, docs []*document.Document) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM chunk_terms WHERE id IN (SELECT id FROM chunks WHERE document_id=?)`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM chunks WHERE document_id=?`, id); err != nil {
		return err
	}
	for i, d := range docs {
		if strings.TrimSpace(d.Content) == "" {
			continue
		}
		h := sha256.Sum256([]byte(fmt.Sprintf("%s:%d:%s", id, i, d.Content)))
		d.ID = hex.EncodeToString(h[:])
		if d.Metadata == nil {
			d.Metadata = map[string]any{}
		}
		d.Metadata["document_id"] = id
		d.Metadata["chunk_index"] = i
		b, err := json.Marshal(d)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO chunks(id,document_id,document_json) VALUES(?,?,?)`, d.ID, id, string(b)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO chunk_terms(id,terms) VALUES(?,?)`, d.ID, strings.Join(lexicalTerms(d.Content), " ")); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE lexical_state SET revision=revision+1 WHERE id=1`); err != nil {
		return err
	}
	return tx.Commit()
}
func getChunk(ctx context.Context, conn *sql.DB, id string) (*document.Document, error) {
	var raw string
	if err := conn.QueryRowContext(ctx, `SELECT document_json FROM chunks WHERE id=?`, id).Scan(&raw); err != nil {
		return nil, err
	}
	var d document.Document
	err := json.Unmarshal([]byte(raw), &d)
	return &d, err
}
func (k *Knowledge) keywordSearch(ctx context.Context, req *knowledge.SearchRequest, allowed map[string]bool) ([]*knowledge.Result, error) {
	q := lexicalQuery(req.Query)
	if q == "" && req.SearchFilter == nil {
		return []*knowledge.Result{}, nil
	}
	var rows *sql.Rows
	var err error
	if q == "" {
		rows, err = k.cache.QueryContext(ctx, `SELECT document_json FROM chunks ORDER BY id LIMIT 1000`)
	} else {
		rows, err = k.cache.QueryContext(ctx, `SELECT c.document_json FROM chunk_terms f JOIN chunks c ON c.id=f.id WHERE chunk_terms MATCH ? ORDER BY bm25(chunk_terms) LIMIT 1000`, q)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*knowledge.Result{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var d document.Document
		if err := json.Unmarshal([]byte(raw), &d); err != nil {
			return nil, err
		}
		if !allowed[fmt.Sprint(d.Metadata["document_id"])] {
			continue
		}
		ok, err := matchDocument(&d, req.SearchFilter)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, &knowledge.Result{Document: &d, Score: 1 / float64(1+len(out))})
		}
		if len(out) >= searchLimit(req) {
			break
		}
	}
	return out, rows.Err()
}
func searchLimit(req *knowledge.SearchRequest) int {
	if req.MaxResults < 1 || req.MaxResults > 10 {
		return 5
	}
	return req.MaxResults
}
func matchDocument(d *document.Document, f *knowledge.SearchFilter) (bool, error) {
	if f == nil {
		return true, nil
	}
	if len(f.DocumentIDs) > 0 {
		found := false
		for _, id := range f.DocumentIDs {
			found = found || d.ID == id
		}
		if !found {
			return false, nil
		}
	}
	for key, v := range f.Metadata {
		if !reflect.DeepEqual(d.Metadata[strings.TrimPrefix(key, "metadata.")], v) {
			return false, nil
		}
	}
	return matchCondition(d, f.FilterCondition)
}
func matchCondition(d *document.Document, c *searchfilter.UniversalFilterCondition) (bool, error) {
	if c == nil {
		return true, nil
	}
	if c.Operator == "and" || c.Operator == "or" {
		b, _ := json.Marshal(c.Value)
		var children []*searchfilter.UniversalFilterCondition
		if err := json.Unmarshal(b, &children); err != nil || len(children) == 0 {
			return false, errors.New("invalid filter conditions")
		}
		result := c.Operator == "and"
		for _, child := range children {
			ok, err := matchCondition(d, child)
			if err != nil {
				return false, err
			}
			if c.Operator == "and" {
				result = result && ok
			} else {
				result = result || ok
			}
		}
		return result, nil
	}
	var v any
	switch c.Field {
	case "id":
		v = d.ID
	case "name":
		v = d.Name
	case "content":
		v = d.Content
	default:
		if !strings.HasPrefix(c.Field, "metadata.") {
			return false, errors.New("unsupported filter field")
		}
		v = d.Metadata[strings.TrimPrefix(c.Field, "metadata.")]
	}
	equal := func(a, b any) bool { return fmt.Sprint(a) == fmt.Sprint(b) }
	switch c.Operator {
	case "eq":
		return equal(v, c.Value), nil
	case "ne":
		return !equal(v, c.Value), nil
	case "in", "not in":
		b, _ := json.Marshal(c.Value)
		var values []any
		if json.Unmarshal(b, &values) != nil {
			return false, errors.New("invalid filter values")
		}
		found := false
		for _, x := range values {
			found = found || equal(v, x)
		}
		if c.Operator == "not in" {
			found = !found
		}
		return found, nil
	case "like", "not like":
		p := regexp.QuoteMeta(fmt.Sprint(c.Value))
		p = strings.ReplaceAll(strings.ReplaceAll(p, "%", ".*"), "_", ".")
		ok, err := regexp.MatchString("(?is)^"+p+"$", fmt.Sprint(v))
		if c.Operator == "not like" {
			ok = !ok
		}
		return ok, err
	default:
		return false, errors.New("unsupported filter operator")
	}
}
