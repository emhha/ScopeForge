// Package executor 是 Agent 主循环(单 Worker 运行时, docs/02 §7)。
// 迁移自上游 run_loop 语义:组装消息 → Stream → 工具轮执行 → 每轮落库,
// 事件全部发往 event.Sink(SSE/审计/回放的数据源)。
package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"scopeforge/internal/constraint"
	"scopeforge/internal/conversation"
	"scopeforge/internal/event"
	"scopeforge/internal/guard"
	"scopeforge/internal/reasonix/provider"
	"scopeforge/internal/reasonix/tool"
	"scopeforge/internal/sandbox"
)

// ErrMaxTurns 是主循环硬上限触达。
var ErrMaxTurns = errors.New("executor: max turns reached")

// Options 是单次运行参数。
type Options struct {
	Provider     provider.Provider
	Registry     *tool.Registry
	Session      *conversation.Session
	Store        *conversation.Store
	MaxTurns     int // 0 = 默认 200
	Gate         *Gate
	Sink         event.Sink
	Compactor    conversation.Compactor // nil = 压缩降级机械折叠
	CompactCfg   conversation.CompactConfig
	SkillIndex   string // 技能索引块(前缀,跨轮稳定)
	MemoryIndex  string // 记忆索引块(前缀,跨轮稳定)
	WorkDir      string
	SystemPrompt string
	// TurnTail 是轮尾注入(会话内变更,不破坏前缀缓存)。
	TurnTail []provider.Message
	// TurnTailFn 每轮动态轮尾(优先于 TurnTail;宿主动态注入,如 Observer steer)。
	TurnTailFn func() []provider.Message
	// OnTurn 每轮落库后回调(宿主进度心跳/stale 判定数据源)。
	OnTurn func(turn int)
	// Ledger 成本记账(usage 事件入账;nil = 不记账)。
	Ledger *constraint.CostLedger
	// Role 账本角色(main | bootstrap | explore | reason | conclude | observer)。
	Role string
	// Pricing 当前 provider 单价(cost 估算)。
	Pricing *provider.Pricing
	// Guard 确定性安全 Hook(M2,docs/04 §5.4/§7;nil = 关闭)。
	Guard *guard.Hook
}

// Result 是运行结果。
type Result struct {
	FinalText string
	Turns     int
	ToolCalls int
	Usage     []provider.Usage
	Err       error
}

