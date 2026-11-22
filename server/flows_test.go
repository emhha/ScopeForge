package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/emhha/scopeforge/agent"
	"github.com/emhha/scopeforge/db"
	"github.com/emhha/scopeforge/internal/flowproto"
	"github.com/emhha/scopeforge/toolkit"
	actool "trpc.group/trpc-go/trpc-agent-go/tool"
)

func TestFlowSSEDeliversBeforeRunFinishes(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	s := New(ctx, m, t.TempDir(), m.dir, t.TempDir())
	raw, _ := json.Marshal(flowproto.ExampleDefinition())
	id, err := m.pg.SaveFlow(0, "assessment", raw, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.pg.CreateFlowRun("live", id, raw, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	s.flowCancels.Store("live", cancel)
	if err := m.pg.AppendFlowEvent("live", "graph.node.start", "node_begin", ""); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	s.registerFlowRoutes(mux)
	server := httptest.NewServer(mux)
	defer server.Close()
	request, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"/api/flow-runs/live/events", nil)
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	scanner := bufio.NewScanner(response.Body)
	sawStart := false
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), "data:") {
			sawStart = strings.Contains(scanner.Text(), "graph.node.start")
			break
		}
	}
	if !sawStart {
		t.Fatalf("no live node event: %v", scanner.Err())
	}
	run, err := m.pg.GetFlowRun("live")
	if err != nil || run.Status != "running" {
		t.Fatalf("event was not delivered while running: %v %v", run, err)
	}
	if err := m.pg.AppendFlowEvent("live", "graph.node.complete", "node_begin", ""); err != nil {
		t.Fatal(err)
	}
	if err := m.pg.FinishFlowRun("live", "completed", `{}`, ""); err != nil {
		t.Fatal(err)
	}
	sawComplete, sawDone := false, false
	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, "graph.node.complete") {
			sawComplete = true
		}
		if line == "event: done" {
			sawDone = true
			break
		}
	}
	if !sawComplete || !sawDone {
		t.Fatalf("missing final node event or terminal frame: complete=%v done=%v err=%v", sawComplete, sawDone, scanner.Err())
	}
}

func TestFlowEditorPublishRunAndReplay(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	s := New(ctx, m, t.TempDir(), m.dir, t.TempDir())
	token, err := signJWT(s.jwtKey)
	if err != nil {
		t.Fatal(err)
	}
	handler := s.Handler()
	do := func(method, path string, body any) *httptest.ResponseRecorder {
		encoded, _ := json.Marshal(body)
		request := httptest.NewRequest(method, path, strings.NewReader(string(encoded)))
		request.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, request)
		return rec
	}
	if rec := do("GET", "/api/flows", nil); rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	rec := do("POST", "/api/flows", map[string]any{"definition": flowproto.ExampleDefinition()})
	if rec.Code != 200 {
		t.Fatalf("save: %d %s", rec.Code, rec.Body.String())
	}
	var saved db.Flow
	if err := json.Unmarshal(rec.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	if len(s.flowTools(ctx, "auto", nil)) != 0 {
		t.Fatal("draft exposed as tool")
	}
	path := fmt.Sprintf("/api/flows/%d", saved.ID)
	rec = do("POST", path+"/publish", map[string]any{"revision": saved.Revision, "agents": []string{"auto"}})
	if rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	tools := s.flowTools(ctx, "auto", nil)
	if len(tools) != 1 || tools[0].Declaration().Name != "flow_assessment" {
		t.Fatalf("published tool missing: %+v", tools)
	}
	if len(s.flowTools(ctx, "worker", nil)) != 0 {
		t.Fatal("published tool leaked to unbound worker")
	}
	result, err := tools[0].Call(ctx, []byte(`{"target":"https://example.test"}`))
	if err != nil || !strings.Contains(toolkit.TextOf(result), "accepted") {
		t.Fatalf("published result: %v %v", result, err)
	}

	visible, _, cleanup := agent.AugmentTools(ctx, "auto", nil)
	defer cleanup()
	found := false
	for _, item := range visible {
		if item.Declaration().Name == "flow_assessment" {
			found = true
			if _, ok := item.(actool.StreamableTool); !ok {
				t.Fatal("tool assembly erased native Flow streaming")
			}
		}
	}
	if !found {
		t.Fatal("published flow missing from actual Agent tool assembly")
	}
	// Editing a draft leaves the published snapshot unchanged.
	draft := flowproto.ExampleDefinition()
	draft.Nodes[3].Output = map[string]any{"decision": "draft-only"}
	rec = do("PUT", path, map[string]any{"definition": draft, "revision": saved.Revision})
	if rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	rec = do("POST", path+"/publish", map[string]any{"revision": saved.Revision, "agents": []string{"auto"}})
	if rec.Code != 409 {
		t.Fatal("stale publication was accepted")
	}
	tools = s.flowTools(ctx, "auto", nil)
	result, err = tools[0].Call(ctx, []byte(`{"target":"https://example.test"}`))
	if err != nil || !strings.Contains(toolkit.TextOf(result), "accepted") {
		t.Fatalf("draft modified publication: %v %v", result, err)
	}
	rec = do("POST", path+"/run", map[string]any{"input": map[string]any{}})
	if rec.Code != 400 {
		t.Fatalf("missing input accepted: %d", rec.Code)
	}
	rec = do("POST", path+"/run", map[string]any{"input": map[string]any{"target": "http://example.test"}})
	if rec.Code != 202 {
		t.Fatal(rec.Body.String())
	}
	var started struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &started)
	deadline := time.Now().Add(5 * time.Second)
	var run *db.FlowRun
	for time.Now().Before(deadline) {
		run, err = m.pg.GetFlowRun(started.ID)
		if err != nil {
			t.Fatal(err)
		}
		if run.Status != "running" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if run.Status != "completed" || !strings.Contains(run.Result, "rejected") {
		t.Fatalf("run=%+v", run)
	}
	events, err := m.pg.FlowEvents(started.ID, 0)
	if err != nil || len(events) == 0 {
		t.Fatalf("events=%v err=%v", events, err)
	}
	rec = do("GET", "/api/flow-runs/"+started.ID+"/events", nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "event: done") || !strings.Contains(rec.Body.String(), "graph.node.start") {
		t.Fatalf("SSE=%d %s", rec.Code, rec.Body.String())
	}
	if rec = do("GET", "/api/flow-runs/missing/events", nil); rec.Code != http.StatusNotFound {
		t.Fatal("unknown run was exposed")
	}
}

func TestFlowCannotPublishUnavailableDependenciesToAgent(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	s := New(ctx, m, t.TempDir(), m.dir, t.TempDir())
	def := flowproto.ExampleDefinition()
	def.Nodes[1].Tool = "http_request"
	def.Nodes[1].Arguments = map[string]any{"url": "https://example.test"}
	raw, _ := json.Marshal(def)
	id, err := m.pg.SaveFlow(0, def.Name, raw, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.pg.PublishFlow(id, 1, raw, []string{"auto"}); err != nil {
		t.Fatal(err)
	}
	// No permitted HTTP tool in this caller's pool: compilation must exclude it.
	// Temporarily remove the global resolver's injection, leaving an empty capability set.
	previous := agent.ToolResolve
	agent.ToolResolve = nil
	defer func() { agent.ToolResolve = previous }()
	if len(s.flowTools(ctx, "auto", nil)) != 0 {
		t.Fatal("flow bypassed missing dependency permission")
	}
}
