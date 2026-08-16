package build

// M3.5 Web 发起任务(serve 内异步 run;前端入口 POST /api/v1/run)。
//
// 统一入口(阶段 2.15 定案:阶段一单 agent 直跑删除,全部走黑板编排):
//   - RunTask:任务文本入口("http://test.com 是你的目标,关注支付逻辑...").
//     文本作为 TaskProfile.Description 注入调度器(04 §1 任务描述注入),
//     黑板编排 bootstrap(建攻击面格子)→ explore(认领执行)→ conclude。
//   - RunChallenge:预置 challenge_id 入口,同一调度器。
//
// 均为异步:立即返回 runID,进度经事件流呈现(run_started/run_done +
// worker/tick/termination 等既有事件)。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"scopeforge/internal/event"
)

// RunTask 启动黑板编排任务(阶段 2.15 起取代阶段一单 agent 直跑)。
// 任务文本作为 TaskProfile.Description 注入(04 §1),bootstrap 依描述建
// 攻击面格子,explore 认领执行,产出进黑板/账本/报告。
// focusURL 为任务指定目标(前端 platform_url);为空时从任务文本提取第一个
// http(s) URL;两者皆无 → 不启用聚焦(全目标模式,阶段 2.21)。
func (a *App) RunTask(ctx context.Context, task string, focusURL ...string) (string, error) {
	if task == "" {
		return "", fmt.Errorf("task 不能为空")
	}
	if len(a.Providers) == 0 {
		return "", fmt.Errorf("无可用 provider(检查配置 providers)")
	}
	focus := ""
	if len(focusURL) > 0 && strings.TrimSpace(focusURL[0]) != "" {
		focus = strings.TrimSpace(focusURL[0])
	} else {
		focus = firstURLInText(task) // 从任务描述兜底提取(聚焦纪律 + 硬过滤)
	}
	runID := fmt.Sprintf("task-%d", time.Now().UnixNano())
	// 04 §1 任务描述注入:输入目标 = 任务描述,profile 其余字段保持静态配置
	tp := a.Cfg.Platform.TaskProfile
	tp.Description = task
	if focus != "" {
		tp.FocusTarget = focus
	}
	a.Cfg.Platform.TaskProfile = tp // 回写 App 配置,Observer 等下游组件可读取
	sch := a.SchedulerForProfile(runID, a.SchedulerCfg(), tp)
	go func() {
		a.Sink.Emit(event.Event{Kind: event.KindRunStarted, ChallengeID: runID,
			Payload: map[string]any{"mode": "task", "task": task, "challenge_id": runID}})
		rctx, cancel := context.WithCancel(ctx)
		defer cancel()
		// 06.15 运行控制:task 模式仅停止(cancel;单会话无暂停语义)
		a.registerRun(runID, cancel)
		defer a.unregisterRun(runID)
		// 并发多任务防 hooks 串台(cancel 后注销,Observer 最后一轮 Steer 不丢)
		defer a.Dispatcher.UnregisterHooks(runID)
		// Observer 跟随配置默认启用(observer_every_n_turns>0 即启用)。
		if a.Cfg.Scheduler.ObserverEvery > 0 {
			obs := a.ObserverFor(runID)
			// 用 rctx 而非 ctx:任务 stop(cancel rctx)时 Observer 同步退出,
			// 否则循环永久存活(goroutine 泄漏 + 停止后继续调 LLM 烧预算)。
			go obs.Loop(rctx, runID)
		}
		if a.Sandbox != nil {
			// 任务级常驻容器:启动即创建(docker ps 立即可见),worker 共享;
			// 登录态/中间产物跨 worker 保持(06.14 用户决策,非 worker 级)。
			if ct, err := a.Sandbox.EnsureContainer(rctx, runID); err == nil && ct != nil {
				a.Sink.Emit(event.Event{Kind: event.KindWorkerLaunch, ChallengeID: runID,
					Payload: map[string]any{"worker": "container", "type": "sandbox", "container": ct.ID}})
			}
			defer a.Sandbox.RemoveForChallenge(context.Background(), runID)
		}
		res, err := sch.Run(rctx, runID)
		payload := map[string]any{"mode": "task", "challenge_id": runID}
		if res != nil {
			payload["turns"] = res.Turns
			payload["terminated"] = res.Terminated
			payload["reason"] = string(res.Reason)
			if res.ReportPath != "" {
				payload["report_path"] = res.ReportPath
			}
		}
		if err != nil {
			// 用户 stop(cancel rctx)不是失败:标记 interrupted 供前端展示
			// "已停止",避免 run_done 携带 context canceled 显示为 failed。
			if errors.Is(err, context.Canceled) {
				payload["interrupted"] = true
			} else {
				payload["error"] = err.Error()
			}
		}
		a.Sink.Emit(event.Event{Kind: event.KindRunDone, ChallengeID: runID, Payload: payload})
	}()
	return runID, nil
}

// registerRun 注册运行中任务的取消函数。
func (a *App) registerRun(id string, cancel context.CancelFunc) {
	a.runMu.Lock()
	defer a.runMu.Unlock()
	if a.runs == nil {
		a.runs = map[string]context.CancelFunc{}
	}
	a.runs[id] = cancel
}

// unregisterRun 注销(run_done 后)。
func (a *App) unregisterRun(id string) {
	a.runMu.Lock()
	defer a.runMu.Unlock()
	delete(a.runs, id)
}

// RunControlFor 返回任务取消函数(不存在返回 nil)。
func (a *App) RunControlFor(id string) context.CancelFunc {
	a.runMu.Lock()
	defer a.runMu.Unlock()
	return a.runs[id]
}
