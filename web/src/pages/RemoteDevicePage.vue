<script setup lang="ts">
import { useHead } from '@unhead/vue'
import axios from 'axios'
import {
  computed,
  defineAsyncComponent,
  nextTick,
  onBeforeUnmount,
  onMounted,
  provide,
  ref,
  type CSSProperties,
  watch,
} from 'vue'
import { useRoute, useRouter } from 'vue-router'

import { problemMessage } from '@/api/auth'
import {
  enableRemoteDeviceAccess,
  getRemoteDevice,
  listRemoteDevices,
  revokeBrowserController,
  revokeRemoteDeviceAccess,
  type RemoteDevice,
  type RemoteProject,
  type RemoteScope,
} from '@/api/remote'
import BrandLogo from '@/components/BrandLogo.vue'
import RemoteAIConfigPanel from '@/components/remote/RemoteAIConfigPanel.vue'
import RemoteChatPanel from '@/components/remote/RemoteChatPanel.vue'
import RemoteFilesPanel from '@/components/remote/RemoteFilesPanel.vue'
import RemoteProjectsPanel from '@/components/remote/RemoteProjectsPanel.vue'
import RemoteSidePanel from '@/components/remote/RemoteSidePanel.vue'
import RemoteTasksPanel from '@/components/remote/RemoteTasksPanel.vue'
import type RemoteTerminalPanelDefinition from '@/components/remote/RemoteTerminalPanel.vue'
import { clearStoredAgentEventCursors } from '@/remote/agentEventCursor'
import { agentEventConnectionKey, createAgentEventConnection } from '@/remote/agentEvents'
import {
  agentCapabilityCacheVersion,
  agentSupportsProjectMethod,
  createRemotePeerClient,
  REMOTE_AGENT_CAPABILITY_PROTOCOL_VERSION,
  type RemoteAgentCapabilities,
} from '@/remote/peerClient'
import {
  loadBrowserControllerIdentity,
  resetBrowserControllerIdentity,
} from '@/remote/peerIdentity'
import { remoteRPCKey, type RemoteConversation } from '@/remote/rpcTypes'
import { useAuthStore } from '@/stores/auth'

type WorkspacePanel = 'projects' | 'files' | 'terminal' | 'tasks' | 'chat' | 'ai'

const RemoteTerminalPanel = defineAsyncComponent(
  () => import('@/components/remote/RemoteTerminalPanel.vue'),
)

interface ChatSidebarState {
  conversations: RemoteConversation[]
  activeId: string
  loading: boolean
  hasMore: boolean
}

const route = useRoute()
const router = useRouter()
const auth = useAuthStore()
const deviceId = computed(() => String(route.params.deviceId ?? ''))
const device = ref<RemoteDevice | null>(null)
const loading = ref(true)
const actionPending = ref(false)
const errorMessage = ref('')
const activePanel = ref<WorkspacePanel>('chat')
const sidebarVisible = ref(true)
const rightPanelVisible = ref(false)
const workspacePanes = ref<HTMLElement | null>(null)
const sidebarWidth = ref(272)
const rightPanelWidth = ref(384)
const terminalActivated = ref(false)
const availableDevices = ref<RemoteDevice[]>([])
const availableProjects = ref<RemoteProject[]>([])
const selectedProjectId = ref('')
const agentCapabilities = ref<RemoteAgentCapabilities | null>(null)
const capabilityVersion = ref('unverified')
const capabilityError = ref('')
const conversationQuery = ref('')
const conversationSearchOpen = ref(false)
const chatPanel = ref<InstanceType<typeof RemoteChatPanel> | null>(null)
const terminalPanel = ref<InstanceType<typeof RemoteTerminalPanelDefinition> | null>(null)
const terminalBackgroundCount = ref(0)
const chatSidebar = ref<ChatSidebarState>({
  conversations: [],
  activeId: '',
  loading: true,
  hasMore: false,
})

const rpc = createRemotePeerClient(deviceId)
provide(remoteRPCKey, rpc)
const agentEvents = createAgentEventConnection(rpc, deviceId, selectedProjectId)
provide(agentEventConnectionKey, agentEvents)

useHead(
  computed(() => ({
    title: device.value?.deviceName ?? '远程设备',
    meta: [{ name: 'robots', content: 'noindex, nofollow' }],
  })),
)

const allScopes: RemoteScope[] = [
  'remote.project.read',
  'remote.project.sync',
  'remote.task.read',
  'remote.task.write',
  'remote.peer.query',
  'remote.peer.ai.config',
  'remote.peer.ai.chat',
  'remote.peer.ai.tools',
  'remote.peer.terminal',
  'remote.peer.terminal.interactive',
  'remote.peer.file.send',
  'remote.peer.file.receive',
  'remote.peer.task.control',
  'remote.peer.events',
]

const isOnline = computed(
  () => device.value?.status === 'active' && device.value.presence === 'online',
)
const remoteEnabled = computed(() => device.value?.status === 'active')
const accountName = computed(() => auth.user?.displayName || auth.user?.email || 'WenzWork 用户')
const accountInitial = computed(() => Array.from(accountName.value.trim())[0] || '我')
const workspacePaneStyle = computed<CSSProperties>(() => ({
  '--workspace-sidebar-width': `${sidebarWidth.value}px`,
  '--workspace-right-panel-width': `${rightPanelWidth.value}px`,
}))
const selectedProject = computed(
  () => availableProjects.value.find((project) => project.id === selectedProjectId.value) ?? null,
)

// The control plane currently exposes remote access as a device-level switch.
// Agent capability and project policy checks remain authoritative per request.
const hasScope = (scope: RemoteScope) => Boolean(scope && remoteEnabled.value)

const projectedCapabilityVersion = (value: RemoteDevice) =>
  [
    'unverified',
    `agent=${value.agentVersion || 'unknown'}`,
    `grant=${value.grantVersion}`,
    `features=${[...value.capabilities].sort().join(',') || 'none'}`,
  ].join('|')

const loadAgentCapabilities = async (refresh = false) => {
  const current = device.value
  if (!current) return
  const requestedDeviceId = current.id
  agentCapabilities.value = null
  capabilityVersion.value = projectedCapabilityVersion(current)
  if (!isOnline.value) {
    capabilityError.value = '设备离线，暂时无法确认 Agent 能力。'
    return
  }
  if (!hasScope('remote.peer.query')) {
    capabilityError.value = '尚未授予 Agent 能力查询权限。'
    return
  }
  try {
    const capabilities = await rpc.getCapabilities(refresh)
    if (device.value?.id !== requestedDeviceId) return
    agentCapabilities.value = capabilities
    capabilityVersion.value = agentCapabilityCacheVersion(capabilities)
    capabilityError.value = ''
  } catch (error) {
    if (device.value?.id !== requestedDeviceId) return
    capabilityError.value =
      error instanceof Error ? error.message : '无法读取 Agent 协议与功能版本。'
  }
}

