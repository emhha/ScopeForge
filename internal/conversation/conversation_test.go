package conversation

import (
	"context"
	"errors"
	"strings"
	"testing"

	"scopeforge/internal/reasonix/provider"
	"scopeforge/internal/testutil"
)

// fakeCompactor 是测试用摘要器。
type fakeCompactor struct {
	summary string
	err     error
}

func (f *fakeCompactor) Summarize(ctx context.Context, msgs []provider.Message) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	if f.summary != "" {
		return f.summary, nil
	}
	// 默认:把 7 标题全带上
	var b strings.Builder
	b.WriteString(SummaryTagOpen + "\n")
	for _, h := range []string{"standing facts", "Goal", "Decisions", "Files", "Commands", "Errors", "Pending"} {
		b.WriteString("## " + h + "\n- (none)\n")
	}
	b.WriteString(SummaryTagClose)
	return b.String(), nil
}

func TestSessionBasics(t *testing.T) {
	s := New("s1", KindMain)
	s.Add(provider.Message{Role: provider.RoleUser, Content: "hi"})
	s.Add(provider.Message{Role: provider.RoleAssistant, Content: "hello"})
	if len(s.Messages) != 2 {
		t.Fatalf("messages = %d", len(s.Messages))
	}
	// Replace
	if err := s.Replace(1, provider.Message{Role: provider.RoleAssistant, Content: "hey"}); err != nil {
		t.Fatal(err)
	}
	if s.Messages[1].Content != "hey" {
		t.Errorf("replace failed: %+v", s.Messages[1])
	}
	// 越界 Replace
	if err := s.Replace(5, provider.Message{}); err == nil {
		t.Error("out-of-range replace should error")
	}
	// Rewrite + 版本
	s.Rewrite([]provider.Message{{Role: provider.RoleSystem, Content: "sys"}})
	if s.RewriteVersion != 1 || len(s.Messages) != 1 {
		t.Errorf("rewrite: version=%d len=%d", s.RewriteVersion, len(s.Messages))
	}
	// CloneWithMessages
	clone := s.CloneWithMessages([]provider.Message{{Role: provider.RoleUser, Content: "x"}})
	if clone.ID != s.ID || clone.RewriteVersion != 1 {
		t.Errorf("clone: %+v", clone)
	}
	// Digest 稳定且随内容变化
	d1, _ := s.Digest()
	d2, _ := s.Digest()
	if d1 != d2 {
		t.Error("digest should be stable")
	}
	s.Add(provider.Message{Role: provider.RoleUser, Content: "more"})
	d3, _ := s.Digest()
	if d1 == d3 {
		t.Error("digest should change with content")
	}
}

func TestStoreSaveLoadAndDigestConflict(t *testing.T) {
	db := testutil.NewTestDB(t)
	st := NewStore(db)

	s := New("sess-1", KindMain)
	s.Provider = "openai"
	s.Model = "mock"
	s.Add(provider.Message{Role: provider.RoleSystem, Content: "sys"})
	s.Add(provider.Message{Role: provider.RoleUser, Content: "hello"})

	// 首次保存(无 baseDigest,直接 upsert)
	if err := st.SaveSnapshot(s); err != nil {
		t.Fatalf("save: %v", err)
	}

	// 加载
	loaded, err := st.Load("sess-1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(loaded.Messages) != 2 || loaded.Messages[1].Content != "hello" {
		t.Errorf("loaded messages = %+v", loaded.Messages)
	}

	// digest 乐观锁:并发写冲突检测
	// 1. 两个并发读者 s1、s2 从同一基线读取
	s1, err := st.Load("sess-1")
	if err != nil {
		t.Fatal(err)
	}
	s2, err := st.Load("sess-1")
	if err != nil {
		t.Fatal(err)
	}
	// 2. s1 修改并保存 → 成功
	s1.Add(provider.Message{Role: provider.RoleAssistant, Content: "world"})
	if err := st.SaveSnapshot(s1); err != nil {
		t.Fatalf("save after load should succeed: %v", err)
	}
	// 3. s2 基于过期基线修改保存 → 冲突
	s2.Add(provider.Message{Role: provider.RoleAssistant, Content: "conflict"})
	if err := st.SaveSnapshot(s2); err == nil {
		t.Error("expected digest conflict on stale baseline")
	} else if !errors.Is(err, ErrDigestConflict) {
		t.Errorf("expected ErrDigestConflict, got %v", err)
	}
	// 4. 同一 Session 连续保存不冲突(保存后基线刷新)
	s1.Add(provider.Message{Role: provider.RoleUser, Content: "more"})
	if err := st.SaveSnapshot(s1); err != nil {
		t.Errorf("repeated save should succeed: %v", err)
	}
}

