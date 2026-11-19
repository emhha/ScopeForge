package server

import (
	"context"
	"testing"

	"github.com/emhha/scopeforge/agent"
	"trpc.group/trpc-go/trpc-agent-go/session"
)

func TestNativeSessionDeletionKeepsOtherConversations(t *testing.T) {
	service := agent.NewSessionService()
	defer service.Close()
	m := &Manager{sessions: service}
	for _, id := range []string{"conv-1", "conv-2", "exp3-worker-i9", "exp4-main"} {
		if _, err := service.CreateSession(t.Context(), session.Key{AppName: "scopeforge", UserID: "operator", SessionID: id}, nil); err != nil {
			t.Fatal(err)
		}
	}
	m.forgetConversationSession(1)
	m.forgetTaskSessions(3, 3)
	items, err := service.ListSessions(context.Background(), session.UserKey{AppName: "scopeforge", UserID: "operator"})
	if err != nil || len(items) != 2 {
		t.Fatalf("remaining=%d error=%v", len(items), err)
	}
	for _, item := range items {
		if item.ID != "conv-2" && item.ID != "exp4-main" {
			t.Fatal("wrong session retained", item.ID)
		}
	}
}
