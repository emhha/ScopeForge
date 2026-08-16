<script setup lang="ts">
// 任务流程视图:上层 Operator 运行流水(进程),中层工作区看板(方向),
// 下层攻击时间线。点击任意 operator / 方向卡 → 时间线按 session 过滤。
import { computed } from 'vue'
import { useBoardStore } from '../stores/board'
import OperatorRunbook from './OperatorRunbook.vue'
import KanbanBoard from './KanbanBoard.vue'
import TimelinePanel from './TimelinePanel.vue'

const props = defineProps<{ challenge: string; agent: string | null }>()
const emit = defineEmits<{
  (e: 'select-agent', id: string | null): void
}>()

const board = useBoardStore()

function uniqueEndpoints(pred: (s: string) => boolean): number {
  const set = new Set<string>()
  for (const c of board.cells) {
    if (pred(c.status)) set.add(`${c.asset}\u0000${c.endpoint || ''}`)
  }
  return set.size
}

const cellEndpoints = computed(() => new Set(board.cells.map((c) => c.endpoint || '/')))
const intentWithoutCell = computed(() =>
  board.intents.filter((i) => !i.target || !cellEndpoints.value.has(i.target)),
)
const planCount = computed(
  () =>
    uniqueEndpoints((s) => s === 'open') +
    intentWithoutCell.value.filter((i) => i.state === 'open' || i.state === 'pending').length,
)
const doingCount = computed(
  () =>
    uniqueEndpoints((s) => s === 'claimed') +
    intentWithoutCell.value.filter((i) => i.state === 'claimed').length,
)

const flowState = computed(() => {
  const running = board.workers.filter((w) => w.status === 'running')
  const synth = board.workers.find((w) => w.worker_type === 'synthesizer')
  const hasSynth = !!synth
  if (planCount.value === 0 && doingCount.value === 0) {
    if (running.some((w) => w.worker_type === 'synthesizer')) {
      return { text: '方向已清空 · Synthesizer 正在汇总收尾', cls: 'ok' }
    }
    if (hasSynth && synth?.status === 'done') {
      return { text: '方向已清空 · Synthesizer 汇总完成', cls: 'info' }
    }
    if (running.length === 0) {
      return { text: '方向已清空 · 所有 operator 已结束,应进入 Synthesizer', cls: 'warn' }
    }
  }
  return {
    text: `计划 ${planCount.value} · 进行中 ${doingCount.value} · 运行中 operator ${running.length}`,
    cls: 'ok',
  }
})
</script>

<template>
  <div>
    <OperatorRunbook @select-agent="emit('select-agent', $event)" />
    <KanbanBoard @select-agent="emit('select-agent', $event)" />

    <div class="panel" style="border-left: 3px solid var(--accent)">
      <div class="row wrap">
        <span class="badge" :class="flowState.cls">{{ flowState.text }}</span>
        <span class="muted">
          Scout 产出方向 → Executor 认领执行 → 方向确认/归档 → 清空后 Synthesizer 汇总;
          Observer 在运行间隙旁路监控。
        </span>
      </div>
    </div>

    <TimelinePanel
      :challenge="props.challenge"
      :agent="props.agent"
      @select-session="emit('select-agent', $event)"
      @clear-agent="emit('select-agent', null)"
    />
  </div>
</template>
