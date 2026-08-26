<script setup lang="ts">
import { inject, onMounted, ref } from 'vue'

import { PaginationBudget } from '@/remote/paginationBudget'
import { remoteRPCKey, type RemoteAIConfiguration } from '@/remote/rpcTypes'

const aggregatePaginationLimits = {
  maximumPages: 16,
  maximumItems: 1_000,
  maximumBytes: 1024 * 1024,
  maximumCursorBytes: 512,
} as const

const rpc = inject(remoteRPCKey)
if (!rpc) throw new Error('Remote RPC provider is required')
const props = defineProps<{ writable: boolean }>()

const loading = ref(true)
const saving = ref(false)
const testing = ref(false)
const deleting = ref(false)
const configurations = ref<RemoteAIConfiguration[]>([])
const configuration = ref<RemoteAIConfiguration | null>(null)
const selectedId = ref('')
const draftId = ref('default')
const name = ref('默认 AI')
const provider = ref<RemoteAIConfiguration['provider']>('openai-compatible')
const baseUrl = ref('https://api.openai.com/v1')
const model = ref('')
const nonSecretHeaders = ref<Record<string, string>>({})
const systemPrompt = ref('You are a helpful assistant.')
const temperature = ref(0.7)
const reasoningEffort = ref('automatic')
const maxTurnOutputTokens = ref(16_000)
const maxActiveContextTokens = ref(120_000)
const requestTimeoutSeconds = ref(300)
const maxRetries = ref(2)
const retryBaseDelayMilliseconds = ref(350)
const showUsage = ref(true)
const apiKey = ref('')
const enabled = ref(true)
const message = ref('')
const errorMessage = ref('')

let fallbackIdentifier = 0
const createConfigurationId = () =>
  `config-${globalThis.crypto?.randomUUID?.() ?? `${Date.now()}-${++fallbackIdentifier}`}`

const apply = (value: RemoteAIConfiguration) => {
  configuration.value = value
  selectedId.value = value.id
  draftId.value = value.id
  name.value = value.name
  provider.value = value.provider
  baseUrl.value = value.baseUrl
  model.value = value.model
  nonSecretHeaders.value = { ...value.nonSecretHeaders }
  systemPrompt.value = value.systemPrompt
  temperature.value = value.temperature
  reasoningEffort.value = value.reasoningEffort
  maxTurnOutputTokens.value = value.maxTurnOutputTokens
  maxActiveContextTokens.value = value.maxActiveContextTokens
  requestTimeoutSeconds.value = value.requestTimeoutSeconds
  maxRetries.value = value.maxRetries
  retryBaseDelayMilliseconds.value = value.retryBaseDelayMilliseconds
  showUsage.value = value.showUsage
  enabled.value = value.enabled
  apiKey.value = ''
}

const beginCreate = (first = false) => {
  if (!first && !props.writable) return
  configuration.value = null
  selectedId.value = ''
  draftId.value = first ? 'default' : createConfigurationId()
  name.value = first ? '默认 AI' : '新 AI 配置'
  provider.value = 'openai-compatible'
  baseUrl.value = 'https://api.openai.com/v1'
  model.value = ''
  nonSecretHeaders.value = {}
  systemPrompt.value = 'You are a helpful assistant.'
  temperature.value = 0.7
  reasoningEffort.value = 'automatic'
  maxTurnOutputTokens.value = 16_000
  maxActiveContextTokens.value = 120_000
  requestTimeoutSeconds.value = 300
  maxRetries.value = 2
  retryBaseDelayMilliseconds.value = 350
  showUsage.value = true
  enabled.value = true
  apiKey.value = ''
  message.value = ''
  errorMessage.value = ''
}

const selectConfiguration = () => {
  const value = configurations.value.find((item) => item.id === selectedId.value)
  if (value) apply(value)
}

