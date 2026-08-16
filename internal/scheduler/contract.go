package scheduler

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"scopeforge/internal/blackboard"
	"scopeforge/internal/breach"
	"scopeforge/internal/dispatcher"
	"scopeforge/internal/event"
	"scopeforge/internal/executor"
)

// ------------------------------------------------------------------ 契约子步骤(06 M2 拆分)

// parseWorkerContract 解析 Worker 输出契约。
// 返回 (contract, findings, ok)。ok=false 时调用方标记 worker failed。
func (s *Scheduler) parseWorkerContract(rw *runningWorker, res *executor.Result) (*dispatcher.WorkerContract, []dispatcher.Finding, bool) {
	contract, err := dispatcher.ParseContract(res.FinalText)
	if err != nil {
		s.mu.Lock()
		s.unparseable[rw.record.Provider]++
		n := s.unparseable[rw.record.Provider]
		s.mu.Unlock()
		s.sink.Emit(event.Event{Kind: event.KindWorkerDone, ChallengeID: rw.record.ChallengeID,
			Payload: map[string]any{"worker": rw.record.ID, "status": "unparseable", "error": err.Error(), "consecutive": n}})
		return nil, nil, false
	}
	// 契约归一化:模型常以 submit_vulnerability 工具参数格式输出漏洞 finding
	// (title/description/evidence 字段,无 prefix)。结构化键(cwe + asset/endpoint)
	// 齐全即视为漏洞产出,补 vuln: 前缀——否则 unExhausted/applyCoverage/
	// focusFilter 全部失效:漏洞确认后 intent 被误判"未穷尽"反复重派,
	// 覆盖矩阵不确认,聚焦模式"确认后即终止"不触发(实测重复派发 4 次)。
	normalizeFindings(contract.Findings)
	return contract, contract.Findings, true
}

// normalizeFindings 就地归一化契约 findings(见 parseWorkerContract 注释)。
// 仅处理 prefix 缺失且带完整漏洞结构化键的条目,已带 prefix 的条目不动。
// asset 必填:与 applyCoverage/applyBreachState 的资产键要求对齐(仅 endpoint
// 的漏洞无法确认覆盖格子,不提升为 vuln)。
func normalizeFindings(findings []dispatcher.Finding) {
	for i := range findings {
		f := &findings[i]
		if f.Prefix != "" {
			continue
		}
		if f.CWE == "" || f.Asset == "" {
			continue
		}
		f.Prefix = blackboard.PrefixVuln
		if f.Weight == 0 {
			f.Weight = 0.9 // 结构化漏洞产出默认高置信
		}
	}
}

// applyFocusFilter 聚焦模式下过滤越界产出。
func (s *Scheduler) applyFocusFilter(contract *dispatcher.WorkerContract, findings []dispatcher.Finding, rw *runningWorker) (*dispatcher.WorkerContract, []dispatcher.Finding) {
	if ft := s.focus(); ft != nil {
		contract = s.focusFilter(contract, ft, rw.record.ChallengeID)
		findings = contract.Findings
	}
	return contract, findings
}

// applyFindings 回写 findings 到黑板(带 seq 冲突重试)。
func (s *Scheduler) applyFindings(ctx context.Context, rw *runningWorker, findings []dispatcher.Finding) {
	wid := rw.record.ID
	challengeID := rw.record.ChallengeID
	if err := s.disp.ReportFindings(ctx, wid, challengeID, rw.launchSeq, findings); err != nil {
		if errors.Is(err, dispatcher.ErrConflict) {
			snap, _ := s.board.SnapshotForWorker(challengeID)
			s.logErr(challengeID, "report findings retry", s.disp.ReportFindings(ctx, wid, challengeID, snap.AsOfSeq, findings))
			s.sink.Emit(event.Event{Kind: event.KindCheckpoint, ChallengeID: challengeID,
				Payload: map[string]any{"action": "merge_re_report", "worker": wid}})
		} else {
			s.sink.Emit(event.Event{Kind: event.KindError, ChallengeID: challengeID,
				Payload: map[string]any{"error": fmt.Sprintf("report findings: %v", err)}})
		}
	}
}

// applyIntents 回写新意图到黑板(含 breach agent edge 自由边落地)。
func (s *Scheduler) applyIntents(ctx context.Context, rw *runningWorker, contract *dispatcher.WorkerContract) {
	wid := rw.record.ID
	challengeID := rw.record.ChallengeID
	for _, it := range contract.NewIntents {
		s.logErr(challengeID, "report intent", s.disp.ReportIntent(ctx, wid, challengeID, it))
		if s.isBreachMode() && s.bspace != nil && rw.edgeID != "" {
			if edge, err := s.bspace.EdgeByID(challengeID, rw.edgeID); err == nil && edge != nil {
				action := it.Text
				if action == "" {
					action = it.Approach
				}
				if err := s.bspace.AddAgentEdge(challengeID, edge.From, action); err == nil {
					s.sink.Emit(event.Event{Kind: event.KindCheckpoint, ChallengeID: challengeID,
						Payload: map[string]any{"action": "agent_transition", "from": edge.From,
							"direction": action, "worker": wid,
							"note": "agent 补充方向落地为自由边,参与距 goal 启发式认领"}})
				}
			}
		}
	}
}

