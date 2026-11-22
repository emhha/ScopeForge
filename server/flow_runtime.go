package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/emhha/scopeforge/agent"
	"github.com/emhha/scopeforge/db"
	"github.com/emhha/scopeforge/internal/flowproto"
	"github.com/emhha/scopeforge/sessionctx"
	"github.com/emhha/scopeforge/skillcatalog"
	"github.com/emhha/scopeforge/toolkit"
	"github.com/google/uuid"
	nativeagent "trpc.group/trpc-go/trpc-agent-go/agent"
	"trpc.group/trpc-go/trpc-agent-go/event"
	actool "trpc.group/trpc-go/trpc-agent-go/tool"
	"trpc.group/trpc-go/trpc-agent-go/tool/file"
)

type workflowContextKey struct{}
type workflowContext struct {
	ID    string
	Stack []int64
	Key   string
}

func (s *Server) flowToolPool(ctx context.Context, key string) (map[string]actool.CallableTool, func(), error) {
	ctx = withWorkflowSurface(ctx)
	surface, restricted := ctx.Value(workflowSurfaceKey{}).(workflowSurface)
	pool := s.flowPool()
	for name, item := range pool {
		if wrapper, ok := item.(*nativeWorkflowTool); ok {
			copy := *wrapper
			copy.key = key
			pool[name] = &copy
		}
	}
	cleanup := []func(){}
	closeAll := func() {
		for _, close := range cleanup {
			close()
		}
	}
	workDir := toolkit.WorkspaceFrom(ctx).Directory
	if workDir == "" {
		workDir = s.m.dir
	}
	files, fileErr := file.NewToolSet(file.WithBaseDir(workDir))
	if fileErr != nil {
		return nil, closeAll, fileErr
	}
	cleanup = append(cleanup, func() { _ = files.Close() })
	names := map[string]string{"read_file": "Read", "save_file": "Write", "replace_content": "Edit", "list_file": "LS", "search_content": "Grep", "search_file": "Glob", "read_multiple_files": "ReadMultiple"}
	for _, item := range files.Tools(ctx) {
		if callable, ok := item.(actool.CallableTool); ok {
			if name := names[item.Declaration().Name]; name != "" {
				pool[name] = toolkit.Named(callable, name)
			}
		}
	}
	if restricted {
		for name, item := range surface.Tools {
			if !workflowMetaTool(name) {
				pool[name] = item
			}
		}
		_, bashAllowed := surface.Tools["Bash"]
		for name, item := range pool {
			_, visible := surface.Tools[name]
			pure := name == "inspect" || name == "merge_subdomains"
			hostAdapter := bashAllowed && (name == "workflow_command" || name == "subfinder" || name == "amass" || name == "httpx" || name == "ehole")
			if !visible && !pure && !hostAdapter {
				row, _ := s.m.pg.GetTool(name)
				if !bashAllowed || row == nil || row.Kind != "shell" {
					delete(pool, name)
					continue
				}
			}
			if surface.Filter != nil && !surface.Filter(ctx, item) {
				delete(pool, name)
			}
		}
	}
	rows, err := s.m.pg.ListTools()
	if err != nil {
		return nil, closeAll, err
	}
	bindings := map[string]*db.Tool{}
	for _, row := range rows {
		bindings[row.Key] = row
	}
	for name := range pool {
		if row := bindings[name]; row != nil && (!row.Enabled || (key != "" && !contains(row.Agents, key))) {
			delete(pool, name)
		}
	}
	for name, item := range pool {
		if row := bindings[name]; row != nil {
			var schema map[string]any
			_ = json.Unmarshal(row.Schema, &schema)
			pool[name] = agent.DecorateTool(item, row.Description, schema)
		}
	}
	// Host execution is a caller capability, including environment-hint adapters.
	if bash := bindings["Bash"]; bash != nil && (!bash.Enabled || (key != "" && !contains(bash.Agents, key))) {
		delete(pool, "Bash")
		delete(pool, "workflow_command")
		for _, row := range rows {
			if row.Kind == "shell" {
				delete(pool, row.Key)
			}
		}
		for _, name := range []string{"subfinder", "amass", "httpx", "ehole"} {
			delete(pool, name)
		}
	}
	allowed := map[int64]bool{}
	if key != "" {
		role, err := s.m.pg.GetAgentByKey(key)
		if err != nil || role == nil {
			return nil, closeAll, fmt.Errorf("unknown agent %s", key)
		}
		ids, _ := s.m.pg.AgentVisible(role.ID, "mcp")
		for _, id := range ids {
			allowed[id] = true
		}
		if !restricted {
			visible, _ := s.m.pg.AgentSkillNames(role.ID)
			set := map[string]bool{}
			for _, name := range visible {
				set[name] = true
			}
			if catalog, err := skillcatalog.Load(s.skillDir); err == nil && catalog != nil {
				servers, _ := s.m.pg.ListMCP()
				for _, entry := range catalog.Filter(set).List() {
					for _, dependency := range entry.MCPs {
						for _, server := range servers {
							if server.Name == dependency {
								allowed[server.ID] = true
							}
						}
					}
				}
			}
		}
	}
	servers, err := s.m.pg.ListMCP()
	if err != nil {
		return nil, closeAll, err
	}
	for _, server := range servers {
		if restricted {
			break
		} // The originating Agent already owns these connections.
		if !server.Enabled || (key != "" && !allowed[server.ID]) {
			continue
		}
		client, err := connectMCP(ctx, server)
		if err != nil {
			continue
		}
		cleanup = append(cleanup, func() { _ = client.Close() })
		items, err := client.Tools(ctx)
		if err != nil {
			continue
		}
		for _, item := range items {
			if pool[item.Declaration().Name] == nil {
				pool[item.Declaration().Name] = item
			}
		}
	}
	for name, item := range pool {
		pool[name] = &workflowCapability{s: s, key: key, CallableTool: item}
	}
	return pool, closeAll, nil
}

