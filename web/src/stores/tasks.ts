// 任务列表 store:GET /api/v1/tasks + SSE 事件驱动的轻量修正。
import { defineStore } from 'pinia'
import { api, type TaskCard } from '../api'

let refreshTimer: ReturnType<typeof setTimeout> | undefined

export const useTasksStore = defineStore('tasks', {
  state: () => ({
    tasks: [] as TaskCard[],
    loading: false,
    lastError: '',
  }),
  getters: {
    running: (s) => s.tasks.filter((t) => t.status === 'running'),
    finished: (s) => s.tasks.filter((t) => t.status !== 'running'),
    byId: (s) => {
      const map: Record<string, TaskCard> = {}
      for (const t of s.tasks) map[t.id] = t
      return map
    },
    totals: (s) => ({
      accepted: s.tasks.reduce((sum, t) => sum + (t.accepted || 0), 0),
      submitted: s.tasks.reduce((sum, t) => sum + (t.vuln_submitted || 0), 0),
      cost: s.tasks.reduce((sum, t) => sum + (t.cost_usd || 0), 0),
      turns: s.tasks.reduce((sum, t) => sum + (t.turns || 0), 0),
    }),
  },
  actions: {
    async refresh() {
      this.loading = true
      try {
        const r = await api.tasks()
        this.tasks = r.tasks || []
        this.lastError = ''
      } catch (e: any) {
        this.lastError = String(e?.message || e)
      } finally {
        this.loading = false
      }
    },
    scheduleRefresh(delay = 1200) {
      window.clearTimeout(refreshTimer)
      refreshTimer = window.setTimeout(() => this.refresh(), delay)
    },
    onBusEvent(kind: string, payload: any, challengeID = '') {
      if (kind === 'run_started') {
        const id = payload?.challenge_id || challengeID
        if (id && !this.byId[id]) this.scheduleRefresh(600)
        return
      }
      if (kind === 'run_done') {
        const id = payload?.challenge_id || challengeID
        const task = id ? this.byId[id] : undefined
        if (task) {
          task.status = payload?.interrupted ? 'interrupted' : payload?.error ? 'failed' : payload?.terminated ? 'terminated' : 'done'
          task.finished_at = Math.floor(Date.now() / 1000)
          this.scheduleRefresh(800)
        } else {
          this.scheduleRefresh(600)
        }
      }
    },
  },
})
