package agent

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/emhha/scopeforge/db"
	"github.com/emhha/scopeforge/internal/modeltest"
	"github.com/emhha/scopeforge/toolkit"
	"trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/tool"
)

func TestToolStepsArriveBeforeRunCompletesWithDurableSession(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	sessions, err := OpenSessionService(filepath.Join(t.TempDir(), "session.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer sessions.Close()
	releaseTool := make(chan struct{})
	releaseModel := make(chan struct{})
	defer func() {
		select {
		case <-releaseTool:
		default:
			close(releaseTool)
		}
		select {
		case <-releaseModel:
		default:
			close(releaseModel)
		}
	}()
	blocked := toolkit.New("blocked", "fixture", map[string]any{"type": "object"}, func(ctx context.Context, _ json.RawMessage) (toolkit.Output, error) {
		select {
		case <-releaseTool:
			return toolkit.Text("tool done"), nil
		case <-ctx.Done():
			return toolkit.Output{}, ctx.Err()
		}
	})
	calls := 0
	mdl := modeltest.Func(func(ctx context.Context, _ *model.Request) (<-chan *model.Response, error) {
		calls++
		if calls == 1 {
			return modeltest.Responses(&model.Response{Choices: []model.Choice{{Message: model.Message{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{{ID: "call", Type: "function", Function: model.FunctionDefinitionParam{Name: "blocked", Arguments: []byte(`{}`)}}}}}}}), nil
		}
		out := make(chan *model.Response)
		go func() {
			defer close(out)
			select {
			case <-releaseModel:
			case <-ctx.Done():
				return
			}
			select {
			case out <- modeltest.Final("done"):
			case <-ctx.Done():
			}
		}()
		return out, nil
	})
	records := make(chan db.Activity, 32)
	done := make(chan error, 1)
	go func() {
		_, _, err := captureRun(ctx, RunConfig{Model: mdl, Sessions: sessions, SessionID: "steps", Tools: []tool.CallableTool{blocked}, DisableHostTools: true, MaxTurns: 3}, "run", func(a db.Activity) { records <- a })
		done <- err
	}()
	waitKind := func(kind string) {
		for {
			select {
			case a := <-records:
				if a.Kind == kind {
					return
				}
			case err := <-done:
				t.Fatalf("run ended before %s: %v", kind, err)
			case <-ctx.Done():
				t.Fatalf("no live %s", kind)
			}
		}
	}
	waitKind("tool_use")
	close(releaseTool)
	waitKind("tool_result")
	select {
	case err := <-done:
		t.Fatalf("run completed before model release: %v", err)
	default:
	}
	close(releaseModel)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