func (s *Server) bashWorkflowTool() actool.CallableTool {
	return toolkit.New("Bash", "已有原生宿主执行器，支持 shell command；结构化输出位于 data.output/data.exit_code。输入数据优先使用 workflow_command 的 argv/stdin", objSchema(map[string]any{"command": strParam("shell 命令")}, "command"), func(ctx context.Context, raw json.RawMessage) (toolkit.Output, error) {
		result, err := agent.ExecuteCommand(ctx, raw)
		return toolkit.Output{Text: toolkit.TextOf(result), Data: result, IsError: toolkit.Failed(result)}, err
	})
}

type workflowCapability struct {
	s   *Server
	key string
	actool.CallableTool
}

func (t *workflowCapability) Call(ctx context.Context, raw []byte) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	row, err := t.s.m.pg.GetTool(t.Declaration().Name)
	if err != nil {
		return nil, err
	}
	if row != nil && (!row.Enabled || (t.key != "" && !contains(row.Agents, t.key))) {
		return nil, fmt.Errorf("tool %s was disabled or unbound", row.Key)
	}
	return t.CallableTool.Call(ctx, raw)
}

func (s *Server) commandWorkflowTool(program string) actool.CallableTool {
	name := "workflow_command"
	if program != "" {
		name = program
	}
	props := map[string]any{"program": strParam("可执行程序名称或路径"), "argv": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}, "stdin": strParam("标准输入文本"), "stdin_artifact": strParam("当前运行的工件 ID"), "parse": map[string]any{"type": "string", "enum": []string{"text", "json", "lines"}}}
	required := []string{}
	if program == "" {
		required = []string{"program"}
	}
	return toolkit.New(name, "通用命令工具：argv 逐项引用，stdin 可接上游文本或工件，输出 data.stdout/data.stderr/data.value 和工件；继承 Bash 能力", objSchema(props, required...), func(ctx context.Context, raw json.RawMessage) (toolkit.Output, error) {
		var in struct {
			Program  string   `json:"program"`
			Argv     []string `json:"argv"`
			Stdin    string   `json:"stdin"`
			Artifact string   `json:"stdin_artifact"`
			Parse    string   `json:"parse"`
		}
		if err := json.Unmarshal(raw, &in); err != nil {
			return toolkit.Output{}, err
		}
		if program != "" {
			in.Program = program
		}
		if strings.TrimSpace(in.Program) == "" || strings.ContainsRune(in.Program, 0) {
			return toolkit.Output{}, fmt.Errorf("program is required")
		}
		input := []byte(in.Stdin)
		if in.Artifact != "" {
			store := toolkit.ArtifactsFrom(ctx)
			if store == nil {
				return toolkit.Output{}, fmt.Errorf("artifact store unavailable")
			}
			var err error
			input, err = store.Read(ctx, in.Artifact)
			if err != nil {
				return toolkit.Output{}, err
			}
		}
		dir, err := os.MkdirTemp("", "scopeforge-command-")
		if err != nil {
			return toolkit.Output{}, err
		}
		defer os.RemoveAll(dir)
		if err := os.WriteFile(filepath.Join(dir, "stdin"), input, 0600); err != nil {
			return toolkit.Output{}, err
		}
		argv := append([]string{in.Program}, in.Argv...)
		for i := range argv {
			argv[i] = shellQuote(argv[i])
		}
		command := strings.Join(argv, " ") + " < " + shellQuote(filepath.Join(dir, "stdin")) + " > " + shellQuote(filepath.Join(dir, "stdout")) + " 2> " + shellQuote(filepath.Join(dir, "stderr"))
		request, _ := json.Marshal(map[string]any{"command": command})
		result, err := s.executeWorkflowHost(ctx, request)
		if err != nil {
			return toolkit.Output{}, err
		}
		stdout, readErr := os.ReadFile(filepath.Join(dir, "stdout"))
		if readErr != nil {
			return toolkit.Output{}, readErr
		}
		stderr, _ := os.ReadFile(filepath.Join(dir, "stderr"))
		if len(stdout) > 32<<20 || len(stderr) > 32<<20 {
			return toolkit.Output{}, fmt.Errorf("command output exceeds 32 MiB")
		}
		var value any = string(stdout)
		switch in.Parse {
		case "json":
			if !toolkit.Failed(result) {
				if err := json.Unmarshal(stdout, &value); err != nil {
					return toolkit.Output{}, fmt.Errorf("stdout is not JSON: %w", err)
				}
			}
		case "lines":
			value = strings.FieldsFunc(string(stdout), func(r rune) bool { return r == '\n' || r == '\r' })
		case "", "text":
		default:
			return toolkit.Output{}, fmt.Errorf("unknown output parser")
		}
		data := map[string]any{"stdout": string(stdout), "stderr": string(stderr), "value": value, "execution": result}
		if store := toolkit.ArtifactsFrom(ctx); store != nil {
			a, err := store.Put(ctx, stdout, "text/plain", true)
			if err != nil {
				return toolkit.Output{}, err
			}
			data["stdout_artifact"] = a
		}
		return toolkit.Output{Text: toolkit.Capture(ctx, string(stdout)+string(stderr)), Data: data, IsError: toolkit.Failed(result)}, nil
	})
}

