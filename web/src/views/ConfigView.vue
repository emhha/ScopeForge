<script setup lang="ts">
// 配置页:YAML GET(密钥脱敏)/ PUT(校验 + 落盘)。热重载需重启 serve。
import { onMounted, ref } from 'vue'
import { api, authToken } from '../api'

const yaml = ref('')
const loading = ref(false)
const saving = ref(false)
const error = ref('')
const ok = ref('')

onMounted(async () => {
  loading.value = true
  try {
    const r = await api.config()
    yaml.value = r.config_yaml || ''
  } catch (e: any) {
    error.value = String(e?.message || e)
  } finally {
    loading.value = false
  }
})

async function save() {
  if (!authToken()) {
    error.value = '保存配置是写操作,请先在右上角保存 API Token'
    return
  }
  saving.value = true
  error.value = ''
  ok.value = ''
  try {
    const r = await api.putConfig(yaml.value)
    ok.value = `已保存到 ${r.path || '(配置路径)'} — 重启 serve 后生效`
  } catch (e: any) {
    error.value = String(e?.message || e)
  } finally {
    saving.value = false
  }
}
</script>

<template>
  <div class="page" style="max-width: 1180px">
    <div class="page-head">
      <h1 class="page-title">配置</h1>
      <span class="page-sub">scopeforge.yaml · 密钥仅显示为 *** · PUT 校验后落盘</span>
      <span class="grow"></span>
      <span v-if="!authToken()" class="badge bad">保存需 Token</span>
      <button class="btn primary" :disabled="saving || loading" @click="save">
        {{ saving ? '保存中…' : '保存配置' }}
      </button>
    </div>

    <div v-if="error" class="badge bad mb8">{{ error }}</div>
    <div v-if="ok" class="badge ok mb8">{{ ok }}</div>
    <div v-if="loading" class="panel muted">加载中…</div>
    <textarea
      v-else
      v-model="yaml"
      rows="42"
      class="w100 mono"
      spellcheck="false"
      style="font-size: 12.5px; line-height: 1.55; background: var(--bg2)"
    ></textarea>

    <div class="panel mt16">
      <div class="panel-title">说明</div>
      <ul class="muted" style="margin: 0; padding-left: 18px">
        <li>API Key 只能经 <code>api_key_env</code> 引用环境变量,GET 返回时统一显示为 ***。</li>
        <li>任务形态配置在 <code>platform.task_profile</code>:goal_shape / interest / goal_scope / constraints / skills。</li>
        <li>前端发起任务时,任务文本会覆盖 task_profile.description 注入调度器。</li>
        <li>保存仅校验 YAML 结构与 provider 基本字段,部分变更需重启 serve 后生效。</li>
      </ul>
    </div>
  </div>
</template>
