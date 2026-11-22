package flowproto

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"

	"github.com/emhha/scopeforge/toolkit"
	"github.com/google/uuid"
	"trpc.group/trpc-go/trpc-agent-go/agent"
	"trpc.group/trpc-go/trpc-agent-go/agent/graphagent"
	"trpc.group/trpc-go/trpc-agent-go/graph"
	"trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/tool"
	agenttool "trpc.group/trpc-go/trpc-agent-go/tool/agent"
)

type Compiled struct {
	Graph *graph.Graph
	Agent agent.Agent
	Tool  *Tool
}

// Tool delegates execution and streaming to the native AgentTool. The fixed
// SDK revision can close a canceled child stream without returning a Call error;
// preserve the caller's cancellation at this ordinary tool boundary.
type Tool struct{ *agenttool.Tool }

func (t *Tool) Call(ctx context.Context, args []byte) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result, err := t.Tool.Call(ctx, args)
	if cause := ctx.Err(); cause != nil {
		return nil, cause
	}
	return result, err
}

type schemaAgent struct {
	agent.Agent
	info agent.Info
}

func (a *schemaAgent) Info() agent.Info { return a.info }

func Compile(def Definition, tools map[string]tool.CallableTool) (*Compiled, error) {
	// Freeze editor data: mutating a draft cannot change an already compiled run.
	data, err := json.Marshal(def)
	if err != nil {
		return nil, err
	}
	var frozen Definition
	if err = json.Unmarshal(data, &frozen); err != nil {
		return nil, err
	}
	def = frozen
	tools = maps.Clone(tools)
	if err = def.validate(tools); err != nil {
		return nil, err
	}
	schema := graph.MessagesStateSchema()
	for _, key := range []string{"flow_input", "flow_steps"} {
		schema.AddField(key, graph.StateField{Type: reflect.TypeOf(map[string]any{}), Reducer: graph.DefaultReducer})
	}
	builder := graph.NewStateGraph(schema)
	// Public node IDs are prefixed. Compiler-owned bridge nodes cannot collide
	// with editor-provided IDs. Tools execute through AddToolsNode, not a new loop.
	nodeID := func(id string) string { return "node_" + id }
	exitID := func(n Node) string {
		if n.Type == "tool" {
			return "collect_" + n.ID
		}
		return nodeID(n.ID)
	}
	for _, n := range def.Nodes {
		id := nodeID(n.ID)
		switch n.Type {
		case "start":
			builder.AddNode(id, func(ctx context.Context, state graph.State) (any, error) {
				raw, _ := state[graph.StateKeyUserInput].(string)
				if err := toolkit.Validate(toolkit.Schema(def.InputSchema), []byte(raw)); err != nil {
					return nil, fmt.Errorf("flow input: %w", err)
				}
				var input map[string]any
				if err := json.Unmarshal([]byte(raw), &input); err != nil {
					return nil, err
				}
				return graph.State{"flow_input": input, "flow_steps": map[string]any{}}, nil
			})
		case "tool":
			selected := tools[n.Tool]
			if n.Arguments == nil {
				n.Arguments = map[string]any{}
			}
			builder.AddNode(id, func(ctx context.Context, state graph.State) (any, error) {
				args, err := resolve(n.Arguments, state)
				if err != nil {
					return nil, err
				}
				encoded, err := json.Marshal(args)
				if err != nil {
					return nil, err
				}
				if err := toolkit.Validate(selected.Declaration().InputSchema, encoded); err != nil {
					return nil, fmt.Errorf("node %s arguments: %w", n.ID, err)
				}
				message := model.Message{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{{ID: uuid.NewString(), Type: "function", Function: model.FunctionDefinitionParam{Name: n.Tool, Arguments: encoded}}}}
				return graph.State{graph.StateKeyMessages: []model.Message{message}}, nil
			})
			nativeID := "tool_" + n.ID
			builder.AddToolsNode(nativeID, map[string]tool.Tool{n.Tool: selected})
			builder.AddNode("collect_"+n.ID, func(ctx context.Context, state graph.State) (any, error) {
				raw, _ := state[graph.StateKeyLastToolResponse].(string)
				if toolkit.FailedJSON(raw) {
					return nil, fmt.Errorf("node %s tool failed: %s", n.ID, raw)
				}
				var result any
				if err := json.Unmarshal([]byte(raw), &result); err != nil {
					// Native result-message formatters may expose HTTP previews
					// as plain text. Preserve that content under the same text
					// field used by toolkit.Output; never infer new evidence.
					result = map[string]any{"text": raw}
				}
				steps, _ := state["flow_steps"].(map[string]any)
				next := maps.Clone(steps)
				if next == nil {
					next = map[string]any{}
				}
				next[n.ID] = result
				return graph.State{"flow_steps": next}, nil
			})
			builder.AddEdge(id, nativeID).AddEdge(nativeID, "collect_"+n.ID)
		case "branch":
			builder.AddNode(id, func(context.Context, graph.State) (any, error) { return graph.State{}, nil })
		case "end":
			builder.AddNode(id, func(ctx context.Context, state graph.State) (any, error) {
				output, err := resolve(n.Output, state)
				if err != nil {
					return nil, err
				}
				result, err := json.Marshal(map[string]any{"flow": def.Name, "version": def.Version, "output": output})
				if err != nil {
					return nil, err
				}
				return graph.State{graph.StateKeyLastResponse: string(result)}, nil
			}).SetFinishPoint(id)
		}
	}
	for _, n := range def.Nodes {
		if n.Type == "branch" {
			id := nodeID(n.ID)
			builder.AddConditionalEdges(id, func(ctx context.Context, state graph.State) (string, error) {
				actual, err := lookup(n.Condition.Ref, state)
				if err != nil {
					return "", err
				}
				if reflect.DeepEqual(actual, n.Condition.Equals) {
					return "then", nil
				}
				return "else", nil
			}, map[string]string{"then": nodeID(n.Condition.Then), "else": nodeID(n.Condition.Else)})
		}
		if n.Type != "end" && n.Type != "branch" {
			builder.AddEdge(exitID(n), nodeID(n.Next))
		}
	}
	g, err := builder.SetEntryPoint(nodeID(def.Entry)).Compile()
	if err != nil {
		return nil, err
	}
	native, err := graphagent.New("flow_"+def.Name, g, graphagent.WithDescription(def.Description))
	if err != nil {
		return nil, err
	}
	wrapped := &schemaAgent{Agent: native, info: agent.Info{Name: "flow_" + def.Name, Description: def.Description, InputSchema: def.InputSchema}}
	callable := agenttool.NewTool(wrapped, agenttool.WithStreamInner(true), agenttool.WithResponseMode(agenttool.ResponseModeFinalOnly))
	return &Compiled{Graph: g, Agent: wrapped, Tool: &Tool{Tool: callable}}, nil
}