type nativeWorkflowTool struct {
	s     *Server
	row   *db.Tool
	key   string
	entry bool
}

func (s *Server) publishedWorkflowTool(row *db.Tool) actool.CallableTool {
	return &nativeWorkflowTool{s: s, row: row}
}
func (t *nativeWorkflowTool) Declaration() *actool.Declaration {
	if t.entry {
		return &actool.Declaration{Name: "workflow_run", Description: "生成并执行临时原生 Graph 工作流，或运行已授权发布版本。临时流程不注册工具，所有节点过程/结果落盘，流式返回内层事件。definition 与 workflow_id 二选一。使用 workflow_validate 检查工具及引用。", InputSchema: toolkit.Schema(objSchema(map[string]any{"definition": flowproto.DefinitionSchema(), "workflow_id": map[string]any{"type": "integer"}, "version_id": map[string]any{"type": "integer"}, "input": map[string]any{"type": "object"}}, "input"))}
	}
	return &actool.Declaration{Name: t.row.Key, Description: t.row.Description, InputSchema: toolkit.Schema(ensureSchema(t.row.Schema))}
}
func (t *nativeWorkflowTool) StructuredStreamErrors() bool { return true }
func (t *nativeWorkflowTool) Call(ctx context.Context, raw []byte) (any, error) {
	reader, err := t.StreamableCall(ctx, raw)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	var result any
	for {
		chunk, err := reader.Recv()
		if err == io.EOF {
			if text, ok := result.(string); ok {
				var value any
				if json.Unmarshal([]byte(text), &value) == nil {
					return value, nil
				}
			}
			return result, nil
		}
		if err != nil {
			return nil, err
		}
		if ev, ok := chunk.Content.(*event.Event); ok {
			// A synchronous ToolsNode consumer must acknowledge the SDK's
			// append barriers; there is no outer Runner draining this child stream.
			if ev.RequiresCompletion {
				if inv, ok := nativeagent.InvocationFromContext(ctx); ok && inv != nil {
					if err := inv.NotifyCompletion(ctx, nativeagent.GetAppendEventNoticeKey(ev.ID)); err != nil {
						return nil, err
					}
				}
			}
		} else {
			result = chunk.Content
		}
	}
}
func (t *nativeWorkflowTool) StreamableCall(ctx context.Context, raw []byte) (*actool.StreamReader, error) {
	var def flowproto.Definition
	flowID, version := int64(0), int64(0)
	input := json.RawMessage(raw)
	source := "tool"
	if t.entry {
		var in struct {
			Definition *flowproto.Definition `json:"definition"`
			ID         int64                 `json:"workflow_id"`
			Version    int64                 `json:"version_id"`
			Input      json.RawMessage       `json:"input"`
		}
		if err := json.Unmarshal(raw, &in); err != nil {
			return nil, err
		}
		if (in.Definition == nil) == (in.ID == 0) {
			return nil, fmt.Errorf("supply exactly one of definition and workflow_id")
		}
		input = in.Input
		if in.Definition != nil {
			def = *in.Definition
			source = "inline"
		} else {
			row, err := t.s.m.pg.GetFlow(in.ID)
			if err != nil {
				return nil, err
			}
			if len(row.Published) == 0 {
				return nil, fmt.Errorf("workflow is not published")
			}
			toolRow, err := t.s.m.pg.GetTool("flow_" + row.Name)
			if err != nil || toolRow == nil || toolRow.Kind != "workflow" || !toolRow.Enabled || (t.key != "" && !contains(toolRow.Agents, t.key)) {
				return nil, fmt.Errorf("workflow is not permitted")
			}
			var publication struct {
				FlowID  int64 `json:"flow_id"`
				Version int64 `json:"version_id"`
			}
			if err := json.Unmarshal(toolRow.Exec, &publication); err != nil || publication.FlowID != in.ID {
				return nil, fmt.Errorf("workflow registration mismatch")
			}
			if in.Version == 0 {
				in.Version = publication.Version
			}
			{
				v, err := t.s.m.pg.GetFlowVersion(in.Version)
				if err != nil || v.FlowID != in.ID {
					return nil, fmt.Errorf("invalid workflow version")
				}
				_ = json.Unmarshal(v.Definition, &def)
				version = v.ID
			}
			flowID = in.ID
		}
	} else {
		current, err := t.s.m.pg.GetTool(t.row.Key)
		if err != nil || current == nil || !current.Enabled || (t.key != "" && !contains(current.Agents, t.key)) {
			return nil, fmt.Errorf("workflow tool disabled or not permitted")
		}
		var spec struct {
			ID      int64 `json:"flow_id"`
			Version int64 `json:"version_id"`
		}
		if err := json.Unmarshal(t.row.Exec, &spec); err != nil {
			return nil, err
		}
		v, err := t.s.m.pg.GetFlowVersion(spec.Version)
		if err != nil || v.FlowID != spec.ID {
			return nil, fmt.Errorf("workflow version unavailable")
		}
		if err := json.Unmarshal(v.Definition, &def); err != nil {
			return nil, err
		}
		flowID, version = spec.ID, spec.Version
	}
	id, reader, finish, err := t.s.beginWorkflow(ctx, flowID, version, def, input, source, t.key)
	if err != nil {
		return nil, err
	}
	stream := actool.NewStream(32)
	go func() {
		defer stream.Writer.Close()
		result, err := t.s.pipeWorkflow(ctx, id, reader, func(chunk actool.StreamChunk) bool { return !stream.Writer.Send(chunk, nil) })
		finish(result, err)
		if err != nil {
			stream.Writer.Send(actool.StreamChunk{}, fmt.Errorf("workflow run %s: %w", id, err))
			return
		}
		stream.Writer.Send(actool.StreamChunk{Content: withFlowRunID(result, id)}, nil)
	}()
	return stream.Reader, nil
}
func withFlowRunID(result, id string) string {
	var value map[string]any
	if json.Unmarshal([]byte(result), &value) != nil {
		value = map[string]any{"output": result}
	}
	value["run_id"] = id
	out, _ := json.Marshal(value)
	return string(out)
}

