package executor

import (
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"strings"
	"sync"
)

// PermissionMode 是权限三模式(docs/02 §3.4)。
type PermissionMode string

const (
	ModeAuto PermissionMode = "auto" // 自动放行只读+安全命令,保留规则
	ModeYolo PermissionMode = "yolo" // 跳过普通确认(仅授权靶场模式,配置显式开启)
)

// Decision 是权限裁决。
type Decision int

const (
	Deny Decision = iota
	Allow
)

func (d Decision) String() string {
	switch d {
	case Deny:
		return "deny"
	case Allow:
		return "allow"
	default:
		return "unknown"
	}
}

// Gate 是确定性权限钩子(A4:提示词与代码钩子双写)。
// 规则(docs/02 §3.4 渗透场景补充 + docs/03 §6.3 ScopeGate):
//   - 命令黑名单 → 硬拦截
//   - 外发流量白名单:目标域/IP/CIDR(M1 ScopeGate 语义);其余默认拒绝
//   - Episode 预算:同一操作 3 次失败自动停
type Gate struct {
	Mode PermissionMode
	// Blacklist 命令黑名单(正则,匹配完整命令文本)。
	Blacklist []string
	// DomainAllowlist 域名白名单(支持 *.example.com 通配、子域匹配、IP、CIDR)。
	DomainAllowlist []string

	mu          sync.Mutex
	failCount   map[string]int
	maxFailures int
	blackRe     []*regexp.Regexp
	domainRe    []*regexp.Regexp
	ips         []netip.Addr
	cidrs       []netip.Prefix
	deniedRe    []*regexp.Regexp // 域黑名单(白名单内再排除,04 §1 exclusions)
}

// NewGate 构建权限门。maxFailures<=0 时默认 3。
func NewGate(mode PermissionMode, blacklist, domainAllowlist []string) (*Gate, error) {
	g := &Gate{
		Mode:            mode,
		Blacklist:       blacklist,
		DomainAllowlist: domainAllowlist,
		failCount:       map[string]int{},
		maxFailures:     3,
	}
	for _, b := range blacklist {
		re, err := regexp.Compile(b)
		if err != nil {
			return nil, fmt.Errorf("executor: bad blacklist pattern %q: %w", b, err)
		}
		g.blackRe = append(g.blackRe, re)
	}
	for _, d := range domainAllowlist {
		if err := g.addTarget(d); err != nil {
			return nil, fmt.Errorf("executor: bad domain pattern %q: %w", d, err)
		}
	}
	return g, nil
}

// AddTargets 追加外发白名单目标(challenge 启动时解析的目标 URL/网段)。
// 支持:域名、*.通配、IP、CIDR。
func (g *Gate) AddTargets(targets []string) error {
	for _, t := range targets {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		// 提取 host(URL 或裸 host)
		host := gateHost(t)
		if err := g.addTarget(host); err != nil {
			return err
		}
	}
	return nil
}

// addTarget 添加单条白名单规则。
func (g *Gate) addTarget(d string) error {
	d = strings.TrimSpace(d)
	if d == "" {
		return nil
	}
	switch {
	case strings.HasPrefix(d, "*."):
		pattern := strings.ReplaceAll(regexp.QuoteMeta(strings.TrimPrefix(d, "*.")), `\*`, `.*`)
		re, err := regexp.Compile(`(^|\.)` + pattern + `$`)
		if err != nil {
			return err
		}
		g.domainRe = append(g.domainRe, re)
		g.DomainAllowlist = append(g.DomainAllowlist, d) // 与 domainRe 同步(domainAllowed 按 i 索引)
	case strings.Contains(d, "/"):
		p, err := netip.ParsePrefix(d)
		if err != nil {
			return err
		}
		g.cidrs = append(g.cidrs, p.Masked())
	case isIPAddr(d):
		ip, err := netip.ParseAddr(d)
		if err != nil {
			return err
		}
		g.ips = append(g.ips, ip.Unmap())
	default:
		pattern := strings.ReplaceAll(regexp.QuoteMeta(d), `\*`, `.*`)
		re, err := regexp.Compile(`^` + pattern + `$`)
		if err != nil {
			return err
		}
		g.domainRe = append(g.domainRe, re)
		g.DomainAllowlist = append(g.DomainAllowlist, d) // 与 domainRe 同步(domainAllowed 按 i 索引)
	}
	return nil
}

// AddDenied 追加外发域黑名单(白名单内再排除,04 §1 Constraints.exclusions)。
// 支持域名/通配/IP/CIDR(与 AddTargets 同语法);命中 → 确定性拒绝。
func (g *Gate) AddDenied(patterns []string) error {
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if err := g.addDenied(p); err != nil {
			return fmt.Errorf("executor: bad denied pattern %q: %w", p, err)
		}
	}
	return nil
}

