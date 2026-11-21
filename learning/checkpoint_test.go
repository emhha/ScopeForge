package learning

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emhha/scopeforge/db"
	"github.com/emhha/scopeforge/internal/modeltest"
	"trpc.group/trpc-go/trpc-agent-go/memory"
	"trpc.group/trpc-go/trpc-agent-go/model"
)

func TestCheckpointPersistsWithoutModelAndKeepsTaskScope(t *testing.T) {
	pg, dir := testDB(t)
	task, err := pg.CreateTask("checkpoint", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	other, err := pg.CreateTask("other", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pg.Exploration(task.ExplorationID).AppendActivity(db.Activity{Kind: "result", Summary: "比较独立身份后，缓存干扰仍需排除。"}); err != nil {
		t.Fatal(err)
	}
	if _, err = pg.Exploration(other.ExplorationID).AppendActivity(db.Activity{Kind: "result", Summary: "OTHER-TASK-PRIVATE"}); err != nil {
		t.Fatal(err)
	}
	m, err := OpenMemories(dir, pg)
	if err != nil {
		t.Fatal(err)
	}
	if !m.Settings().Checkpoint {
		t.Fatal("checkpoint extraction is not on by default")
	}
	if err = m.QueueCheckpoint(task.ID); err != nil {
		t.Fatal(err)
	}
	if err = m.Close(); err != nil {
		t.Fatal(err)
	}
	m, err = OpenMemories(dir, pg)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	var source, revision, processed string
	if err = m.conn.QueryRow(`SELECT source,revision,processed FROM checkpoints WHERE task_id=?`, task.ID).Scan(&source, &revision, &processed); err != nil {
		t.Fatal(err)
	}
	if processed == revision || strings.Contains(source, "OTHER-TASK-PRIVATE") {
		t.Fatal("checkpoint lost or included another task")
	}
	calls := 0
	m.SetTaskModelProvider(func(id int64) model.Model {
		if id != task.ID {
			t.Fatalf("wrong task model %d", id)
		}
		return modeltest.Func(func(ctx context.Context, req *model.Request) (<-chan *model.Response, error) {
			calls++
			args, _ := json.Marshal(map[string]any{"memory": "权限比较后仍需排除缓存干扰，结论尚未确认。", "memory_kind": "fact"})
			return modeltest.Responses(&model.Response{Choices: []model.Choice{{Message: model.Message{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{{ID: "checkpoint", Type: "function", Function: model.FunctionDefinitionParam{Name: "memory_add", Arguments: args}}}}}}, Done: true}), nil
		})
	})
	if err = m.checkpointNext(t.Context()); err != nil {
		t.Fatal(err)
	}
	entries, err := m.Task(task.ID).ReadMemories(t.Context(), memory.UserKey{}, 10)
	if err != nil || len(entries) != 1 {
		t.Fatalf("checkpoint memory: %+v %v", entries, err)
	}
	if global, err := m.Global(t.Context(), ""); err != nil || len(global) != 0 {
		t.Fatal("checkpoint bypassed global abstraction review")
	}
	if err = m.QueueCheckpoint(task.ID); err != nil {
		t.Fatal(err)
	}
	if err = m.checkpointNext(t.Context()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("unchanged checkpoint was extracted again")
	}
	if _, err = pg.Exploration(task.ExplorationID).AppendActivity(db.Activity{Kind: "result", Summary: "新的阶段记录"}); err != nil {
		t.Fatal(err)
	}
	if err = m.QueueCheckpoint(task.ID); err != nil {
		t.Fatal(err)
	}
	if err = pg.DeleteTask(task.ID); err != nil {
		t.Fatal(err)
	}
	if err = m.ClearTask(t.Context(), task.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	if err = m.conn.QueryRow(`SELECT count(*) FROM checkpoints WHERE task_id=?`, task.ID).Scan(&n); err != nil || n != 0 {
		t.Fatal("deleted task retained checkpoint input", n, err)
	}
}

func TestDefaultStageFlowAutomaticallyPublishesOnlyReviewedExperience(t *testing.T) {
	pg, dir := testDB(t)
	task, err := pg.CreateTask("stage", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pg.Exploration(task.ExplorationID).AppendActivity(db.Activity{Kind: "result", Summary: "已排除缓存干扰，准备比较独立主体的对象归属。"}); err != nil {
		t.Fatal(err)
	}
	m, err := OpenMemories(dir, pg)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	var calls atomic.Int32
	experience := "适用场景：验证权限边界。建议思路：比较不同主体的对象归属。验证步骤：排除缓存和对象不存在的干扰因素。不适用条件：身份与对象归属无法确认时不能得出结论。"
	mdl := modeltest.Func(func(ctx context.Context, req *model.Request) (<-chan *model.Response, error) {
		n := calls.Add(1)
		if n == 3 {
			return modeltest.Responses(modeltest.Final(`{"approved":true}`)), nil
		}
		text := "当前阶段已经排除缓存干扰，权限比较仍需继续验证。"
		if n == 2 {
			text = experience
		}
		args, _ := json.Marshal(map[string]any{"memory": text, "memory_kind": "fact"})
		return modeltest.Responses(&model.Response{Choices: []model.Choice{{Message: model.Message{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{{ID: "extract", Type: "function", Function: model.FunctionDefinitionParam{Name: "memory_add", Arguments: args}}}}}}, Done: true}), nil
	})
	m.SetTaskModelProvider(func(int64) model.Model { return mdl })
	if err = m.QueueCheckpoint(task.ID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		entries, err := m.Global(t.Context(), "")
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) == 1 {
			if entries[0].Memory.Memory != experience || calls.Load() != 3 {
				t.Fatal("unexpected global write", entries, calls.Load())
			}
			if strings.Contains(entries[0].Memory.Memory, "当前阶段") {
				t.Fatal("concrete stage information escaped into global memory")
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("default stage/checkpoint/review pipeline did not publish experience")
}

func TestDisablingTaskMemoryStopsExistingReadersWithoutDeletingData(t *testing.T) {
	pg, dir := testDB(t)
	task, err := pg.CreateTask("toggle", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	m, err := OpenMemories(dir, pg)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	svc := m.Task(task.ID)
	if err = svc.AddMemory(t.Context(), memory.UserKey{}, "existing task fact", nil); err != nil {
		t.Fatal(err)
	}
	settings := m.Settings()
	settings.TaskEnabled = false
	if err = m.Configure(settings); err != nil {
		t.Fatal(err)
	}
	if entries, err := svc.ReadMemories(t.Context(), memory.UserKey{}, 10); err != nil || len(entries) != 0 {
		t.Fatal("existing preload/loader bypassed disabled setting", entries, err)
	}
	if err = svc.AddMemory(t.Context(), memory.UserKey{}, "late write", nil); err == nil {
		t.Fatal("disabled memory accepted a write")
	}
	settings.TaskEnabled = true
	if err = m.Configure(settings); err != nil {
		t.Fatal(err)
	}
	if entries, err := svc.ReadMemories(t.Context(), memory.UserKey{}, 10); err != nil || len(entries) != 1 {
		t.Fatal("toggle deleted persisted memory", entries, err)
	}
}
