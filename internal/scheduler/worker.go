package scheduler

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"scopeforge/internal/blackboard"
	"scopeforge/internal/constraint"
	"scopeforge/internal/dispatcher"
	"scopeforge/internal/event"
	"scopeforge/internal/executor"
	"scopeforge/internal/reasonix/provider"
)

// Code extracted from scheduler.go (06 C1 split).

// ------------------------------------------------------------------ Worker 运行

// runWorker 在 goroutine 中运行 executor,结束后解析契约回写黑板。
func (s *Scheduler) runWorker(ctx context.Context, rw *runningWorker) {
	defer close(rw.done)
	rec := rw.record
	wid := rec.ID
	workerType := rec.WorkerType
	challengeID := rec.ChallengeID

	p := s.providers[rec.Provider]
	if p == nil {
		for _, pp := range s.providers {
			p = pp
			break
		}
	}
	sysVars := map[string]string{
		"{max_turns}": fmt.Sprintf("%d", s.workerTurns(workerType, rw.phase)),
	}
	// 切片 5 任务描述注入(04 §1):description + skill 卡清单 + 禁区约束,
	// 以独立上下文块注入所有 worker(非空时)。
	if ctx := taskContextBlock(s.taskProfile); ctx != "" {
		sysVars["{task_context}"] = ctx
	}
	if workerType == WorkerSynthesizer {
		s.mu.Lock()
		sysVars["{termination_reason}"] = string(s.termReason)
		s.mu.Unlock()
	}
	// 成本账本 role:operator 按 phase 记账(recon/explore/reason 分离),synthesizer 独立。
	role := workerType
	if workerType == WorkerOperator {
		role = string(rw.phase)
	}
	opts := &executor.Options{
		Provider: p,
		Registry: s.workerRegistryFor(rec),
		Session:  rw.session,
		Store:    s.convSt,
		MaxTurns: s.workerTurns(workerType, rw.phase),
		Gate:     s.gate,
		// 注入 challengeID:executor 内部事件(turn_start/tool_call/usage 等)
		// 不带 challenge_id,前端按任务过滤会丢事件(06.10 已知残留)。
		Sink: event.FuncSink(func(e event.Event) {
			if e.ChallengeID == "" {
				e.ChallengeID = challengeID
			}
			if e.TraceID == "" {
				e.TraceID = s.traceID
			}
			s.sink.Emit(e)
		}),
		CompactCfg:   s.compact,
		WorkDir:      s.workDir,
		SystemPrompt: s.buildSystem(workerType, s.targetText(), sysVars, s.focus() != nil, rw.phase),
		SkillIndex:   s.skillIndexBlock(),
		Ledger:       s.ledger,
		Role:         role,
		Pricing:      s.pricing[rec.Provider],
		TurnTailFn: func() []provider.Message {
			rw.mu.Lock()
			defer rw.mu.Unlock()
			if len(rw.steer) == 0 {
				return nil
			}
			out := rw.steer
			rw.steer = nil
			return out
		},
		OnTurn: func(turn int) {
			s.logErr(challengeID, "worker heartbeat", s.board.WorkerHeartbeat(wid, false))
		},
		Guard: s.guard,
	}
	// 可用工具落一条 checkpoint:时间线可直接看到 executor 是否具备
	// 攻击工具经 kali-tools skill 卡说明,直接由容器 bash 调用;
	toolNames := opts.Registry.Names()
	toolLabel := workerType
	if workerType == WorkerOperator {
		toolLabel = string(rw.phase)
	}
	s.sink.Emit(event.Event{Kind: event.KindCheckpoint, ChallengeID: challengeID, SessionID: wid,
		Payload: map[string]any{
			"action": "worker_tools", "worker": wid, "phase": toolLabel, "tools": toolNames,
			"note": fmt.Sprintf("%s 可用工具(%d): %s", toolLabel, len(toolNames), strings.Join(toolNames, ", ")),
		}})

	// worker 专用注册表(与 Launch 中一致;runWorker 内重新构造,含平台工具)
	res, runErr := executor.Run(ctx, opts)

	// 结束处理:契约解析 → 回写
	status := "done"
	if runErr != nil {
		status = "failed"
		if errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) {
			status = "aborted"
		}
	}
	if runErr == nil || status == "done" {
		// applyContract 返回 false = 契约解析失败(unparseable)→ 标记 failed,
		// 让 shouldBootstrap/未穷尽判定把这次执行视为失败(重试而非收尾)。
		if !s.applyContract(ctx, rw, res, status) {
			status = "failed"
		}
	} else {
		s.sink.Emit(event.Event{Kind: event.KindWorkerDone, ChallengeID: challengeID,
			Payload: map[string]any{"worker": wid, "status": status, "error": errString(runErr)}})
	}
	s.logErr(challengeID, "worker finished", s.board.MarkWorkerFinished(wid, status))

	// intent 清理
	if rw.intentID > 0 && status == "aborted" {
		s.logErr(challengeID, "intent open on abort", s.board.UpdateIntentState(rw.intentID, blackboard.StateOpen, ""))
	}

	s.mu.Lock()
	if cur, ok := s.running[wid]; ok && cur == rw {
		delete(s.running, wid)
	}
	concluding := s.concluding
	s.mu.Unlock()

	if concluding && workerType == WorkerSynthesizer {
		// phase2 S6:方向耗尽(no_more_work)类收尾必须带穷尽声明证据;
		// 无证据不采信 → 解除 concluding,调度继续(防懒收尾,docs/phase2/02 §2.4)。
		s.mu.Lock()
		needEvidence := s.termReason == "no_more_work"
		s.mu.Unlock()
		if needEvidence && !rw.exhaustedOK {
			// 穷尽声明被拒:解除 concluding 继续调度,但计数防无限空转
			// (模型持续输出无证据 exhausted → 达上限强制收尾,预算不再白烧)。
			s.mu.Lock()
			s.exhaustRejects++
			force := s.exhaustRejects >= maxExhaustionRejects
			if !force {
				s.concluding = false
				s.termReason = ""
			}
			s.mu.Unlock()
			s.sink.Emit(event.Event{Kind: event.KindCheckpoint, ChallengeID: challengeID,
				Payload: map[string]any{"action": "exhaustion_rejected", "worker": wid,
					"rejects":        s.exhaustRejects,
					"reason":         "conclude 无穷尽声明证据(或 exhausted=false),调度继续",
					"force_conclude": force}})
			if force {
				s.mu.Lock()
				s.concluded = true
				s.mu.Unlock()
			}
		} else {
			s.mu.Lock()
			s.concluded = true
			s.mu.Unlock()
		}
	}
}