const featureUnavailableReason = (feature: 'chat' | 'files' | 'tasks') => {
  const capabilities = agentCapabilities.value
  if (!capabilities) return capabilityError.value || '正在检测 Agent 协议与功能版本…'
  if (
    capabilities.protocolMinimum > REMOTE_AGENT_CAPABILITY_PROTOCOL_VERSION ||
    capabilities.protocolMaximum < REMOTE_AGENT_CAPABILITY_PROTOCOL_VERSION ||
    (capabilities.featureVersions.projects ?? 0) < 2
  ) {
    return 'Agent 需升级后才能使用项目绑定的 v2 能力。'
  }
  if (capabilities.features['project.v2'] !== true) {
    return '设备策略或灰度开关尚未开启项目 v2。'
  }
  if (feature === 'tasks') {
    return agentSupportsProjectMethod(capabilities, 'task.list')
      ? ''
      : '设备策略或 Agent 版本尚未开放任务 v2。'
  }
  const versionKey = feature === 'chat' ? 'ai' : 'files'
  const featureFlag = feature === 'chat' ? 'ai.v2' : 'file.v2'
  if ((capabilities.featureVersions[versionKey] ?? 0) < 2) {
    return `Agent 需升级后才能使用${feature === 'chat' ? ' AI' : '文件'} v2。`
  }
  if (capabilities.features[featureFlag] !== true) {
    return `设备策略或灰度开关尚未开启${feature === 'chat' ? ' AI' : '文件'} v2。`
  }
  return ''
}

const chatUnavailableReason = computed(() => featureUnavailableReason('chat'))
const filesUnavailableReason = computed(() => featureUnavailableReason('files'))
const tasksUnavailableReason = computed(() => featureUnavailableReason('tasks'))
const canDeleteProjects = computed(
  () =>
    isOnline.value &&
    hasScope('remote.peer.query') &&
    agentCapabilities.value !== null &&
    agentSupportsProjectMethod(agentCapabilities.value, 'project.remove'),
)

const panelTitle = computed(() => {
  if (activePanel.value === 'chat') {
    return (
      chatSidebar.value.conversations.find(
        (conversation) => conversation.id === chatSidebar.value.activeId,
      )?.title || '远程 AI 对话'
    )
  }
  return {
    projects: '项目管理',
    files: '文件管理',
    terminal: '终端管理',
    tasks: '任务管理',
    ai: '设备与 AI 设置',
  }[activePanel.value]
})
const showContentHeader = computed(
  () => activePanel.value !== 'files' && activePanel.value !== 'tasks',
)

const filteredConversations = computed(() => {
  const query = conversationQuery.value.trim().toLocaleLowerCase()
  if (!query) return chatSidebar.value.conversations
  return chatSidebar.value.conversations.filter((conversation) =>
    [conversation.title, conversation.modelBinding.model, conversation.modelBinding.provider]
      .join(' ')
      .toLocaleLowerCase()
      .includes(query),
  )
})

const formatConversationTime = (value: string) => {
  const timestamp = new Date(value).getTime()
  if (!Number.isFinite(timestamp)) return '刚刚'
  const minutes = Math.max(0, Math.floor((Date.now() - timestamp) / 60_000))
  if (minutes < 1) return '刚刚'
  if (minutes < 60) return `${minutes} 分钟前`
  const hours = Math.floor(minutes / 60)
  if (hours < 24) return `${hours} 小时前`
  const days = Math.floor(hours / 24)
  return days < 7 ? `${days} 天前` : new Date(value).toLocaleDateString('zh-CN')
}

const acceptProjects = (projects: RemoteProject[]) => {
  availableProjects.value = projects
  if (!projects.some((project) => project.id === selectedProjectId.value)) {
    selectedProjectId.value =
      projects.find((project) => project.state === 'available')?.id ?? projects[0]?.id ?? ''
  }
}

const selectProject = (projectId: string) => {
  if (projectId === selectedProjectId.value) return
  selectedProjectId.value = projectId
  chatSidebar.value = { conversations: [], activeId: '', loading: true, hasMore: false }
  terminalActivated.value = activePanel.value === 'terminal'
  terminalBackgroundCount.value = 0
  void rpc.close()
}

const selectPanel = (panel: WorkspacePanel) => {
  activePanel.value = panel
  if (panel === 'terminal') terminalActivated.value = true
  if (window.innerWidth <= 760) sidebarVisible.value = false
}

const openConversation = async (conversation: RemoteConversation) => {
  selectPanel('chat')
  await nextTick()
  await chatPanel.value?.openConversation(conversation)
}

const createConversation = async () => {
  selectPanel('chat')
  await nextTick()
  await chatPanel.value?.createConversation()
}

const deleteConversation = async (conversation: RemoteConversation) => {
  await chatPanel.value?.deleteConversation(conversation)
}

const receiveChatSidebar = (state: ChatSidebarState) => {
  chatSidebar.value = state
}

const closeConversationSearch = () => {
  conversationSearchOpen.value = false
  conversationQuery.value = ''
}

type ResizablePane = 'sidebar' | 'right'
let stopPaneResize: (() => void) | null = null

const clampPaneWidth = (pane: ResizablePane, requestedWidth: number) => {
  const measuredWidth = workspacePanes.value?.getBoundingClientRect().width ?? 0
  const panesWidth = measuredWidth > 0 ? measuredWidth : window.innerWidth
  const otherWidth =
    pane === 'sidebar' && rightPanelVisible.value && panesWidth > 1120
      ? rightPanelWidth.value + 6
      : pane === 'right' && sidebarVisible.value
        ? sidebarWidth.value + 6
        : 0
  const minimum = pane === 'sidebar' ? 240 : 320
  const fallback = pane === 'sidebar' ? 272 : 384
  const maximum = Math.max(minimum, panesWidth - otherWidth - 360 - 6)
  return Math.round(Math.min(Math.max(requestedWidth || fallback, minimum), maximum))
}

const setPaneWidth = (pane: ResizablePane, value: number) => {
  if (pane === 'sidebar') sidebarWidth.value = clampPaneWidth(pane, value)
  else rightPanelWidth.value = clampPaneWidth(pane, value)
}