func (s *Server) beginWorkflow(parent context.Context, flowID, version int64, def flowproto.Definition, input json.RawMessage, source, key string) (string, *actool.StreamReader, func(string, error), error) {
	parent = withWorkflowSurface(parent)
	ancestry, _ := parent.Value(workflowContextKey{}).(workflowContext)
	if len(ancestry.Stack) >= 8 {
		return "", nil, nil, fmt.Errorf("maximum workflow nesting depth reached")
	}
	if flowID != 0 {
		for _, id := range ancestry.Stack {
			if id == flowID {
				return "", nil, nil, fmt.Errorf("recursive workflow invocation rejected")
			}
		}
	}
	if def.Version == 0 {
		def.Version = 1
	}
	if def.Name == "" {
		def.Name = "temporary"
	}
	if err := s.checkWorkflowCycles(flowID, def, map[int64]bool{}); err != nil {
		return "", nil, nil, err
	}
	ctx, cancel := context.WithTimeout(parent, 5*time.Minute)
	pool, cleanup, err := s.flowToolPool(ctx, key)
	if err != nil {
		cancel()
		return "", nil, nil, err
	}
	compiled, err := flowproto.Compile(def, pool, toolkit.CallHooksFrom(parent))
	if err != nil {
		cleanup()
		cancel()
		return "", nil, nil, err
	}
	if err := toolkit.Validate(compiled.Tool.Declaration().InputSchema, input); err != nil {
		cleanup()
		cancel()
		return "", nil, nil, err
	}
	raw, _ := json.Marshal(def)
	id := uuid.NewString()
	s.flowCancels.Store(id, cancel)
	s.flowContexts.Store(id, ctx)
	if err := ctx.Err(); err != nil {
		cancel()
		cleanup()
		s.flowCancels.Delete(id)
		s.flowContexts.Delete(id)
		return "", nil, nil, err
	}
	info := agent.RunInfoFrom(ctx)
	if err := s.m.pg.CreateWorkflowRun(db.FlowRun{ID: id, FlowID: flowID, Definition: raw, Input: input, Source: source, AgentKey: key, TaskID: info.TaskID, SessionID: info.SessionID, ParentRunID: ancestry.ID, VersionID: version}); err != nil {
		cancel()
		cleanup()
		s.flowCancels.Delete(id)
		s.flowContexts.Delete(id)
		return "", nil, nil, err
	}

	stack := append(append([]int64{}, ancestry.Stack...), flowID)
	ctx = context.WithValue(ctx, workflowContextKey{}, workflowContext{ID: id, Stack: stack, Key: key})
	ctx = toolkit.WithArtifacts(ctx, flowArtifacts{s: s, run: id})
	if sessionctx.ID(ctx) == "" {
		ctx = sessionctx.WithID(ctx, id)
	}
	if toolkit.WorkspaceFrom(ctx).Directory == "" {
		ctx = toolkit.WithWorkspace(ctx, toolkit.Workspace{Directory: s.m.dir})
	}
	finish := func(result string, err error) {
		if err == nil {
			err = ctx.Err()
		}
		status, message := "completed", ""
		if err != nil {
			status, message = "failed", err.Error()
			if errors.Is(err, context.Canceled) {
				status = "cancelled"
			}
		}
		cancel()
		if err := s.m.pg.FinishFlowRun(id, status, result, message); err != nil {
			log.Printf("[workflow] finish %s: %v", id, err)
		}
		s.flowCancels.Delete(id)
		s.flowContexts.Delete(id)
		cleanup()
	}
	reader, err := compiled.Tool.StreamableCall(ctx, input)
	if err != nil {
		finish("", err)
		return "", nil, nil, err
	}
	return id, reader, finish, nil
}
func (s *Server) pipeWorkflow(ctx context.Context, id string, reader *actool.StreamReader, forward func(actool.StreamChunk) bool) (string, error) {
	defer reader.Close()
	if child, ok := s.flowContexts.Load(id); ok {
		ctx = child.(context.Context)
	}
	result := ""
	for {
		chunk, err := reader.Recv()
		if err == io.EOF {
			if ctx.Err() != nil {
				return result, ctx.Err()
			}
			return result, nil
		}
		if err != nil {
			return result, err
		}
		if ev, ok := chunk.Content.(*event.Event); ok {
			for _, choice := range ev.Choices {
				if choice.Message.Role == "assistant" && choice.Message.Content != "" {
					result = choice.Message.Content
				}
			}
			payload, _ := json.Marshal(ev)
			if len(ev.StateDelta) > 0 {
				var view map[string]any
				_ = json.Unmarshal(payload, &view)
				state := map[string]any{}
				for name, raw := range ev.StateDelta {
					var value any
					if json.Unmarshal(raw, &value) == nil {
						state[name] = value
					}
				}
				view["state"] = state
				payload, _ = json.Marshal(view)
			}
			message := ""
			if ev.Error != nil {
				message = ev.Error.Error()
			}
			if err := s.m.pg.AppendFlowPayload(id, ev.Object, ev.Author, message, payload); err != nil {
				return result, err
			}
			if forward != nil && !forward(chunk) {
				return result, context.Canceled
			}
			if ev.Error != nil {
				return result, ev.Error
			}
		} else {
			result = toolkit.TextOf(chunk.Content)
		}
	}
}

