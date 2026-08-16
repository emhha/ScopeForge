<script setup lang="ts">
// 工作区看板:只呈现"探索方向"这一种工作项。
// 方向来源 = coverage_matrix 格子(按 asset+endpoint 聚合为一个端点方向卡)
// 和 intents。facts / 漏洞账本 / workers 不再混入四列——它们分别属于
// 黑板、漏洞账本和 Operator 运行流水,避免同一件事被拆成多张卡。
import { computed, ref } from 'vue'
import { useBoardStore } from '../stores/board'
import { useTaskEvents } from '../composables/useTaskEvents'
import type { CoverageCell, Intent } from '../api'
import { phaseLabel } from '../utils/events'
import { cellStatusMeta, intentStateMeta } from '../utils/format'

const emit = defineEmits<{ (e: 'select-agent', id: string | null): void }>()

const board = useBoardStore()
const challenge = computed(() => board.challenge)
const { events: taskEvents } = useTaskEvents(() => challenge.value, () => null, 2000)

type LaneKey = 'todo' | 'doing' | 'done' | 'archive'

interface WorkCard {
  key: string
  kind: 'intent' | 'endpoint'
  title: string
  sub: string
  state: string
  stateLabel: string
  stateCls: string
  weight: number
  ts: number
  tags: { text: string; cls?: string; mono?: boolean }[]
  agentId?: string
  agentLabel?: string
  intentId?: number
  cells: CoverageCell[]
}

// ---------------- operator/session 关联 ----------------
function workerTypeLabel(w: { worker_type: string; phase?: string }): string {
  return phaseLabel(w.phase)
    ? `${w.worker_type}:${phaseLabel(w.phase)}`
    : w.worker_type
}

function agentMeta(id?: string): { label: string; running: boolean } | undefined {
  if (!id) return undefined
  const w = board.workers.find((x) => x.id === id)
  if (w) return { label: workerTypeLabel(w), running: w.status === 'running' }
  if (id === 'observer') return { label: 'observer', running: false }
  return { label: id, running: false }
}

function scoutWorkerId(): string | undefined {
  const scouts = board.workers.filter((w) => w.phase === 'scout')
  return (scouts.find((w) => w.status === 'running') || scouts[scouts.length - 1])?.id
}

// finding 事件 → worker 映射(1s 快照,避免每个 SSE 事件全量扫描)。
const findingWorkerMap = computed(() => {
  const exact = new Map<string, string>()
  const fallback = new Map<string, string>()
  for (const e of taskEvents.value) {
    if (e.Kind !== 'finding') continue
    const p = e.Payload || {}
    if (typeof p.worker !== 'string' || !p.asset) continue
    const base = `${p.asset}\u0000${p.endpoint || ''}`
    fallback.set(base, p.worker)
    if (p.cwe) exact.set(`${base}\u0000${p.cwe}`, p.worker)
  }
  return { exact, fallback }
})

function findingWorker(cell: CoverageCell): string | undefined {
  const base = `${cell.asset}\u0000${cell.endpoint || ''}`
  return findingWorkerMap.value.exact.get(`${base}\u0000${cell.cwe}`) || findingWorkerMap.value.fallback.get(base)
}

function claimedIntentFor(cell: CoverageCell): Intent | undefined {
  return board.intents.find((i) => i.state === 'claimed' && i.target === cell.endpoint)
}

function cellAgent(cell: CoverageCell): string | undefined {
  if (cell.status === 'claimed') return claimedIntentFor(cell)?.claimed_by
  if (cell.status === 'confirmed') return findingWorker(cell)
  if (cell.status === 'open') return scoutWorkerId()
  return undefined
}

