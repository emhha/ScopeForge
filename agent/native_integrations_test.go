package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/internal/modeltest"
	"github.com/Autumn-27/artex/toolkit"
	"trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/tool"
)

func TestNativeRunnerExecutesHostCommandInWorkspace(t *testing.T) {
	dir := t.TempDir()
	calls := 0
	mdl := modeltest.Func(func(_ context.Context, req *model.Request) (<-chan *model.Response, error) {
		calls++
		if calls == 1 {
			return modeltest.Responses(&model.Response{Choices: []model.Choice{{Message: model.Message{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{{ID: "write", Type: "function", Function: model.FunctionDefinitionParam{Name: "Bash", Arguments: []byte(`{"command":"printf native > result.txt"}`)}}}}}}}), nil
		}
		if len(req.Messages) < 3 {
			t.Error("tool result was not added to native context")
		}
		return modeltest.Responses(modeltest.Final("done")), nil
	})
	text, _, err := captureRun(t.Context(), RunConfig{Model: mdl, WorkingDir: dir, MaxTurns: 3}, "write", nil)
	if err != nil || text != "done" {
		t.Fatalf("text=%q error=%v", text, err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "result.txt"))
	if err != nil || string(data) != "native" {
		t.Fatalf("file=%q error=%v", data, err)
	}
}
func TestDefaultsAreReviewedBeforeNativeExecution(t *testing.T) {
	var reviewed string
	seen := false
	probe := DecorateTool(toolkit.New("probe", "probe", map[string]any{"type": "object", "properties": map[string]any{"target": map[string]any{"type": "string"}}}, func(_ context.Context, args json.RawMessage) (toolkit.Output, error) {
		seen = strings.Contains(string(args), "effective")
		return toolkit.Text("done"), nil
	}), "probe", map[string]any{"type": "object", "properties": map[string]any{"target": map[string]any{"type": "string", "default": "effective"}}})
	calls := 0
	mdl := modeltest.Func(func(context.Context, *model.Request) (<-chan *model.Response, error) {
		calls++
		if calls == 1 {
			return modeltest.Responses(&model.Response{Choices: []model.Choice{{Message: model.Message{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{{ID: "p", Function: model.FunctionDefinitionParam{Name: "probe", Arguments: []byte(`{}`)}}}}}}}), nil
		}
		return modeltest.Responses(modeltest.Final("done")), nil
	})
	callbacks := tool.NewCallbacks().RegisterBeforeTool(func(_ context.Context, args *tool.BeforeToolArgs) (*tool.BeforeToolResult, error) {
		reviewed = string(args.Arguments)
		return nil, nil
	})
	_, _, err := captureRun(t.Context(), RunConfig{Model: mdl, Tools: []tool.CallableTool{probe}, Hooks: callbacks, DisableHostTools: true}, "run", nil)
	if err != nil || !seen || !strings.Contains(reviewed, "effective") {
		t.Fatalf("reviewed=%q executed=%v error=%v", reviewed, seen, err)
	}
}
func TestNativeFetchCapturesStatusAndBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(201)
		_, _ = io.WriteString(w, "fixture body")
	}))
	defer server.Close()
	args, _ := json.Marshal(map[string]string{"url": server.URL})
	result, err := fetchTool("", "").Call(t.Context(), args)
	if err != nil || !strings.Contains(toolkit.TextOf(result), "fixture body") {
		t.Fatalf("fetch=%v error=%v", result, err)
	}
}
func TestDeepSeekSearchUsesNativeModelWithServerTool(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		tools, ok := body["tools"].([]any)
		if !ok || len(tools) != 1 || tools[0].(map[string]any)["type"] != "deepseek_web_search" {
			t.Errorf("native search declaration=%v", body["tools"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"fixture","type":"message","role":"assistant","content":[{"type":"text","text":"A source: https://example.com"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer server.Close()
	items, err := ProbeSearch(t.Context(), WebSearchOpts{Backend: "deepseek", DeepSeekBaseURL: server.URL, DeepSeekAPIKey: "fixture", DeepSeekModel: "fixture"}, "query", 3)
	if err != nil || len(items) != 1 {
		t.Fatalf("search=%v error=%v", items, err)
	}
}