// Run 执行主循环:maxTurns 硬上限,禁 while true(docs/02 §7.2)。
func Run(ctx context.Context, opts *Options) (*Result, error) {
	if opts.Sink == nil {
		opts.Sink = event.Discard
	}
	if opts.MaxTurns <= 0 {
		opts.MaxTurns = 200
	}
	if opts.Gate == nil {
		// 无权限门时默认 yolo(宿主未配置的安全兜底由 build 层保证)
		opts.Gate, _ = NewGate(ModeYolo, nil, nil)
	}
	// 会话损坏回滚(docs/03 §7):配对校验失败 → 回滚 recovery 分支
	if opts.Store != nil && opts.Session != nil {
		if reason := conversation.ValidatePairing(opts.Session.Messages); reason != "" {
			rb, err := opts.Store.LoadRecoveryBranch(opts.Session.ID)
			if err == nil && rb != nil {
				*opts.Session = *rb
				opts.Sink.Emit(event.Event{Kind: event.KindCheckpoint, SessionID: opts.Session.ID,
					Payload: map[string]any{"action": "rollback_recovery", "reason": reason, "rewrite_version": rb.RewriteVersion}})
			} else {
				opts.Sink.Emit(event.Event{Kind: event.KindError, SessionID: opts.Session.ID,
					Payload: map[string]any{"error": fmt.Sprintf("session pairing broken (%s) and no recovery branch", reason)}})
			}
		}
	}
	res := &Result{}
	s := opts.Session

	for turn := 0; turn < opts.MaxTurns; turn++ {
		res.Turns = turn + 1
		select {
		case <-ctx.Done():
			saveSnapshot(opts)
			return res, ctx.Err()
		default:
		}

		opts.Sink.Emit(event.Event{Kind: event.KindTurnStart, SessionID: s.ID, Payload: map[string]any{"turn": turn}})

		msgs, err := ComposeMessages(opts, turn)
		if err != nil {
			return res, err
		}
		stream, err := opts.Provider.Stream(ctx, provider.Request{
			Messages: msgs,
			Tools:    opts.Registry.Schemas(),
		})
		if err != nil {
			opts.Sink.Emit(event.Event{Kind: event.KindError, SessionID: s.ID, Payload: map[string]any{"error": err.Error()}})
			return res, err
		}

		var calls []provider.ToolCall
		var finalText string
		var usage provider.Usage
		var reasoningText strings.Builder
		for chunk := range stream {
			switch chunk.Type {
			case provider.ChunkReasoning:
				// 不在流式阶段逐 token 落库/广播;回合结束时作为一条消息级事件发出,
				// 避免 SQLite 每 token 一次事务与前端每秒上百次重算。
				reasoningText.WriteString(chunk.Text)
			case provider.ChunkText:
				finalText += chunk.Text
			case provider.ChunkToolCallStart:
				opts.Sink.Emit(event.Event{Kind: event.KindToolCallStart, SessionID: s.ID,
					Payload: map[string]any{"id": chunk.ToolCall.ID, "name": chunk.ToolCall.Name}})
			case provider.ChunkToolCallArgsDelta:
				// 参数增量不逐条落库;最终参数随 tool_call_result / 会话消息持久化。
			case provider.ChunkToolCall:
				calls = append(calls, *chunk.ToolCall)
			case provider.ChunkUsage:
				if chunk.Usage != nil {
					usage = *chunk.Usage
				}
				res.Usage = append(res.Usage, *chunk.Usage)
				if opts.Ledger != nil {
					_ = opts.Ledger.Record(s.ID, s.ChallengeID, opts.Role, opts.Provider.Name(), chunk.Usage, opts.Pricing)
				}
				opts.Sink.Emit(event.Event{Kind: event.KindUsage, SessionID: s.ID, Payload: chunk.Usage})
			case provider.ChunkError:
				emitMessageEvents(opts.Sink, s.ID, reasoningText.String(), finalText)
				opts.Sink.Emit(event.Event{Kind: event.KindError, SessionID: s.ID, Payload: map[string]any{"error": chunk.Err.Error()}})
				saveSnapshot(opts)
				return res, chunk.Err
			case provider.ChunkDone:
			}
		}
		emitMessageEvents(opts.Sink, s.ID, reasoningText.String(), finalText)

		// 记录本轮 assistant 回复(文本 + 工具调用)
		if finalText != "" || len(calls) > 0 {
			s.Add(provider.Message{
				Role:      provider.RoleAssistant,
				Content:   finalText,
				ToolCalls: calls,
			})
		}

		// 无工具调用 → 最终回复
		if len(calls) == 0 {
			saveSnapshot(opts)
			if opts.OnTurn != nil {
				opts.OnTurn(turn)
			}
			res.FinalText = finalText
			return res, nil
		}

		// 工具轮
		for _, call := range calls {
			if err := ctx.Err(); err != nil {
				saveSnapshot(opts)
				return res, err
			}
			res.ToolCalls++
			resultText, err := executeOne(ctx, opts, call)
			s.Add(provider.Message{
				Role:       provider.RoleTool,
				ToolCallID: call.ID,
				Name:       call.Name,
				Content:    resultText,
			})
			opts.Sink.Emit(event.Event{Kind: event.KindToolCallResult, SessionID: s.ID,
				Payload: map[string]any{"id": call.ID, "name": call.Name, "args": call.Arguments, "output": resultText, "error": err != nil}})
		}

		// 每轮落库(事件表 + sessions 表)
		saveSnapshot(opts)
		if opts.OnTurn != nil {
			opts.OnTurn(turn)
		}

		// 压缩检查(宿主侧硬触发, A5)
		if opts.Compactor != nil && usage.PromptTokens > 0 {
			level := conversation.LevelFor(float64(usage.PromptTokens)/float64(opts.CompactCfg.ContextWindow), opts.CompactCfg)
			switch level {
			case conversation.LevelSnip:
				s.Rewrite(conversation.SnipStaleToolResults(s.Snapshot(), 3))
				_ = opts.Store.SaveRewrite(s)
			case conversation.LevelCompact, conversation.LevelForce:
				opts.Sink.Emit(event.Event{Kind: event.KindCheckpoint, SessionID: s.ID,
					Payload: map[string]any{"action": "compact", "level": level.String(), "rewrite_version": s.RewriteVersion}})
				compactAndSave(ctx, opts, s)
			}
		}
	}
	saveSnapshot(opts)
	return res, ErrMaxTurns
}

