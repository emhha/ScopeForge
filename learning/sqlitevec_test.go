package learning

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"context"
	"github.com/emhha/scopeforge/internal/modeltest"
	"github.com/emhha/scopeforge/internal/semantic"
	"trpc.group/trpc-go/trpc-agent-go/agent/llmagent"
	"trpc.group/trpc-go/trpc-agent-go/knowledge"
	"trpc.group/trpc-go/trpc-agent-go/memory"
	"trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/runner"
)

func installTestComponent(t *testing.T, k *Knowledge, local bool) {
	t.Helper()
	catalog, err := filepath.Abs(filepath.Join("..", "dist", "semantic", "catalog.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(catalog); err != nil {
		t.Skip("run make semantic-component to test the real native component")
	}
	k.components.Close()
	k.components = semantic.NewInstaller(filepath.Join(k.dir, "components"), catalog)
	k.semantic.installer = k.components
	bundle := ""
	if local {
		source := filepath.Join("..", "dist", "semantic-linux-amd64.zip")
		if semantic.Platform() != "linux/amd64" {
			t.Skip("local fixture is for linux/amd64")
		}
		if _, err := os.Stat(source); err != nil {
			t.Skip("run make semantic-offline to test the model")
		}
		// Installer owns/removes its input spool, as with the HTTP import route.
		data, err := os.ReadFile(source)
		if err != nil {
			t.Fatal(err)
		}
		bundle = filepath.Join(t.TempDir(), "bundle.zip")
		if err = os.WriteFile(bundle, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := k.components.Install(local, bundle); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Minute)
	for k.components.Status().Installing && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	s := k.components.Status()
	if s.Installing || !s.Installed || s.Error != "" || local && !s.ModelInstalled {
		t.Fatalf("component install failed: %+v", s)
	}
	if k.Config().Mode != "none" {
		t.Fatal("installation implicitly enabled memory embeddings")
	}
	if client, _ := k.semantic.remote(); client != nil {
		t.Fatal("installation started the active semantic client")
	}
}

func TestRealSQLiteVecRemoteEmbeddingIsolationAndFallback(t *testing.T) {
	pg, dir := testDB(t)
	k, err := OpenKnowledge(dir, pg)
	if err != nil {
		t.Fatal(err)
	}
	defer k.Close()
	installTestComponent(t, k, false)
	endpoint := embeddingServer(t)
	defer endpoint.Close()
	c := EmbeddingConfig{Mode: "remote", BaseURL: endpoint.URL + "/v1", Model: "test"}
	checked, err := k.TestConnection(t.Context(), c)
	if err != nil || checked.Dimensions != 3 {
		t.Fatalf("probe %+v %v", checked, err)
	}
	c.Dimensions = checked.Dimensions
	if err = k.Configure(c); err != nil {
		t.Fatal(err)
	}
	d, err := k.Upload("ownership.md", strings.NewReader("Compare ownership across independent subjects."))
	if err != nil {
		t.Fatal(err)
	}
	waitReady(t, pg, d.ID)
	k.semantic.ensure(k.embedding())
	if err = k.syncDocuments(t.Context()); err != nil {
		t.Fatal(err)
	}
	result, err := k.Search(t.Context(), &knowledge.SearchRequest{Query: "跨用户访问控制"})
	if err != nil || len(result.Documents) != 1 {
		t.Fatalf("native semantic search: %+v %v", result, err)
	}
	m, err := OpenMemories(dir, pg)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	m.SetSemanticIndex(k.semantic)
	a, _ := pg.CreateTask("a", "goal", nil, 0, 0)
	b, _ := pg.CreateTask("b", "goal", nil, 0, 0)
	if err = m.Task(a.ID).AddMemory(t.Context(), memory.UserKey{}, "Compare ownership across independent subjects.", nil); err != nil {
		t.Fatal(err)
	}
	if err = m.Task(b.ID).AddMemory(t.Context(), memory.UserKey{}, "Private task B record", nil); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err = m.syncOneScope(t.Context(), k.semantic); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := m.Task(a.ID).SearchMemories(t.Context(), taskKey(b.ID), "跨用户访问控制", memory.WithSearchOptions(memory.SearchOptions{Query: "跨用户访问控制", HybridSearch: true, Deduplicate: true}))
	if err != nil || len(entries) != 1 || strings.Contains(entries[0].Memory.Memory, "Private") {
		t.Fatalf("scope leak or missing vector hit: %+v %v", entries, err)
	}

	// Exercise the actual native memory_search tool, whose defaults enable
	// HybridSearch and Deduplicate, rather than only calling Reader directly.
	calls := 0
	mdl := modeltest.Func(func(ctx context.Context, req *model.Request) (<-chan *model.Response, error) {
		calls++
		if calls == 1 {
			return modeltest.Responses(&model.Response{Choices: []model.Choice{{Message: model.Message{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{{ID: "search", Type: "function", Function: model.FunctionDefinitionParam{Name: "memory_search", Arguments: []byte(`{"query":"跨用户访问控制"}`)}}}}}}, Done: true}), nil
		}
		found := false
		for _, msg := range req.Messages {
			if msg.Role == model.RoleTool && strings.Contains(msg.Content, "Compare ownership") {
				found = true
			}
		}
		if !found {
			t.Error("native tool bypassed semantic retrieval")
		}
		return modeltest.Responses(modeltest.Final("done")), nil
	})
	scoped := m.Task(a.ID)
	run := runner.NewRunner("operator", llmagent.New("memory-agent", llmagent.WithModel(mdl), llmagent.WithTools(scoped.Tools())), runner.WithMemoryService(scoped))
	events, err := run.Run(t.Context(), "operator", "semantic-test", model.NewUserMessage("search saved context"))
	if err != nil {
		t.Fatal(err)
	}
	for ev := range events {
		if ev.Error != nil {
			t.Fatal(ev.Error)
		}
	}
	run.Close()
	if err = m.Task(a.ID).DeleteMemory(t.Context(), memory.Key{MemoryID: entries[0].ID}); err != nil {
		t.Fatal(err)
	}
	entries, err = m.Task(a.ID).SearchMemories(t.Context(), memory.UserKey{}, "跨用户访问控制")
	if err != nil || len(entries) != 0 {
		t.Fatal("stale vector returned after deletion", entries, err)
	}
	endpoint.Close()
	result, err = k.Search(t.Context(), &knowledge.SearchRequest{Query: "independent"})
	if err != nil || len(result.Documents) != 1 {
		t.Fatal("outage broke keywords", result, err)
	}
	if k.semantic.Status().Ready {
		t.Fatal("outage did not report fallback")
	}
	if err = k.Delete(t.Context(), d.ID); err != nil {
		t.Fatal(err)
	}
	result, err = k.Search(t.Context(), &knowledge.SearchRequest{Query: "independent"})
	if err != nil || len(result.Documents) != 0 {
		t.Fatal("deleted document returned", result, err)
	}
}

func TestOfflineLocalModelInstallEnableAndDisable(t *testing.T) {
	pg, dir := testDB(t)
	k, err := OpenKnowledge(dir, pg)
	if err != nil {
		t.Fatal(err)
	}
	defer k.Close()
	installTestComponent(t, k, true)
	if err = k.Configure(EmbeddingConfig{Mode: "local"}); err != nil {
		t.Fatal(err)
	}
	k.semantic.ensure(k.embedding())
	client, _ := k.semantic.remote()
	if client == nil || client.ModelLoaded() {
		t.Fatal("configuration eagerly loaded model")
	}
	d, err := k.Upload("权限.md", strings.NewReader("检查不同用户访问同一资源时的权限边界。"))
	if err != nil {
		t.Fatal(err)
	}
	waitReady(t, pg, d.ID)
	if err = k.syncDocuments(t.Context()); err != nil {
		t.Fatal(err)
	}
	result, err := k.Search(t.Context(), &knowledge.SearchRequest{Query: "how to check object ownership between accounts"})
	if err != nil || len(result.Documents) != 1 {
		t.Fatalf("local multilingual retrieval: %+v %v", result, err)
	}
	if !client.Running() {
		t.Fatal("inference did not start component")
	}
	if err = k.Configure(EmbeddingConfig{Mode: "none"}); err != nil {
		t.Fatal(err)
	}
	if client.Running() {
		t.Fatal("disabling semantic retrieval retained model process")
	}
	result, err = k.Search(t.Context(), &knowledge.SearchRequest{Query: "权限"})
	if err != nil || len(result.Documents) != 1 {
		t.Fatal("disable broke keywords", result, err)
	}
}
