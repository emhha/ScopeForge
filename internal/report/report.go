// Package report 是报告生成(docs/phase2/05)。
//
// 数据源:漏洞账本 + 覆盖矩阵 + breach 状态空间 + events 轨迹 + 成本。
// 报告按 goal_shape 分叉(05 §2):coverage → 漏洞清单版;breach → 攻击链版。
//
// 质量纪律(§5.3):
//   - 证据引用校验:报告引用的证据(flowRef/event seq)必须存在于 events 表,
//     伪造引用被拒(防幻觉引用)
//   - 脱敏(05 §3):凭据/内网 IP/个人数据(邮箱/手机/身份证)强制打码;未脱敏版
//     需显式允许(AllowUnredacted),否则生成拒绝(fail-closed)
//   - 签名与导出(05 §5):Markdown(默认)+ JSON(机器可读);复核签名(报告 hash +
//     复核人/时间戳)写入 reports/<task>.meta.json(审计留痕,非加密签名)
//   - 触发:任务终止(三路 OR 任一)后自动生成;也可手动触发
package report

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"scopeforge/internal/blackboard"
	"scopeforge/internal/breach"
	"scopeforge/internal/constraint"
	"scopeforge/internal/coverage"
	"scopeforge/internal/event"
	"scopeforge/internal/store"
)

// Generator 是报告生成器。并发安全:同 challenge 串行生成(§5.3 防并发写损坏)。
type Generator struct {
	DB      *store.DB
	Board   *blackboard.Blackboard
	Ledger  *constraint.CostLedger
	Sink    event.Sink
	WorkDir string

	// AllowUnredacted 是否允许生成未脱敏版(05 §3 fail-closed:默认 false,未脱敏
	// 生成被拒;管理员经 API token 显式置 true 或走独立端点留痕)。
	AllowUnredacted bool

	// phase2 报告分叉(docs/phase2/05 §2):空 = coverage(默认)。
	GoalShape string           // coverage | breach
	Breach    *breach.Space    // breach 攻击链/权限证明数据源(nil = 跳过攻击链表)
	Coverage  *coverage.Matrix // coverage 覆盖矩阵数据源(nil = 跳过覆盖矩阵节)

	// Interest 交付过滤(04 §5.2:如企业 B 只对高危感兴趣):低于 interest 的
	// 漏洞只留账本与审计,不进交付报告正文(05 §2.1 注);空 = 不过滤。
	Interest constraint.Interest

	mu sync.Mutex // 串行化生成(自动 + 手动可能并发触发)
}

// Report 是一份报告。
type Report struct {
	ChallengeID      string   `json:"challenge_id"`
	Markdown         string   `json:"markdown"`
	Redacted         bool     `json:"redacted"`
	GeneratedAt      int64    `json:"generated_at"`
	EvidenceOK       int      `json:"evidence_ok"`
	EvidenceRejected []string `json:"evidence_rejected"`
	Path             string   `json:"path,omitempty"`
}

// Generate 生成报告。redacted=true 时凭据/内网 IP/个人数据脱敏(默认)。
// 05 §3 fail-closed:redacted=false(未脱敏版)仅 AllowUnredacted 时允许,否则拒绝。
// 并发安全:全局串行;未脱敏版独立文件 <id>-unredacted.md(不覆盖脱敏版,审计留痕)。
func (g *Generator) Generate(ctx context.Context, challengeID string, redacted bool) (*Report, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.generateLocked(ctx, challengeID, redacted)
}

