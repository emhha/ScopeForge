// 通用格式化与状态元数据(纯函数)。

export function fmtTime(t?: number | null): string {
  return t ? new Date(t * 1000).toLocaleString() : '-'
}

export function fmtClock(t?: number | null): string {
  return t ? new Date(t * 1000).toLocaleTimeString('zh-CN', { hour12: false }) : '-'
}

export function fmtDate(t?: number | null): string {
  return t ? new Date(t * 1000).toLocaleDateString('zh-CN') : '-'
}

export function fmtUsd(v?: number | null): string {
  return `$${(v || 0).toFixed(4)}`
}

/** start/end 均为秒级 unix;end 缺省为当前时间。 */
export function dur(start?: number | null, end?: number | null): string {
  if (!start) return '-'
  const e = end || Math.floor(Date.now() / 1000)
  const s = Math.max(0, e - start)
  const d = Math.floor(s / 86400)
  const h = Math.floor((s % 86400) / 3600)
  const m = Math.floor((s % 3600) / 60)
  const sec = s % 60
  if (d > 0) return `${d}d${h}h`
  if (h > 0) return `${h}h${m}m`
  if (m > 0) return `${m}m${sec}s`
  return `${sec}s`
}

export function durShort(seconds: number): string {
  const s = Math.max(0, Math.floor(seconds))
  if (s < 60) return `${s}s`
  if (s < 3600) return `${Math.floor(s / 60)}m`
  if (s < 86400) return `${Math.floor(s / 3600)}h`
  return `${Math.floor(s / 86400)}d`
}

export const statusMeta: Record<string, { label: string; cls: string; icon: string }> = {
  running: { label: '运行中', cls: 'ok', icon: '●' },
  done: { label: '完成', cls: 'info', icon: '✓' },
  terminated: { label: '已终止', cls: 'warn', icon: '⛔' },
  interrupted: { label: '已停止', cls: 'warn', icon: '■' },
  failed: { label: '失败', cls: 'bad', icon: '✗' },
}

export const severityMeta: Record<string, { label: string; cls: string; rank: number }> = {
  critical: { label: '严重', cls: 'bad', rank: 5 },
  high: { label: '高危', cls: 'bad', rank: 4 },
  medium: { label: '中危', cls: 'warn', rank: 3 },
  low: { label: '低危', cls: 'info', rank: 2 },
  info: { label: '信息', cls: 'faint', rank: 1 },
}

export const cellStatusMeta: Record<string, { label: string; cls: string; icon: string }> = {
  open: { label: '未覆盖', cls: 'hyp', icon: '○' },
  claimed: { label: '进行中', cls: 'ok', icon: '◐' },
  confirmed: { label: '已覆盖', cls: 'ok', icon: '●' },
  dead: { label: '已排除', cls: 'dead', icon: '✕' },
  skipped: { label: '已跳过', cls: 'warn', icon: '↷' },
}

export const ledgerStatusMeta: Record<string, { label: string; cls: string; icon: string }> = {
  submitted: { label: '待回执', cls: 'warn', icon: '◐' },
  accepted: { label: '已确认', cls: 'ok', icon: '✓' },
  duplicate: { label: '重复', cls: 'info', icon: '≡' },
  false_positive: { label: '误报', cls: 'bad', icon: '✗' },
  rejected: { label: '已驳回', cls: 'dead', icon: '↩' },
}

export const factStateMeta: Record<string, { label: string; cls: string }> = {
  candidate: { label: '候选', cls: 'warn' },
  confirmed: { label: '确认', cls: 'ok' },
  superseded: { label: '已取代', cls: 'dead' },
  falsified: { label: '证伪', cls: 'dead' },
}

export const intentStateMeta: Record<string, { label: string; cls: string }> = {
  open: { label: '计划', cls: 'hyp' },
  pending: { label: '待条件', cls: 'warn' },
  claimed: { label: '进行中', cls: 'ok' },
  done: { label: '已完成', cls: 'info' },
  dead: { label: '归档', cls: 'dead' },
}

export const prefixMeta: Record<string, { label: string; cls: string }> = {
  'obs:': { label: '观察', cls: 'obs' },
  'vuln:': { label: '漏洞', cls: 'vuln' },
  'flag:': { label: 'Flag', cls: 'flag' },
  'dead:': { label: '排除', cls: 'dead' },
  'hint:': { label: '提示', cls: 'hint' },
  'hyp:': { label: '假设', cls: 'hyp' },
}

export function prefixCls(p?: string): string {
  return prefixMeta[p || '']?.cls || 'obs'
}

export function shortId(id: string, n = 8): string {
  if (!id) return ''
  return id.length <= n + 2 ? id : `${id.slice(0, n)}…`
}