// saveSnapshot 每轮落库(digest 冲突时发错误事件,宿主可重试)。
func saveSnapshot(opts *Options) {
	if opts.Store == nil {
		return
	}
	if err := opts.Store.SaveSnapshot(opts.Session); err != nil {
		// digest 冲突:重载后重试一次
		opts.Sink.Emit(event.Event{Kind: event.KindError, SessionID: opts.Session.ID,
			Payload: map[string]any{"error": fmt.Sprintf("save snapshot: %v", err)}})
	}
}

// compactAndSave 先落库再压缩(SaveSnapshot → Compact → SaveRewrite),
// 崩溃可回退(恢复分支语义)。
func compactAndSave(ctx context.Context, opts *Options, s *conversation.Session) {
	if opts.Store == nil || opts.Compactor == nil {
		return
	}
	_ = opts.Store.SaveSnapshot(s)
	summary, err := conversation.Compact(ctx, opts.Compactor, s, opts.CompactCfg)
	if err != nil {
		return
	}
	if summary.Degraded {
		opts.Sink.Emit(event.Event{Kind: event.KindCheckpoint, SessionID: s.ID,
			Payload: map[string]any{"action": "compact_degraded", "note": summary.ArchiveNote}})
	} else {
		opts.Sink.Emit(event.Event{Kind: event.KindCheckpoint, SessionID: s.ID,
			Payload: map[string]any{"action": "compact_done", "rewrite_version": s.RewriteVersion}})
	}
	_ = opts.Store.SaveRewrite(s)
	// 恢复分支:压缩后立即留一个 recovery 分支(摘要劣化可回退)
	_ = opts.Store.SaveRecoveryBranch(s, "post-compaction recovery branch")
}

// emitMessageEvents 按消息粒度落一条推理/回复事件(替代逐 token delta)。
func emitMessageEvents(sink event.Sink, sessionID, reasoning, text string) {
	if reasoning != "" {
		sink.Emit(event.Event{Kind: event.KindReasoningDelta, SessionID: sessionID,
			Payload: map[string]any{"text": reasoning, "aggregated": true}})
	}
	if text != "" {
		sink.Emit(event.Event{Kind: event.KindTextDelta, SessionID: sessionID,
			Payload: map[string]any{"text": text, "aggregated": true}})
	}
}

// ComposeMessages 组装发送消息:系统提示[缓存前缀] + 会话 + 轮尾注入 + 收尾压力提示。
// 系统前缀跨轮字节稳定(cache-first, docs/02 §1.4)。
func ComposeMessages(opts *Options, turn int) ([]provider.Message, error) {
	system := buildSystemPrompt(opts)
	tail := turnTail(opts)
	out := make([]provider.Message, 0, 2+len(opts.Session.Messages)+len(tail))
	out = append(out, provider.Message{Role: provider.RoleSystem, Content: system})

	// LocalOnly 消息剔除(含中断标记),发送前修复配对
	sessionMsgs := provider.NormalizeSessionMessages(opts.Session.Messages)
	for _, m := range sessionMsgs {
		if m.LocalOnly {
			continue
		}
		out = append(out, m)
	}
	// 中断恢复:若会话尾部有 InterruptedTurn,注入恢复消息
	if len(sessionMsgs) > 0 {
		last := sessionMsgs[len(sessionMsgs)-1]
		if last.InterruptedTurn != nil && last.InterruptedTurn.Pending {
			out = append(out, conversation.InterruptedTurnRecoveryMessage(last.InterruptedTurn))
		}
	}
	out = append(out, tail...)
	// 轮次压力提示(真实 LLM 不收尾的确定性兜底,docs/07 §4 止损话术):
	// 超过 2/3 轮次上限后每轮注入强制收尾指令(仍不遵守则 maxTurns 硬顶)。
	if opts.MaxTurns > 0 && turn >= opts.MaxTurns*2/3 {
		out = append(out, provider.Message{
			Role:    provider.RoleUser,
			Content: fmt.Sprintf("[系统] 轮次已消耗 %d/%d,即将耗尽。立即停止一切工具调用,严格按 CONTRACT 输出最终 JSON 收尾(汇总已确认的 findings/new_intents/dead_ends;哪怕发现有限也要输出,不要继续探测)。", turn+1, opts.MaxTurns),
		})
	}
	return provider.NormalizeMessages(out), nil
}

// turnTail 返回动态轮尾(优先 TurnTailFn)。
func turnTail(opts *Options) []provider.Message {
	if opts.TurnTailFn != nil {
		if t := opts.TurnTailFn(); t != nil {
			return t
		}
	}
	return opts.TurnTail
}