// addDenied 添加单条黑名单规则。
func (g *Gate) addDenied(d string) error {
	switch {
	case strings.HasPrefix(d, "*."):
		pattern := strings.ReplaceAll(regexp.QuoteMeta(strings.TrimPrefix(d, "*.")), `\*`, `.*`)
		re, err := regexp.Compile(`(^|\.)` + pattern + `$`)
		if err != nil {
			return err
		}
		g.deniedRe = append(g.deniedRe, re)
	default:
		pattern := strings.ReplaceAll(regexp.QuoteMeta(d), `\*`, `.*`)
		re, err := regexp.Compile(`^` + pattern + `$`)
		if err != nil {
			return err
		}
		g.deniedRe = append(g.deniedRe, re)
	}
	return nil
}

// Check 裁决一次调用(executable 为命令文本或工具名)。
func (g *Gate) Check(executable string, readOnly bool, args map[string]any) (Decision, string) {
	// 1. 命令黑名单硬拦截(任何模式)
	for _, re := range g.blackRe {
		if re.MatchString(executable) {
			return Deny, fmt.Sprintf("command matches blacklist pattern %q", re.String())
		}
	}
	// 2. 域名白名单(外发流量);云元数据端点永远拒绝(docs/04 §5.3)
	if u, ok := args["url"].(string); ok && u != "" {
		if MetaDataBlocked(u) {
			return Deny, "metadata endpoint access denied (169.254.169.254 / metadata.google.internal)"
		}
		// 域黑名单优先(白名单内再排除,04 §1 exclusions → 禁区目标确定性拒绝)
		host := extractHost(u)
		for _, re := range g.deniedRe {
			if re.MatchString(host) {
				return Deny, fmt.Sprintf("domain %q in exclusions (denied)", host)
			}
		}
		if !g.domainAllowed(u) {
			return Deny, fmt.Sprintf("domain %q not in allowlist", u)
		}
	}
	// 3. Episode 预算
	fp := fingerprint(executable, args)
	g.mu.Lock()
	fails := g.failCount[fp]
	g.mu.Unlock()
	if fails >= g.maxFailures {
		return Deny, fmt.Sprintf("episode budget exhausted: operation failed %d times", fails)
	}
	switch g.Mode {
	case ModeYolo:
		return Allow, "yolo mode"
	default:
		// auto(默认):黑名单/白名单/预算已在上方拦截,其余自动放行。
		// 06.14 起任务不再寻求工具审批(用户决策:彻底删除审批机制)。
		return Allow, "auto mode"
	}
}

// RecordFailure / RecordSuccess 维护 Episode 预算。
func (g *Gate) RecordFailure(executable string, args map[string]any) {
	fp := fingerprint(executable, args)
	g.mu.Lock()
	g.failCount[fp]++
	g.mu.Unlock()
}

func (g *Gate) RecordSuccess(executable string, args map[string]any) {
	fp := fingerprint(executable, args)
	g.mu.Lock()
	delete(g.failCount, fp)
	g.mu.Unlock()
}

func (g *Gate) domainAllowed(u string) bool {
	host := extractHost(u)
	if host == "" {
		return true
	}
	host = strings.ToLower(host)
	// IP/CIDR 匹配
	if ip, err := netip.ParseAddr(host); err == nil {
		ip = ip.Unmap()
		for _, a := range g.ips {
			if a == ip {
				return true
			}
		}
		for _, c := range g.cidrs {
			if c.Contains(ip) {
				return true
			}
		}
	}
	for i, re := range g.domainRe {
		if re.MatchString(host) {
			return true
		}
		if !strings.Contains(g.DomainAllowlist[i], "*") {
			parts := strings.Split(host, ".")
			for j := 1; j < len(parts); j++ {
				if re.MatchString(strings.Join(parts[j:], ".")) {
					return true
				}
			}
		}
	}
	return false
}

func extractHost(u string) string {
	u = strings.TrimSpace(u)
	u = strings.TrimPrefix(u, "https://")
	u = strings.TrimPrefix(u, "http://")
	if i := strings.IndexAny(u, "/?#"); i >= 0 {
		u = u[:i]
	}
	if i := strings.IndexByte(u, ':'); i >= 0 {
		u = u[:i]
	}
	return strings.ToLower(u)
}

func fingerprint(executable string, args map[string]any) string {
	var b strings.Builder
	b.WriteString(executable)
	if args != nil {
		if v, ok := args["command"]; ok {
			b.WriteString("|")
			b.WriteString(fmt.Sprintf("%v", v))
		}
		if v, ok := args["path"]; ok {
			b.WriteString("|")
			b.WriteString(fmt.Sprintf("%v", v))
		}
	}
	return b.String()
}

// gateHost 从 URL/裸 host 提取主机部分(与 constraint.ScopeGate 同语义)。
func gateHost(t string) string {
	t = strings.TrimSpace(t)
	if i := strings.Index(t, "://"); i >= 0 {
		t = t[i+3:]
	}
	if i := strings.IndexByte(t, '/'); i >= 0 {
		t = t[:i]
	}
	if h, _, err := net.SplitHostPort(t); err == nil {
		return h
	}
	return t
}

func isIPAddr(s string) bool {
	return net.ParseIP(s) != nil
}

// MetaDataBlocked 云元数据端点永远拒绝(docs/04 §5.3 网络边界)。
func MetaDataBlocked(u string) bool {
	host := extractHost(u)
	return host == "169.254.169.254" || host == "metadata.google.internal"
}

