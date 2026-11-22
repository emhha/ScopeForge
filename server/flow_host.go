package server

import (
	"context"
	"fmt"

	"github.com/emhha/scopeforge/agent"
	"github.com/emhha/scopeforge/toolkit"
	"github.com/google/uuid"
	actool "trpc.group/trpc-go/trpc-agent-go/tool"
)

// CLI adapters derive their host capability from Bash. Check the underlying
// command with the same callbacks, so a Bash-only interception rule still works.
func (s *Server) executeWorkflowHost(ctx context.Context, raw []byte) (any, error) {
	owner, _ := ctx.Value(workflowContextKey{}).(workflowContext)
	row, err := s.m.pg.GetTool("Bash")
	if err != nil {
		return nil, err
	}
	if row != nil && (!row.Enabled || (owner.Key != "" && !contains(row.Agents, owner.Key))) {
		return nil, fmt.Errorf("Bash capability disabled or unbound")
	}
	execute := agent.ExecuteCommand
	declaration := s.bashWorkflowTool().Declaration()
	if surface, ok := ctx.Value(workflowSurfaceKey{}).(workflowSurface); ok {
		if bash := surface.Tools["Bash"]; bash != nil {
			execute = bash.Call
			declaration = bash.Declaration()
		}
	}
	hooks := toolkit.CallHooksFrom(ctx)
	if hooks == nil {
		return execute(ctx, raw)
	}
	id := uuid.NewString()
	before, err := hooks.RunBeforeTool(ctx, &actool.BeforeToolArgs{ToolCallID: id, ToolName: "Bash", Declaration: declaration, Arguments: raw})
	if err != nil {
		return nil, err
	}
	var result any
	if before != nil {
		if before.Context != nil {
			ctx = before.Context
		}
		if before.ModifiedArguments != nil {
			raw = before.ModifiedArguments
		}
		result = before.CustomResult
	}
	short := result != nil
	if !short {
		result, err = execute(ctx, raw)
	}
	after, afterErr := hooks.RunAfterTool(ctx, &actool.AfterToolArgs{ToolCallID: id, ToolName: "Bash", Declaration: declaration, Arguments: raw, Result: result, Error: err})
	if afterErr != nil {
		return nil, afterErr
	}
	if after != nil && after.CustomResult != nil {
		result = after.CustomResult
	}
	if short && toolkit.Failed(result) {
		return nil, fmt.Errorf("Bash: %s", toolkit.TextOf(result))
	}
	return result, err
}
