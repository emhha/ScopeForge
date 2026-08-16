// 事件流展示辅助:kind 元数据、摘要、payload 文本、增量合并时间线。
import type { EventItem } from '../api/types'

// 2.41 后 phase 字符串统一为 scout/executor/analyst;兼容历史值。
export function phaseLabel(ph?: string | null): string {
  if (!ph) return ''
  return ({ recon: 'scout', explore: 'executor', reason: 'analyst' } as Record<string, string>)[ph] || ph
}

export interface KindMeta {
  label: string
  cls: string
  icon: string
}

export const kindMeta: Record<string, KindMeta> = {
  turn_start: { label: '回合开始', cls: 'info', icon: '◐' },
  reasoning_delta: { label: '推理', cls: 'info', icon: '🧠' },
  text_delta: { label: '回复', cls: 'info', icon: '💬' },
  tool_call_start: { label: '调用工具', cls: 'accent', icon: '🔧' },
  tool_call_args_delta: { label: '参数', cls: 'accent', icon: '…' },
  tool_call_result: { label: '工具结果', cls: 'accent', icon: '↩' },
  usage: { label: '用量', cls: 'faint', icon: 'ⓘ' },
  submission: { label: '提交', cls: 'ok', icon: '🚩' },
  finding: { label: '发现', cls: 'warn', icon: '💥' },
  steer: { label: '纠偏', cls: 'purple', icon: '🎯' },
  checkpoint: { label: '检查点', cls: 'faint', icon: '◆' },
  error: { label: '错误', cls: 'bad', icon: '✗' },
  worker_launch: { label: 'Worker 派发', cls: 'info', icon: '▶' },
  worker_done: { label: 'Worker 完成', cls: 'ok', icon: '✔' },
  worker_abort: { label: 'Worker 中止', cls: 'bad', icon: '■' },
  scheduler_tick: { label: '调度节拍', cls: 'faint', icon: '⏱' },
  termination: { label: '任务终止', cls: 'bad', icon: '⛔' },
  board_change: { label: '黑板变更', cls: 'info', icon: '▤' },
  denied: { label: '拦截', cls: 'bad', icon: '🛡' },
  traffic: { label: '流量', cls: 'info', icon: '⇄' },
  route: { label: '隧道', cls: 'info', icon: '⇄' },
  listener: { label: '监听器', cls: 'purple', icon: '◎' },
  tool_unlock: { label: '工具解锁', cls: 'ok', icon: '🔓' },
  kb_search: { label: '知识库检索', cls: 'info', icon: '📚' },
  approval: { label: '审批', cls: 'warn', icon: '🖋' },
  report: { label: '报告', cls: 'purple', icon: '📄' },
  review: { label: '复核', cls: 'purple', icon: '🔎' },
  run_started: { label: '任务启动', cls: 'ok', icon: '🚀' },
  run_done: { label: '任务结束', cls: 'info', icon: '🏁' },
}

export function kindOf(kind: string): KindMeta {
  return kindMeta[kind] || { label: kind, cls: 'faint', icon: '•' }
}

function str(v: any, max = 160): string {
  if (v === undefined || v === null) return ''
  const s = typeof v === 'string' ? v : JSON.stringify(v)
  return s.length > max ? `${s.slice(0, max)}…` : s
}

