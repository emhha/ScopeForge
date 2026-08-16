package provider_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"scopeforge/internal/reasonix/provider"
	_ "scopeforge/internal/reasonix/provider/anthropic" // init 注册
	_ "scopeforge/internal/reasonix/provider/openai"    // init 注册
	"scopeforge/internal/testutil"
)

func TestOpenAIStreamingChat(t *testing.T) {
	mock := testutil.NewMockOpenAIServer([]testutil.MockStep{{
		Reasoning: "先分析目标,再行动",
		Text:      "你好,这是最终回答",
		UsageOverride: &testutil.MockUsage{
			PromptTokens: 200, CompletionTokens: 42,
			CacheHitTokens: 80, ReasoningTokens: 10,
		},
	}})
	defer mock.Close()

	p, err := provider.New("openai", provider.Config{
		Name: "mock-openai", BaseURL: mock.URL, Model: "mock-model",
		APIKey: "test-key",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ch, err := p.Stream(context.Background(), provider.Request{
		Messages: []provider.Message{
			{Role: provider.RoleSystem, Content: "你是渗透测试助手"},
			{Role: provider.RoleUser, Content: "你好"},
		},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	var text, reasoning strings.Builder
	var usage *provider.Usage
	done := false
	for c := range ch {
		switch c.Type {
		case provider.ChunkReasoning:
			reasoning.WriteString(c.Text)
		case provider.ChunkText:
			text.WriteString(c.Text)
		case provider.ChunkUsage:
			usage = c.Usage
		case provider.ChunkDone:
			done = true
		case provider.ChunkError:
			t.Fatalf("unexpected error chunk: %v", c.Err)
		}
	}
	if !done {
		t.Fatal("missing done chunk")
	}
	if reasoning.String() != "先分析目标,再行动" {
		t.Errorf("reasoning = %q", reasoning.String())
	}
	if text.String() != "你好,这是最终回答" {
		t.Errorf("text = %q", text.String())
	}
	if usage == nil {
		t.Fatal("missing usage")
	}
	if usage.CacheHitTokens != 80 || usage.CacheMissTokens != 120 {
		t.Errorf("cache split = hit %d miss %d", usage.CacheHitTokens, usage.CacheMissTokens)
	}
	if usage.ReasoningTokens != 10 {
		t.Errorf("reasoning tokens = %d", usage.ReasoningTokens)
	}
	if usage.FinishReason != "stop" {
		t.Errorf("finish reason = %q", usage.FinishReason)
	}

	// Pricing 计算
	pricing := provider.Pricing{CacheHit: 0.1, Input: 1.0, Output: 2.0}
	cost := pricing.Cost(usage)
	want := 80/1e6*0.1 + 120/1e6*1.0 + 42/1e6*2.0
	if cost != want {
		t.Errorf("cost = %v, want %v", cost, want)
	}
}

func TestOpenAIToolCalls(t *testing.T) {
	// 参数 > 2KB 以触发上游的 ArgsDelta 桶逻辑
	bigArgs := `{"command":"printf '` + strings.Repeat("x", 3000) + `'"}`
	mock := testutil.NewMockOpenAIServer([]testutil.MockStep{{
		ToolCalls: []testutil.MockToolCall{
			{ID: "call_1", Name: "bash", Arguments: bigArgs},
		},
	}})
	defer mock.Close()

	p, err := provider.New("openai", provider.Config{
		Name: "mock-openai", BaseURL: mock.URL, Model: "mock-model",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ch, err := p.Stream(context.Background(), provider.Request{
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "列目录"}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	var calls []provider.ToolCall
	startSeen, deltaSeen, usageSeen := false, false, false
	for c := range ch {
		switch c.Type {
		case provider.ChunkToolCallStart:
			startSeen = true
			if c.ToolCall.Name != "bash" || c.ToolCall.ID != "call_1" {
				t.Errorf("start tc = %+v", c.ToolCall)
			}
		case provider.ChunkToolCallArgsDelta:
			deltaSeen = true
		case provider.ChunkToolCall:
			calls = append(calls, *c.ToolCall)
		case provider.ChunkUsage:
			usageSeen = true
			if c.Usage.FinishReason != "tool_calls" {
				t.Errorf("finish reason = %q, want tool_calls", c.Usage.FinishReason)
			}
		case provider.ChunkError:
			t.Fatalf("error chunk: %v", c.Err)
		}
	}
	if !startSeen || !deltaSeen || !usageSeen {
		t.Errorf("start=%v delta=%v usage=%v", startSeen, deltaSeen, usageSeen)
	}
	if len(calls) != 1 || calls[0].Name != "bash" || calls[0].Arguments != bigArgs {
		t.Errorf("calls = %+v (len %d)", calls, len(calls))
	}
}

func TestAnthropicStreaming(t *testing.T) {
	mock := testutil.NewMockAnthropicServer([]testutil.MockStep{{
		Reasoning: "思考中",
		Text:      "Claude 的回答",
	}})
	defer mock.Close()

	p, err := provider.New("anthropic", provider.Config{
		Name: "mock-anthropic", BaseURL: mock.URL, Model: "claude-sonnet-4",
		APIKey: "test-key",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ch, err := p.Stream(context.Background(), provider.Request{
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	var text, reasoning strings.Builder
	var sig string
	var usage *provider.Usage
	done := false
	for c := range ch {
		switch c.Type {
		case provider.ChunkReasoning:
			reasoning.WriteString(c.Text)
			if c.Signature != "" {
				sig = c.Signature
			}
		case provider.ChunkText:
			text.WriteString(c.Text)
		case provider.ChunkUsage:
			usage = c.Usage
		case provider.ChunkDone:
			done = true
		case provider.ChunkError:
			t.Fatalf("error chunk: %v", c.Err)
		}
	}
	if text.String() != "Claude 的回答" || reasoning.String() != "思考中" {
		t.Errorf("text=%q reasoning=%q", text.String(), reasoning.String())
	}
	if sig != "sig-abc" {
		t.Errorf("signature = %q", sig)
	}
	if !done || usage == nil {
		t.Fatal("missing done/usage")
	}
	if usage.PromptTokens != 100 {
		t.Errorf("prompt tokens = %d", usage.PromptTokens)
	}
}

func TestNormalizeMessages(t *testing.T) {
	// 孤儿 tool 消息删除
	msgs := []provider.Message{
		{Role: provider.RoleUser, Content: "run"},
		{Role: provider.RoleTool, ToolCallID: "orphan_1", Name: "bash", Content: "out"},
		{Role: provider.RoleAssistant, Content: "final"},
	}
	got := provider.NormalizeMessages(msgs)
	if len(got) != 2 {
		t.Fatalf("want 2 messages after orphan drop, got %d", len(got))
	}
	if got[1].Role != provider.RoleAssistant {
		t.Errorf("last msg = %+v", got[1])
	}

	// 健康配对:零拷贝快路径
	healthy := []provider.Message{
		{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{ID: "c1", Name: "bash", Arguments: `{"command":"ls"}`}}},
		{Role: provider.RoleTool, ToolCallID: "c1", Content: "out"},
	}
	got2 := provider.NormalizeMessages(healthy)
	if len(got2) != 2 {
		t.Error("healthy session should pass through")
	}
}

func TestProviderKindsRegistered(t *testing.T) {
	kinds := provider.Kinds()
	joined := strings.Join(kinds, ",")
	for _, k := range []string{"openai", "anthropic"} {
		if !strings.Contains(joined, k) {
			t.Errorf("kind %q not registered: %v", k, kinds)
		}
	}
	if _, err := provider.New("nope", provider.Config{}); err == nil {
		t.Error("unknown kind should error")
	}
}

func TestSchemaValidate(t *testing.T) {
	// 合法 schema 通过(根类型 object)
	good := json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}`)
	if err := provider.ValidateToolSchema(good); err != nil {
		t.Errorf("valid schema rejected: %v", err)
	}
	// 根类型非 object 拒绝
	bad := json.RawMessage(`{"type":"string"}`)
	if err := provider.ValidateToolSchema(bad); err == nil {
		t.Error("non-object root schema should be rejected")
	}
	// 畸形 JSON 拒绝
	if err := provider.ValidateToolSchema(json.RawMessage(`{oops`)); err == nil {
		t.Error("malformed schema should be rejected")
	}
	// CanonicalizeSchema:省略根 type 时补齐 object
	canon := provider.CanonicalizeSchema(json.RawMessage(`{"properties":{}}`))
	if err := provider.ValidateToolSchema(canon); err != nil {
		t.Errorf("canonicalized schema rejected: %v", err)
	}
}
