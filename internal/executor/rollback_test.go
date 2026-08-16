package executor

import (
	"context"
	"testing"
	"time"

	"scopeforge/internal/conversation"
	"scopeforge/internal/event"
	"scopeforge/internal/reasonix/provider"
	"scopeforge/internal/reasonix/tool"

	_ "scopeforge/internal/reasonix/provider/openai"
	"scopeforge/internal/testutil"
)

// TestSessionRollback 会话配对损坏 → 回滚 recovery 分支(docs/03 §7)。
func TestSessionRollback(t *testing.T) {
	db := testutil.NewTestDB(t)
	store := conversation.NewStore(db)
	sess := conversation.New("rollback-1", conversation.KindMain)
	// 健康快照
	sess.Add(provider.Message{Role: provider.RoleUser, Content: "任务"})
	sess.Add(provider.Message{Role: provider.RoleAssistant, Content: "", ToolCalls: []provider.ToolCall{{ID: "c1", Name: "bash", Arguments: `{"command":"echo hi"}`}}})
	sess.Add(provider.Message{Role: provider.RoleTool, ToolCallID: "c1", Name: "bash", Content: "hi"})
	if err := store.SaveSnapshot(sess); err != nil {
		t.Fatal(err)
	}
	// 压缩后留 recovery 分支(健康内容)
	if err := store.SaveRecoveryBranch(sess, "pre-compaction"); err != nil {
		t.Fatal(err)
	}
	// 模拟损坏:直接构造含游离 tool 消息的会话(不经 LoadLatest,后者已优先 recovery)
	corrupt := conversation.New("rollback-1", conversation.KindMain)
	corrupt.Messages = append(append([]provider.Message{}, sess.Messages...),
		provider.Message{Role: provider.RoleTool, ToolCallID: "orphan", Name: "bash", Content: "orphan result"})
	corrupt.Provider = "mock"
	// 同时把损坏内容保存为最新 snapshot(模拟压缩后落库)
	if err := store.SaveSnapshot(corrupt); err != nil {
		t.Fatal(err)
	}
	if reason := conversation.ValidatePairing(corrupt.Messages); reason == "" {
		t.Fatal("expected pairing corruption detected")
	}

	// 运行 executor:入口检测损坏 → 回滚 recovery
	events := &eventRecorder{}
	mock := testutil.NewMockOpenAIServer([]testutil.MockStep{
		{Text: "回滚后正常完成", FinishReason: "stop"},
	})
	p, err := provider.New("openai", provider.Config{Name: "mock", BaseURL: mock.URL, Model: "m", APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	gate, _ := NewGate(ModeYolo, nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := Run(ctx, &Options{
		Provider: p,
		Registry: tool.NewRegistry(),
		Session:  corrupt,
		Store:    store,
		MaxTurns: 3,
		Gate:     gate,
		Sink:     events,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	_ = res
	// 回滚事件已发
	if !events.sawAction("rollback_recovery") {
		t.Errorf("no rollback event, events: %v", events.kindsList())
	}
	// 会话已回滚为健康内容(无孤儿 tool)
	if reason := conversation.ValidatePairing(corrupt.Messages); reason != "" {
		t.Errorf("session still corrupted after rollback: %s", reason)
	}
}

// eventRecorder 记录事件。
type eventRecorder struct {
	kinds []event.Kind
	evs   []event.Event
}

func (e *eventRecorder) Emit(ev event.Event) {
	e.kinds = append(e.kinds, ev.Kind)
	e.evs = append(e.evs, ev)
}

func (e *eventRecorder) saw(k event.Kind) bool {
	for _, kk := range e.kinds {
		if kk == k {
			return true
		}
	}
	return false
}

// sawAction 检查 checkpoint 事件的 action 载荷。
func (e *eventRecorder) sawAction(action string) bool {
	for _, ev := range e.evs {
		if ev.Kind != event.KindCheckpoint {
			continue
		}
		if m, ok := ev.Payload.(map[string]any); ok {
			if a, ok := m["action"].(string); ok && a == action {
				return true
			}
		}
	}
	return false
}

func (e *eventRecorder) kindsList() []event.Kind { return e.kinds }

var _ = time.Now
