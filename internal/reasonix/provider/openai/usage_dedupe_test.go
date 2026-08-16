package openai

// 回归测试:SiliconFlow 等 OpenAI 兼容网关在每个 SSE chunk 都附 usage 累计值
// (非标准;OpenAI/DeepSeek 官方只在流末尾发一次)。修复前 executor 按 chunk
// 记账导致成本与 turn 计数虚高百倍。断言:消费端只收到 1 个 ChunkUsage,
// 且为流末累计值。

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"scopeforge/internal/reasonix/provider"
)

func TestStreamDedupesPerChunkUsage(t *testing.T) {
	// 模拟 SiliconFlow:每个 chunk 都带 usage,completion_tokens 累计
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for i := 1; i <= 3; i++ {
			fmt.Fprintf(w, "data: %s\n\n", mkChunk(i, 100+i))
			fl.Flush()
		}
		// 末尾 content + 最终 usage
		fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":3,"total_tokens":103}}`)
		fl.Flush()
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer srv.Close()

	c, err := New(provider.Config{Name: "sf-test", BaseURL: srv.URL, Model: "deepseek-ai/DeepSeek-V4-Flash"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ch, err := c.Stream(context.Background(), provider.Request{
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	var usageCount int
	var lastUsage *provider.Usage
	var text string
	done := false
	for chunk := range ch {
		switch chunk.Type {
		case provider.ChunkUsage:
			usageCount++
			lastUsage = chunk.Usage
		case provider.ChunkText:
			text += chunk.Text
		case provider.ChunkError:
			t.Fatalf("stream error: %v", chunk.Err)
		case provider.ChunkDone:
			done = true
		}
	}
	if !done {
		t.Fatal("stream 未正常结束([DONE] 缺失)")
	}
	if usageCount != 1 {
		t.Fatalf("usage 事件 = %d,期望 1(逐 chunk 附 usage 的网关只记一次)", usageCount)
	}
	if lastUsage == nil || lastUsage.PromptTokens != 100 || lastUsage.CompletionTokens != 3 {
		t.Fatalf("usage = %+v,期望流末累计值 prompt=100 completion=3", lastUsage)
	}
	if text != "hello" {
		t.Fatalf("text = %q,期望 hello", text)
	}
}

// mkChunk 构造带 reasoning_content + usage 的 SSE chunk(completion 累计)。
func mkChunk(completion, total int) string {
	return fmt.Sprintf(`{"choices":[{"index":0,"delta":{"content":null,"reasoning_content":"r%d"},"finish_reason":null}],"usage":{"prompt_tokens":100,"completion_tokens":%d,"total_tokens":%d}}`,
		completion, completion, total)
}

// TestStreamNoUsageWhenInterrupted 中断的流不记账(缓存 usage 不发送)。
func TestStreamNoUsageWhenInterrupted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		fmt.Fprintf(w, "data: %s\n\n", mkChunk(1, 101))
		fl.Flush()
		// 中途断开:无 [DONE]、无 finish_reason
		conn, _, _ := w.(http.Hijacker).Hijack()
		conn.Close()
	}))
	defer srv.Close()

	c, err := New(provider.Config{Name: "sf-test2", BaseURL: srv.URL, Model: "m"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ch, err := c.Stream(context.Background(), provider.Request{
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var usageCount int
	var errChunk error
	for chunk := range ch {
		switch chunk.Type {
		case provider.ChunkUsage:
			usageCount++
		case provider.ChunkError:
			errChunk = chunk.Err
		}
	}
	if errChunk == nil {
		t.Fatal("中断流应报错(不重连:已发出 reasoning token)")
	}
	if usageCount != 0 {
		t.Fatalf("中断流 usage 事件 = %d,期望 0(不记账)", usageCount)
	}
	_ = strings.TrimSpace
}
