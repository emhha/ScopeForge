package server

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"trpc.group/trpc-go/trpc-agent-go/session"
)

func (m *Manager) forgetSessions(match func(string) bool) {
	if m.sessions == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	m.forgetSessionsMatching(ctx, match)
}

func (m *Manager) forgetSessionsMatching(ctx context.Context, match func(string) bool) {
	if m.sessions == nil {
		return
	}
	items, err := m.sessions.ListSessions(ctx, session.UserKey{AppName: "scopeforge", UserID: "operator"}, session.WithListSessionOnlyMeta())
	if err != nil {
		log.Printf("[session] list cleanup: %v", err)
		return
	}
	for _, item := range items {
		if match(item.ID) {
			if err := m.sessions.DeleteSession(ctx, session.Key{AppName: "scopeforge", UserID: "operator", SessionID: item.ID}); err != nil {
				log.Printf("[session] delete cleanup: %v", err)
			}
		}
	}
}
func (m *Manager) forgetConversationSession(id int64) {
	key := fmt.Sprintf("conv-%d", id)
	m.forgetSessions(func(s string) bool { return s == key })
	if m.pg != nil {
		m.pg.ForgetSideConversation(id)
	}
}
func (m *Manager) forgetTaskSessions(taskID, explorationID int64) {
	prefix := fmt.Sprintf("exp%d-", explorationID)
	m.forgetSessions(func(s string) bool { return strings.HasPrefix(s, prefix) })
	if m.pg != nil {
		m.pg.ForgetSideTask(taskID)
	}
}
