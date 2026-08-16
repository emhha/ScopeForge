package scheduler

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"scopeforge/internal/blackboard"
	"scopeforge/internal/breach"
	"scopeforge/internal/constraint"
	"scopeforge/internal/coverage"
	"scopeforge/internal/dispatcher"
	"scopeforge/internal/event"
)

// dispatch 经 Dispatcher 派活(唯一入口)。phase 决定 Operator 的运行阶段
// (recon/explore/reason);Synthesizer 传空 phase(独立收尾语义)。
func (s *Scheduler) dispatch(ctx context.Context, challengeID, workerType string, phase Phase, intentText string, intentID int64, edgeID ...string) string {
	handoff := dispatcher.Handoff{
		ChallengeID:  challengeID,
		IntentID:     intentID,
		Phase:        string(phase),
		SnapshotText: s.snapshotText(challengeID),
	}
	if len(edgeID) > 0 {
		handoff.EdgeID = edgeID[0]
	}
	// 当前 intent 走独立槽位(曾经占用 Completed:worker 既看不到真实
	// 已完成事项——重复派发诱因,又在"不要重复"标题下读到当前任务,
	// 指令语义自相矛盾——docs/phase2/11 §2.5)。
	if intentText != "" {
		handoff.IntentText = intentText
	}
	wid, err := s.disp.Launch(ctx, workerType, handoff)
	if err != nil {
		s.sink.Emit(event.Event{Kind: event.KindError, ChallengeID: challengeID,
			Payload: map[string]any{"error": fmt.Sprintf("launch %s: %v", workerType, err)}})
		return ""
	}
	return wid
}

// maxExhaustionRejects 穷尽声明连续被拒上限:达到后强制收尾,
// 防止模型持续输出无证据 exhausted 造成 token 空转(预算兜底前的最后护栏)。
const maxExhaustionRejects = 3

// maxUnExhaustedRetries 未穷尽重试上限(03 §3.3 防空转):同一 intent 连续
// 无产出无 new_intent 达上限 → 视为方向耗尽(dead),不再重试。
const maxUnExhaustedRetries = 3

// maxReasonDispatches reason worker 单任务派发上限:hyp 未被 resolve 时
// reason 会反复产出新 intent 空转(阶段 2.11 起无平台完成态兜底),达上限
// 后让位 conclude 收尾路径(穷尽声明/强制收尾/预算)。
const maxReasonDispatches = 3

// claimBreachEdge 认领一条 open 转移边并派 explore(03 §3.2 breach 分支)。
// 距 goal 启发式:按"from 节点到 goal 节点"的图距离(BFS)升序认领——距目标近的
// 转移边优先;goal_scope=specific 时只认领可达 goal 的边(不可达边不派,防死锁:
// 全不可达时回退全部候选);maximize(默认)dist 优先 + 同 dist 按创建序。
func (s *Scheduler) claimBreachEdge(ctx context.Context, challengeID string) bool {
	edges, err := s.bspace.OpenEdges(challengeID)
	if err != nil || len(edges) == 0 {
		return false
	}
	edge := s.pickBreachEdge(challengeID, edges)
	if edge == nil {
		return false
	}
	if err := s.bspace.ClaimEdge(edge.ID); err != nil {
		return false
	}
	text := fmt.Sprintf("执行转移:%s (from %s)", edge.Action, edge.From)
	// 经 intent 派活(复用 explore worker 轨迹;edge 关联经 handoff 传递)
	it, err := s.board.AddIntent(challengeID, text, 0.7, "scheduler",
		blackboard.IntentIn{Target: edge.From, Approach: "breach:" + edge.Action})
	if err != nil {
		if !errors.Is(err, blackboard.ErrNoChange) {
			// 硬失败:回滚边到 open,防状态空间卡死
			s.logErr(challengeID, "breach unclaim edge", s.bspace.UnclaimEdge(edge.ID))
			return false
		}
		// 已有同方向 intent:认领它(过滤 breach 方向,不误认领 observer/relay)
		intents, _ := s.board.Intents(challengeID, []string{blackboard.StateOpen}, 0)
		found := false
		for i := range intents {
			if intents[i].Approach == "breach:"+edge.Action {
				it = &intents[i]
				found = true
				break
			}
		}
		if !found {
			s.logErr(challengeID, "breach unclaim edge", s.bspace.UnclaimEdge(edge.ID))
			return false
		}
	}
	if err := s.board.UpdateIntentState(it.ID, blackboard.StateClaimed, ""); err != nil {
		s.logErr(challengeID, "claim intent", err)
		return false // 已被其他 tick 认领,跳过
	}
	s.dispatch(ctx, challengeID, WorkerOperator, PhaseExplore, text, it.ID, edge.ID)
	return true
}

// maxGoalDist 是距 goal 不可达距离(scheduler 侧判定,与 breach.maxDist 同值)。
const maxGoalDist = 1 << 20

// pickBreachEdge 按距 goal 启发式选取下一条认领边(03 §3.2 breach 分支)。
//   - maximize(默认):dist 升序优先,同 dist 按创建序(edges 原序)
//   - specific:只认领可达 goal 的边(dist < maxDist);全部不可达时回退全部候选
//     (防死锁:状态图未展开到 goal 前不可达是常态,不派活会卡死调度)
func (s *Scheduler) pickBreachEdge(challengeID string, edges []breach.Edge) *breach.Edge {
	if len(edges) == 0 {
		return nil
	}
	type cand struct {
		edge *breach.Edge
		dist int
	}
	cands := make([]cand, 0, len(edges))
	for i := range edges {
		cands = append(cands, cand{edge: &edges[i], dist: s.bspace.DistToGoal(challengeID, edges[i].From)})
	}
	sort.SliceStable(cands, func(a, b int) bool { return cands[a].dist < cands[b].dist })
	if s.taskProfile.GoalScope == "specific" {
		reachable := 0
		for _, c := range cands {
			if c.dist < maxGoalDist {
				reachable++
			}
		}
		if reachable > 0 {
			// 只认领可达 goal 的边
			for _, c := range cands {
				if c.dist < maxGoalDist {
					return c.edge
				}
			}
		}
		// 全不可达 → 回退最近候选(防死锁)
	}
	return cands[0].edge
}

