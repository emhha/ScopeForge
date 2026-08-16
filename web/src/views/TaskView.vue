<script setup lang="ts">
// 任务工作台:核心页。看板 / 覆盖矩阵 / 漏洞账本 / 黑板 / 时间线 / 报告 / 终端。
import { computed, onUnmounted, ref, watch } from 'vue'
import { useRoute } from 'vue-router'
import { api } from '../api'
import { useBoardStore } from '../stores/board'
import { useTasksStore } from '../stores/tasks'
import { useBus } from '../stores/bus'
import { dur, fmtTime, fmtUsd, statusMeta } from '../utils/format'
import ProcessView from '../components/ProcessView.vue'
import CoverageMatrix from '../components/CoverageMatrix.vue'
import LedgerPanel from '../components/LedgerPanel.vue'
import BlackboardPanel from '../components/BlackboardPanel.vue'
import TimelinePanel from '../components/TimelinePanel.vue'
import ReportPanel from '../components/ReportPanel.vue'
import TerminalPanel from '../components/TerminalPanel.vue'
import EvidenceDrawer from '../components/EvidenceDrawer.vue'

type Tab = 'process' | 'coverage' | 'ledger' | 'blackboard' | 'timeline' | 'report' | 'terminal'

const route = useRoute()
const board = useBoardStore()
const tasks = useTasksStore()
const bus = useBus()

const id = computed(() => String(route.params.id || ''))
const task = computed(() => tasks.byId[id.value])
const tab = ref<Tab>('process')
const selectedAgent = ref<string | null>(null)
const evidenceRef = ref<string | null>(null)
const stopping = ref(false)
const actionError = ref('')

const stats = computed(() => board.ledgerStats)

watch(
  id,
  async (challenge) => {
    if (!challenge) return
    selectedAgent.value = null
    evidenceRef.value = null
    await board.attach(challenge)
    if (!task.value) tasks.refresh()
  },
  { immediate: true },
)

onUnmounted(() => board.detach())

function selectAgent(agent: string | null) {
  selectedAgent.value = agent
  if (agent && tab.value !== 'process' && tab.value !== 'timeline') tab.value = 'timeline'
}

function statusOf() {
  const s = task.value?.status || 'running'
  return statusMeta[s] || { label: s, cls: 'faint', icon: '•' }
}

async function stopTask() {
  if (!window.confirm(`停止任务 ${id.value}?`)) return
  stopping.value = true
  actionError.value = ''
  try {
    await api.stopRun(id.value)
    await tasks.refresh()
  } catch (e: any) {
    actionError.value = String(e?.message || e)
  } finally {
    stopping.value = false
  }
}

const TABS = computed<{ key: Tab; label: string; badge?: string }[]>(() => [
  { key: 'process', label: '⚙ 流程', badge: String(board.workers.length) },
  { key: 'coverage', label: '▦ 覆盖矩阵', badge: String(board.cells.length) },
  { key: 'ledger', label: '🚩 漏洞账本', badge: String(board.vulnerabilities.length) },
  { key: 'blackboard', label: '▤ 黑板' },
  { key: 'timeline', label: '⚡ 时间线' },
  { key: 'report', label: '📄 报告' },
  { key: 'terminal', label: '🖥 终端' },
])
</script>

<template>
  <div class="page">
    <!-- 任务头 -->
    <div class="panel task-hero" :class="`st-${task?.status || 'running'}`">
      <div class="row wrap">
        <h1 class="page-title mono" style="font-size: 17px">{{ id }}</h1>
        <span class="badge" :class="statusOf().cls">{{ statusOf().icon }} {{ statusOf().label }}</span>
        <span v-if="task?.mode" class="tag mono">{{ task.mode }}</span>
        <span v-if="board.interest && (board.interest.severity_min || board.interest.cwes?.length)" class="badge flag">
          🎯 interest {{ board.interest.severity_min || board.interest.cwes?.join(',') }}
        </span>
        <span class="grow"></span>
        <span v-if="bus.connected" class="badge ok">实时</span>
        <button v-if="task?.status === 'running'" class="btn danger sm" :disabled="stopping" @click="stopTask">
          {{ stopping ? '停止中…' : '⏹ 停止任务' }}
        </button>
        <button class="btn sm ghost" :disabled="board.loading" @click="board.refresh()">刷新</button>
      </div>

      <div v-if="actionError" class="badge bad mt8">{{ actionError }}</div>
      <div v-if="task?.description" class="mt8" style="white-space: pre-wrap; color: var(--muted); font-size: 13px">{{ task.description }}</div>

      <div class="row wrap mt8" style="gap: 8px">
        <span class="tag">启动 {{ fmtTime(task?.started_at) }}</span>
        <span class="tag mono">耗时 {{ dur(task?.started_at, task?.finished_at) }}</span>
        <span class="tag mono">turns {{ task?.turns || board.spend?.Turns || 0 }}</span>
        <span class="tag mono">成本 {{ fmtUsd(task?.cost_usd || board.spend?.CostUSD) }}</span>
        <span class="tag">✓ accepted {{ task?.accepted ?? stats.accepted }}</span>
        <span class="tag">待 submitted {{ task?.vuln_submitted ?? stats.submitted }}</span>
        <span class="tag mono">board seq {{ board.asOfSeq }}</span>
        <span v-if="board.spend?.PromptTokens" class="tag mono">tokens {{ board.spend.PromptTokens }} in / {{ board.spend.OutputTokens || 0 }} out</span>
      </div>
    </div>

    <div v-if="board.lastError" class="badge bad mb8">详情加载:{{ board.lastError }}</div>

    <!-- tabs -->
    <div class="tabbar">
      <button
        v-for="t in TABS"
        :key="t.key"
        :class="{ active: tab === t.key }"
        @click="tab = t.key"
      >
        {{ t.label }}<span v-if="t.badge" class="muted" style="margin-left: 4px">({{ t.badge }})</span>
      </button>
      <span class="grow"></span>
      <span v-if="selectedAgent" class="badge ok">时间线过滤: {{ selectedAgent.slice(0, 16) }}</span>
    </div>

    <!-- content -->
    <div v-if="board.loading && board.facts.length === 0 && board.cells.length === 0" class="panel muted">加载任务详情…</div>

    <ProcessView v-else-if="tab === 'process'" :challenge="id" :agent="selectedAgent" @select-agent="selectAgent" />

    <CoverageMatrix v-else-if="tab === 'coverage'" @refresh="board.refresh()" />

    <LedgerPanel v-else-if="tab === 'ledger'" @open-evidence="evidenceRef = $event" />

    <BlackboardPanel v-else-if="tab === 'blackboard'" @select-agent="selectAgent" @open-evidence="evidenceRef = $event" />

    <TimelinePanel v-else-if="tab === 'timeline'" :challenge="id" :agent="selectedAgent" @select-session="selectAgent" @clear-agent="selectedAgent = null" />

    <ReportPanel v-else-if="tab === 'report'" :challenge="id" />

    <TerminalPanel v-else-if="tab === 'terminal'" :challenge="id" />

    <EvidenceDrawer :challenge="id" :ref-id="evidenceRef" @close="evidenceRef = null" />
  </div>
</template>
