<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'

import RemoteFilesPanel from '@/components/remote/RemoteFilesPanel.vue'
import RemoteTasksPanel from '@/components/remote/RemoteTasksPanel.vue'

type SideTabType = 'web' | 'files' | 'tasks'

interface SideTab {
  id: number
  type: SideTabType
  title: string
}

interface SideTabContextMenu {
  tabId: number
  x: number
  y: number
}

const props = defineProps<{
  deviceId: string
  deviceName: string
  projectId: string
  protocolVersion: number
  capabilityVersion: string
  online: boolean
  writable: boolean
  filesAvailable: boolean
  tasksAvailable: boolean
}>()

const tabs = ref<SideTab[]>([])
const activeId = ref(0)
const address = ref('https://')
const contextMenu = ref<SideTabContextMenu | null>(null)
let nextId = 1

const definitions: Record<SideTabType, { title: string; icon: string; description: string }> = {
  web: { title: '浏览网页', icon: '◎', description: '快速打开网址与常用站点' },
  files: { title: '阅读文件', icon: '▤', description: '在右侧浏览远程项目文件' },
  tasks: { title: '任务管理', icon: '✓', description: '查看与跟进设备端任务' },
}

const quickLinks = [
  ['必应', 'https://www.bing.com'],
  ['GitHub', 'https://github.com'],
  ['Stack Overflow', 'https://stackoverflow.com'],
  ['MDN', 'https://developer.mozilla.org'],
  ['pub.dev', 'https://pub.dev'],
  ['Dart', 'https://dart.dev'],
] as const

const tabEnabled = (type: SideTabType) => {
  if (type === 'files') return props.filesAvailable
  if (type === 'tasks') return props.tasksAvailable
  return true
}

const openTab = (type: SideTabType) => {
  if (!tabEnabled(type)) return
  const existing = tabs.value.find((tab) => tab.type === type)
  if (existing) {
    activeId.value = existing.id
    return
  }
  const tab: SideTab = { id: nextId++, type, title: definitions[type].title }
  tabs.value.push(tab)
  activeId.value = tab.id
}

const closeTabs = (ids: Set<number>) => {
  const activeIndex = tabs.value.findIndex((tab) => tab.id === activeId.value)
  if (!tabs.value.some((tab) => ids.has(tab.id))) return
  tabs.value = tabs.value.filter((tab) => !ids.has(tab.id))
  if (!tabs.value.some((tab) => tab.id === activeId.value)) {
    activeId.value = tabs.value[Math.min(Math.max(activeIndex, 0), tabs.value.length - 1)]?.id ?? 0
  }
  contextMenu.value = null
}

const closeTab = (id: number) => closeTabs(new Set([id]))

const closeOtherTabs = (id: number) =>
  closeTabs(new Set(tabs.value.filter((tab) => tab.id !== id).map((tab) => tab.id)))

const closeTabsBeside = (id: number, side: 'left' | 'right') => {
  const index = tabs.value.findIndex((tab) => tab.id === id)
  if (index < 0) return
  const candidates = side === 'left' ? tabs.value.slice(0, index) : tabs.value.slice(index + 1)
  closeTabs(new Set(candidates.map((tab) => tab.id)))
}

const openTabContextMenu = (tab: SideTab, event: MouseEvent) => {
  event.preventDefault()
  activeId.value = tab.id
  contextMenu.value = {
    tabId: tab.id,
    x: Math.max(8, Math.min(event.clientX, window.innerWidth - 176)),
    y: Math.max(8, Math.min(event.clientY, window.innerHeight - 170)),
  }
}

const closeContextMenu = () => {
  contextMenu.value = null
}

const contextTabIndex = computed(() =>
  contextMenu.value ? tabs.value.findIndex((tab) => tab.id === contextMenu.value!.tabId) : -1,
)

const resetTabs = () => {
  tabs.value = []
  activeId.value = 0
  contextMenu.value = null
}

