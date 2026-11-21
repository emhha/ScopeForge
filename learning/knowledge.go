package learning

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/emhha/scopeforge/db"
	"github.com/google/uuid"
	"trpc.group/trpc-go/trpc-agent-go/knowledge"
	"trpc.group/trpc-go/trpc-agent-go/knowledge/document"
	_ "trpc.group/trpc-go/trpc-agent-go/knowledge/document/reader/docx"
	"trpc.group/trpc-go/trpc-agent-go/knowledge/source"
	filesource "trpc.group/trpc-go/trpc-agent-go/knowledge/source/file"
	"trpc.group/trpc-go/trpc-agent-go/memory"
)

const MaxDocumentBytes = 20 << 20

var documentKey = memory.UserKey{AppName: "scopeforge-knowledge", UserID: "documents"}

type Knowledge struct {
	*Retrieval
	syncMu        sync.Mutex
	mu            sync.Mutex
	pg            *db.DB
	dir           string
	ctx           context.Context
	cancel        context.CancelFunc
	wake          chan struct{}
	wg            sync.WaitGroup
	ownsRetrieval bool
	activeDoc     string
	activeCancel  context.CancelFunc
}

func OpenKnowledge(dir string, pg *db.DB) (*Knowledge, error) {
	retrieval, err := OpenRetrieval(dir, pg)
	if err != nil {
		return nil, err
	}
	k, err := OpenKnowledgeWithRetrieval(dir, pg, retrieval)
	if err != nil {
		retrieval.Close()
		return nil, err
	}
	k.ownsRetrieval = true
	return k, nil
}
func OpenKnowledgeWithRetrieval(dir string, pg *db.DB, retrieval *Retrieval) (*Knowledge, error) {
	path := filepath.Join(dir, "knowledge")
	ctx, cancel := context.WithCancel(context.Background())
	k := &Knowledge{Retrieval: retrieval, pg: pg, dir: path, ctx: ctx, cancel: cancel, wake: make(chan struct{}, 1)}
	fail := func(err error) (*Knowledge, error) { cancel(); return nil, err }
	if err := os.MkdirAll(filepath.Join(path, "uploads"), 0700); err != nil {
		return fail(err)
	}
	if _, err := k.cache.Exec(lexicalSchema); err != nil {
		return fail(err)
	}
	// Older installations have originals and embedding caches, but no text index.
	chunkVersion, _, err := pg.GetSetting("knowledge_chunking_version")
	if err != nil {
		return fail(err)
	}
	docs, err := pg.KnowledgeDocuments()
	if err != nil {
		return fail(err)
	}
	for _, d := range docs {
		var n int
		if err := k.cache.QueryRow(`SELECT count(*) FROM chunks WHERE document_id=?`, d.ID).Scan(&n); err != nil {
			return fail(err)
		}
		if (chunkVersion != "e5-400-80-v1" && d.Status == "ready") || d.Status == "indexing" || (d.Status == "ready" && n == 0) || (d.Status == "failed" && n == 0 && strings.Contains(d.Error, "Embedding")) {
			if err := pg.SetKnowledgeDocumentStatus(d.ID, "queued", "", 0); err != nil {
				return fail(err)
			}
		}
	}
	if err := pg.SetSetting("knowledge_chunking_version", "e5-400-80-v1"); err != nil {
		return fail(err)
	}
	k.wg.Add(1)
	go k.work()
	k.signal()
	return k, nil
}
func (k *Knowledge) Close() error {
	k.cancel()
	k.wg.Wait()
	if k.ownsRetrieval {
		return k.Retrieval.Close()
	}
	return nil
}
func (k *Knowledge) Status() SemanticStatus {
	status := k.semantic.Status()
	var revision, synced int64
	var generation string
	if err := k.cache.QueryRow(`SELECT revision,synced_revision,generation FROM lexical_state WHERE id=1`).Scan(&revision, &synced, &generation); err == nil && status.Ready && (revision != synced || generation != status.Generation) {
		status.Mode = "keyword"
		status.Message = "语义索引同步中，关键词检索可用"
	} else if status.Ready {
		status.Message = "语义增强已启用，关键词检索同时可用"
	}
	return status
}
func (k *Knowledge) path(d db.KnowledgeDocument) string {
	return filepath.Join(k.dir, d.ID+strings.ToLower(filepath.Ext(d.Name)))
}
func (k *Knowledge) source(d db.KnowledgeDocument) source.Source {
	return filesource.New([]string{k.path(d)}, filesource.WithName("document:"+d.ID), filesource.WithMetadata(map[string]any{"document_id": d.ID, "file_name": d.Name, "relative_path": d.RelativePath, "import_id": d.ImportID}), filesource.WithChunkSize(400), filesource.WithChunkOverlap(80))
}
func (k *Knowledge) Upload(name string, r io.Reader) (db.KnowledgeDocument, error) {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	ext := strings.ToLower(filepath.Ext(name))
	if len(name) > 240 || (ext != ".md" && ext != ".txt" && ext != ".docx") {
		return db.KnowledgeDocument{}, errors.New("支持 Markdown、TXT、DOCX，文件名不超过 240 字节")
	}
	d := db.KnowledgeDocument{ID: uuid.NewString(), Name: name, Status: "queued"}
	f, err := os.OpenFile(k.path(d), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return d, err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(r, MaxDocumentBytes+1))
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if n > MaxDocumentBytes {
		err = errors.New("文档超过 20 MiB")
	}
	if n == 0 {
		err = errors.New("文档不能为空")
	}
	if err != nil {
		os.Remove(k.path(d))
		return d, err
	}
	d.Size = n
	d.SHA256 = hex.EncodeToString(h.Sum(nil))
	k.mu.Lock()
	err = k.pg.AddKnowledgeDocument(d)
	k.mu.Unlock()
	if err != nil {
		os.Remove(k.path(d))
		return d, err
	}
	k.signal()
	return d, nil
}
func (k *Knowledge) Retry(id string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	docs, err := k.pg.KnowledgeDocuments()
	if err != nil {
		return err
	}
	for _, d := range docs {
		if d.ID == id {
			if err := k.pg.SetKnowledgeDocumentStatus(id, "queued", "", 0); err != nil {
				return err
			}
			k.signal()
			return nil
		}
	}
	return os.ErrNotExist
}
func (k *Knowledge) Delete(ctx context.Context, id string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	docs, err := k.pg.KnowledgeDocuments()
	if err != nil {
		return err
	}
	for _, d := range docs {
		if d.ID != id {
			continue
		}
		// Delete authoritative metadata first; stale semantic hits are rejected immediately.
		if err := k.pg.DeleteKnowledgeDocument(id); err != nil {
			return err
		}
		if err := replaceChunks(ctx, k.cache, id, nil); err != nil {
			return err
		}
		k.signal()
		if err := os.Remove(k.path(d)); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	return os.ErrNotExist
}
func (k *Knowledge) signal() {
	select {
	case k.wake <- struct{}{}:
	default:
	}
}
func (k *Knowledge) work() {
	defer k.wg.Done()
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-k.ctx.Done():
			return
		case <-k.wake:
		case <-ticker.C:
		}
		for k.ctx.Err() == nil {
			imported := k.importNext()
			parsed := k.indexNext()
			if !imported && !parsed {
				break
			}
		}
		_ = k.pg.FinishKnowledgeImports()
		if k.ctx.Err() != nil {
			return
		}
		if err := k.syncDocuments(k.ctx); err != nil && k.ctx.Err() == nil {
			log.Printf("[knowledge] semantic index pending; keyword retrieval remains available")
		}
	}
}
func (k *Knowledge) indexNext() bool {
	k.mu.Lock()
	docs, err := k.pg.KnowledgeDocuments()
	if err != nil {
		k.mu.Unlock()
		return false
	}
	var chosen *db.KnowledgeDocument
	for i := range docs {
		if docs[i].Status == "queued" {
			chosen = &docs[i]
			break
		}
	}
	if chosen == nil {
		k.mu.Unlock()
		return false
	}
	d := *chosen
	if err = k.pg.SetKnowledgeDocumentStatus(d.ID, "indexing", "", 0); err != nil {
		k.mu.Unlock()
		return false
	}
	ctx, cancel := context.WithTimeout(k.ctx, time.Minute)
	k.activeDoc = d.ID
	k.activeCancel = cancel
	k.mu.Unlock()
	chunks, err := k.readDocument(ctx, d)
	k.mu.Lock()
	defer k.mu.Unlock()
	defer cancel()
	k.activeDoc = ""
	k.activeCancel = nil
	var status string
	if e := k.pg.QueryRow(`SELECT status FROM knowledge_documents WHERE id=?1`, d.ID).Scan(&status); e != nil || status != "indexing" {
		return true
	}
	if k.ctx.Err() != nil {
		return false
	} // startup recovers interrupted indexing
	if err == nil && len(chunks) == 0 {
		err = errors.New("empty document")
	}
	if err == nil {
		for _, chunk := range chunks {
			chunk.Name = d.Name
		}
		err = replaceChunks(ctx, k.cache, d.ID, chunks)
	}
	if err != nil {
		log.Printf("[knowledge] document %s indexing failed: %v", d.ID, err)
		_ = k.pg.SetKnowledgeDocumentStatus(d.ID, "failed", "无法解析文档，请检查文件格式和文本内容", 0)
	} else {
		_ = k.pg.SetKnowledgeDocumentStatus(d.ID, "ready", "", len(chunks))
	}
	return true
}
func (k *Knowledge) syncDocuments(ctx context.Context) error {
	k.syncMu.Lock()
	defer k.syncMu.Unlock()
	if !k.semantic.Status().Ready {
		return nil
	}
	_, generation := k.semantic.remote()
	var revision, synced int64
	var previous string
	if err := k.cache.QueryRowContext(ctx, `SELECT revision,synced_revision,generation FROM lexical_state WHERE id=1`).Scan(&revision, &synced, &previous); err != nil {
		return err
	}
	if revision == synced && previous == generation {
		return nil
	}
	docs, err := k.pg.KnowledgeDocuments()
	if err != nil {
		return err
	}
	allowed := map[string]bool{}
	for _, d := range docs {
		if d.Status == "ready" {
			allowed[d.ID] = true
		}
	}
	rows, err := k.cache.QueryContext(ctx, `SELECT document_json FROM chunks ORDER BY id`)
	if err != nil {
		return err
	}
	entries := []*memory.Entry{}
	for rows.Next() {
		var raw string
		if err = rows.Scan(&raw); err != nil {
			break
		}
		var d document.Document
		if err = json.Unmarshal([]byte(raw), &d); err != nil {
			break
		}
		id, _ := d.Metadata["document_id"].(string)
		if allowed[id] {
			entries = append(entries, &memory.Entry{Memory: &memory.Memory{Memory: d.Content, Kind: memory.KindFact, Location: d.ID}})
		}
	}
	rowErr := rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if rowErr != nil {
		return rowErr
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	if err := k.semantic.Sync(ctx, documentKey, entries); err != nil {
		return err
	}
	if _, current := k.semantic.remote(); current != generation {
		return nil
	}
	_, err = k.cache.ExecContext(ctx, `UPDATE lexical_state SET synced_revision=?,generation=? WHERE id=1 AND revision=?`, revision, generation, revision)
	return err
}
func (k *Knowledge) Search(ctx context.Context, req *knowledge.SearchRequest) (*knowledge.SearchResult, error) {
	if req == nil {
		return nil, errors.New("search request is required")
	}
	docs, err := k.pg.KnowledgeDocuments()
	if err != nil {
		return nil, err
	}
	allowed := map[string]bool{}
	for _, d := range docs {
		if d.Status == "ready" {
			allowed[d.ID] = true
		}
	}
	lexical, err := k.keywordSearch(ctx, req, allowed)
	if err != nil {
		return nil, err
	}
	mode := "keyword"
	combined := []*knowledge.Result{}
	seen := map[string]bool{}
	semanticIDs := []string{}
	lexicalIDs := []string{}
	if k.Status().Mode == "hybrid" && strings.TrimSpace(req.Query) != "" {
		entries, searchErr := k.semantic.Search(ctx, documentKey, req.Query)
		if searchErr == nil {
			mode = "hybrid"
			for _, entry := range entries {
				if entry.Memory == nil {
					continue
				}
				d, err := getChunk(ctx, k.cache, entry.Memory.Location)
				if err != nil || d.Content != entry.Memory.Memory {
					continue
				}
				id, _ := d.Metadata["document_id"].(string)
				if !allowed[id] {
					continue
				}
				ok, err := matchDocument(d, req.SearchFilter)
				if err != nil {
					return nil, err
				}
				if ok && !seen[d.ID] {
					semanticIDs = append(semanticIDs, d.ID)
					combined = append(combined, &knowledge.Result{Document: d, Score: entry.Score})
					seen[d.ID] = true
				}
			}
		}
	}
	for _, hit := range lexical {
		lexicalIDs = append(lexicalIDs, hit.Document.ID)
		if !seen[hit.Document.ID] {
			combined = append(combined, hit)
			seen[hit.Document.ID] = true
		}
	}
	if mode == "hybrid" {
		scores := fuseRanks(semanticIDs, lexicalIDs)
		rankByScore(combined, func(h *knowledge.Result) string { return h.Document.ID }, scores)
	}
	if len(combined) > searchLimit(req) {
		combined = combined[:searchLimit(req)]
	}
	result := &knowledge.SearchResult{Documents: combined}
	for _, hit := range combined {
		hit.Document.Metadata["retrieval_mode"] = mode
	}
	if len(combined) > 0 {
		result.Document = combined[0].Document
		result.Text = result.Document.Content
		result.Score = combined[0].Score
	}
	return result, nil
}

func (k *Knowledge) UploadDirectory() string { return filepath.Join(k.dir, "uploads") }
