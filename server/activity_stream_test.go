package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/emhha/scopeforge/agent"
	"github.com/emhha/scopeforge/db"
	"github.com/emhha/scopeforge/internal/modeltest"
	"trpc.group/trpc-go/trpc-agent-go/model"
)

func TestActivitySSEArrivesBeforeModelCompletes(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	s := New(ctx, m, t.TempDir(), m.dir, t.TempDir())
	task, err := m.CreateTask("live SSE", "fixture", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	token, err := signJWT(s.jwtKey)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(s.Handler())
	defer server.Close()
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/api/exploration/activity/stream?token="+token+"&task="+task.ID, nil)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	mdl := modeltest.Func(func(ctx context.Context, _ *model.Request) (<-chan *model.Response, error) {
		out := make(chan *model.Response)
		go func() {
			defer close(out)
			partial := modeltest.Text("live body")
			partial.Choices[0].Delta.ReasoningContent = "live reasoning"
			select {
			case out <- partial:
			case <-ctx.Done():
				return
			}
			select {
			case <-release:
			case <-ctx.Done():
				return
			}
			select {
			case out <- modeltest.Final("live body"):
			case <-ctx.Done():
			}
		}()
		return out, nil
	})
	sessions, err := agent.OpenSessionService(filepath.Join(t.TempDir(), "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer sessions.Close()
	chat := agent.NewChatAgent(mdl, "fixture", t.TempDir(), sessions, 0)
	done := make(chan error, 1)
	go func() {
		_, err := chat.Chat(ctx, "mainagent", "live-test", "hello", 2, 0, false, func(a db.Activity) { s.engine.emitActivity(task, a) })
		done <- err
	}()
	scanner := bufio.NewScanner(response.Body)
	seen := map[string]bool{}
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var activity ActivityDTO
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &activity); err != nil {
			t.Fatal(err)
		}
		if activity.Streaming && (activity.Kind == "thinking" || activity.Kind == "text") {
			if activity.Seq <= 0 || activity.MessageID == "" || activity.Revision != 1 || activity.Summary == "" {
				t.Fatalf("missing persisted content: %+v", activity)
			}
			seen[activity.Kind] = true
		}
		if seen["thinking"] && seen["text"] {
			break
		}
	}
	if !seen["thinking"] || !seen["text"] {
		t.Fatalf("no SSE before completion: seen=%v error=%v", seen, scanner.Err())
	}
	select {
	case err := <-done:
		t.Fatalf("model completed before release: %v", err)
	default:
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestActivitySSEReplaysCommittedRevisionsInOrder(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	s := New(ctx, m, t.TempDir(), m.dir, t.TempDir())
	task, err := m.CreateTask("replay", "fixture", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	token, err := signJWT(s.jwtKey)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(s.Handler())
	defer server.Close()
	connect := func(last string) *http.Response {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/api/exploration/activity/stream?token="+token+"&task="+task.ID+"&since=0", nil)
		req.Header.Set("Last-Event-ID", last)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { resp.Body.Close() })
		return resp
	}
	read := func(scanner *bufio.Scanner) ActivityDTO {
		for scanner.Scan() {
			if strings.HasPrefix(scanner.Text(), "data: ") {
				var a ActivityDTO
				if err := json.Unmarshal([]byte(strings.TrimPrefix(scanner.Text(), "data: ")), &a); err != nil {
					t.Fatal(err)
				}
				return a
			}
		}
		t.Fatalf("SSE closed before next revision: %v", scanner.Err())
		return ActivityDTO{}
	}
	response := connect("")
	defer response.Body.Close()
	scanner := bufio.NewScanner(response.Body)
	appendRevision := func(revision int, body string) db.Activity {
		metadata, _ := json.Marshal(map[string]any{"message": map[string]any{"id": "reply", "revision": revision, "complete": revision == 3}})
		a := db.Activity{Worker: "mainagent", Kind: "text", Summary: body, Detail: body, Metadata: metadata}
		a.ID, err = task.Store.AppendActivity(a)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	first := appendRevision(1, "first")
	second := appendRevision(2, "first, second")
	// Wake up with the higher ID first. Delivery must still come from the DB,
	// preserving both commits rather than advancing past the lower ID.
	s.engine.Broadcaster().Publish(task.ID, second)
	s.engine.Broadcaster().Publish(task.ID, first)
	a, b := read(scanner), read(scanner)
	if a.Seq != first.ID || b.Seq != second.ID || b.Detail != "first, second" || a.MessageID != b.MessageID || b.Revision != 2 {
		t.Fatalf("unordered revisions: %+v %+v", a, b)
	}
	response.Body.Close()
	replay := connect(fmt.Sprint(first.ID))
	defer replay.Body.Close()
	replayScanner := bufio.NewScanner(replay.Body)
	recovered := read(replayScanner)
	if recovered.Seq != second.ID || recovered.Detail != b.Detail {
		t.Fatalf("replay differs from live: %+v", recovered)
	}
	// No broadcast at all: periodic DB catch-up must recover the last update.
	third := appendRevision(3, "complete reply")
	recovered = read(replayScanner)
	if recovered.Seq != third.ID || recovered.Streaming || recovered.Detail != "complete reply" {
		t.Fatalf("missed commit: %+v", recovered)
	}
}

func TestSSEExplicitTokenIsNotOverriddenByStaleCookie(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	s := New(ctx, m, t.TempDir(), m.dir, t.TempDir())
	token, err := signJWT(s.jwtKey)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(s.Handler())
	defer server.Close()
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/api/logs", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	request.AddCookie(&http.Cookie{Name: "scopeforge_token", Value: "stale-cookie"})
	history, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	history.Body.Close()
	if history.StatusCode != 200 {
		t.Fatalf("history status=%d", history.StatusCode)
	}
	request, _ = http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/api/logs/stream?token="+token, nil)
	request.AddCookie(&http.Cookie{Name: "scopeforge_token", Value: "stale-cookie"})
	live, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Body.Close()
	if live.StatusCode != 200 {
		t.Fatalf("history works but SSE is denied: status=%d", live.StatusCode)
	}
}
