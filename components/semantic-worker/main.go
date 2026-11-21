// The native component is deliberately a separate Go module: its CGO and ONNX
// dependencies never become dependencies of the portable application binary.
package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/emhha/scopeforge/internal/semantic"
	"trpc.group/trpc-go/trpc-agent-go/knowledge/document"
	"trpc.group/trpc-go/trpc-agent-go/knowledge/vectorstore"
	"trpc.group/trpc-go/trpc-agent-go/knowledge/vectorstore/sqlitevec"
	"trpc.group/trpc-go/trpc-agent-go/memory"
	memoryvec "trpc.group/trpc-go/trpc-agent-go/memory/sqlitevec"
)

type worker struct {
	store    *sqlitevec.Store
	options  semantic.Options
	model    *localModel
	memories *memoryvec.Service
	vectors  *suppliedVector
}

// Embeddings are generated/cached by the host. Native Memory Service consumes
// the supplied vector inside this single-request worker; it still owns its
// SQL, scopes, metadata filters, deduplication and hybrid ranking.
type suppliedVector struct {
	dimensions int
	vector     []float64
}

func (e *suppliedVector) GetDimensions() int { return e.dimensions }
func (e *suppliedVector) GetEmbedding(context.Context, string) ([]float64, error) {
	if len(e.vector) != e.dimensions {
		return nil, errors.New("missing supplied embedding")
	}
	return e.vector, nil
}
func (e *suppliedVector) GetEmbeddingWithUsage(ctx context.Context, t string) ([]float64, map[string]any, error) {
	v, err := e.GetEmbedding(ctx, t)
	return v, nil, err
}

func scopeID(key memory.UserKey) string {
	sum := sha256.Sum256([]byte(key.AppName + "\x00" + key.UserID))
	return hex.EncodeToString(sum[:])
}
func recordID(scope, id string) string {
	sum := sha256.Sum256([]byte(scope + "\x00" + id))
	return hex.EncodeToString(sum[:])
}

