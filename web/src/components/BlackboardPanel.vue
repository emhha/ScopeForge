<script setup lang="ts">
// 黑板面板:facts(混合结构化)+ intents + workers 持久化心跳。
import { computed } from 'vue'
import { useBoardStore } from '../stores/board'
import type { Fact } from '../api'
import { factStateMeta, fmtTime, intentStateMeta, prefixCls, prefixMeta, shortId } from '../utils/format'
import { phaseLabel } from '../utils/events'

const emit = defineEmits<{
  (e: 'open-evidence', ref: string): void
  (e: 'select-agent', id: string | null): void
}>()

const board = useBoardStore()

const facts = computed(() => [...board.facts].sort((a, b) => b.weight - a.weight || b.seq - a.seq))
const intents = computed(() => [...board.intents].sort((a, b) => b.weight - a.weight || (b.seq || 0) - (a.seq || 0)))
const workers = computed(() => [...board.workers].sort((a, b) => (a.created_at || 0) - (b.created_at || 0)))

function workerLabel(w: { worker_type: string; phase?: string }) {
  return w.phase ? `${w.worker_type}:${phaseLabel(w.phase)}` : w.worker_type
}
const SEV_RANK: Record<string, number> = { critical: 5, high: 4, medium: 3, low: 2, info: 1 }

function factGoal(f: Fact): boolean {
  const interest = board.interest
  if (!interest || f.state !== 'confirmed') return false
  if (!interest.severity_min && !(interest.cwes || []).length) return false
  if (interest.severity_min && SEV_RANK[f.severity || 'info'] < SEV_RANK[interest.severity_min]) return false
  if ((interest.cwes || []).length && !interest.cwes.includes(f.cwe || '')) return false
  return true
}

function workerAge(w: { last_progress_at: number }) {
  if (!w.last_progress_at) return ''
  const s = Math.floor(Date.now() / 1000 - w.last_progress_at)
  if (s < 60) return `${s}s`
  if (s < 3600) return `${Math.floor(s / 60)}m`
  return `${Math.floor(s / 3600)}h`
}
</script>

<template>
  <div class="panel">
    <span class="badge info">▤ 黑板 ≠ 看板</span>
    <span class="muted">黑板是系统持久化的事实/意图/worker 心跳;「流程」页是这些数据的过程化投影,按 operator 和方向归并展示。</span>
  </div>

  <div class="grid" style="grid-template-columns: 1fr 1fr; gap: 14px">
    <!-- facts -->
    <div class="panel">
      <div class="panel-title">
        ▤ 事实 Facts
        <span class="grow"></span>
        <span class="badge faint">{{ facts.length }}</span>
      </div>
      <div v-if="facts.length === 0" class="empty">等待 agent 写入结构化发现</div>
      <div class="scroll-y" style="max-height: 640px">
        <div v-for="f in facts" :key="f.id" class="card mb8" style="border-left: 3px solid var(--border2)">
          <div class="row wrap" style="gap: 6px">
            <span class="badge" :class="prefixCls(f.prefix)">{{ prefixMeta[f.prefix]?.label || f.prefix || 'obs' }}</span>
            <span v-if="f.state !== 'confirmed'" class="badge" :class="factStateMeta[f.state]?.cls">{{ factStateMeta[f.state]?.label || f.state }}</span>
            <span v-if="f.severity" class="badge" :class="f.severity === 'critical' || f.severity === 'high' ? 'bad' : 'warn'">{{ f.severity }}</span>
            <span v-if="factGoal(f)" class="badge flag">🎯</span>
            <span class="grow"></span>
            <span class="faint mono">{{ (f.weight * 100).toFixed(0) }}%</span>
          </div>
          <div class="mt4" style="font-size: 13px">{{ f.text }}</div>
          <div v-if="f.asset || f.endpoint || f.cwe" class="row wrap mt4" style="gap: 4px">
            <span v-if="f.cwe" class="tag mono">{{ f.cwe }}</span>
            <span v-if="f.asset" class="tag mono">{{ f.asset }}</span>
            <span v-if="f.endpoint" class="tag mono">{{ f.endpoint }}</span>
          </div>
          <div class="row wrap mt8" style="gap: 6px">
            <button v-if="f.created_by" class="btn xs" @click="emit('select-agent', f.created_by)">⏱ {{ shortId(f.created_by) }}</button>
            <button v-if="f.evidence_ref" class="btn xs" @click="emit('open-evidence', f.evidence_ref)">证据 →</button>
            <span class="grow"></span>
            <span class="faint mono">seq {{ f.seq }} · {{ fmtTime(f.created_at) }}</span>
          </div>
        </div>
      </div>
    </div>

    <div class="col" style="gap: 14px">
      <!-- workers -->
      <div class="panel">
        <div class="panel-title">
          ⚙ Workers
          <span class="grow"></span>
          <span class="badge faint">{{ workers.length }}</span>
        </div>
        <div v-if="workers.length === 0" class="empty">暂无 worker 记录</div>
        <div v-else class="scroll-y" style="max-height: 250px">
          <div v-for="w in workers" :key="w.id" class="card mb8">
            <div class="row wrap" style="gap: 6px">
              <span class="badge" :class="w.status === 'running' ? 'ok' : w.status === 'done' ? 'info' : 'dead'">{{ w.status }}</span>
              <span class="badge hyp">{{ workerLabel(w) }}</span>
              <span class="grow"></span>
              <span v-if="w.status === 'running'" class="faint mono">{{ workerAge(w) }}</span>
            </div>
            <div class="row wrap mt4" style="gap: 6px">
              <button class="btn xs" @click="emit('select-agent', w.id)">⏱ 时间线</button>
              <span class="tag mono">{{ shortId(w.id) }}</span>
              <span v-if="w.provider" class="tag">{{ w.provider }}</span>
              <span v-if="w.intent_id" class="tag mono">intent#{{ w.intent_id }}</span>
              <span v-if="w.has_correct_submission" class="badge ok">已确认提交</span>
            </div>
            <div v-if="w.handoff" class="muted mt8 ellipsis" :title="w.handoff">{{ w.handoff }}</div>
          </div>
        </div>
      </div>

      <!-- intents -->
      <div class="panel">
        <div class="panel-title">
          🎯 Intents
          <span class="grow"></span>
          <span class="badge faint">{{ intents.length }}</span>
        </div>
        <div v-if="intents.length === 0" class="empty">暂无意图</div>
        <div v-else class="scroll-y" style="max-height: 340px">
          <div v-for="i in intents" :key="i.id" class="card mb8">
            <div class="row wrap" style="gap: 6px">
              <span class="badge" :class="intentStateMeta[i.state]?.cls">{{ intentStateMeta[i.state]?.label || i.state }}</span>
              <span v-if="i.target" class="tag mono">{{ i.target }}</span>
              <span v-if="i.approach" class="tag mono">{{ i.approach }}</span>
              <span class="grow"></span>
              <span class="faint mono">w{{ i.weight.toFixed(1) }}</span>
            </div>
            <div class="mt4" style="font-size: 13px">{{ i.text }}</div>
            <div class="row mt4">
              <button v-if="i.claimed_by" class="btn xs" @click="emit('select-agent', i.claimed_by)">⏱ {{ shortId(i.claimed_by) }}</button>
              <span class="grow"></span>
              <span class="faint mono">seq {{ i.seq }}</span>
            </div>
          </div>
        </div>
      </div>
    </div>
  </div>
</template>
