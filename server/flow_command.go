package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/emhha/scopeforge/agent"
	"github.com/emhha/scopeforge/toolkit"
)

// Keep the custom command template's existing semantics while separating its
// full data output from the model-visible console preview.
func (s *Server) runWorkflowShell(ctx context.Context, command string) (toolkit.Output, error) {
	dir, err := os.MkdirTemp("", "scopeforge-flow-shell-")
	if err != nil {
		return toolkit.Output{}, err
	}
	defer os.RemoveAll(dir)
	stdoutPath, stderrPath := filepath.Join(dir, "stdout"), filepath.Join(dir, "stderr")
	raw, _ := json.Marshal(map[string]any{"command": "(\n" + command + "\n) > " + shellQuote(stdoutPath) + " 2> " + shellQuote(stderrPath)})
	result, err := agent.ExecuteCommand(ctx, raw)
	if err != nil {
		return toolkit.Output{}, err
	}
	if err := ctx.Err(); err != nil {
		return toolkit.Output{}, err
	}
	stdout, err := os.ReadFile(stdoutPath)
	if err != nil {
		return toolkit.Output{}, err
	}
	stderr, _ := os.ReadFile(stderrPath)
	if len(stdout) > 32<<20 || len(stderr) > 32<<20 {
		return toolkit.Output{}, fmt.Errorf("command output exceeds 32 MiB")
	}
	var value any = string(stdout)
	if json.Valid(stdout) {
		_ = json.Unmarshal(stdout, &value)
	}
	data := map[string]any{"output": string(stdout), "stdout": string(stdout), "stderr": string(stderr), "value": value, "execution": result}
	if store := toolkit.ArtifactsFrom(ctx); store != nil {
		artifact, err := store.Put(ctx, stdout, "text/plain", true)
		if err != nil {
			return toolkit.Output{}, err
		}
		data["stdout_artifact"] = artifact
	}
	return toolkit.Output{Text: toolkit.Capture(ctx, string(stdout)+string(stderr)), Data: data, IsError: toolkit.Failed(result)}, nil
}
