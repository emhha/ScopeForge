<script setup lang="ts">
// Operator 运行流水:按 worker 持久化记录展示 Scout / Executor / Analyst /
// Synthesizer 的派发顺序、运行状态与产出摘要。Observer 无 workers 表记录,
// 从 observer_review checkpoint 事件推导旁路监控活动。
import { computed } from 'vue'
import { useBoardStore } from '../stores/board'
import { useTaskEvents } from '../composables/useTaskEvents'
import { phaseLabel } from '../utils/events'
import { dur, fmtClock } from '../utils/format'

const emit = defineEmits<{ (e: 'select-agent', id: string): void }>()

const board = useBoardStore()
const challenge = computed(() => board.challenge)
const { events: taskEvents } = useTaskEvents(() => challenge.value, () => null, 2000)

const workerStatusMeta: Record<string, { label: string; cls: string; icon: string }> = {
  running: { label: '运行中', cls: 'ok', icon: '●' },
  done: { label: '完成', cls: 'info', icon: '✓' },
  failed: { label: '失败', cls: 'bad', icon: '✗' },
  aborted: { label: '中止', cls: 'warn', icon: '■' },
  orphaned: { label: '残留', cls: 'dead', icon: '◆' },
}

const phaseMeta: Record<string, { label: string; cls: string; icon: string }> = {
  scout: { label: 'Scout', cls: 'info', icon: '🔭' },
  executor: { label: 'Executor', cls: 'ok', icon: '⚡' },
  analyst: { label: 'Analyst', cls: 'purple', icon: '🧩' },
  synthesizer: { label: 'Synthesizer', cls: 'cyan', icon: '📦' },
}

function stopLabel(s: string): string {
  return ({ exhausted: '方向耗尽', intent_done: '方向完成', conclude: '任务收尾' } as Record<string, string>)[s] || s || '完成'
}

interface OperatorRun {
  key: string
  id: string
  type: string
  phase: string
  status: string
  seq: number
  title: string
  sub: string
  start: number
  finish: number
  lastProgress: number
  outcome: string
  outcomeCls: string
  icon: string
  cls: string
}

const runs = computed<OperatorRun[]>(() => {
  const ws = [...board.workers].sort((a, b) => (a.created_at || 0) - (b.created_at || 0))
  const events = taskEvents.value
  return ws.map((w, idx) => {
    const done = events
      .filter((e) => e.Kind === 'worker_done' && (e.Payload as any)?.worker === w.id)
      .sort((a, b) => a.Seq - b.Seq)
      .pop()
    const p = (done?.Payload || {}) as any
    const status = w.status || 'running'
    const sm = workerStatusMeta[status] || { label: status, cls: 'faint', icon: '•' }
    let outcome = status === 'running' ? '执行中' : `${sm.label} · ${stopLabel(p.stop_reason || '')}`
    let outcomeCls = sm.cls
    if (p?.status === 'unparseable' || p?.error) {
      outcome = `契约失败: ${String(p.error || p.status || '').slice(0, 80)}`
      outcomeCls = 'bad'
    } else if (status === 'done') {
      outcome = `${sm.label}: 发现 ${p.findings ?? 0} · 新方向 ${p.new_intents ?? 0} · 排除 ${p.dead_ends ?? 0}`
      outcomeCls = (p.findings || 0) > 0 ? 'ok' : 'info'
    }

    const intent = w.intent_id ? board.intents.find((i) => i.id === w.intent_id) : undefined
    const type = w.worker_type === 'synthesizer' ? 'synthesizer' : 'operator'
    const ph = phaseLabel(w.phase)
    const meta = phaseMeta[type === 'synthesizer' ? 'synthesizer' : ph] || phaseMeta.scout

    return {
      key: w.id,
      id: w.id,
      type,
      phase: ph,
      status,
      seq: idx + 1,
      title: type === 'synthesizer' ? 'Synthesizer · 汇总收尾' : `${meta.label} · ${ph || 'operator'}`,
      sub: intent?.text || (type === 'synthesizer' ? '汇总账本/覆盖矩阵,生成报告' : '攻击面测绘 / 方向认领'),
      start: w.created_at || 0,
      finish: w.finished_at || 0,
      lastProgress: w.last_progress_at || 0,
      outcome,
      outcomeCls,
      icon: meta.icon,
      cls: meta.cls,
    }
  })
})

const observerReviews = computed(() =>
  taskEvents.value.filter(
    (e) => e.Kind === 'checkpoint' && (e.Payload as any)?.action === 'observer_review',
  ),
)

const observerLast = computed(() => {
  const arr = observerReviews.value
  if (!arr.length) return null
  return arr[arr.length - 1]
})