// applyContract 解析 Worker 输出契约并回写黑板(docs/03 §3.2)。
// 返回 false = 契约解析失败(unparseable,调用方标记 worker failed 触发重试)。
// 阶段 06 M2: 编排方法——11 个子步骤拆分到 contract.go。
func (s *Scheduler) applyContract(ctx context.Context, rw *runningWorker, res *executor.Result, status string) bool {
	// 1. 解析契约
	contract, findings, ok := s.parseWorkerContract(rw, res)
	if !ok {
		return false
	}
	// 2. 聚焦过滤
	contract, findings = s.applyFocusFilter(contract, findings, rw)
	// 3-7. 回写
	s.applyFindings(ctx, rw, findings)
	s.applyIntents(ctx, rw, contract)
	s.applyDeadEnds(ctx, rw, contract)
	s.applyAttackSurface(ctx, rw, contract)
	// 8. 覆盖矩阵 + 聚焦完成判定
	focusedConfirmed := s.applyCoverage(ctx, rw, findings)
	if focusedConfirmed && !s.isConcluding() {
		s.enterConclude(ctx, rw.record.ChallengeID, constraint.ReasonTaskDone, rw.record.ID)
	}
	// 9-12. 语义后处理
	s.applyBreachState(ctx, rw, findings)
	s.applyExhaustion(ctx, rw, contract)
	s.applyAuthGained(ctx, rw, findings)
	s.applyPrivilege(ctx, rw, findings)
	s.applyIntentTransition(ctx, rw, contract, findings)
	// 13. 事件
	s.emitWorkerDone(rw, contract, findings)
	return true
}

// unExhausted 判定 explore 是否"未穷尽"(03 §3.3):
// intent_done 收尾 + 无 new_intent 回报 + 本方向无漏洞产出(vuln finding)。
// conclude 收尾(explore 判定任务完成)与有产出方向不算未穷尽。
func (s *Scheduler) unExhausted(rw *runningWorker, contract *dispatcher.WorkerContract, findings []dispatcher.Finding) bool {
	// unExhausted 仅对 Explore 阶段生效(07); Conclude/Recon/Reason 不判定未穷尽。
	if rw.phase != PhaseExplore {
		return false
	}
	if contract.StopReason == "conclude" {
		return false // explore 判定任务完成 → 正常收尾
	}
	if len(contract.NewIntents) > 0 {
		return false // 有后续方向回报 → 方向已延伸,不算未穷尽
	}
	for _, f := range findings {
		if f.Prefix == blackboard.PrefixVuln {
			return false // 本方向有漏洞产出
		}
	}
	return true
}