// buildSystemPrompt 构建稳定系统前缀:角色 + 工具契约 + 技能/记忆索引。
func buildSystemPrompt(opts *Options) string {
	var b strings.Builder
	base := opts.SystemPrompt
	if base == "" {
		base = "你是 ScopeForge 的自主任务执行智能体。使用工具完成任务,输出最终结论。"
	}
	b.WriteString(base)
	b.WriteString("\n\n## 可用工具\n")
	schemas := opts.Registry.Schemas()
	if len(schemas) == 0 {
		b.WriteString("(无)\n")
	}
	for _, s := range schemas {
		b.WriteString("- ")
		b.WriteString(s.Name)
		b.WriteString(": ")
		b.WriteString(s.Description)
		b.WriteString("\n")
	}
	if opts.SkillIndex != "" {
		b.WriteString("\n## 技能索引\n")
		b.WriteString(opts.SkillIndex)
	}
	if opts.MemoryIndex != "" {
		b.WriteString("\n## 记忆索引\n")
		b.WriteString(opts.MemoryIndex)
	}
	b.WriteString("\n\n规则:需要外部信息时使用工具;全部完成后给出最终结论。")
	return b.String()
}

// executeOne 执行单个工具调用:解析 → 权限 → 执行 → 结果。
func executeOne(ctx context.Context, opts *Options, call provider.ToolCall) (string, error) {
	resolved, canonical, candidates := opts.Registry.ResolveCall(call.Name)
	if resolved == nil {
		return fmt.Sprintf("[tool error] tool %q not found (candidates: %v)", call.Name, candidates), fmt.Errorf("tool %q not found", call.Name)
	}
	args := json.RawMessage(canonical)
	if len(call.Arguments) > 0 && json.Valid([]byte(call.Arguments)) {
		args = json.RawMessage(call.Arguments)
	}

	// 权限裁决(executable = 命令文本或工具名)
	argsMap := parseArgs(args)
	executable := call.Name
	if cmd, ok := argsMap["command"].(string); ok && cmd != "" {
		executable = cmd
	}
	decision, reason := opts.Gate.Check(executable, resolved.ReadOnly(), argsMap)
	switch decision {
	case Deny:
		return fmt.Sprintf("[permission denied] %s", reason), fmt.Errorf("permission denied: %s", reason)
	}

	// M2 确定性安全 Hook(§5.4 命令黑名单 + §7 凭据外泄;denied 记录入 events)
	// 检查范围:命令文本 + 全部字符串参数(堵住 tmux cmd/route target/nmap args
	// 等工具参数通道,防 "tmux_new_session(cmd='bash -i >& /dev/tcp/...')" 绕过)。
	if opts.Guard != nil {
		full := executable
		for _, v := range argsMap {
			if s, ok := v.(string); ok && s != "" && len(full)+len(s) < 4096 {
				full += " " + s
			}
		}
		if reason, denied := opts.Guard.CheckCommand(full); denied {
			return fmt.Sprintf("[guard denied] %s", reason), fmt.Errorf("guard denied: %s", reason)
		}
		if u, ok := argsMap["url"].(string); ok && u != "" {
			if reason, denied := opts.Guard.CheckOutbound(u); denied {
				return fmt.Sprintf("[guard denied] %s", reason), fmt.Errorf("guard denied: %s", reason)
			}
		}
	}

	// 容器模式:注入 challengeID,供容器化工具(如 sandbox bash)定位挑战容器
	// (Cairn 一题一容器;executor/executor.go 与 sandbox/bashwrap.go 共享契约)。
	if opts.Session != nil {
		ctx = sandbox.ContextWithChallenge(ctx, opts.Session.ChallengeID)
	}
	out, err := resolved.Execute(ctx, args)
	if err != nil {
		opts.Gate.RecordFailure(call.Name, parseArgs(args))
		// 06.27 修复:工具失败但 out 为空时,错误文本必须进会话/事件
		// (此前 ContainerBash 等返回 ("", err) → 时间线 output 全空,无法排查)
		if out == "" {
			out = fmt.Sprintf("[tool error] %s", err.Error())
		} else {
			out = out + fmt.Sprintf("\n[tool error] %s", err.Error())
		}
	} else {
		opts.Gate.RecordSuccess(call.Name, parseArgs(args))
	}
	// 输出截断(防上下文爆炸)
	if len(out) > 32*1024 {
		out = out[:32*1024] + "\n...[output truncated]..."
	}
	return out, err
}

func parseArgs(args json.RawMessage) map[string]any {
	var m map[string]any
	if err := json.Unmarshal(args, &m); err != nil {
		return nil
	}
	return m
}
