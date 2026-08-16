// Package testutil 提供测试辅助:mock LLM 服务器(OpenAI 兼容 / Anthropic)。
package testutil

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
)

// MockStep 描述 mock 服务器的一次补全行为。
type MockStep struct {
	Reasoning    string
	Text         string
	ToolCalls    []MockToolCall
	FinishReason string
	UsageOverride *MockUsage
}

// MockToolCall 是一次工具调用。
type MockToolCall struct {
	ID        string
	Name      string
	Arguments string
}

// MockUsage 是 mock usage。
type MockUsage struct {
	PromptTokens     int
	CompletionTokens int
	CacheHitTokens   int
	ReasoningTokens  int
}

// MockOpenAIServer 是 OpenAI 兼容 mock 服务器。
type MockOpenAIServer struct {
	*httptest.Server
	Steps       []MockStep
	CallCount   atomic.Int64
	LastRequest []byte
}

// NewMockOpenAIServer 创建 mock 服务器。
func NewMockOpenAIServer(steps []MockStep) *MockOpenAIServer {
	m := &MockOpenAIServer{Steps: steps}
	handler := func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, 0)
		buf := make([]byte, 4096)
		for {
			n, err := r.Body.Read(buf)
			body = append(body, buf[:n]...)
			if err != nil {
				break
			}
		}
		m.LastRequest = body

		idx := int(m.CallCount.Add(1) - 1)
		step := m.Steps[min(idx, len(m.Steps)-1)]
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl := w.(http.Flusher)

		usage := MockUsage{PromptTokens: 120, CompletionTokens: 30}
		if step.UsageOverride != nil {
			usage = *step.UsageOverride
		}

		writeEvent := func(obj any) {
			data, _ := json.Marshal(obj)
			fmt.Fprintf(w, "data: %s\n\n", data)
			fl.Flush()
		}

		if step.Reasoning != "" {
			half := len(step.Reasoning) / 2
			for _, part := range []string{step.Reasoning[:half], step.Reasoning[half:]} {
				writeEvent(map[string]any{
					"choices": []any{map[string]any{
						"delta": map[string]any{"reasoning_content": part},
					}},
				})
			}
		}
		if len(step.ToolCalls) > 0 {
			for i, tc := range step.ToolCalls {
				writeEvent(map[string]any{
					"choices": []any{map[string]any{
						"delta": map[string]any{
							"tool_calls": []any{map[string]any{
								"index": i, "id": tc.ID,
								"type": "function",
								"function": map[string]any{"name": tc.Name},
							}},
						},
					}},
				})
				args := tc.Arguments
				half := len(args) / 2
				for _, part := range []string{args[:half], args[half:]} {
					writeEvent(map[string]any{
						"choices": []any{map[string]any{
							"delta": map[string]any{
								"tool_calls": []any{map[string]any{
									"index": i,
									"function": map[string]any{"arguments": part},
								}},
							},
						}},
					})
				}
			}
			finish := step.FinishReason
			if finish == "" {
				finish = "tool_calls"
			}
			writeEvent(map[string]any{
				"choices": []any{map[string]any{
					"delta":         map[string]any{},
					"finish_reason": finish,
				}},
				"usage": map[string]any{
					"prompt_tokens":              usage.PromptTokens,
					"completion_tokens":          usage.CompletionTokens,
					"total_tokens":               usage.PromptTokens + usage.CompletionTokens,
					"prompt_cache_hit_tokens":    usage.CacheHitTokens,
					"prompt_cache_miss_tokens":   usage.PromptTokens - usage.CacheHitTokens,
					"completion_tokens_details":  map[string]any{"reasoning_tokens": usage.ReasoningTokens},
				},
			})
		} else if step.Text != "" {
			writeEvent(map[string]any{
				"choices": []any{map[string]any{
					"delta": map[string]any{"content": step.Text},
				}},
			})
			writeEvent(map[string]any{
				"choices": []any{map[string]any{
					"delta":         map[string]any{},
					"finish_reason": "stop",
				}},
				"usage": map[string]any{
					"prompt_tokens":             usage.PromptTokens,
					"completion_tokens":         usage.CompletionTokens,
					"total_tokens":              usage.PromptTokens + usage.CompletionTokens,
					"prompt_cache_hit_tokens":   usage.CacheHitTokens,
					"prompt_cache_miss_tokens":  usage.PromptTokens - usage.CacheHitTokens,
					"completion_tokens_details": map[string]any{"reasoning_tokens": usage.ReasoningTokens},
				},
			})
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", handler)
	mux.HandleFunc("/chat/completions", handler)
	m.Server = httptest.NewServer(mux)
	return m
}

// MockAnthropicServer 是 Anthropic Messages API mock 服务器。
type MockAnthropicServer struct {
	*httptest.Server
	Steps     []MockStep
	CallCount atomic.Int64
}

// NewMockAnthropicServer 创建 mock 服务器。
func NewMockAnthropicServer(steps []MockStep) *MockAnthropicServer {
	m := &MockAnthropicServer{Steps: steps}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		idx := int(m.CallCount.Add(1) - 1)
		step := m.Steps[min(idx, len(m.Steps)-1)]
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl := w.(http.Flusher)

		writeEvent := func(obj any) {
			data, _ := json.Marshal(obj)
			fmt.Fprintf(w, "data: %s\n\n", data)
			fl.Flush()
		}

		writeEvent(map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"usage": map[string]any{"input_tokens": 100, "output_tokens": 1},
			},
		})
		if step.Reasoning != "" {
			writeEvent(map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "thinking"}})
			writeEvent(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "thinking_delta", "thinking": step.Reasoning}})
			writeEvent(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "signature_delta", "signature": "sig-abc"}})
			writeEvent(map[string]any{"type": "content_block_stop", "index": 0})
		}
		if len(step.ToolCalls) > 0 {
			for i, tc := range step.ToolCalls {
				writeEvent(map[string]any{"type": "content_block_start", "index": i, "content_block": map[string]any{
					"type": "tool_use", "id": tc.ID, "name": tc.Name,
				}})
				args := tc.Arguments
				half := len(args) / 2
				for _, part := range []string{args[:half], args[half:]} {
					writeEvent(map[string]any{"type": "content_block_delta", "index": i, "delta": map[string]any{"type": "input_json_delta", "partial_json": part}})
				}
				writeEvent(map[string]any{"type": "content_block_stop", "index": i})
			}
		} else if step.Text != "" {
			writeEvent(map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text"}})
			writeEvent(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": step.Text}})
			writeEvent(map[string]any{"type": "content_block_stop", "index": 0})
		}
		stop := step.FinishReason
		if stop == "" {
			if len(step.ToolCalls) > 0 {
				stop = "tool_use"
			} else {
				stop = "end_turn"
			}
		}
		writeEvent(map[string]any{
			"type":  "message_delta",
			"delta": map[string]any{"stop_reason": stop},
			"usage": map[string]any{"output_tokens": 30},
		})
	})
	m.Server = httptest.NewServer(mux)
	return m
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// StripSSE 把 SSE 流拆成 data 行(供测试断言)。
func StripSSE(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(line, "data:") {
			out = append(out, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	return out
}