// applyDeadEnds 回写死路标记。
func (s *Scheduler) applyDeadEnds(ctx context.Context, rw *runningWorker, contract *dispatcher.WorkerContract) {
	for _, de := range contract.DeadEnds {
		s.logErr(rw.record.ChallengeID, "report dead end", s.disp.ReportDeadEnd(ctx, rw.record.ID, rw.record.ChallengeID, de, ""))
	}
}

// applyAttackSurface 落地攻击面清单到覆盖矩阵。
func (s *Scheduler) applyAttackSurface(ctx context.Context, rw *runningWorker, contract *dispatcher.WorkerContract) {
	if len(contract.AttackSurface) == 0 {
		return
	}
	if err := s.disp.ReportAttackSurface(ctx, rw.record.ID, rw.record.ChallengeID, []dispatcher.AttackSurfaceItem(contract.AttackSurface)); err != nil {
		s.sink.Emit(event.Event{Kind: event.KindError, ChallengeID: rw.record.ChallengeID,
			Payload: map[string]any{"error": fmt.Sprintf("attack surface: %v", err)}})
	}
}

// applyCoverage 覆盖矩阵更新: vuln 发现 → 格子 confirmed + 接力。
// 返回 focusedConfirmed(聚焦模式下 explore 确认漏洞后触发任务完成)。
func (s *Scheduler) applyCoverage(ctx context.Context, rw *runningWorker, findings []dispatcher.Finding) bool {
	if s.cov == nil {
		return false
	}
	challengeID := rw.record.ChallengeID
	var focusedConfirmed bool
	for _, f := range findings {
		if f.Prefix != blackboard.PrefixVuln || f.CWE == "" || f.Asset == "" {
			continue
		}
		s.logErr(challengeID, "cov mark confirmed", s.cov.MarkConfirmed(challengeID, f.CWE, f.Asset, f.Endpoint))
		if s.focus() == nil {
			s.logErr(challengeID, "cov generate relay", s.cov.GenerateRelay(challengeID, f.CWE, f.Asset, f.Endpoint))
		} else if rw.phase != PhaseRecon {
			focusedConfirmed = true
		}
	}
	return focusedConfirmed
}

// applyBreachState breach 状态空间: vuln 发现 → 状态节点 + 转移边展开。
func (s *Scheduler) applyBreachState(ctx context.Context, rw *runningWorker, findings []dispatcher.Finding) {
	if !s.isBreachMode() || s.bspace == nil {
		return
	}
	challengeID := rw.record.ChallengeID
	for _, f := range findings {
		if f.Prefix != blackboard.PrefixVuln || f.Asset == "" {
			continue
		}
		nodeID := "state@" + f.Asset
		created, _ := s.bspace.ConfirmNode(challengeID, nodeID, "web", f.Asset, "")
		if rw.edgeID != "" {
			s.logErr(challengeID, "breach confirm edge", s.bspace.ConfirmEdge(challengeID, rw.edgeID, nodeID))
		}
		if created {
			s.logErr(challengeID, "breach add transitions", s.bspace.AddTransitions(challengeID, nodeID))
		}
	}
}

// applyExhaustion S6 穷尽声明验证: conclude worker 的 exhausted 声明必须带证据。
func (s *Scheduler) applyExhaustion(ctx context.Context, rw *runningWorker, contract *dispatcher.WorkerContract) {
	if rw.record.WorkerType != WorkerSynthesizer {
		return
	}
	// 证据必须含非空 direction(空对象 [{}] 不采信,防模型空壳声明)。
	rw.exhaustedOK = contract.Exhausted && hasCoverageEvidence(contract.CoverageEvidence)
	for _, risk := range contract.RemainingRisks {
		s.logErr(rw.record.ChallengeID, "risk intent",
			s.disp.ReportIntent(ctx, rw.record.ID, rw.record.ChallengeID, dispatcher.IntentIn{Text: risk, Weight: 0.5}))
	}
}

// hasCoverageEvidence 是否存在至少一条带方向的穷尽证据。
func hasCoverageEvidence(ev []dispatcher.CoverageEvidence) bool {
	for _, e := range ev {
		if e.Direction != "" {
			return true
		}
	}
	return false
}

// applyAuthGained 登录态获取 → 重开"缺登录态"跳过的覆盖格子。
func (s *Scheduler) applyAuthGained(ctx context.Context, rw *runningWorker, findings []dispatcher.Finding) {
	if s.cov == nil {
		return
	}
	var authText string
	for _, f := range findings {
		if f.AuthGained {
			authText = f.Text
			break
		}
	}
	if authText != "" && s.focus() == nil {
		if n, err := s.cov.ReopenSkippedAuth(rw.record.ChallengeID, ""); err == nil && n > 0 {
			s.sink.Emit(event.Event{Kind: event.KindCheckpoint, ChallengeID: rw.record.ChallengeID,
				Payload: map[string]any{"action": "auth_gained_reopen", "reopened": n,
					"finding": authText,
					"note":    "声明获得登录态后重开缺登录态跳过的方向"}})
		}
	}
}

