package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/emhha/scopeforge/db"
	"trpc.group/trpc-go/trpc-agent-go/model"
)

// Exercise the same config endpoint used by Agent selectors, then route real
// SDK requests. Distinct protocols catch accidental reuse of another role's model.
func TestAgentLLMBindingsRouteRolesIndependently(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if req.Model == "gpt-planner" {
			if r.URL.Path != "/v1/responses" {
				t.Errorf("GPT path=%s", r.URL.Path)
			}
			fmt.Fprintf(w, `{"id":"gpt-response","object":"response","status":"completed","model":"gpt-planner","output":[{"type":"message","id":"gpt-message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"gpt-planner","annotations":[]}]}],"usage":{"input_tokens":3,"output_tokens":1,"total_tokens":4}}`)
		} else {
			if r.URL.Path != "/v1/chat/completions" {
				t.Errorf("DP/fallback path=%s", r.URL.Path)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "chat-response", "model": req.Model, "choices": []map[string]any{{"index": 0, "message": map[string]string{"role": "assistant", "content": req.Model}, "finish_reason": "stop"}}, "usage": map[string]int{"prompt_tokens": 3, "completion_tokens": 1, "total_tokens": 4}})
		}
	}))
	defer api.Close()
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	ids := map[string]int64{}
	for name, format := range map[string]string{"deepseek-flash": "openai", "gpt-planner": "openai-responses", "task-fallback": "openai"} {
		id, err := m.pg.SaveProfile(&db.LLMProfile{Name: name, Format: format, Model: name, APIKey: "fixture", BaseURL: api.URL + "/v1", Streaming: false})
		if err != nil {
			t.Fatal(err)
		}
		ids[name] = id
	}
	if _, err := m.pg.CreateAgent("research_helper", "研究助手", ""); err != nil {
		t.Fatal(err)
	}
	task, err := m.CreateTaskWithOptions("split agent models", "fixture", db.TaskCreateOptions{LLMProfileIDs: []int64{ids["task-fallback"]}})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.pg.SetAgentMaxTurns("worker", 19); err != nil {
		t.Fatal(err)
	}
	s := &Server{m: m, ctx: t.Context(), engine: NewEngine(m), jwtKey: []byte("agent-binding-test-key"), provByProfile: map[int64]*provEntry{}}
	handler := s.Handler()
	token, err := signJWT(s.jwtKey)
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, path string, body any) (int, map[string]any) {
		t.Helper()
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(method, path, bytes.NewReader(data))
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s: %s", path, rec.Body.String())
		}
		return rec.Code, out
	}
	bind := func(key string, id *int64) {
		t.Helper()
		code, out := request("PUT", "/api/agents/"+key+"/config", map[string]any{"llm_profile_id": id})
		if code != 200 {
			t.Fatalf("bind %s: status=%d body=%v", key, code, out)
		}
		code, out = request("GET", "/api/agents/"+key, nil)
		if code != 200 {
			t.Fatalf("get %s: status=%d", key, code)
		}
		actual := out["agent"].(map[string]any)["llm_profile_id"]
		if (id == nil && actual != nil) || (id != nil && actual != float64(*id)) {
			t.Fatalf("%s binding=%v want=%v", key, actual, id)
		}
		for _, item := range out["llm_profiles"].([]any) {
			if _, exists := item.(map[string]any)["api_key"]; exists {
				t.Fatal("profile choices exposed credentials")
			}
		}
	}
	agents, err := m.pg.ListAgents()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range agents {
		id := ids["deepseek-flash"]
		if a.Key == "planner" {
			id = ids["gpt-planner"]
		}
		bind(a.Key, &id)
	}
	call := func(route *taskLLMRuntime, want string) {
		t.Helper()
		if info := route.Info(); info.Name != want {
			t.Fatalf("%s info=%+v want=%s", route.agentKey, info, want)
		}
		req := &model.Request{Messages: []model.Message{model.NewUserMessage("fixture")}}
		events, err := collectTaskLLMStream(route.GenerateContent(t.Context(), req))
		if err != nil {
			t.Fatal(err)
		}
		var text string
		for _, ev := range events {
			for _, choice := range ev.Choices {
				text += choice.Message.Content
			}
		}
		if text != want {
			t.Fatalf("%s called %q want=%q", route.agentKey, text, want)
		}
	}
	bundle := s.agentsForTask(task)
	call(bundle.runtime, "deepseek-flash")
	call(bundle.plannerRuntime, "gpt-planner")
	call(bundle.workerRuntime, "deepseek-flash")
	call(bundle.mainRuntime, "deepseek-flash")
	if ca := s.resolveChatAgent(&db.Conversation{AgentKey: "research_helper"}); ca == nil {
		t.Fatal("custom agent binding requires a global profile")
	}
	gpt := ids["gpt-planner"]
	bind("goals", &gpt)
	if next := s.agentsForTask(task); next == bundle {
		t.Fatal("model change retained a stale task bundle")
	} else {
		bundle = next
	}
	call(bundle.runtime, "gpt-planner")
	call(bundle.plannerRuntime, "gpt-planner")
	bind("goals", nil)
	bundle = s.agentsForTask(task)
	call(bundle.runtime, "task-fallback")
	call(bundle.plannerRuntime, "gpt-planner")
	if worker, err := m.pg.GetAgentByKey("worker"); err != nil || worker.MaxTurns != 19 {
		t.Fatalf("LLM-only save changed worker budget: %+v %v", worker, err)
	}
	if code, _ := request("PUT", "/api/agents/planner/config", map[string]any{"llm_profile_id": 999999}); code != 400 {
		t.Fatalf("invalid profile status=%d", code)
	}
	call(bundle.plannerRuntime, "gpt-planner")
}
