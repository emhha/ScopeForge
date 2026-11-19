package llmrec

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/internal/modeltest"
	"github.com/Autumn-27/artex/modelutil"
	"github.com/Autumn-27/artex/sessionctx"
	"trpc.group/trpc-go/trpc-agent-go/model"
)

func TestTaskIDContextUsesExplicitRegistryID(t *testing.T) {
	ctx := WithTaskID(t.Context(), "42")
	if TaskIDFrom(ctx) != "42" {
		t.Fatal("lost task identity")
	}
}
func TestParseSessionFallbackIsExplorationScoped(t *testing.T) {
	id, worker := parseSession("exp3-worker-i7")
	if id != "3" || worker != "worker" {
		t.Fatal(id, worker)
	}
}
func TestNativeRecorderPreservesCountersAndWritesOnce(t *testing.T) {
	store, err := db.Open(filepath.Join(t.TempDir(), "test.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	mdl := modeltest.Func(func(context.Context, *model.Request) (<-chan *model.Response, error) {
		delta := modeltest.Text("answer")
		delta.Usage = &model.Usage{PromptTokens: 23, CompletionTokens: 5}
		final := modeltest.Final("answer")
		final.Usage = &model.Usage{PromptTokens: 23, CompletionTokens: 5}
		return modeltest.Responses(delta, final), nil
	})
	rec := Wrap(mdl, store, "fixture", "test", "", "", func() bool { return false })
	ctx := WithTaskID(sessionctx.WithID(t.Context(), "exp3-worker-i7"), "42")
	message, _, usage, err := modelutil.Complete(ctx, rec, model.Request{})
	if err != nil || message.Content != "answer" || usage.PromptTokens != 23 || usage.CompletionTokens != 5 {
		t.Fatalf("message=%+v usage=%+v error=%v", message, usage, err)
	}
	var n, input, output int
	if err := store.QueryRow(`SELECT count(*),sum(input_tokens),sum(output_tokens) FROM llm_usage`).Scan(&n, &input, &output); err != nil || n != 1 || input != 23 || output != 5 {
		t.Fatalf("ledger=%d %d/%d error=%v", n, input, output, err)
	}
	var heavy int
	_ = store.QueryRow(`SELECT count(*) FROM llm_records`).Scan(&heavy)
	if heavy != 0 {
		t.Fatal("disabled body recorder wrote model history")
	}
}