export function evSummary(kind: string, p: any): string {
  const payload = p || {}
  switch (kind) {
    case 'turn_start':
      return `第 ${payload.turn ?? payload.Turn ?? '?'} 轮`
    case 'tool_call_start':
      return `工具: ${payload.name ?? '?'}${payload.args ? ` — ${str(payload.args, 80)}` : ''}`
    case 'tool_call_result':
      return `${payload.error ? '✗ 失败' : '✓ 成功'} ${payload.name ?? ''}${payload.error ? ` — ${str(payload.error, 160)}` : str(payload.output || payload.result, 160)}`
    case 'tool_call_args_delta':
      return `参数增量 ${payload.arg_chars ?? ''} 字符`
    case 'reasoning_delta':
    case 'text_delta':
      return str(payload.text || payload.delta, 180)
    case 'usage': {
      const cost = payload.CostUSD ?? payload.cost_usd ?? 0
      return `${payload.PromptTokens ?? payload.prompt_tokens ?? '?'} in / ${payload.OutputTokens ?? payload.output_tokens ?? payload.CompletionTokens ?? '?'} out · $${Number(cost).toFixed(5)}`
    }
    case 'submission':
      return `提交 ${payload.flag ? `${String(payload.flag).slice(0, 8)}***` : '?'} → ${payload.result || payload.status || '?'}`
    case 'finding':
      return str(payload.text || payload.title || payload.note, 180)
    case 'worker_launch':
      return `${payload.worker || ''} · ${payload.type || payload.worker_type || ''}${payload.phase ? `:${phaseLabel(payload.phase)}` : ''}${payload.intent_id ? ` · intent#${payload.intent_id}` : ''}`
    case 'worker_done':
      return `${payload.worker || ''} · turns=${payload.turns ?? '-'}${payload.error ? ` · ${str(payload.error, 100)}` : ''}`
    case 'worker_abort':
      return `${payload.worker || ''} · ${payload.reason || ''}`
    case 'scheduler_tick':
      return `tick #${payload.tick ?? ''} · running=${payload.running ?? ''}`
    case 'termination':
      return `原因: ${payload.reason || ''}`
    case 'traffic':
      return `${payload.method || ''} ${payload.host || ''}${payload.path || ''} → ${payload.status ?? ''}`
    case 'approval':
      return `${payload.tool || ''} → ${payload.status || ''}${payload.decision_by ? ` by ${payload.decision_by}` : ''}`
    case 'report':
      return `${payload.challenge_id || ''} · 证据校验 ${payload.evidence_ok ?? '?'}${payload.evidence_rejected?.length ? ` · 拒绝 ${payload.evidence_rejected.length}` : ''}${payload.redacted ? ' · 脱敏' : ''}`
    case 'review':
      return `${payload.vuln_id || payload.title || ''} → ${payload.status || payload.verdict || ''}${payload.reviewer ? ` by ${payload.reviewer}` : ''}`
    case 'run_started':
      return `${payload.mode || ''} · ${str(payload.task || payload.challenge_id || '', 120)}`
    case 'run_done':
      return payload.interrupted
        ? '用户停止'
        : payload.error
          ? `失败: ${str(payload.error, 140)}`
          : `turns=${payload.turns ?? '-'} · terminated=${payload.terminated ?? '-'} · ${payload.reason || '完成'}`
    case 'error':
      return str(payload.error || payload.message, 180)
    case 'denied':
      return `${payload.kind || ''}${payload.pattern ? ` · ${payload.pattern}` : ''}`
    case 'tool_unlock':
      return `解锁工具组: ${payload.group || ''}`
    case 'kb_search':
      return `${payload.product || ''}(${payload.hits ?? 0} 条)`
    case 'checkpoint':
      return str(payload.note || payload.action || payload, 180)
    default:
      return str(payload, 180)
  }
}

/** 时间线详情文本:普通事件输出 payload;流式事件输出拼接全文。 */
export function eventDetail(e: EventItem): string {
  const p = e.Payload || {}
  switch (e.Kind) {
    case 'reasoning_delta':
    case 'text_delta':
      return String(p.text || p.delta || '')
    case 'tool_call_args_delta':
      return String(p.arg_chars || p.args || '')
    case 'tool_call_start':
      return `工具: ${p.name || ''}${p.args ? `\n参数: ${typeof p.args === 'string' ? p.args : JSON.stringify(p.args, null, 2)}` : ''}`
    case 'tool_call_result':
      return `${p.error ? `✗ ${p.error}` : `✓ ${p.output || p.result || ''}`}`
    default:
      return JSON.stringify(p, null, 2)
  }
}

function parseArgs(args: any): any {
  if (typeof args === 'string') {
    try {
      return JSON.parse(args)
    } catch {
      return args
    }
  }
  return args
}

function compactText(v: any, max = 240): string {
  if (v === undefined || v === null) return ''
  const text = typeof v === 'string' ? v : JSON.stringify(v)
  const flat = text.replace(/\s+/g, ' ').trim()
  return flat.length > max ? `${flat.slice(0, max)}…` : flat
}

/** 工具调用摘要:优先显示 bash 命令 / submit_vulnerability 标题,再显示结果。 */
export function toolSummary(p: any): string {
  const payload = p || {}
  const name = payload.name || 'tool'
  const args = parseArgs(payload.args)
  let head = `🔧 ${name}`
  if (args && typeof args === 'object') {
    if (typeof args.command === 'string') head += `: ${compactText(args.command, 200)}`
    else if (name === 'submit_vulnerability' && args.title) head += `: ${compactText(args.title, 180)}`
    else if (name === 'grep' && args.pattern) head += `: ${compactText(args.pattern, 160)}`
    else head += `: ${compactText(args, 180)}`
  } else if (args) {
    head += `: ${compactText(args, 180)}`
  }
  if (payload.error) return `${head} → ✗ ${compactText(payload.error, 240)}`
  const output = compactText(payload.output ?? payload.result, 260)
  return output ? `${head} → ✓ ${output}` : `${head} → ✓ 完成(无输出)`
}

