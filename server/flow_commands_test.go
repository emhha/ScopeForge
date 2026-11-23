package server

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/emhha/scopeforge/internal/flowproto"
	"github.com/emhha/scopeforge/toolkit"
	nativeagent "trpc.group/trpc-go/trpc-agent-go/agent"
	"trpc.group/trpc-go/trpc-agent-go/agent/llmagent"
	actool "trpc.group/trpc-go/trpc-agent-go/tool"
)

func TestWorkflowDirectCommandsShareDomainWithoutRegistration(t *testing.T) {
	s, _ := workflowTestServer(t)
	ref := func(path string) any { return map[string]any{"$ref": path} }
	def := flowproto.Definition{Name: "direct_commands", Version: 1, InputSchema: map[string]any{"type": "object", "required": []string{"domain"}, "properties": map[string]any{"domain": map[string]any{"type": "string"}}}, Entry: "begin", Nodes: []flowproto.Node{
		{ID: "begin", Type: "start", Next: "fork"},
		{ID: "fork", Type: "parallel", Branches: []string{"dns", "http", "shell"}},
		{ID: "dns", Type: "tool", Tool: "workflow_command", Arguments: map[string]any{"program": "printf", "argv": []any{"%s", ref("input.domain")}}, Next: "join"},
		{ID: "http", Type: "tool", Tool: "workflow_command", Arguments: map[string]any{"program": "cat", "stdin": ref("input.domain")}, Next: "join"},
		{ID: "shell", Type: "tool", Tool: "Bash", Arguments: map[string]any{"command": "printf 'direct-shell' | cat"}, Next: "join"},
		{ID: "join", Type: "join", From: []string{"dns", "http", "shell"}, Next: "end"},
		{ID: "end", Type: "end", Output: map[string]any{"dns": ref("steps.dns.data.value"), "http": ref("steps.http.data.stdout"), "shell": ref("steps.shell.data.output")}},
	}}
	// Metacharacters are data in argv/stdin and cannot become a shell command.
	domain := "example.test; printf MUST_NOT_EXECUTE"
	request, _ := json.Marshal(map[string]any{"definition": def, "input": map[string]any{"domain": domain}})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	result, err := (&nativeWorkflowTool{s: s, key: "auto", entry: true}).Call(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		Output struct{ DNS, HTTP, Shell string }
	}
	if err := json.Unmarshal([]byte(toolkit.TextOf(result)), &response); err != nil {
		t.Fatal(err)
	}
	if response.Output.DNS != domain || response.Output.HTTP != domain || !strings.Contains(response.Output.Shell, "direct-shell") {
		t.Fatalf("shared input or branch output lost: %s", toolkit.TextOf(result))
	}
	custom, err := s.m.pg.ListCustomTools()
	if err != nil || len(custom) != 0 {
		t.Fatalf("direct commands registered custom tools: %+v %v", custom, err)
	}
	runs, err := s.m.pg.ListAllFlowRuns()
	if err != nil || len(runs) != 1 || runs[0].Status != "completed" {
		t.Fatalf("run was not persisted: %+v %v", runs, err)
	}
}

func TestWorkflowBashResultContractMatchesAgentAndManualCalls(t *testing.T) {
	s, _ := workflowTestServer(t)
	// The caller's real Bash must still execute; only its output is normalized.
	called := false
	// Use a native-shaped raw result rather than toolkit.Output.
	rawBash := &rawWorkflowBash{CallableTool: s.bashWorkflowTool(), called: &called}
	parent := llmagent.New("parent", llmagent.WithTools([]actool.Tool{rawBash}))
	ctx := nativeagent.NewInvocationContext(t.Context(), nativeagent.NewInvocation(nativeagent.WithInvocationAgent(parent)))
	pool, close, err := s.flowToolPool(ctx, "auto")
	if err != nil {
		t.Fatal(err)
	}
	defer close()
	result, err := pool["Bash"].Call(ctx, []byte(`{"command":"ignored"}`))
	if err != nil {
		t.Fatal(err)
	}
	output, ok := result.(toolkit.Output)
	if !called || !ok || output.Data.(map[string]any)["output"] != "caller-result" {
		t.Fatalf("Bash result differs by entry point: %+v", result)
	}
}

type rawWorkflowBash struct {
	actool.CallableTool
	called *bool
}

func TestWorkflowCatalogDoesNotAdvertiseOptionalCLI(t *testing.T) {
	s, do := workflowTestServer(t)
	response := do("GET", "/api/flows/catalog", nil)
	if response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	var catalog struct {
		Tools []struct{ Name string }
	}
	if err := json.Unmarshal(response.Body.Bytes(), &catalog); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, item := range catalog.Tools {
		names[item.Name] = true
	}
	for _, name := range []string{"subfinder", "amass", "httpx", "ehole", "merge_subdomains"} {
		if names[name] {
			t.Errorf("optional CLI adapter %s is still advertised", name)
		}
	}
	if !names["Bash"] || !names["workflow_command"] {
		t.Fatal("generic host execution tools are missing")
	}
	// The recon example must use only the real production catalog. Compiling it
	// never probes installed programs or launches a command.
	data, err := os.ReadFile("../internal/flowproto/testdata/domain-recon.json")
	if err != nil {
		t.Fatal(err)
	}
	var definition flowproto.Definition
	if err := json.Unmarshal(data, &definition); err != nil {
		t.Fatal(err)
	}
	pool := s.flowPool()
	pool["workflow_command"] = toolkit.New("workflow_command", "validation fixture", toolkit.SchemaMap(pool["workflow_command"].Declaration().InputSchema), func(context.Context, json.RawMessage) (toolkit.Output, error) {
		t.Fatal("catalog/validation must not execute a CLI")
		return toolkit.Output{}, nil
	})
	if _, err := flowproto.Compile(definition, pool); err != nil {
		t.Fatal(err)
	}
}

func (t *rawWorkflowBash) Call(context.Context, []byte) (any, error) {
	*t.called = true
	return map[string]any{"output": "caller-result", "exit_code": 0}, nil
}
