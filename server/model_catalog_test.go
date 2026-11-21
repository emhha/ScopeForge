package server

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

// API-specific capability overrides must not leak into another API of the same
// model, and unknown protocols or non-chat models cannot be configured as agents.
func TestCatalogAPIOverridesAndLiveRefresh(t *testing.T) {
	var revision atomic.Int32
	revision.Store(1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cache-Control") != "no-cache" {
			t.Error("catalog request leaked credentials or enabled cache")
		}
		fmt.Fprintf(w, `{"fixture":{"name":"Fixture","models":{"new":{"name":"New","type":"chat","capabilities":{"tools":{"supported":true},"reasoning":{"supported":true,"effort":["none","high"]}},"limit":{"context":1048576,"output":16384},"recommended_api":"responses","api":{"responses":{"base_url":"https://example.com/v%d","capabilities":{"reasoning":{"effort":["high"]}}},"chat-completions":{"base_url":"https://example.com/v1"},"unsupported":{"base_url":"https://example.com"}}},"embed":{"type":"embedding","limit":{"context":8192},"capabilities":{"tools":{"supported":true}}},"no-tools":{"type":"chat","limit":{"context":8192}}}}}`, revision.Load())
	}))
	defer upstream.Close()
	first, err := fetchModelCatalog(t.Context(), upstream.Client(), upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Providers) != 1 || len(first.Providers[0].Models) != 1 {
		t.Fatalf("unexpected providers: %+v", first.Providers)
	}
	m := first.Providers[0].Models[0]
	if m.ID != "new" || m.Context != 1048576 || len(m.APIs) != 2 || m.APIs[0].Format != "openai-responses" || m.APIs[0].Reasoning.Effort[0] != "high" || m.APIs[1].Reasoning.Effort[0] != "none" {
		t.Fatalf("bad capabilities: %+v", m)
	}
	revision.Store(2)
	second, err := fetchModelCatalog(t.Context(), upstream.Client(), upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	if second.Providers[0].Models[0].APIs[0].BaseURL == m.APIs[0].BaseURL {
		t.Fatal("catalog was cached instead of fetching a new revision")
	}
}
func TestCatalogRejectsMalformedAndUnavailableSources(t *testing.T) {
	for _, body := range []string{`{`, `{}`, `{"bad":{"models":{"x":{"type":"chat","limit":{"context":100000},"capabilities":{"tools":{"supported":true}},"api":{"messages":{"base_url":"https://secret@example.com/v1"}}}}}}`} {
		if _, err := parseModelCatalog([]byte(body)); err == nil {
			t.Fatal("accepted invalid catalog", body)
		}
	}
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer source.Close()
	if _, err := fetchModelCatalog(t.Context(), source.Client(), source.URL); err == nil {
		t.Fatal("unavailable catalog accepted")
	}
}

func TestCatalogProxyPrecedence(t *testing.T) {
	// Public metadata must honor the launch environment rather than force direct
	// access; no target request or application proxy setting is changed here.
	transport, err := modelCatalogTransport("")
	if err != nil {
		t.Fatal(err)
	}
	defer transport.CloseIdleConnections()
	if reflect.ValueOf(transport.Proxy).Pointer() != reflect.ValueOf(http.ProxyFromEnvironment).Pointer() {
		t.Fatal("catalog ignores environment proxy")
	}
	var seen atomic.Bool
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Store(true)
		if r.URL.Host != "catalog.invalid" {
			t.Errorf("unexpected destination: %s", r.URL.Host)
		}
		w.WriteHeader(503)
	}))
	defer proxy.Close()
	explicit, err := modelCatalogTransport(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer explicit.CloseIdleConnections()
	if _, err = fetchModelCatalog(t.Context(), &http.Client{Transport: explicit}, "http://catalog.invalid/api.json"); err == nil {
		t.Fatal("proxy fixture 503 ignored")
	}
	if !seen.Load() {
		t.Fatal("explicit application proxy did not take precedence")
	}
	if _, err = modelCatalogTransport("not-a-proxy"); err == nil {
		t.Fatal("invalid proxy accepted")
	}
}

func TestCatalogFailureExplainsTimeoutAndHTTPStatus(t *testing.T) {
	if got := modelCatalogError(context.DeadlineExceeded); !strings.Contains(got, "超时") || !strings.Contains(got, "后端") {
		t.Fatal(got)
	}
	if got := modelCatalogError(fmt.Errorf("模型目录服务返回 HTTP 403")); !strings.Contains(got, "HTTP 403") {
		t.Fatal(got)
	}
}

type catalogTestTransport func(*http.Request) (*http.Response, error)

func (f catalogTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestCatalogRetriesTransientHandshakeTimeoutOnce(t *testing.T) {
	for _, recover := range []bool{false, true} {
		t.Run(fmt.Sprint(recover), func(t *testing.T) {
			calls := 0
			client := &http.Client{Transport: catalogTestTransport(func(*http.Request) (*http.Response, error) {
				calls++
				if calls == 1 || !recover {
					return nil, &net.DNSError{Err: "fixture handshake timeout", IsTimeout: true}
				}
				body := `{"fixture":{"models":{"chat":{"type":"chat","limit":{"context":128000},"capabilities":{"tools":{"supported":true}},"api":{"messages":{"base_url":"https://example.com"}}}}}}`
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			_, err := fetchModelCatalog(t.Context(), client, "https://catalog.invalid/api.json")
			if calls != 2 || (recover && err != nil) || (!recover && err == nil) {
				t.Fatalf("calls=%d recovery=%v err=%v", calls, recover, err)
			}
			if !recover && strings.Contains(modelCatalogError(err), "120 秒") {
				t.Fatal("handshake timeout incorrectly reported as whole-download timeout")
			}
		})
	}
}

func TestBundledCatalogLoadsWithoutManagerOrNetwork(t *testing.T) {
	// A default request must work with no Manager, database, proxy or HTTP client.
	// The online path needs the Manager, so this also guards against accidental
	// network access being reintroduced when the model form opens.
	s := &Server{}
	w := httptest.NewRecorder()
	s.llmModelCatalog(w, httptest.NewRequest(http.MethodGet, "/api/llm/catalog", nil))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var catalog modelCatalog
	if err := json.Unmarshal(w.Body.Bytes(), &catalog); err != nil {
		t.Fatal(err)
	}
	if catalog.Origin != "bundled" || catalog.Source != modelCatalogURL || catalog.UpdatedAt.IsZero() || len(catalog.Providers) == 0 {
		t.Fatalf("invalid bundled catalog: origin=%s providers=%d", catalog.Origin, len(catalog.Providers))
	}
	found := map[string]bool{}
	for _, p := range catalog.Providers {
		if len(p.Models) > 0 {
			found[p.ID] = true
		}
	}
	if !found["openai"] || !found["anthropic"] {
		t.Fatal("bundled catalog lost core providers")
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("catalog response cache policy changed")
	}
}

func TestBundledCatalogSnapshotIntegrity(t *testing.T) {
	var metadata struct {
		SHA256 string `json:"sha256"`
	}
	if err := json.Unmarshal(bundledModelCatalogMetadata, &metadata); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(bundledModelCatalogJSON)
	if fmt.Sprintf("%x", sum) != metadata.SHA256 {
		t.Fatal("snapshot metadata is stale; run scripts/update-model-catalog.py")
	}
}