func TestSaveRewriteAndRecoveryBranch(t *testing.T) {
	db := testutil.NewTestDB(t)
	st := NewStore(db)

	s := New("sess-2", KindMain)
	s.Add(provider.Message{Role: provider.RoleUser, Content: "task"})
	if err := st.SaveSnapshot(s); err != nil {
		t.Fatal(err)
	}
	// 压缩后 rewrite
	s.Rewrite([]provider.Message{{Role: provider.RoleUser, Content: "compacted"}})
	if err := st.SaveRewrite(s); err != nil {
		t.Fatal(err)
	}
	// 崩溃恢复分支
	if err := st.SaveRecoveryBranch(s, "kill -9 during compaction"); err != nil {
		t.Fatal(err)
	}
	// LoadLatest 应取 recovery 分支
	latest, err := st.LoadLatest("sess-2")
	if err != nil {
		t.Fatal(err)
	}
	if latest.Messages[0].Content != "compacted" {
		t.Errorf("latest = %+v", latest.Messages)
	}
	if latest.Metadata["recovery_cause"] != "kill -9 during compaction" {
		t.Errorf("recovery metadata = %+v", latest.Metadata)
	}
	// 直接 Load(主键冲突取任意行)也应可用
	if _, err := st.Load("sess-2"); err != nil {
		t.Fatalf("load by id: %v", err)
	}
	// List
	list, err := st.List(string(KindMain))
	if err != nil {
		t.Fatal(err)
	}
	if len(list) < 1 {
		t.Error("list empty")
	}
}

func TestCompactionLevels(t *testing.T) {
	cfg := DefaultCompactConfig()
	cases := []struct {
		ratio float64
		want  Level
	}{
		{0.4, LevelNone},
		{0.5, LevelSoft},
		{0.55, LevelSoft},
		{0.6, LevelSnip},
		{0.7, LevelSnip},
		{0.8, LevelCompact},
		{0.85, LevelCompact},
		{0.9, LevelForce},
		{0.95, LevelForce},
	}
	for _, c := range cases {
		if got := LevelFor(c.ratio, cfg); got != c.want {
			t.Errorf("ratio %.2f: got %v want %v", c.ratio, got, c.want)
		}
	}
}

func TestSnipStaleToolResults(t *testing.T) {
	big := strings.Repeat("x", 10000)
	msgs := []provider.Message{
		{Role: provider.RoleUser, Content: "do it"},
		{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{ID: "c1", Name: "bash", Arguments: `{"command":"ls"}`}}},
		{Role: provider.RoleTool, ToolCallID: "c1", Content: big},
		{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{ID: "c2", Name: "bash", Arguments: `{"command":"cat"}`}}},
		{Role: provider.RoleTool, ToolCallID: "c2", Content: big},
	}
	out := SnipStaleToolResults(msgs, 1)
	// 最近 1 个工具结果(c2,index 4)保留完整;更早的(c1,index 2)被截断
	if len(out[2].Content) >= len(big) {
		t.Errorf("stale tool result should be snipped, got len %d", len(out[2].Content))
	}
	if len(out[4].Content) != len(big) {
		t.Errorf("most recent should be kept full, got len %d", len(out[4].Content))
	}
}

