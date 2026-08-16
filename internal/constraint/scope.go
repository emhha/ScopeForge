// Package constraint 是约束平面(docs/03 §6):
//
//	ScopeGate    — 目标白名单(域名/IP/CIDR),默认拒绝,外发流量唯一闸门
//	BudgetMeter  — 每题 token/turn/美元 三维熔断(ledger 实时累计)
//	CostLedger   — 按 role×model 记账(每 Usage 事件入账)
//	TerminationHub — 三路 OR 终止判定(平台完成/正分/预算熔断)
package constraint

import (
	"net"
	"net/netip"
	"regexp"
	"strings"
)

// ------------------------------------------------------------------ ScopeGate

// ScopeGate 是目标白名单(docs/03 §6.3):
// challenge 启动时解析目标(域名/URL/内网网段),工具执行前 CheckTarget。
// 规则:白名单匹配放行;未匹配 → deny;LLM 网关与平台端点永远禁止。
type ScopeGate struct {
	domains  []string     // 精确域名(含其子域)
	wildcard []string     // 通配后缀(example.com 形式)
	ips      []netip.Addr // 精确 IP
	cidrs    []netip.Prefix
	denied   []string // 永远禁止的 host(LLM 网关/平台端点)
}

var urlHostRe = regexp.MustCompile(`^(?i)(https?|ftp)://([^/?#]+)`)

// NewScopeGate 解析白名单目标。targets 支持:
//
//	https://target.example.com    → 域名 target.example.com(含子域)
//	example.com                   → 域名(含子域)
//	*.example.com                 → 通配后缀
//	10.10.0.0/24                  → 网段
//	172.16.5.10                   → 精确 IP
func NewScopeGate(targets []string) (*ScopeGate, error) {
	g := &ScopeGate{}
	for _, t := range targets {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		host := extractHost(t)
		switch {
		case strings.HasPrefix(host, "*."):
			g.wildcard = append(g.wildcard, strings.TrimPrefix(host, "*."))
		case strings.Contains(host, "/"):
			p, err := netip.ParsePrefix(host)
			if err != nil {
				return nil, err
			}
			g.cidrs = append(g.cidrs, p.Masked())
		case isIP(host):
			ip, err := netip.ParseAddr(host)
			if err != nil {
				return nil, err
			}
			g.ips = append(g.ips, ip.Unmap())
		default:
			g.domains = append(g.domains, strings.ToLower(host))
		}
	}
	return g, nil
}

// AddDenied 追加永远禁止的 host(LLM 网关/平台端点)。
func (g *ScopeGate) AddDenied(hosts ...string) {
	for _, h := range hosts {
		if h = extractHost(h); h != "" {
			g.denied = append(g.denied, strings.ToLower(h))
		}
	}
}

// CheckTarget 裁决一次外发目标。target 可以是 URL 或裸 host/IP。
func (g *ScopeGate) CheckTarget(target string) (bool, string) {
	host := extractHost(target)
	if host == "" {
		return false, "empty target"
	}
	lower := strings.ToLower(host)
	for _, d := range g.denied {
		if lower == d || strings.HasSuffix(lower, "."+d) {
			return false, "host " + host + " is permanently denied (gateway/platform)"
		}
	}
	if ip, err := netip.ParseAddr(lower); err == nil {
		ip = ip.Unmap()
		for _, a := range g.ips {
			if a == ip {
				return true, "ip allowlisted"
			}
		}
		for _, c := range g.cidrs {
			if c.Contains(ip) {
				return true, "cidr allowlisted"
			}
		}
		return false, "ip " + host + " not in allowlist"
	}
	// 域名:精确 / 通配后缀 / 规则域名子域
	for _, d := range g.domains {
		if lower == d || strings.HasSuffix(lower, "."+d) {
			return true, "domain allowlisted"
		}
	}
	for _, w := range g.wildcard {
		if strings.HasSuffix(lower, "."+w) {
			return true, "wildcard allowlisted"
		}
	}
	return false, "host " + host + " not in allowlist"
}

// extractHost 从 URL 或裸 host 提取主机部分(含端口则剔除)。
func extractHost(t string) string {
	t = strings.TrimSpace(t)
	if m := urlHostRe.FindStringSubmatch(t); m != nil {
		t = m[2]
	}
	// 去掉 userinfo@ 前缀
	if i := strings.LastIndexByte(t, '@'); i >= 0 {
		t = t[i+1:]
	}
	// IPv6 字面量 [::1]:8080
	if strings.HasPrefix(t, "[") {
		if i := strings.IndexByte(t, ']'); i >= 0 {
			return t[1:i]
		}
	}
	if h, _, err := net.SplitHostPort(t); err == nil {
		return h
	}
	return strings.TrimSuffix(t, "/")
}

func isIP(s string) bool {
	return net.ParseIP(s) != nil
}
