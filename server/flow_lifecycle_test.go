package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/emhha/scopeforge/agent"
	"github.com/emhha/scopeforge/db"
	"github.com/emhha/scopeforge/internal/flowproto"
	"github.com/emhha/scopeforge/internal/modeltest"
	"github.com/emhha/scopeforge/internal/nettest"
	"github.com/emhha/scopeforge/toolkit"
	nativeagent "trpc.group/trpc-go/trpc-agent-go/agent"
	"trpc.group/trpc-go/trpc-agent-go/agent/llmagent"
	"trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/runner"
	actool "trpc.group/trpc-go/trpc-agent-go/tool"
)

func workflowTestServer(t *testing.T) (*Server, func(string, string, any) *httptest.ResponseRecorder) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	s := New(ctx, m, t.TempDir(), m.dir, t.TempDir())
	token, err := signJWT(s.jwtKey)
	if err != nil {
		t.Fatal(err)
	}
	handler := s.Handler()
	return s, func(method, path string, body any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(method, path, strings.NewReader(string(raw)))
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
}

func TestWorkflowAgentGeneratesTemporaryDefinitionWithLiveEvents(t *testing.T) {
	s, _ := workflowTestServer(t)
	items, _, close := agent.AugmentTools(t.Context(), "auto", nil)
	defer close()
	var entry actool.CallableTool
	for _, item := range items {
		if item.Declaration().Name == "workflow_run" {
			entry = item
		}
	}
	if entry == nil {
		t.Fatal("workflow_run not bound to actual Agent tool assembly")
	}
	fixture := toolkit.New("invocation_only_fixture", "native invocation-scoped fixture", objSchema(map[string]any{"target": strParam("fixture input")}, "target"), func(context.Context, json.RawMessage) (toolkit.Output, error) { return toolkit.Text("allowed"), nil })
	definition := flowproto.ExampleDefinition()
	definition.Nodes[1].Tool = fixture.Declaration().Name
	raw, _ := json.Marshal(map[string]any{"definition": definition, "input": map[string]any{"target": "https://example.test"}})
	calls := 0
	mdl := modeltest.Func(func(_ context.Context, req *model.Request) (<-chan *model.Response, error) {
		calls++
		if calls == 1 {
			return modeltest.Responses(&model.Response{Choices: []model.Choice{{Message: model.Message{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{{ID: "inline", Type: "function", Function: model.FunctionDefinitionParam{Name: "workflow_run", Arguments: raw}}}}}}}), nil
		}
		found := false
		for _, message := range req.Messages {
			if message.Role == model.RoleTool && strings.Contains(message.Content, "accepted") && strings.Contains(message.Content, "run_id") {
				found = true
			}
		}
		if !found {
			return nil, fmt.Errorf("parent model did not receive inline workflow output")
		}
		return modeltest.Responses(modeltest.Final("temporary complete")), nil
	})
	parent := llmagent.New("parent", llmagent.WithModel(mdl), llmagent.WithTools([]actool.Tool{entry, fixture}), llmagent.WithMaxLLMCalls(2))
	r := runner.NewRunner("workflow", parent)
	defer r.Close()
	events, err := r.Run(t.Context(), "operator", "inline-session", model.NewUserMessage("compose and run"))
	if err != nil {
		t.Fatal(err)
	}
	progress := false
	for ev := range events {
		if ev.Error != nil {
			t.Fatalf("native parent failed: %+v", ev.Error)
		}
		if ev.Object == "graph.node.start" {
			progress = true
		}
	}
	if !progress || calls != 2 {
		t.Fatalf("missing live inner events: progress=%v calls=%d", progress, calls)
	}
	runs, err := s.m.pg.ListAllFlowRuns()
	if err != nil || len(runs) != 1 || runs[0].Source != "inline" {
		t.Fatalf("missing durable temporary run: %+v %v", runs, err)
	}
}

