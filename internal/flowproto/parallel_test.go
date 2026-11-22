package flowproto

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emhha/scopeforge/toolkit"
	"trpc.group/trpc-go/trpc-agent-go/tool"
)

func TestNativeParallelToolSubgraphsDoNotShareMessages(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	go func() {
		for i := 0; i < 2; i++ {
			select {
			case <-started:
			case <-ctx.Done():
				return
			}
		}
		close(release)
	}()
	echo := toolkit.New("echo", "fixture", map[string]any{"type": "object", "properties": map[string]any{"value": map[string]any{"type": "string"}}, "required": []string{"value"}}, func(ctx context.Context, raw json.RawMessage) (toolkit.Output, error) {
		var in struct {
			Value string `json:"value"`
		}
		if err := json.Unmarshal(raw, &in); err != nil {
			return toolkit.Output{}, err
		}
		started <- struct{}{}
		select {
		case <-release:
			return toolkit.Text(in.Value), nil
		case <-ctx.Done():
			return toolkit.Output{}, ctx.Err()
		}
	})
	def := Definition{Name: "parallel", Version: 1, InputSchema: map[string]any{"type": "object"}, Entry: "begin", Nodes: []Node{
		{ID: "begin", Type: "start", Next: "fork"}, {ID: "fork", Type: "parallel", Branches: []string{"left", "right"}},
		{ID: "left", Type: "tool", Tool: "echo", Arguments: map[string]any{"value": "left-only"}, Next: "join"},
		{ID: "right", Type: "tool", Tool: "echo", Arguments: map[string]any{"value": "right-only"}, Next: "join"},
		{ID: "join", Type: "join", From: []string{"left", "right"}, Next: "end"},
		{ID: "end", Type: "end", Output: map[string]any{"left": map[string]any{"$ref": "steps.left.text"}, "right": map[string]any{"$ref": "#/steps/right/text"}}},
	}}
	compiled, err := Compile(def, map[string]tool.CallableTool{"echo": echo})
	if err != nil {
		t.Fatal(err)
	}
	result, err := compiled.Tool.Call(ctx, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	text := toolkit.TextOf(result)
	if !strings.Contains(text, `"left":"left-only"`) || !strings.Contains(text, `"right":"right-only"`) {
		t.Fatalf("tool subgraph state mixed: %s", text)
	}
}

func TestFailedToolSubgraphDoesNotRunIndependentNextTool(t *testing.T) {
	for _, goError := range []bool{false, true} {
		t.Run(fmt.Sprint(goError), func(t *testing.T) {
			var calls atomic.Int32
			fail := toolkit.New("fail", "failure fixture", nil, func(context.Context, json.RawMessage) (toolkit.Output, error) {
				if goError {
					return toolkit.Output{}, fmt.Errorf("stop here")
				}
				return toolkit.Error("stop here"), nil
			})
			next := toolkit.New("next", "must not run", nil, func(context.Context, json.RawMessage) (toolkit.Output, error) {
				calls.Add(1)
				return toolkit.Text("unexpected"), nil
			})
			def := Definition{Name: "stop_on_failure", Version: 1, InputSchema: map[string]any{"type": "object"}, Entry: "begin", Nodes: []Node{{ID: "begin", Type: "start", Next: "fail"}, {ID: "fail", Type: "tool", Tool: "fail", Next: "next"}, {ID: "next", Type: "tool", Tool: "next", Next: "end"}, {ID: "end", Type: "end", Output: "unexpected"}}}
			compiled, err := Compile(def, map[string]tool.CallableTool{"fail": fail, "next": next})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := compiled.Tool.Call(t.Context(), []byte(`{}`)); err == nil {
				t.Fatal("failed child reported success")
			}
			if calls.Load() != 0 {
				t.Fatal("native graph ran a downstream side effect after failure")
			}
		})
	}
}

func TestRejectJoinOfMutuallyExclusiveBranches(t *testing.T) {
	def := Definition{Name: "invalid_join", Version: 1, InputSchema: map[string]any{"type": "object"}, Entry: "begin", Nodes: []Node{
		{ID: "begin", Type: "start", Next: "choose"},
		{ID: "choose", Type: "branch", Condition: &Condition{Ref: "input.value", Equals: "left", Then: "left", Else: "right"}},
		{ID: "left", Type: "transform", Operation: "lines", Arguments: map[string]any{"text": "left"}, Next: "join"},
		{ID: "right", Type: "transform", Operation: "lines", Arguments: map[string]any{"text": "right"}, Next: "join"},
		{ID: "join", Type: "join", From: []string{"left", "right"}, Next: "end"},
		{ID: "end", Type: "end", Output: "done"},
	}}
	if _, err := Compile(def, nil); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("dead join was not rejected: %v", err)
	}
}

func TestRejectFutureStepReference(t *testing.T) {
	def := ExampleDefinition()
	def.Nodes[1].Arguments = map[string]any{"target": map[string]any{"$ref": "steps.accepted.text"}}
	if _, err := Compile(def, map[string]tool.CallableTool{"inspect": fixtureTool()}); err == nil || !strings.Contains(err.Error(), "guaranteed") {
		t.Fatalf("future result reference accepted: %v", err)
	}
}
