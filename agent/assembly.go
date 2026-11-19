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
	HTTPGuidance     string
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
	def.HTTPGuidance = targetHTTPGuidance(out)
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

// Describe only the final permitted tools, including disabled/binding overrides.
func hasTool(tools []actool.CallableTool, name string) bool {
	for _, item := range tools {
		if item.Declaration().Name == name {
			return true
		}
	}
	return false
}

func targetHTTPGuidance(tools []actool.CallableTool) string {
	if !hasTool(tools, "http_request") {
		return ""
	}
	text := "\n\n**目标 HTTP 请求**：向目标发送 HTTP/HTTPS 请求时优先使用 http_request，由 netloom 处理请求、TLS、重定向及流量采集。不要通过 Bash 的 curl/wget、WebFetch 或临时 HTTP 脚本绕过该入口。浏览器交互和需要专用协议的工具仍使用相应工具。根据 status_code、headers、body、body_encoding 判断下一步；HTTP 4xx/5xx 也是有效响应。body_preview_truncated 只表示展示截断，complete=false 表示本地采集不完整，不能据此推断服务端响应不完整。流量捕获启用时每跳返回 exchange_ids；捕获关闭时仍可请求，但不产生流量记录。"
	if hasTool(tools, "traffic_get") {
		text += "预览不足时用 traffic_get(id=exchange_id) 读取已采集内容；已有内容足够时无需再查，也不要仅为补证据重复请求。"
	}
	return text
}
