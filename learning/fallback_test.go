package learning

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emhha/scopeforge/internal/modeltest"
	"trpc.group/trpc-go/trpc-agent-go/event"
	"trpc.group/trpc-go/trpc-agent-go/knowledge"
	"trpc.group/trpc-go/trpc-agent-go/memory"
	"trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/session"
)

func TestNoModelChineseKnowledgeAndNoNetwork(t *testing.T) {
	pg, dir := testDB(t)
	var calls atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(503) }))
	defer endpoint.Close()
	k, err := OpenKnowledge(dir, pg)
	if err != nil {
		t.Fatal(err)
	}
	defer k.Close()
	if err := k.Configure(EmbeddingConfig{Mode: "none", BaseURL: endpoint.URL, Model: "never-call"}); err != nil {
		t.Fatal(err)
	}
	doc, err := k.Upload("中文.md", strings.NewReader("# 越权验证\n检查对象归属与会话身份，避免缓存影响。\n接口 token_scope 需要保留原样。"))
	if err != nil {
		t.Fatal(err)
	}
	waitReady(t, pg, doc.ID)
	for _, query := range []string{"越权", "对象归属", "token_scope"} {
		result, err := k.Search(t.Context(), &knowledge.SearchRequest{Query: query})
		if err != nil || len(result.Documents) != 1 {
			t.Fatalf("keyword query %q: %+v %v", query, result, err)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("keyword mode issued %d network requests", calls.Load())
	}
	if !strings.Contains(k.Status().Message, "关键词") {
		t.Fatal(k.Status())
	}
}