const beginPaneResize = (pane: ResizablePane, event: PointerEvent) => {
  if (event.button !== 0 || window.innerWidth <= 760) return
  stopPaneResize?.()
  event.preventDefault()
  const startX = event.clientX
  const startWidth = pane === 'sidebar' ? sidebarWidth.value : rightPanelWidth.value
  const cursor = 'col-resize'
  document.body.style.cursor = cursor
  document.body.style.userSelect = 'none'

  const move = (moveEvent: PointerEvent) => {
    const delta = moveEvent.clientX - startX
    setPaneWidth(pane, startWidth + (pane === 'sidebar' ? delta : -delta))
  }
  const finish = () => {
    window.removeEventListener('pointermove', move)
    window.removeEventListener('pointerup', finish)
    window.removeEventListener('pointercancel', finish)
    document.body.style.removeProperty('cursor')
    document.body.style.removeProperty('user-select')
    stopPaneResize = null
  }
  stopPaneResize = finish
  window.addEventListener('pointermove', move)
  window.addEventListener('pointerup', finish, { once: true })
  window.addEventListener('pointercancel', finish, { once: true })
}

const resizePaneWithKeyboard = (pane: ResizablePane, event: KeyboardEvent) => {
  const direction = pane === 'sidebar' ? 1 : -1
  if (event.key === 'Home') {
    event.preventDefault()
    setPaneWidth(pane, pane === 'sidebar' ? 272 : 384)
    return
  }
  if (event.key !== 'ArrowLeft' && event.key !== 'ArrowRight') return
  event.preventDefault()
  const delta = event.key === 'ArrowRight' ? 16 : -16
  const current = pane === 'sidebar' ? sidebarWidth.value : rightPanelWidth.value
  setPaneWidth(pane, current + delta * direction)
}

const load = async () => {
  loading.value = true
  try {
    device.value = await getRemoteDevice(deviceId.value)
    capabilityVersion.value = projectedCapabilityVersion(device.value)
    errorMessage.value = ''
    void loadAgentCapabilities(true)
  } catch (error) {
    errorMessage.value = problemMessage(error, '暂时无法读取设备详情。')
  } finally {
    loading.value = false
  }
}

const loadDeviceSwitcher = async () => {
  try {
    const page = await listRemoteDevices(undefined, 100)
    availableDevices.value = page.items
  } catch {
    availableDevices.value = device.value ? [device.value] : []
  }
}

const switchDevice = async (event: Event) => {
  const target = event.target as HTMLSelectElement
  if (!target.value || target.value === deviceId.value) return
  await router.push({ name: 'remote-app-device', params: { deviceId: target.value } })
}

const enable = async () => {
  if (!device.value || actionPending.value) return
  if (
    !window.confirm(
      '开启该设备的项目、文件、交互式终端、任务与 AI（含工具审批）远程控制？设备端项目策略仍会逐项校验。',
    )
  )
    return
  actionPending.value = true
  try {
    await enableRemoteDeviceAccess(device.value.id, allScopes)
    device.value = { ...device.value, status: 'active', scopes: [...allScopes] }
    void loadAgentCapabilities(true)
  } catch (error) {
    errorMessage.value = problemMessage(error, '无法开启远程控制。')
  } finally {
    actionPending.value = false
  }
}

const revoke = async () => {
  if (!device.value || actionPending.value) return
  if (!window.confirm('关闭远程控制？现有加密会话、终端和未完成命令会被撤销。')) return
  actionPending.value = true
  try {
    await revokeRemoteDeviceAccess(device.value.id)
    await agentEvents.close({ clearCursor: true })
    await rpc.close()
    device.value = { ...device.value, status: 'disabled', scopes: [] }
    agentCapabilities.value = null
    capabilityVersion.value = projectedCapabilityVersion(device.value)
  } catch (error) {
    errorMessage.value = problemMessage(error, '无法关闭远程控制。')
  } finally {
    actionPending.value = false
  }
}

const rebuildController = async () => {
  const userId = auth.user?.id
  if (!userId || actionPending.value) return
  if (!window.confirm('吊销当前浏览器控制器并生成一套新密钥？当前加密连接会立即关闭。')) return
  actionPending.value = true
  try {
    const identity = await loadBrowserControllerIdentity(userId)
    try {
      await revokeBrowserController(identity.controllerId)
    } catch (error) {
      if (!axios.isAxiosError(error) || error.response?.status !== 404) throw error
    }
    clearStoredAgentEventCursors()
    await rpc.close()
    await resetBrowserControllerIdentity(userId, identity.controllerId)
    errorMessage.value = ''
    await rpc.connect()
  } catch (error) {
    errorMessage.value = problemMessage(error, '无法重建浏览器控制器，请刷新页面后重试。')
  } finally {
    actionPending.value = false
  }
}

watch(deviceId, () => {
  void rpc.close()
  availableProjects.value = []
  selectedProjectId.value = ''
  agentCapabilities.value = null
  capabilityVersion.value = 'unverified'
  capabilityError.value = ''
  chatSidebar.value = { conversations: [], activeId: '', loading: true, hasMore: false }
  activePanel.value = 'chat'
  terminalActivated.value = false
  terminalBackgroundCount.value = 0
  void Promise.all([load(), loadDeviceSwitcher()])
})

onMounted(() => {
  void Promise.all([load(), loadDeviceSwitcher()])
  void agentEvents.start()
})
onBeforeUnmount(() => {
  stopPaneResize?.()
  void agentEvents.close()
  void rpc.close()
})
</script>