// generateLocked 生成报告(调用方持锁)。
func (g *Generator) generateLocked(ctx context.Context, challengeID string, redacted bool) (*Report, error) {
	if !redacted && !g.AllowUnredacted {
		return nil, fmt.Errorf("report: 未脱敏生成被拒(fail-closed,05 §3);需管理员显式允许")
	}
	rep := &Report{ChallengeID: challengeID, Redacted: redacted, GeneratedAt: time.Now().Unix()}

	facts, err := g.Board.Facts(challengeID, 0)
	if err != nil {
		return nil, err
	}
	chalEvents, _ := event.QueryForChallenge(g.DB, challengeID, 0)
	spend := constraint.Spend{}
	if g.Ledger != nil {
		spend, _ = g.Ledger.Spend(challengeID)
	}

	md := g.render(challengeID, facts, chalEvents, spend)
	// 证据校验:引用必须存在(§5.3 防幻觉引用)
	refs := extractRefs(md)
	ok, rejected := ValidateEvidence(chalEvents, refs)
	rep.EvidenceOK = ok
	rep.EvidenceRejected = rejected
	if len(rejected) > 0 {
		// 伪造引用被拒:报告头部加警告(§5.4)
		md = "> ⚠ 证据校验:以下引用不存在,已被标记拒绝: " + strings.Join(rejected, ", ") + "\n\n" + md
	}

	if redacted {
		md = Redact(md)
	}
	rep.Markdown = md

	// 落盘 reports/<id>.md(未脱敏版独立文件,互不覆盖)
	dir := filepath.Join(g.WorkDir, "reports")
	if err := os.MkdirAll(dir, 0o755); err == nil {
		name := challengeID + ".md"
		if !redacted {
			name = challengeID + "-unredacted.md"
		}
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(md), 0o644); err == nil {
			rep.Path = path
		}
	}
	if g.Sink != nil {
		g.Sink.Emit(event.Event{Kind: event.KindReport, ChallengeID: challengeID,
			Payload: map[string]any{"challenge_id": challengeID, "redacted": redacted,
				"evidence_ok": ok, "evidence_rejected": rejected, "path": rep.Path}})
	}
	return rep, nil
}

// render 按 goal_shape 分叉渲染(docs/phase2/05 §2):
// coverage → 漏洞清单版(§2.1);breach → 攻击链版(§2.2)。
func (g *Generator) render(challengeID string, facts []blackboard.Fact,
	events []event.Event, spend constraint.Spend) string {
	if g.GoalShape == constraint.GoalShapeBreach {
		return g.renderBreach(challengeID, facts, events, spend)
	}
	return g.renderCoverage(challengeID, facts, events, spend)
}