const openAddress = (value?: string) => {
  if (value) address.value = value
  const raw = address.value.trim()
  if (!raw) return
  try {
    const target = new URL(raw.includes('://') ? raw : `https://${raw}`)
    if (target.protocol !== 'http:' && target.protocol !== 'https:') {
      throw new Error('unsupported protocol')
    }
    window.open(target.href, '_blank', 'noopener,noreferrer')
  } catch {
    window.alert('请输入有效的 HTTP 或 HTTPS 网址。')
  }
}

watch([() => props.deviceId, () => props.projectId], resetTabs)

onMounted(() => {
  window.addEventListener('click', closeContextMenu)
  window.addEventListener('blur', closeContextMenu)
})

onBeforeUnmount(() => {
  window.removeEventListener('click', closeContextMenu)
  window.removeEventListener('blur', closeContextMenu)
})
</script>

<template>
  <aside class="remote-side-panel" aria-label="右侧辅助栏">
    <div v-if="tabs.length" class="side-tab-strip">
      <div class="side-tab-scroll" role="tablist">
        <div
          v-for="tab in tabs"
          :key="tab.id"
          class="side-tab"
          :class="{ active: tab.id === activeId }"
          @contextmenu="openTabContextMenu(tab, $event)"
        >
          <button
            class="side-tab-select"
            type="button"
            role="tab"
            :aria-selected="tab.id === activeId"
            @click="activeId = tab.id"
          >
            <span aria-hidden="true">{{ definitions[tab.type].icon }}</span>
            {{ tab.title }}
          </button>
          <button
            class="side-tab-close"
            type="button"
            aria-label="关闭标签页"
            @click="closeTab(tab.id)"
          >
            ×
          </button>
        </div>
      </div>
      <details class="side-new-menu">
        <summary title="新建标签页">＋</summary>
        <div>
          <button
            v-for="(definition, type) in definitions"
            :key="type"
            type="button"
            :disabled="!tabEnabled(type) || tabs.some((tab) => tab.type === type)"
            @click="openTab(type)"
          >
            <span aria-hidden="true">{{ definition.icon }}</span
            >{{ definition.title }}
          </button>
        </div>
      </details>
    </div>

    <div
      v-if="contextMenu"
      class="side-tab-context-menu"
      role="menu"
      :style="{ left: `${contextMenu.x}px`, top: `${contextMenu.y}px` }"
      @click.stop
    >
      <button type="button" role="menuitem" @click="closeTab(contextMenu.tabId)">关闭</button>
      <button
        type="button"
        role="menuitem"
        :disabled="tabs.length <= 1"
        @click="closeOtherTabs(contextMenu.tabId)"
      >
        关闭其他
      </button>
      <button
        type="button"
        role="menuitem"
        :disabled="contextTabIndex <= 0"
        @click="closeTabsBeside(contextMenu.tabId, 'left')"
      >
        关闭左侧
      </button>
      <button
        type="button"
        role="menuitem"
        :disabled="contextTabIndex < 0 || contextTabIndex >= tabs.length - 1"
        @click="closeTabsBeside(contextMenu.tabId, 'right')"
      >
        关闭右侧
      </button>
    </div>

    <nav v-if="tabs.length === 0" class="side-navigation" aria-label="辅助视图导航">
      <header>
        <h2>导航</h2>
        <p>选择要在右侧打开的视图，可与远程工作区同时使用。</p>
      </header>
      <button
        v-for="(definition, type) in definitions"
        :key="type"
        type="button"
        :disabled="!tabEnabled(type)"
        @click="openTab(type)"
      >
        <span class="side-nav-icon" aria-hidden="true">{{ definition.icon }}</span>
        <span
          ><strong>{{ definition.title }}</strong
          ><small>{{ definition.description }}</small></span
        >
        <span aria-hidden="true">›</span>
      </button>
    </nav>

    <div v-else class="side-tab-content">
      <div
        v-for="tab in tabs"
        :key="tab.id"
        v-show="tab.id === activeId"
        class="side-tab-view"
        role="tabpanel"
      >
        <form v-if="tab.type === 'web'" class="side-web" @submit.prevent="openAddress()">
          <div class="side-address">
            <span aria-hidden="true">⌕</span>
            <input v-model="address" aria-label="网址" placeholder="输入网址或搜索" />
            <button type="submit" aria-label="打开网址">→</button>
          </div>
          <small>回车前往；链接会在新的浏览器标签页中打开。</small>
          <strong>常用站点</strong>
          <div class="side-quick-links">
            <button
              v-for="link in quickLinks"
              :key="link[1]"
              type="button"
              @click="openAddress(link[1])"
            >
              {{ link[0] }}
            </button>
          </div>
        </form>

        <RemoteFilesPanel
          v-else-if="tab.type === 'files' && projectId"
          :key="`side-files:${deviceId}:${projectId}:${capabilityVersion}`"
          class="side-embedded-panel"
          :project-id="projectId"
          :writable="writable"
        />
        <RemoteTasksPanel
          v-else-if="tab.type === 'tasks' && projectId"
          :key="`side-tasks:${deviceId}:${projectId}:${capabilityVersion}`"
          class="side-embedded-panel"
          :device-id="deviceId"
          :device-name="deviceName"
          :project-id="projectId"
          :protocol-version="protocolVersion"
          :capability-version="capabilityVersion"
          :online="online"
          :writable="writable"
        />
        <div v-else class="side-empty">
          <strong>请先选择项目</strong>
          <p>右侧辅助视图与中央工作区共用同一个远程项目上下文。</p>
        </div>
      </div>
    </div>
  </aside>
