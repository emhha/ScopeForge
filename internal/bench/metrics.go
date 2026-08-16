package bench

import (
	"scopeforge/internal/blackboard"
	"scopeforge/internal/coverage"
)

// Metrics 是单任务评测指标(docs/phase2/02 §3.1)。
type Metrics struct {
	GoalShape string `json:"goal_shape"` // coverage | breach

	// coverage 主指标
	Recall     float64  `json:"recall"`       // 发现的预埋漏洞数 / 预埋总数
	Found      []string `json:"found_seeds"`  // 命中的种子 ID
	Missed     []string `json:"missed_seeds"` // 未发现的种子 ID
	Coverage   float64  `json:"coverage"`     // 覆盖矩阵终态格子 / 全部格子
	FPR        float64  `json:"fpr"`          // false_positive 回执数 / 提交总数
	Efficiency float64  `json:"efficiency"`   // 成本 / 召回数(每召回 1 漏洞的 USD)
	Converged  bool     `json:"converged"`    // 正常收敛终止(非预算硬断)

	// breach 主指标
	GoalReached bool `json:"goal_reached"` // 目标状态经独立验证器确认
	PathLen     int  `json:"path_len"`     // 达成目标的状态转移边数

	// 账本明细(审计/报告)
	TotalSubmissions int `json:"total_submissions"`
	Accepted         int `json:"accepted"`
	Duplicate        int `json:"duplicate"`
	FalsePositive    int `json:"false_positive"`
	Rejected         int `json:"rejected"`
	Submitted        int `json:"submitted"`

	CostUSD float64 `json:"cost_usd"`
	Turns   int     `json:"turns"`
}

// Thresholds 是验收判定阈值(README 验收底线 1)。
type Thresholds struct {
	RecallMin float64 // recall ≥ 60% 通过
	FPRMax    float64 // fpr ≤ 20% 通过
}

// DefaultThresholds 返回默认判定阈值。
func DefaultThresholds() Thresholds {
	return Thresholds{RecallMin: 0.6, FPRMax: 0.2}
}

// Evaluate 从账本 + 覆盖矩阵计算单任务指标(02 §3.1)。
//   - recall = accepted 且命中预埋的种子数 / 预埋总数
//   - fpr = false_positive 回执数 / 提交总数(含未回执 submitted,02 §3.1)
//   - coverage = 终态格子(confirmed/dead/skipped/claimed) / 全部格子
//   - efficiency = costUSD / 召回数(recall 分母为 0 时记 0)
//   - converged = 非预算硬断终止(由 runner 传入 Reason)
func Evaluate(t *SRCTarget, vulns []blackboard.Vulnerability, cells []coverage.Cell, costUSD float64, turns int, converged bool) Metrics {
	j := NewJudger(t)
	found := j.FoundSeeds(vulns)
	missed := MissedSeeds(t, found)

	recall := 0.0
	if len(t.Seeds) > 0 {
		recall = float64(len(found)) / float64(len(t.Seeds))
	}

	stats := ledgerStats(vulns)

	fpr := 0.0
	if stats.Total > 0 {
		fpr = float64(stats.FalsePositive) / float64(stats.Total)
	}

	efficiency := 0.0
	if len(found) > 0 {
		efficiency = costUSD / float64(len(found))
	}

	return Metrics{
		GoalShape:        "coverage",
		Recall:           recall,
		Found:            found,
		Missed:           missed,
		Coverage:         CoverageRate(cells),
		FPR:              fpr,
		Efficiency:       efficiency,
		Converged:        converged,
		TotalSubmissions: stats.Total,
		Accepted:         stats.Accepted,
		Duplicate:        stats.Duplicate,
		FalsePositive:    stats.FalsePositive,
		Rejected:         stats.Rejected,
		Submitted:        stats.Submitted,
		CostUSD:          costUSD,
		Turns:            turns,
	}
}

// Pass 按阈值判定通过与否(README 验收底线 1)。
func (m *Metrics) Pass(th Thresholds) (recallOK, fprOK bool) {
	return m.Recall >= th.RecallMin, m.FPR <= th.FPRMax
}

// CoverageRate 覆盖度:终态格子(confirmed/dead/skipped/claimed)占比。
// 无格子时返回 0(攻击面未测绘,不算收敛——02 §2.3 no_attack_surface)。
func CoverageRate(cells []coverage.Cell) float64 {
	if len(cells) == 0 {
		return 0
	}
	terminal := 0
	for _, c := range cells {
		switch c.Status {
		case coverage.StatusConfirmed, coverage.StatusDead, coverage.StatusSkipped, coverage.StatusClaimed:
			terminal++
		}
	}
	return float64(terminal) / float64(len(cells))
}

// ledgerStats 从账本统计回执分布。
func ledgerStats(vulns []blackboard.Vulnerability) blackboard.LedgerStats {
	var st blackboard.LedgerStats
	for _, v := range vulns {
		st.Total++
		switch v.Status {
		case blackboard.LedgerAccepted:
			st.Accepted++
		case blackboard.LedgerDuplicate:
			st.Duplicate++
		case blackboard.LedgerFalsePositive:
			st.FalsePositive++
		case blackboard.LedgerRejected:
			st.Rejected++
		default:
			st.Submitted++
		}
	}
	return st
}