func TestWorkflowCannotRestoreHostToolsMissingFromCallingAgent(t *testing.T) {
	s, _ := workflowTestServer(t)
	entry := &nativeWorkflowTool{s: s, key: "auto", entry: true}
	parent := llmagent.New("without_host", llmagent.WithTools([]actool.Tool{entry}))
	inv := nativeagent.NewInvocation(nativeagent.WithInvocationAgent(parent))
	ctx := nativeagent.NewInvocationContext(t.Context(), inv)
	def := flowproto.Definition{Name: "blocked_host", Version: 1, InputSchema: map[string]any{"type": "object"}, Entry: "begin", Nodes: []flowproto.Node{{ID: "begin", Type: "start", Next: "command"}, {ID: "command", Type: "tool", Tool: "workflow_command", Arguments: map[string]any{"program": "printf", "argv": []any{"unexpected"}}, Next: "end"}, {ID: "end", Type: "end", Output: "unexpected"}}}
	request, _ := json.Marshal(map[string]any{"definition": def, "input": map[string]any{}})
	if _, err := entry.Call(ctx, request); err == nil || !strings.Contains(err.Error(), "unavailable tool") {
		t.Fatalf("workflow restored a caller-disabled host capability: %v", err)
	}
}

func TestWorkflowPreservesPerNodeToolInterception(t *testing.T) {
	s, _ := workflowTestServer(t)
	called := false
	hooks := actool.NewCallbacks().RegisterBeforeTool(func(_ context.Context, args *actool.BeforeToolArgs) (*actool.BeforeToolResult, error) {
		if args.ToolName == "workflow_command" {
			called = true
			return &actool.BeforeToolResult{CustomResult: toolkit.Error("blocked-node")}, nil
		}
		return nil, nil
	})
	ctx := toolkit.WithCallHooks(t.Context(), hooks)
	def := flowproto.Definition{Name: "intercepted", Version: 1, InputSchema: map[string]any{"type": "object"}, Entry: "begin", Nodes: []flowproto.Node{{ID: "begin", Type: "start", Next: "command"}, {ID: "command", Type: "tool", Tool: "workflow_command", Arguments: map[string]any{"program": "printf", "argv": []any{"unexpected"}}, Next: "end"}, {ID: "end", Type: "end", Output: "unexpected"}}}
	request, _ := json.Marshal(map[string]any{"definition": def, "input": map[string]any{}})
	if _, err := (&nativeWorkflowTool{s: s, key: "auto", entry: true}).Call(ctx, request); err == nil || !strings.Contains(err.Error(), "blocked-node") {
		t.Fatalf("tool hook was bypassed: %v", err)
	}
	if !called {
		t.Fatal("node-level interceptor did not run")
	}
}

func TestWorkflowHostAdapterPreservesBashInterception(t *testing.T) {
	s, _ := workflowTestServer(t)
	called := false
	hooks := actool.NewCallbacks().RegisterBeforeTool(func(_ context.Context, args *actool.BeforeToolArgs) (*actool.BeforeToolResult, error) {
		if args.ToolName == "Bash" {
			called = true
			return &actool.BeforeToolResult{CustomResult: toolkit.Error("blocked-bash")}, nil
		}
		return nil, nil
	})
	ctx := toolkit.WithCallHooks(t.Context(), hooks)
	def := flowproto.Definition{Name: "host_intercepted", Version: 1, InputSchema: map[string]any{"type": "object"}, Entry: "begin", Nodes: []flowproto.Node{{ID: "begin", Type: "start", Next: "command"}, {ID: "command", Type: "tool", Tool: "workflow_command", Arguments: map[string]any{"program": "printf", "argv": []any{"unexpected"}}, Next: "end"}, {ID: "end", Type: "end", Output: "unexpected"}}}
	request, _ := json.Marshal(map[string]any{"definition": def, "input": map[string]any{}})
	if result, err := (&nativeWorkflowTool{s: s, key: "auto", entry: true}).Call(ctx, request); err == nil || !strings.Contains(err.Error(), "blocked-bash") {
		t.Fatalf("Bash interceptor was bypassed by host adapter: %v, called=%v, result=%#v", err, called, result)
	}
	if !called {
		t.Fatal("underlying Bash interceptor did not run")
	}
}

