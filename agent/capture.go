package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/emhha/scopeforge/db"
	"github.com/emhha/scopeforge/intercept"
	"github.com/emhha/scopeforge/llmrec"
	"github.com/emhha/scopeforge/modelutil"
	"github.com/emhha/scopeforge/runstate"
	"github.com/emhha/scopeforge/sessionctx"
	"github.com/emhha/scopeforge/toolkit"
	"trpc.group/trpc-go/trpc-agent-go/model"
)

func captureRun(ctx context.Context, cfg RunConfig, input string, emit func(db.Activity)) (string, runstate.Reason, error) {
	t := newTurn(cfg)
	defer t.Close()
	return captureRunSession(ctx, t, input, emit)
}
func captureRunSession(ctx context.Context, t *turn, input string, emit func(db.Activity)) (text string, reason runstate.Reason, runErr error) {
	ctx = sessionctx.WithID(ctx, t.key.SessionID)
	sources := &nodeSources{calls: map[string][]db.NodeSource{}}
	ctx = context.WithValue(ctx, nodeSourcesKey{}, sources)
	ctx, audit := intercept.WithTrace(ctx, input, approvalHistory(t.History(ctx)))
	defer audit.Finish()
	trace := &runTrace{startedAt: time.Now()}
	names := map[string]string{}
	var total, current model.Usage
	var lastUsageAt time.Time
	var finalMessage *messageProgress
	rec := func(a db.Activity) {
		if emit != nil {
			emit(a)
		}
	}
	var modelCallsMu sync.Mutex
	var modelCalls []db.Activity
	ctx = llmrec.WithCallObserver(ctx, func(call llmrec.CallRecord) {
		metadata, _ := json.Marshal(map[string]any{"llm_call": call})
		detail, _ := json.Marshal(map[string]any{"system_prompt": call.SystemPrompt})
		a := db.Activity{Kind: "model_call", IsError: call.Status == "error", Summary: call.ProfileName + " · " + call.Model, Metadata: metadata, Detail: string(detail)}
		modelCallsMu.Lock()
		modelCalls = append(modelCalls, a)
		modelCallsMu.Unlock()
	})
	flushModelCalls := func() {
		modelCallsMu.Lock()
		pending := modelCalls
		modelCalls = nil
		modelCallsMu.Unlock()
		for _, a := range pending {
			rec(a)
		}
	}
	run := func(callCtx context.Context, prompt string, settling bool) error {
		events, err := t.Run(callCtx, prompt, settling)
		if err != nil {
			return err
		}
		var visible, thinking strings.Builder
		var textProgress, thinkingProgress messageProgress

		for ev := range events {
			flushModelCalls()
			if ev == nil || ev.Response == nil {
				continue
			}
			if ev.Usage != nil {
				modelutil.MergeUsage(&current, *ev.Usage)
				live := total
				modelutil.AddUsage(&live, current)
				// Providers often attach cumulative usage to every streamed token.
				// Keep a live sample without persisting thousands of invisible rows.
				if !ev.IsPartial || time.Since(lastUsageAt) >= time.Second {
					rec(activityWithUsage(db.Activity{Kind: "usage"}, &live))
					lastUsageAt = time.Now()
				}
				if !ev.IsPartial {
					modelutil.AddUsage(&total, current)
					current = model.Usage{}
				}
			}
			if ev.Error != nil {
				err = ev.Error
				continue
			}
			for _, choice := range ev.Choices {
				if ev.IsPartial {
					visible.WriteString(choice.Delta.Content)
					thinking.WriteString(choice.Delta.ReasoningContent)
					thinkingProgress.emit("thinking", thinking.String(), false, rec)
					textProgress.emit("text", visible.String(), false, rec)
					continue
				}
				m := choice.Message
				if m.Role == model.RoleTool {
					failed := toolkit.FailedJSON(m.Content)
					trace.done(m.ToolID)
					audit.Complete(m.ToolID, m.Content, failed)
					rec(db.Activity{Kind: "tool_result", NodeSources: sources.take(m.ToolID), Tool: names[m.ToolID], ToolUseID: m.ToolID, IsError: failed, Summary: firstLine(m.Content, 200), Detail: m.Content})
					continue
				}
				if m.Role != model.RoleAssistant {
					continue
				}
				if m.ReasoningContent != "" {
					thinking.Reset()
					thinking.WriteString(m.ReasoningContent)
				}
				if thinking.Len() > 0 {
					thinkingProgress.emit("thinking", thinking.String(), true, rec)
					thinkingProgress = messageProgress{}
					thinking.Reset()
				}
				if m.Content != "" {
					visible.Reset()
					visible.WriteString(m.Content)
				}
				if len(m.ToolCalls) == 0 {
					text = visible.String()
					finalMessage = &textProgress
				} else if visible.Len() > 0 {
					textProgress.emit("text", visible.String(), true, rec)
					textProgress = messageProgress{}
				}
				visible.Reset()
				// Provenance is consumed with its tool_result, never cleared here: the
				// SDK may already have executed this call before delivering its event.
				for _, call := range m.ToolCalls {
					names[call.ID] = call.Function.Name
					trace.start(call.ID, call.Function.Name, string(call.Function.Arguments))
					audit.Start(call.ID, call.Function.Name, call.Function.Arguments)
					rec(db.Activity{Kind: "tool_use", Tool: call.Function.Name, ToolUseID: call.ID, Summary: call.Function.Name + " " + firstLine(string(call.Function.Arguments), 200), Detail: string(call.Function.Arguments)})
				}
			}
		}
		t.models.Wait()
		flushModelCalls()
		if visible.Len() > 0 {
			text = visible.String()
		}
		if thinking.Len() > 0 {
			thinkingProgress.emit("thinking", thinking.String(), true, rec)
		}
		if callCtx.Err() != nil || err != nil {
			textProgress.emit("text", visible.String(), true, rec)
			if callCtx.Err() != nil {
				return callCtx.Err()
			}
		}
		return err
	}
	callCtx, cancel := context.WithCancel(ctx)
	if t.config.MaxDuration > 0 {
		cancel()
		callCtx, cancel = context.WithTimeout(ctx, t.config.MaxDuration)
	}
	runErr = run(callCtx, input, false)
	cancel()
	reason = runstate.ReasonCompleted
	if runErr != nil {
		reason = runstate.ReasonModelError
		if strings.Contains(strings.ToLower(runErr.Error()), "max llm") {
			reason = runstate.ReasonMaxTurns
		}
		if errors.Is(runErr, context.DeadlineExceeded) {
			reason = runstate.ReasonTimeout
		}
		if ctx.Err() != nil {
			reason = runstate.ReasonAbortedStreaming
			if trace.pending {
				reason = runstate.ReasonAbortedTools
			}
		}
	}
	if (reason == runstate.ReasonMaxTurns || reason == runstate.ReasonTimeout) && ctx.Err() == nil && t.config.Settlement != nil {
		policy := t.config.Settlement
		prompt := policy.Prompt
		if value := policy.PromptByReason[reason]; value != "" {
			prompt = value
		}
		settleCtx, stop := context.WithTimeout(ctx, 30*time.Second)
		settleErr := run(settleCtx, prompt, true)
		stop()
		if settleErr == nil {
			runErr = nil
		} else {
			runErr = settleErr
		}
	}
	t.models.Wait()
	t.usageMu.Lock()
	total = t.usage
	t.usageMu.Unlock()
	summary, detail := text, text
	if strings.TrimSpace(summary) == "" || runErr != nil {
		summary, detail = terminalText(ctx, &runstate.Outcome{Reason: reason, Text: text, Err: runErr, Usage: total}, trace)
	}
	var messageMetadata json.RawMessage
	if runErr == nil && finalMessage != nil {
		messageMetadata = finalMessage.metadata(true)
	}
	rec(activityWithUsage(db.Activity{Kind: "result", IsError: runErr != nil, Summary: firstLine(summary, 400), Detail: detail, Metadata: messageMetadata}, &total))
	return
}
func activityWithUsage(a db.Activity, u *model.Usage) db.Activity {
	if u == nil {
		return a
	}
	i, o, r, w := modelutil.Counts(*u)
	a.InputTokens = &i
	a.OutputTokens = &o
	a.CacheReadTokens = &r
	a.CacheWriteTokens = &w
	return a
}
func firstLine(s string, max int) string {
	s = strings.TrimSpace(s)
	if before, _, found := strings.Cut(s, "\n"); found {
		s = before
	}
	if utf8.RuneCountInString(s) > max {
		s = string([]rune(s)[:max]) + "…"
	}
	return s
}
func approvalHistory(messages []model.Message) []db.InterceptContextEntry {
	var out []db.InterceptContextEntry
	for _, m := range messages {
		if m.Content != "" {
			kind := string(m.Role)
			if m.Role == model.RoleTool {
				kind = "tool_result"
			}
			out = append(out, db.InterceptContextEntry{Kind: kind, Tool: m.ToolName, ToolUseID: m.ToolID, Text: m.Content})
		}
		for _, c := range m.ToolCalls {
			out = append(out, db.InterceptContextEntry{Kind: "tool_use", Tool: c.Function.Name, ToolUseID: c.ID, Text: string(c.Function.Arguments)})
		}
	}
	return out
}
