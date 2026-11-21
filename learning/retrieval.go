package learning

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/emhha/scopeforge/db"
	"github.com/emhha/scopeforge/internal/semantic"
	"github.com/google/uuid"
	"os"
	"path/filepath"
	"sync"
	"time"
	"trpc.group/trpc-go/trpc-agent-go/knowledge/embedder"
)

const embeddingSetting = "retrieval_embedding"

type Retrieval struct {
	mu         sync.RWMutex
	pg         *db.DB
	dir        string
	config     EmbeddingConfig
	ctx        context.Context
	cancel     context.CancelFunc
	wake       chan struct{}
	wg         sync.WaitGroup
	cache      *sql.DB
	semantic   *SemanticIndex
	components *semantic.Installer
}

func OpenRetrieval(dir string, pg *db.DB) (*Retrieval, error) {
	// Preserve the existing files while moving ownership out of Knowledge.
	path := filepath.Join(dir, "knowledge")
	if err := os.MkdirAll(path, 0700); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	k := &Retrieval{pg: pg, dir: path, ctx: ctx, cancel: cancel, wake: make(chan struct{}, 1), components: semantic.NewInstaller(filepath.Join(path, "components"), semantic.FindCatalog())}
	k.semantic = newSemanticIndex(path, k.components)
	cache, err := sql.Open("sqlite3", filepath.Join(path, "embeddings.db"))
	if err != nil {
		cancel()
		return nil, err
	}
	cache.SetMaxOpenConns(1)
	k.cache = cache
	fail := func(err error) (*Retrieval, error) { cancel(); k.components.Close(); cache.Close(); return nil, err }
	if _, err := cache.Exec(`PRAGMA busy_timeout=5000; CREATE TABLE IF NOT EXISTS embeddings(id TEXT PRIMARY KEY,embedding TEXT NOT NULL);`); err != nil {
		return fail(err)
	}
	if _, ok, err := pg.GetSetting(embeddingSetting); err != nil {
		return fail(err)
	} else if !ok {
		if raw, exists, err := pg.GetSetting("knowledge_embedding"); err != nil {
			return fail(err)
		} else if exists {
			if err = pg.SetSetting(embeddingSetting, raw); err != nil {
				return fail(err)
			}
		}
	}
	if raw, ok, err := pg.GetSetting(embeddingSetting); err != nil {
		return fail(err)
	} else if ok {
		var legacy map[string]json.RawMessage
		_ = json.Unmarshal([]byte(raw), &legacy)
		if legacy["chroma_url"] != nil {
			raw = `{"mode":"none"}`
			if err := pg.SetSetting(embeddingSetting, raw); err != nil {
				return fail(err)
			}
		}
		if err := json.Unmarshal([]byte(raw), &k.config); err != nil {
			return fail(err)
		}
	}
	if _, err := pg.InsertSettingIfAbsent("learning_instance_id", uuid.NewString()); err != nil {
		return fail(err)
	}
	instance, _, err := pg.GetSetting("learning_instance_id")
	if err != nil {
		return fail(err)
	}
	k.config.namespace = instance
	k.config, err = normalizeEmbedding(k.config, k.config)
	if err != nil {
		return fail(err)
	}
	k.semantic.configure(k.config)

	k.wg.Add(1)
	go k.work()
	k.signal()
	return k, nil
}
func (k *Retrieval) Close() error {
	k.cancel()
	k.wg.Wait()
	k.components.Close()
	return errors.Join(k.semantic.Close(), k.cache.Close())
}
func (k *Retrieval) Components() *semantic.Installer { return k.components }
func (k *Retrieval) SemanticIndex() *SemanticIndex   { return k.semantic }
func (k *Retrieval) Config() EmbeddingConfig {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return k.config.sanitized()
}
func (k *Retrieval) Status() SemanticStatus { return k.semantic.Status() }
func (k *Retrieval) signal() {
	select {
	case k.wake <- struct{}{}:
	default:
	}
}
func (k *Retrieval) work() {
	defer k.wg.Done()
	timer := time.NewTicker(10 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-k.ctx.Done():
			return
		case <-timer.C:
		case <-k.wake:
		}
		k.semantic.ensure(k.embedding())
	}
}
func (k *Retrieval) prepare(c EmbeddingConfig) (EmbeddingConfig, error) {
	k.mu.RLock()
	old := k.config
	k.mu.RUnlock()
	return normalizeEmbedding(c, old)
}
func (k *Retrieval) TestConnection(ctx context.Context, c EmbeddingConfig) (EmbeddingConfig, error) {
	c, err := k.prepare(c)
	if err != nil {
		return c, err
	}
	if c.Mode == "none" {
		return c.sanitized(), nil
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var v []float64
	if c.Mode == "local" {
		path, opts, e := k.components.Paths(true)
		if e != nil {
			return c.sanitized(), e
		}
		opts.Dimensions = 384
		opts.Database = filepath.Join(k.dir, "test-model.db")
		client := semantic.NewClient(path, opts)
		defer client.Close()
		res, e := client.Call(ctx, semantic.Request{Op: "embed", Text: "query: 对象归属与权限验证"})
		v, err = res.Vector, e
	} else {
		v, err = nativeEmbedder(c).GetEmbedding(ctx, "ScopeForge embedding connection test")
	}
	if err != nil {
		return c.sanitized(), errors.New("Embedding 连接失败，请检查地址、凭据和模型服务")
	}
	if len(v) < 1 || len(v) > 8192 || !validEmbedding(v) {
		return c.sanitized(), errors.New("模型返回的向量维度无效")
	}
	c.Dimensions = len(v)

	return c.sanitized(), nil
}
func (k *Retrieval) Configure(c EmbeddingConfig) error {
	c, err := k.prepare(c)
	if err != nil {
		return err
	}
	if c.Mode != "none" {
		if k.components.Status().Installing {
			return errors.New("组件安装中，请等待安装完成后启用")
		}
		if _, _, err := k.components.Paths(c.Mode == "local"); err != nil {
			return err
		}
		if c.Mode == "local" {
			inst, _ := k.components.Installed()
			c.Version = inst.Manifest.ModelVersion
		}
	}
	if c.Mode != "none" && c.Dimensions == 0 {
		checked, err := k.TestConnection(k.ctx, c)
		if err != nil {
			return err
		}
		c.Dimensions = checked.Dimensions
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	raw, _ := json.Marshal(c)
	if err := k.pg.SetSetting(embeddingSetting, string(raw)); err != nil {
		return err
	}
	k.config = c
	k.semantic.configure(c)
	k.signal()
	return nil
}
func (k *Retrieval) embedding() embedder.Embedder {
	k.mu.RLock()
	c := k.config
	k.mu.RUnlock()
	var e embedder.Embedder = nativeEmbedder(c)
	if c.Mode == "local" {
		e = localEmbedder{index: k.semantic}
	}
	return &cachedEmbedder{Embedder: e, db: k.cache, namespace: c.modelFingerprint()}
}