// ---------------- 覆盖格:按 asset+endpoint 聚合 ----------------
const endpointCards = computed<WorkCard[]>(() => {
  const groups = new Map<string, CoverageCell[]>()
  for (const cell of board.cells) {
    const key = `${cell.asset}\u0000${cell.endpoint || ''}`
    const arr = groups.get(key)
    if (arr) arr.push(cell)
    else groups.set(key, [cell])
  }

  return [...groups.entries()].map(([, cells]) => {
    const first = cells[0]
    const asset = first.asset
    const endpoint = first.endpoint || '/'

    // 端点方向状态:claimed 优先(有 Executor 正在跑),其次 confirmed,
    // 再 open;全部 dead/skipped 才归档。这样聚焦任务确认漏洞后不会因为
    // 攻击面预置的其它候选 CWE 还 open 而继续留在计划列。
    let state = 'open'
    if (cells.some((c) => c.status === 'claimed')) state = 'claimed'
    else if (cells.some((c) => c.status === 'confirmed')) state = 'confirmed'
    else if (cells.some((c) => c.status === 'dead')) state = 'dead'
    else if (cells.some((c) => c.status === 'skipped')) state = 'skipped'

    const sm = cellStatusMeta[state] || { label: state, cls: '', icon: '•' }
    const confirmedCWEs = [...new Set(cells.filter((c) => c.status === 'confirmed').map((c) => c.cwe).filter(Boolean))]
    const openCWEs = [...new Set(cells.filter((c) => c.status === 'open').map((c) => c.cwe).filter(Boolean))]
    const skipReason = cells.find((c) => c.skip_reason)?.skip_reason || ''
    const agentId = cells.map(cellAgent).find(Boolean)
    const agent = agentMeta(agentId)

    return {
      key: `ep${asset}|${endpoint}`,
      kind: 'endpoint',
      title: `${confirmedCWEs.length ? confirmedCWEs.join('/') : first.cwe || '攻击面'} @ ${endpoint}`,
      sub: asset,
      state,
      stateLabel: sm.label,
      stateCls: sm.cls,
      weight: state === 'claimed' ? 1 : state === 'open' ? 0.6 : 0.3,
      ts: Math.max(...cells.map((c) => c.updated_at || 0)),
      tags: [
        ...(confirmedCWEs.length ? [{ text: `✓ ${confirmedCWEs.join(' / ')}`, cls: 'ok', mono: true }] : []),
        ...(openCWEs.length ? [{ text: `候选 ${openCWEs.join(' / ')}`, cls: 'hyp', mono: true }] : []),
        ...(first.form ? [{ text: first.form, mono: true }] : []),
        ...(skipReason ? [{ text: skipReason, cls: 'warn' }] : []),
      ],
      agentId,
      agentLabel: agent?.label,
      cells,
    }
  })
})

const intentCards = computed<WorkCard[]>(() => {
  // 同一端点的 intent 与 coverage 格子是同一件事的两种表示;矩阵格子是主
  // 表示,避免 Scout 建格后计划/进行中出现两张重复卡。
  const cellEndpoints = new Set(board.cells.map((c) => c.endpoint || '/'))
  return board.intents
    .filter((i) => !i.target || !cellEndpoints.has(i.target))
    .map((i) => {
    const sm = intentStateMeta[i.state] || { label: i.state || 'open', cls: '' }
    const agentId = i.claimed_by || i.created_by || undefined
    const agent = agentMeta(agentId)
    return {
      key: `i${i.id}`,
      kind: 'intent',
      title: i.text,
      sub: [i.target, i.approach].filter(Boolean).join(' · '),
      state: i.state,
      stateLabel: sm.label,
      stateCls: sm.cls,
      weight: i.weight,
      ts: i.claimed_at || i.created_at || 0,
      tags: [
        { text: `w${i.weight.toFixed(1)}`, mono: true },
        ...(agent ? [{ text: agent.label, mono: true }] : []),
      ],
      agentId,
      agentLabel: agent?.label,
      intentId: i.id,
      cells: [],
    }
    })
})

