// Package guard applies platform approvals through native tRPC tool callbacks.
package guard

import (
	"context"
	"encoding/json"
	"regexp"
	"sync"
	"time"

	"github.com/emhha/scopeforge/intercept"
	"github.com/emhha/scopeforge/toolkit"
	"trpc.group/trpc-go/trpc-agent-go/tool"
)

type AuditEntry struct {
	TS      int64  `json:"ts"`
	Tool    string `json:"tool"`
	Action  string `json:"action"`
	Reason  string `json:"reason,omitempty"`
	Command string `json:"command,omitempty"`
}
type Guard struct {
	mu          sync.Mutex
	audit       []AuditEntry
	attrib      map[string]int
	interceptor *intercept.Interceptor
}

func New() *Guard { return NewWithInterceptor(nil) }
func NewWithInterceptor(ic *intercept.Interceptor) *Guard {
	return &Guard{attrib: map[string]int{}, interceptor: ic}
}
func (g *Guard) Hooks() *tool.Callbacks {
	return tool.NewCallbacks().RegisterBeforeTool(g.before).RegisterAfterTool(g.after)
}
func (g *Guard) before(ctx context.Context, args *tool.BeforeToolArgs) (*tool.BeforeToolResult, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	var in struct {
		Command string `json:"command"`
		Text    string `json:"text"`
	}
	_ = json.Unmarshal(args.Arguments, &in)
	command := in.Command
	if command == "" {
		command = in.Text
	}
	g.record(args.ToolName, "allow", "", command)
	if g.interceptor == nil || !g.interceptor.IsToolEnabled(args.ToolName) {
		return nil, nil
	}
	ctx = intercept.WithCall(ctx, args.ToolName, args.Arguments)
	decision, matched := g.interceptor.Match(args.ToolName, args.Arguments)
	if !matched {
		var judged bool
		decision, judged = g.interceptor.Judge(ctx, args.ToolName, args.Arguments)
		if !judged {
			return nil, nil
		}
	}
	deny := func(reason string) (*tool.BeforeToolResult, error) {
		g.record(args.ToolName, "block", reason, command)
		return &tool.BeforeToolResult{CustomResult: toolkit.Error(systemBlockMessage(reason)), SkipStateDelta: true}, nil
	}
	switch decision.Action {
	case "deny":
		g.interceptor.Log(ctx, intercept.ConvIDFromContext(ctx), decision, args.ToolName, args.Arguments, "denied")
		return deny(decision.Message)
	case "allow":
		g.interceptor.Log(ctx, intercept.ConvIDFromContext(ctx), decision, args.ToolName, args.Arguments, "allowed")
		return nil, nil
	case "ask":
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !g.interceptor.HandleAsk(ctx, intercept.ConvIDFromContext(ctx), decision, args.ToolName, args.Arguments) {
			return deny("人工审批未通过（用户拒绝或审批超时）")
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return nil, nil
}
func systemBlockMessage(reason string) string {
	return "【ScopeForge 平台管控·非目标防御】此调用被平台拦截。原因：" + reason + "。此操作被禁止。"
}

var reBlocked = regexp.MustCompile(`(?i)\b(403|forbidden|waf|blocked|rate.?limit|429|captcha|denied)\b`)

func (g *Guard) after(_ context.Context, args *tool.AfterToolArgs) (*tool.AfterToolResult, error) {
	if args.ToolName != "Bash" && args.ToolName != "exec_command" {
		return nil, nil
	}
	class := "ok"
	if reBlocked.MatchString(toolkit.TextOf(args.Result)) {
		class = "blocked"
	} else if args.Error != nil || toolkit.Failed(args.Result) {
		class = "error"
	}
	g.mu.Lock()
	g.attrib[class]++
	g.mu.Unlock()
	return nil, nil
}
func (g *Guard) Attributions() map[string]int {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := map[string]int{}
	for k, v := range g.attrib {
		out[k] = v
	}
	return out
}
func (g *Guard) record(tool, action, reason, cmd string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.audit = append(g.audit, AuditEntry{time.Now().Unix(), tool, action, reason, cmd})
	if len(g.audit) > 2000 {
		g.audit = g.audit[len(g.audit)-2000:]
	}
}
func (g *Guard) Audit() []AuditEntry {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]AuditEntry(nil), g.audit...)
}