const load = async () => {
  loading.value = true
  errorMessage.value = ''
  try {
    await rpc.connect()
    const items: RemoteAIConfiguration[] = []
    const budget = new PaginationBudget('AI 配置列表', aggregatePaginationLimits)
    let cursor: string | undefined
    do {
      budget.assertCanRequestPage()
      const page = await rpc.call<{
        items: RemoteAIConfiguration[]
        highWatermark: number
        nextCursor?: string | null
      }>('ai.config.list', { ...(cursor ? { cursor } : {}), limit: 100 })
      budget.admitPage(page.items)
      items.push(...page.items)
      cursor = budget.admitCursor(page.nextCursor)
    } while (cursor)
    configurations.value = items.sort((left, right) => left.name.localeCompare(right.name, 'zh-CN'))
    if (configurations.value[0]) apply(configurations.value[0])
    else beginCreate(true)
  } catch (error) {
    errorMessage.value = error instanceof Error ? error.message : '无法读取设备上的 AI 配置。'
  } finally {
    loading.value = false
  }
}

const save = async () => {
  if (saving.value || !props.writable) return
  saving.value = true
  message.value = ''
  errorMessage.value = ''
  try {
    const value = await rpc.call<RemoteAIConfiguration>('ai.config.update', {
      id: configuration.value?.id ?? draftId.value,
      expectedRevision: configuration.value?.revision ?? 0,
      name: name.value.trim(),
      provider: provider.value,
      baseUrl: baseUrl.value.trim(),
      nonSecretHeaders: nonSecretHeaders.value,
      model: model.value.trim(),
      systemPrompt: systemPrompt.value,
      temperature: temperature.value,
      reasoningEffort: reasoningEffort.value,
      maxTurnOutputTokens: maxTurnOutputTokens.value,
      maxActiveContextTokens: maxActiveContextTokens.value,
      requestTimeoutSeconds: requestTimeoutSeconds.value,
      maxRetries: maxRetries.value,
      retryBaseDelayMilliseconds: retryBaseDelayMilliseconds.value,
      showUsage: showUsage.value,
      enabled: enabled.value,
      secretAction: apiKey.value ? 'replace' : 'keep',
      ...(apiKey.value ? { secret: apiKey.value } : {}),
    })
    const index = configurations.value.findIndex((item) => item.id === value.id)
    if (index >= 0) configurations.value.splice(index, 1, value)
    else configurations.value.push(value)
    configurations.value.sort((left, right) => left.name.localeCompare(right.name, 'zh-CN'))
    apply(value)
    message.value = '配置已在目标设备本地保存；密钥未返回管理端。'
  } catch (error) {
    errorMessage.value = error instanceof Error ? error.message : '无法保存 AI 配置。'
  } finally {
    saving.value = false
  }
}

const remove = async () => {
  const value = configuration.value
  if (
    !value ||
    !props.writable ||
    deleting.value ||
    !window.confirm(`删除设备上的 AI 配置“${value.name}”？此操作不会返回其中的密钥。`)
  )
    return
  deleting.value = true
  message.value = ''
  errorMessage.value = ''
  try {
    await rpc.call('ai.config.delete', { id: value.id, expectedRevision: value.revision })
    configurations.value = configurations.value.filter((item) => item.id !== value.id)
    if (configurations.value[0]) apply(configurations.value[0])
    else beginCreate(true)
    message.value = '配置已从目标设备删除。'
  } catch (error) {
    errorMessage.value = error instanceof Error ? error.message : '无法删除设备上的 AI 配置。'
  } finally {
    deleting.value = false
  }
}

const test = async () => {
  testing.value = true
  message.value = ''
  errorMessage.value = ''
  try {
    const result = await rpc.call<{ latencyMs: number; model: string }>('ai.config.test', {
      id: configuration.value?.id,
    })
    message.value = `设备连接成功：${result.model}，${result.latencyMs} ms。`
  } catch (error) {
    errorMessage.value = error instanceof Error ? error.message : '设备上的连接测试失败。'
  } finally {
    testing.value = false
  }
}

onMounted(() => void load())
</script>

