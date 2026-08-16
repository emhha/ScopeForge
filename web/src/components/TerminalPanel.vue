<script setup lang="ts">
// tmux 观测终端:xterm.js + /ws/pty。
// 默认只读;勾选写模式后经 query token 鉴权(浏览器 WS 不能带自定义 header)。
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { Terminal } from 'xterm'
import 'xterm/css/xterm.css'
import { api, authToken } from '../api'

const props = defineProps<{ challenge: string }>()

const el = ref<HTMLDivElement | null>(null)
const writeMode = ref(false)
const connected = ref(false)
const session = ref('')
const sessions = ref<string[]>([])
const fatal = ref('')

let term: Terminal | null = null
let ws: WebSocket | null = null
let reconnectTimer: ReturnType<typeof setTimeout> | undefined
let captureTimer: ReturnType<typeof setInterval> | undefined
let attempts = 0
let stopped = false
let lastError = ''

const defaultSession = computed(() => props.challenge.slice(0, 40).replace(/[^A-Za-z0-9._-]/g, '-') || 'pf-task')

function writeLine(text: string) {
  term?.write(`\r\n${text}\r\n`)
}

function armCapture() {
  if (captureTimer) clearInterval(captureTimer)
  captureTimer = setInterval(() => {
    if (connected.value && ws?.readyState === WebSocket.OPEN) {
      ws.send(JSON.stringify({ type: 'tmux', action: 'capture', session: session.value }))
    }
  }, 2000)
}

function connect() {
  if (stopped) return
  window.clearTimeout(reconnectTimer)
  ws?.close()
  const url = api.ptyUrl(writeMode.value)
  const socket = new WebSocket(url)
  ws = socket

  socket.onopen = () => {
    attempts = 0
    connected.value = true
    if (!session.value) session.value = defaultSession.value
    socket.send(JSON.stringify({ type: 'tmux', action: 'list' }))
    socket.send(JSON.stringify({ type: 'tmux', action: 'capture', session: session.value }))
  }
  socket.onmessage = (ev) => {
    try {
      const msg = JSON.parse(ev.data)
      if (msg.type === 'pong') return
      if (msg.type === 'output' && msg.action === 'capture' && msg.session === session.value) {
        term?.write(`\x1b[2J\r\n${msg.data || '(空)'}\r\n`)
      } else if (msg.type === 'output' && msg.action === 'list') {
        sessions.value = String(msg.data || '').split('\n').filter(Boolean)
      } else if (msg.type === 'error') {
        const line = String(msg.cmd || '')
        if (line !== lastError) {
          lastError = line
          writeLine(`[终端] ${line}`)
        }
        if (/executable file not found|not found in \$PATH|No such file/i.test(line)) {
          fatal.value = `终端不可用:${line}`
        }
      }
    } catch {
      // 忽略坏帧
    }
  }
  socket.onclose = () => {
    connected.value = false
    if (stopped) return
    attempts++
    const delay = Math.min(30000, 1000 * 2 ** Math.min(attempts, 5))
    reconnectTimer = setTimeout(connect, delay)
  }
  socket.onerror = () => socket.close()
}

watch(writeMode, () => {
  if (!authToken()) {
    // 未保存 token 时保持只读
    writeMode.value = false
    return
  }
  attempts = 0
  connect()
})

function send(action: string, extra: Record<string, string> = {}) {
  if (!connected.value || ws?.readyState !== WebSocket.OPEN) return
  ws.send(JSON.stringify({ type: 'tmux', action, session: session.value, ...extra }))
}

function refresh() { send('capture') }
function listSessions() { send('list') }
function newSession() {
  if (!writeMode.value) return
  const cmd = window.prompt('启动命令(交互式工具走 tmux):')
  if (cmd) {
    send('new', { cmd })
    setTimeout(refresh, 400)
  }
}
function sendKeys() {
  if (!writeMode.value) return
  const keys = window.prompt('发送按键(回车自动附加):')
  if (keys) {
    send('send', { keys })
    setTimeout(refresh, 400)
  }
}

onMounted(() => {
  term = new Terminal({
    convertEol: true,
    fontSize: 12,
    fontFamily: 'ui-monospace, SFMono-Regular, Menlo, Consolas, "Liberation Mono", monospace',
    theme: { background: '#0d1117', foreground: '#d7e2ee' },
    scrollback: 3000,
  })
  if (el.value) term.open(el.value)
  term.writeln('ScopeForge tmux 观测终端 · 只读 capture/list;写模式需 API token')
  session.value = defaultSession.value
  connect()
  armCapture()
})

onUnmounted(() => {
  stopped = true
  window.clearTimeout(reconnectTimer)
  window.clearInterval(captureTimer)
  ws?.close()
  term?.dispose()
  term = null
})
</script>

<template>
  <div class="panel">
    <div class="panel-title">
      🖥 终端
      <span class="grow"></span>
      <span class="badge" :class="connected ? 'ok' : 'bad'">{{ connected ? '已连接' : '重连中' }}</span>
    </div>

    <div v-if="fatal" class="badge warn mb8" style="white-space: normal">{{ fatal }}</div>

    <div class="row wrap mb8">
      <select v-model="session" style="min-width: 180px" @change="refresh">
        <option v-if="!sessions.includes(session)" :value="session">{{ session || '会话…' }}</option>
        <option v-for="s in sessions" :key="s" :value="s">{{ s }}</option>
      </select>
      <button class="btn sm" @click="refresh">刷新</button>
      <button class="btn sm" @click="listSessions">会话列表</button>
      <button class="btn sm" :disabled="!writeMode" @click="newSession">新会话</button>
      <button class="btn sm" :disabled="!writeMode" @click="sendKeys">发送按键</button>
      <span class="grow"></span>
      <label style="display: inline-flex; align-items: center; gap: 5px; font-size: 12px">
        <input v-model="writeMode" type="checkbox" :disabled="!authToken()" />
        写模式
      </label>
      <span v-if="!authToken()" class="faint">(右上角先保存 Token)</span>
    </div>

    <div ref="el" style="height: 320px; background: #0d1117; border: 1px solid var(--border); border-radius: 6px; padding: 4px"></div>
  </div>
</template>