<template>
  <section class="remote-workspace">
    <header class="workspace-titlebar">
      <div class="workspace-titlebar-side">
        <button
          class="pane-toggle"
          type="button"
          :title="sidebarVisible ? '折叠侧边栏' : '展开侧边栏'"
          :aria-pressed="sidebarVisible"
          @click="sidebarVisible = !sidebarVisible"
        >
          <span aria-hidden="true">☰</span>
        </button>
        <a class="workspace-brand" href="/account/remote" target="_blank" rel="noopener">
          <BrandLogo /><span>设备工作台</span>
        </a>
      </div>

      <label v-if="device" class="device-switcher">
        <span class="device-switcher-dot" :class="device.presence" aria-hidden="true"></span>
        <span class="sr-only">切换设备</span>
        <select :value="device.id" aria-label="切换设备" @change="switchDevice">
          <option v-for="item in availableDevices" :key="item.id" :value="item.id">
            {{ item.deviceName }} · {{ item.presence === 'online' ? '在线' : '离线' }}
          </option>
        </select>
      </label>

      <div class="workspace-titlebar-side end">
        <span
          class="encrypted-connection-state"
          :class="{ connected: rpc.connected.value, reconnecting: rpc.reconnecting.value }"
          :title="rpc.error.value || '端到端加密设备连接'"
        >
          <i aria-hidden="true"></i>
          {{
            rpc.connected.value ? '已加密连接' : rpc.reconnecting.value ? '正在恢复' : '按需连接'
          }}
        </span>
        <button
          class="pane-toggle"
          type="button"
          :title="rightPanelVisible ? '折叠右侧栏' : '展开右侧栏'"
          :aria-pressed="rightPanelVisible"
          @click="rightPanelVisible = !rightPanelVisible"
        >
          <span aria-hidden="true">▥</span>
        </button>
      </div>
    </header>

    <div
      ref="workspacePanes"
      class="workspace-panes"
      :style="workspacePaneStyle"
      :class="{
        'without-sidebar': !sidebarVisible,
        'without-right-panel': !rightPanelVisible || !device,
      }"
    >
      <aside v-if="sidebarVisible" class="workspace-sidebar" aria-label="设备工作台侧边栏">
        <div class="project-selector">
          <span aria-hidden="true">▰</span>
          <select
            :value="selectedProjectId"
            :disabled="availableProjects.length === 0"
            aria-label="当前远程项目"
            @change="selectProject(($event.target as HTMLSelectElement).value)"
          >
            <option value="" disabled>
              {{ remoteEnabled ? '选择项目' : '远程访问未开启' }}
            </option>
            <option v-for="project in availableProjects" :key="project.id" :value="project.id">
              {{ project.displayName }}
            </option>
          </select>
          <button type="button" title="项目管理" @click="selectPanel('projects')">⚙</button>
        </div>

        <nav class="workspace-destinations" aria-label="设备功能">
          <button
            type="button"
            :class="{ active: activePanel === 'files' }"
            :disabled="!hasScope('remote.peer.file.receive')"
            @click="selectPanel('files')"
          >
            <span aria-hidden="true">▤</span><span>文件管理</span
            ><small v-if="!remoteEnabled">⌑</small>
          </button>
          <button
            type="button"
            :class="{ active: activePanel === 'terminal' }"
            :disabled="!remoteEnabled"
            @click="selectPanel('terminal')"
          >
            <span aria-hidden="true">⌨</span><span>终端管理</span>
            <small
              v-if="terminalBackgroundCount"
              class="terminal-background-action"
              title="关闭后台终端"
              @click.stop="terminalPanel?.closeBackgroundTerminals()"
              >{{ terminalBackgroundCount }} 后台 ×</small
            >
            <small v-else-if="!remoteEnabled">⌑</small>
          </button>
          <button
            type="button"
            :class="{ active: activePanel === 'tasks' }"
            :disabled="!hasScope('remote.peer.task.control')"
            @click="selectPanel('tasks')"
          >
            <span aria-hidden="true">✓</span><span>任务管理</span
            ><small v-if="!remoteEnabled">⌑</small>
          </button>
        </nav>

        <section class="conversation-sidebar" aria-label="AI 对话列表">
          <header v-if="conversationSearchOpen" class="conversation-search">
            <input
              v-model="conversationQuery"
              type="search"
              autofocus
              placeholder="搜索会话"
              aria-label="搜索会话"
            />
            <button type="button" title="关闭搜索" @click="closeConversationSearch">×</button>
          </header>
          <header v-else>
            <span aria-hidden="true">◌</span>
            <strong>对话</strong>
            <small>{{ chatSidebar.conversations.length }}</small>
            <button
              type="button"
              title="搜索会话"
              :disabled="!selectedProjectId"
              @click="conversationSearchOpen = true"
            >
              ⌕
            </button>
            <button
              type="button"
              title="新对话"
              :disabled="!selectedProjectId || Boolean(chatUnavailableReason)"
              @click="createConversation"
            >
              ＋
            </button>
          </header>
          <div class="conversation-list">
            <p v-if="!selectedProjectId">选择项目后，会话将显示在这里。</p>
            <p v-else-if="chatUnavailableReason">{{ chatUnavailableReason }}</p>
            <p v-else-if="chatSidebar.loading && chatSidebar.conversations.length === 0">
              正在读取会话…
            </p>
            <p v-else-if="filteredConversations.length === 0">
              {{ conversationQuery ? '没有匹配的会话。' : '还没有 AI 对话。' }}
            </p>
            <button
              v-for="conversation in filteredConversations"
              :key="conversation.id"
              class="conversation-tile"
              :class="{
                active: chatSidebar.activeId === conversation.id && activePanel === 'chat',
              }"
              type="button"
              @click="openConversation(conversation)"
            >
              <span>
                <strong>{{ conversation.title || '新对话' }}</strong>
                <small>{{ formatConversationTime(conversation.updatedAt) }}</small>
              </span>
              <span
                class="conversation-delete"
                role="button"
                tabindex="0"
                title="删除会话"
                @click.stop="deleteConversation(conversation)"
                @keydown.enter.stop="deleteConversation(conversation)"
                >×</span
              >
            </button>
            <button
              v-if="chatSidebar.hasMore"
              class="conversation-more"
              type="button"
              @click="chatPanel?.loadMoreConversations()"
            >
              加载更多
            </button>
          </div>
        </section>

        <footer class="workspace-account">
          <span class="account-avatar">{{ accountInitial }}</span>
          <span :title="accountName">{{ accountName }}</span>
          <button
            type="button"
            title="设备设置"
            :class="{ active: activePanel === 'ai' }"
            @click="selectPanel('ai')"
          >
            ⚙
          </button>
        </footer>
      </aside>

      <button
        v-if="sidebarVisible"
        class="workspace-resizer sidebar-resizer"
        type="button"
        role="separator"
        aria-label="调整左侧栏宽度"
        aria-orientation="vertical"
        :aria-valuenow="sidebarWidth"
        aria-valuemin="240"
        title="拖动调整左侧栏；按 Home 恢复默认宽度"
        @pointerdown="beginPaneResize('sidebar', $event)"
        @keydown="resizePaneWithKeyboard('sidebar', $event)"
      ></button>

      <main class="workspace-content" :class="{ 'without-header': !showContentHeader }">
        <header v-if="showContentHeader" class="workspace-content-header">
          <h1>{{ panelTitle }}</h1>
          <span v-if="selectedProject" :title="selectedProject.id">{{
            selectedProject.displayName
          }}</span>
        </header>

        <div class="workspace-notices">
          <div v-if="rpc.error.value" class="workspace-notice error" role="alert">
            <span>加密连接：{{ rpc.error.value }}</span>
            <button type="button" :disabled="actionPending" @click="rebuildController">
              重建浏览器控制器
            </button>
          </div>
          <div v-else-if="rpc.reconnecting.value" class="workspace-notice">
            加密连接已中断，正在重新会合…
          </div>
          <div v-if="errorMessage" class="workspace-notice error" role="alert">
            {{ errorMessage }}
          </div>
        </div>

        <div v-if="loading" class="workspace-loading">正在读取设备…</div>
        <div v-else-if="!device" class="workspace-loading">找不到该设备。</div>
        <div v-else class="workspace-panel-stack">
          <RemoteProjectsPanel
            v-if="hasScope('remote.project.read')"
            v-show="activePanel === 'projects'"
            :key="device.id"
            :device-id="device.id"
            :can-sync="hasScope('remote.project.sync')"
            :can-delete="canDeleteProjects"
            :selected-project-id="selectedProjectId"
            @loaded="acceptProjects"
            @select="selectProject"
          />

          <RemoteFilesPanel
            v-if="
              activePanel === 'files' &&
              hasScope('remote.peer.file.receive') &&
              selectedProjectId &&
              !filesUnavailableReason
            "
            :key="`files:${device.id}:${selectedProjectId}:${capabilityVersion}`"
            :project-id="selectedProjectId"
            :writable="hasScope('remote.peer.file.send')"
          />

          <RemoteTerminalPanel
            v-if="terminalActivated && selectedProjectId"
            v-show="activePanel === 'terminal'"
            ref="terminalPanel"
            :key="`terminal:${device.id}:${selectedProjectId}:${capabilityVersion}`"
            :project-id="selectedProjectId"
            :active="activePanel === 'terminal'"
            :interactive-authorized="hasScope('remote.peer.terminal.interactive')"
            :legacy-authorized="hasScope('remote.peer.terminal')"
            :capabilities="agentCapabilities"
            :capability-error="capabilityError"
            @sessions="terminalBackgroundCount = $event.backgroundCount"
          />

          <RemoteTasksPanel
            v-if="
              activePanel === 'tasks' &&
              hasScope('remote.peer.task.control') &&
              selectedProjectId &&
              !tasksUnavailableReason
            "
            :key="`tasks:${device.id}:${selectedProjectId}:${capabilityVersion}`"
            :device-id="device.id"
            :device-name="device.deviceName"
            :project-id="selectedProjectId"
            :protocol-version="REMOTE_AGENT_CAPABILITY_PROTOCOL_VERSION"
            :capability-version="capabilityVersion"
            :online="isOnline"
            :writable="hasScope('remote.peer.task.control')"
          />

          <RemoteChatPanel
            v-if="hasScope('remote.peer.ai.chat') && selectedProjectId && !chatUnavailableReason"
            v-show="activePanel === 'chat'"
            ref="chatPanel"
            :key="`chat:${device.id}:${selectedProjectId}:${capabilityVersion}`"
            :device-id="device.id"
            :project-id="selectedProjectId"
            :protocol-version="REMOTE_AGENT_CAPABILITY_PROTOCOL_VERSION"
            :capability-version="capabilityVersion"
            :tools-authorized="hasScope('remote.peer.ai.tools')"
            :attachments-authorized="hasScope('remote.peer.file.send')"
            :show-conversation-list="false"
            workspace
            @sidebar-state="receiveChatSidebar"
          />

          <div v-if="activePanel === 'ai'" class="settings-workspace">
            <section class="device-settings-card">
              <header>
                <div>
                  <span class="device-live-dot" :class="{ online: isOnline }"></span>
                  <strong>{{ device.deviceName }}</strong>
                  <small>{{ device.platform }} · Agent {{ device.agentVersion }}</small>
                </div>
                <button
                  v-if="!remoteEnabled"
                  class="primary"
                  type="button"
                  :disabled="actionPending"
                  @click="enable"
                >
                  开启远程访问
                </button>
                <button
                  v-else
                  class="danger"
                  type="button"
                  :disabled="actionPending"
                  @click="revoke"
                >
                  关闭远程访问
                </button>
              </header>
              <p>
                设备 ID {{ device.id }} · Grant
                {{ device.grantVersion }}。终端和文件仍受目标项目本地策略约束。
              </p>
              <button type="button" :disabled="actionPending" @click="rebuildController">
                重建此浏览器的控制器密钥
              </button>
            </section>
            <RemoteAIConfigPanel :writable="hasScope('remote.peer.ai.config')" />
          </div>

          <div
            v-if="
              (activePanel === 'files' ||
                activePanel === 'terminal' ||
                activePanel === 'tasks' ||
                activePanel === 'chat') &&
              !selectedProjectId
            "
            class="workspace-empty"
          >
            <span aria-hidden="true">▱</span>
            <strong>请先选择项目</strong>
            <p>文件、终端、任务和 AI 对话始终绑定到一个已登记项目。</p>
          </div>
          <div
            v-else-if="activePanel === 'files' && filesUnavailableReason"
            class="workspace-empty"
          >
            <strong>文件管理不可用</strong>
            <p>{{ filesUnavailableReason }}</p>
          </div>
          <div
            v-else-if="activePanel === 'tasks' && tasksUnavailableReason"
            class="workspace-empty"
          >
            <strong>任务管理不可用</strong>
            <p>{{ tasksUnavailableReason }}</p>
          </div>
          <div v-else-if="activePanel === 'chat' && chatUnavailableReason" class="workspace-empty">
            <strong>AI 对话不可用</strong>
            <p>{{ chatUnavailableReason }}</p>
          </div>
          <div v-else-if="!remoteEnabled && activePanel !== 'ai'" class="workspace-empty">
            <span aria-hidden="true">⌑</span>
            <strong>远程访问未开启</strong>
            <p>请在左下角设备设置中开启远程访问。</p>
            <button type="button" @click="selectPanel('ai')">打开设备设置</button>
          </div>
        </div>
      </main>

      <button
        v-if="rightPanelVisible && device"
        class="workspace-resizer right-resizer"
        type="button"
        role="separator"
        aria-label="调整右侧栏宽度"
        aria-orientation="vertical"
        :aria-valuenow="rightPanelWidth"
        aria-valuemin="320"
        title="拖动调整右侧栏；按 Home 恢复默认宽度"
        @pointerdown="beginPaneResize('right', $event)"
        @keydown="resizePaneWithKeyboard('right', $event)"
      ></button>

      <RemoteSidePanel
        v-if="rightPanelVisible && device"
        :device-id="device.id"
        :device-name="device.deviceName"
        :project-id="selectedProjectId"
        :protocol-version="REMOTE_AGENT_CAPABILITY_PROTOCOL_VERSION"
        :capability-version="capabilityVersion"
        :online="isOnline"
        :writable="remoteEnabled"
        :files-available="remoteEnabled && !Boolean(filesUnavailableReason)"
        :tasks-available="remoteEnabled && !Boolean(tasksUnavailableReason)"
      />
    </div>
  </section>
