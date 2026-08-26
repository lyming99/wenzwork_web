<script setup lang="ts">
import { computed, inject, nextTick, ref, watch } from 'vue'

import { agentSupportsProjectMethod, type RemoteAgentCapabilities } from '@/remote/peerClient'
import { remoteRPCKey } from '@/remote/rpcTypes'

import RemoteTerminalSession from './RemoteTerminalSession.vue'

interface TerminalTab {
  id: number
  label: string
  state: string
  message: string
}

interface LegacyResult {
  command: string
  workingDirectory: string
  output: string
  exitCode: number
  truncated: boolean
}

const props = defineProps<{
  projectId: string
  active?: boolean
  interactiveAuthorized: boolean
  legacyAuthorized: boolean
  capabilities: RemoteAgentCapabilities | null
  capabilityError?: string
}>()
const emit = defineEmits<{
  sessions: [value: { count: number; backgroundCount: number }]
}>()

const rpc = inject(remoteRPCKey)
if (!rpc) throw new Error('Remote RPC provider is required')

let nextTabId = 2
const tabs = ref<TerminalTab[]>([
  { id: 1, label: 'Terminal 1', state: 'opening', message: '正在打开远程 PTY…' },
])
const activeTabId = ref(1)
const sessionRefs = new Map<number, InstanceType<typeof RemoteTerminalSession>>()
const legacyCommand = ref('')
const legacyRunning = ref(false)
const legacyError = ref('')
const legacyHistory = ref<LegacyResult[]>([])

const interactiveSupported = computed(
  () =>
    props.interactiveAuthorized &&
    props.capabilities !== null &&
    agentSupportsProjectMethod(props.capabilities, 'terminal.open'),
)
const legacySupported = computed(
  () =>
    props.legacyAuthorized &&
    props.capabilities !== null &&
    (props.capabilities.featureVersions.terminal ?? 0) >= 1,
)
const mode = computed<'loading' | 'interactive' | 'legacy' | 'unavailable'>(() => {
  if (!props.capabilities) return props.capabilityError ? 'unavailable' : 'loading'
  if (interactiveSupported.value) return 'interactive'
  if (legacySupported.value) return 'legacy'
  return 'unavailable'
})
const backgroundCount = computed(() =>
  mode.value === 'interactive' && props.active === false
    ? tabs.value.filter((tab) => !['closed', 'exited', 'failed'].includes(tab.state)).length
    : 0,
)

const setSessionRef = (id: number, value: InstanceType<typeof RemoteTerminalSession> | null) => {
  if (value) sessionRefs.set(id, value)
  else sessionRefs.delete(id)
}

const newTab = () => {
  const id = nextTabId++
  tabs.value.push({
    id,
    label: `Terminal ${tabs.value.length + 1}`,
    state: 'opening',
    message: '正在打开远程 PTY…',
  })
  activeTabId.value = id
  void nextTick(() => sessionRefs.get(id)?.focus())
}

const selectTab = (id: number) => {
  activeTabId.value = id
  void nextTick(() => sessionRefs.get(id)?.focus())
}

const closeTab = (id: number) => {
  const index = tabs.value.findIndex((tab) => tab.id === id)
  if (index < 0) return
  tabs.value.splice(index, 1)
  if (tabs.value.length === 0) {
    newTab()
    return
  }
  if (activeTabId.value === id) {
    activeTabId.value = tabs.value[Math.min(index, tabs.value.length - 1)]!.id
  }
}

const updateTabState = (id: number, value: { state: string; message: string }) => {
  const tab = tabs.value.find((item) => item.id === id)
  if (!tab) return
  tab.state = value.state
  tab.message = value.message
}

const closeBackgroundTerminals = () => {
  if (props.active !== false) return
  tabs.value = []
  activeTabId.value = 0
}

const runLegacy = async () => {
  const command = legacyCommand.value.trim()
  if (!command || legacyRunning.value) return
  if (new TextEncoder().encode(command).length > 512) {
    legacyError.value = '命令不能超过 512 字节。'
    return
  }
  legacyRunning.value = true
  legacyError.value = ''
  legacyCommand.value = ''
  try {
    const result = await rpc.call<LegacyResult>(
      'terminal.execute',
      { command },
      { projectId: props.projectId },
    )
    legacyHistory.value.push(result)
  } catch (error) {
    legacyError.value = error instanceof Error ? error.message : '无法执行设备命令。'
  } finally {
    legacyRunning.value = false
  }
}

watch(
  [tabs, () => props.active, mode],
  () => {
    if (props.active !== false && mode.value === 'interactive' && tabs.value.length === 0) newTab()
    emit('sessions', { count: tabs.value.length, backgroundCount: backgroundCount.value })
  },
  { deep: true, immediate: true },
)

defineExpose({ backgroundCount, closeBackgroundTerminals })
</script>