// renderCoverage 覆盖清单版报告(docs/phase2/05 §2.1):
// 执行摘要 / 漏洞清单(账本) / 覆盖矩阵 / 复现步骤 / 剩余风险 / 附录。
func (g *Generator) renderCoverage(challengeID string, facts []blackboard.Fact,
	events []event.Event, spend constraint.Spend) string {
	var b strings.Builder
	now := time.Now().Format("2006-01-02 15:04:05")
	vulns, _ := g.Board.Vulnerabilities(challengeID)
	// 交付过滤(04 §5.2 interest):低于 interest 的漏洞只留账本/审计,不进交付正文
	delivered, filtered := g.deliverable(vulns)
	sevDist := map[string]int{}
	for _, v := range delivered {
		if v.Status == blackboard.LedgerAccepted {
			sevDist[v.Severity]++
		}
	}
	order := []string{"critical", "high", "medium", "low", "info"}

	b.WriteString("# ScopeForge 漏洞报告(coverage)\n\n")
	// 1. 执行摘要
	fmt.Fprintf(&b, "## 1. 执行摘要\n\n- 任务: `%s`\n- 时间: %s\n", challengeID, now)
	if len(filtered) > 0 {
		fmt.Fprintf(&b, "> interest 过滤: %d 条低于交付门槛的漏洞仅留账本与审计(05 §2.1)\n\n", len(filtered))
	}
	fmt.Fprintf(&b, "- 已确认漏洞: %d 条(severity 分布: ", vulnCount(sevDist))
	first := true
	for _, s := range order {
		if sevDist[s] > 0 {
			if !first {
				b.WriteString(", ")
			}
			first = false
			fmt.Fprintf(&b, "%s×%d", s, sevDist[s])
		}
	}
	if first {
		b.WriteString("无")
	}
	b.WriteString(")\n")
	// 覆盖度结论
	if g.Coverage != nil {
		cells, _ := g.Coverage.Cells(challengeID)
		open, confirmed, skipped := 0, 0, 0
		for _, c := range cells {
			switch c.Status {
			case coverage.StatusOpen:
				open++
			case coverage.StatusConfirmed:
				confirmed++
			case coverage.StatusSkipped, coverage.StatusDead:
				skipped++
			}
		}
		fmt.Fprintf(&b, "- 覆盖矩阵: %d 已覆盖 / %d 未覆盖 / %d 排除(已试)\n", confirmed, open, skipped)
	}
	fmt.Fprintf(&b, "- 成本 %.4f USD, %d 轮\n\n", spend.CostUSD, spend.Turns)

	// 2. 漏洞清单(账本为唯一主体,05 §2.1;interest 过滤后仅交付条目)
	b.WriteString("## 2. 漏洞清单\n\n")
	if len(delivered) == 0 {
		b.WriteString("(无交付漏洞)")
		if len(filtered) > 0 {
			fmt.Fprintf(&b, "(账本另有 %d 条低于 interest 门槛)", len(filtered))
		}
		b.WriteString("\n\n")
	}
	for i, v := range delivered {
		fmt.Fprintf(&b, "### %d. %s\n\n", i+1, shortTitle(v.Title))
		fmt.Fprintf(&b, "- CWE: %s\n- 资产: `%s`\n- 端点: `%s`\n- 严重级: %s\n",
			orDash(v.CWE), orDash(v.Asset), orDash(v.Endpoint), orDash(v.Severity))
		fmt.Fprintf(&b, "- 回执状态: %s\n- 证据: %s\n\n", v.Status, refLabel(v.EvidenceRef))
	}

	// 3. 覆盖矩阵(S6 公理可视化)
	b.WriteString("## 3. 覆盖矩阵\n\n")
	if g.Coverage == nil {
		b.WriteString("(覆盖矩阵未启用)\n\n")
	} else {
		cells, _ := g.Coverage.Cells(challengeID)
		if len(cells) == 0 {
			b.WriteString("(无攻击面格子)\n\n")
		} else {
			b.WriteString("| CWE | 资产 | 端点 | 状态 | 排除理由 |\n|---|---|---|---|---|\n")
			for _, c := range cells {
				reason := c.SkipReason
				if c.Status != coverage.StatusSkipped && c.Status != coverage.StatusDead {
					reason = ""
				}
				fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n",
					orDash(c.CWE), orDash(c.Asset), orDash(c.Endpoint), c.Status, orDash(reason))
			}
			b.WriteString("\n")
		}
	}

	// 4. 复现步骤(关键工具调用序列,从事件流重建)
	b.WriteString("## 4. 复现步骤\n\n")
	steps := reproductionSteps(events)
	if len(steps) == 0 {
		b.WriteString("(无工具调用记录)\n\n")
	} else {
		for i, s := range steps {
			fmt.Fprintf(&b, "%d. `%s` %s\n", i+1, s.tool, s.arg)
		}
		b.WriteString("\n")
	}

	// 5. 剩余风险(未完成方向 + 建议)
	b.WriteString("## 5. 剩余风险\n\n")
	risks := g.remainingRisks(challengeID)
	if len(risks) == 0 {
		b.WriteString("(无已知未覆盖方向)\n\n")
	} else {
		for _, r := range risks {
			fmt.Fprintf(&b, "- %s\n", r)
		}
		b.WriteString("\n")
	}

	// 6. 附录
	g.appendAppendix(&b, challengeID, facts, events, spend)
	return b.String()
}

