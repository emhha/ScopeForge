package learning

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/emhha/scopeforge/internal/semantic"
	"github.com/google/uuid"
	"github.com/openai/openai-go/option"
	"math"
	"path/filepath"

	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"trpc.group/trpc-go/trpc-agent-go/knowledge/embedder"
	"trpc.group/trpc-go/trpc-agent-go/knowledge/embedder/openai"
	"trpc.group/trpc-go/trpc-agent-go/memory"
)

// EmbeddingConfig keeps model execution independent from storage and chat LLMs.
// Mode=none performs no model downloads or embedding requests.
type EmbeddingConfig struct {
	namespace  string
	Mode       string `json:"mode"`
	BaseURL    string `json:"base_url"`
	Model      string `json:"model"`
	Version    string `json:"version"`
	Dimensions int    `json:"dimensions"`
	APIKey     string `json:"api_key,omitempty"`
	Configured bool   `json:"configured"`
}

func (c EmbeddingConfig) fingerprint() string {
	b, _ := json.Marshal([]any{c.namespace, c.modelFingerprint(), semantic.ProtocolVersion})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])[:20]
}
func (c EmbeddingConfig) modelFingerprint() string {
	b, _ := json.Marshal([]any{c.Mode, c.BaseURL, c.Model, c.Version, c.Dimensions, "e5-prefix-v1"})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])[:20]
}
func validEmbedding(vector []float64) bool {
	var norm float64
	for _, v := range vector {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
		norm += v * v
	}
	return norm > 0 && !math.IsInf(norm, 0)
}
func (c EmbeddingConfig) enabled() bool {
	return c.Mode != "none" && c.Model != "" && c.Dimensions > 0
}
func (c EmbeddingConfig) sanitized() EmbeddingConfig {
	c.APIKey = ""
	c.Configured = c.Mode != "none" && c.Model != ""
	return c
}
func validEndpoint(value string) bool {
	u, err := url.Parse(value)
	return err == nil && u.Host != "" && (u.Scheme == "http" || u.Scheme == "https") && u.User == nil && u.RawQuery == "" && u.Fragment == ""
}
func normalizeEmbedding(c, old EmbeddingConfig) (EmbeddingConfig, error) {
	c.namespace = old.namespace
	if c.Mode == "" {
		if c.Model == "" {
			c.Mode = "none"
		} else {
			c.Mode = "remote"
		}
	}
	if c.Mode != "none" && c.Mode != "local" && c.Mode != "remote" {
		return c, errors.New("请选择关键词、本地模型或在线 Embedding 模式")
	}
	c.BaseURL = strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	c.Model = strings.TrimSpace(c.Model)
	c.Version = strings.TrimSpace(c.Version)
	if c.Mode == "local" {
		c.BaseURL = ""
		c.APIKey = ""
		c.Model = "multilingual-e5-small"
		c.Dimensions = 384
	}
	if c.Mode == "remote" && (!validEndpoint(c.BaseURL) || c.Model == "") {
		return c, errors.New("请填写有效的 Embedding API 地址和模型")
	}
	if c.Dimensions < 0 || c.Dimensions > 8192 {
		return c, errors.New("向量维度必须为 1–8192，或通过测试连接自动检测")
	}
	// Never forward an existing secret to a newly configured endpoint.
	if c.Mode != "local" && c.APIKey == "" && c.BaseURL == old.BaseURL {
		c.APIKey = old.APIKey
	}
	c.Configured = false
	return c, nil
}
func nativeEmbedder(c EmbeddingConfig) embedder.Embedder {
	// Do not inherit the chat provider's OPENAI_API_KEY from the process when
	// the explicitly configured local embedding endpoint needs no authentication.
	key := defaultText(c.APIKey, "scopeforge-no-api-key")
	opts := []openai.Option{openai.WithBaseURL(c.BaseURL), openai.WithAPIKey(key), openai.WithModel(c.Model), openai.WithMaxRetries(0), openai.WithRequestOptions(option.WithHeader("OpenAI-Organization", ""), option.WithHeader("OpenAI-Project", ""))}
	// Expected dimension is validated by the host. Let fixed-dimension local
	// models choose their native output instead of requesting dimensionality reduction.
	return declaredEmbedder{Embedder: openai.New(opts...), dimensions: c.Dimensions}
}

