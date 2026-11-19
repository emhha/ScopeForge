package agent

import (
	"context"
	"testing"

	"github.com/Autumn-27/artex/internal/modeltest"
	"github.com/Autumn-27/artex/sidequestion"
	"trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/session/inmemory"
)

func TestNativeSessionContinuesInMemoryAndDoesNotReplayAfterRestart(t *testing.T) {
	service := inmemory.NewSessionService()
	defer service.Close()
	var requests []model.Request
	mdl := modeltest.Func(func(_ context.Context, r *model.Request) (<-chan *model.Response, error) {
		copy, _ := sidequestion.CloneRequest(*r)
		requests = append(requests, copy)
		return modeltest.Responses(modeltest.Final("reply")), nil
	})
	var snapshots []sidequestion.Snapshot
	ctx := sidequestion.WithPublisher(t.Context(), func(s sidequestion.Snapshot) { snapshots = append(snapshots, s) })
	ctx = sidequestion.Attach(ctx, sidequestion.Parent{ConversationID: 1})
	mdlBound := sidequestion.Bind(mdl, sidequestion.Model{Model: "fixture"})
	cfg := RunConfig{Model: mdlBound, Sessions: service, SessionID: "conv-1", DisableHostTools: true}
	for _, text := range []string{"first question", "second question"} {
		if _, _, err := captureRun(ctx, cfg, text, nil); err != nil {
			t.Fatal(err)
		}
	}
	found := false
	for _, m := range requests[1].Messages {
		found = found || m.Content == "first question"
	}
	if !found {
		t.Fatal("native session lost prior turn")
	}
	if len(snapshots) == 0 {
		t.Fatal("checkpoint not published")
	}
	fresh := inmemory.NewSessionService()
	defer fresh.Close()
	cfg.Sessions = fresh
	if _, _, err := captureRun(t.Context(), cfg, "after restart", nil); err != nil {
		t.Fatal(err)
	}
	for _, m := range requests[2].Messages {
		if m.Content == "first question" {
			t.Fatal("history replayed after restart")
		}
	}
}