const counts = computed(() => {
  const c = { scout: 0, executor: 0, analyst: 0, synthesizer: 0, running: 0 }
  for (const w of board.workers) {
    if (w.worker_type === 'synthesizer') c.synthesizer++
    else if (phaseLabel(w.phase) === 'executor') c.executor++
    else if (phaseLabel(w.phase) === 'analyst') c.analyst++
    else c.scout++
    if (w.status === 'running') c.running++
  }
  return c
})
</script>

<template>
  <div class="panel">
    <div class="panel-title">
      ⚙ Operator 运行流水
      <span class="grow"></span>
      <span class="badge info">Scout {{ counts.scout }}</span>
      <span class="badge ok">Executor {{ counts.executor }}</span>
      <span class="badge purple">Analyst {{ counts.analyst }}</span>
      <span class="badge cyan">Synthesizer {{ counts.synthesizer }}</span>
      <span v-if="counts.running" class="badge ok pulse">{{ counts.running }} running</span>
    </div>

    <div class="runbook">
      <div
        v-for="run in runs"
        :key="run.key"
        class="run-card card clickable"
        :class="[`run-${run.status}`, `run-${run.cls}`]"
        @click="emit('select-agent', run.id)"
      >
        <div class="row" style="gap: 6px">
          <span class="run-seq">{{ run.seq }}</span>
          <span class="badge" :class="run.cls">{{ run.icon }} {{ run.title }}</span>
          <span class="grow"></span>
          <span class="badge" :class="(workerStatusMeta[run.status] || {}).cls">
            {{ (workerStatusMeta[run.status] || {}).icon }} {{ (workerStatusMeta[run.status] || {}).label }}
          </span>
        </div>
        <div class="muted mt4 ellipsis" :title="run.sub">{{ run.sub }}</div>
        <div class="row wrap mt8" style="gap: 6px">
          <span class="tag mono">⏱ {{ fmtClock(run.start) }} → {{ run.finish ? fmtClock(run.finish) : '…' }}</span>
          <span v-if="run.status === 'running'" class="tag mono">心跳 {{ dur(run.lastProgress) }}</span>
          <span v-else-if="run.start && run.finish" class="tag mono">耗时 {{ dur(run.start, run.finish) }}</span>
          <span class="grow"></span>
          <button class="btn xs">时间线 →</button>
        </div>
        <div class="mt4" style="font-size: 12px" :class="run.outcomeCls === 'bad' ? 'text-danger' : run.outcomeCls === 'ok' ? 'text-ok' : 'muted'">
          {{ run.outcome }}
        </div>
      </div>

      <div
        v-if="observerReviews.length"
        class="run-card card clickable run-observer"
        @click="emit('select-agent', 'observer')"
      >
        <div class="row" style="gap: 6px">
          <span class="run-seq">◈</span>
          <span class="badge purple">🧭 Observer · 旁路监控</span>
          <span class="grow"></span>
          <span class="badge ok">● 活跃</span>
        </div>
        <div class="muted mt4">审查黑板质量与覆盖建议;不占用热路径。</div>
        <div class="row wrap mt8" style="gap: 6px">
          <span class="tag">审查 {{ observerReviews.length }} 次</span>
          <span v-if="observerLast" class="tag mono">最近 {{ fmtClock(observerLast.TS) }}</span>
          <span class="grow"></span>
          <button class="btn xs">时间线 →</button>
        </div>
      </div>
      <div v-else class="run-card card run-observer idle">
        <div class="row" style="gap: 6px">
          <span class="run-seq">◈</span>
          <span class="badge faint">🧭 Observer · 旁路监控</span>
          <span class="grow"></span>
          <span class="badge faint">无审查事件</span>
        </div>
        <div class="muted mt4">配置 observer_every_n_turns 后,在 operator 运行间隙自动审查。</div>
      </div>
    </div>
  </div>
</template>

<style scoped>
.runbook { display: grid; grid-template-columns: repeat(auto-fill, minmax(280px, 1fr)); gap: 10px; }
.run-card { border-left: 3px solid var(--border2); }
.run-card.run-running { border-left-color: var(--ok); }
.run-card.run-done { border-left-color: var(--info); }
.run-card.run-failed { border-left-color: var(--bad); }
.run-card.run-aborted, .run-card.run-orphaned { border-left-color: var(--warn); }
.run-card.run-observer { border-left-color: var(--purple); }
.run-card.run-observer.idle { opacity: .6; }
.run-seq {
  display: inline-flex; align-items: center; justify-content: center;
  min-width: 22px; height: 22px; padding: 0 5px; border-radius: 11px;
  background: var(--panel3); color: var(--muted); font-family: var(--mono); font-size: 11px;
}
.badge.cyan { background: rgba(77, 208, 225, .12); color: var(--cyan); }
</style>
