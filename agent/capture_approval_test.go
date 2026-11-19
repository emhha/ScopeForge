package agent

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/emhha/scopeforge/db"
	"github.com/emhha/scopeforge/guard"
	"github.com/emhha/scopeforge/intercept"
	"github.com/emhha/scopeforge/internal/modeltest"
	"github.com/emhha/scopeforge/toolkit"
	"trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/tool"
)

func TestCaptureApprovalLifecycle(t *testing.T) {
	for _, action := range []string{"allow", "deny", "ask"} {
		t.Run(action, func(t *testing.T) {
			store, err := db.Open(filepath.Join(t.TempDir(), "test.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			ic := intercept.New(store)
			_ = ic.SetEnabledTools([]string{"ApprovalAuditProbe"})
			_ = ic.SetJudgeConfig(intercept.JudgeConfig{Enabled: true, AskTimeoutSeconds: 1, AskTimeoutAction: "deny"})
			ic.SetReviewer(func(context.Context, int64, string, intercept.ReviewInput) (intercept.Decision, error) {
				return intercept.Decision{Action: action, Message: "policy"}, nil
			})
			ctx := intercept.WithTaskContext(t.Context(), "test", "worker", func(a db.Activity) {
				if action == "ask" && a.Kind == "intercept_request" {
					var in struct {
						ID int64 `json:"pending_id"`
					}
					_ = json.Unmarshal([]byte(a.Detail), &in)
					_ = ic.Decide(in.ID, true)
				}
			})
			calls, executed := 0, 0
			mdl := modeltest.Func(func(context.Context, *model.Request) (<-chan *model.Response, error) {
				calls++
				if calls == 1 {
					return modeltest.Responses(&model.Response{Choices: []model.Choice{{Message: model.Message{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{{ID: "probe", Type: "function", Function: model.FunctionDefinitionParam{Name: "ApprovalAuditProbe", Arguments: []byte(`{}`)}}}}}}}), nil
				}
				return modeltest.Responses(modeltest.Final("done")), nil
			})
			probe := toolkit.New("ApprovalAuditProbe", "probe", map[string]any{"type": "object"}, func(context.Context, json.RawMessage) (toolkit.Output, error) {
				executed++
				return toolkit.Text("executed"), nil
			})
			var records []db.Activity
			_, _, err = captureRun(ctx, RunConfig{Model: mdl, Tools: []tool.CallableTool{probe}, Hooks: guard.NewWithInterceptor(ic).Hooks(), DisableHostTools: true, MaxTurns: 3}, "run", func(a db.Activity) { records = append(records, a) })
			if err != nil {
				t.Fatal(err)
			}
			expected := 1
			if action == "deny" {
				expected = 0
			}
			if executed != expected {
				t.Fatalf("executed=%d expected=%d", executed, expected)
			}
			used, result := false, false
			for _, a := range records {
				used = used || a.Kind == "tool_use"
				result = result || a.Kind == "tool_result"
			}
			if !used || !result {
				t.Fatalf("unpaired trace: %+v", records)
			}
		})
	}
}
