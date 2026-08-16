package constraint

import (
	"context"

	"scopeforge/internal/blackboard"
	"scopeforge/internal/event"
)

// 目标形态常量(docs/phase2/02 §2.2)。
// 阶段 2.11 定案(阶段二取代阶段一):ctf 形态与平台抽象层一并删除,
// 提交语义由 skill 卡承载;goal_shape 只剩 coverage | breach。
const (
	GoalShapeCoverage = "coverage" // 漏洞清单:覆盖矩阵收敛终止(默认)
	GoalShapeBreach   = "breach"   // 目标状态:goal 达成或路径闭合终止
)

// Interest 是客户兴趣过滤(docs/phase2/04 §3.2/§5.2):
// 覆盖度收敛判定与报告收录只对 interest 命中的格子生效
// (如企业 B 只收敛高危格子:interest.severity_min=high)。
type Interest struct {
	SeverityMin string   `json:"severity_min"` // "" = 不限;critical|high|medium|low|info
	CWEs        []string `json:"cwes"`         // 空 = 不限;CWE 集合(命中任一即通过)
}

var severityRank = map[string]int{
	"critical": 5, "high": 4, "medium": 3, "low": 2, "info": 1,
}

// SeverityRank 返回严重级排名(critical=5 ... info=1);未知值返回 false。
func SeverityRank(s string) (int, bool) {
	r, ok := severityRank[s]
	return r, ok
}

// Match 判定 (severity, cwe) 是否命中兴趣。
func (i Interest) Match(severity, cwe string) bool {
	if i.SeverityMin != "" {
		if severityRank[severity] < severityRank[i.SeverityMin] {
			return false
		}
	}
	if len(i.CWEs) > 0 {
		for _, c := range i.CWEs {
			if c == cwe {
				return true
			}
		}
		return false
	}
	return true
}

// Policy 是终止策略(docs/phase2/02 §2.5 契约 v2,层 2 声明式配置)。
// 三路 OR 的"②"按 GoalShape 选择实现:
//
//	coverage → 覆盖矩阵收敛(ConvergenceEvaluator,默认)
//	breach   → goal_reached / space_closed(GoalVerifier)
type Policy struct {
	GoalShape      string   // coverage | breach
	Interest       Interest // 覆盖度收敛判定的过滤(企业 B 只收敛高危)
	MaxConvergence int      // 覆盖度收敛检查次数上限(<=0 不限;达上限后停止检查,预算兜底)
}

// ConvergenceEvaluator 是 coverage 形态的收敛判定器(docs/phase2/02 §2.3)。
// 覆盖矩阵在 internal/coverage(切片 3)实现;nil = 该路跳过(由预算/穷尽声明兜底)。
type ConvergenceEvaluator interface {
	IsConverged(ctx context.Context, challengeID string, interest Interest) (bool, string)
}

// GoalVerifier 是 breach 形态的目标判定器(docs/phase2/02 §2.3)。
// goal 断言必须经独立验证器确认(平台回执/命令输出验证),模型自述不采信。
// 可达状态图在切片 2/3 实现;nil = 该路跳过。
type GoalVerifier interface {
	IsGoalReached(ctx context.Context, taskID string) (bool, string)
	IsSpaceClosed(ctx context.Context, taskID string) (bool, string)
}

// FocusedTaskEvaluator 是聚焦任务的完成判定器(08 §3 缺失④)。
// 聚焦模式下,指定方向(CWE×Asset×Endpoint)的格子 confirmed 即视为任务完成。
// nil = 聚焦未启用。
type FocusedTaskEvaluator interface {
	IsFocusedCellConfirmed(ctx context.Context, challengeID string, interest Interest) (bool, string)
}

// TerminationHub 是三路 OR 终止判定(docs/03 §5 + docs/phase2/02 §2):
//
//	① 按 goal_shape 分叉(coverage 收敛 / breach 目标达成或路径闭合)
//	② BudgetMeter 超限(token / turn / 美元)
//
// 模型自述"做完了"不构成停止条件;①② 全不满足 → 继续(进入 stale 观察期)。
// 提交 accepted 只记账,不终止(02 §2.2 ②' 语义)。
// 并发:ShouldStop 由调度 tick 单 goroutine 调用;SetPolicy/SetConvergence/
// SetGoalVerifier 仅在装配期调用,运行期不得修改,否则需加锁。
type TerminationHub struct {
	board            *blackboard.Blackboard
	meter            *Meter
	sink             event.Sink
	policy           Policy
	conv             ConvergenceEvaluator
	verifier         GoalVerifier
	focused          FocusedTaskEvaluator // 聚焦任务完成判定(08 §3)
	convCks          int  // 收敛判定累计次数(MaxConvergence 上限)
	convLimitEmitted bool // 收敛上限提示已发(防事件刷屏)
}

