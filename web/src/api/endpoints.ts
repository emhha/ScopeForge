// 全部 REST 端点的唯一入口(与 internal/api 当前路由对齐)。
import { authToken, req } from './client'
import type {
  ChallengeDetail, EventPage, LedgerRow, SessionInfo, SystemInfo, TaskCard,
} from './types'

export const api = {
  health: () => req<{ status: string; version: string; time: number }>('/health'),

  tasks: () => req<{ tasks: TaskCard[]; ts: number; generated: string }>('/api/v1/tasks'),
  task: (id: string) => req<ChallengeDetail>(`/api/v1/challenges/${encodeURIComponent(id)}`),
  evidence: (id: string, ref: string) =>
    req<any>(`/api/v1/challenges/${encodeURIComponent(id)}/evidence/${encodeURIComponent(ref)}`),

  events: (after = 0, limit = 500) =>
    req<EventPage>(`/api/v1/events?after=${after}&limit=${limit}`),
  eventsBefore: (before: number, limit = 500, session = '') =>
    req<EventPage>(`/api/v1/events?before=${before}&limit=${limit}${session ? `&session=${encodeURIComponent(session)}` : ''}`),
  eventsRecent: (limit = 1000) =>
    req<EventPage>(`/api/v1/events?before=${Number.MAX_SAFE_INTEGER}&limit=${limit}`),
  // 历史翻页:merge=1 由后端聚合 reasoning/text 增量,响应从逐 token 压缩为消息块。
  eventsBeforeMerged: (before: number, limit = 1000, session = '') =>
    req<EventPage>(`/api/v1/events?before=${before}&limit=${limit}&merge=1${session ? `&session=${encodeURIComponent(session)}` : ''}`),
  sessions: () => req<{ sessions: SessionInfo[] }>('/api/v1/sessions'),

  // 看板人工操作(2.25)
  boardIntentState: (id: string, intentId: number, state: 'open' | 'done' | 'dead' | 'pending') =>
    req<{ status: string }>(`/api/v1/challenges/${encodeURIComponent(id)}/board/intent/${intentId}`, {
      method: 'POST',
      body: JSON.stringify({ state }),
    }),
  boardCellAction: (
    id: string,
    payload: { cwe?: string; asset: string; endpoint?: string; action: 'skip' | 'dead' | 'reopen'; reason?: string },
  ) =>
    req<{ status: string }>(`/api/v1/challenges/${encodeURIComponent(id)}/board/cell`, {
      method: 'POST',
      body: JSON.stringify(payload),
    }),

  // 任务控制:当前后端仅提供 stop(task 模式取消上下文)
  stopRun: (id: string) =>
    req<{ run_id: string; action: string }>(`/api/v1/runs/${encodeURIComponent(id)}/stop`, { method: 'POST' }),

  // 系统 / 配置 / 成本账本
  ledger: () => req<{ rows: LedgerRow[] }>('/api/v1/ledger'),
  system: () => req<SystemInfo>('/api/v1/system'),
  config: () => req<{ config_yaml: string }>('/api/v1/config'),
  putConfig: (yaml: string) =>
    req<{ status: string; path: string; note: string }>('/api/v1/config', {
      method: 'PUT',
      headers: { 'Content-Type': 'text/yaml' },
      body: yaml,
    }),

  // Web 发起任务:当前仅 task 模式;platform_url 作为 focusURL 注入聚焦模式。
  runTask: (payload: { mode: 'task'; task: string; platform_url?: string }) =>
    req<{ run_id: string; mode: string }>('/api/v1/run', {
      method: 'POST',
      body: JSON.stringify(payload),
    }),

  ptyUrl(write: boolean): string {
    const proto = location.protocol === 'https:' ? 'wss' : 'ws'
    const base = `${proto}://${location.host}/ws/pty`
    if (!write) return base
    const token = authToken()
    return token ? `${base}?write=1&token=${encodeURIComponent(token)}` : `${base}?write=1`
  },
}