// claimIntentCoverage phase2 coverage 认领(03 §3.2):
// 过滤与 confirmed/claimed 格子键相同的 intent(已覆盖/在跑方向不再派),
// 剩余按 weight 降序取第一个。全被过滤 → nil(调用方走补格)。
func (s *Scheduler) claimIntentCoverage(challengeID string, open []blackboard.Intent) *blackboard.Intent {
	if len(open) == 0 {
		return nil
	}
	// 矩阵态势:confirmed/claimed 格子的 endpoint 集合(intent 按 target 过滤)
	blocked := map[string]bool{}
	if s.cov != nil {
		if cells, err := s.cov.Cells(challengeID); err == nil {
			for _, c := range cells {
				if (c.Status == coverage.StatusConfirmed || c.Status == coverage.StatusClaimed) && c.Endpoint != "" {
					blocked[c.Endpoint] = true
				}
			}
		}
	}
	for _, it := range open {
		// 补格 intent(approach=gap_fill)不与格子过滤冲突——它本身就是
		// 为 open 格生成的,直接派(避免 claimed 格与 open intent 的假死锁)。
		if it.Approach == "gap_fill" {
			return &it
		}
		// intent 带 target 且命中已覆盖/在跑格子 → 跳过(open 已按 weight 降序)
		if it.Target == "" || !blocked[it.Target] {
			return &it
		}
	}
	return nil
}

// dispatchGapFiller 矩阵有 open 格子但无可用 intent 时,生成"补格 intent"派活
// (03 §3.2 第 4 条)。open 格子按 id 序(攻击面落地序)取第一个未覆盖者。
func (s *Scheduler) dispatchGapFiller(ctx context.Context, challengeID string) bool {
	if s.cov == nil {
		return false
	}
	cells, err := s.cov.OpenCells(challengeID)
	if err != nil || len(cells) == 0 {
		return false
	}
	for _, c := range cells {
		if c.Status != coverage.StatusOpen {
			continue
		}
		text := fmt.Sprintf("补格:测试 %s%s 的 %s", c.Asset, c.Endpoint, cweName(c.CWE))
		it, err := s.board.AddIntent(challengeID, text, 0.6, "scheduler",
			blackboard.IntentIn{Target: c.Endpoint, Approach: "gap_fill"})
		if err != nil {
			if errors.Is(err, blackboard.ErrNoChange) {
				continue // 已有同方向 intent
			}
			return false
		}
		if err := s.board.UpdateIntentState(it.ID, blackboard.StateClaimed, ""); err != nil {
			s.logErr(challengeID, "claim intent", err)
			return false
		}
		s.logErr(challengeID, "gap cov claimed", s.cov.MarkClaimedCell(challengeID, c.CWE, c.Asset, c.Endpoint))
		s.dispatch(ctx, challengeID, WorkerOperator, PhaseExplore, text, it.ID)
		return true
	}
	return false
}

// hasOpenCells 矩阵是否存在 open 格子(补格分支判定)。
func (s *Scheduler) hasOpenCells(challengeID string) bool {
	if s.cov == nil {
		return false
	}
	cells, err := s.cov.OpenCells(challengeID)
	if err != nil {
		return false
	}
	return len(cells) > 0
}

// cweName 补格 intent 文本用(白名单名或编号)。
func cweName(cweID string) string {
	if cweID == "" {
		return "常见漏洞"
	}
	if n, ok := cweNames[cweID]; ok {
		return n
	}
	return cweID
}

var cweNames = map[string]string{
	"CWE-89": "SQL 注入", "CWE-79": "XSS", "CWE-639": "越权",
	"CWE-22": "路径穿越", "CWE-434": "文件上传", "CWE-307": "弱口令",
	"CWE-287": "认证绕过", "CWE-352": "CSRF", "CWE-200": "信息泄露",
}

// nextBestIntent 从 claimed/pending 遗留意图中找权重最高者(被 stale abort 后重试)。
func (s *Scheduler) nextBestIntent(challengeID string) *blackboard.Intent {
	intents, err := s.board.Intents(challengeID, []string{blackboard.StateClaimed, blackboard.StatePending}, 1)
	if err != nil || len(intents) == 0 {
		return nil
	}
	return &intents[0]
}

// shouldBootstrap 是否需要(重)启动 bootstrap:
// 未启动过 → 启动;启动过但全部失败(≤上限)且黑板仍空 → 重试;成功过 → 不再启动。
func (s *Scheduler) shouldBootstrap(challengeID string) bool {
	ws, err := s.board.Workers(challengeID)
	if err != nil {
		return false // 查询失败保守起见不再启动
	}
	var failed int
	for _, w := range ws {
		if w.WorkerType != WorkerOperator || w.Phase != string(PhaseRecon) {
			continue
		}
		switch w.Status {
		case "done":
			return false // 成功过 → 不重试
		case "failed", "aborted":
			failed++
		default: // running/pending 等 → 不重试
			return false
		}
	}
	return failed <= maxBootstrapRetries
}

// isBreachMode 当前终止策略是否为 breach 形态。
func (s *Scheduler) isBreachMode() bool {
	return s.hub != nil && s.hub.Policy().GoalShape == constraint.GoalShapeBreach
}