// renderBreach 攻击链版报告(docs/phase2/05 §2.2):
// 执行摘要 / 攻击链 / 权限证明 / 漏洞清单 / 未达成路径 / 处置建议。
func (g *Generator) renderBreach(challengeID string, facts []blackboard.Fact,
	events []event.Event, spend constraint.Spend) string {
	var b strings.Builder
	now := time.Now().Format("2006-01-02 15:04:05")
	vulns, _ := g.Board.Vulnerabilities(challengeID)

	// goal 判定(独立验证器,模型自述不采信)
	goalReached, goalID := false, ""
	if g.Breach != nil {
		ok, id := g.Breach.IsGoalReached(context.Background(), challengeID)
		goalReached, goalID = ok, id
	}
	closed, _ := false, ""
	if g.Breach != nil {
		closed, _ = g.Breach.IsSpaceClosed(context.Background(), challengeID)
	}

	b.WriteString("# ScopeForge 渗透测试报告(breach)\n\n")
	// 1. 执行摘要
	fmt.Fprintf(&b, "## 1. 执行摘要\n\n- 任务: `%s`\n- 时间: %s\n", challengeID, now)
	switch {
	case goalReached:
		fmt.Fprintf(&b, "- 目标达成: ✅ `%s`(独立验证器确认)\n", goalID)
	case closed:
		b.WriteString("- 目标未达成: 可达状态空间已闭合(所有路径已尝试)\n")
	default:
		b.WriteString("- 目标未达成: 任务提前终止(预算/穷尽声明)\n")
	}
	fmt.Fprintf(&b, "- 已确认状态节点: %d 个\n- 漏洞账本: %d 条\n- 成本 %.4f USD, %d 轮\n\n",
		len(g.confirmedNodes(challengeID)), len(vulns), spend.CostUSD, spend.Turns)

	// 2. 攻击链(状态转移边序列,03 §3.4.1)
	b.WriteString("## 2. 攻击链\n\n")
	nodes := g.confirmedNodes(challengeID)
	if len(nodes) == 0 {
		b.WriteString("(无已确认状态)\n\n")
	} else {
		for _, n := range nodes {
			fmt.Fprintf(&b, "- 状态: `%s`(kind=%s, asset=%s%s)\n",
				n.ID, n.Kind, orDash(n.Asset), privilegeSuffix(n.Privilege))
		}
		b.WriteString("\n转移边:\n\n")
		if g.Breach != nil {
			if edges, err := g.Breach.Edges(challengeID); err == nil && len(edges) > 0 {
				for _, e := range edges {
					fmt.Fprintf(&b, "- `%s` --[%s]--> `%s` (%s%s)\n",
						e.From, orDash(e.Action), orDash(e.To), e.Status, edgeReason(e))
				}
				b.WriteString("\n")
			} else {
				b.WriteString("(无转移边)\n\n")
			}
		}
	}

	// 3. 权限证明(独立验证器确认的状态,非模型自述)
	b.WriteString("## 3. 权限证明\n\n")
	if len(nodes) == 0 {
		b.WriteString("(无)\n\n")
	} else {
		for _, n := range nodes {
			fmt.Fprintf(&b, "- ✅ `%s`(验证器确认,非模型自述)\n", n.ID)
		}
		b.WriteString("\n")
	}

	// 4. 漏洞清单(路径中利用的漏洞,复用账本)
	b.WriteString("## 4. 漏洞清单\n\n")
	if len(vulns) == 0 {
		b.WriteString("(无已提交漏洞)\n\n")
	}
	for i, v := range vulns {
		fmt.Fprintf(&b, "### %d. %s\n\n- CWE: %s\n- 资产: `%s`\n- 端点: `%s`\n- 严重级: %s\n- 回执状态: %s\n- 证据: %s\n\n",
			i+1, shortTitle(v.Title), orDash(v.CWE), orDash(v.Asset), orDash(v.Endpoint),
			orDash(v.Severity), v.Status, refLabel(v.EvidenceRef))
	}

	// 5. 未达成路径(已尝试 + 排除理由,space_closed 证据,S6)
	b.WriteString("## 5. 未达成路径\n\n")
	if g.Breach != nil {
		if edges, err := g.Breach.Edges(challengeID); err == nil {
			dead := 0
			for _, e := range edges {
				if e.Status == "dead" || e.Status == "skipped" {
					dead++
					fmt.Fprintf(&b, "- `%s` --[%s]--> `%s`: %s\n", e.From, orDash(e.Action), orDash(e.To), edgeReason(e))
				}
			}
			if dead == 0 {
				b.WriteString("(无失败边)\n\n")
			} else {
				b.WriteString("\n")
			}
		}
	} else {
		b.WriteString("(breach 空间未启用)\n\n")
	}

	// 6. 处置建议(按路径依赖:堵住入口即断链)
	b.WriteString("## 6. 处置建议\n\n")
	entries := g.entryNodes(challengeID)
	if len(entries) == 0 {
		b.WriteString("(无可确认入口)\n\n")
	} else {
		b.WriteString("按路径依赖排序——修复入口节点即断链:\n\n")
		for i, n := range entries {
			fmt.Fprintf(&b, "%d. 优先修复入口 `%s`(kind=%s, asset=%s)\n", i+1, n.ID, n.Kind, orDash(n.Asset))
		}
		b.WriteString("\n")
	}

	// 附录(时间线/成本/账本提交记录)
	g.appendAppendix(&b, challengeID, facts, events, spend)
	return b.String()
}

