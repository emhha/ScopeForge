package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"trpc.group/trpc-go/trpc-agent-go/agent/llmagent"
	"trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/tool"
)

// Use the real Responses adapter and production wrappers: projected-only fake
// models cannot expose SDK validation of native provider identity and payloads.
func TestTaskRouterNativeResponsesConversation(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		for _, edited := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%v/edited=%v", streaming, edited), func(t *testing.T) {
				oldAugment := agent.ToolAugment
				t.Cleanup(func() { agent.ToolAugment = oldAugment })
				callbacks := model.NewCallbacks().RegisterAfterModel(func(_ context.Context, args *model.AfterModelArgs) (*model.AfterModelResult, error) {
					if !edited || args.Response == nil || args.Response.IsPartial {
						return nil, nil
					}
					for i := range args.Response.Choices {
						data := args.Response.Choices[i].Message.ProviderData
						if data != nil {
							data.Payload = json.RawMessage(strings.ReplaceAll(string(data.Payload), "printf native", "printf projected"))
						}
					}
					return nil, nil
				})
				agent.ToolAugment = func(context.Context, string) ([]tool.CallableTool, agent.AugmentInfo, func()) {
					return nil, agent.AugmentInfo{AgentOptions: []llmagent.Option{llmagent.WithModelCallbacks(callbacks)}}, nil
				}
				var calls atomic.Int32
				api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var req struct {
						Model  string          `json:"model"`
						Stream bool            `json:"stream"`
						Input  json.RawMessage `json:"input"`
					}
					if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
						t.Error(err)
						w.WriteHeader(400)
						return
					}
					n := calls.Add(1)
					if req.Model != "deepseek-flash" || req.Stream != streaming {
						t.Errorf("request model=%q stream=%v", req.Model, req.Stream)
					}
					if n > 1 && !strings.Contains(string(req.Input), "opaque-route-state") {
						t.Error("native reasoning was lost between model calls")
					}
					if n == 2 {
						command := "printf native"
						if edited {
							command = "printf projected"
						}
						if !strings.Contains(string(req.Input), command) || !strings.Contains(string(req.Input), "function_call_output") {
							t.Errorf("native tool exchange missing: %s", req.Input)
						}
					}
					output := []map[string]any{{"type": "message", "id": fmt.Sprintf("msg-%d", n), "role": "assistant", "status": "completed", "content": []map[string]any{{"type": "output_text", "text": fmt.Sprintf("reply-%d", n), "annotations": []any{}}}}}
					if n == 1 {
						output = []map[string]any{
							{"type": "reasoning", "id": "rs-1", "summary": []any{}, "encrypted_content": "opaque-route-state"},
							{"type": "function_call", "id": "fc-1", "call_id": "call-1", "name": "Bash", "arguments": `{"command":"printf native > route-result.txt"}`},
						}
					}
					response := map[string]any{"id": fmt.Sprintf("response-%d", n), "object": "response", "status": "completed", "model": "deepseek-flash", "output": output, "usage": map[string]int{"input_tokens": 10, "output_tokens": 3, "total_tokens": 13}}
					if streaming {
						w.Header().Set("Content-Type", "text/event-stream")
						event, _ := json.Marshal(map[string]any{"type": "response.completed", "response": response})
						fmt.Fprintf(w, "event: response.completed\ndata: %s\n\n", event)
					} else {
						w.Header().Set("Content-Type", "application/json")
						_ = json.NewEncoder(w).Encode(response)
					}
				}))
				defer api.Close()
				m, err := NewManager(t.TempDir(), "")
				if err != nil {
					t.Fatal(err)
				}
				defer m.Close()
				id, err := m.pg.SaveProfile(&db.LLMProfile{Name: "native-responses", Format: "openai-responses", Model: "deepseek-flash", APIKey: "fixture", BaseURL: api.URL, Streaming: streaming})
				if err != nil {
					t.Fatal(err)
				}
				task, err := m.CreateTaskWithOptions("native routing", "fixture", db.TaskCreateOptions{LLMProfileIDs: []int64{id}})
				if err != nil {
					t.Fatal(err)
				}
				s := &Server{m: m, provByProfile: map[int64]*provEntry{}}
				route := &taskLLMRuntime{s: s, taskID: task.ID, agentKey: "worker"}
				ca := agent.NewChatAgent(route, "task-router", m.dir, m.sessions, 100000)
				ca.SetNonStreaming(func() bool { return !streaming })
				for i, prompt := range []string{"run probe", "continue conversation"} {
					text, err := ca.Chat(t.Context(), "worker", "conv-native", prompt, 4, 0, false, nil)
					if err != nil || text != fmt.Sprintf("reply-%d", i+2) {
						t.Fatalf("turn %d: text=%q error=%v", i+1, text, err)
					}
				}
				if calls.Load() != 3 {
					t.Fatalf("provider calls=%d, want 3", calls.Load())
				}
				got, err := os.ReadFile(filepath.Join(m.dir, "sessions", "conv-native", "route-result.txt"))
				want := "native"
				if edited {
					want = "projected"
				}
				if err != nil || string(got) != want {
					t.Fatalf("tool output=%q error=%v", got, err)
				}
			})
		}
	}
}
