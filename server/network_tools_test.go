package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/emhha/scopeforge/agent"
	"github.com/emhha/scopeforge/db"
	"github.com/emhha/scopeforge/internal/modeltest"
	"github.com/emhha/scopeforge/internal/nettest"
	"github.com/emhha/scopeforge/llmrec"
	"github.com/emhha/scopeforge/toolkit"
	"trpc.group/trpc-go/trpc-agent-go/model"
)

func TestNetworkToolsAndMappingConfigThroughNativeRunner(t *testing.T) {
	var targetCalls atomic.Int32
	target := nettest.Server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetCalls.Add(1)
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "native-http-result")
	}), false)
	m, err := NewManager(t.TempDir(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	s := New(ctx, m, t.TempDir(), m.dir, t.TempDir())
	if err := m.SetTrafficEnabled(true); err != nil {
		t.Fatal(err)
	}
	handler := s.Handler()
	token, err := signJWT(s.jwtKey)
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, path string, body any, authenticate bool) (int, map[string]any) {
		t.Helper()
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(method, path, bytes.NewReader(raw))
		if authenticate {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s status=%d body=%s", path, rec.Code, rec.Body.String())
		}
		return rec.Code, out
	}
	if code, _ := request("GET", "/api/discovery/sources", nil, false); code != 401 {
		t.Fatalf("anonymous source config status=%d", code)
	}
	const secret = "source-api-fixture-secret"
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("key") != secret {
			t.Error("stored FOFA credential not passed to SDK")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"error":false,"size":1,"results":[["https://asset.example.test","203.0.113.9","443","https","example.test"]]}`)
	}))
	defer source.Close()
	if code, out := request("PUT", "/api/discovery/sources/fofa", map[string]any{"enabled": true, "key": secret, "endpoint": source.URL}, true); code != 200 {
		t.Fatalf("save source status=%d body=%v", code, out)
	}
	if code, out := request("PUT", "/api/discovery/sources/fofa", map[string]any{"timeout_seconds": 45}, true); code != 200 || out["key_set"] != true {
		t.Fatalf("partial config save status=%d body=%v", code, out)
	}
	if code, out := request("GET", "/api/discovery/sources", nil, true); code != 200 {
		t.Fatalf("sources status=%d", code)
	} else {
		raw, _ := json.Marshal(out)
		if strings.Contains(string(raw), secret) {
			t.Fatal("configuration API exposed API key")
		}
	}
	tools := s.networkTools()
	for _, item := range tools {
		switch item.Declaration().Name {
		case "list_recon_sources":
			out, err := item.Call(t.Context(), []byte(`{}`))
			if err != nil || toolkit.Failed(out) || strings.Contains(toolkit.TextOf(out), secret) || strings.Contains(toolkit.TextOf(out), source.URL) {
				t.Fatalf("source tool=%v err=%v", out, err)
			}
		case "search_mapping":
			out, err := item.Call(t.Context(), []byte(`{"provider":"fofa","query":"example.test","limit":1}`))
			if err != nil || toolkit.Failed(out) || !strings.Contains(toolkit.TextOf(out), "asset.example.test") || strings.Contains(toolkit.TextOf(out), secret) {
				t.Fatalf("mapping tool=%v err=%v", out, err)
			}
		}
	}
	mounted, _, cleanup := agent.AugmentTools(t.Context(), "worker", nil)
	cleanup()
	for _, name := range []string{"http_request", "search_mapping", "list_recon_sources"} {
		found := false
		for _, item := range mounted {
			found = found || item.Declaration().Name == name
		}
		if !found {
			t.Errorf("worker missing native tool %s", name)
		}
	}
	turns := 0
	provider := modeltest.Func(func(_ context.Context, req *model.Request) (<-chan *model.Response, error) {
		turns++
		if turns == 1 {
			guidance := false
			for _, message := range req.Messages {
				if message.Role == model.RoleSystem && strings.Contains(message.Content, "目标 HTTP 请求") && strings.Contains(message.Content, "http_request") {
					guidance = true
				}
			}
			if !guidance {
				t.Error("native model did not receive conditional HTTP guidance")
			}
			for _, item := range req.Tools {
				if item.Declaration().Name == "WebFetch" {
					t.Error("duplicate WebFetch bypass advertised alongside http_request")
				}
			}
			args, _ := json.Marshal(map[string]string{"url": target.URL + "/probe"})
			return modeltest.Responses(&model.Response{Choices: []model.Choice{{Message: model.Message{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{{ID: "request-1", Type: "function", Function: model.FunctionDefinitionParam{Name: "http_request", Arguments: args}}}}}}}), nil
		}
		found := false
		for _, message := range req.Messages {
			found = found || (message.Role == model.RoleTool && strings.Contains(message.Content, "native-http-result") && strings.Contains(message.Content, "exchange_ids"))
		}
		if !found {
			t.Error("native tool result was not included in context")
		}
		return modeltest.Responses(modeltest.Final("done")), nil
	})
	var modelAudits []db.Activity
	recordedProvider := llmrec.Wrap(provider, nil, "fixture-model", "HTTP fixture profile", "", "", func() bool { return false })
	chat := agent.NewChatAgent(recordedProvider, "fixture", m.dir, m.sessions, 100000)
	text, err := chat.Chat(t.Context(), "worker", "conv-network-tool", "request fixture", 3, 0, false, func(a db.Activity) {
		if a.Kind == "model_call" {
			modelAudits = append(modelAudits, a)
		}
	})
	if err != nil || text != "done" || targetCalls.Load() != 1 {
		t.Fatalf("native run text=%q err=%v target calls=%d", text, err, targetCalls.Load())
	}
	if len(modelAudits) != 2 {
		t.Fatalf("actual model calls missing from native conversation: %+v", modelAudits)
	}
	for _, activity := range modelAudits {
		if !strings.Contains(string(activity.Metadata), "fixture-model") || strings.Contains(string(activity.Metadata), "目标 HTTP 请求") || !strings.Contains(activity.Detail, "目标 HTTP 请求") {
			t.Fatalf("model call metadata/detail contract: %+v", activity)
		}
	}
	if count, err := m.traffic.Count(); err != nil || count != 1 {
		t.Fatalf("recorded=%d err=%v", count, err)
	}
	targetURL, _ := url.Parse(target.URL)
	if _, err := m.pg.CreateAssetInterceptRule("exact_ip", targetURL.Hostname(), "fixture block", true); err != nil {
		t.Fatal(err)
	}
	for _, item := range tools {
		if item.Declaration().Name == "http_request" {
			raw, _ := json.Marshal(map[string]string{"url": target.URL})
			out, err := item.Call(t.Context(), raw)
			if err != nil || !toolkit.Failed(out) {
				t.Fatalf("blocked HTTP output=%v err=%v", out, err)
			}
		}
	}
	if targetCalls.Load() != 1 {
		t.Fatal("blocked request reached the target")
	}
	if code, _ := request("PUT", "/api/discovery/sources/fofa", map[string]any{"key": "", "enabled": false}, true); code != 200 {
		t.Fatalf("clear source status=%d", code)
	}
	if code, out := request("GET", "/api/discovery/sources", nil, true); code == 200 {
		found := false
		for _, entry := range out["sources"].([]any) {
			source := entry.(map[string]any)
			if source["id"] == "fofa" {
				found = source["key_set"] == false && source["enabled"] == false
			}
		}
		if !found {
			t.Fatal("cleared source remains configured")
		}
	} else {
		t.Fatal(fmt.Sprintf("sources status=%d", code))
	}
}