const lanes = computed<Record<LaneKey, WorkCard[]>>(() => {
  const g: Record<LaneKey, WorkCard[]> = { todo: [], doing: [], done: [], archive: [] }
  for (const card of [...endpointCards.value, ...intentCards.value]) {
    switch (card.kind) {
      case 'endpoint':
        if (card.state === 'open') g.todo.push(card)
        else if (card.state === 'claimed') g.doing.push(card)
        else if (card.state === 'confirmed') g.done.push(card)
        else g.archive.push(card)
        break
      case 'intent':
        if (card.state === 'claimed') g.doing.push(card)
        else if (card.state === 'done') g.done.push(card)
        else if (card.state === 'dead') g.archive.push(card)
        else g.todo.push(card)
        break
    }
  }
  for (const key of ['todo', 'doing'] as LaneKey[]) g[key].sort((a, b) => b.weight - a.weight)
  for (const key of ['done', 'archive'] as LaneKey[]) g[key].sort((a, b) => b.ts - a.ts)
  return g
})

const LANE_META: { key: LaneKey; label: string; hint: string }[] = [
  { key: 'todo', label: '📥 计划', hint: '待认领方向' },
  { key: 'doing', label: '🔵 进行中', hint: 'Executor 已认领' },
  { key: 'done', label: '✅ 已完成', hint: '已确认覆盖' },
  { key: 'archive', label: '🗄 归档', hint: '排除/无产出' },
]

// ---------------- 人工拖拽/按钮 ----------------
const dragCard = ref<WorkCard | null>(null)
const dragOver = ref<LaneKey | null>(null)
const message = ref('')

function onDragStart(card: WorkCard) {
  if (card.kind !== 'intent' && card.kind !== 'endpoint') return
  dragCard.value = card
  dragOver.value = null
  message.value = ''
}
function onDragEnd() {
  dragCard.value = null
  dragOver.value = null
}

async function applyDrop(target: LaneKey, card: WorkCard) {
  message.value = ''
  try {
    if (card.kind === 'intent') {
      if (target === 'doing') {
        message.value = '进行中由调度认领驱动,不可手动放入'
        return
      }
      const state = target === 'todo' ? 'open' : target === 'done' ? 'done' : 'dead'
      if (state !== card.state) await board.setIntentState(card.intentId!, state)
      return
    }
    if (target === 'doing') {
      message.value = '进行中由调度认领驱动,不可手动放入'
      return
    }
    if (target === 'archive') {
      const actionable = card.cells.filter((c) => c.status === 'open' || c.status === 'claimed')
      if (!actionable.length) return
      await cellActionCells(actionable, 'skip', '人工拖拽归档')
    } else if (target === 'todo') {
      const actionable = card.cells.filter((c) => c.status === 'skipped' || c.status === 'dead')
      if (!actionable.length) return
      await cellActionCells(actionable, 'reopen')
    }
  } catch (e: any) {
    message.value = String(e?.message || e)
  }
}

function onDrop(lane: LaneKey) {
  const card = dragCard.value
  const target = dragOver.value
  dragCard.value = null
  dragOver.value = null
  if (card && target === lane) applyDrop(lane, card)
}

async function moveIntent(card: WorkCard, state: 'open' | 'done' | 'dead') {
  try {
    await board.setIntentState(card.intentId!, state)
  } catch (e: any) {
    message.value = String(e?.message || e)
  }
}

async function cellActionCells(cells: CoverageCell[], action: 'skip' | 'dead' | 'reopen', reason = '') {
  const promptReason = action === 'reopen' ? '' : reason || window.prompt(action === 'skip' ? '跳过原因:' : '排除原因:') || ''
  for (const cell of cells) {
    await board.setCellState(cell, action, promptReason || undefined)
  }
}

async function cellAction(card: WorkCard, action: 'skip' | 'dead' | 'reopen') {
  const cells = card.cells.filter((c) =>
    action === 'reopen' ? c.status === 'skipped' || c.status === 'dead' : c.status === 'open' || c.status === 'claimed',
  )
  if (cells.length) await cellActionCells(cells, action)
}