<template>
  <section class="remote-panel" aria-labelledby="remote-ai-config-heading">
    <div class="remote-panel-heading">
      <div>
        <h2 id="remote-ai-config-heading">AI 配置</h2>
        <p>配置和密钥经端到端加密发送到设备；密钥只写入设备本地受限状态文件，页面不会回读明文。</p>
      </div>
      <div class="remote-form-actions">
        <span class="encrypted-pill">E2EE</span>
        <button
          type="button"
          :disabled="saving || deleting || !writable"
          @click="beginCreate(false)"
        >
          新增配置
        </button>
      </div>
    </div>
    <p v-if="errorMessage" class="remote-notice error" role="alert">{{ errorMessage }}</p>
    <p v-if="message" class="remote-notice success" role="status">{{ message }}</p>
    <p v-if="!writable" class="remote-notice warning" role="status">
      当前授权为只读；可查看并测试已保存配置，但不能新增、修改或删除。
    </p>
    <p v-if="loading" class="remote-panel-empty">正在建立加密连接…</p>
    <form v-else class="remote-settings-form" @submit.prevent="save">
      <label v-if="configurations.length" class="wide">
        <span>已保存配置</span>
        <select v-model="selectedId" @change="selectConfiguration">
          <option v-if="!configuration" value="">正在创建：{{ name }}</option>
          <option v-for="item in configurations" :key="item.id" :value="item.id">
            {{ item.name }} · {{ item.model || '未选择模型' }}
          </option>
        </select>
      </label>
      <label
        ><span>配置名称</span
        ><input
          v-model.trim="name"
          name="aiConfigName"
          required
          maxlength="120"
          :disabled="!writable"
      /></label>
      <label>
        <span>提供方</span>
        <select v-model="provider" :disabled="!writable">
          <option value="openai">OpenAI</option>
          <option value="anthropic">Anthropic</option>
          <option value="google">Google Gemini</option>
          <option value="deepseek">DeepSeek</option>
          <option value="ollama">Ollama</option>
          <option value="openai-compatible">OpenAI 兼容 API</option>
        </select>
      </label>
      <label class="wide">
        <span>API 地址</span>
        <input
          v-model.trim="baseUrl"
          name="aiBaseUrl"
          required
          type="url"
          autocomplete="off"
          :disabled="!writable"
        />
      </label>
      <label
        ><span>模型</span
        ><input
          v-model.trim="model"
          name="aiModel"
          required
          autocomplete="off"
          :disabled="!writable"
      /></label>
      <label>
        <span>推理强度</span>
        <select v-model="reasoningEffort" :disabled="!writable">
          <option value="automatic">自动</option>
          <option value="none">关闭</option>
          <option value="minimal">最低</option>
          <option value="low">低</option>
          <option value="medium">中</option>
          <option value="high">高</option>
          <option value="xhigh">超高</option>
          <option value="max">最大</option>
        </select>
      </label>
      <label>
        <span>温度</span>
        <input
          v-model.number="temperature"
          type="number"
          min="0"
          max="2"
          step="0.1"
          :disabled="!writable"
        />
      </label>
      <label class="wide">
        <span>系统 Prompt</span>
        <textarea
          v-model="systemPrompt"
          rows="4"
          maxlength="32768"
          :disabled="!writable"
        ></textarea>
      </label>
      <label>
        <span>API Key</span>
        <input
          v-model="apiKey"
          name="aiApiKey"
          type="password"
          autocomplete="new-password"
          placeholder="留空则保留设备现有密钥"
          :disabled="!writable"
        />
        <small>{{ configuration?.secretConfigured ? '设备已配置密钥' : '设备尚未配置密钥' }}</small>
      </label>
      <label class="remote-checkbox"
        ><input v-model="enabled" type="checkbox" :disabled="!writable" /> 启用此配置</label
      >
      <p class="wide remote-config-identifier">配置 ID：{{ configuration?.id ?? draftId }}</p>
      <div class="remote-form-actions wide">
        <button type="submit" :disabled="saving || !writable">
          {{ saving ? '保存中…' : '保存到设备' }}
        </button>
        <button type="button" :disabled="testing || !configuration" @click="test">
          {{ testing ? '测试中…' : '在设备上测试' }}
        </button>
        <button
          v-if="configuration"
          type="button"
          :disabled="deleting || saving || !writable"
          @click="remove"
        >
          {{ deleting ? '删除中…' : '删除配置' }}
        </button>
      </div>
    </form>
  </section>
</template>