// appendAppendix 附录:时间线 + 成本明细 + 账本提交记录(两版共用)。
func (g *Generator) appendAppendix(b *strings.Builder, challengeID string, facts []blackboard.Fact,
	events []event.Event, spend constraint.Spend) {
	b.WriteString("## 附录\n\n")
	b.WriteString("### 时间线(事件摘要)\n\n")
	b.WriteString("| seq | kind | 摘要 |\n|---|---|---|\n")
	timeline := 0
	for _, e := range events {
		summary := eventSummary(e)
		if summary == "" {
			continue
		}
		timeline++
		if timeline > 200 {
			b.WriteString("| ... | (截断) | 完整轨迹见 events 表 / 回放 |\n")
			break
		}
		fmt.Fprintf(b, "| %d | %s | %s |\n", e.Seq, e.Kind, escapePipe(summary))
	}
	b.WriteString("\n### 成本明细\n\n")
	fmt.Fprintf(b, "- prompt=%d cache_hit=%d output=%d reasoning=%d tokens\n- cost=%.4f USD, turns=%d\n\n",
		spend.PromptTokens, spend.CacheHitTokens, spend.OutputTokens, spend.ReasoningTokens, spend.CostUSD, spend.Turns)
	b.WriteString("### 账本提交记录\n\n")
	vulns, _ := g.Board.Vulnerabilities(challengeID)
	if len(vulns) == 0 {
		b.WriteString("- (无)\n")
	}
	for _, v := range vulns {
		fmt.Fprintf(b, "- [%s] %s @ %s%s → %s\n", timeStr(v.SubmittedAt), shortTitle(v.Title), orDash(v.Asset), orDash(v.Endpoint), v.Status)
	}
	b.WriteString("\n### 脱敏说明\n\n")
	b.WriteString("- 凭据/密钥/内网 IP 默认脱敏;未脱敏版需管理员权限并留痕\n")
	_ = facts
}

// confirmedNodes 已确认状态节点(breach)。
func (g *Generator) confirmedNodes(challengeID string) []breach.Node {
	if g.Breach == nil {
		return nil
	}
	nodes, err := g.Breach.ConfirmedNodes(challengeID)
	if err != nil {
		return nil
	}
	return nodes
}