function clickCard(card: WorkCard) {
  if (card.agentId) emit('select-agent', card.agentId)
}
</script>

<template>
  <div>
    <div v-if="board.actionError || message" class="badge bad mb8">{{ board.actionError || message }}</div>

    <div class="kanban">
      <section
        v-for="lane in LANE_META"
        :key="lane.key"
        class="kanban-lane"
        :class="{ 'drag-over': dragOver === lane.key }"
        @dragover.prevent="dragOver = lane.key"
        @dragleave="dragOver = null"
        @drop.prevent="onDrop(lane.key)"
      >
        <div class="lane-head">
          {{ lane.label }}
          <span class="badge faint">{{ lanes[lane.key].length }}</span>
          <span class="grow"></span>
          <span class="hint">{{ lane.hint }}</span>
        </div>
        <div class="lane-body">
          <div
            v-for="card in lanes[lane.key]"
            :key="card.key"
            class="board-card"
            :class="{ dragging: dragCard?.key === card.key }"
            :draggable="card.kind === 'intent' || card.kind === 'endpoint'"
            @click="clickCard(card)"
            @dragstart="onDragStart(card)"
            @dragend="onDragEnd"
          >
            <div class="row" style="gap: 6px">
              <span class="badge" :class="card.kind === 'endpoint' ? 'info' : 'hyp'">
                {{ card.kind === 'endpoint' ? '覆盖方向' : '意图' }}
              </span>
              <span class="grow"></span>
              <span class="badge" :class="card.stateCls">{{ card.stateLabel }}</span>
            </div>
            <div class="title mt4">{{ card.title }}</div>
            <div v-if="card.sub" class="sub mt4">{{ card.sub }}</div>
            <div class="meta">
              <span v-for="tag in card.tags" :key="tag.text" class="tag" :class="[tag.cls || '', { mono: tag.mono }]">{{ tag.text }}</span>
            </div>
            <div class="actions">
              <button
                v-if="card.agentId"
                class="btn xs"
                title="查看该方向对应 operator 时间线"
                @click.stop="emit('select-agent', card.agentId!)"
              >⏱ {{ card.agentLabel || card.agentId.slice(0, 14) }}</button>
              <span class="grow"></span>
              <template v-if="card.kind === 'intent' && card.state !== 'claimed'">
                <button v-if="card.state !== 'open'" class="btn xs" @click.stop="moveIntent(card, 'open')">计划</button>
                <button v-if="card.state !== 'done'" class="btn xs" @click.stop="moveIntent(card, 'done')">完成</button>
                <button v-if="card.state !== 'dead'" class="btn xs" @click.stop="moveIntent(card, 'dead')">归档</button>
              </template>
              <template v-if="card.kind === 'endpoint'">
                <button v-if="card.state === 'open' || card.state === 'claimed'" class="btn xs" @click.stop="cellAction(card, 'skip')">跳过</button>
                <button v-if="card.state === 'open' || card.state === 'claimed'" class="btn xs" @click.stop="cellAction(card, 'dead')">排除</button>
                <button v-if="card.state === 'skipped' || card.state === 'dead'" class="btn xs" @click.stop="cellAction(card, 'reopen')">重开</button>
              </template>
            </div>
          </div>
          <div v-if="lanes[lane.key].length === 0" class="empty" style="padding: 14px">
            {{ lane.key === 'doing' ? '无进行中方向' : lane.key === 'todo' ? '暂无计划方向' : lane.key === 'done' ? '暂无已确认覆盖' : '暂无归档' }}
          </div>
        </div>
      </section>
    </div>

    <div class="legend mt8">
      <span class="item"><span class="badge info">覆盖方向</span> 一个端点 = 一张卡(内部 CWE 候选在标签中)</span>
      <span class="item"><span class="badge hyp">意图</span> Scout / Observer 补充的探索建议</span>
      <span class="item">点击卡片 → 下方时间线过滤到对应 operator</span>
    </div>
  </div>
</template>