<template>
  <section class="remote-panel terminal-panel" aria-label="终端管理">
    <div v-if="mode === 'loading'" class="remote-panel-empty">正在检测 Agent 终端能力…</div>
    <div v-else-if="mode === 'unavailable'" class="terminal-unavailable remote-panel-empty">
      <span class="terminal-unavailable-icon" aria-hidden="true">⌨</span>
      <strong>终端管理不可用</strong>
      <p>
        {{ capabilityError || '请为设备授予交互式终端权限，并在目标项目的设备策略中启用终端。' }}
      </p>
    </div>

    <template v-else-if="mode === 'interactive'">
      <div class="terminal-tabs" role="tablist" aria-label="终端标签页">
        <div class="terminal-tab-scroll">
          <div
            v-for="tab in tabs"
            :key="tab.id"
            class="terminal-tab"
            :class="{ active: activeTabId === tab.id }"
          >
            <button
              class="terminal-tab-select"
              type="button"
              role="tab"
              :aria-selected="activeTabId === tab.id"
              @click="selectTab(tab.id)"
            >
              <span class="terminal-tab-state" :class="tab.state" aria-hidden="true"></span>
              <span>{{ tab.label }}</span>
            </button>
            <button
              class="terminal-tab-close"
              type="button"
              aria-label="关闭终端"
              @click="closeTab(tab.id)"
            >
              ×
            </button>
          </div>
        </div>
        <button class="terminal-new" type="button" title="新建终端" @click="newTab">＋</button>
      </div>
      <div class="terminal-stage">
        <RemoteTerminalSession
          v-for="tab in tabs"
          v-show="activeTabId === tab.id"
          :key="tab.id"
          :ref="
            (value) =>
              setSessionRef(tab.id, value as InstanceType<typeof RemoteTerminalSession> | null)
          "
          :project-id="projectId"
          :active="active !== false && activeTabId === tab.id"
          @state="updateTabState(tab.id, $event)"
        />
      </div>
    </template>

    <template v-else>
      <p class="remote-notice warning" role="status">
        当前 Agent 或项目未开放交互式 PTY，已切换到桌面端兼容的受限只读命令模式。
      </p>
      <p v-if="legacyError" class="remote-notice error" role="alert">{{ legacyError }}</p>
      <div class="legacy-terminal" aria-live="polite">
        <p v-if="legacyHistory.length === 0" class="legacy-empty">
          支持：pwd、ls/dir、git status、git diff --stat、git log
        </p>
        <pre
          v-for="(item, index) in legacyHistory"
          :key="index"
        ><strong>$ {{ item.command }}</strong>
{{ item.output }}
[exit {{ item.exitCode }}{{ item.truncated ? ', truncated' : '' }}]</pre>
      </div>
      <form class="legacy-command" @submit.prevent="runLegacy">
        <input
          v-model="legacyCommand"
          maxlength="512"
          :disabled="legacyRunning"
          placeholder="pwd、ls、git status…"
          autocomplete="off"
          spellcheck="false"
        />
        <button type="submit" :disabled="legacyRunning || !legacyCommand.trim()">
          {{ legacyRunning ? '执行中…' : '执行' }}
        </button>
      </form>
    </template>
  </section>
</template>

<style scoped>
.terminal-panel {
  box-sizing: border-box;
  display: grid;
  grid-template-rows: auto minmax(0, 1fr);
  min-height: 540px;
  height: 100%;
  padding: 12px 28px 24px;
}
.terminal-tabs {
  display: flex;
  align-items: center;
  gap: 6px;
  min-width: 0;
  margin-bottom: 10px;
}
.terminal-tab-scroll {
  display: flex;
  flex: 1;
  gap: 6px;
  min-width: 0;
  overflow-x: auto;
}
.terminal-tab {
  display: inline-flex;
  flex: 0 0 auto;
  align-items: center;
  border-color: transparent;
  border-radius: 8px;
  color: var(--ink-soft);
  background: var(--paper-soft);
  font-size: 0.75rem;
  font-weight: 600;
}
.terminal-tab.active {
  color: var(--teal-dark);
  background: var(--brand-tint);
}
.terminal-tab-select {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  border: 0 !important;
  padding: 7px 5px 7px 10px !important;
  color: inherit !important;
  background: transparent !important;
  font: inherit;
}
.terminal-tab-state {
  width: 6px;
  height: 6px;
  border-radius: 50%;
  background: var(--ink-faint);
}
.terminal-tab-state.running {
  background: var(--teal);
}
.terminal-tab-state.failed {
  background: #d92d20;
}
.terminal-tab-state.reconnecting,
.terminal-tab-state.opening {
  background: #f79009;
}
.terminal-tab-close {
  display: grid;
  width: 22px;
  height: 22px;
  place-items: center;
  border: 0 !important;
  border-radius: 6px;
  padding: 0 !important;
  color: inherit !important;
  background: transparent !important;
  font-size: 1rem;
}
.terminal-tab-close:hover {
  background: rgb(0 0 0 / 7%);
}
.terminal-new {
  width: 32px;
  height: 32px;
  padding: 0;
  border-color: transparent;
  font-size: 1.1rem;
}
.terminal-stage {
  min-height: 0;
}
.terminal-unavailable {
  align-self: center;
  display: grid;
  justify-items: center;
  gap: 8px;
}
.terminal-unavailable p {
  max-width: 38rem;
  margin: 0;
}
.terminal-unavailable-icon {
  font-size: 2.3rem;
  color: var(--ink-faint);
}
.legacy-terminal {
  min-height: 360px;
  max-height: min(58vh, 620px);
  overflow: auto;
  border-radius: 12px;
  padding: 16px;
  color: #f2f4f7;
  background: #101828;
}
.legacy-terminal pre {
  margin: 0 0 14px;
  color: inherit;
  font:
    0.82rem/1.45 'Cascadia Mono',
    'SFMono-Regular',
    Consolas,
    monospace;
  white-space: pre-wrap;
}
.legacy-empty {
  display: grid;
  min-height: 320px;
  margin: 0;
  place-items: center;
  color: #d0d5dd;
}
.legacy-command {
  display: grid;
  grid-template-columns: minmax(0, 1fr) auto;
  gap: 8px;
  margin-top: 12px;
}
.legacy-command input {
  min-width: 0;
  padding: 10px 12px;
  font-family: 'Cascadia Mono', 'SFMono-Regular', Consolas, monospace;
}
@media (max-width: 720px) {
  .terminal-panel {
    padding: 10px;
  }
}
</style>