func (s *Server) workflowControlTools(key string) []actool.CallableTool {
	catalog := toolkit.New("workflow_catalog", "获取当前调用者允许的工作流工具 Schema、DSL 和转换操作。添加新工具后重新查询即可使用，不需要改代码", objSchema(map[string]any{"query": strParam("按工具名或说明过滤；可留空")}), func(ctx context.Context, raw json.RawMessage) (toolkit.Output, error) {
		var in struct {
			Query string `json:"query"`
		}
		_ = json.Unmarshal(raw, &in)
		pool, close, err := s.flowToolPool(ctx, key)
		if err != nil {
			return toolkit.Output{}, err
		}
		defer close()
		for name, item := range pool {
			if in.Query != "" && !strings.Contains(strings.ToLower(name+" "+item.Declaration().Description), strings.ToLower(in.Query)) {
				delete(pool, name)
			}
		}
		value := map[string]any{"tools": sortedFlowTools(pool), "definition_schema": flowproto.DefinitionSchema(), "operations": flowproto.Operations}
		b, _ := json.Marshal(value)
		return toolkit.Text(string(b)), nil
	})
	validate := toolkit.New("workflow_validate", "校验临时工作流定义和当前 Agent 的工具依赖，不执行", objSchema(map[string]any{"definition": flowproto.DefinitionSchema()}, "definition"), func(ctx context.Context, raw json.RawMessage) (toolkit.Output, error) {
		var in struct {
			Definition flowproto.Definition `json:"definition"`
		}
		if err := json.Unmarshal(raw, &in); err != nil {
			return toolkit.Output{}, err
		}
		if in.Definition.Version == 0 {
			in.Definition.Version = 1
		}
		pool, close, err := s.flowToolPool(ctx, key)
		if err != nil {
			return toolkit.Output{}, err
		}
		defer close()
		_, err = flowproto.Compile(in.Definition, pool)
		if err != nil {
			return toolkit.Error(err.Error()), nil
		}
		return toolkit.Text(`{"valid":true}`), nil
	})
	get := toolkit.New("workflow_get_run", "读取本 Agent 的工作流运行结果和过程", objSchema(map[string]any{"run_id": strParam("运行 ID")}, "run_id"), func(ctx context.Context, raw json.RawMessage) (toolkit.Output, error) {
		var in struct {
			ID string `json:"run_id"`
		}
		_ = json.Unmarshal(raw, &in)
		run, err := s.flowRun(in.ID)
		if err != nil {
			return toolkit.Output{}, err
		}
		if run.AgentKey != key || run.TaskID != agent.RunInfoFrom(ctx).TaskID || run.SessionID != agent.RunInfoFrom(ctx).SessionID {
			return toolkit.Output{}, fmt.Errorf("run is not owned by this caller")
		}
		b, _ := json.Marshal(run)
		return toolkit.Text(string(b)), nil
	})
	cancel := toolkit.New("workflow_cancel_run", "取消本 Agent 创建的工作流运行", objSchema(map[string]any{"run_id": strParam("运行 ID")}, "run_id"), func(ctx context.Context, raw json.RawMessage) (toolkit.Output, error) {
		var in struct {
			ID string `json:"run_id"`
		}
		_ = json.Unmarshal(raw, &in)
		run, err := s.flowRun(in.ID)
		if err != nil {
			return toolkit.Output{}, err
		}
		if run.AgentKey != key || run.TaskID != agent.RunInfoFrom(ctx).TaskID || run.SessionID != agent.RunInfoFrom(ctx).SessionID {
			return toolkit.Output{}, fmt.Errorf("run is not owned by this caller")
		}
		if stop, ok := s.flowCancels.Load(in.ID); ok {
			stop.(context.CancelFunc)()
		}
		return toolkit.Text("cancel requested"), nil
	})
	return []actool.CallableTool{catalog, validate, &nativeWorkflowTool{s: s, key: key, entry: true}, get, cancel}
}

func sortedFlowTools(pool map[string]actool.CallableTool) []map[string]any {
	names := []string{}
	for name := range pool {
		names = append(names, name)
	}
	sort.Strings(names)
	out := []map[string]any{}
	for _, name := range names {
		declaration := pool[name].Declaration()
		out = append(out, map[string]any{"name": declaration.Name, "description": declaration.Description, "input_schema": declaration.InputSchema, "output_schema": declaration.OutputSchema})
	}
	return out
}