func TestWorkflowCancellationStopsNativeCommand(t *testing.T) {
	s, _ := workflowTestServer(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	def := flowproto.Definition{Name: "cancellable", Version: 1, InputSchema: map[string]any{"type": "object"}, Entry: "begin", Nodes: []flowproto.Node{{ID: "begin", Type: "start", Next: "wait"}, {ID: "wait", Type: "tool", Tool: "workflow_command", Arguments: map[string]any{"program": "sh", "argv": []any{"-c", "sleep 30"}}, Next: "end"}, {ID: "end", Type: "end", Output: "unexpected"}}}
	raw, _ := json.Marshal(map[string]any{"definition": def, "input": map[string]any{}})
	reader, err := (&nativeWorkflowTool{s: s, key: "auto", entry: true}).StreamableCall(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	done := make(chan error, 1)
	go func() {
		for {
			_, err := reader.Recv()
			if err != nil {
				done <- err
				return
			}
		}
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil || err == io.EOF {
			t.Fatalf("cancellation returned success: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("native command did not stop")
	}
	runs, err := s.m.pg.ListAllFlowRuns()
	if err != nil || len(runs) != 1 || runs[0].Status != "cancelled" {
		t.Fatalf("cancel not persisted: %+v %v", runs, err)
	}
}

func TestWorkflowSSEReportsBeforeActualCommandFinishes(t *testing.T) {
	s, do := workflowTestServer(t)
	def := flowproto.Definition{Name: "live_command", Version: 1, InputSchema: map[string]any{"type": "object"}, Entry: "begin", Nodes: []flowproto.Node{{ID: "begin", Type: "start", Next: "wait"}, {ID: "wait", Type: "tool", Tool: "workflow_command", Arguments: map[string]any{"program": "sh", "argv": []any{"-c", "sleep 2; printf complete"}}, Next: "end"}, {ID: "end", Type: "end", Output: "complete"}}}
	rec := do("POST", "/api/flow-runs", map[string]any{"definition": def, "input": map[string]any{}})
	if rec.Code != 202 {
		t.Fatal(rec.Body.String())
	}
	var started struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &started)
	token, err := signJWT(s.jwtKey)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(s.Handler())
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"/api/flow-runs/"+started.ID+"/events", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	scanner := bufio.NewScanner(response.Body)
	sawNode := false
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), "data:") && strings.Contains(scanner.Text(), "graph.node.start") {
			sawNode = true
			break
		}
	}
	run, err := s.m.pg.GetFlowRun(started.ID)
	if !sawNode || err != nil || run.Status != "running" {
		t.Fatalf("no node event before completion: run=%+v err=%v scan=%v", run, err, scanner.Err())
	}
	// Stop the fixture without waiting for its full sleep, then drain the stream.
	if stop, ok := s.flowCancels.Load(started.ID); ok {
		stop.(context.CancelFunc)()
	}
	for scanner.Scan() {
		if scanner.Text() == "event: done" {
			break
		}
	}
	run, err = s.m.pg.GetFlowRun(started.ID)
	if err != nil || run.Status != "cancelled" {
		t.Fatalf("cancellation not observed via SSE: %+v %v", run, err)
	}
}

