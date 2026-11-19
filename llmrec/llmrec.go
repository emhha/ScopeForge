package llmrec

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/emhha/scopeforge/db"
	"github.com/emhha/scopeforge/modelutil"
	"github.com/emhha/scopeforge/sessionctx"
	llm "trpc.group/trpc-go/trpc-agent-go/model"
)

type taskIDContextKey struct{}
type workerContextKey struct{}

// WithTaskID attaches the owning task registry id to an LLM call. Session ids
// are based on exploration ids, which are not interchangeable with task ids.
func WithTaskID(ctx context.Context, taskID string) context.Context {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return ctx
	}
	return context.WithValue(ctx, taskIDContextKey{}, taskID)
}

// TaskIDFrom returns the explicit task registry id attached by the task runtime.
func TaskIDFrom(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	taskID, _ := ctx.Value(taskIDContextKey{}).(string)
	return strings.TrimSpace(taskID)
}

// WithWorker overrides the agent-lane ("worker") label for an LLM call. The lane
// is normally parsed from the session id (exp<N>-<role>); calls made
// outside the engine's worker/planner sessions — e.g. the intercept fallback judge
// — carry no such session, so they attach their lane explicitly here. This lets the
// usage ledger single out that spend (worker='judge') for the config page.
func WithWorker(ctx context.Context, worker string) context.Context {
	worker = strings.TrimSpace(worker)
	if worker == "" {
		return ctx
	}
	return context.WithValue(ctx, workerContextKey{}, worker)
}

// workerFrom returns the explicit lane override, or "" when none is set.
func workerFrom(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	worker, _ := ctx.Value(workerContextKey{}).(string)
	return strings.TrimSpace(worker)
}

type Recorder struct {
	inner                                      llm.Model
	pg                                         *db.DB
	model, prof, thinkingType, reasoningEffort string
	enabled                                    func() bool
}

func Wrap(inner llm.Model, pg *db.DB, model, prof, thinking, effort string, enabled func() bool) *Recorder {
	return &Recorder{inner, pg, model, prof, thinking, effort, enabled}
}
func (r *Recorder) Info() llm.Info { return r.inner.Info() }
func parseSession(s string) (taskID, worker string) {
	// format: exp<N>-<role>[-suffix]
	if !strings.HasPrefix(s, "exp") {
		return "", ""
	}
	rest := s[3:] // after "exp"
	// split task number
	i := strings.IndexByte(rest, '-')
	if i < 0 {
		return rest, ""
	}
	taskID = rest[:i]
	rest = rest[i+1:]
	// worker role is up to the next '-' (e.g. "worker" from "worker-i3")
	if j := strings.IndexByte(rest, '-'); j >= 0 {
		worker = rest[:j]
	} else {
		worker = rest
	}
	return taskID, worker
}

func (r *Recorder) ProjectProviderData(data *llm.ProviderData) (llm.Message, error) {
	return modelutil.ProjectProviderData(r.inner, data)
}
func (r *Recorder) GenerateContent(ctx context.Context, req *llm.Request) (<-chan *llm.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("nil request")
	}
	enabled := r.enabled == nil || r.enabled()
	start := time.Now()
	sid := sessionctx.ID(ctx)
	parsed, worker := parseSession(sid)
	if w := workerFrom(ctx); w != "" {
		worker = w
	}
	task := TaskIDFrom(ctx)
	if task == "" {
		task = parsed
	}
	var capture *Capture
	if enabled {
		ctx, capture = NewCapture(ctx)
	}
	source, startErr := r.inner.GenerateContent(ctx, req)
	out := make(chan *llm.Response)
	go func() {
		defer close(out)
		var usage llm.Usage
		var text, thinking strings.Builder
		var callErr = startErr
		defer func() {
			if r.pg == nil {
				return
			}
			if ctx.Err() != nil {
				callErr = ctx.Err()
			}
			status := "ok"
			errText := ""
			if callErr != nil {
				status = "error"
				errText = callErr.Error()
			}
			input, output, read, write := modelutil.Counts(usage)
			latency := int(time.Since(start).Milliseconds())
			if err := r.pg.InsertLLMUsage(&db.LLMUsage{TaskID: task, ExplorationID: db.ParseExpID(parsed), Worker: worker, Model: r.model, ProfileName: r.prof, LatencyMs: latency, InputTokens: input, OutputTokens: output, CacheRead: read, CacheWrite: write, Status: status}); err != nil {
				log.Printf("[llmusage] %v", err)
			}
			if !enabled {
				return
			}
			body := r.serializeRequest(*req)
			response, _ := json.Marshal(map[string]any{"text": text.String(), "thinking": thinking.String(), "usage": usage})
			if err := r.pg.InsertLLMRecord(&db.LLMRecord{SessionID: sid, TaskID: task, Worker: worker, Model: r.model, ProfileName: r.prof, LatencyMs: latency, InputTokens: input, OutputTokens: output, CacheRead: read, CacheWrite: write, Status: status, Error: errText, RequestBody: body, ResponseBody: string(response), RawRequest: capture.RawRequest(), RawResponse: capture.RawResponse()}); err != nil {
				log.Printf("[llmrec] %v", err)
			}
		}()
		if startErr != nil {
			select {
			case out <- &llm.Response{Error: &llm.ResponseError{Type: llm.ErrorTypeAPIError, Message: startErr.Error()}, Done: true}:
			case <-ctx.Done():
			}
			return
		}
		if source == nil {
			callErr = fmt.Errorf("model returned nil channel")
			return
		}
		for ev := range source {
			if ev == nil {
				continue
			}
			if ev.Usage != nil {
				modelutil.MergeUsage(&usage, *ev.Usage)
			}
			if ev.Error != nil {
				callErr = ev.Error
			}
			if len(ev.Choices) > 0 {
				choice := ev.Choices[0]
				if ev.IsPartial {
					text.WriteString(choice.Delta.Content)
					thinking.WriteString(choice.Delta.ReasoningContent)
				} else {
					if choice.Message.Content != "" {
						text.Reset()
						text.WriteString(choice.Message.Content)
					}
					if choice.Message.ReasoningContent != "" {
						thinking.Reset()
						thinking.WriteString(choice.Message.ReasoningContent)
					}
				}
			}
			select {
			case out <- ev:
			case <-ctx.Done():
			}
		}
	}()
	return out, nil
}
func (r *Recorder) serializeRequest(req llm.Request) string {
	tools := map[string]any{}
	for name, t := range req.Tools {
		tools[name] = t.Declaration()
	}
	body, _ := json.Marshal(map[string]any{"messages": req.Messages, "tools": tools, "generation": req.GenerationConfig, "thinking": map[string]string{"type": r.thinkingType, "effort": r.reasoningEffort}})
	return string(body)
}
