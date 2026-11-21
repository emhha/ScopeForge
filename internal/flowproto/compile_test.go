package flowproto

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emhha/scopeforge/internal/modeltest"
	"github.com/emhha/scopeforge/toolkit"
	"trpc.group/trpc-go/trpc-agent-go/agent/llmagent"
	"trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/runner"
	"trpc.group/trpc-go/trpc-agent-go/session/inmemory"
	"trpc.group/trpc-go/trpc-agent-go/tool"
)

func example() Definition {
	return Definition{Name: "assessment", Version: 1, Description: "Assess a supplied target", Entry: "begin", InputSchema: map[string]any{"type": "object", "required": []string{"target"}, "additionalProperties": false, "properties": map[string]any{"target": map[string]any{"type": "string"}}}, Nodes: []Node{
		{ID: "begin", Type: "start", Next: "inspect"},
		{ID: "inspect", Type: "tool", Tool: "inspect", Arguments: map[string]any{"target": map[string]any{"$ref": "input.target"}}, Next: "decide"},
		{ID: "decide", Type: "branch", Condition: &Condition{Ref: "steps.inspect.text", Equals: "allowed", Then: "accepted", Else: "rejected"}},
		{ID: "accepted", Type: "end", Output: map[string]any{"decision": "accepted", "target": map[string]any{"$ref": "input.target"}}},
		{ID: "rejected", Type: "end", Output: map[string]any{"decision": "rejected"}},
	}}
}

func fixtureTool() tool.CallableTool {
	return toolkit.New("inspect", "fixture", map[string]any{"type": "object", "required": []string{"target"}, "properties": map[string]any{"target": map[string]any{"type": "string"}}}, func(_ context.Context, raw json.RawMessage) (toolkit.Output, error) {
		var input struct {
			Target string `json:"target"`
		}
		if err := json.Unmarshal(raw, &input); err != nil {
			return toolkit.Output{}, err
		}
		if strings.HasPrefix(input.Target, "https://") {
			return toolkit.Text("allowed"), nil
		}
		return toolkit.Text("denied"), nil
	})
}

func TestNativeGraphAgentToolExecutesBranches(t *testing.T) {
	def := example()
	compiled, err := Compile(def, map[string]tool.CallableTool{"inspect": fixtureTool()})
	if err != nil {
		t.Fatal(err)
	}
	if compiled.Tool.Declaration().Name != "flow_assessment" || compiled.Tool.Declaration().InputSchema.Properties["target"] == nil {
		t.Fatal("native AgentTool lost input schema")
	}
	// Editing the draft after publication cannot change the compiled workflow.
	def.Nodes[3].Output.(map[string]any)["decision"] = "mutated"
	for _, tc := range []struct{ input, want string }{{`{"target":"https://example.test"}`, "accepted"}, {`{"target":"http://example.test"}`, "rejected"}} {
		result, err := compiled.Tool.Call(t.Context(), []byte(tc.input))
		if err != nil {
			t.Fatal(err)
		}
		text := toolkit.TextOf(result)
		if !strings.Contains(text, tc.want) || strings.Contains(text, "mutated") {
			t.Fatalf("result=%s want=%s", text, tc.want)
		}
	}
}

func TestNativeRunnerCallsFlowAsToolAndForwardsProgress(t *testing.T) {
	compiled, err := Compile(example(), map[string]tool.CallableTool{"inspect": fixtureTool()})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	mdl := modeltest.Func(func(_ context.Context, req *model.Request) (<-chan *model.Response, error) {
		calls++
		if calls == 1 {
			return modeltest.Responses(&model.Response{Choices: []model.Choice{{Message: model.Message{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{{ID: "flow-call", Type: "function", Function: model.FunctionDefinitionParam{Name: "flow_assessment", Arguments: []byte(`{"target":"https://example.test"}`)}}}}}}}), nil
		}
		found := false
		for _, message := range req.Messages {
			if message.Role == model.RoleTool && message.ToolID == "flow-call" && strings.Contains(message.Content, "accepted") {
				found = true
			}
		}
		if !found {
			return nil, errors.New("parent did not receive structured flow result")
		}
		return modeltest.Responses(modeltest.Final("parent complete")), nil
	})
	parent := llmagent.New("parent", llmagent.WithModel(mdl), llmagent.WithTools([]tool.Tool{compiled.Tool}), llmagent.WithMaxLLMCalls(2))
	sessions := inmemory.NewSessionService()
	defer sessions.Close()
	r := runner.NewRunner("flowproto", parent, runner.WithSessionService(sessions))
	defer r.Close()
	events, err := r.Run(t.Context(), "tester", "run", model.NewUserMessage("test flow"))
	if err != nil {
		t.Fatal(err)
	}
	forwarded, completed := false, false
	for ev := range events {
		if ev.Error != nil {
			t.Fatal(ev.Error)
		}
		if ev.Author == "flow_assessment" {
			forwarded = true
		}
		for _, choice := range ev.Choices {
			if choice.Message.Content == "parent complete" {
				completed = true
			}
		}
	}
	if !forwarded || !completed {
		t.Fatalf("forwarded=%v completed=%v calls=%d", forwarded, completed, calls)
	}
}

