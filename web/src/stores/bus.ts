// 全局 SSE 实时层:App 级单例 EventSource + ChallengeID 分桶。
// 断线指数退避重连,after=<lastSeq> 续拉;45s 无数据(服务端 25s 心跳)主动重连。
import { defineStore } from 'pinia'
import type { EventItem } from '../api/types'

const BUCKET_MAX = 5000

// SSE 连接对象与去重集合保持模块级(非响应式/非代理)。
let es: EventSource | null = null
let reconnectTimer: ReturnType<typeof setTimeout> | undefined
let noDataTimer: ReturnType<typeof setTimeout> | undefined
let attempts = 0
const seen = new Set<number>()

export const useBus = defineStore('bus', {
  state: () => ({
    connected: false,
    retry: 0,
    lastSeq: 0,
    started: false,
    buckets: {} as Record<string, EventItem[]>,
    pending: [] as EventItem[],
    revision: 0,
  }),
  getters: {
    globalEvents(state): EventItem[] {
      return state.buckets['__global__'] || []
    },
  },
  actions: {
    eventsFor(challenge: string): EventItem[] {
      // 桶内按追加顺序天然升序(App 初始历史 reverse 后追加 + SSE after 续推),
      // 返回浅拷贝即可;高频事件下避免每个事件都触发 O(n log n) 排序。
      return [...(this.buckets[challenge] || [])]
    },
    start(after = 0) {
      if (this.started) return
      this.started = true
      this.lastSeq = after
      this.connect()
    },
    stop() {
      this.started = false
      this.connected = false
      window.clearTimeout(reconnectTimer)
      window.clearTimeout(noDataTimer)
      reconnectTimer = undefined
      noDataTimer = undefined
      const current = es
      es = null
      current?.close()
    },
    append(e: EventItem) {
      if (!e || typeof e.Seq !== 'number' || e.Seq <= 0) return
      if (seen.has(e.Seq)) return
      seen.add(e.Seq)
      const key = e.ChallengeID || '__global__'
      const bucket = this.buckets[key] || (this.buckets[key] = [])
      bucket.push(e)
      if (bucket.length > BUCKET_MAX) bucket.splice(0, bucket.length - BUCKET_MAX)
      this.pending.push(e)
      this.revision++
    },
    drainNew(): EventItem[] {
      if (!this.pending.length) return []
      const out = this.pending
      this.pending = []
      return out
    },
    connect() {
      if (!this.started) return
      window.clearTimeout(reconnectTimer)
      const url = `/api/v1/events/stream?after=${this.lastSeq}`
      const stream = new EventSource(url)
      es = stream

      stream.onopen = () => {
        attempts = 0
        this.connected = true
        this.retry = 0
        this.armNoDataTimer()
      }
      stream.onmessage = (ev) => {
        this.armNoDataTimer()
        try {
          const data = JSON.parse(ev.data)
          // 心跳帧 {"hb":1}
          if (!data || typeof data.Seq !== 'number') return
          const event = data as EventItem
          if (event.Seq > this.lastSeq) {
            this.lastSeq = event.Seq
            this.append(event)
          }
        } catch {
          // 忽略坏帧/心跳
        }
      }
      stream.onerror = () => {
        this.clearNoDataTimer()
        stream.close()
        if (es === stream) es = null
        if (!this.started) return
        this.connected = false
        attempts++
        this.retry = attempts
        const delay = Math.min(30000, 1000 * 2 ** Math.min(attempts, 5))
        reconnectTimer = window.setTimeout(() => this.connect(), delay)
      }
    },
    armNoDataTimer() {
      this.clearNoDataTimer()
      noDataTimer = window.setTimeout(() => {
        this.clearNoDataTimer()
        this.connected = false
        const current = es
        es = null
        current?.close()
        if (this.started) this.connect()
      }, 45000)
    },
    clearNoDataTimer() {
      window.clearTimeout(noDataTimer)
      noDataTimer = undefined
    },
  },
})
