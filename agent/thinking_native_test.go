package agent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	llm "trpc.group/trpc-go/trpc-agent-go/model"
)

func TestNativeThinkingConfigurationOnWire(t *testing.T) {
	for _, tc := range []struct {
		name, format, model, thinking, effort string
		budget                                int
		check                                 func(*testing.T, map[string]any)
	}{
		{name: "openai-none", format: "openai", model: "gpt-5", effort: "none", check: func(t *testing.T, b map[string]any) {
			if b["reasoning_effort"] != "none" || b["thinking"] != nil {
				t.Fatalf("wrong effort: %v", b)
			}
		}},
		{name: "anthropic-budget", format: "anthropic", model: "claude-sonnet-4-20250514", thinking: "enabled", budget: 4096, check: func(t *testing.T, b map[string]any) {
			v, _ := b["thinking"].(map[string]any)
			if v["type"] != "enabled" || v["budget_tokens"] != float64(4096) {
				t.Fatalf("wrong budget: %v", b)
			}
		}},
		{name: "anthropic-adaptive", format: "anthropic", model: "claude-sonnet-4-6", thinking: "adaptive", effort: "high", check: func(t *testing.T, b map[string]any) {
			v, _ := b["thinking"].(map[string]any)
			out, _ := b["output_config"].(map[string]any)
			if v["type"] != "adaptive" || out["effort"] != "high" || v["budget_tokens"] != nil {
				t.Fatalf("wrong adaptive config: %v", b)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			received := make(chan map[string]any, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				received <- body
				w.Header().Set("Content-Type", "application/json")
				if tc.format == "anthropic" {
					_, _ = w.Write([]byte(`{"id":"msg_test","type":"message","role":"assistant","model":"fixture","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
				} else {
					_, _ = w.Write([]byte(`{"id":"test","choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
				}
			}))
			defer srv.Close()
			cfg := Config{Format: tc.format, BaseURL: srv.URL, APIKey: "fixture", Model: tc.model, ThinkingType: tc.thinking, ThinkingBudget: tc.budget, ReasoningEffort: tc.effort}
			mdl, err := cfg.NewModel()
			if err != nil {
				t.Fatal(err)
			}
			req := &llm.Request{Messages: []llm.Message{{Role: llm.RoleUser, Content: "fixture"}}, GenerationConfig: llm.GenerationConfig{MaxTokens: llm.IntPtr(8192)}}
			events, err := mdl.GenerateContent(t.Context(), req)
			if err != nil {
				t.Fatal(err)
			}
			for e := range events {
				if e.Error != nil {
					t.Fatal(e.Error)
				}
			}
			select {
			case body := <-received:
				tc.check(t, body)
			default:
				t.Fatal("no native request")
			}
			if req.ThinkingEnabled != nil || req.ReasoningEffort != nil {
				t.Fatal("shared request was mutated")
			}
		})
	}
}
