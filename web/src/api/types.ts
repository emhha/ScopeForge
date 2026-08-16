// ScopeForge 后端 REST/SSE 契约类型。
// 事件项字段保持大写(Go event.Event JSON 序列化未加 tag);REST 聚合端点使用 snake_case。

export interface TaskCard {
  id: string
  mode: string
  status: 'running' | 'done' | 'failed' | 'terminated' | 'interrupted' | string
  description: string
  started_at: number
  finished_at: number
  turns: number
  cost_usd: number
  accepted: number
  vuln_submitted: number
  last_event_at: number
}

export interface Fact {
  id: number
  seq: number
  prefix: string
  text: string
  weight: number
  state: string
  superseded_by: number
  created_by: string
  evidence_ref: string
  challenge_id: string
  created_at: number
  cwe?: string
  asset?: string
  endpoint?: string
  severity?: string
}

export interface Intent {
  id: number
  seq: number
  challenge_id: string
  text: string
  weight: number
  state: string
  claimed_by: string
  claimed_at: number
  created_by: string
  created_at: number
  target?: string
  approach?: string
}

export interface WorkerInfo {
  id: string
  challenge_id: string
  worker_type: string
  phase: string
  provider: string
  model: string
  session_id: string
  status: string
  handoff: string
  intent_id: number
  last_progress_at: number
  has_correct_submission: boolean
  created_at: number
  finished_at: number
}

export type LedgerStatus = 'submitted' | 'accepted' | 'duplicate' | 'false_positive' | 'rejected' | string

export interface LedgerEntry {
  id: number
  seq: number
  challenge_id: string
  cwe: string
  asset: string
  endpoint: string
  severity: string
  title: string
  description: string
  evidence_ref: string
  platform_ref: string
  status: LedgerStatus
  submitted_at: number
}

export interface CoverageCell {
  cwe: string
  asset: string
  endpoint: string
  status: 'open' | 'claimed' | 'confirmed' | 'dead' | 'skipped' | string
  form: string
  skip_reason: string
  updated_at: number
}

export interface Interest {
  severity_min: string
  cwes: string[]
}

export interface SpendInfo {
  PromptTokens?: number
  CacheHitTokens?: number
  OutputTokens?: number
  ReasoningTokens?: number
  CostUSD?: number
  Turns?: number
}

export interface ChallengeDetail {
  challenge_id: string
  facts: Fact[]
  intents: Intent[]
  workers: WorkerInfo[]
  vulnerabilities: LedgerEntry[]
  cells: CoverageCell[]
  interest?: Interest
  spend?: SpendInfo
}

export interface EventItem {
  Seq: number
  TS: number
  Kind: string
  Payload: Record<string, any> | null
  SessionID: string
  ChallengeID: string
  TraceID?: string
  Level?: string
}

export interface EventPage {
  events: EventItem[] | null
  latest: number
  /** 合并前原始事件条数;history merge=1 时用于判断是否还有更早分页。 */
  raw_count?: number
}

export interface SessionInfo {
  id: string
  kind: string
  provider: string
  model: string
  rewrite_version: number
  updated_at: number
}

export interface SystemInfo {
  ts: number
  providers: { name: string; available: boolean }[]
  routes: RouteInfo[]
  listeners: ListenerInfo[]
  docker_available?: boolean
  sandbox_image?: string
  kb_entries?: { builtin: number; plugins: number }
}

export interface RouteInfo {
  id: string
  proto: string
  target: string
  local_addr: string
  status: string
  backend: string
  created: number
  client_cmd?: string
}

export interface ListenerInfo {
  id: string
  proto: string
  addr: string
  port: number
  created: number
}

export interface LedgerRow {
  role: string
  model: string
  prompt_tokens: number
  cache_hit_tokens: number
  output_tokens: number
  reasoning_tokens: number
  cost_usd: number
  calls: number
}
