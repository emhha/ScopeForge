package learning

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/emhha/scopeforge/db"
	"github.com/emhha/scopeforge/internal/modeltest"
	"trpc.group/trpc-go/trpc-agent-go/agent/llmagent"
	"trpc.group/trpc-go/trpc-agent-go/knowledge"
	"trpc.group/trpc-go/trpc-agent-go/memory"
	"trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/runner"
)

func testDB(t *testing.T) (*db.DB, string) {
	t.Helper()
	dir := t.TempDir()
	pg, err := db.Open(filepath.Join(dir, "business.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pg.Close() })
	return pg, dir
}

func TestTaskMemoryScopePersistenceAndDeletion(t *testing.T) {
	pg, dir := testDB(t)
	first, err := pg.CreateTask("one", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	second, err := pg.CreateTask("two", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	m, err := OpenMemories(dir, pg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	a, b := m.Task(first.ID), m.Task(second.ID)
	spoof := memory.UserKey{AppName: "scopeforge-global", UserID: "shared"}
	if err := a.AddMemory(ctx, spoof, "alpha task evidence", nil); err != nil {
		t.Fatal(err)
	}
	entries, err := a.ReadMemories(ctx, spoof, 10)
	if err != nil || len(entries) != 1 {
		t.Fatalf("same task read: %v %v", entries, err)
	}
	if got, err := b.ReadMemories(ctx, spoof, 10); err != nil || len(got) != 0 {
		t.Fatalf("cross-task leak: %v %v", got, err)
	}
	if got, err := m.Global(ctx, ""); err != nil || len(got) != 0 {
		t.Fatalf("global leak: %v %v", got, err)
	}
	if err := b.UpdateMemory(ctx, memory.Key{MemoryID: entries[0].ID}, "stolen", nil); err == nil {
		t.Fatal("updated another task's entry")
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	m, err = OpenMemories(dir, pg)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	entries, err = m.Task(first.ID).ReadMemories(ctx, memory.UserKey{}, 10)
	if err != nil || len(entries) != 1 {
		t.Fatalf("restart lost memory: %v %v", entries, err)
	}
	if err := pg.DeleteTask(first.ID); err != nil {
		t.Fatal(err)
	}
	if err := m.ClearTask(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	if err := m.Task(first.ID).AddMemory(ctx, spoof, "late write", nil); err == nil {
		t.Fatal("deleted task accepted a late write")
	}
	if got, err := m.store.ReadMemories(ctx, taskKey(first.ID), 10); err != nil || len(got) != 0 {
		t.Fatalf("cleanup failed: %v %v", got, err)
	}
}

func TestExperienceValidation(t *testing.T) {
	valid := "适用场景：验证权限边界。建议思路：比较不同主体的对象归属。验证步骤：排除缓存和对象不存在的干扰因素。 不适用条件：身份与对象归属无法确认时不能得出结论。"
	if err := validateExperience(valid); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{" https://private.example.com", " /admin/users", " 某目标返回了错误", " 192.0.2.1", " password=secret"} {
		if err := validateExperience(valid + suffix); err == nil {
			t.Fatalf("accepted concrete information: %s", suffix)
		}
	}
}

func TestNativeMemoryToolContextUsesTaskScope(t *testing.T) {
	pg, dir := testDB(t)
	task, err := pg.CreateTask("native tools", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	m, err := OpenMemories(dir, pg)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	scoped := m.Task(task.ID)
	calls := 0
	mdl := modeltest.Func(func(ctx context.Context, req *model.Request) (<-chan *model.Response, error) {
		calls++
		if calls == 1 {
			return modeltest.Responses(&model.Response{Choices: []model.Choice{{Message: model.Message{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{{ID: "remember", Type: "function", Function: model.FunctionDefinitionParam{Name: "memory_add", Arguments: []byte(`{"memory":"remembered through native invocation","topics":[]}`)}}}}}}, Done: true}), nil
		}
		return modeltest.Responses(modeltest.Final("saved")), nil
	})
	ag := llmagent.New("test", llmagent.WithModel(mdl), llmagent.WithTools(scoped.Tools()))
	run := runner.NewRunner("scopeforge", ag, runner.WithMemoryService(scoped))
	defer run.Close()
	events, err := run.Run(t.Context(), "operator", "existing-session", model.NewUserMessage("remember"))
	if err != nil {
		t.Fatal(err)
	}
	for ev := range events {
		if ev != nil && ev.Response != nil && ev.Error != nil {
			t.Fatal(ev.Error)
		}
	}
	entries, err := scoped.ReadMemories(t.Context(), memory.UserKey{}, 10)
	if err != nil || len(entries) != 1 {
		t.Fatalf("native tool did not write in task scope: %+v %v", entries, err)
	}
	if entries, err := m.Global(t.Context(), ""); err != nil || len(entries) != 0 {
		t.Fatalf("native write leaked globally: %+v %v", entries, err)
	}
}

func TestGlobalPromotionRequiresBothAbstractionAndReview(t *testing.T) {
	valid := "适用场景：验证权限边界。建议思路：比较不同主体的对象归属。验证步骤：排除缓存和对象不存在的干扰因素。不适用条件：身份与对象归属无法确认时不能得出结论。"
	for _, tc := range []struct {
		name, text, verdict string
		want                int
	}{{"approved", valid, `{"approved":true}`, 1}, {"rejected", valid, `{"approved":false}`, 0}, {"malformed review", valid, `not json`, 0}, {"concrete", valid + " https://private.example.com", `{"approved":true}`, 0}} {
		t.Run(tc.name, func(t *testing.T) {
			pg, dir := testDB(t)
			task, err := pg.CreateTask("task", "goal", nil, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			m, err := OpenMemories(dir, pg)
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			if err := m.Task(task.ID).AddMemory(t.Context(), memory.UserKey{}, "比较权限时尚未排除缓存干扰，需要进一步验证。", nil); err != nil {
				t.Fatal(err)
			}
			calls := 0
			mdl := modeltest.Func(func(ctx context.Context, req *model.Request) (<-chan *model.Response, error) {
				calls++
				if calls == 1 {
					args, _ := json.Marshal(map[string]any{"memory": tc.text, "memory_kind": "fact", "topics": []string{"experience"}})
					return modeltest.Responses(&model.Response{Choices: []model.Choice{{Message: model.Message{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{{ID: "candidate", Type: "function", Function: model.FunctionDefinitionParam{Name: "memory_add", Arguments: args}}}}}}, Done: true}), nil
				}
				return modeltest.Responses(modeltest.Final(tc.verdict)), nil
			})
			_, _ = m.Distill(t.Context(), task.ID, mdl)
			entries, err := m.Global(t.Context(), "")
			if err != nil || len(entries) != tc.want {
				t.Fatalf("global entries=%d, want %d (model calls %d): %v", len(entries), tc.want, calls, err)
			}
		})
	}
}

func embeddingServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			http.NotFound(w, r)
			return
		}
		var req struct {
			Input json.RawMessage `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		var inputs []string
		if json.Unmarshal(req.Input, &inputs) != nil {
			var one string
			_ = json.Unmarshal(req.Input, &one)
			inputs = []string{one}
		}
		data := []any{}
		for i := range inputs {
			data = append(data, map[string]any{"object": "embedding", "index": i, "embedding": []float64{1, 0, 0}})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data, "model": "test", "usage": map[string]int{"prompt_tokens": 1, "total_tokens": 1}})
	}))
}
func waitReady(t *testing.T, pg *db.DB, id string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		docs, err := pg.KnowledgeDocuments()
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range docs {
			if d.ID != id {
				continue
			}
			if d.Status == "failed" {
				t.Fatalf("index failed: %+v", d)
			}
			if d.Status == "ready" {
				if d.Chunks < 1 {
					t.Fatal("empty index")
				}
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("index timed out")
}
func TestNativeKnowledgeUploadSearchRestartDelete(t *testing.T) {
	pg, dir := testDB(t)
	endpoint := embeddingServer(t)
	defer endpoint.Close()
	k, err := OpenKnowledge(dir, pg)
	if err != nil {
		t.Fatal(err)
	}
	if err := k.Configure(EmbeddingConfig{Mode: "none", BaseURL: endpoint.URL + "/v1", Model: "test", Dimensions: 3, APIKey: "not-returned"}); err != nil {
		k.Close()
		t.Fatal(err)
	}
	if k.Config().APIKey != "" || k.Config().Configured {
		t.Fatal("config disclosed key or is not configured")
	}
	doc, err := k.Upload("guide.md", strings.NewReader("# Review\nCompare object ownership and exclude cache interference."))
	if err != nil {
		t.Fatal(err)
	}
	waitReady(t, pg, doc.ID)
	result, err := k.Search(t.Context(), &knowledge.SearchRequest{Query: "ownership"})
	if err != nil || len(result.Documents) != 1 {
		t.Fatalf("search: %+v %v", result, err)
	}
	if result.Documents[0].Document.Metadata["file_name"] != "guide.md" {
		t.Fatalf("missing citation: %+v", result.Documents[0])
	}
	if err := k.Close(); err != nil {
		t.Fatal(err)
	}
	// Both index reconstruction and the repeated query must work offline from
	// cached embeddings; restarting must not silently bill the provider again.
	endpoint.Close()
	k, err = OpenKnowledge(dir, pg)
	if err != nil {
		t.Fatal(err)
	}
	defer k.Close()
	waitReady(t, pg, doc.ID)
	result, err = k.Search(t.Context(), &knowledge.SearchRequest{Query: "ownership"})
	if err != nil || len(result.Documents) != 1 {
		t.Fatalf("persistent index: %+v %v", result, err)
	}
	if err := pg.SetKnowledgeDocumentStatus(doc.ID, "failed", "test partial source", 0); err != nil {
		t.Fatal(err)
	}
	result, err = k.Search(t.Context(), &knowledge.SearchRequest{Query: "ownership"})
	if err != nil || len(result.Documents) != 0 {
		t.Fatalf("failed source leaked: %+v %v", result, err)
	}
	if err := k.Delete(context.Background(), doc.ID); err != nil {
		t.Fatal(err)
	}
	result, err = k.Search(t.Context(), &knowledge.SearchRequest{Query: "ownership"})
	if err != nil || len(result.Documents) != 0 {
		t.Fatalf("deleted document leaked: %+v %v", result, err)
	}
}