// entryNodes 攻击链入口节点(被确认边的起点,处置建议的修复优先级)。
func (g *Generator) entryNodes(challengeID string) []breach.Node {
	nodes := g.confirmedNodes(challengeID)
	if len(nodes) == 0 || g.Breach == nil {
		return nil
	}
	edges, err := g.Breach.Edges(challengeID)
	if err != nil {
		return nil
	}
	hasOut := map[string]bool{}
	for _, e := range edges {
		hasOut[e.From] = true
	}
	var entries []breach.Node
	for _, n := range nodes {
		if hasOut[n.ID] {
			entries = append(entries, n)
		}
	}
	if len(entries) == 0 {
		return nodes // 无出边时以全部确认节点兜底
	}
	return entries
}

// remainingRisks 剩余风险:未认领/未完成的方向(任务终止时仍未覆盖,05 §2.1 第 5 节)。
// open = 调度未认领(时间/预算不足);pending = 认领后无进展。
func (g *Generator) remainingRisks(challengeID string) []string {
	intents, err := g.Board.Intents(challengeID, []string{blackboard.StateOpen, blackboard.StatePending}, 0)
	if err != nil {
		return nil
	}
	var out []string
	for _, it := range intents {
		out = append(out, it.Text)
	}
	return out
}

// reproductionStep 复现步骤单步。
type reproductionStep struct {
	tool string
	arg  string
}

// reproductionSteps 从事件流重建关键工具调用序列(05 §2.1 第 4 节)。
func reproductionSteps(events []event.Event) []reproductionStep {
	var out []reproductionStep
	for _, e := range events {
		if e.Kind != event.KindToolCallStart {
			continue
		}
		p, ok := e.Payload.(map[string]any)
		if !ok {
			continue
		}
		name, _ := p["name"].(string)
		if name == "" {
			continue
		}
		arg := ""
		if a, ok := p["args"].(string); ok {
			arg = truncateStr(a, 60)
		}
		out = append(out, reproductionStep{tool: name, arg: arg})
	}
	return out
}

// deliverable 按 Interest 过滤交付漏洞(04 §5.2:低危只记账不进交付报告)。
// Interest 零值(无门槛)→ 全部交付。
func (g *Generator) deliverable(vulns []blackboard.Vulnerability) (delivered, filtered []blackboard.Vulnerability) {
	if g.Interest.SeverityMin == "" && len(g.Interest.CWEs) == 0 {
		return vulns, nil
	}
	for _, v := range vulns {
		if g.Interest.Match(v.Severity, v.CWE) {
			delivered = append(delivered, v)
		} else {
			filtered = append(filtered, v)
		}
	}
	return delivered, filtered
}

