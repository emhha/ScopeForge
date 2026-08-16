package scheduler

// 任务聚焦(阶段 2.21):任务入口指定目标 URL(FocusTarget)时启用聚焦模式——
// Scout/Executor 产出的攻击面/意图只保留目标 host + 路径前缀,
// 防"只测一个端点却全站测绘/端口扫描"的扩散(用户实测暴露)。
//
// 过滤是确定性硬约束(提示词纪律之外的兜底):调度器在 applyContract 时
// 对 Worker 契约做 focusFilter,越界条目丢弃并记 hint 事件(可审计)。

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"scopeforge/internal/blackboard"
	"scopeforge/internal/coverage"
	"scopeforge/internal/cwe"
	"scopeforge/internal/dispatcher"
	"scopeforge/internal/event"
)

// focusTarget 是解析后的聚焦目标。
type focusTarget struct {
	Raw        string // 原始 URL(含 query 也会被解析,但匹配只取 path)
	Host       string // host:port(如 192.168.81.167:3000;无端口 = host)
	HostOnly   string // 纯 host(192.168.81.167)
	PathPrefix string // 路径前缀(如 /rest/products/search;根路径 = "/")
}

// parseFocusTarget 解析聚焦目标 URL。
// 空输入返回 nil(聚焦关闭)。非法 URL 返回 error(入口 fail-fast)。
func parseFocusTarget(raw string) (*focusTarget, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if u.Host == "" {
		return nil, nil
	}
	ft := &focusTarget{
		Raw:        raw,
		Host:       u.Host,
		HostOnly:   u.Hostname(),
		PathPrefix: cwe.NormalizeEndpoint(u.Path),
	}
	if ft.PathPrefix == "" {
		ft.PathPrefix = "/"
	}
	return ft, nil
}

// enabled 判断聚焦模式是否启用。
func (ft *focusTarget) enabled() bool { return ft != nil && ft.Host != "" }

// matchEndpoint 端点是否在聚焦路径内(归一化后前缀匹配;根路径 = 全部)。
func (ft *focusTarget) matchEndpoint(endpoint string) bool {
	if endpoint == "" {
		return true // 无端点信息不拦截(保底放行,防误杀)
	}
	e := cwe.NormalizeEndpoint(endpoint)
	if ft.PathPrefix == "/" {
		return true // 只聚焦主机,不聚焦路径
	}
	if e == "/" {
		return false
	}
	return strings.HasPrefix(e, ft.PathPrefix)
}

// matchAsset 资产主机是否为目标主机(host 与 host:port 均算)。
func (ft *focusTarget) matchAsset(asset string) bool {
	if asset == "" {
		return true // 无资产信息不拦截
	}
	a := strings.ToLower(cwe.NormalizeAsset(asset))
	if a == "" {
		return true
	}
	if i := strings.IndexByte(a, ':'); i >= 0 {
		a = a[:i] // 去端口比较
	}
	return a == ft.HostOnly
}