func TestValidationAndRuntimeFailure(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Definition)
	}{
		{"missing tool", func(d *Definition) { d.Nodes[1].Tool = "not-bound" }},
		{"cycle", func(d *Definition) { d.Nodes[1].Next = "begin" }},
		{"missing destination", func(d *Definition) { d.Nodes[1].Next = "missing" }},
		{"duplicate", func(d *Definition) { d.Nodes[1].ID = "begin" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			def := example()
			tc.mutate(&def)
			if _, err := Compile(def, map[string]tool.CallableTool{"inspect": fixtureTool()}); err == nil {
				t.Fatal("invalid graph accepted")
			}
		})
	}
	compiled, err := Compile(example(), map[string]tool.CallableTool{"inspect": fixtureTool()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := compiled.Tool.Call(t.Context(), []byte(`{}`)); err == nil {
		t.Fatal("missing required input accepted")
	}
	def := example()
	def.Nodes[1].Arguments = map[string]any{"target": map[string]any{"$ref": "input.missing"}}
	compiled, err = Compile(def, map[string]tool.CallableTool{"inspect": fixtureTool()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := compiled.Tool.Call(t.Context(), []byte(`{"target":"https://example.test"}`)); err == nil {
		t.Fatal("broken mapping accepted")
	}
}

func TestConcurrentCallsDoNotShareState(t *testing.T) {
	compiled, err := Compile(example(), map[string]tool.CallableTool{"inspect": fixtureTool()})
	if err != nil {
		t.Fatal(err)
	}
	var failed atomic.Int32
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			target := "https://example.test"
			want := "accepted"
			if i%2 == 1 {
				target = "http://example.test"
				want = "rejected"
			}
			input, _ := json.Marshal(map[string]any{"target": target})
			result, err := compiled.Tool.Call(t.Context(), input)
			if err != nil || !strings.Contains(toolkit.TextOf(result), want) {
				failed.Add(1)
			}
		}()
	}
	wg.Wait()
	if failed.Load() != 0 {
		t.Fatalf("%d calls leaked state or failed", failed.Load())
	}
}

func TestToolFailureStopsNativeGraph(t *testing.T) {
	failing := toolkit.New("inspect", "failure fixture", nil, func(context.Context, json.RawMessage) (toolkit.Output, error) {
		return toolkit.Error("fixture failure"), nil
	})
	compiled, err := Compile(example(), map[string]tool.CallableTool{"inspect": failing})
	if err != nil {
		t.Fatal(err)
	}
	result, err := compiled.Tool.Call(t.Context(), []byte(`{"target":"https://example.test"}`))
	if err == nil {
		t.Fatalf("failed tool produced success: %+v", result)
	}
}

func TestNativeGraphCancellationReachesTool(t *testing.T) {
	started, stopped := make(chan struct{}), make(chan struct{})
	blocking := toolkit.New("inspect", "cancellation fixture", nil, func(ctx context.Context, _ json.RawMessage) (toolkit.Output, error) {
		close(started)
		defer close(stopped)
		<-ctx.Done()
		return toolkit.Output{}, ctx.Err()
	})
	compiled, err := Compile(example(), map[string]tool.CallableTool{"inspect": blocking})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := compiled.Tool.Call(ctx, []byte(`{"target":"https://example.test"}`)); done <- err }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("native tool never started")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled flow returned %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("flow did not cancel")
	}
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("tool outlived flow cancellation")
	}
}
