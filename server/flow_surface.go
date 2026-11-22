package server

import (
	"context"

	nativeagent "trpc.group/trpc-go/trpc-agent-go/agent"
	actool "trpc.group/trpc-go/trpc-agent-go/tool"
)

type workflowSurfaceKey struct{}
type workflowSurface struct {
	Tools  map[string]actool.CallableTool
	Filter actool.FilterFunc
}

// Freeze the actual calling Agent's native tool surface, including activated
// ToolSets and run-scoped filters. Keep this origin through nested graph Agents.
func withWorkflowSurface(ctx context.Context) context.Context {
	if ctx.Value(workflowSurfaceKey{}) != nil {
		return ctx
	}
	inv, ok := nativeagent.InvocationFromContext(ctx)
	if !ok || inv == nil || inv.Agent == nil {
		return ctx
	}
	provider, ok := inv.Agent.(nativeagent.InvocationToolSurfaceProvider)
	if !ok {
		return ctx
	}
	items, _ := provider.InvocationToolSurface(ctx, inv)
	items = append(items, inv.RunOptions.AdditionalTools...)
	surface := workflowSurface{Tools: map[string]actool.CallableTool{}, Filter: inv.RunOptions.ToolFilter}
	for _, item := range items {
		if inv.RunOptions.ToolFilter != nil && !inv.RunOptions.ToolFilter(ctx, item) {
			continue
		}
		if callable, ok := item.(actool.CallableTool); ok {
			surface.Tools[item.Declaration().Name] = callable
		}
	}
	return context.WithValue(ctx, workflowSurfaceKey{}, surface)
}

func workflowMetaTool(name string) bool {
	switch name {
	case "workflow_catalog", "workflow_validate", "workflow_run", "workflow_get_run", "workflow_cancel_run", "transfer_to_agent":
		return true
	}
	return false
}