</template>

<style scoped>
.remote-workspace {
  display: grid;
  grid-template-rows: 42px minmax(0, 1fr);
  width: 100%;
  height: 100vh;
  overflow: hidden;
  background: #f8faf9;
}
.workspace-titlebar {
  position: relative;
  z-index: 50;
  display: grid;
  grid-template-columns: 1fr minmax(220px, 340px) 1fr;
  align-items: center;
  gap: 14px;
  border-bottom: 1px solid var(--line);
  padding: 0 10px;
  background: #fff;
}
.workspace-titlebar-side {
  display: flex;
  min-width: 0;
  align-items: center;
  gap: 8px;
}
.workspace-titlebar-side.end {
  justify-content: flex-end;
}
.pane-toggle {
  display: grid;
  width: 32px;
  height: 32px;
  place-items: center;
  border: 0;
  border-radius: 7px;
  padding: 0;
  color: var(--ink-soft);
  background: transparent;
  font-size: 1rem;
  cursor: pointer;
}
.pane-toggle:hover,
.pane-toggle:focus-visible {
  background: var(--paper-soft);
}
.workspace-brand {
  display: flex;
  min-width: 0;
  align-items: center;
  gap: 8px;
  color: var(--ink);
  font-size: 0.78rem;
  font-weight: 680;
  text-decoration: none;
}
.workspace-brand :deep(.brand-mark) {
  width: 25px;
  height: 25px;
}
.device-switcher {
  position: relative;
  display: flex;
  min-width: 0;
}
.device-switcher-dot {
  position: absolute;
  z-index: 1;
  top: 50%;
  left: 12px;
  width: 7px;
  height: 7px;
  border-radius: 50%;
  background: var(--ink-faint);
  transform: translateY(-50%);
  pointer-events: none;
}
.device-switcher-dot.online {
  background: #16a34a;
}
.device-switcher select {
  width: 100%;
  min-height: 34px;
  overflow: hidden;
  border: 1px solid var(--line);
  border-radius: 999px;
  padding: 0 34px 0 29px;
  color: var(--ink);
  background: #fff;
  font-size: 0.76rem;
  font-weight: 650;
  text-overflow: ellipsis;
}
.encrypted-connection-state {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  color: var(--ink-soft);
  font-size: 0.7rem;
  font-weight: 650;
}
.encrypted-connection-state i {
  width: 6px;
  height: 6px;
  border-radius: 50%;
  background: var(--ink-faint);
}
.encrypted-connection-state.connected {
  color: var(--teal-dark);
}
.encrypted-connection-state.connected i {
  background: var(--teal);
}
.encrypted-connection-state.reconnecting i {
  background: #f79009;
}
.workspace-panes {
  position: relative;
  display: grid;
  grid-template-columns:
    var(--workspace-sidebar-width, 272px) 6px minmax(360px, 1fr) 6px
    var(--workspace-right-panel-width, 384px);
  min-width: 0;
  min-height: 0;
  overflow: hidden;
}
.workspace-panes.without-sidebar {
  grid-template-columns: minmax(360px, 1fr) 6px var(--workspace-right-panel-width, 384px);
}
.workspace-panes.without-right-panel {
  grid-template-columns: var(--workspace-sidebar-width, 272px) 6px minmax(360px, 1fr);
}
.workspace-panes.without-sidebar.without-right-panel {
  grid-template-columns: minmax(0, 1fr);
}
.workspace-resizer {
  position: relative;
  z-index: 8;
  width: 6px;
  min-width: 6px;
  height: 100%;
  border: 0;
  border-radius: 0;
  padding: 0;
  background: var(--line);
  cursor: col-resize;
  touch-action: none;
}
.workspace-resizer::after {
  position: absolute;
  inset: 0 -3px;
  content: '';
}
.workspace-resizer:hover,
.workspace-resizer:focus-visible {
  background: var(--teal);
  outline: none;
}
.workspace-sidebar {
  display: grid;
  grid-template-rows: auto auto minmax(0, 1fr) auto;
  min-width: 0;
  min-height: 0;
  border-right: 1px solid var(--line);
  background: #f4f6f5;
}
.project-selector {
  display: grid;
  grid-template-columns: auto minmax(0, 1fr) auto;
  align-items: center;
  gap: 7px;
  margin: 12px 10px 10px;
  border: 1px solid var(--line);
  border-radius: 10px;
  padding: 0 5px 0 10px;
  color: var(--teal-dark);
  background: #fff;
}
.project-selector select {
  min-width: 0;
  height: 42px;
  border: 0;
  color: var(--ink);
  background: transparent;
  font-size: 0.8rem;
  font-weight: 650;
}
.project-selector button,
.conversation-sidebar header button,
.workspace-account button {
  display: grid;
  width: 30px;
  height: 30px;
  place-items: center;
  border: 0;
  border-radius: 7px;
  padding: 0;
  color: var(--ink-soft);
  background: transparent;
  cursor: pointer;
}
.project-selector button:hover,
.conversation-sidebar header button:hover,
.workspace-account button:hover,
.workspace-account button.active {
  color: var(--teal-dark);
  background: var(--brand-tint);
}
.workspace-destinations {
  display: grid;
  gap: 3px;
  padding: 0 8px 4px;
}
.workspace-destinations button {
  display: grid;
  grid-template-columns: 18px minmax(0, 1fr) auto;
  align-items: center;
  gap: 10px;
  border: 0;
  border-radius: 8px;
  padding: 8px 10px;
  color: var(--ink);
  background: transparent;
  font: inherit;
  font-size: 0.82rem;
  font-weight: 520;
  text-align: left;
  cursor: pointer;
}
.workspace-destinations button.active {
  color: var(--teal-dark);
  background: var(--brand-tint);
  font-weight: 680;
}
.workspace-destinations button:disabled {
  color: var(--ink-faint);
  cursor: not-allowed;
}
.terminal-background-action {
  border-radius: 999px;
  padding: 2px 6px;
  color: #7a4e00;
  background: #ffedc2;
  font-size: 0.62rem;
  white-space: nowrap;
}
.conversation-sidebar {
  display: grid;
  grid-template-rows: auto minmax(0, 1fr);
  min-height: 0;
  padding: 8px 8px 6px;
}
.conversation-sidebar > header {
  display: grid;
  grid-template-columns: auto auto auto 1fr auto;
  align-items: center;
  gap: 5px;
  min-height: 32px;
  padding: 0 2px 5px 7px;
  color: var(--ink-soft);
}
.conversation-sidebar > header strong {
  color: var(--ink);
  font-size: 0.76rem;
}
.conversation-sidebar > header small {
  border-radius: 999px;
  padding: 1px 6px;
  background: #e7ebe9;
  font-size: 0.65rem;
}
.conversation-search {
  grid-template-columns: minmax(0, 1fr) auto !important;
  padding-left: 0 !important;
}
.conversation-search input {
  min-width: 0;
  height: 32px;
  border: 0;
  border-radius: 8px;
  padding: 0 10px;
  background: #e7ebe9;
  font-size: 0.75rem;
}
.conversation-list {
  min-height: 0;
  overflow: auto;
}
.conversation-list > p {
  margin: 10px;
  color: var(--ink-soft);
  font-size: 0.72rem;
  line-height: 1.5;
  text-align: center;
}
.conversation-tile {
  display: grid;
  grid-template-columns: minmax(0, 1fr) auto;
  width: 100%;
  align-items: center;
  gap: 5px;
  border: 0;
  border-radius: 8px;
  padding: 7px 6px 7px 10px;
  color: var(--ink);
  background: transparent;
  text-align: left;
  cursor: pointer;
}
.conversation-tile:hover,
.conversation-tile.active {
  background: var(--brand-tint);
}
.conversation-tile.active {
  color: var(--teal-dark);
}
.conversation-tile > span:first-child {
  display: grid;
  min-width: 0;
  gap: 1px;
}
.conversation-tile strong,
.conversation-tile small {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.conversation-tile strong {
  font-size: 0.76rem;
  font-weight: 570;
}
.conversation-tile small {
  color: var(--ink-soft);
  font-size: 0.64rem;
}
.conversation-delete {
  display: grid;
  width: 25px;
  height: 25px;
  place-items: center;
  border-radius: 6px;
  color: var(--ink-soft);
  opacity: 0;
}
.conversation-tile:hover .conversation-delete,
.conversation-tile.active .conversation-delete,
.conversation-delete:focus-visible {
  opacity: 1;
}
.conversation-delete:hover {
  background: rgb(0 0 0 / 7%);
}
.conversation-more {
  display: block;
  margin: 8px auto;
  border: 0;
  color: var(--teal-dark);
  background: transparent;
  font-size: 0.72rem;
}
.workspace-account {
  display: grid;
  grid-template-columns: auto minmax(0, 1fr) auto;
  align-items: center;
  gap: 9px;
  border-top: 1px solid var(--line);
  padding: 10px 8px 10px 14px;
}
.account-avatar {
  display: grid;
  width: 30px;
  height: 30px;
  place-items: center;
  border-radius: 50%;
  color: #fff;
  background: var(--teal);
  font-size: 0.72rem;
}
.workspace-account > span:nth-child(2) {
  overflow: hidden;
  font-size: 0.76rem;
  font-weight: 650;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.workspace-content {
  display: grid;
  grid-template-rows: auto auto minmax(0, 1fr);
  min-width: 0;
  min-height: 0;
  overflow: hidden;
  background: #fbfcfb;
}
.workspace-content.without-header {
  grid-template-rows: auto minmax(0, 1fr);
}
.workspace-content-header {
  display: flex;
  min-height: 48px;
  align-items: center;
  justify-content: space-between;
  gap: 14px;
  border-bottom: 1px solid var(--line);
  padding: 0 28px;
  background: #fff;
}
.workspace-content-header h1 {
  overflow: hidden;
  margin: 0;
  font-size: 0.96rem;
  font-weight: 680;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.workspace-content-header span {
  overflow: hidden;
  color: var(--ink-soft);
  font-size: 0.7rem;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.workspace-notice {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  border-bottom: 1px solid #f2d18c;
  padding: 8px 14px;
  color: #704000;
  background: #fff8e8;
  font-size: 0.75rem;
}
.workspace-notice.error {
  border-color: #efc0bc;
  color: #9b3028;
  background: #fff4f2;
}
.workspace-notice button {
  border: 1px solid currentColor;
  border-radius: 7px;
  padding: 5px 8px;
  color: inherit;
  background: #fff;
}
.workspace-loading,
.workspace-empty {
  display: grid;
  min-height: 0;
  height: 100%;
  place-content: center;
  justify-items: center;
  gap: 8px;
  padding: 30px;
  color: var(--ink-soft);
  text-align: center;
}
.workspace-empty > span {
  font-size: 2rem;
  color: var(--ink-faint);
}
.workspace-empty strong {
  color: var(--ink);
}
.workspace-empty p {
  max-width: 34rem;
  margin: 0;
}
.workspace-panel-stack {
  position: relative;
  min-width: 0;
  min-height: 0;
  overflow: auto;
}
.workspace-panel-stack > :deep(.remote-panel),
.workspace-panel-stack > :deep(.projects-panel) {
  box-sizing: border-box;
  min-height: 100%;
  border: 0;
  border-radius: 0;
  padding: 24px 28px;
  background: #fff;
  box-shadow: none;
}
.workspace-panel-stack > :deep(.chat-panel.workspace) {
  padding: 0;
}
.workspace-panel-stack > :deep(.terminal-panel) {
  padding: 12px 28px 24px;
}
.settings-workspace {
  display: grid;
  gap: 16px;
  padding: 22px 28px 30px;
}
.settings-workspace > :deep(.remote-panel) {
  border: 1px solid var(--line);
  border-radius: 12px;
  padding: 20px;
  background: #fff;
}
.device-settings-card {
  border: 1px solid var(--line);
  border-radius: 12px;
  padding: 16px;
  background: #fff;
}
.device-settings-card header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 14px;
}
.device-settings-card header > div {
  display: grid;
  grid-template-columns: auto auto;
  align-items: center;
  gap: 3px 8px;
}
.device-settings-card header small {
  grid-column: 2;
  color: var(--ink-soft);
}
.device-live-dot {
  grid-row: 1 / span 2;
  width: 8px;
  height: 8px;
  border-radius: 50%;
  background: var(--ink-faint);
}
.device-live-dot.online {
  background: #16a34a;
}
.device-settings-card p {
  color: var(--ink-soft);
  font-size: 0.78rem;
}
.device-settings-card button,
:deep(.remote-panel button) {
  border: 1px solid var(--line);
  border-radius: 8px;
  padding: 8px 11px;
  color: var(--ink);
  background: #fff;
  font: inherit;
  font-weight: 650;
  cursor: pointer;
}
.device-settings-card button.primary {
  border-color: var(--teal);
  color: #fff;
  background: var(--teal);
}
.device-settings-card button.danger {
  border-color: #efc0bc;
  color: #a53a31;
}
:deep(.remote-panel button:disabled) {
  cursor: not-allowed;
  opacity: 0.5;
}
:deep(.remote-panel-heading) {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 20px;
  margin-bottom: 18px;
}
:deep(.remote-panel-heading h2),
:deep(.remote-panel-heading h3) {
  margin: 0 0 5px;
}
:deep(.remote-panel-heading p) {
  margin: 0;
  color: var(--ink-soft);
  line-height: 1.55;
}
:deep(.remote-notice) {
  border-radius: 9px;
  padding: 10px 12px;
  font-size: 0.76rem;
}
:deep(.remote-notice.warning) {
  border: 1px solid #eed08e;
  background: #fff8e8;
}
:deep(.remote-notice.success),
:deep(.remote-notice.info) {
  border: 1px solid var(--mint);
  color: var(--teal-dark);
  background: var(--brand-tint);
}
:deep(.remote-notice.error) {
  border: 1px solid #efc0bc;
  color: #9b3028;
  background: #fff4f2;
}
:deep(.remote-panel-empty) {
  border: 1px dashed var(--line);
  border-radius: 12px;
  padding: 28px;
  color: var(--ink-soft);
  text-align: center;
}
:deep(.remote-load-more) {
  display: block;
  margin: 16px auto 0;
}
:deep(.file-heading-actions),
:deep(.chat-heading-actions) {
  display: flex;
  flex-wrap: wrap;
  gap: 7px;
}
:deep(.file-search) {
  display: grid;
  grid-template-columns: minmax(180px, 1fr) auto auto;
  gap: 8px;
  margin: 12px 0;
}
:deep(.file-search input) {
  min-width: 0;
  border: 1px solid var(--line);
  border-radius: 8px;
  padding: 9px 11px;
}
:deep(.file-breadcrumbs) {
  display: flex;
  gap: 5px;
  overflow-x: auto;
  margin-bottom: 12px;
}
:deep(.file-breadcrumbs button) {
  border: 0;
  padding: 5px;
  color: var(--teal-dark);
  background: transparent;
}
:deep(.file-layout) {
  display: grid;
  grid-template-columns: minmax(300px, 0.9fr) minmax(320px, 1.1fr);
  min-height: 520px;
  border: 1px solid var(--line);
  border-radius: 10px;
  overflow: hidden;
}
:deep(.file-list) {
  overflow: auto;
  border-right: 1px solid var(--line);
}
:deep(.file-list article) {
  display: grid;
  grid-template-columns: minmax(0, 1fr) auto;
  align-items: center;
  border-bottom: 1px solid var(--line);
  padding: 7px;
}
:deep(.file-list article.selected) {
  background: var(--brand-tint);
}
:deep(.file-open) {
  display: flex;
  min-width: 0;
  align-items: center;
  gap: 10px;
  border-color: transparent !important;
  text-align: left;
}
:deep(.file-open > span:last-child) {
  display: grid;
  min-width: 0;
  gap: 3px;
}
:deep(.file-open strong),
:deep(.file-open small) {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
:deep(.file-open small) {
  color: var(--ink-faint);
}
:deep(.file-actions) {
  display: flex;
  gap: 3px;
}
:deep(.file-actions button) {
  border: 0;
  padding: 6px;
  color: var(--teal-dark);
  background: transparent;
  font-size: 0.7rem;
}
:deep(.file-preview) {
  display: grid;
  grid-template-rows: auto minmax(0, 1fr);
  min-width: 0;
  padding: 14px;
}
:deep(.file-preview-head) {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  margin-bottom: 9px;
}
:deep(.file-preview-head > span) {
  display: grid;
  min-width: 0;
  gap: 3px;
}
:deep(.file-preview-head small) {
  color: var(--ink-faint);
}
:deep(.file-preview textarea) {
  box-sizing: border-box;
  width: 100%;
  min-height: 450px;
  border: 1px solid var(--line);
  border-radius: 8px;
  padding: 12px;
  font-family: 'Cascadia Mono', 'SFMono-Regular', Consolas, monospace;
  resize: none;
}
:deep(.chat-layout) {
  display: grid;
  grid-template-columns: 230px minmax(0, 1fr);
  min-height: 540px;
  border: 1px solid var(--line);
  border-radius: 10px;
  overflow: hidden;
}
:deep(.chat-layout.without-sidebar) {
  grid-template-columns: minmax(0, 1fr);
}
:deep(.chat-layout aside) {
  display: grid;
  align-content: start;
  gap: 3px;
  border-right: 1px solid var(--line);
  padding: 10px;
  background: var(--paper-soft);
}
:deep(.chat-main) {
  position: relative;
  display: grid;
  grid-template-rows: auto auto auto minmax(0, 1fr) auto;
  min-width: 0;
  min-height: 0;
  padding: 16px;
}
:deep(.message-list) {
  display: grid;
  align-content: start;
  gap: 12px;
  min-height: 220px;
  overflow: auto;
  padding: 10px 0;
}
:deep(.message-list article) {
  max-width: 82%;
  border-radius: 11px;
  padding: 10px 12px;
  background: var(--paper-soft);
}
:deep(.message-list article.user) {
  justify-self: end;
  background: var(--brand-tint);
}
:deep(.message-list article p) {
  margin: 4px 0 0;
  white-space: pre-wrap;
}
:deep(.chat-composer) {
  display: grid;
  grid-template-columns: minmax(0, 1fr) auto;
  align-items: end;
  gap: 8px;
}
:deep(.chat-composer textarea) {
  box-sizing: border-box;
  width: 100%;
  border: 1px solid var(--line);
  border-radius: 9px;
  padding: 10px;
  resize: vertical;
}
@media (max-width: 1120px) {
  .workspace-panes,
  .workspace-panes.without-right-panel {
    grid-template-columns: var(--workspace-sidebar-width, 272px) 6px minmax(360px, 1fr);
  }
  .workspace-panes.without-sidebar,
  .workspace-panes.without-sidebar.without-right-panel {
    grid-template-columns: minmax(0, 1fr);
  }
  .workspace-panes > :deep(.remote-side-panel) {
    position: absolute;
    z-index: 35;
    top: 0;
    right: 0;
    bottom: 0;
    width: min(380px, calc(100% - 36px));
    box-shadow: -12px 0 28px rgb(31 52 46 / 13%);
  }
  .right-resizer {
    display: none;
  }
}
@media (max-width: 760px) {
  .workspace-titlebar {
    grid-template-columns: auto minmax(120px, 1fr) auto;
    gap: 5px;
  }
  .workspace-brand span,
  .encrypted-connection-state span {
    display: none;
  }
  .encrypted-connection-state {
    width: 20px;
    justify-content: center;
    font-size: 0;
  }
  .workspace-panes,
  .workspace-panes.without-right-panel,
  .workspace-panes.without-sidebar,
  .workspace-panes.without-sidebar.without-right-panel {
    grid-template-columns: minmax(0, 1fr);
  }
  .workspace-resizer {
    display: none;
  }
  .workspace-sidebar {
    position: absolute;
    z-index: 40;
    top: 0;
    bottom: 0;
    left: 0;
    width: min(280px, calc(100% - 40px));
    box-shadow: 12px 0 28px rgb(31 52 46 / 16%);
  }
  .workspace-content-header {
    padding: 0 14px;
  }
  .workspace-panel-stack > :deep(.remote-panel),
  .workspace-panel-stack > :deep(.projects-panel),
  .settings-workspace {
    padding: 16px 12px 24px;
  }
  :deep(.remote-panel-heading),
  .device-settings-card header {
    display: grid;
  }
  :deep(.file-search),
  :deep(.file-layout),
  :deep(.chat-layout),
  :deep(.chat-composer) {
    grid-template-columns: 1fr;
  }
  :deep(.file-list),
  :deep(.chat-layout aside) {
    border-right: 0;
    border-bottom: 1px solid var(--line);
  }
}
</style>
