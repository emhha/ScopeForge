<script setup lang="ts">
// 总览:运行中任务横幅 + 历史任务表 + 发起任务入口。
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import { api, type TaskCard } from '../api'
import { useTasksStore } from '../stores/tasks'
import { useBus } from '../stores/bus'
import { dur, fmtTime, fmtUsd, statusMeta } from '../utils/format'
import LaunchTaskModal from '../components/LaunchTaskModal.vue'

const tasks = useTasksStore()
const bus = useBus()
const router = useRouter()

const showLaunch = ref(false)
const stopping = ref('')
const actionError = ref('')
const now = ref(Math.floor(Date.now() / 1000))
let pollTimer: ReturnType<typeof setInterval> | undefined
let clockTimer: ReturnType<typeof setInterval> | undefined

onMounted(() => {
  tasks.refresh()
  pollTimer = setInterval(() => tasks.refresh(), 5000)
  clockTimer = setInterval(() => { now.value = Math.floor(Date.now() / 1000) }, 1000)
})
onUnmounted(() => {
  clearInterval(pollTimer)
  clearInterval(clockTimer)
})

const running = computed(() => tasks.running)
const finished = computed(() => tasks.finished)
const totals = computed(() => tasks.totals)

function statusOf(t: TaskCard) {
  return statusMeta[t.status] || { label: t.status, cls: 'faint', icon: '•' }
}

async function stopTask(t: TaskCard) {
  if (!window.confirm(`停止任务 ${t.id}?取消上下文后不可恢复。`)) return
  stopping.value = t.id
  actionError.value = ''
  try {
    await api.stopRun(t.id)
    await tasks.refresh()
  } catch (e: any) {
    actionError.value = String(e?.message || e)
  } finally {
    stopping.value = ''
  }
}

function onStarted(id: string) {
  tasks.scheduleRefresh(400)
  router.push(`/task/${id}`)
}
</script>

<template>
  <div class="page">
    <div class="page-head">
      <h1 class="page-title">任务总览</h1>
      <span class="page-sub">SRC 第一性 · 提交漏洞只记账,终止 = 预算 / 覆盖收敛 / 穷尽声明</span>
      <span class="grow"></span>
      <span class="badge" :class="bus.connected ? 'ok' : 'bad'">{{ bus.connected ? 'SSE 实时' : `重连 ${bus.retry}` }}</span>
      <button class="btn primary" @click="showLaunch = true">+ 发起任务</button>
    </div>

    <div v-if="tasks.lastError || actionError" class="badge bad mb8">{{ tasks.lastError || actionError }}</div>

    <div class="grid cards mb16" style="grid-template-columns: repeat(auto-fit, minmax(150px, 1fr))">
      <div class="card stat-card">
        <div class="k">运行中</div>
        <div class="v text-ok">{{ running.length }}</div>
        <div class="s">SSE 实时状态</div>
      </div>
      <div class="card stat-card">
        <div class="k">历史任务</div>
        <div class="v">{{ finished.length }}</div>
        <div class="s">全部 challenge_id 聚合</div>
      </div>
      <div class="card stat-card">
        <div class="k">已确认漏洞</div>
        <div class="v text-ok">{{ totals.accepted }}</div>
        <div class="s">accepted</div>
      </div>
      <div class="card stat-card">
        <div class="k">待回执</div>
        <div class="v text-warn">{{ totals.submitted }}</div>
        <div class="s">submitted</div>
      </div>
      <div class="card stat-card">
        <div class="k">累计成本</div>
        <div class="v">{{ fmtUsd(totals.cost) }}</div>
        <div class="s">{{ totals.turns }} turns</div>
      </div>
    </div>

    <!-- 运行中 -->
    <div v-if="running.length" class="panel" style="border-color: rgba(52,199,89,.35)">
      <div class="panel-title">
        <span class="pulse" style="width:9px;height:9px;border-radius:50%;background:var(--ok)"></span>
        运行中任务
        <span class="grow"></span>
        <span class="badge ok">{{ running.length }}</span>
      </div>
      <div class="grid cards">
        <router-link v-for="t in running" :key="t.id" :to="`/task/${t.id}`" class="card clickable task-card" :class="`st-${t.status}`">
          <div class="row">
            <strong class="ellipsis mono" style="flex:1">{{ t.id }}</strong>
            <span class="badge ok">{{ statusOf(t).label }}</span>
          </div>
          <div v-if="t.description" class="muted mt4 ellipsis" :title="t.description">{{ t.description }}</div>
          <div class="row wrap mt8" style="gap: 6px">
            <span class="tag">⏱ {{ dur(t.started_at, now) }}</span>
            <span class="tag mono">turns {{ t.turns }}</span>
            <span class="tag mono">{{ fmtUsd(t.cost_usd) }}</span>
            <span class="tag">✓ {{ t.accepted }}</span>
            <span v-if="t.vuln_submitted" class="tag">待 {{ t.vuln_submitted }}</span>
            <span class="grow"></span>
            <button class="btn sm danger" :disabled="stopping === t.id" @click.prevent="stopTask(t)">
              {{ stopping === t.id ? '停止中…' : '⏹ 停止' }}
            </button>
          </div>
        </router-link>
      </div>
    </div>

    <!-- 历史 -->
    <div class="panel">
      <div class="panel-title">
        历史任务
        <span class="grow"></span>
        <button class="btn sm ghost" :disabled="tasks.loading" @click="tasks.refresh()">刷新</button>
      </div>
      <div v-if="finished.length === 0 && running.length === 0" class="empty">
        <div class="big">◈</div>
        暂无任务 — 点击右上角「发起任务」输入授权目标。
      </div>
      <div v-else-if="finished.length === 0" class="empty">暂无已完成任务</div>
      <div v-else class="table-wrap">
        <table>
          <thead>
            <tr>
              <th>任务</th><th>状态</th><th>模式</th><th>启动</th><th>耗时</th>
              <th>turns</th><th>成本</th><th>确认 / 待回执</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="t in finished" :key="t.id" style="cursor: pointer" @click="router.push(`/task/${t.id}`)">
              <td>
                <router-link :to="`/task/${t.id}`" class="mono">{{ t.id }}</router-link>
                <div v-if="t.description" class="muted" style="font-size: 11px" :title="t.description">{{ t.description }}</div>
              </td>
              <td><span class="badge" :class="statusOf(t).cls">{{ statusOf(t).icon }} {{ statusOf(t).label }}</span></td>
              <td class="mono">{{ t.mode || '-' }}</td>
              <td class="muted mono nowrap">{{ fmtTime(t.started_at) }}</td>
              <td class="mono">{{ dur(t.started_at, t.finished_at) }}</td>
              <td class="mono">{{ t.turns }}</td>
              <td class="mono">{{ fmtUsd(t.cost_usd) }}</td>
              <td>
                <span class="badge" :class="t.accepted ? 'ok' : 'faint'">✓ {{ t.accepted }}</span>
                <span v-if="t.vuln_submitted" class="badge warn" style="margin-left: 4px">待 {{ t.vuln_submitted }}</span>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </div>

    <LaunchTaskModal v-if="showLaunch" @close="showLaunch = false" @started="onStarted" />
  </div>
</template>

<style scoped>
.task-card.st-running { border-left: 3px solid var(--ok); }
.task-card.st-done { border-left: 3px solid var(--info); }
.task-card.st-terminated, .task-card.st-interrupted { border-left: 3px solid var(--warn); }
.task-card.st-failed { border-left: 3px solid var(--bad); }
</style>
