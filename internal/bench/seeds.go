// Package bench 是阶段二评测体系(docs/phase2/02 §3)。
//
// 阶段 2.11 定案:阶段二取代阶段一,平台抽象层与 ctf 形态删除;评测随之重建:
//   - mock SRC 靶场(02 §3.2):预埋 N 个漏洞(按 CWE×端点分布),recall 基准
//   - 回执判定器:模拟平台回执(命中→accepted/重复→duplicate/未命中→false_positive),
//     只作离线闭环评测组件,不做平台对接(阶段 2.9/2.11 定案)
//   - 指标:recall/fpr/coverage/efficiency/convergence(02 §3.1)
//   - 判定:recall ≥ 60% 通过、fpr ≤ 20% 通过(README 验收底线 1)
package bench

import (
	"sort"

	"scopeforge/internal/blackboard"
	"scopeforge/internal/cwe"
)

// Seed 是 mock SRC 靶场预埋漏洞(02 §3.2:按 CWE×端点分布)。
type Seed struct {
	ID       string // 唯一标识(如 "v1")
	CWE      string // CWE-89 等
	Asset    string // shop.example.com
	Endpoint string // /login(可空 = 资产级漏洞)
	Severity string // critical|high|medium|low|info
	Title    string // 漏洞标题(评测报告可读)
}

// key 返回归一化匹配键(与幂等键双轨同规:03 §1.2)。
func (s *Seed) key() string {
	return seedKey(s.CWE, s.Asset, s.Endpoint)
}

func seedKey(cweID, asset, endpoint string) string {
	n := cweID
	if norm, ok := cwe.NormalizeCWE(cweID); ok {
		n = norm
	}
	return n + "|" + cwe.NormalizeAsset(asset) + "|" + cwe.NormalizeEndpoint(endpoint)
}

// SRCTarget 是 mock SRC 靶场:预埋漏洞集合(recall 基准)。
type SRCTarget struct {
	Name  string
	Seeds []Seed
}

// MatchSeed 用归一化键 (cwe, asset, endpoint) 匹配预埋漏洞。
// 未命中返回 nil。
func (t *SRCTarget) MatchSeed(cweID, asset, endpoint string) *Seed {
	k := seedKey(cweID, asset, endpoint)
	for i := range t.Seeds {
		if t.Seeds[i].key() == k {
			return &t.Seeds[i]
		}
	}
	return nil
}

// DefaultSRCTarget 返回默认预埋靶场(03 §5 验收用:3 漏洞按 CWE×端点分布)。
// 布局与 03 §3.4 端点形态示例一致:
//   - /login(认证入口):CWE-89 注入 + CWE-79 XSS
//   - /api/order(带参端点):CWE-639 越权
func DefaultSRCTarget() *SRCTarget {
	return &SRCTarget{
		Name: "mock-src-default",
		Seeds: []Seed{
			{ID: "v1", CWE: "CWE-89", Asset: "shop.example.com", Endpoint: "/login", Severity: "high", Title: "登录接口 SQL 注入"},
			{ID: "v2", CWE: "CWE-79", Asset: "shop.example.com", Endpoint: "/login", Severity: "medium", Title: "登录接口存储型 XSS"},
			{ID: "v3", CWE: "CWE-639", Asset: "shop.example.com", Endpoint: "/api/order", Severity: "high", Title: "订单越权访问"},
		},
	}
}

// JuiceShopTarget 返回本地 OWASP Juice Shop 真实靶场的评测预埋集(--set juice,
// docs/phase2/11 §3-④:真实 LLM 端到端 benchmark)。与 mock 靶场的区别:
// 目标真实可达、工具真实执行、漏洞真实存在——recall 度量的是真实挖掘能力,
// 不是 mock 管道正确性。baseURL 形如 http://127.0.0.1:3000。
//
// 种子选型(juice-shop-target skill 卡 §3 的经典挑战,覆盖三种漏洞形态):
//   - unionSqlInjection:/rest/products/search?q= union 注入(CWE-89)
//   - basketAccessChallenge:越权访问他人购物车 /rest/basket/{id}(CWE-639)
//   - scoreBoardChallenge:隐藏 Score Board 页面信息泄露(CWE-200)
func JuiceShopTarget(baseURL string) *SRCTarget {
	asset := cwe.NormalizeAsset(baseURL) // 端口保留:127.0.0.1:3000
	return &SRCTarget{
		Name: "juice-shop",
		Seeds: []Seed{
			{ID: "j1", CWE: "CWE-89", Asset: asset, Endpoint: "/rest/products/search", Severity: "high", Title: "商品搜索 SQL 注入(union)"},
			{ID: "j2", CWE: "CWE-639", Asset: asset, Endpoint: "/rest/basket", Severity: "high", Title: "购物车越权访问(IDOR)"},
			{ID: "j3", CWE: "CWE-200", Asset: asset, Endpoint: "/score-board", Severity: "low", Title: "隐藏页面信息泄露(Score Board)"},
		},
	}
}

// Judger 是回执判定器(模拟平台回执,离线闭环评测组件)。
// 纪律对齐 02 §1.3:accepted → 覆盖矩阵已覆盖(不终止);duplicate → 已覆盖;
// false_positive → 计入误报率,方向可重试。
type Judger struct {
	target   *SRCTarget
	accepted map[string]bool // seed.ID → 已 accepted(去重纪律)
}

// NewJudger 构建判定器。
func NewJudger(t *SRCTarget) *Judger {
	return &Judger{target: t, accepted: map[string]bool{}}
}

// Judge 判定一条账本提交的回执:
//   - 命中预埋且该种子首次 → accepted
//   - 命中预埋但该种子已 accepted → duplicate(重复提交被幂等拦截)
//   - 未命中预埋 → false_positive
//
// platformRef 固定 "mock-src"(离线闭环标识)。
func (j *Judger) Judge(v blackboard.Vulnerability) (status, platformRef string) {
	s := j.target.MatchSeed(v.CWE, v.Asset, v.Endpoint)
	if s == nil {
		return blackboard.LedgerFalsePositive, "mock-src"
	}
	if j.accepted[s.ID] {
		return blackboard.LedgerDuplicate, "mock-src"
	}
	j.accepted[s.ID] = true
	return blackboard.LedgerAccepted, "mock-src"
}

// FoundSeeds 返回账本中已被 accepted 且命中预埋的种子 ID 列表(去重,升序)。
func (j *Judger) FoundSeeds(vulns []blackboard.Vulnerability) []string {
	found := map[string]bool{}
	for _, v := range vulns {
		if v.Status != blackboard.LedgerAccepted {
			continue
		}
		if s := j.target.MatchSeed(v.CWE, v.Asset, v.Endpoint); s != nil {
			found[s.ID] = true
		}
	}
	out := make([]string, 0, len(found))
	for id := range found {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// MissedSeeds 返回未被 found 覆盖的种子 ID 列表(升序)。
func MissedSeeds(t *SRCTarget, found []string) []string {
	have := map[string]bool{}
	for _, id := range found {
		have[id] = true
	}
	var out []string
	for _, s := range t.Seeds {
		if !have[s.ID] {
			out = append(out, s.ID)
		}
	}
	sort.Strings(out)
	return out
}