// applyPrivilege breach 能力维度落地: auth_gained/privilege → 节点能力累加 + 转移候选解锁。
func (s *Scheduler) applyPrivilege(ctx context.Context, rw *runningWorker, findings []dispatcher.Finding) {
	if !s.isBreachMode() || s.bspace == nil {
		return
	}
	challengeID := rw.record.ChallengeID
	for _, f := range findings {
		var caps []string
		if f.Privilege != "" {
			for _, c := range strings.Split(f.Privilege, ",") {
				if c = strings.TrimSpace(c); c != "" {
					caps = append(caps, c)
				}
			}
		}
		if f.AuthGained {
			caps = append(caps, breach.PrivilegeAuthenticated)
		}
		if len(caps) == 0 {
			continue
		}
		key := f.Asset
		if key == "" {
			key = f.Endpoint
		}
		if key == "" {
			key = "unknown"
		}
		nodeID := "state@" + key
		_, _ = s.bspace.ConfirmNode(challengeID, nodeID, "web", key, strings.Join(caps, ","))
		if rw.edgeID != "" {
			s.logErr(challengeID, "breach confirm edge2", s.bspace.ConfirmEdge(challengeID, rw.edgeID, nodeID))
		}
		s.logErr(challengeID, "breach add transitions2", s.bspace.AddTransitions(challengeID, nodeID))
		s.sink.Emit(event.Event{Kind: event.KindCheckpoint, ChallengeID: challengeID,
			Payload: map[string]any{"action": "privilege_gained_node", "node": nodeID,
				"privilege": strings.Join(caps, ","), "finding": f.Text,
				"note": "能力声明落地 → 节点 privilege 累加,转移候选解锁(kind 基线 ∪ 能力动作)"}})
	}
}

// applyIntentTransition intent 状态机收尾: 按 stop_reason 更新 intent 状态 + 覆盖/breach 联动。
func (s *Scheduler) applyIntentTransition(ctx context.Context, rw *runningWorker, contract *dispatcher.WorkerContract, findings []dispatcher.Finding) {
	if rw.intentID <= 0 {
		return
	}
	challengeID := rw.record.ChallengeID
	// 查找 intent(按 claimed 状态)
	var intent *blackboard.Intent
	intents, _ := s.board.Intents(challengeID, []string{blackboard.StateClaimed}, 0)
	for i := range intents {
		if intents[i].ID == rw.intentID {
			intent = &intents[i]
			break
		}
	}
	switch contract.StopReason {
	case "exhausted":
		s.logErr(challengeID, "intent dead", s.board.UpdateIntentState(rw.intentID, blackboard.StateDead, ""))
		if s.cov != nil && intent != nil {
			s.logErr(challengeID, "cov mark dead", s.cov.MarkDead(challengeID, "", intent.Target))
		}
		if s.bspace != nil && rw.edgeID != "" {
			s.logErr(challengeID, "breach mark edge dead", s.bspace.MarkEdgeDead(rw.edgeID))
		}
	case "intent_done", "conclude":
		if s.unExhausted(rw, contract, findings) {
			s.mu.Lock()
			s.unExhaustedCount[rw.intentID]++
			over := s.unExhaustedCount[rw.intentID] > maxUnExhaustedRetries
			s.mu.Unlock()
			if over {
				s.logErr(challengeID, "intent dead retry", s.board.UpdateIntentState(rw.intentID, blackboard.StateDead, ""))
				if s.cov != nil && intent != nil {
					s.logErr(challengeID, "cov mark dead retry", s.cov.MarkDead(challengeID, "", intent.Target))
				}
			} else {
				s.logErr(challengeID, "intent open retry", s.board.UpdateIntentState(rw.intentID, blackboard.StateOpen, ""))
			}
		} else {
			s.logErr(challengeID, "intent done", s.board.UpdateIntentState(rw.intentID, blackboard.StateDone, ""))
		}
	default:
		s.logErr(challengeID, "intent pending", s.board.UpdateIntentState(rw.intentID, blackboard.StatePending, ""))
	}
}

// emitWorkerDone 发送 worker 完成事件。
func (s *Scheduler) emitWorkerDone(rw *runningWorker, contract *dispatcher.WorkerContract, findings []dispatcher.Finding) {
	s.sink.Emit(event.Event{Kind: event.KindWorkerDone, ChallengeID: rw.record.ChallengeID,
		Payload: map[string]any{"worker": rw.record.ID, "status": "done", "stop_reason": contract.StopReason,
			"findings": len(findings), "new_intents": len(contract.NewIntents), "dead_ends": len(contract.DeadEnds)}})
}
