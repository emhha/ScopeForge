package agent

import (
	"context"
	"encoding/json"
	"strings"

	"trpc.group/trpc-go/trpc-agent-go/tool/hostexec"

	"github.com/emhha/scopeforge/toolkit"
	"trpc.group/trpc-go/trpc-agent-go/tool"
)

// Bash waits for the native host executor to finish. Interactive sessions are
// exposed separately when the role enables them; there is no second shell engine.
type nativeCommand struct{ execute, poll tool.CallableTool }

func (t *nativeCommand) Declaration() *tool.Declaration {
	return toolkit.Named(t.execute, "Bash").Declaration()
}
func (t *nativeCommand) Call(ctx context.Context, args []byte) (any, error) {
	result, err := t.execute.Call(ctx, args)
	if err != nil {
		return nil, err
	}
	var output strings.Builder
	for {
		value, ok := result.(map[string]any)
		if !ok {
			return result, nil
		}
		if text, ok := value["output"].(string); ok {
			output.WriteString(text)
		}
		if value["status"] != "running" {
			value["output"] = toolkit.Capture(ctx, output.String())
			return value, nil
		}
		id, _ := value["session_id"].(string)
		poll, _ := json.Marshal(map[string]any{"session_id": id, "chars": "", "yield_time_ms": 1000})
		result, err = t.poll.Call(ctx, poll)
		if err != nil {
			return nil, err
		}
	}
}

func ExecuteCommand(ctx context.Context, args []byte) (any, error) {
	workspace := toolkit.WorkspaceFrom(ctx)
	env := map[string]string{}
	for _, entry := range workspace.Env {
		if k, v, ok := strings.Cut(entry, "="); ok {
			env[k] = v
		}
	}
	set, err := hostexec.NewToolSet(hostexec.WithBaseDir(workspace.Directory), hostexec.WithBaseEnv(env))
	if err != nil {
		return nil, err
	}
	defer set.Close()
	var execute, poll tool.CallableTool
	for _, item := range set.Tools(ctx) {
		switch item.Declaration().Name {
		case "exec_command":
			execute = item.(tool.CallableTool)
		case "write_stdin":
			poll = item.(tool.CallableTool)
		}
	}
	return (&nativeCommand{execute: execute, poll: poll}).Call(ctx, args)
}