func vulnCount(m map[string]int) int {
	n := 0
	for _, c := range m {
		n += c
	}
	return n
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func privilegeSuffix(p string) string {
	if p == "" {
		return ""
	}
	return ", privilege=" + p
}

func edgeReason(e breach.Edge) string {
	if e.SkipReason != "" {
		return "排除理由: " + e.SkipReason
	}
	return ""
}

// ValidateEvidence 校验证据引用存在性(§5.3 防幻觉引用)。
// 引用格式: ev-<seq>(events 表 seq) 或 flow-<n>(traffic flow_ref)。
// 返回:通过数 + 被拒引用列表。
func ValidateEvidence(chalEvents []event.Event, refs []string) (int, []string) {
	seqSet := map[int64]bool{}
	flowSet := map[string]bool{}
	for _, e := range chalEvents {
		seqSet[e.Seq] = true
		if e.Kind == event.KindTraffic {
			if p, ok := e.Payload.(map[string]any); ok {
				if fr, ok := p["flow_ref"].(string); ok {
					flowSet[fr] = true
				}
			}
		}
	}
	ok := 0
	var rejected []string
	for _, ref := range refs {
		valid := false
		if strings.HasPrefix(ref, "ev-") {
			var seq int64
			if _, err := fmt.Sscanf(ref, "ev-%d", &seq); err == nil && seqSet[seq] {
				valid = true
			}
		}
		if strings.HasPrefix(ref, "flow-") && flowSet[ref] {
			valid = true
		}
		if valid {
			ok++
		} else {
			rejected = append(rejected, ref)
		}
	}
	return ok, rejected
}

// extractRefs 提取 Markdown 中的证据引用。
func extractRefs(md string) []string {
	re := regexp.MustCompile(`(?:ev-\d+|flow-\d+)`)
	seen := map[string]bool{}
	var out []string
	for _, m := range re.FindAllString(md, -1) {
		if !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	sort.Strings(out)
	return out
}

// ------------------------------------------------------------------ 05 §5 导出与签名

// ReportJSON 是机器可读报告(05 §5:JSON 导出,含完整证据引用,供机器校验/回放)。
type ReportJSON struct {
	ChallengeID    string                     `json:"challenge_id"`
	GoalShape      string                     `json:"goal_shape"`
	GeneratedAt    int64                      `json:"generated_at"`
	SeverityDist   map[string]int             `json:"severity_distribution"`
	Vulns          []blackboard.Vulnerability `json:"vulnerabilities"`
	Coverage       []coverage.Cell            `json:"coverage_matrix,omitempty"`
	BreachNodes    []breach.Node              `json:"breach_nodes,omitempty"`
	BreachEdges    []breach.Edge              `json:"breach_edges,omitempty"`
	RemainingRisks []string                   `json:"remaining_risks"`
	EvidenceRefs   []string                   `json:"evidence_refs"`
	Spend          constraint.Spend           `json:"spend"`
}

// ExportJSON 生成并落盘结构化 JSON 报告(05 §5:reports/<id>.json)。
// 数据源与 Markdown 版同源(账本/覆盖矩阵/breach 状态空间/剩余风险/成本)。
func (g *Generator) ExportJSON(ctx context.Context, challengeID string) (*ReportJSON, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	vulns, _ := g.Board.Vulnerabilities(challengeID)
	sevDist := map[string]int{}
	for _, v := range vulns {
		if v.Status == blackboard.LedgerAccepted {
			sevDist[v.Severity]++
		}
	}
	spend := constraint.Spend{}
	if g.Ledger != nil {
		spend, _ = g.Ledger.Spend(challengeID)
	}
	out := &ReportJSON{
		ChallengeID: challengeID, GoalShape: g.GoalShape, GeneratedAt: time.Now().Unix(),
		SeverityDist: sevDist, Vulns: vulns, Spend: spend,
	}
	if g.GoalShape == "" {
		out.GoalShape = constraint.GoalShapeCoverage
	}
	if g.Coverage != nil {
		out.Coverage, _ = g.Coverage.Cells(challengeID)
	}
	if g.Breach != nil {
		out.BreachNodes, _ = g.Breach.ConfirmedNodes(challengeID)
		out.BreachEdges, _ = g.Breach.Edges(challengeID)
	}
	out.RemainingRisks = g.remainingRisks(challengeID)
	seen := map[string]bool{}
	for _, v := range vulns {
		if v.EvidenceRef != "" && !seen[v.EvidenceRef] {
			seen[v.EvidenceRef] = true
			out.EvidenceRefs = append(out.EvidenceRefs, v.EvidenceRef)
		}
	}
	sort.Strings(out.EvidenceRefs)

	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(g.WorkDir, "reports")
	path := ""
	if os.MkdirAll(dir, 0o755) == nil {
		path = filepath.Join(dir, challengeID+".json")
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return nil, err
		}
	}
	if g.Sink != nil {
		g.Sink.Emit(event.Event{Kind: event.KindReport, ChallengeID: challengeID,
			Payload: map[string]any{"action": "export", "format": "json", "path": path}})
	}
	return out, nil
}

// Signature 是复核签名留痕(05 §5:非加密签名——报告 hash + 复核人/时间戳,
// 合规签名的加密要求留 M4 后评估,诚实边界)。
type Signature struct {
	ChallengeID string `json:"challenge_id"`
	Reviewer    string `json:"reviewer"`
	SignedAt    int64  `json:"signed_at"`
	ReportHash  string `json:"report_hash"` // sha256(脱敏 Markdown)
	Redacted    bool   `json:"redacted"`
}

// Sign 复核签名:生成脱敏报告 → sha256 → 追加 reports/<id>.meta.json(审计留痕)。
// 每次复核通过调用一次;多条签名按时间累积(复核链可回放)。
func (g *Generator) Sign(ctx context.Context, challengeID, reviewer string) (*Signature, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if reviewer == "" {
		return nil, fmt.Errorf("report: sign 需要复核人(reviewer)")
	}
	rep, err := g.generateLocked(ctx, challengeID, true)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(rep.Markdown))
	sig := &Signature{
		ChallengeID: challengeID, Reviewer: reviewer,
		SignedAt: time.Now().Unix(), ReportHash: hex.EncodeToString(sum[:]), Redacted: true,
	}

	dir := filepath.Join(g.WorkDir, "reports")
	if os.MkdirAll(dir, 0o755) != nil {
		return nil, fmt.Errorf("report: mkdir reports: %w", err)
	}
	metaPath := filepath.Join(dir, challengeID+".meta.json")
	var sigs []Signature
	if b, rerr := os.ReadFile(metaPath); rerr == nil {
		_ = json.Unmarshal(b, &sigs)
	}
	sigs = append(sigs, *sig)
	data, merr := json.MarshalIndent(sigs, "", "  ")
	if merr != nil {
		return nil, merr
	}
	if err := os.WriteFile(metaPath, data, 0o644); err != nil {
		return nil, err
	}
	sigPath := metaPath

	if g.Sink != nil {
		g.Sink.Emit(event.Event{Kind: event.KindReport, ChallengeID: challengeID,
			Payload: map[string]any{"action": "sign", "reviewer": reviewer,
				"report_hash": sig.ReportHash, "path": sigPath}})
	}
	return sig, nil
}

