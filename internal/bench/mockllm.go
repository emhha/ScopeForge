package bench

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
)

// scriptedLLM 是评测用 mock LLM(离线闭环,02 §3.2"mock SRC 靶场"配套)。
// 行为按 worker 角色路由(依据 systemPrompt 角色标记,与 scheduler_test 同规):
//   - scout:首轮调 bash 侦察,次轮输出 attack_surface(从预埋靶场端点生成)
//   - executor:每方向先调 submit_vulnerability 写入账本(预埋种子队列依次消费),
//     次轮输出确认契约;种子队列耗尽后输出 exhausted(方向耗尽 → 格子 dead)
//   - synthesizer:输出穷尽声明(带 coverage_evidence,02 §2.4 S6)
//   - analyst:默认假设契约
//
// 注意:mock 只是"把预埋漏洞交出来",验证的是机制(回执推进/矩阵/指标);
// 真实 recall ≥ 60% 验收需真实 LLM(runner 的 LLMBase 注入)。
type scriptedLLM struct {
	*httptest.Server

	target   *SRCTarget
	queue    []Seed // 待确认种子(executor 提交轮依次消费)
	lastSeed Seed   // 最近消费的种子(executor 确认轮契约用)

	mu       sync.Mutex
	requests []string // 请求消息尾部(断言/调试)
}

// newScriptedLLM 启动 mock LLM 服务器。
func newScriptedLLM(t *SRCTarget) *scriptedLLM {
	m := &scriptedLLM{target: t}
	m.queue = append(m.queue, t.Seeds...)

	mux := http.NewServeMux()
	handler := func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(strings.NewReader(string(bodyBytes)))
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(bodyBytes, &req)

		m.mu.Lock()
		m.requests = append(m.requests, string(bodyBytes))
		m.mu.Unlock()

		sys := ""
		lastRole := ""
		for i, msg := range req.Messages {
			if i == 0 {
				sys = msg.Content
			}
			if msg.Role == "user" || msg.Role == "tool" || msg.Role == "assistant" {
				lastRole = msg.Role
			}
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl := w.(http.Flusher)
		write := func(v any) {
			data, _ := json.Marshal(v)
			fmt.Fprintf(w, "data: %s\n\n", data)
			fl.Flush()
		}
		usage := map[string]any{"prompt_tokens": 100, "completion_tokens": 20, "total_tokens": 120}

		switch {
		case strings.Contains(sys, "Scout。"):
			// scout:首轮侦察工具,次轮输出攻击面清单(03 §2.2)
			if lastRole == "tool" {
				write(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": m.scoutContract()}}}})
			} else {
				writeToolCallSSE(w, fl, "bash", `{"command":"curl -s http://127.0.0.1/ | head -5"}`, usage)
			}

		case strings.Contains(sys, "Executor。"):
			// executor:先提交种子漏洞到账本(消费队列),工具结果回填后输出确认契约
			if lastRole == "tool" {
				// 确认轮:种子已在提交轮消费,用 lastSeed 输出确认契约
				m.mu.Lock()
				s := m.lastSeed
				m.mu.Unlock()
				write(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": m.confirmContract(s)}}}})
			} else {
				// 提交轮:消费下一个种子,调 submit_vulnerability 写账本(02 §1)
				seed, ok := m.nextSeed()
				if !ok {
					// 种子队列耗尽:方向耗尽,格子 dead(03 §3.1)
					write(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": `{"accepted":true,"findings":[],"new_intents":[],"dead_ends":[],"stop_reason":"exhausted"}`}}}})
					write(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": "stop"}}, "usage": usage})
					fmt.Fprint(w, "data: [DONE]\n\n")
					fl.Flush()
					return
				}
				m.mu.Lock()
				m.lastSeed = seed
				m.mu.Unlock()
				args, _ := json.Marshal(map[string]string{
					"cwe": seed.CWE, "asset": seed.Asset, "endpoint": seed.Endpoint,
					"severity": seed.Severity, "title": seed.Title,
				})
				writeToolCallSSE(w, fl, "submit_vulnerability", string(args), usage)
				return
			}

		case strings.Contains(sys, "Synthesizer。"):
			// synthesizer:穷尽声明带覆盖证据(02 §2.4 S6)
			write(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": `{"accepted":true,"exhausted":true,"coverage_evidence":[{"direction":"mock-src","tried":["all"],"excluded":"方向耗尽"}],"findings":[{"prefix":"obs:","text":"最终确认:评测任务完成","weight":0.9}],"new_intents":[],"dead_ends":[],"stop_reason":"conclude"}`}}}})

		default: // analyst
			write(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": `{"accepted":true,"findings":[{"prefix":"hyp:","text":"疑似存在漏洞,待验证","weight":0.7}],"new_intents":[{"text":"验证候选方向","weight":0.8}],"dead_ends":[],"stop_reason":"intent_done"}`}}}})
		}

		write(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": "stop"}}, "usage": usage})
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	}
	mux.HandleFunc("/v1/chat/completions", handler)
	mux.HandleFunc("/chat/completions", handler)
	m.Server = httptest.NewServer(mux)
	return m
}

// scoutContract 生成攻击面清单契约(03 §2.2):预埋靶场的去重 (asset, endpoint)。
func (m *scriptedLLM) scoutContract() string {
	seen := map[string]bool{}
	var items []map[string]any
	for _, s := range m.target.Seeds {
		k := s.Asset + s.Endpoint
		if seen[k] {
			continue
		}
		seen[k] = true
		params := []string{"q"}
		if s.Endpoint == "/api/order" {
			params = []string{"id"}
		}
		items = append(items, map[string]any{
			"asset": s.Asset, "endpoint": s.Endpoint, "params": params,
			"auth": "form", "tech": "express", "notes": "mock 靶场端点",
		})
	}
	contract := map[string]any{
		"accepted": true, "findings": []any{}, "new_intents": []any{}, "dead_ends": []any{},
		"attack_surface": items, "stop_reason": "intent_done",
	}
	data, _ := json.Marshal(contract)
	return string(data)
}

// confirmContract 生成确认契约(executor 种子命中后)。
func (m *scriptedLLM) confirmContract(s Seed) string {
	contract := map[string]any{
		"accepted": true,
		"findings": []any{map[string]any{
			"prefix": "obs:", "text": "已确认:" + s.Title, "weight": 0.9, "evidence_ref": "exec-e1",
		}},
		"new_intents": []any{}, "dead_ends": []any{}, "stop_reason": "intent_done",
	}
	data, _ := json.Marshal(contract)
	return string(data)
}

// nextSeed 消费下一个待确认种子(线程安全)。
func (m *scriptedLLM) nextSeed() (Seed, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.queue) == 0 {
		return Seed{}, false
	}
	s := m.queue[0]
	m.queue = m.queue[1:]
	return s, true
}

// writeToolCallSSE 输出一次工具调用 SSE 帧(与 scheduler_test 同规)。
func writeToolCallSSE(w http.ResponseWriter, fl http.Flusher, name, args string, usage map[string]any) {
	write := func(v any) {
		data, _ := json.Marshal(v)
		fmt.Fprintf(w, "data: %s\n\n", data)
		fl.Flush()
	}
	write(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{
		"tool_calls": []any{map[string]any{"index": 0, "id": "call_1", "type": "function", "function": map[string]any{"name": name, "arguments": args}}},
	}}}})
	write(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": "tool_calls"}}, "usage": usage})
	fmt.Fprint(w, "data: [DONE]\n\n")
	fl.Flush()
}
