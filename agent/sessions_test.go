package agent

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emhha/scopeforge/internal/modeltest"
	"github.com/emhha/scopeforge/toolkit"
	"trpc.group/trpc-go/trpc-agent-go/event"
	"trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/session"
	"trpc.group/trpc-go/trpc-agent-go/tool"
)

type persistentProtocolFixture struct{ model.Model }

func (m persistentProtocolFixture) Info() model.Info {
	return model.Info{Name: "fixture", ProviderDataProtocol: "openai.responses"}
}

func TestPersistentNativeContextToolsStateAndSummaryAfterReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.sqlite")
	service, err := OpenSessionService(path)
	if err != nil {
		t.Fatal(err)
	}
	key := session.Key{AppName: "scopeforge", UserID: "operator", SessionID: "conv-1"}
	if _, err = service.CreateSession(t.Context(), key, session.StateMap{"skill:loaded": []byte(`["fixture"]`)}); err != nil {
		t.Fatal(err)
	}
	var executions atomic.Int32
	probe := toolkit.New("probe", "fixture", map[string]any{"type": "object"}, func(context.Context, json.RawMessage) (toolkit.Output, error) {
		executions.Add(1)
		return toolkit.Text("recorded tool result"), nil
	})
	calls := 0
	mdl := modeltest.Func(func(_ context.Context, req *model.Request) (<-chan *model.Response, error) {
		calls++
		if calls == 1 {
			return modeltest.Responses(&model.Response{Choices: []model.Choice{{Message: model.Message{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{{ID: "probe-1", Type: "function", Function: model.FunctionDefinitionParam{Name: "probe", Arguments: []byte(`{}`)}}}}}}}), nil
		}
		return modeltest.Responses(modeltest.Final("completed turn")), nil
	})
	cfg := RunConfig{Model: mdl, Sessions: service, SessionID: key.SessionID, Tools: []tool.CallableTool{probe}, DisableHostTools: true, MaxTurns: 3}
	if _, _, err = captureRun(t.Context(), cfg, "original question", nil); err != nil {
		t.Fatal(err)
	}
	sess, err := service.GetSession(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	payload := json.RawMessage(`[{"type":"reasoning","id":"rs-fixture","encrypted_content":"opaque-state"}]`)
	e := event.NewResponseEvent("payload", "fixture", &model.Response{Choices: []model.Choice{{Message: model.Message{Role: model.RoleAssistant, ProviderData: &model.ProviderData{Protocol: "openai.responses", Model: "fixture", Payload: payload}}}}})
	if err = service.AppendEvent(t.Context(), sess, e); err != nil {
		t.Fatal(err)
	}
	summaryModel := modeltest.Func(func(context.Context, *model.Request) (<-chan *model.Response, error) {
		return modeltest.Responses(modeltest.Final("summary retains the original question and tool result")), nil
	})
	ctx := context.WithValue(t.Context(), summaryModelKey{}, summaryModel)
	if err = service.CreateSessionSummary(ctx, sess, "", true); err != nil {
		t.Fatal(err)
	}
	if err = service.Close(); err != nil {
		t.Fatal(err)
	}
	service, err = OpenSessionService(path)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	sess, err = service.GetSession(t.Context(), key)
	if err != nil || sess == nil {
		t.Fatalf("restore=%+v err=%v", sess, err)
	}
	if len(sess.Summaries) == 0 || string(sess.State["skill:loaded"]) != `["fixture"]` {
		t.Fatal("summary or skill state lost")
	}
	foundPayload, foundTool := false, false
	for _, ev := range sess.GetEvents() {
		for _, choice := range ev.Choices {
			m := choice.Message
			foundTool = foundTool || (m.Role == model.RoleTool && strings.Contains(m.Content, "recorded tool result"))
			foundPayload = foundPayload || (m.ProviderData != nil && strings.Contains(string(m.ProviderData.Payload), "opaque-state"))
		}
	}
	if !foundPayload || !foundTool {
		t.Fatal("native tool result or opaque provider data was lost")
	}
	cfg.Sessions = service
	cfg.Model = persistentProtocolFixture{modeltest.Func(func(_ context.Context, req *model.Request) (<-chan *model.Response, error) {
		found := false
		for _, m := range req.Messages {
			found = found || strings.Contains(m.Content, "summary retains") || m.Content == "original question"
		}
		if !found {
			t.Error("restored context not sent to next model turn")
		}
		return modeltest.Responses(modeltest.Final("continued")), nil
	})}
	if _, _, err = captureRun(t.Context(), cfg, "follow-up after restart", nil); err != nil {
		t.Fatal(err)
	}
	if executions.Load() != 1 {
		t.Fatal("completed tool was replayed after restart")
	}
}

func TestNativePauseCancellationPersistsAndCanContinue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.sqlite")
	service, err := OpenSessionService(path)
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	var executions atomic.Int32
	probe := toolkit.New("wait_probe", "fixture", map[string]any{"type": "object"}, func(ctx context.Context, _ json.RawMessage) (toolkit.Output, error) {
		executions.Add(1)
		close(started)
		<-ctx.Done()
		return toolkit.Error("interrupted; execution result is unknown"), nil
	})
	mdl := modeltest.Func(func(context.Context, *model.Request) (<-chan *model.Response, error) {
		return modeltest.Responses(&model.Response{Choices: []model.Choice{{Message: model.Message{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{{ID: "pause-1", Type: "function", Function: model.FunctionDefinitionParam{Name: "wait_probe", Arguments: []byte(`{}`)}}}}}}}), nil
	})
	cfg := RunConfig{Model: mdl, Sessions: service, SessionID: "exp1-worker-i1", Tools: []tool.CallableTool{probe}, DisableHostTools: true, MaxTurns: 2}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { _, _, err := captureRun(ctx, cfg, "start", nil); done <- err }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("tool did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("pause error=%v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pause did not settle")
	}
	if err = service.Close(); err != nil {
		t.Fatal(err)
	}
	service, err = OpenSessionService(path)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	cfg.Sessions = service
	cfg.Model = modeltest.Func(func(_ context.Context, req *model.Request) (<-chan *model.Response, error) {
		pending := map[string]bool{}
		for _, message := range req.Messages {
			for _, call := range message.ToolCalls {
				pending[call.ID] = true
			}
			if message.Role == model.RoleTool {
				delete(pending, message.ToolID)
			}
		}
		if len(pending) > 0 {
			t.Errorf("unmatched interrupted tool calls sent to model: %v", pending)
		}
		return modeltest.Responses(modeltest.Final("continue after pause")), nil
	})
	if _, _, err = captureRun(t.Context(), cfg, "continue", nil); err != nil {
		t.Fatal(err)
	}
	if executions.Load() != 1 {
		t.Fatal("interrupted tool automatically replayed")
	}
}