func TestCompactSuccess(t *testing.T) {
	s := New("sess-3", KindMain)
	s.Add(provider.Message{Role: provider.RoleSystem, Content: "你是渗透测试助手"})
	for i := 0; i < 20; i++ {
		s.Add(provider.Message{Role: provider.RoleUser, Content: "step " + string(rune('a'+i))})
		s.Add(provider.Message{Role: provider.RoleAssistant, Content: "reply " + string(rune('a'+i))})
	}
	cfg := DefaultCompactConfig()
	cfg.Tail = 10 // 小 tail 强制压缩大部分消息

	result, err := Compact(context.Background(), &fakeCompactor{}, s, cfg)
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if result.Degraded {
		t.Fatalf("unexpected degrade: %s", result.ArchiveNote)
	}
	// 摘要插入
	var summaryFound bool
	for _, m := range s.Messages {
		if strings.Contains(m.Content, SummaryTagOpen) {
			summaryFound = true
		}
	}
	if !summaryFound {
		t.Error("summary message not inserted")
	}
	// 系统前缀保留
	if s.Messages[0].Role != provider.RoleSystem || s.Messages[0].Content != "你是渗透测试助手" {
		t.Errorf("system prefix changed: %+v", s.Messages[0])
	}
	// 版本号 +1
	if s.RewriteVersion != 1 {
		t.Errorf("rewrite version = %d", s.RewriteVersion)
	}
	// 归档消息存在
	if len(result.Compacted) == 0 {
		t.Error("compacted archive empty")
	}
}

func TestCompactDegradesOnMissingCriticalFacts(t *testing.T) {
	s := New("sess-4", KindMain)
	s.Add(provider.Message{Role: provider.RoleSystem, Content: "sys"})
	s.Add(provider.Message{Role: provider.RoleUser, Content: "请提交 flag 到 https://platform.example.com/submit,密钥 api_key=sk-abc123"})
	s.Add(provider.Message{Role: provider.RoleAssistant, Content: "好的"})
	cfg := DefaultCompactConfig()
	cfg.Tail = 10

	// 摘要不含 critical facts
	bad := &fakeCompactor{summary: SummaryTagOpen + "\n## Goal\n- x\n" + SummaryTagClose}
	result, err := Compact(context.Background(), bad, s, cfg)
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if !result.Degraded {
		t.Fatal("expected degrade on missing critical facts")
	}
	// 消息未被摘要替换,机械折叠保留结构
	found := false
	for _, m := range s.Messages {
		if strings.Contains(m.Content, "sk-abc123") {
			found = true
		}
	}
	if !found {
		t.Error("critical fact should survive mechanical fold")
	}
}

func TestCompactDegradesOnCompactorError(t *testing.T) {
	s := New("sess-5", KindMain)
	s.Add(provider.Message{Role: provider.RoleSystem, Content: "sys"})
	s.Add(provider.Message{Role: provider.RoleUser, Content: strings.Repeat("data", 200)})
	s.Add(provider.Message{Role: provider.RoleAssistant, Content: "done"})
	cfg := DefaultCompactConfig()
	cfg.Tail = 10

	fail := &fakeCompactor{err: errors.New("compactor down")}
	result, err := Compact(context.Background(), fail, s, cfg)
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if !result.Degraded {
		t.Fatal("expected degrade on compactor error")
	}
}

func TestMarkInterrupted(t *testing.T) {
	s := New("sess-6", KindMain)
	s.Add(provider.Message{Role: provider.RoleUser, Content: "task"})
	MarkInterrupted(s, "partial output", []string{"call_9"})

	last := s.Messages[len(s.Messages)-1]
	if !last.LocalOnly {
		t.Fatal("interrupted message must be LocalOnly")
	}
	if last.InterruptedTurn == nil || len(last.InterruptedTurn.InterruptedTools) != 1 {
		t.Fatalf("interrupted marker = %+v", last.InterruptedTurn)
	}
	// LocalOnly 消息不得外发
	normalized := provider.NormalizeMessages(s.Messages)
	for _, m := range normalized {
		if m.LocalOnly {
			t.Fatal("LocalOnly leaked into wire messages")
		}
	}
	// 恢复消息
	rec := InterruptedTurnRecoveryMessage(last.InterruptedTurn)
	if !strings.Contains(rec.Content, "call_9") || !strings.Contains(rec.Content, "被中断") {
		t.Errorf("recovery message = %q", rec.Content)
	}
}
