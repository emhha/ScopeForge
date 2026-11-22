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

func Compile(def Definition, tools map[string]tool.CallableTool, hooks ...*tool.Callbacks) (*Compiled, error) {
	callbacks := tool.NewCallbacks()
	if len(hooks) > 0 && hooks[0] != nil {
		callbacks.BeforeTool = append(callbacks.BeforeTool, hooks[0].BeforeTool...)
		callbacks.AfterTool = append(callbacks.AfterTool, hooks[0].AfterTool...)
	}
	// ToolsNode errors can end a child without a final response. Normalize them
	// into an explicit failed result so collect reliably fails the parent graph.
	callbacks.RegisterAfterTool(func(_ context.Context, args *tool.AfterToolArgs) (*tool.AfterToolResult, error) {
		if args.Error != nil {
			return &tool.AfterToolResult{CustomResult: toolkit.Error(args.Error.Error())}, nil
		}
		return nil, nil
	})
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
	schema.AddField("flow_input", graph.StateField{Type: reflect.TypeOf(map[string]any{}), Reducer: graph.DefaultReducer})
	schema.AddField("flow_steps", graph.StateField{Type: reflect.TypeOf(map[string]any{}), Reducer: func(existing, update any) any {
		old, _ := existing.(map[string]any)
		next := maps.Clone(old)
		if next == nil {
			next = map[string]any{}
		}
		delta, _ := update.(map[string]any)
		for k, v := range delta {
			next[k] = v
		}
		return next
	}})
	children := []agent.Agent{}
	builder := graph.NewStateGraph(schema)
	// Public node IDs are prefixed. Compiler-owned bridge nodes cannot collide
	// with editor-provided IDs. Tools execute through AddToolsNode, not a new loop.
	nodeID := func(id string) string { return "node_" + id }
	exitID := func(n Node) string {
		if n.Type == "tool" {
			return "tool_" + n.ID
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
			argsKey := "args_" + n.ID
			schema.AddField(argsKey, graph.StateField{Type: reflect.TypeOf(""), Reducer: graph.DefaultReducer})
			builder.AddNode(id, func(ctx context.Context, state graph.State) (any, error) {
				args := n.Arguments
				if args == nil {
					args = map[string]any{}
				}
				value, err := resolve(args, state)
				if err != nil {
					return nil, err
				}
				if object, ok := value.(map[string]any); ok {
					object = maps.Clone(object)
					value = object
					if schema := selected.Declaration().InputSchema; schema != nil {
						for name, property := range schema.Properties {
							if property != nil && property.Default != nil && (property.Type == "string" || property.Type == "integer" || property.Type == "number" || property.Type == "boolean") {
								current, exists := object[name]
								if !exists || current == nil || current == "" {
									object[name] = property.Default
								}
							}
						}
					}
				}
				raw, err := json.Marshal(value)
				if err != nil {
					return nil, err
				}
				if err := toolkit.Validate(selected.Declaration().InputSchema, raw); err != nil {
					return nil, fmt.Errorf("node %s arguments: %w", n.ID, err)
				}
				return graph.State{argsKey: string(raw)}, nil
			})
			childBuilder := graph.NewStateGraph(graph.MessagesStateSchema())
			childBuilder.AddNode("prepare", func(_ context.Context, state graph.State) (any, error) {
				raw, _ := state[graph.StateKeyUserInput].(string)
				message := model.Message{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{{ID: uuid.NewString(), Type: "function", Function: model.FunctionDefinitionParam{Name: n.Tool, Arguments: []byte(raw)}}}}
				return graph.State{graph.StateKeyMessages: []model.Message{message}}, nil
			})
			childBuilder.AddToolsNode("execute", map[string]tool.Tool{n.Tool: selected}, graph.WithToolCallbacks(callbacks))
			childBuilder.AddNode("collect", func(_ context.Context, state graph.State) (any, error) {
				raw, _ := state[graph.StateKeyLastToolResponse].(string)
				if toolkit.FailedJSON(raw) {
					return nil, fmt.Errorf("node %s tool failed: %s", n.ID, raw)
				}
				var value any
				if json.Unmarshal([]byte(raw), &value) != nil {
					value = map[string]any{"text": raw}
				}
				encoded, err := json.Marshal(value)
				if err != nil {
					return nil, err
				}
				return graph.State{graph.StateKeyLastResponse: string(encoded)}, nil
			})
			childGraph, err := childBuilder.SetEntryPoint("prepare").AddEdge("prepare", "execute").AddEdge("execute", "collect").SetFinishPoint("collect").Compile()
			if err != nil {
				return nil, err
			}
			childName := "tool_" + n.ID
			child, err := graphagent.New(childName, childGraph)
			if err != nil {
				return nil, err
			}
			children = append(children, child)
			builder.AddSubgraphNode(childName, graph.WithUserInputKey(argsKey), graph.WithAgentNodeRunOptions(agent.WithPropagateChildAgentErrors(true)), graph.WithSubgraphIsolatedMessages(true), graph.WithSubgraphInputMapper(func(parent graph.State) graph.State { return graph.State{graph.StateKeyUserInput: parent[argsKey]} }), graph.WithSubgraphOutputMapper(func(_ graph.State, result graph.SubgraphResult) graph.State {
				var value any
				_ = json.Unmarshal([]byte(result.LastResponse), &value)
				return graph.State{"flow_steps": map[string]any{n.ID: value}}
			}))
			builder.AddEdge(id, childName)
		case "parallel", "join":
			builder.AddNode(id, func(context.Context, graph.State) (any, error) { return graph.State{}, nil })
		case "transform":
			builder.AddNode(id, func(ctx context.Context, state graph.State) (any, error) {
				args, err := resolve(n.Arguments, state)
				if err != nil {
					return nil, err
				}
				arguments, _ := args.(map[string]any)
				if arguments == nil {
					arguments = map[string]any{}
				}
				value, err := Transform(ctx, n.Operation, arguments)
				if err != nil {
					return nil, fmt.Errorf("node %s: %w", n.ID, err)
				}
				return graph.State{"flow_steps": map[string]any{n.ID: map[string]any{"data": value}}}, nil
			})
		case "branch":
			builder.AddNode(id, func(context.Context, graph.State) (any, error) { return graph.State{}, nil })
		case "end":
			builder.AddNode(id, func(ctx context.Context, state graph.State) (any, error) {
				output, err := resolve(n.Output, state)
				if err != nil {
					return nil, err
				}
				if def.OutputSchema != nil {
					b, _ := json.Marshal(output)
					if err := toolkit.Validate(toolkit.Schema(def.OutputSchema), b); err != nil {
						return nil, fmt.Errorf("output schema: %w", err)
					}
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
		if n.Type == "parallel" {
			for _, branch := range n.Branches {
				builder.AddEdge(nodeID(n.ID), nodeID(branch))
			}
			continue
		}
		if n.Type == "join" {
			sources := []string{}
			for _, from := range n.From {
				for _, candidate := range def.Nodes {
					if candidate.ID == from {
						sources = append(sources, exitID(candidate))
					}
				}
			}
			builder.AddJoinEdge(sources, nodeID(n.ID))
		}
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
			isJoin := false
			for _, target := range def.Nodes {
				if target.ID == n.Next && target.Type == "join" {
					isJoin = true
				}
			}
			if !isJoin {
				builder.AddEdge(exitID(n), nodeID(n.Next))
			}
		}
	}
	g, err := builder.SetEntryPoint(nodeID(def.Entry)).Compile()
	if err != nil {
		return nil, err
	}
	native, err := graphagent.New("flow_"+def.Name, g, graphagent.WithDescription(def.Description), graphagent.WithSubAgents(children), graphagent.WithExecutorOptions(graph.WithMaxSteps(512), graph.WithMaxConcurrency(8)))
	if err != nil {
		return nil, err
	}
	wrapped := &schemaAgent{Agent: native, info: agent.Info{Name: "flow_" + def.Name, Description: def.Description, InputSchema: def.InputSchema}}
	callable := agenttool.NewTool(wrapped, agenttool.WithStreamInner(true), agenttool.WithResponseMode(agenttool.ResponseModeFinalOnly))
	return &Compiled{Graph: g, Agent: wrapped, Tool: &Tool{Tool: callable}}, nil
}
