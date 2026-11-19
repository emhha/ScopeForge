package agent

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/emhha/scopeforge/db"
	"github.com/emhha/scopeforge/guard"
	"github.com/emhha/scopeforge/intercept"
	"trpc.group/trpc-go/trpc-agent-go/tool"
)

func TestNativeReviewReceivesOnlyCurrentArguments(t *testing.T) {
	store, err := db.Open(filepath.Join(t.TempDir(), "test.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ic := intercept.New(store)
	_ = ic.SetEnabledTools([]string{"probe"})
	_ = ic.SetJudgeConfig(intercept.JudgeConfig{Enabled: true})
	var got intercept.ReviewInput
	ic.SetReviewer(func(_ context.Context, _ int64, _ string, input intercept.ReviewInput) (intercept.Decision, error) {
		got = input
		return intercept.Decision{Action: "deny", Message: "denied"}, nil
	})
	ctx, _ := intercept.WithTrace(t.Context(), "secret prior task", []db.InterceptContextEntry{{Kind: "text", Text: "secret history"}})
	args := json.RawMessage(`{"step":2}`)
	result, err := guard.NewWithInterceptor(ic).Hooks().RunBeforeTool(ctx, &tool.BeforeToolArgs{ToolName: "probe", Arguments: args})
	if err != nil || result == nil || result.CustomResult == nil {
		t.Fatalf("guard did not deny: %v", err)
	}
	if got.Tool != "probe" || string(got.Arguments) != string(args) {
		t.Fatalf("review=%+v", got)
	}
}