// NewTerminationHub 构建终止判定,默认 coverage 语义(覆盖矩阵收敛)。
func NewTerminationHub(board *blackboard.Blackboard, meter *Meter, sink event.Sink) *TerminationHub {
	return NewTerminationHubWithPolicy(board, meter, sink, Policy{GoalShape: GoalShapeCoverage})
}

// NewTerminationHubWithPolicy 按策略构建终止判定。
func NewTerminationHubWithPolicy(board *blackboard.Blackboard, meter *Meter, sink event.Sink, policy Policy) *TerminationHub {
	if sink == nil {
		sink = event.Discard
	}
	if policy.GoalShape == "" {
		policy.GoalShape = GoalShapeCoverage
	}
	return &TerminationHub{board: board, meter: meter, sink: sink, policy: policy}
}

// SetPolicy 动态切换终止策略(任务级 task_profile 注入)。
func (h *TerminationHub) SetPolicy(p Policy) {
	h.policy = p
}

// SetConvergence 注入 coverage 收敛判定器(覆盖矩阵,切片 3 接入)。
func (h *TerminationHub) SetConvergence(e ConvergenceEvaluator) { h.conv = e }

// SetGoalVerifier 注入 breach 目标判定器(切片 2/3 接入)。
func (h *TerminationHub) SetGoalVerifier(v GoalVerifier) { h.verifier = v }

// SetFocused 注入聚焦任务完成判定器(08 §3)。
func (h *TerminationHub) SetFocused(e FocusedTaskEvaluator) { h.focused = e }

// Policy 返回当前策略(只读视图)。
func (h *TerminationHub) Policy() Policy { return h.policy }

// Reason 是终止原因。
type Reason string

const (
	ReasonNone              Reason = ""
	ReasonBudget            Reason = "budget_exceeded"
	ReasonCoverageConverged Reason = "coverage_converged"
	ReasonGoalReached       Reason = "goal_reached"
	ReasonSpaceClosed       Reason = "space_closed"
	ReasonTaskDone          Reason = "task_done" // 聚焦任务:指定格子已确认(08 §3 缺失④)
)

// ShouldStop 三路 OR 判定。返回 (是否停, 原因)。
func (h *TerminationHub) ShouldStop(ctx context.Context, challengeID string) (bool, Reason, error) {
	// 0. 聚焦任务(08 §3):指定格子 confirmed → 任务完成。
	// 这条路径优先于覆盖收敛——聚焦任务只有1个格子,不需要全矩阵收敛。
	if h.focused != nil {
		if ok, _ := h.focused.IsFocusedCellConfirmed(ctx, challengeID, h.policy.Interest); ok {
			return true, ReasonTaskDone, nil
		}
	}
	// ① 按 goal_shape 分叉(coverage 默认)
	switch h.policy.GoalShape {
	case GoalShapeBreach:
		if h.verifier != nil {
			if ok, _ := h.verifier.IsGoalReached(ctx, challengeID); ok {
				return true, ReasonGoalReached, nil
			}
			if ok, _ := h.verifier.IsSpaceClosed(ctx, challengeID); ok {
				return true, ReasonSpaceClosed, nil
			}
		}
	default: // coverage(默认形态)
		if h.conv != nil {
			if h.policy.MaxConvergence > 0 && h.convCks >= h.policy.MaxConvergence {
				// 达到检查上限:停止收敛检查,由预算/穷尽声明兜底(仅提示一次)
				if !h.convLimitEmitted {
					h.convLimitEmitted = true
					h.sink.Emit(event.Event{Kind: event.KindTick, ChallengeID: challengeID,
						Payload: map[string]any{"note": "coverage convergence check limit reached, budget/exhaustion takes over"}})
				}
			} else {
				h.convCks++
				ok, _ := h.conv.IsConverged(ctx, challengeID, h.policy.Interest)
				if ok {
					return true, ReasonCoverageConverged, nil
				}
			}
		}
	}
	// ② 预算熔断
	if h.meter != nil {
		if ok, _ := h.meter.Check(challengeID); !ok {
			return true, ReasonBudget, nil
		}
	}
	return false, ReasonNone, nil
}
