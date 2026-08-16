<script setup lang="ts">
// 系统页:health / providers / 隧道 / 监听器 / 能力组件 / 成本账本。
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { api, type LedgerRow, type SystemInfo } from '../api'
import { fmtTime, fmtUsd } from '../utils/format'

const sys = ref<SystemInfo | null>(null)
const health = ref<{ status: string; version: string } | null>(null)
const ledger = ref<LedgerRow[]>([])
const error = ref('')
let timer: ReturnType<typeof setInterval> | undefined

async function load() {
  error.value = ''
  try {
    const [s, l] = await Promise.all([api.system(), api.ledger()])
    sys.value = s
    ledger.value = l.rows || []
  } catch (e: any) {
    error.value = String(e?.message || e)
  }
  try {
    health.value = await api.health()
  } catch {
    health.value = { status: 'degraded', version: '-' }
  }
}

onMounted(() => {
  load()
  timer = setInterval(load, 10000)
})
onUnmounted(() => clearInterval(timer))

const totalCost = computed(() => ledger.value.reduce((sum, r) => sum + (r.cost_usd || 0), 0))
const totalCalls = computed(() => ledger.value.reduce((sum, r) => sum + (r.calls || 0), 0))
const totalTokens = computed(() => ledger.value.reduce((sum, r) => sum + (r.prompt_tokens || 0) + (r.output_tokens || 0), 0))
</script>

<template>
  <div class="page" style="max-width: 1280px">
    <div class="page-head">
      <h1 class="page-title">系统状态</h1>
      <span v-if="health" class="badge" :class="health.status === 'ok' ? 'ok' : 'warn'">
        {{ health.status }} · {{ health.version }}
      </span>
      <span class="grow"></span>
      <span v-if="sys" class="faint mono">{{ fmtTime(sys.ts) }} · 10s 刷新</span>
      <button class="btn sm ghost" @click="load">刷新</button>
    </div>

    <div v-if="error" class="badge bad mb8">{{ error }}</div>
    <div v-if="!sys" class="panel muted">加载中…</div>

    <template v-else>
      <div class="grid cards mb16" style="grid-template-columns: repeat(auto-fit, minmax(150px, 1fr))">
        <div class="card stat-card"><div class="k">Providers</div><div class="v">{{ sys.providers?.length || 0 }}</div></div>
        <div class="card stat-card"><div class="k">隧道</div><div class="v">{{ sys.routes?.length || 0 }}</div></div>
        <div class="card stat-card"><div class="k">监听器</div><div class="v">{{ sys.listeners?.length || 0 }}</div></div>
        <div class="card stat-card"><div class="k">成本账本</div><div class="v">{{ fmtUsd(totalCost) }}</div><div class="s">{{ totalCalls }} calls · {{ totalTokens }} tokens</div></div>
      </div>

      <!-- Providers -->
      <div class="panel">
        <div class="panel-title">LLM Providers</div>
        <div class="row wrap">
          <div v-for="p in sys.providers || []" :key="p.name" class="card" style="display: flex; gap: 8px; align-items: center">
            <span :style="{ width: '9px', height: '9px', borderRadius: '50%', background: p.available ? 'var(--ok)' : 'var(--bad)' }"></span>
            <strong>{{ p.name }}</strong>
            <span class="badge" :class="p.available ? 'ok' : 'bad'">{{ p.available ? '可用' : '不可用' }}</span>
          </div>
          <div v-if="!sys.providers?.length" class="faint">无 providers 配置</div>
        </div>
      </div>

      <!-- 隧道 / 监听器 -->
      <div class="panel">
        <div class="panel-title">隧道 / 监听器</div>
        <div v-if="!sys.routes?.length && !sys.listeners?.length" class="faint">无活动隧道与监听器</div>
        <div v-if="sys.routes?.length" class="table-wrap mb16">
          <table>
            <thead><tr><th>隧道</th><th>协议</th><th>目标</th><th>本地出口</th><th>后端</th><th>状态</th><th>创建</th></tr></thead>
            <tbody>
              <tr v-for="r in sys.routes" :key="r.id">
                <td class="mono">{{ r.id }}</td>
                <td class="mono">{{ r.proto }}</td>
                <td class="mono">{{ r.target }}</td>
                <td class="mono">{{ r.local_addr }}</td>
                <td class="mono">{{ r.backend }}</td>
                <td><span class="badge" :class="r.status === 'running' ? 'ok' : 'warn'">{{ r.status }}</span></td>
                <td class="muted mono">{{ fmtTime(r.created) }}</td>
              </tr>
            </tbody>
          </table>
        </div>
        <div v-if="sys.listeners?.length" class="table-wrap">
          <table>
            <thead><tr><th>监听器</th><th>协议</th><th>地址</th><th>端口</th><th>创建</th></tr></thead>
            <tbody>
              <tr v-for="l in sys.listeners" :key="l.id">
                <td class="mono">{{ l.id }}</td>
                <td class="mono">{{ l.proto }}</td>
                <td class="mono">{{ l.addr }}</td>
                <td class="mono">{{ l.port }}</td>
                <td class="muted mono">{{ fmtTime(l.created) }}</td>
              </tr>
            </tbody>
          </table>
        </div>
      </div>

      <!-- 能力组件 -->
      <div class="panel">
        <div class="panel-title">能力组件</div>
        <div class="row wrap">
          <span class="badge info">Kali 攻击工具:经容器 bash + kali-tools skill 卡调用</span>
          <span v-if="sys.docker_available !== undefined" class="badge" :class="sys.docker_available ? 'ok' : 'warn'">
            沙箱 Docker: {{ sys.docker_available ? '可用' : '不可用' }}
          </span>
          <span v-if="sys.sandbox_image" class="tag mono">{{ sys.sandbox_image }}</span>
          <span v-if="sys.kb_entries" class="badge info">
            知识库: builtin {{ sys.kb_entries.builtin }} / plugins {{ sys.kb_entries.plugins }}
          </span>
        </div>
      </div>

      <!-- 成本账本 -->
      <div class="panel">
        <div class="panel-title">
          成本账本
          <span class="grow"></span>
          <span class="faint mono">总计 {{ fmtUsd(totalCost) }} · {{ totalCalls }} 次调用</span>
        </div>
        <div v-if="ledger.length === 0" class="empty">暂无调用账本</div>
        <div v-else class="table-wrap">
          <table>
            <thead>
              <tr><th>role</th><th>model</th><th>prompt</th><th>cache_hit</th><th>output</th><th>reasoning</th><th>调用</th><th>成本</th></tr>
            </thead>
            <tbody>
              <tr v-for="r in ledger" :key="`${r.role}:${r.model}`">
                <td class="mono">{{ r.role }}</td>
                <td class="mono">{{ r.model }}</td>
                <td class="mono">{{ r.prompt_tokens }}</td>
                <td class="mono">{{ r.cache_hit_tokens }}</td>
                <td class="mono">{{ r.output_tokens }}</td>
                <td class="mono">{{ r.reasoning_tokens }}</td>
                <td class="mono">{{ r.calls }}</td>
                <td class="mono">{{ fmtUsd(r.cost_usd) }}</td>
              </tr>
            </tbody>
          </table>
        </div>
      </div>
    </template>
  </div>
</template>