// ------------------------------------------------------------------ 05 §3 脱敏(实现见 redact.go)

// refLabel 生成证据标签(带存在性校验标记由 ValidateEvidence 统一处理)。
func refLabel(ref string) string {
	if ref == "" {
		return "(无)"
	}
	return ref
}

func eventSummary(e event.Event) string {
	switch e.Kind {
	case event.KindTurnStart:
		return "turn start"
	case event.KindToolCallStart:
		if p, ok := e.Payload.(map[string]any); ok {
			return fmt.Sprintf("tool: %v", p["name"])
		}
	case event.KindToolCallResult:
		if p, ok := e.Payload.(map[string]any); ok {
			err := ""
			if e, _ := p["error"].(bool); e {
				err = " (error)"
			}
			return fmt.Sprintf("result: %v%s", p["name"], err)
		}
	case event.KindSubmission:
		if p, ok := e.Payload.(map[string]any); ok {
			return fmt.Sprintf("submission → %v", p["result"])
		}
	case event.KindFinding:
		if p, ok := e.Payload.(map[string]any); ok {
			return fmt.Sprintf("finding: %v", truncateStr(fmt.Sprint(p["text"]), 60))
		}
	case event.KindTraffic:
		if p, ok := e.Payload.(map[string]any); ok {
			return fmt.Sprintf("traffic %v %v%v", p["method"], p["host"], p["path"])
		}
	case event.KindSteer:
		return "steer"
	case event.KindTermination:
		return "termination"
	case event.KindApproval:
		if p, ok := e.Payload.(map[string]any); ok {
			return fmt.Sprintf("approval %v → %v", p["tool"], p["status"])
		}
	case event.KindReport:
		return "report generated"
	}
	return ""
}

func shortTitle(s string) string {
	r := []rune(s)
	if len(r) > 40 {
		return string(r[:40]) + "..."
	}
	return s
}

func escapePipe(s string) string {
	return strings.ReplaceAll(s, "|", "\\|")
}

func timeStr(ts int64) string {
	if ts == 0 {
		return "-"
	}
	return time.Unix(ts, 0).Format("15:04:05")
}

func truncateStr(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}
