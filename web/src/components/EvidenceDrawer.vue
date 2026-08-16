<script setup lang="ts">
// 证据溯源抽屉:fact.evidence_ref → 服务端权威全文 + 支撑事实。
import { ref, watch } from 'vue'
import { api } from '../api'

const props = defineProps<{ challenge: string; refId: string | null }>()
const emit = defineEmits<{ (e: 'close'): void }>()

const data = ref<any>(null)
const loading = ref(false)
const error = ref('')

watch(
  () => props.refId,
  async (id) => {
    if (!id) {
      data.value = null
      error.value = ''
      return
    }
    loading.value = true
    error.value = ''
    try {
      data.value = await api.evidence(props.challenge, id)
    } catch (e: any) {
      error.value = String(e?.message || e)
      data.value = null
    } finally {
      loading.value = false
    }
  },
  { immediate: true },
)
</script>

<template>
  <div v-if="refId">
    <div class="drawer-mask" @click="emit('close')"></div>
    <div class="drawer">
      <div class="row mb16">
        <strong class="mono">证据溯源 · {{ refId }}</strong>
        <span class="grow"></span>
        <button class="btn ghost sm" @click="emit('close')">✕</button>
      </div>

      <div v-if="loading" class="muted">加载中…</div>
      <div v-else-if="error" class="panel">
        <span class="badge bad mb8">加载失败</span>
        <div class="muted">{{ error }}</div>
      </div>

      <template v-else-if="data">
        <div v-if="data.event" class="panel">
          <div class="panel-title">
            <span class="badge info">{{ data.event.Kind }}</span>
            <span class="mono muted">seq {{ data.event.Seq }} · {{ new Date(data.event.TS * 1000).toLocaleString() }}</span>
          </div>
          <div class="muted mono mb8">Session {{ data.event.SessionID || '-' }} · Challenge {{ data.event.ChallengeID || '-' }}</div>
          <pre class="payload">{{ JSON.stringify(data.event.Payload, null, 2) }}</pre>
        </div>

        <div v-if="data.supporting_facts?.length" class="panel">
          <div class="panel-title">支撑事实</div>
          <div v-for="f in data.supporting_facts" :key="f.id" class="card mb8">
            <div class="row wrap">
              <span class="badge obs">{{ f.prefix }}</span>
              <span class="muted">{{ (f.weight * 100).toFixed(0) }}% · {{ f.state }}</span>
              <span class="grow"></span>
              <span class="faint mono">seq {{ f.seq }}</span>
            </div>
            <div class="mt4" style="font-size: 13px">{{ f.text }}</div>
          </div>
        </div>

        <div v-if="data.flow" class="panel">
          <div class="panel-title">流量记录</div>
          <pre class="payload">{{ JSON.stringify(data.flow, null, 2) }}</pre>
        </div>

        <div v-if="!data.event && !data.flow && !data.supporting_facts?.length" class="empty">
          无额外证据内容
        </div>
      </template>
    </div>
  </div>
</template>
