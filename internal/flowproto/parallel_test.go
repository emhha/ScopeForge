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

func parallelFixture() Definition {
	return Definition{Name: "parallel_domain", Version: 1, InputSchema: map[string]any{"type": "object"}, Entry: "begin", Nodes: []Node{
		{ID: "begin", Type: "start", Next: "fork"}, {ID: "fork", Type: "parallel", Branches: []string{"left", "right"}},
		{ID: "left", Type: "transform", Operation: "lines", Arguments: map[string]any{"text": "left"}, Next: "join"},
		{ID: "right", Type: "transform", Operation: "lines", Arguments: map[string]any{"text": "right"}, Next: "join"},
		{ID: "join", Type: "join", From: []string{"left", "right"}, Next: "end"},
		{ID: "end", Type: "end", Output: "done"},
	}}
}

func TestParallelRequiresExplicitSynchronizedConvergence(t *testing.T) {
	for _, mode := range []string{"duplicate", "no_join", "early_end", "shared_command"} {
		t.Run(mode, func(t *testing.T) {
			def := parallelFixture()
			switch mode {
			case "duplicate":
				def.Nodes[1].Branches = []string{"left", "left", "right"}
			case "no_join":
				def.Nodes[2].Next, def.Nodes[3].Next = "end", "end"
				def.Nodes = append(def.Nodes[:4], def.Nodes[5])
			case "early_end":
				def.Nodes[3].Type, def.Nodes[3].Next = "branch", ""
				def.Nodes[3].Condition = &Condition{Ref: "input.domain", Then: "right_done", Else: "end"}
				def.Nodes = append(def.Nodes, Node{ID: "right_done", Type: "transform", Operation: "lines", Next: "join"})
				def.Nodes[4].From = []string{"left", "right_done"}
			case "shared_command":
				def.Nodes[1].Branches = []string{"left", "middle", "right"}
				def.Nodes = append(def.Nodes, Node{ID: "middle", Type: "transform", Operation: "lines", Next: "left"})
			}
			if _, err := Compile(def, nil); err == nil {
				t.Fatal("unsafe parallel graph accepted")
			}
		})
	}
}

func TestNestedParallelJoinsAreSupported(t *testing.T) {
	def := parallelFixture()
	def.Nodes[2].Next = "inner"
	def.Nodes[4].From = []string{"inner_join", "right"}
	def.Nodes = append(def.Nodes,
		Node{ID: "inner", Type: "parallel", Branches: []string{"a", "b"}},
		Node{ID: "a", Type: "transform", Operation: "lines", Arguments: map[string]any{"text": "a"}, Next: "inner_join"},
		Node{ID: "b", Type: "transform", Operation: "lines", Arguments: map[string]any{"text": "b"}, Next: "inner_join"},
		Node{ID: "inner_join", Type: "join", From: []string{"a", "b"}, Next: "join"},
	)
	compiled, err := Compile(def, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, err := compiled.Tool.Call(ctx, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
}

func TestParallelBranchCanContainConditionalMerge(t *testing.T) {
	def := parallelFixture()
	def.Nodes[2].Next = "choose"
	def.Nodes[4].From = []string{"merge", "right"}
	def.Nodes = append(def.Nodes,
		Node{ID: "choose", Type: "branch", Condition: &Condition{Ref: "input.domain", Equals: "left", Then: "a", Else: "b"}},
		Node{ID: "a", Type: "transform", Operation: "lines", Arguments: map[string]any{"text": "a"}, Next: "merge"},
		Node{ID: "b", Type: "transform", Operation: "lines", Arguments: map[string]any{"text": "b"}, Next: "merge"},
		Node{ID: "merge", Type: "transform", Operation: "lines", Arguments: map[string]any{"text": "merged"}, Next: "join"},
	)
	compiled, err := Compile(def, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{`{"domain":"left"}`, `{"domain":"right"}`} {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		_, err := compiled.Tool.Call(ctx, []byte(input))
		cancel()
		if err != nil {
			t.Fatal(err)
		}
	}
}
