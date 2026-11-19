package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/emhha/scopeforge/runstate"
	"github.com/emhha/scopeforge/toolkit"
	"github.com/google/uuid"
	"trpc.group/trpc-go/trpc-agent-go/agent/llmagent"
	"trpc.group/trpc-go/trpc-agent-go/event"
	"trpc.group/trpc-go/trpc-agent-go/knowledge"
	"trpc.group/trpc-go/trpc-agent-go/memory"
	"trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/runner"
	"trpc.group/trpc-go/trpc-agent-go/session"
	"trpc.group/trpc-go/trpc-agent-go/tool"
	"trpc.group/trpc-go/trpc-agent-go/tool/file"
	"trpc.group/trpc-go/trpc-agent-go/tool/hostexec"
	"trpc.group/trpc-go/trpc-agent-go/tool/todo"
)

// RunConfig contains product role policies. The native Runner owns the model/tool loop.
type RunConfig struct {
	AgentOptions                   []llmagent.Option
	Model                          model.Model
	Instructions                   []string
	Tools                          []tool.CallableTool
	Sessions                       session.Service
	SessionID                      string
	WorkingDir, ToolOutputDir      string
	BashEnv                        []string
	MaxTurns, MaxTokens            int
	MaxDuration                    time.Duration
	NonStreaming, DisableHostTools bool
	Hooks                          *tool.Callbacks
	AugmentCallbacks               *tool.Callbacks
	InteractiveShell               bool
	Settlement                     *runstate.Wrapup
	// Reserved native dependencies. Nil leaves both features unmounted.
	Memory    memory.Service
	Knowledge knowledge.Knowledge

	EnableWebFetch, EnableWebSearch                                  bool
	WebFetchProxy, WebFetchCACert                                    string
	WebSearchBackend, WebSearchProxy                                 string
	BraveSearchAPIKey, TavilySearchAPIKey                            string
	DeepSeekSearchBaseURL, DeepSeekSearchAPIKey, DeepSeekSearchModel string
}

type turn struct {
	models  sync.WaitGroup
	usageMu sync.Mutex
	usage   model.Usage
	config  RunConfig
	service session.Service
	key     session.Key
	runner  runner.Runner
	closers []func() error
	owned   bool
}