// focusFilter 对 Worker 契约做聚焦过滤(返回副本,不修改原契约)。
// 越界条目丢弃;过滤计数经 hint 事件上报(可审计)。
func (s *Scheduler) focusFilter(contract *dispatcher.WorkerContract, ft *focusTarget, challengeID string) *dispatcher.WorkerContract {
	if contract == nil || !ft.enabled() {
		return contract
	}
	out := *contract // 浅拷贝,切片重建
	dropAS, dropIt, dropF := 0, 0, 0

	// attack_surface:资产主机 + 端点前缀都必须匹配
	as := make([]dispatcher.AttackSurfaceItem, 0, len(contract.AttackSurface))
	for _, it := range contract.AttackSurface {
		if ft.matchAsset(it.Asset) && ft.matchEndpoint(it.Endpoint) {
			as = append(as, it)
		} else {
			dropAS++
		}
	}
	out.AttackSurface = as

	// new_intents:带 target 的按端点匹配;无 target 的纯文本意图丢弃
	// (聚焦模式下不扩散未知方向——这是防扩散的核心闸门)
	its := make([]dispatcher.IntentIn, 0, len(contract.NewIntents))
	for _, it := range contract.NewIntents {
		if it.Target != "" && ft.matchEndpoint(it.Target) {
			its = append(its, it)
		} else {
			dropIt++
		}
	}
	out.NewIntents = its

	// findings:带结构化键且越界的丢弃(防越界漏洞/观测进黑板引导扩散);
	// 无结构化键的纯文本观测保留(技术栈等无害信息)
	fs := make([]dispatcher.Finding, 0, len(contract.Findings))
	for _, f := range contract.Findings {
		if f.Endpoint != "" && !ft.matchEndpoint(f.Endpoint) {
			dropF++
			continue
		}
		if f.Asset != "" && !ft.matchAsset(f.Asset) {
			dropF++
			continue
		}
		fs = append(fs, f)
	}
	out.Findings = fs

	if dropAS+dropIt+dropF > 0 {
		s.sink.Emit(event.Event{Kind: event.KindCheckpoint, ChallengeID: challengeID,
			Payload: map[string]any{
				"action":           "focus_filter",
				"focus":            ft.Raw,
				"dropped_surface":  dropAS,
				"dropped_intents":  dropIt,
				"dropped_findings": dropF,
				"note":             "聚焦模式:越界攻击面/意图/发现已丢弃(任务仅检测指定端点)",
			}})
	}
	return &out
}

// focus 解析任务聚焦目标(惰性缓存;非法 URL 视为未启用)。
// 聚焦模式启用条件:TaskProfile.FocusTarget 非空且可解析。
func (s *Scheduler) focus() *focusTarget {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.focusParsed {
		return s.focusCached
	}
	s.focusParsed = true
	ft, err := parseFocusTarget(s.taskProfile.FocusTarget)
	if err != nil {
		s.sink.Emit(event.Event{Kind: event.KindError, ChallengeID: s.targetText(),
			Payload: map[string]any{"error": fmt.Sprintf("focus target %q: %v(聚焦未启用)", s.taskProfile.FocusTarget, err)}})
		ft = nil
	}
	s.focusCached = ft
	return ft
}

// seedFocus Scout 契约连续失败重试耗尽后的确定性兜底(阶段 2.21):
// 聚焦目标直接建攻击面格子 + 初始 intent——不再依赖 LLM 契约格式,
// Executor 下个 tick 经正常认领执行("Scout 只产生一个格子"的保底实现)。
// 返回 true = 本次新建了种子(调用方本 tick 结束,下 tick 认领);
// false = 聚焦未启用或种子已存在(调用方继续走正常认领分支)。
func (s *Scheduler) seedFocus(challengeID string) bool {
	ft := s.focus()
	if ft == nil || !ft.enabled() {
		return false
	}
	text := fmt.Sprintf("测试 %s 的注入/XSS/越权(任务指定端点;Scout 契约失败后的确定性种子)", ft.PathPrefix)
	// 直接经 board.AddIntent:ReportIntent 会吞掉 ErrNoChange,
	// 无法区分"新建"与"已存在"(已存在时必须返回 false 走正常认领分支)。
	if _, err := s.board.AddIntent(challengeID, text, 0.9, "scheduler-seed",
		blackboard.IntentIn{Target: ft.PathPrefix, Approach: "probe"}); err != nil {
		if errors.Is(err, blackboard.ErrNoChange) {
			return false // 种子已存在 → 交由正常认领分支派 explore
		}
		s.sink.Emit(event.Event{Kind: event.KindError, ChallengeID: challengeID,
			Payload: map[string]any{"error": fmt.Sprintf("focus seed intent: %v", err)}})
		return false // 建失败 → 让调度走其他分支(防 tick 死循环)
	}
	if s.cov != nil {
		_ = s.cov.EnsureOpen(challengeID, "", ft.HostOnly, ft.PathPrefix, coverage.FormParam)
	}
	s.sink.Emit(event.Event{Kind: event.KindCheckpoint, ChallengeID: challengeID,
		Payload: map[string]any{
			"action": "focus_seed",
			"focus":  ft.Raw,
			"note":   "Scout 契约失败耗尽重试后,由聚焦目标确定性建格(不依赖 LLM 输出)",
		}})
	return true
}
