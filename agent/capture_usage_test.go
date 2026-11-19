package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/emhha/scopeforge/db"
	"github.com/emhha/scopeforge/internal/modeltest"
	"trpc.group/trpc-go/trpc-agent-go/model"
)

func TestCaptureRunPersistsUsageOnProviderFailure(t *testing.T) {
	mdl := modeltest.Func(func(context.Context, *model.Request) (<-chan *model.Response, error) {
		ev := modeltest.Text("partial")
		ev.Usage = &model.Usage{PromptTokens: 11, CompletionTokens: 7, PromptTokensDetails: model.PromptTokensDetails{CachedTokens: 3, CacheCreationTokens: 2}}
		return modeltest.Responses(ev, modeltest.Failure(errors.New("provider failed"))), nil
	})
	var records []db.Activity
	_, _, err := captureRun(t.Context(), RunConfig{Model: mdl, MaxTurns: 1, DisableHostTools: true}, "test", func(a db.Activity) { records = append(records, a) })
	if err == nil || !strings.Contains(err.Error(), "provider failed") {
		t.Fatalf("error=%v", err)
	}
	assertCapturedResultUsage(t, records, 11, 7, 3, 2)
}
func TestCaptureRunPersistsUsageOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	started := make(chan struct{})
	mdl := modeltest.Func(func(ctx context.Context, _ *model.Request) (<-chan *model.Response, error) {
		out := make(chan *model.Response)
		go func() {
			defer close(out)
			ev := modeltest.Text("partial")
			ev.Usage = &model.Usage{PromptTokens: 13, CompletionTokens: 9, PromptTokensDetails: model.PromptTokensDetails{CachedTokens: 5, CacheCreationTokens: 4}}
			out <- ev
			close(started)
			<-ctx.Done()
		}()
		return out, nil
	})
	var records []db.Activity
	done := make(chan error, 1)
	go func() {
		_, _, err := captureRun(ctx, RunConfig{Model: mdl, DisableHostTools: true}, "test", func(a db.Activity) { records = append(records, a) })
		done <- err
	}()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
	assertCapturedResultUsage(t, records, 13, 9, 5, 4)
}
func assertCapturedResultUsage(t *testing.T, records []db.Activity, i, o, r, w int) {
	t.Helper()
	var last *db.Activity
	for index := range records {
		if records[index].Kind == "result" {
			last = &records[index]
		}
	}
	if last == nil || last.InputTokens == nil || *last.InputTokens != i || *last.OutputTokens != o || *last.CacheReadTokens != r || *last.CacheWriteTokens != w {
		t.Fatalf("result=%+v expected %d/%d/%d/%d", last, i, o, r, w)
	}
}