func (w *worker) handle(req semantic.Request) (res semantic.Response, err error) {
	ctx := context.Background()
	if req.Op == "init" {
		if req.Options == nil || req.Options.Protocol != semantic.ProtocolVersion {
			return res, errors.New("unsupported protocol")
		}
		if w.store != nil {
			if w.options != *req.Options {
				return res, errors.New("component already configured")
			}
			return semantic.Response{Protocol: semantic.ProtocolVersion}, nil
		}
		w.options = *req.Options
		w.store, err = sqlitevec.New(sqlitevec.WithDSN(w.options.Database+"?_busy_timeout=5000&_journal_mode=WAL"), sqlitevec.WithIndexDimension(w.options.Dimensions), sqlitevec.WithMaxResults(20))
		if err != nil {
			return res, err
		}
		conn, e := sql.Open("sqlite3", w.options.Database+"?_busy_timeout=5000&_journal_mode=WAL")
		if e != nil {
			return res, e
		}
		conn.SetMaxOpenConns(1)
		w.vectors = &suppliedVector{dimensions: w.options.Dimensions}
		w.memories, err = memoryvec.NewService(conn, memoryvec.WithEmbedder(w.vectors), memoryvec.WithIndexDimension(w.options.Dimensions), memoryvec.WithMemoryLimit(1000), memoryvec.WithMaxResults(20), memoryvec.WithSoftDelete(false))
		if err != nil {
			conn.Close()
			return res, err
		}
		return semantic.Response{Protocol: semantic.ProtocolVersion}, err
	}
	if w.store == nil {
		return res, errors.New("component not initialized")
	}
	if req.Op == "embed" {
		if w.model == nil {
			w.model, err = openModel(w.options)
			if err != nil {
				return res, err
			}
		}
		res.Vector, err = w.model.embed(req.Text)
		return res, err
	}
	scope := scopeID(req.Scope)
	if req.Scope.AppName == "" || req.Scope.UserID == "" {
		return res, errors.New("scope required")
	}
	if req.Scope.AppName != "scopeforge-knowledge" {
		return w.handleMemory(ctx, req)
	}
	switch req.Op {
	case "list":
		metadata, e := w.store.GetMetadata(ctx, vectorstore.WithGetMetadataFilter(map[string]any{"scope": scope}), vectorstore.WithGetMetadataLimit(100001))
		if e != nil {
			return res, e
		}
		for id := range metadata {
			doc, _, e := w.store.Get(ctx, id)
			if e != nil {
				return res, e
			}
			var entry memory.Entry
			if e = json.Unmarshal([]byte(doc.Content), &entry); e != nil {
				return res, e
			}
			res.Entries = append(res.Entries, &entry)
		}
	case "put":
		if req.Entry == nil || req.Entry.ID == "" || req.Entry.Memory == nil {
			return res, errors.New("entry required")
		}
		if len(req.Vector) != w.options.Dimensions {
			return res, errors.New("embedding dimension mismatch")
		}
		data, _ := json.Marshal(req.Entry)
		doc := &document.Document{ID: recordID(scope, req.Entry.ID), Content: string(data), Metadata: map[string]any{"scope": scope}}
		// Native Update handles insertion into vec0 with delete/insert semantics.
		if _, _, e := w.store.Get(ctx, doc.ID); e == nil {
			err = w.store.Update(ctx, doc, req.Vector)
		} else {
			err = w.store.Add(ctx, doc, req.Vector)
		}
	case "delete":
		err = w.store.Delete(ctx, recordID(scope, req.ID))
	case "search":
		if len(req.Vector) != w.options.Dimensions {
			return res, errors.New("query dimension mismatch")
		}
		result, e := w.store.Search(ctx, &vectorstore.SearchQuery{Vector: req.Vector, Limit: 20, SearchMode: vectorstore.SearchModeVector, Filter: &vectorstore.SearchFilter{Metadata: map[string]any{"scope": scope}}})
		if e != nil {
			return res, e
		}
		for _, hit := range result.Results {
			var entry memory.Entry
			if e = json.Unmarshal([]byte(hit.Document.Content), &entry); e != nil {
				return res, e
			}
			entry.Score = hit.Score
			res.Entries = append(res.Entries, &entry)
		}
	default:
		err = fmt.Errorf("unsupported operation %q", req.Op)
	}
	return res, err
}

func (w *worker) handleMemory(ctx context.Context, req semantic.Request) (res semantic.Response, err error) {
	w.vectors.vector = req.Vector
	switch req.Op {
	case "list":
		res.Entries, err = w.memories.ReadMemories(ctx, req.Scope, 100000)
	case "delete":
		err = w.memories.DeleteMemory(ctx, memory.Key{AppName: req.Scope.AppName, UserID: req.Scope.UserID, MemoryID: req.ID})
	case "put":
		if req.Entry == nil || req.Entry.Memory == nil {
			return res, errors.New("entry required")
		}
		m := req.Entry.Memory
		err = w.memories.AddMemory(ctx, req.Scope, m.Memory, m.Topics, memory.WithMetadata(&memory.Metadata{Kind: m.Kind, EventTime: m.EventTime, Participants: m.Participants, Location: m.Location}))
	case "search":
		var opts []memory.SearchOption
		if req.Search != nil {
			opts = append(opts, memory.WithSearchOptions(*req.Search))
		}
		res.Entries, err = w.memories.SearchMemories(ctx, req.Scope, req.Text, opts...)
	default:
		err = errors.New("unsupported memory operation")
	}
	return res, err
}

func main() {
	w := &worker{}
	defer func() {
		if w.model != nil {
			w.model.close()
		}
		if w.store != nil {
			w.store.Close()
		}
		if w.memories != nil {
			w.memories.Close()
		}
	}()
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 64<<10), 32<<20)
	out := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var req semantic.Request
		var res semantic.Response
		err := json.Unmarshal(scanner.Bytes(), &req)
		if err == nil {
			res, err = w.handle(req)
		}
		if err != nil {
			res.Error = err.Error()
		}
		res.ModelLoaded = w.model != nil
		if out.Encode(res) != nil {
			return
		}
	}
}