/** 工具调用展开详情:参数 + 完整输出。 */
export function toolDetail(p: any): string {
  const payload = p || {}
  const name = payload.name || 'tool'
  const args = parseArgs(payload.args)
  let text = `工具: ${name}\n`
  if (payload.id) text += `调用 id: ${payload.id}\n`
  text += `\n参数:\n`
  if (args && typeof args === 'object') text += JSON.stringify(args, null, 2)
  else text += String(args ?? '(无参数)')
  text += `\n\n结果:\n`
  if (payload.error) text += String(payload.error)
  else text += String(payload.output ?? payload.result ?? '(无输出)')
  return text
}

export interface MergedEvent {
  key: string
  kind: string
  mergedKind: string
  sum: string
  seqStart: number
  seqEnd: number
  ts: number
  session: string
  challenge: string
  payloads: EventItem[]
  item: EventItem | null
}

const DELTA_KINDS = new Set(['reasoning_delta', 'text_delta', 'tool_call_args_delta'])

/**
 * 将逐字增量事件合并为可读条目;同 session、相邻 seq(≤200 间隙)的同类增量合并。
 * tool_call_start 创建可累积后续 tool_call_args_delta 的条目。
 */
export function mergeTimeline(events: EventItem[]): MergedEvent[] {
  const out: MergedEvent[] = []
  // tool_call_start 没有参数(参数只在 result 里),按 call id 配对合并,
  // 避免时间线出现两条只有名字的 "调用工具 bash / 工具结果 bash"。
  const pendingTools = new Map<string, number>()
  for (const e of events) {
    if (DELTA_KINDS.has(e.Kind)) {
      const text = String((e.Payload || {} as any).text || (e.Payload || {} as any).delta || '')
      const last = out[out.length - 1]
      if (last && last.mergedKind === e.Kind && last.session === e.SessionID && e.Seq - last.seqEnd <= 200) {
        last.sum += text
        last.seqEnd = e.Seq
        last.payloads.push(e)
        continue
      }
      out.push({
        key: `m${e.Seq}`, kind: e.Kind, mergedKind: e.Kind, sum: text,
        seqStart: e.Seq, seqEnd: e.Seq, ts: e.TS, session: e.SessionID,
        challenge: e.ChallengeID, payloads: [e], item: null,
      })
      continue
    }
    if (e.Kind === 'tool_call_start') {
      const p = e.Payload || {} as any
      const name = p.name || 'tool'
      const key = `t${p.id || e.Seq}`
      out.push({
        key, kind: e.Kind, mergedKind: 'tool_call_args_delta', sum: `🔧 ${name}`,
        seqStart: e.Seq, seqEnd: e.Seq, ts: e.TS, session: e.SessionID,
        challenge: e.ChallengeID, payloads: [e], item: e,
      })
      if (p.id) pendingTools.set(`${e.SessionID}\u0000${p.id}`, out.length - 1)
      continue
    }
    if (e.Kind === 'tool_call_result') {
      const p = e.Payload || {} as any
      const idx = p.id ? pendingTools.get(`${e.SessionID}\u0000${p.id}`) : undefined
      if (idx !== undefined && out[idx] && out[idx].session === e.SessionID) {
        const start = out[idx]
        start.kind = 'tool_call_result'
        start.mergedKind = 'tool'
        start.sum = toolSummary(p)
        start.seqEnd = e.Seq
        start.payloads.push(e)
        start.item = e
        pendingTools.delete(`${e.SessionID}\u0000${p.id}`)
        continue
      }
      out.push({
        key: `t${p.id || e.Seq}`, kind: e.Kind, mergedKind: 'tool', sum: toolSummary(p),
        seqStart: e.Seq, seqEnd: e.Seq, ts: e.TS, session: e.SessionID,
        challenge: e.ChallengeID, payloads: [e], item: e,
      })
      continue
    }
    out.push({
      key: `m${e.Seq}`, kind: e.Kind, mergedKind: '', sum: evSummary(e.Kind, e.Payload),
      seqStart: e.Seq, seqEnd: e.Seq, ts: e.TS, session: e.SessionID,
      challenge: e.ChallengeID, payloads: [], item: e,
    })
  }
  return out
}
