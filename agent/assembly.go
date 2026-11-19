package agent

import (
	"context"
	"fmt"
	"log"
	"runtime/debug"

	"github.com/emhha/scopeforge/toolkit"
	"trpc.group/trpc-go/trpc-agent-go/agent/llmagent"

	actool "trpc.group/trpc-go/trpc-agent-go/tool"
)

// AugmentInfo carries native skill/toolset options and product audit callbacks.
type AugmentInfo struct {
	FindingGuidance  string
	AgentOptions     []llmagent.Option
	Callbacks        *actool.Callbacks
	InteractiveShell bool
}

// ToolAugment adds the role's visible MCP tools, native skill options and callbacks.
// The server resolves visibility from SQLite. Cleanup closes spawned MCP clients.
var ToolAugment func(ctx context.Context, agentKey string) (extra []actool.CallableTool, def AugmentInfo, cleanup func())

// AugmentTools returns base plus the agent's visible skill/MCP tools, the
// AugmentInfo, and a cleanup func the caller must defer (closes MCP clients).
// Built-in base tools are kept as-is — never filtered (内置工具留代码层，不做可见性过滤).
func AugmentTools(ctx context.Context, agentKey string, base []actool.CallableTool) ([]actool.CallableTool, AugmentInfo, func()) {
	var (
		def     AugmentInfo
		cleanup = func() {}
		out     = base
	)
	if ToolAugment != nil {
		var extra []actool.CallableTool
		var cl func()
		extra, def, cl = ToolAugment(ctx, agentKey)
		if cl != nil {
			cleanup = cl
		}
		if len(extra) > 0 {
			out = append(append([]actool.CallableTool{}, base...), extra...)
		}
	}
	// DB tools table has the final say on the built-in tools: drop the ones this
	// agent isn't bound to (or that are disabled) and swap in overridden
	// descriptions/schemas + default injection. MCP/skill/host tools have no row
	// and pass through untouched, while native skill activation manages tool visibility.
	if ToolResolve != nil {
		out = ToolResolve(ctx, agentKey, out)
	}
	out, def.FindingGuidance = findingWorkflowTools(agentKey, out)
	for i, t := range out {
		out[i] = guardPanic(t)
	}
	return out, def, cleanup
}

// guardPanic reports a domain tool panic as an ordinary tool result with diagnostics.
func guardPanic(t actool.CallableTool) actool.CallableTool { return &guardedTool{CallableTool: t} }

type guardedTool struct{ actool.CallableTool }

func (g *guardedTool) Call(ctx context.Context, in []byte) (res any, err error) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[tools] %s panic: %v\n%s", g.Declaration().Name, r, debug.Stack())
			res, err = toolkit.Error(fmt.Sprintf("工具 %s 内部错误：%v（本次调用已失败，可换个参数或改用别的工具）", g.Declaration().Name, r)), nil
		}
	}()
	return g.CallableTool.Call(ctx, in)
}