type declaredEmbedder struct {
	embedder.Embedder
	dimensions int
}

func (e declaredEmbedder) GetDimensions() int { return e.dimensions }

type SemanticStatus struct {
	Mode       string `json:"mode"`
	Ready      bool   `json:"ready"`
	Message    string `json:"message"`
	Generation string `json:"generation"`
}

// SemanticIndex coordinates a rebuildable local SQLiteVec index. All vector SQL
// and ranking live in the native tRPC adapter inside the optional component.
type SemanticIndex struct {
	retryAt    time.Time
	failures   int
	connectMu  sync.Mutex
	syncMu     sync.Mutex
	mu         sync.RWMutex
	config     EmbeddingConfig
	service    *semantic.Client
	status     SemanticStatus
	generation uint64
	installer  *semantic.Installer
	dir        string
	embedder   embedder.Embedder
}

func newSemanticIndex(dir string, installer *semantic.Installer) *SemanticIndex {
	return &SemanticIndex{dir: dir, installer: installer, status: SemanticStatus{Mode: "keyword", Message: "使用关键词检索"}}
}
func defaultText(v, d string) string {
	if v == "" {
		return d
	}
	return v
}
func (s *SemanticIndex) configure(c EmbeddingConfig) {
	s.mu.Lock()
	old := s.service
	s.service = nil
	s.config = c
	s.generation++
	s.retryAt = time.Time{}
	s.failures = 0
	s.status = SemanticStatus{Mode: "keyword", Generation: c.fingerprint(), Message: "使用关键词检索"}
	if c.enabled() {
		s.status.Message = "语义组件将在需要时加载，关键词检索可用"
	}
	s.mu.Unlock()
	if old != nil {
		old.Close()
	}
}
func (s *SemanticIndex) Status() SemanticStatus { s.mu.RLock(); defer s.mu.RUnlock(); return s.status }
func (s *SemanticIndex) Close() error {
	s.mu.Lock()
	old := s.service
	s.service = nil
	s.failures++
	s.retryAt = time.Now().Add(time.Duration(1<<min(s.failures-1, 4)) * time.Minute)
	s.status.Ready = false
	s.status.Mode = "keyword"
	s.mu.Unlock()
	if old != nil {
		return old.Close()
	}
	return nil
}
func (s *SemanticIndex) ensure(e embedder.Embedder) {
	s.connectMu.Lock()
	defer s.connectMu.Unlock()
	s.mu.RLock()
	c, g, ready := s.config, s.generation, s.service != nil || time.Now().Before(s.retryAt)
	s.mu.RUnlock()
	if ready || !c.enabled() {
		return
	}
	path, opts, err := s.installer.Paths(c.Mode == "local")
	s.mu.Lock()
	defer s.mu.Unlock()
	if g != s.generation {
		return
	}
	if err != nil {
		s.status.Message = err.Error() + "；使用关键词检索"
		return
	}
	opts.Database = filepath.Join(s.dir, "vectors-"+c.fingerprint()+".db")
	opts.Dimensions = c.Dimensions
	// Creating a client does not start the native process or load the model.
	s.service = semantic.NewClient(path, opts)
	s.embedder = e
	s.status = SemanticStatus{Mode: "hybrid", Ready: true, Generation: c.fingerprint() + ":" + uuid.NewString(), Message: "语义组件已启用，索引将在后台同步"}
}
func (s *SemanticIndex) Runtime() (*semantic.Client, string) { return s.remote() }

