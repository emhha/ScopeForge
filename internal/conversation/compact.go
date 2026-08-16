package conversation

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"scopeforge/internal/reasonix/provider"
)

// CompactConfig 是压缩阈值配置(docs/02 §2.4,继承迁移源,可配置)。
type CompactConfig struct {
	Soft         float64 // 窗口利用率阈值:只提示不动前缀
	Snip         float64 // 先廉价裁剪陈旧工具输出
	Compact      float64 // 触发压缩(异步)
	Force        float64 // 强制压缩(同步阻塞)
	Tail         int     // 压缩后尾部保留预算(token)
	ContextWindow int    // 上下文窗口容量(token)
}

// DefaultCompactConfig 返回默认阈值。
func DefaultCompactConfig() CompactConfig {
	return CompactConfig{
		Soft:          0.5,
		Snip:          0.6,
		Compact:       0.8,
		Force:         0.9,
		Tail:          16384,
		ContextWindow: 65536,
	}
}

// Level 是压缩等级。
type Level int

const (
	LevelNone    Level = iota // 无需处理
	LevelSoft                 // 只提示不动前缀
	LevelSnip                 // 廉价裁剪陈旧工具输出
	LevelCompact              // 压缩(异步)
	LevelForce                // 强制压缩(同步阻塞)
)

func (l Level) String() string {
	switch l {
	case LevelSoft:
		return "soft"
	case LevelSnip:
		return "snip"
	case LevelCompact:
		return "compact"
	case LevelForce:
		return "force"
	default:
		return "none"
	}
}

// LevelFor 根据窗口利用率返回压缩等级。
func LevelFor(ratio float64, cfg CompactConfig) Level {
	switch {
	case ratio >= cfg.Force:
		return LevelForce
	case ratio >= cfg.Compact:
		return LevelCompact
	case ratio >= cfg.Snip:
		return LevelSnip
	case ratio >= cfg.Soft:
		return LevelSoft
	default:
		return LevelNone
	}
}

// EstimateTokens 估算 token 数(字符数/4,中文保守取 /2)。
func EstimateTokens(s string) int {
	cjk := 0
	other := 0
	for _, r := range s {
		if r >= 0x4E00 && r <= 0x9FFF {
			cjk++
		} else {
			other++
		}
	}
	return cjk/2 + other/4
}

// SnipStaleToolResults 廉价裁剪陈旧工具输出:保留最近 keepRecent 个工具结果,
// 更早的超大 tool 消息输出截断为摘要提示。
func SnipStaleToolResults(msgs []provider.Message, keepRecent int) []provider.Message {
	if keepRecent < 1 {
		keepRecent = 1
	}
	total := 0
	for _, m := range msgs {
		if m.Role == provider.RoleTool {
			total++
		}
	}
	kept := 0
	out := make([]provider.Message, 0, len(msgs))
	for _, m := range msgs {
		if m.Role == provider.RoleTool {
			kept++
			if kept <= total-keepRecent && len(m.Content) > 4000 {
				m.Content = truncateForSnip(m.Content)
			}
		}
		out = append(out, m)
	}
	return out
}

func truncateForSnip(s string) string {
	const maxSnip = 2000
	if len(s) <= maxSnip {
		return s
	}
	// 保留头尾(命令输出常为日志:头部概要 + 尾部错误)
	head := s[:maxSnip/2]
	tail := s[len(s)-maxSnip/2:]
	return head + "\n...[snip: " + fmt.Sprint(len(s)) + " chars trimmed by SnipStaleToolResults]...\n" + tail
}

// CompactionSummary 是压缩结果。
type CompactionSummary struct {
	Summary      string                 // 摘要文本(含 <compaction-summary> 标签)
	Compacted    []provider.Message     // 被压缩的消息(归档用)
	KeptTail     []provider.Message     // 保留的尾部消息
	Degraded     bool                   // 是否降级为机械折叠
	ArchiveNote  string
}

// Compactor 由模型生成摘要(压缩小模型,独立便宜 provider)。
type Compactor interface {
	// Summarize 生成摘要。返回包裹 <compaction-summary> 标签的文本。
	Summarize(ctx context.Context, msgs []provider.Message) (string, error)
}

// SummaryPrompt 是摘要系统提示,强制 7 个标题契约(docs/02 §2.4)。
const SummaryPrompt = `你是上下文压缩器。请阅读以下对话,生成一份压缩摘要,必须包裹在 <compaction-summary> 标签内。
摘要必须包含以下 7 个标题(顺序不限,缺失即为不合格):
1. standing facts — 已经确认不变的事实
2. Goal — 当前目标
3. Decisions — 已经做出的关键决策
4. Files — 涉及的文件与路径
5. Commands — 运行过的关键命令
6. Errors — 遇到过的错误与排除状态
7. Pending — 尚未完成的事项
要求:
- 保留所有 critical facts:URL、凭据、路径、payload、flag 片段,一字不差
- 输出为中文,简洁,每标题下用列表
- 不要添加对话中没有的信息`

// SummaryTagOpen/Close 是摘要包裹标签。
const (
	SummaryTagOpen  = "<compaction-summary>"
	SummaryTagClose = "</compaction-summary>"
)

