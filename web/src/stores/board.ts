// 任务详情 store:GET /api/v1/challenges/{id} 聚合黑板/账本/覆盖矩阵,
// SSE 事件去抖刷新 + 人工看板写操作。
import { defineStore } from 'pinia'
import { api } from '../api'
import type {
  ChallengeDetail, CoverageCell, Fact, Intent, Interest, LedgerEntry, SpendInfo, WorkerInfo,
} from '../api/types'

const POLL_MS = 30000

export const useBoardStore = defineStore('board', {
  state: () => ({
    challenge: '',
    facts: [] as Fact[],
    intents: [] as Intent[],
    workers: [] as WorkerInfo[],
    vulnerabilities: [] as LedgerEntry[],
    cells: [] as CoverageCell[],
    interest: null as Interest | null,
    spend: null as SpendInfo | null,
    loading: false,
    lastError: '',
    lastRefresh: 0,
    asOfSeq: 0,
    pollTimer: 0 as ReturnType<typeof setInterval> | 0,
    debounceTimer: 0 as ReturnType<typeof setTimeout> | 0,
    actionError: '',
  }),
  getters: {
    cellCounts(state) {
      const counts = { open: 0, claimed: 0, confirmed: 0, dead: 0, skipped: 0 }
      for (const c of state.cells) if (counts[c.status as keyof typeof counts] !== undefined) counts[c.status as keyof typeof counts]++
      return counts
    },
    ledgerStats(state) {
      const stats = { accepted: 0, submitted: 0, duplicate: 0, false_positive: 0, rejected: 0, total: state.vulnerabilities.length }
      for (const v of state.vulnerabilities) {
        if (v.status in stats) stats[v.status as keyof typeof stats]++
      }
      return stats
    },
    workerForIntent(state) {
      return (intentId: number) => {
        const ws = state.workers.filter((w) => w.intent_id === intentId)
        return ws.find((w) => w.status === 'running') || ws[0]
      }
    },
  },
  actions: {
    async attach(id: string) {
      if (this.challenge === id) {
        await this.refresh()
        return
      }
      this.detach()
      this.challenge = id
      this.facts = []
      this.intents = []
      this.workers = []
      this.vulnerabilities = []
      this.cells = []
      this.interest = null
      this.spend = null
      await this.refresh()
      this.pollTimer = setInterval(() => this.refresh(), POLL_MS)
    },
    detach() {
      if (this.pollTimer) clearInterval(this.pollTimer)
      this.pollTimer = 0
      window.clearTimeout(this.debounceTimer)
      this.debounceTimer = 0
      this.challenge = ''
    },
    async refresh() {
      if (!this.challenge) return
      this.loading = true
      try {
        const d: ChallengeDetail = await api.task(this.challenge)
        this.facts = d.facts || []
        this.intents = d.intents || []
        this.workers = d.workers || []
        this.vulnerabilities = d.vulnerabilities || []
        this.cells = d.cells || []
        this.interest = d.interest || null
        this.spend = d.spend || null
        this.asOfSeq = Math.max(
          ...(this.facts.map((f) => f.seq)),
          ...(this.intents.map((i) => i.seq || 0)),
          0,
        )
        this.lastRefresh = Date.now()
        this.lastError = ''
      } catch (e: any) {
        this.lastError = String(e?.message || e)
      } finally {
        this.loading = false
      }
    },
    onBusEvent(kind: string, challengeID = '') {
      if (!this.challenge) return
      if (challengeID && challengeID !== this.challenge) return
      if (['board_change', 'finding', 'submission', 'worker_done', 'worker_launch', 'termination', 'checkpoint', 'report', 'review'].includes(kind)) {
        window.clearTimeout(this.debounceTimer)
        this.debounceTimer = window.setTimeout(() => this.refresh(), 900)
      }
    },
    async setIntentState(intentId: number, state: 'open' | 'done' | 'dead' | 'pending') {
      this.actionError = ''
      try {
        await api.boardIntentState(this.challenge, intentId, state)
        await this.refresh()
      } catch (e: any) {
        this.actionError = String(e?.message || e)
        throw e
      }
    },
    async setCellState(cell: CoverageCell, action: 'skip' | 'dead' | 'reopen', reason?: string) {
      this.actionError = ''
      try {
        await api.boardCellAction(this.challenge, {
          cwe: cell.cwe,
          asset: cell.asset,
          endpoint: cell.endpoint,
          action,
          reason,
        })
        await this.refresh()
      } catch (e: any) {
        this.actionError = String(e?.message || e)
        throw e
      }
    },
  },
})
