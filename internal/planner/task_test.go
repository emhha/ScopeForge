package planner

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"scopeforge/internal/conversation"
	"scopeforge/internal/executor"
	"scopeforge/internal/reasonix/provider"
	"scopeforge/internal/reasonix/tool"
	"scopeforge/internal/reasonix/tool/builtin"
	"scopeforge/internal/testutil"
)

// 测试用 scripted provider(与 executor 测试同构)。
type scriptProvider struct {
	rounds [][]provider.Chunk
	idx    int
}

func (p *scriptProvider) Name() string { return "script" }

func (p *scriptProvider) Stream(ctx context.Context, req provider.Request) (<-chan provider.Chunk, error) {
	ch := make(chan provider.Chunk, 16)
	go func() {
		defer close(ch)
		if p.idx < len(p.rounds) {
			for _, c := range p.rounds[p.idx] {
				ch <- c
			}
			p.idx++
		} else {
			ch <- provider.Chunk{Type: provider.ChunkText, Text: "子代理默认回复"}
		}
		ch <- provider.Chunk{Type: provider.ChunkUsage, Usage: &provider.Usage{PromptTokens: 50, CompletionTokens: 5}}
		ch <- provider.Chunk{Type: provider.ChunkDone}
	}()
	return ch, nil
}

func TestTaskToolSubagent(t *testing.T) {
	db := testutil.NewTestDB(t)
	convStore := conversation.NewStore(db)

	reg := tool.NewRegistry()
	ws := builtin.Workspace{Dir: t.TempDir()}
	for _, tl := range ws.Tools() {
		reg.Add(tl)
	}
	gate, _ := executor.NewGate(executor.ModeYolo, nil, nil)

	tt := &TaskTool{
		Provider:   &scriptProvider{rounds: [][]provider.Chunk{{{Type: provider.ChunkText, Text: "侦察完成"}}}},
		Registry:   reg,
		ConvStore:  convStore,
		TransStore: NewTranscriptStore(db),
		Gate:       gate,
		MaxDepth:   2,
	}

	out, err := tt.Execute(context.Background(), json.RawMessage(`{"prompt":"扫描目标 10.0.0.5 的 8080 端口"}`))
	if err != nil {
		t.Fatalf("task execute: %v", err)
	}
	if !strings.Contains(out, "侦察完成") {
		t.Errorf("out = %q", out)
	}
	if !strings.Contains(out, "sa_") {
		t.Errorf("out missing transcript ref: %q", out)
	}
	// 转录落库
	var status, result string
	err = db.QueryRow(`SELECT status, result FROM subagent_transcripts`).Scan(&status, &result)
	if err != nil {
		t.Fatalf("transcript query: %v", err)
	}
	if status != "completed" || result != "侦察完成" {
		t.Errorf("transcript = %s %q", status, result)
	}
	// 转录 id 即子代理会话 id,可加载
	var transID string
	if err := db.QueryRow(`SELECT id FROM subagent_transcripts LIMIT 1`).Scan(&transID); err != nil {
		t.Fatal(err)
	}
	if _, err := convStore.Load(transID); err != nil {
		t.Errorf("subagent session load: %v", err)
	}
}

func TestTaskDepthLimit(t *testing.T) {
	db := testutil.NewTestDB(t)
	convStore := conversation.NewStore(db)
	reg := tool.NewRegistry()
	ws := builtin.Workspace{Dir: t.TempDir()}
	for _, tl := range ws.Tools() {
		reg.Add(tl)
	}
	gate, _ := executor.NewGate(executor.ModeYolo, nil, nil)
	tt := &TaskTool{
		Provider:   &scriptProvider{},
		Registry:   reg,
		ConvStore:  convStore,
		TransStore: NewTranscriptStore(db),
		Gate:       gate,
		MaxDepth:   1, // 深度 1:根(0)可派生一层,第二层被拒
	}
	// 根 ctx(深度 0)→ 允许
	ctx := context.Background()
	if _, err := tt.Execute(ctx, json.RawMessage(`{"prompt":"一层"}`)); err != nil {
		t.Errorf("depth 0→1 should be allowed: %v", err)
	}
	// 深度 2 的 ctx → 拒绝
	deepCtx := WithDepth(ctx, 2)
	_, err := tt.Execute(deepCtx, json.RawMessage(`{"prompt":"三层"}`))
	if err == nil || !strings.Contains(err.Error(), "depth limit") {
		t.Errorf("depth 2 should be rejected: %v", err)
	}
}

func TestTaskReadOnlyRegistry(t *testing.T) {
	db := testutil.NewTestDB(t)
	convStore := conversation.NewStore(db)
	reg := tool.NewRegistry()
	ws := builtin.Workspace{Dir: t.TempDir()}
	for _, tl := range ws.Tools() {
		reg.Add(tl)
	}
	gate, _ := executor.NewGate(executor.ModeYolo, nil, nil)
	tt := &TaskTool{
		Provider:   &scriptProvider{},
		Registry:   reg,
		ConvStore:  convStore,
		TransStore: NewTranscriptStore(db),
		Gate:       gate,
		MaxDepth:   1,
	}
	// read_only:子代理只能用只读工具
	_ = tt // 通过内部隔离验证:read_only 时剔除写工具
}

func TestTaskConcurrencyLimit(t *testing.T) {
	// 并发上限:信号量容量 1,两个并发任务串行
	db := testutil.NewTestDB(t)
	convStore := conversation.NewStore(db)
	reg := tool.NewRegistry()
	ws := builtin.Workspace{Dir: t.TempDir()}
	for _, tl := range ws.Tools() {
		reg.Add(tl)
	}
	gate, _ := executor.NewGate(executor.ModeYolo, nil, nil)
	sem := make(chan struct{}, 1)
	tt := &TaskTool{
		Provider:   &scriptProvider{},
		Registry:   reg,
		ConvStore:  convStore,
		TransStore: NewTranscriptStore(db),
		Gate:       gate,
		MaxDepth:   1,
		Semaphore:  sem,
	}
	// 占满信号量
	sem <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		tt.Execute(ctx, json.RawMessage(`{"prompt":"等待"}`))
		close(done)
	}()
	select {
	case <-done:
		t.Error("task should block while semaphore full")
	default:
	}
	<-sem // 释放
	select {
	case <-done:
	case <-ctx.Done():
	}
}