var (
	urlRe      = regexp.MustCompile(`https?://[^\s"'<>]+`)
	credRe     = regexp.MustCompile(`(?i)(api[_-]?key|token|password|secret|flag)\s*[:=]\s*[^\s,;]+`)
	pathRe     = regexp.MustCompile(`(/[A-Za-z0-9_.\-/]+){2,}`)
)

// Compact 执行分级压缩(docs/02 §2.4):
//  1. 调用方必须先落库再压缩(SaveSnapshot → 审计可回退)
//  2. compactor.Summarize → 7 标题契约
//  3. critical facts 校验:URL/凭据/路径/payload 必须仍在摘要,否则降级机械折叠
//  4. 归档旧消息
//
// 返回压缩后的完整消息序列(系统提示前缀原样保留,摘要插入其后)。
func Compact(ctx context.Context, compactor Compactor, s *Session, cfg CompactConfig) (*CompactionSummary, error) {
	msgs := s.Snapshot()

	// 找到系统提示边界(前缀稳定:系统提示原样保留)
	prefixEnd := 0
	for i, m := range msgs {
		if m.Role == provider.RoleSystem {
			prefixEnd = i + 1
		} else {
			break
		}
	}

	// 尾部保留预算:从后往前累计
	tailStart := len(msgs)
	tokens := 0
	for i := len(msgs) - 1; i >= prefixEnd; i-- {
		t := EstimateTokens(msgs[i].Content)
		for _, tc := range msgs[i].ToolCalls {
			t += EstimateTokens(tc.Arguments)
		}
		if tokens+t > cfg.Tail && i < len(msgs)-1 {
			break
		}
		tokens += t
		tailStart = i
	}

	toCompact := msgs[prefixEnd:tailStart]
	keptTail := msgs[tailStart:]
	keptPrefix := msgs[:prefixEnd]

	result := &CompactionSummary{
		KeptTail:  append([]provider.Message{}, keptTail...),
		Compacted: append([]provider.Message{}, toCompact...),
	}

	if len(toCompact) == 0 {
		// 无可压缩内容:直接保留
		return result, nil
	}

	summary, err := compactor.Summarize(ctx, toCompact)
	if err != nil || !strings.Contains(summary, SummaryTagOpen) {
		// 降级:机械折叠 + 归档
		result.Degraded = true
		result.Summary = ""
		result.ArchiveNote = fmt.Sprintf("compactor failed (%v); degraded to mechanical fold", err)
		folded := mechanicalFold(toCompact)
		newMsgs := append(append(append([]provider.Message{}, keptPrefix...), folded...), keptTail...)
		s.Rewrite(newMsgs)
		return result, nil
	}

	// critical facts 校验
	critical := extractCriticalFacts(toCompact)
	if len(critical) > 0 && !containsAll(summary, critical) {
		// 摘要丢失关键事实 → 降级机械折叠 + 归档
		result.Degraded = true
		result.Summary = summary
		result.ArchiveNote = fmt.Sprintf("summary dropped critical facts (%d missing); degraded", len(critical)-countContained(summary, critical))
		folded := mechanicalFold(toCompact)
		newMsgs := append(append(append([]provider.Message{}, keptPrefix...), folded...), keptTail...)
		s.Rewrite(newMsgs)
		return result, nil
	}

	result.Summary = summary
	// 摘要作为 user 消息插入系统提示之后
	summaryMsg := provider.Message{Role: provider.RoleUser, Content: summary}
	newMsgs := append(append(append([]provider.Message{}, keptPrefix...), summaryMsg), keptTail...)
	s.Rewrite(newMsgs)
	return result, nil
}

// mechanicalFold 机械折叠:压缩消息中所有 tool 输出截断,保留结构。
func mechanicalFold(msgs []provider.Message) []provider.Message {
	out := make([]provider.Message, 0, len(msgs))
	for _, m := range msgs {
		if m.Role == provider.RoleTool && len(m.Content) > 500 {
			m.Content = truncateForSnip(m.Content)
		}
		out = append(out, m)
	}
	return out
}

// extractCriticalFacts 提取 URL / 凭据 / 路径 / payload 关键事实。
func extractCriticalFacts(msgs []provider.Message) []string {
	var facts []string
	seen := map[string]bool{}
	add := func(f string) {
		if f == "" || seen[f] {
			return
		}
		seen[f] = true
		facts = append(facts, f)
	}
	for _, m := range msgs {
		text := m.Content
		for _, tc := range m.ToolCalls {
			text += " " + tc.Name + " " + tc.Arguments
		}
		for _, u := range urlRe.FindAllString(text, -1) {
			add(strings.Trim(u, ".,;:()[]"))
		}
		for _, c := range credRe.FindAllString(text, -1) {
			add(strings.Trim(c, ".,;:"))
		}
		for _, p := range pathRe.FindAllString(text, -1) {
			if !strings.Contains(p, " ") {
				add(p)
			}
		}
	}
	return facts
}

func containsAll(haystack string, needles []string) bool {
	for _, n := range needles {
		if !strings.Contains(haystack, n) {
			return false
		}
	}
	return true
}

func countContained(haystack string, needles []string) int {
	n := 0
	for _, x := range needles {
		if strings.Contains(haystack, x) {
			n++
		}
	}
	return n
}