</template>

<style scoped>
.remote-side-panel {
  box-sizing: border-box;
  min-width: 0;
  height: 100%;
  overflow: hidden;
  border-left: 1px solid var(--line);
  color: var(--ink);
  background: #f4f6f5;
}
.side-tab-strip {
  display: flex;
  height: 42px;
  align-items: center;
  border-bottom: 1px solid var(--line);
}
.side-tab-context-menu {
  position: fixed;
  z-index: 100;
  display: grid;
  width: 168px;
  border: 1px solid var(--line);
  border-radius: 9px;
  padding: 5px;
  background: #fff;
  box-shadow: var(--shadow-medium);
}
.side-tab-context-menu button {
  border: 0;
  border-radius: 6px;
  padding: 8px 10px;
  color: var(--ink);
  background: transparent;
  font: inherit;
  font-size: 0.76rem;
  text-align: left;
  cursor: pointer;
}
.side-tab-context-menu button:hover:not(:disabled),
.side-tab-context-menu button:focus-visible {
  background: var(--paper-soft);
}
.side-tab-context-menu button:disabled {
  color: var(--ink-faint);
  cursor: not-allowed;
}
.side-tab-scroll {
  display: flex;
  flex: 1;
  gap: 4px;
  min-width: 0;
  overflow-x: auto;
  padding: 6px 4px 6px 8px;
}
.side-tab {
  display: inline-flex;
  flex: 0 0 auto;
  align-items: center;
  height: 30px;
  border-radius: 8px;
  color: var(--ink-soft);
  background: #e7ebe9;
  font: inherit;
  font-size: 0.72rem;
}
.side-tab.active {
  color: var(--teal-dark);
  background: var(--brand-tint);
  font-weight: 700;
}
.side-tab-select {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  height: 30px;
  border: 0;
  padding: 0 4px 0 8px;
  color: inherit;
  background: transparent;
  font: inherit;
  cursor: pointer;
}
.side-tab-close {
  display: grid;
  width: 20px;
  height: 20px;
  place-items: center;
  border: 0;
  border-radius: 5px;
  padding: 0;
  color: inherit;
  background: transparent;
  font-size: 0.9rem;
  cursor: pointer;
}
.side-tab-close:hover {
  background: rgb(0 0 0 / 7%);
}
.side-new-menu {
  position: relative;
  margin-right: 8px;
}
.side-new-menu summary {
  display: grid;
  width: 30px;
  height: 30px;
  place-items: center;
  border-radius: 8px;
  background: #e7ebe9;
  cursor: pointer;
  list-style: none;
}
.side-new-menu summary::-webkit-details-marker {
  display: none;
}
.side-new-menu > div {
  position: absolute;
  z-index: 10;
  top: calc(100% + 6px);
  right: 0;
  display: grid;
  width: 170px;
  border: 1px solid var(--line);
  border-radius: 10px;
  padding: 5px;
  background: #fff;
  box-shadow: var(--shadow-small);
}
.side-new-menu button {
  display: flex;
  gap: 8px;
  border: 0;
  padding: 8px 10px;
  background: transparent;
  text-align: left;
}
.side-navigation {
  display: grid;
  align-content: start;
  gap: 8px;
  height: 100%;
  overflow: auto;
  padding: 18px 14px 20px;
}
.side-navigation header {
  margin: 0 4px 8px;
}
.side-navigation h2 {
  margin: 0 0 4px;
  font-size: 1rem;
}
.side-navigation p {
  margin: 0;
  color: var(--ink-soft);
  font-size: 0.72rem;
  line-height: 1.5;
}
.side-navigation > button {
  display: grid;
  grid-template-columns: 32px minmax(0, 1fr) auto;
  align-items: center;
  gap: 11px;
  border: 1px solid var(--line);
  border-radius: 10px;
  padding: 11px 12px;
  color: var(--ink);
  background: #fff;
  text-align: left;
  cursor: pointer;
}
.side-navigation > button:disabled {
  cursor: not-allowed;
  opacity: 0.46;
}
.side-nav-icon {
  display: grid;
  width: 32px;
  height: 32px;
  place-items: center;
  border-radius: 8px;
  color: var(--teal-dark);
  background: var(--brand-tint);
}
.side-navigation button > span:nth-child(2) {
  display: grid;
  gap: 2px;
}
.side-navigation small {
  color: var(--ink-soft);
  font-size: 0.7rem;
}
.side-tab-content {
  height: calc(100% - 42px);
  overflow: hidden;
}
.side-tab-view {
  height: 100%;
  overflow: auto;
}
.side-web {
  display: grid;
  gap: 14px;
  padding: 16px 14px 20px;
}
.side-address {
  display: grid;
  grid-template-columns: auto minmax(0, 1fr) auto;
  align-items: center;
  gap: 6px;
  border-radius: 8px;
  padding-left: 9px;
  background: #e7ebe9;
}
.side-address input {
  min-width: 0;
  border: 0;
  padding: 9px 0;
  background: transparent;
}
.side-address button {
  border: 0;
  border-radius: 7px;
  padding: 8px 11px;
  color: #fff;
  background: var(--teal);
}
.side-web > small {
  margin-top: -8px;
  color: var(--ink-soft);
}
.side-web > strong {
  color: var(--ink-soft);
  font-size: 0.75rem;
}
.side-quick-links {
  display: flex;
  flex-wrap: wrap;
  gap: 7px;
}
.side-quick-links button {
  border: 1px solid var(--line);
  border-radius: 8px;
  padding: 6px 9px;
  background: #fff;
}
.side-empty {
  display: grid;
  min-height: 300px;
  place-content: center;
  padding: 22px;
  text-align: center;
}
.side-empty p {
  max-width: 260px;
  color: var(--ink-soft);
  font-size: 0.75rem;
}
:deep(.side-embedded-panel) {
  box-sizing: border-box;
  min-width: 0;
  min-height: 100%;
  border: 0;
  border-radius: 0;
  padding: 14px;
  box-shadow: none;
}
:deep(.side-embedded-panel .remote-panel-heading) {
  display: grid;
}
:deep(.side-embedded-panel .file-layout) {
  grid-template-columns: minmax(0, 1fr);
}
:deep(.side-embedded-panel .file-list) {
  max-height: 420px;
  border-right: 0;
  border-bottom: 1px solid var(--line);
}
:deep(.side-embedded-panel .file-toolbar) {
  grid-template-columns: auto 1fr;
}
:deep(.side-embedded-panel .file-sort-actions) {
  grid-column: 1 / -1;
  grid-row: 2;
  justify-content: flex-start;
}
</style>
