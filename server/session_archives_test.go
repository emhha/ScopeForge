package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/emhha/scopeforge/agent"
	"trpc.group/trpc-go/trpc-agent-go/event"
	"trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/session"
)

func TestNativeSessionArchiveScopeAndSummaryRoundTrip(t *testing.T) {
	dir := t.TempDir()
	service, err := agent.OpenSessionService(filepath.Join(dir, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	m := &Manager{dir: dir, sessions: service}
	keys := []session.Key{{AppName: "scopeforge", UserID: "operator", SessionID: "exp3-worker-i1"}, {AppName: "scopeforge", UserID: "operator", SessionID: "exp30-worker-i1"}, {AppName: "scopeforge", UserID: "operator", SessionID: "conv-3"}}
	for i, key := range keys {
		sess, e := service.CreateSession(t.Context(), key, session.StateMap{"fixture": []byte(`"state"`)})
		if e != nil {
			t.Fatal(e)
		}
		if e = service.AppendEvent(t.Context(), sess, event.NewResponseEvent("turn", "fixture", &model.Response{Choices: []model.Choice{{Message: model.Message{Role: model.RoleUser, Content: []string{"owned task", "unrelated task secret", "unrelated conversation secret"}[i]}}}})); e != nil {
			t.Fatal(e)
		}
	}
	// Populate a native summary representation, then verify the archive copies its bytes unchanged.
	conn, e := sql.Open("sqlite3", filepath.Join(dir, "sessions.sqlite"))
	if e != nil {
		t.Fatal(e)
	}
	summary, e := json.Marshal(&session.Summary{Summary: "native summary fixture", UpdatedAt: time.Now().UTC()})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = conn.Exec(`INSERT INTO session_summaries(app_name,user_id,session_id,filter_key,summary,updated_at) VALUES('scopeforge','operator','exp3-worker-i1','',?,?)`, summary, time.Now().UTC().UnixNano()); e != nil {
		t.Fatal(e)
	}
	conn.Close()
	path := filepath.Join(t.TempDir(), nativeSessionArchiveFile)
	count, err := m.exportNativeSessions(t.Context(), path, 3)
	if err != nil || count != 1 {
		t.Fatalf("count=%d err=%v", count, err)
	}
	// A recovered export replaces the previous snapshot without duplicating rows.
	if retried, e := m.exportNativeSessions(t.Context(), path, 3); e != nil || retried != count {
		t.Fatalf("retry count=%d err=%v", retried, e)
	}
	bytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(bytes), "unrelated task secret") || strings.Contains(string(bytes), "unrelated conversation secret") {
		t.Fatal("another scope leaked into archive pages")
	}
	m.forgetTaskSessions(3, 3)
	if sess, e := service.GetSession(t.Context(), keys[0]); e != nil || sess != nil {
		t.Fatal("archived context remained live")
	}
	undo, err := m.restoreNativeSessions(t.Context(), path, 3, count)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := service.GetSession(t.Context(), keys[0])
	if err != nil || restored == nil || len(restored.GetEvents()) != 1 || len(restored.Summaries) != 1 {
		t.Fatalf("restored=%+v err=%v", restored, err)
	}
	if _, err = m.restoreNativeSessions(t.Context(), path, 30, count); err == nil {
		t.Fatal("cross-task archive accepted")
	}
	undo()
	if sess, e := service.GetSession(context.Background(), keys[0]); e != nil || sess != nil {
		t.Fatal("restore rollback left native context")
	}
	for _, key := range keys[1:] {
		if sess, e := service.GetSession(t.Context(), key); e != nil || sess == nil {
			t.Fatal("another task or conversation was deleted")
		}
	}
}

func TestTaskArchiveJobsKeepNativeContextAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	m, err := NewManager(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { m.Close() }()
	task, err := m.CreateTask("persistent archive fixture", "fixture", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := strconv.ParseInt(task.ID, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := m.pg.GetTask(taskID)
	if err != nil {
		t.Fatal(err)
	}
	key := session.Key{AppName: "scopeforge", UserID: "operator", SessionID: fmt.Sprintf("exp%d-worker-i1", saved.ExplorationID)}
	sess, err := m.sessions.CreateSession(t.Context(), key, session.StateMap{})
	if err != nil {
		t.Fatal(err)
	}
	if err = m.sessions.AppendEvent(t.Context(), sess, event.NewResponseEvent("roundtrip", "fixture", &model.Response{Choices: []model.Choice{{Message: model.Message{Role: model.RoleUser, Content: "context survives cold storage"}}}})); err != nil {
		t.Fatal(err)
	}
	if err = m.pg.SetPaused(taskID, true); err != nil {
		t.Fatal(err)
	}
	// Stop the automatic worker, then drive the exact same job entry point.
	createServer := func(m *Manager) *Server {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		s := New(ctx, m, dir, dir, dir)
		s.archiveWG.Wait()
		s.ctx = t.Context()
		return s
	}
	s := createServer(m)
	job, err := m.pg.QueueTaskArchive(taskID)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.runOneTaskArchiveJob(); err != nil {
		t.Fatal(err)
	}
	if got, err := m.sessions.GetSession(t.Context(), key); err != nil || got != nil {
		t.Fatal("hot session not removed", err)
	}
	if err = m.Close(); err != nil {
		t.Fatal(err)
	}
	m, err = NewManager(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	s = createServer(m)
	if _, err = m.pg.QueueTaskArchiveRestore(job.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.runOneTaskArchiveJob(); err != nil {
		t.Fatal(err)
	}
	restored, err := m.sessions.GetSession(t.Context(), key)
	if err != nil || restored == nil || len(restored.GetEvents()) != 1 {
		t.Fatalf("restored=%+v err=%v", restored, err)
	}
	if restored.GetEvents()[0].Choices[0].Message.Content != "context survives cold storage" {
		t.Fatal("native event changed")
	}
}