func TestConfiguredAPIEmbedderDoesNotInheritChatCredentials(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "chat-secret-must-not-leak")
	t.Setenv("OPENAI_ORG_ID", "chat-organization")
	var authorization string
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		if r.Header.Get("OpenAI-Organization") != "" {
			t.Error("inherited chat organization")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"object":"embedding","index":0,"embedding":[1,0,0]}],"model":"local"}`))
	}))
	defer endpoint.Close()
	_, err := nativeEmbedder(EmbeddingConfig{Mode: "remote", BaseURL: endpoint.URL, Model: "local"}).GetEmbedding(t.Context(), "connection probe")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(authorization, "chat-secret") {
		t.Fatal("embedding request inherited the chat API key")
	}
}

func TestNativeAutoExtractionScopeAndLateDelete(t *testing.T) {
	for _, deleted := range []bool{false, true} {
		t.Run(fmt.Sprint("deleted=", deleted), func(t *testing.T) {
			pg, dir := testDB(t)
			task, err := pg.CreateTask("auto", "goal", nil, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			m, err := OpenMemories(dir, pg)
			if err != nil {
				t.Fatal(err)
			}
			if err := m.Configure(MemorySettings{TaskEnabled: true, GlobalEnabled: true, AutoExtract: true}); err != nil {
				t.Fatal(err)
			}
			entered, release := make(chan struct{}), make(chan struct{})
			var calls atomic.Int32
			m.SetModelProvider(func() model.Model {
				return modeltest.Func(func(ctx context.Context, req *model.Request) (<-chan *model.Response, error) {
					if calls.Add(1) == 1 {
						close(entered)
					}
					select {
					case <-release:
					case <-ctx.Done():
						return nil, ctx.Err()
					}
					return modeltest.Responses(&model.Response{Choices: []model.Choice{{Message: model.Message{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{{ID: "auto", Type: "function", Function: model.FunctionDefinitionParam{Name: "memory_add", Arguments: []byte(`{"memory":"task fact from native auto extraction","memory_kind":"fact"}`)}}}}}}, Done: true}), nil
				})
			})
			sess := session.NewSession("scopeforge", "operator", "existing-session", session.WithSessionEvents([]event.Event{*event.NewResponseEvent("run", "user", &model.Response{Choices: []model.Choice{{Message: model.NewUserMessage("请记住已经确认的任务事实")}}})}))
			if err := m.Task(task.ID).EnqueueAutoMemoryJob(t.Context(), sess); err != nil {
				t.Fatal(err)
			}
			select {
			case <-entered:
			case <-time.After(45 * time.Second):
				m.Close()
				t.Fatal("native auto extractor did not run")
			}
			if deleted {
				if err := pg.DeleteTask(task.ID); err != nil {
					t.Fatal(err)
				}
				if err := m.ClearTask(t.Context(), task.ID); err != nil {
					t.Fatal(err)
				}
			}
			close(release)
			if err := m.Close(); err != nil {
				t.Fatal(err)
			}
			if sess.AppName != "scopeforge" || sess.UserID != "operator" {
				t.Fatal("auto extraction changed original session identity")
			}
			m, err = OpenMemories(dir, pg)
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			entries, err := m.store.ReadMemories(t.Context(), taskKey(task.ID), 10)
			if err != nil {
				t.Fatal(err)
			}
			want := 1
			if deleted {
				want = 0
			}
			if len(entries) != want {
				t.Fatalf("native auto task scope: got %d want %d", len(entries), want)
			}
			if global, err := m.Global(t.Context(), ""); err != nil || len(global) != 0 {
				t.Fatal("native task auto extraction leaked into global memory")
			}
		})
	}
}

func TestAutomaticDistillQueueSurvivesRestartAndRetries(t *testing.T) {
	pg, dir := testDB(t)
	task, err := pg.CreateTask("task", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	m, err := OpenMemories(dir, pg)
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Settings(); !got.TaskEnabled || !got.GlobalEnabled || !got.AutoDistill || got.AutoExtract {
		t.Fatalf("unexpected defaults: %+v", got)
	}
	if err := m.Task(task.ID).AddMemory(t.Context(), memory.UserKey{}, "权限测试需要先排除缓存的影响。", nil); err != nil {
		t.Fatal(err)
	}
	m.QueueDistill(task.ID)
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	m, err = OpenMemories(dir, pg)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if m.JobStatus().Pending != 1 {
		t.Fatal("pending distillation was lost on restart")
	}
	m.SetModelProvider(func() model.Model {
		return modeltest.Func(func(context.Context, *model.Request) (<-chan *model.Response, error) {
			return nil, errors.New("temporary provider error")
		})
	})
	if err := m.distillNext(t.Context()); err == nil {
		t.Fatal("expected temporary error")
	}
	if m.JobStatus().Retrying != 1 {
		t.Fatal("failed job was not retained")
	}
	var calls atomic.Int32
	valid := "适用场景：验证权限边界。建议思路：比较不同主体的对象归属。验证步骤：排除缓存和对象不存在的干扰因素。不适用条件：身份与对象归属无法确认时不能得出结论。"
	mdl := modeltest.Func(func(ctx context.Context, req *model.Request) (<-chan *model.Response, error) {
		if calls.Add(1) == 1 {
			args, _ := json.Marshal(map[string]any{"memory": valid, "memory_kind": "fact"})
			return modeltest.Responses(&model.Response{Choices: []model.Choice{{Message: model.Message{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{{ID: "candidate", Type: "function", Function: model.FunctionDefinitionParam{Name: "memory_add", Arguments: args}}}}}}, Done: true}), nil
		}
		return modeltest.Responses(modeltest.Final(`{"approved":true}`)), nil
	})
	m.SetModelProvider(func() model.Model { return mdl })
	m.QueueDistill(task.ID)
	if err := m.distillNext(t.Context()); err != nil {
		t.Fatal(err)
	}
	global, err := m.Global(t.Context(), "")
	if err != nil || len(global) != 1 {
		t.Fatalf("automatic global promotion: %+v %v", global, err)
	}
	if m.JobStatus().Pending != 0 {
		t.Fatal("completed revision not acknowledged")
	}
	if err := m.distillNext(t.Context()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatal("unchanged task was distilled again")
	}
}