func (s *SemanticIndex) remote() (*semantic.Client, string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.service, s.status.Generation
}
func (s *SemanticIndex) failed(svc *semantic.Client) {
	s.mu.Lock()
	if s.service != svc {
		s.mu.Unlock()
		return
	}
	s.service = nil
	s.failures++
	s.retryAt = time.Now().Add(time.Duration(1<<min(s.failures-1, 4)) * time.Minute)
	s.status.Ready = false
	s.status.Mode = "keyword"
	s.status.Message = "语义组件不可用，已回退关键词检索"
	s.mu.Unlock()
	svc.Close()
}
func (s *SemanticIndex) localEmbedding(ctx context.Context, text string) ([]float64, error) {
	svc, _ := s.remote()
	if svc == nil {
		return nil, errors.New("语义组件尚未就绪")
	}
	r, err := svc.Call(ctx, semantic.Request{Op: "embed", Text: text})
	return r.Vector, err
}
func (s *SemanticIndex) Sync(ctx context.Context, key memory.UserKey, entries []*memory.Entry) (runErr error) {
	s.syncMu.Lock()
	defer s.syncMu.Unlock()
	svc, _ := s.remote()
	if svc == nil {
		return errors.New("semantic component unavailable")
	}
	defer func() {
		if runErr != nil {
			s.failed(svc)
		}
	}()
	existing, err := svc.Call(ctx, semantic.Request{Op: "list", Scope: key})
	if err != nil {
		return err
	}
	identity := func(e *memory.Entry) string {
		if e == nil || e.Memory == nil {
			return ""
		}
		if key == documentKey {
			return "chunk:" + e.Memory.Location
		}
		return e.ID
	}
	wanted := map[string]*memory.Entry{}
	for _, e := range entries {
		if id := identity(e); id != "" {
			wanted[id] = e
		}
	}
	current := map[string]*memory.Entry{}
	for _, e := range existing.Entries {
		id := identity(e)
		if wanted[id] == nil {
			if _, err := svc.Call(ctx, semantic.Request{Op: "delete", Scope: key, ID: e.ID}); err != nil {
				return err
			}
		} else {
			current[id] = e
		}
	}
	s.mu.RLock()
	e := s.embedder
	c := s.config
	s.mu.RUnlock()
	for _, entry := range entries {
		id := identity(entry)
		if id == "" {
			continue
		}
		if previous := current[id]; previous != nil && previous.Memory != nil {
			if sameIndexedMemory(previous.Memory, entry.Memory) {
				continue
			}
		}
		text := entry.Memory.Memory
		if c.Mode == "local" {
			text = "passage: " + text
		}
		vector, err := e.GetEmbedding(ctx, text)
		if err != nil {
			return err
		}
		if len(vector) != c.Dimensions || !validEmbedding(vector) {
			return errors.New("invalid embedding")
		}
		copy := *entry
		copy.ID = id
		if _, err := svc.Call(ctx, semantic.Request{Op: "put", Scope: key, Entry: &copy, Vector: vector}); err != nil {
			return err
		}
	}
	return nil
}
func (s *SemanticIndex) Search(ctx context.Context, key memory.UserKey, q string, opts ...memory.SearchOption) ([]*memory.Entry, error) {
	svc, _ := s.remote()
	if svc == nil {
		return nil, errors.New("semantic component unavailable")
	}
	timeout := 4 * time.Second
	s.mu.RLock()
	if s.config.Mode == "local" {
		timeout = 15 * time.Second
	}
	s.mu.RUnlock()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	original := q
	search := memory.ResolveSearchOptions(q, opts)
	s.mu.RLock()
	e, c := s.embedder, s.config
	s.mu.RUnlock()
	if c.Mode == "local" {
		q = "query: " + q
	}
	vector, err := e.GetEmbedding(ctx, q)
	if err == nil && !validEmbedding(vector) {
		err = errors.New("invalid query embedding")
	}
	var res semantic.Response
	if err == nil {
		res, err = svc.Call(ctx, semantic.Request{Op: "search", Scope: key, Vector: vector, Text: original, Search: &search})
	}
	if err != nil {
		s.failed(svc)
	}
	return res.Entries, err
}

type localEmbedder struct{ index *SemanticIndex }

func (e localEmbedder) GetDimensions() int { return 384 }
func (e localEmbedder) GetEmbedding(ctx context.Context, t string) ([]float64, error) {
	return e.index.localEmbedding(ctx, t)
}
func (e localEmbedder) GetEmbeddingWithUsage(ctx context.Context, t string) ([]float64, map[string]any, error) {
	v, err := e.GetEmbedding(ctx, t)
	return v, nil, err
}

// SQL and JSON backends may represent empty slices and time zones differently.
// Compare semantic fields, avoiding unnecessary updates after a reconnect.
func sameIndexedMemory(a, b *memory.Memory) bool {
	if a.Memory != b.Memory || a.Kind != b.Kind || a.Location != b.Location || !slices.Equal(a.Topics, b.Topics) || !slices.Equal(a.Participants, b.Participants) {
		return false
	}
	if a.EventTime == nil || b.EventTime == nil {
		return a.EventTime == nil && b.EventTime == nil
	}
	return a.EventTime.Equal(*b.EventTime)
}
