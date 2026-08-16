<script setup lang="ts">
// 全局壳:导航 + SSE 单例 + 事件分发 + Token 管理。
// 启动时先拉最近 1000 条事件(倒序 → 转正序),再以 latest 游标接 SSE,
// 避免只拉最早一页造成中间缺口。
import { onMounted, onUnmounted, ref } from 'vue'
import { api, authToken, setAuthToken } from './api'
import { useBus } from './stores/bus'
import { useTasksStore } from './stores/tasks'
import { useBoardStore } from './stores/board'

const bus = useBus()
const tasks = useTasksStore()
const board = useBoardStore()

const showToken = ref(false)
const tokenInput = ref('')
const hasToken = ref(!!authToken())
let dispatchTimer: ReturnType<typeof setInterval> | undefined

function dispatch() {
  const events = bus.drainNew()
  for (const e of events) {
    tasks.onBusEvent(e.Kind, e.Payload || {}, e.ChallengeID)
    board.onBusEvent(e.Kind, e.ChallengeID)
  }
}

onMounted(async () => {
  try {
    const page = await api.eventsRecent(1000)
    const events = [...(page.events || [])].reverse()
    for (const e of events) bus.append(e)
    bus.start(page.latest || 0)
  } catch {
    bus.start(0)
  }
  tasks.refresh()
  dispatchTimer = setInterval(dispatch, 350)
  document.addEventListener('visibilitychange', onVisibility)
})

function onVisibility() {
  if (document.visibilityState === 'visible') tasks.refresh()
}

onUnmounted(() => {
  bus.stop()
  if (dispatchTimer) clearInterval(dispatchTimer)
  document.removeEventListener('visibilitychange', onVisibility)
})

function saveToken() {
  setAuthToken(tokenInput.value.trim())
  hasToken.value = !!authToken()
  tokenInput.value = ''
  showToken.value = false
}
</script>

<template>
  <nav class="navbar">
    <router-link to="/" class="brand">◈ ScopeForge</router-link>
    <router-link to="/" class="nav-link">总览</router-link>
    <router-link to="/system" class="nav-link">系统</router-link>
    <router-link to="/config" class="nav-link">配置</router-link>

    <span class="grow"></span>

    <span v-if="tasks.running.length" class="badge ok">
      <span class="pulse" style="width:8px;height:8px;border-radius:50%;background:var(--ok)"></span>
      {{ tasks.running.length }} 运行中
    </span>
    <span class="nav-conn" :class="bus.connected ? 'ok' : 'bad'">
      {{ bus.connected ? '实时' : `重连 ${bus.retry}` }}
    </span>
    <button class="btn sm ghost" @click="showToken = !showToken">
      {{ hasToken ? 'Token ✓' : 'Token' }}
    </button>
  </nav>

  <div v-if="showToken" class="modal-mask" @click.self="showToken = false">
    <div class="modal" style="width: 440px">
      <h3 style="margin-top: 0">API Token</h3>
      <p class="muted">
        看板人工操作 / 配置保存 / 终端写模式等写接口需要 Bearer token,
        与 serve.auth.token_env 对应。Token 仅保存在浏览器 localStorage。
      </p>
      <input v-model="tokenInput" type="password" placeholder="粘贴 token" class="w100 mb8" />
      <div class="row" style="justify-content: flex-end">
        <button class="btn" @click="showToken = false">取消</button>
        <button class="btn primary" @click="saveToken">保存</button>
      </div>
    </div>
  </div>

  <router-view />
</template>
