package flowproto_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdkhttp "github.com/emhha/netloom/nethttp"
	"github.com/emhha/scopeforge/internal/flowproto"
	"github.com/emhha/scopeforge/internal/nettest"
	"github.com/emhha/scopeforge/netrequest"
	"github.com/emhha/scopeforge/toolkit"
	nativeagent "trpc.group/trpc-go/trpc-agent-go/agent"
	"trpc.group/trpc-go/trpc-agent-go/graph"
	"trpc.group/trpc-go/trpc-agent-go/tool"
	"trpc.group/trpc-go/trpc-agent-go/tool/function"
)

// This is a proposed HTTP result adapter, not the production http_request tool.
// Verify typed data through the existing native graph before designing its API.
func TestDesignProbeHTTPStructuredComposition(t *testing.T) {
	server := nettest.Server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"hosts":["a.example.test","b.example.test"]}`)
	}), false)
	service, err := netrequest.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	type request struct {
		URL string `json:"url"`
	}
	type consume struct {
		Body   string `json:"body"`
		Status int    `json:"status"`
	}
	fetch := function.NewFunctionTool(func(ctx context.Context, in request) (map[string]any, error) {
		out, err := service.Do(ctx, sdkhttp.RequestInput{URL: in.URL}, nil, nil)
		if err != nil {
			return nil, err
		}
		return map[string]any{"data": map[string]any{"status_code": out.Status, "body_text": out.Body, "headers": out.Headers, "complete": out.Complete}}, nil
	}, function.WithName("http_contract_probe"), function.WithDescription("Local HTTP structured data contract probe"))
	parse := function.NewFunctionTool(func(_ context.Context, in consume) (map[string]any, error) {
		if in.Status != 200 {
			return nil, fmt.Errorf("wrong status %d", in.Status)
		}
		var decoded struct {
			Hosts []string `json:"hosts"`
		}
		if err := json.Unmarshal([]byte(in.Body), &decoded); err != nil {
			return nil, err
		}
		return map[string]any{"hosts": decoded.Hosts}, nil
	}, function.WithName("parse_contract_probe"), function.WithDescription("Parse local HTTP response in a following native tool node"))
	ref := func(path string) any { return map[string]any{"$ref": path} }
	def := flowproto.Definition{Name: "http_contract_probe", Version: 1, InputSchema: map[string]any{"type": "object", "properties": map[string]any{"url": map[string]any{"type": "string"}}, "required": []string{"url"}}, Entry: "begin", Nodes: []flowproto.Node{
		{ID: "begin", Type: "start", Next: "fetch"},
		{ID: "fetch", Type: "tool", Tool: "http_contract_probe", Arguments: map[string]any{"url": ref("input.url")}, Next: "parse"},
		{ID: "parse", Type: "tool", Tool: "parse_contract_probe", Arguments: map[string]any{"body": ref("steps.fetch.data.body_text"), "status": ref("steps.fetch.data.status_code")}, Next: "result"},
		{ID: "result", Type: "end", Output: map[string]any{"hosts": ref("steps.parse.hosts")}},
	}}
	compiled, err := flowproto.Compile(def, map[string]tool.CallableTool{"http_contract_probe": fetch, "parse_contract_probe": parse})
	if err != nil {
		t.Fatal(err)
	}
	input, _ := json.Marshal(map[string]string{"url": server.URL})
	result, err := compiled.Tool.Call(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	var output struct {
		Output struct {
			Hosts []string `json:"hosts"`
		} `json:"output"`
	}
	if err := json.Unmarshal([]byte(toolkit.TextOf(result)), &output); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(output.Output.Hosts, []string{"a.example.test", "b.example.test"}) {
		t.Fatalf("lost typed HTTP result: %v", result)
	}
}

func TestDesignProbeHTTPPreviewCannotBeFullPipelineInput(t *testing.T) {
	body := strings.Repeat("x", 32768)
	server := nettest.Server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }), false)
	service, err := netrequest.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	out, err := service.Do(t.Context(), sdkhttp.RequestInput{URL: server.URL}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Complete || out.BodyBytes != len(body) || len(out.Body) >= len(body) || !out.PreviewTruncated {
		t.Fatalf("expected complete collection but truncated presentation: %+v", out)
	}
}

// Verify the pinned SDK's native fan-out / join with independent state fields.
// Parallel AddToolsNode message isolation is a separate production acceptance gate.
func TestDesignProbeNativeParallelJoin(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	schema := graph.NewStateSchema()
	for _, key := range []string{"left", "right"} {
		schema.AddField(key, graph.StateField{Type: reflect.TypeOf(""), Reducer: graph.DefaultReducer})
	}
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
	branch := func(key string) graph.NodeFunc {
		return func(ctx context.Context, _ graph.State) (any, error) {
			started <- struct{}{}
			select {
			case <-release:
				return graph.State{key: key}, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
	var joins atomic.Int32
	g, err := graph.NewStateGraph(schema).
		AddNode("begin", func(context.Context, graph.State) (any, error) { return graph.State{}, nil }).
		AddNode("left", branch("left")).AddNode("right", branch("right")).
		AddNode("join", func(_ context.Context, state graph.State) (any, error) {
			if state["left"] != "left" || state["right"] != "right" {
				return nil, fmt.Errorf("lost branch state: %v", state)
			}
			joins.Add(1)
			return graph.State{}, nil
		}).SetEntryPoint("begin").AddEdge("begin", "left").AddEdge("begin", "right").AddJoinEdge([]string{"left", "right"}, "join").SetFinishPoint("join").Compile()
	if err != nil {
		t.Fatal(err)
	}
	executor, err := graph.NewExecutor(g)
	if err != nil {
		t.Fatal(err)
	}
	events, err := executor.Execute(ctx, graph.State{}, &nativeagent.Invocation{InvocationID: "design-probe"})
	if err != nil {
		t.Fatal(err)
	}
	for ev := range events {
		if ev.Error != nil {
			t.Fatalf("native join failed: %+v", ev.Error)
		}
	}
	if joins.Load() != 1 {
		t.Fatalf("join ran %d times", joins.Load())
	}
}