func newTurn(config RunConfig) *turn {
	svc := config.Sessions
	owned := svc == nil
	if svc == nil {
		svc = NewSessionService()
	}
	id := config.SessionID
	if id == "" {
		id = uuid.NewString()
	}
	return &turn{config: config, service: svc, key: session.Key{AppName: "scopeforge", UserID: "operator", SessionID: id}, owned: owned}
}
func (t *turn) History(ctx context.Context) []model.Message {
	sess, err := t.service.GetSession(ctx, t.key)
	if err != nil || sess == nil {
		return nil
	}
	var out []model.Message
	for _, ev := range sess.GetEvents() {
		if ev.Response != nil && !ev.IsPartial {
			for _, c := range ev.Choices {
				if c.Message.Role != "" {
					out = append(out, c.Message)
				}
			}
		}
	}
	return out
}
func (t *turn) Close() error {
	if t.runner != nil {
		_ = t.runner.Close()
	}
	for _, close := range t.closers {
		_ = close()
	}
	if t.owned {
		return t.service.Close()
	}
	return nil
}
func (t *turn) Run(ctx context.Context, input string, settling bool) (<-chan *event.Event, error) {
	cfg := t.config
	if cfg.Model == nil {
		return nil, fmt.Errorf("model is required")
	}
	tools := make([]tool.Tool, 0, len(cfg.Tools)+12)
	for _, item := range cfg.Tools {
		tools = append(tools, item)
	}
	if !cfg.DisableHostTools && !settling {
		env := map[string]string{}
		for _, pair := range cfg.BashEnv {
			if k, v, ok := strings.Cut(pair, "="); ok {
				env[k] = v
			}
		}
		host, err := hostexec.NewToolSet(hostexec.WithBaseDir(cfg.WorkingDir), hostexec.WithBaseEnv(env))
		if err != nil {
			return nil, err
		}
		var execTool, pollTool, killTool tool.CallableTool
		for _, item := range host.Tools(ctx) {
			switch item.Declaration().Name {
			case "exec_command":
				execTool = item.(tool.CallableTool)
			case "write_stdin":
				pollTool = item.(tool.CallableTool)
			case "kill_session":
				killTool = item.(tool.CallableTool)
			}
		}
		tools = append(tools, &nativeCommand{execute: execTool, poll: pollTool})
		if cfg.InteractiveShell {
			tools = append(tools, toolkit.Named(execTool, "shell_open"), toolkit.Named(pollTool, "shell_send"), toolkit.Named(killTool, "shell_close"))
		}
		if c, ok := host.(interface{ Close() error }); ok {
			t.closers = append(t.closers, c.Close)
		}
		files, err := file.NewToolSet(file.WithBaseDir(cfg.WorkingDir))
		if err != nil {
			return nil, err
		}
		names := map[string]string{"read_file": "Read", "save_file": "Write", "replace_content": "Edit", "list_file": "LS", "search_content": "Grep", "search_file": "Glob", "read_multiple_files": "ReadMultiple"}
		for _, nativeTool := range files.Tools(ctx) {
			callable := nativeTool.(tool.CallableTool)
			if name := names[nativeTool.Declaration().Name]; name != "" {
				callable = toolkit.Named(callable, name)
			}
			tools = append(tools, callable)
		}
		tools = append(tools, todo.New())
		if cfg.EnableWebFetch && !hasTool(cfg.Tools, "http_request") {
			tools = append(tools, fetchTool(cfg.WebFetchProxy, cfg.WebFetchCACert))
		}
		if cfg.EnableWebSearch {
			tools = append(tools, searchTool(WebSearchOpts{Backend: cfg.WebSearchBackend, BraveKey: cfg.BraveSearchAPIKey, TavilyKey: cfg.TavilySearchAPIKey, DeepSeekBaseURL: cfg.DeepSeekSearchBaseURL, DeepSeekAPIKey: cfg.DeepSeekSearchAPIKey, DeepSeekModel: cfg.DeepSeekSearchModel, Proxy: cfg.WebSearchProxy}))
		}
	}
	if settling && cfg.Settlement != nil {
		drop := map[string]bool{"Bash": true, "shell_send": true, "shell_close": true, "exec_command": true, "write_stdin": true, "kill_session": true}
		for _, n := range cfg.Settlement.DisabledTools {
			drop[n] = true
		}
		filtered := tools[:0]
		for _, item := range tools {
			if !drop[item.Declaration().Name] {
				filtered = append(filtered, item)
			}
		}
		tools = filtered
	}
	maxCalls := cfg.MaxTurns
	if settling && cfg.Settlement != nil {
		maxCalls = cfg.Settlement.MaxTurns
	}
	gen := model.GenerationConfig{Stream: !cfg.NonStreaming}
	if cfg.MaxTokens > 0 {
		gen.MaxTokens = &cfg.MaxTokens
	}
	opts := []llmagent.Option{llmagent.WithModel(&usageModel{inner: cfg.Model, turn: t}), llmagent.WithInstruction(strings.Join(cfg.Instructions, "\n\n")), llmagent.WithTools(tools), llmagent.WithGenerationConfig(gen), llmagent.WithToolCallbacks(t.callbacks()), llmagent.WithMaxLLMCalls(maxCalls), llmagent.WithEnableParallelTools(false), llmagent.WithAddSessionSummary(true), llmagent.WithSyncSummaryIntraRun(true)}
	opts = append(opts, cfg.AgentOptions...)
	if cfg.Knowledge != nil {
		opts = append(opts, llmagent.WithKnowledge(cfg.Knowledge))
	}
	ag := llmagent.New("scopeforge", opts...)
	ropts := []runner.Option{runner.WithSessionService(t.service)}
	if cfg.Memory != nil {
		ropts = append(ropts, runner.WithMemoryService(cfg.Memory))
	}
	if t.runner != nil {
		_ = t.runner.Close()
	}
	t.runner = runner.NewRunner(t.key.AppName, ag, ropts...)
	ctx = context.WithValue(ctx, summaryModelKey{}, cfg.Model)
	ctx = toolkit.WithWorkspace(ctx, toolkit.Workspace{Directory: cfg.WorkingDir, Env: cfg.BashEnv, OutputDirectory: cfg.ToolOutputDir})
	return t.runner.Run(ctx, t.key.UserID, t.key.SessionID, model.Message{Role: model.RoleUser, Content: input})
}