func TestPublishedWorkflowCanBeUsedAsAnotherWorkflowTool(t *testing.T) {
	s, do := workflowTestServer(t)
	child := flowproto.ExampleDefinition()
	child.Name = "nested_child"
	raw, _ := json.Marshal(child)
	id, err := s.m.pg.SaveFlow(0, child.Name, raw, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.m.pg.RegisterFlow(id, 1, []string{"auto"}); err != nil {
		t.Fatal(err)
	}
	parent := flowproto.Definition{Name: "nested_parent", Version: 1, InputSchema: map[string]any{"type": "object"}, Entry: "begin", Nodes: []flowproto.Node{{ID: "begin", Type: "start", Next: "child"}, {ID: "child", Type: "tool", Tool: "flow_nested_child", Arguments: map[string]any{"target": "https://example.test"}, Next: "end"}, {ID: "end", Type: "end", Output: map[string]any{"decision": map[string]any{"$ref": "steps.child.output.decision"}}}}}
	request, _ := json.Marshal(map[string]any{"definition": parent, "input": map[string]any{}})
	result, err := (&nativeWorkflowTool{s: s, key: "auto", entry: true}).Call(t.Context(), request)
	if err != nil || !strings.Contains(toolkit.TextOf(result), "accepted") {
		t.Fatalf("nested result lost: %v %v", result, err)
	}
	runs, err := s.m.pg.ListAllFlowRuns()
	if err != nil || len(runs) != 2 {
		t.Fatalf("missing nested audit: %+v %v", runs, err)
	}
	for _, run := range runs {
		if run.FlowID == id && (run.AgentKey != "auto" || run.ParentRunID == "") {
			t.Fatalf("nested caller attribution lost: %+v", run)
		}
	}
	var root, childRun string
	for _, run := range runs {
		if run.ParentRunID == "" {
			root = run.ID
		} else {
			childRun = run.ID
		}
	}
	replay := do("GET", "/api/flow-runs/"+root+"/events", nil)
	if replay.Code != 200 || !strings.Contains(replay.Body.String(), `"run_id":"`+childRun+`"`) {
		t.Fatalf("parent SSE omitted child events: %d %s", replay.Code, replay.Body.String())
	}
}

func TestPublishedWorkflowArtifactFeedsParentCommand(t *testing.T) {
	s, _ := workflowTestServer(t)
	child := flowproto.Definition{Name: "artifact_child", Version: 1, InputSchema: map[string]any{"type": "object"}, Entry: "begin", Nodes: []flowproto.Node{{ID: "begin", Type: "start", Next: "emit"}, {ID: "emit", Type: "tool", Tool: "workflow_command", Arguments: map[string]any{"program": "printf", "argv": []any{"%s", "nested-bytes"}}, Next: "end"}, {ID: "end", Type: "end", Output: map[string]any{"$ref": "steps.emit.data.stdout_artifact"}}}}
	raw, _ := json.Marshal(child)
	id, err := s.m.pg.SaveFlow(0, child.Name, raw, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.m.pg.RegisterFlow(id, 1, []string{"auto"}); err != nil {
		t.Fatal(err)
	}
	parent := flowproto.Definition{Name: "consume_artifact", Version: 1, InputSchema: map[string]any{"type": "object"}, Entry: "begin", Nodes: []flowproto.Node{{ID: "begin", Type: "start", Next: "child"}, {ID: "child", Type: "tool", Tool: "flow_artifact_child", Next: "copy"}, {ID: "copy", Type: "tool", Tool: "workflow_command", Arguments: map[string]any{"program": "cat", "stdin_artifact": map[string]any{"$ref": "steps.child.output.id"}}, Next: "end"}, {ID: "end", Type: "end", Output: map[string]any{"$ref": "steps.copy.data.stdout"}}}}
	request, _ := json.Marshal(map[string]any{"definition": parent, "input": map[string]any{}})
	result, err := (&nativeWorkflowTool{s: s, key: "auto", entry: true}).Call(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	data, ok := result.(map[string]any)
	if !ok || data["output"] != "nested-bytes" {
		t.Fatalf("parent command lost child artifact bytes: %v", result)
	}
}

func TestNestedWorkflowDoesNotGainAdministratorDependencies(t *testing.T) {
	s, _ := workflowTestServer(t)
	secret := &db.Tool{Key: "private_dependency", Kind: "command", Description: "worker-only fixture", Enabled: true, Agents: []string{"worker"}, Schema: json.RawMessage(`{"type":"object"}`), Exec: json.RawMessage(`{"command":"printf private"}`)}
	if err := s.m.pg.CreateCustomTool(secret); err != nil {
		t.Fatal(err)
	}
	child := flowproto.Definition{Name: "restricted_child", Version: 1, InputSchema: map[string]any{"type": "object"}, Entry: "begin", Nodes: []flowproto.Node{{ID: "begin", Type: "start", Next: "private"}, {ID: "private", Type: "tool", Tool: secret.Key, Next: "end"}, {ID: "end", Type: "end", Output: "private"}}}
	raw, _ := json.Marshal(child)
	id, err := s.m.pg.SaveFlow(0, child.Name, raw, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate an old publication, before the new publish-time role preflight.
	if _, err := s.m.pg.RegisterFlow(id, 1, []string{"auto"}); err != nil {
		t.Fatal(err)
	}
	parent := flowproto.Definition{Name: "parent", Version: 1, InputSchema: map[string]any{"type": "object"}, Entry: "begin", Nodes: []flowproto.Node{{ID: "begin", Type: "start", Next: "child"}, {ID: "child", Type: "tool", Tool: "flow_restricted_child", Next: "end"}, {ID: "end", Type: "end", Output: "unexpected"}}}
	request, _ := json.Marshal(map[string]any{"definition": parent, "input": map[string]any{}})
	if _, err := (&nativeWorkflowTool{s: s, key: "auto", entry: true}).Call(t.Context(), request); err == nil {
		t.Fatal("nested workflow escalated its tool capability")
	}
}

func TestWorkflowDynamicCatalogAndPublicationLifecycle(t *testing.T) {
	s, do := workflowTestServer(t)
	row := &db.Tool{Key: "nmap", Kind: "command", Description: "fixture command, no network", Enabled: true, Agents: []string{"auto"}, Schema: json.RawMessage(`{"type":"object","properties":{"target":{"type":"string"}},"required":["target"]}`), Exec: json.RawMessage(`{"command":"printf '%s' {target}"}`)}
	if err := s.m.pg.CreateCustomTool(row); err != nil {
		t.Fatal(err)
	}
	catalog := do("GET", "/api/flows/catalog", nil)
	if catalog.Code != 200 || !strings.Contains(catalog.Body.String(), `"name":"nmap"`) {
		t.Fatalf("dynamic catalog: %d %s", catalog.Code, catalog.Body.String())
	}
	def := flowproto.ExampleDefinition()
	def.Name = "dynamic_test"
	def.Nodes = []flowproto.Node{{ID: "begin", Type: "start", Next: "scan"}, {ID: "scan", Type: "tool", Tool: "nmap", Arguments: map[string]any{"target": map[string]any{"$ref": "input.target"}}, Next: "result"}, {ID: "result", Type: "end", Output: map[string]any{"value": map[string]any{"$ref": "steps.scan.data.output"}}}}
	rec := do("POST", "/api/flows", map[string]any{"definition": def})
	if rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	var flow db.Flow
	_ = json.Unmarshal(rec.Body.Bytes(), &flow)
	path := fmt.Sprintf("/api/flows/%d", flow.ID)
	rec = do("POST", path+"/publish", map[string]any{"revision": flow.Revision, "agents": []string{"auto"}})
	if rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	toolRow, err := s.m.pg.GetTool("flow_dynamic_test")
	if err != nil || toolRow == nil || toolRow.Kind != "workflow" || toolRow.System {
		t.Fatalf("not a real custom tool: %+v %v", toolRow, err)
	}
	tools := onlyPublishedFlows(s.flowTools(t.Context(), "auto", nil))
	if len(tools) != 1 {
		t.Fatalf("tools=%v", tools)
	}
	result, err := tools[0].Call(t.Context(), []byte(`{"target":"fixture.example"}`))
	if err != nil || !strings.Contains(toolkit.TextOf(result), "fixture.example") {
		t.Fatalf("result=%v err=%v", result, err)
	}
	runs, err := s.m.pg.ListAllFlowRuns()
	if err != nil || len(runs) != 1 || runs[0].Source != "tool" || runs[0].AgentKey != "auto" {
		t.Fatalf("missing tool-run audit: %+v %v", runs, err)
	}
	events, err := s.m.pg.FlowEvents(runs[0].ID, 0)
	if err != nil || len(events) == 0 || len(events[0].Payload) == 0 {
		t.Fatalf("missing native payloads: %+v %v", events, err)
	}
	// Revoking a dependency must be checked even on an already assembled tool.
	if err := s.m.pg.UpdateTool("nmap", row.Description, row.Schema, json.RawMessage(`["auto"]`), false); err != nil {
		t.Fatal(err)
	}
	if _, err := tools[0].Call(t.Context(), []byte(`{"target":"fixture.example"}`)); err == nil {
		t.Fatal("disabled dependency remained callable")
	}
	if err := s.m.pg.UpdateTool("nmap", row.Description, row.Schema, json.RawMessage(`["auto"]`), true); err != nil {
		t.Fatal(err)
	}
	def.Description = "new publication"
	rec = do("PUT", path, map[string]any{"definition": def, "revision": flow.Revision})
	if rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &flow)
	rec = do("POST", path+"/publish", map[string]any{"revision": flow.Revision, "agents": []string{"auto"}})
	if rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	versions, err := s.m.pg.FlowVersions(flow.ID)
	if err != nil || len(versions) != 2 {
		t.Fatalf("versions=%v err=%v", versions, err)
	}
	rec = do("POST", path+"/rollback", map[string]any{"version_id": versions[1].ID})
	if rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	rec = do("POST", path+"/unpublish", nil)
	if rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	if _, err := tools[0].Call(t.Context(), []byte(`{"target":"fixture.example"}`)); err == nil {
		t.Fatal("unpublished tool remained callable")
	}
	s.initWorkflows()
	toolRow, _ = s.m.pg.GetTool("flow_dynamic_test")
	if toolRow.Enabled {
		t.Fatal("restart re-enabled an unpublished tool")
	}
}

func TestWorkflowInlineHTTPFullBodyComposition(t *testing.T) {
	s, _ := workflowTestServer(t)
	body := `{"padding":"` + strings.Repeat("x", 32768) + `","marker":"complete-body"}`
	target := nettest.Server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body)
	}), false)
	ref := func(path string) any { return map[string]any{"$ref": path} }
	def := flowproto.Definition{Name: "inline_http", Version: 1, InputSchema: map[string]any{"type": "object"}, Entry: "begin", Nodes: []flowproto.Node{
		{ID: "begin", Type: "start", Next: "fetch"},
		{ID: "fetch", Type: "tool", Tool: "http_request", Arguments: map[string]any{"url": target.URL}, Next: "parse"},
		{ID: "parse", Type: "transform", Operation: "json.parse", Arguments: map[string]any{"artifact": ref("steps.fetch.data.body_artifact")}, Next: "result"},
		{ID: "result", Type: "end", Output: map[string]any{"marker": ref("steps.parse.data.marker"), "preview_truncated": ref("steps.fetch.data.body_preview_truncated"), "artifact": ref("steps.fetch.data.body_artifact")}},
	}}
	entry := &nativeWorkflowTool{s: s, key: "auto", entry: true}
	request, _ := json.Marshal(map[string]any{"definition": def, "input": map[string]any{}})
	result, err := entry.Call(t.Context(), request)
	if err != nil || !strings.Contains(toolkit.TextOf(result), "complete-body") {
		t.Fatalf("HTTP composition result=%v err=%v", result, err)
	}
	flows, err := s.m.pg.ListFlows()
	if err != nil || len(flows) != 0 {
		t.Fatal("inline run polluted workflow catalog")
	}
	runs, err := s.m.pg.ListAllFlowRuns()
	if err != nil || len(runs) != 1 || runs[0].FlowID != 0 || runs[0].Source != "inline" {
		t.Fatalf("inline run=%+v err=%v", runs, err)
	}
	var id string
	if err := s.m.pg.QueryRow(`SELECT id FROM flow_artifacts WHERE run_id=?1`, runs[0].ID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	data, err := (flowArtifacts{s: s, run: runs[0].ID}).Read(t.Context(), id)
	if err != nil || string(data) != body {
		t.Fatalf("artifact lost bytes: %d %v", len(data), err)
	}
	if _, err := (flowArtifacts{s: s, run: "another-run"}).Read(t.Context(), id); err == nil {
		t.Fatal("artifact ownership bypassed")
	}
}
