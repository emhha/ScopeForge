// Package guard 是确定性安全 Hook(docs/04 §5.4 命令黑名单 + §7 凭据外泄拦截)。
// 与 executor.Gate 的静态黑名单互补:guard 面向渗透场景的破坏性与外泄模式,
// 挂在工具执行前(executor.Gate.Check 之后),每次拦截:
//   - 记录入 events(kind=denied),供审计与提示词校准(docs/04 §5.4)
//   - 保留进程内审计队列(测试与报告可读)
//
// 覆盖(默认启用,可追加):
//   - 破坏性命令:rm -rf /、mkfs、磁盘擦除(dd/shred)
//   - 反弹 shell 变体、--network host
//   - 代理凭据外泄(export http_proxy=http://user:pass@host)
//   - 外发内容中的凭据形态(出站命令/URL 含密钥 → 拦截)
//
// 子进程环境过滤与凭据脱敏复用上游 secrets 包(docs/04 §7 密钥行)。
package guard

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"scopeforge/internal/event"
	"scopeforge/internal/reasonix/secrets"
)

// Denial 是一条拦截记录。
type Denial struct {
	TS      int64
	Kind    string // command | exfil
	Pattern string
	Text    string // 截断原文(≤512 字符)
}

// DefaultDenyPatterns 返回默认硬拦截模式(正则,匹配完整命令文本)。
// 这些模式是确定性 Hook(A4),不可通过配置关闭;配置只能追加。
// 注意:只拦"根删除/磁盘破坏/反弹 shell"等破坏性模式,授权内常规操作
// (rm -rf /tmp/xxx、curl -u 目标 basic auth)不拦截;凭据外泄由 exfil 模式覆盖。
func DefaultDenyPatterns() []string {
	return []string{
		`rm\s+(-[A-Za-z]*[rf][A-Za-z]*\s+)*(-+)?\s*/(\s|$)`, // rm -rf /(仅根删除)
		`\bmkfs\b`,              // 格式化磁盘
		`dd\s+[^|;]*of=/dev/sd`, // dd 直接写盘
		`\bshred\s+`,            // 磁盘擦除
		`(bash|sh|zsh)\s+-i\s*[><&]+\s*/dev/tcp/`, // 反弹 shell 直连
		`--network\s+host`,                        // 禁 --network host(docs/04 §5.1 反模式)
		`export\s+(http|https|all|ftp)_proxy=.*@`, // 代理凭据外泄
	}
}

// DefaultExfilPatterns 返回默认外泄检测模式(正则,匹配外发内容中的凭据形态)。
// 保守集合:只拦截高熵密钥形态,避免误伤正常流量。
func DefaultExfilPatterns() []string {
	return []string{
		`\b(sk|rk)-(proj-)?[A-Za-z0-9_-]{12,}\b`,                            // OpenAI/Anthropic 风格
		`\b(AKIA|ASIA)[0-9A-Z]{16}\b`,                                       // AWS access key
		`\bgh[pousr]_[A-Za-z0-9_]{20,}\b`,                                   // GitHub token
		`\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b`, // JWT
		`\bxox[baprs]-[A-Za-z0-9-]{16,}\b`,                                  // Slack token
	}
}

// Hook 是确定性拦截钩子。
type Hook struct {
	mu        sync.Mutex
	denyRe    []*regexp.Regexp
	exfilRe   []*regexp.Regexp
	sink      event.Sink
	challenge string
	denials   []Denial
	maxAudit  int
}

// NewHook 构建 Hook(默认模式 + 可选追加)。
func NewHook(sink event.Sink, challengeID string) (*Hook, error) {
	h := &Hook{sink: sink, challenge: challengeID, maxAudit: 200}
	if err := h.AddDenyPatterns(DefaultDenyPatterns()...); err != nil {
		return nil, err
	}
	if err := h.AddExfilPatterns(DefaultExfilPatterns()...); err != nil {
		return nil, err
	}
	return h, nil
}

// AddDenyPatterns 追加硬拦截模式。
func (h *Hook) AddDenyPatterns(patterns ...string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			return fmt.Errorf("guard: bad deny pattern %q: %w", p, err)
		}
		h.denyRe = append(h.denyRe, re)
	}
	return nil
}

// AddExfilPatterns 追加外泄检测模式。
func (h *Hook) AddExfilPatterns(patterns ...string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			return fmt.Errorf("guard: bad exfil pattern %q: %w", p, err)
		}
		h.exfilRe = append(h.exfilRe, re)
	}
	return nil
}

// CheckCommand 裁决一条命令文本。denied=true 时已记录审计与事件。
func (h *Hook) CheckCommand(cmd string) (reason string, denied bool) {
	h.mu.Lock()
	denyRe := h.denyRe
	exfilRe := h.exfilRe
	h.mu.Unlock()
	for _, re := range denyRe {
		if re.MatchString(cmd) {
			h.record("command", re.String(), cmd)
			return fmt.Sprintf("command matches guard deny pattern %q", re.String()), true
		}
	}
	// 凭据形态:出站命令必查;非出站命令也查
	// (防 echo/print 密钥进会话与日志,docs/06 §3.1 运行日志无密钥;
	//  exfil 模式均为高熵强形态,普通命令误伤率极低)
	for _, re := range exfilRe {
		if re.MatchString(cmd) {
			h.record("exfil", re.String(), cmd)
			return fmt.Sprintf("command carries credential pattern %q", re.String()), true
		}
	}
	return "", false
}

// CheckOutbound 裁决外发内容(web_fetch URL / 提交文本等)。denied=true 时已记录。
func (h *Hook) CheckOutbound(text string) (reason string, denied bool) {
	h.mu.Lock()
	exfilRe := h.exfilRe
	h.mu.Unlock()
	for _, re := range exfilRe {
		if re.MatchString(text) {
			h.record("exfil", re.String(), text)
			return fmt.Sprintf("outbound content carries credential pattern %q", re.String()), true
		}
	}
	return "", false
}

// Denials 返回审计队列副本。
func (h *Hook) Denials() []Denial {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]Denial, len(h.denials))
	copy(out, h.denials)
	return out
}

// record 记录拦截(进程内队列 + events 落库)。
// 命令原文脱敏后入日志(docs/06 §3.1 运行日志无密钥,密钥不落明文)。
func (h *Hook) record(kind, pattern, text string) {
	d := Denial{TS: time.Now().Unix(), Kind: kind, Pattern: pattern, Text: truncate(secrets.RedactCredentials(text), 512)}
	h.mu.Lock()
	h.denials = append(h.denials, d)
	if len(h.denials) > h.maxAudit {
		h.denials = h.denials[len(h.denials)-h.maxAudit:]
	}
	h.mu.Unlock()
	if h.sink != nil {
		h.sink.Emit(event.Event{Kind: event.KindDenied, ChallengeID: h.challenge,
			Payload: map[string]any{"kind": kind, "pattern": pattern, "text": d.Text}})
	}
}

// isOutbound 判断命令是否可能外发流量。
func isOutbound(cmd string) bool {
	first := cmd
	if i := strings.IndexAny(first, "|;&"); i >= 0 {
		first = first[:i]
	}
	first = strings.TrimSpace(first)
	for _, kw := range []string{"curl", "wget", "nc ", "ncat", "socat", "python3 -c", "python -c"} {
		if strings.HasPrefix(first, kw) || strings.Contains(first, " "+kw) {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
