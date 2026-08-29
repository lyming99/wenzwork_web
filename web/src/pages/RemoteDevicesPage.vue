<script setup lang="ts">
import { useHead } from '@unhead/vue'
import { computed, onMounted, ref } from 'vue'

import {
  createDeviceAccessKey,
  createRemoteIdempotencyKey,
  deleteDeviceAccessKey,
  deleteRemoteDevice,
  listDeviceAccessKeys,
  listRemoteDevices,
  revokeDeviceAccessKey,
  rotateDeviceAccessKey,
  updateRemoteDevice,
  type DeviceAccessKey,
  type RemoteDevice,
} from '@/api/remote'
import { problemMessage } from '@/api/auth'
import { getLatestRelease, type Release } from '@/api/catalog'
import {
  createDeviceInstaller,
  createDeviceEnvironmentFile,
  deviceEnvironmentFileName,
  downloadInstallerFile,
  findPortableAsset,
  installerFileName,
  type PortableTarget,
} from '@/utils/portableInstaller'
import { openRemoteManagerWindow, REMOTE_MANAGER_WINDOW_NAME } from '@/utils/remoteManagerWindow'

useHead({
  title: '远程设备｜WenzWork',
  meta: [{ name: 'robots', content: 'noindex, nofollow' }],
})

const deviceWorkspaceHref = (deviceId: string) => `/remote/${encodeURIComponent(deviceId)}`
const openDeviceWorkspace = (deviceId: string, event: MouseEvent) => {
  if (openRemoteManagerWindow(deviceWorkspaceHref(deviceId))) event.preventDefault()
}
const openInspectedWorkspace = (event: MouseEvent) => {
  const current = inspectedDevice.value
  if (!current) return
  openDeviceWorkspace(current.id, event)
  inspectedDevice.value = null
}

const loading = ref(true)
const loadingMore = ref(false)
const errorMessage = ref('')
const searchQuery = ref('')
const devices = ref<RemoteDevice[]>([])
const nextCursor = ref<string | null>(null)
const observedAt = ref<string | null>(null)
const keys = ref<DeviceAccessKey[]>([])
const keyPanelOpen = ref(false)
const keyName = ref('我的设备')
const keyPending = ref(false)
const createdKey = ref<DeviceAccessKey | null>(null)
const keyError = ref('')
const deletingDeviceId = ref<string | null>(null)
const updatingDeviceId = ref<string | null>(null)
const deletingKeyId = ref<string | null>(null)
const pendingCreateKeyRequest = ref<{ signature: string; idempotencyKey: string } | null>(null)
const pendingRotationKeys = new Map<string, string>()
const latestWebRelease = ref<Release>()
const installTarget = ref<PortableTarget>({ platform: 'windows', architecture: 'x64' })
const installerMessage = ref('')
const inspectedDevice = ref<RemoteDevice | null>(null)
const editingDevice = ref<RemoteDevice | null>(null)
const editedDeviceName = ref('')
const editedDirectModeEnabled = ref(false)
const editError = ref('')
const deviceActionMessage = ref('')
const securePage = typeof window !== 'undefined' && window.location.protocol === 'https:'

const onlineCount = computed(
  () => devices.value.filter((device) => device.presence === 'online').length,
)

const deviceInstallerAsset = computed(() =>
  findPortableAsset(latestWebRelease.value, 'device-agent', installTarget.value),
)

const filteredDevices = computed(() => {
  const query = searchQuery.value.trim().toLocaleLowerCase('zh-CN')
  if (!query) return devices.value
  return devices.value.filter((device) =>
    [
      device.deviceName,
      device.platform,
      platformLabel(device.platform),
      device.agentVersion,
      device.presence,
      presenceLabel(device),
      device.installationDeviceId,
      device.connectionMode,
      device.directIp ?? '',
      device.directPort?.toString() ?? '',
    ].some((value) => value.toLocaleLowerCase('zh-CN').includes(query)),
  )
})

const formatDate = (value?: string | null) => {
  if (!value) return '从未'
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return '未知'
  return new Intl.DateTimeFormat('zh-CN', {
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    hour12: false,
  }).format(date)
}

const platformLabel = (platform: RemoteDevice['platform']) =>
  ({ windows: 'Windows', macos: 'macOS', linux: 'Linux' })[platform]

const presenceLabel = (device: RemoteDevice) => {
  if (device.status === 'revoked') return '已吊销'
  if (device.status !== 'active') return '未启用'
  return { online: '在线', offline: '离线', degraded: '连接不稳定' }[device.presence]
}

const connectionModeLabel = (device: RemoteDevice) => {
  if (!device.directModeEnabled) return 'Relay 中转'
  return device.directAvailable ? 'IP 直连' : '直连不可用'
}

const directEndpointLabel = (device: RemoteDevice) => {
  if (!device.directIp || !device.directPort) return 'Agent 未开启'
  const host = device.directIp.includes(':') ? `[${device.directIp}]` : device.directIp
  return `${device.directTlsEnabled ? 'wss' : 'ws'}://${host}:${device.directPort}`
}

const load = async (append = false) => {
  if (append) loadingMore.value = true
  else loading.value = true
  try {
    const page = await listRemoteDevices(append ? (nextCursor.value ?? undefined) : undefined)
    devices.value = append ? [...devices.value, ...page.items] : page.items
    nextCursor.value = page.nextCursor
    observedAt.value = page.observedAt
    errorMessage.value = ''
  } catch (error) {
    errorMessage.value = problemMessage(error, '暂时无法读取远程设备。')
  } finally {
    loading.value = false
    loadingMore.value = false
  }
}

const loadKeys = async () => {
  try {
    keys.value = await listDeviceAccessKeys()
  } catch {
    // Older servers can still list registered devices without Device Keys.
  }
}

const createKey = async () => {
  if (!keyName.value.trim() || keyPending.value) return
  keyPending.value = true
  keyError.value = ''
  createdKey.value = null
  const request = {
    label: keyName.value.trim(),
  }
  const signature = JSON.stringify(request)
  if (pendingCreateKeyRequest.value?.signature !== signature) {
    pendingCreateKeyRequest.value = { signature, idempotencyKey: createRemoteIdempotencyKey() }
  }
  try {
    const key = await createDeviceAccessKey(request, pendingCreateKeyRequest.value.idempotencyKey)
    pendingCreateKeyRequest.value = null
    createdKey.value = key
    keys.value = [key, ...keys.value.filter((item) => item.id !== key.id)]
  } catch (error) {
    keyError.value = problemMessage(error, '无法创建设备 Access Key。')
  } finally {
    keyPending.value = false
  }
}

const deleteDevice = async (device: RemoteDevice) => {
  if (deletingDeviceId.value) return
  const confirmed = window.confirm(
    `删除“${device.deviceName}”后，会撤销其登录会话和已绑定的 Access Key，并清除服务器保存的远程项目与任务索引。此操作不可恢复。`,
  )
  if (!confirmed) return
  deletingDeviceId.value = device.id
  errorMessage.value = ''
  try {
    await deleteRemoteDevice(device.id)
    devices.value = devices.value.filter((item) => item.id !== device.id)
    await loadKeys()
  } catch (error) {
    errorMessage.value = problemMessage(error, '无法删除该设备。')
  } finally {
    deletingDeviceId.value = null
  }
}

const showDeviceInfo = (device: RemoteDevice) => {
  inspectedDevice.value = device
}

const beginDeviceEdit = (device: RemoteDevice) => {
  editingDevice.value = device
  editedDeviceName.value = device.deviceName
  editedDirectModeEnabled.value = device.directModeEnabled
  editError.value = ''
}

const saveDeviceName = async () => {
  const current = editingDevice.value
  const deviceName = editedDeviceName.value.trim()
  if (!current || updatingDeviceId.value) return
  if (!deviceName) {
    editError.value = '请输入设备名称。'
    return
  }
  if (Array.from(deviceName).length > 120) {
    editError.value = '设备名称不能超过 120 个字符。'
    return
  }
  if (editedDirectModeEnabled.value && !current.directAvailable && !current.directModeEnabled) {
    editError.value = 'Device Agent 尚未上报可用的直连 IP 和端口。'
    return
  }
  if (
    deviceName === current.deviceName &&
    editedDirectModeEnabled.value === current.directModeEnabled
  ) {
    editingDevice.value = null
    return
  }
  updatingDeviceId.value = current.id
  const modeChanged = editedDirectModeEnabled.value !== current.directModeEnabled
  editError.value = ''
  try {
    const updated = modeChanged
      ? await updateRemoteDevice(current.id, deviceName, editedDirectModeEnabled.value)
      : await updateRemoteDevice(current.id, deviceName)
    devices.value = devices.value.map((device) => (device.id === updated.id ? updated : device))
    if (inspectedDevice.value?.id === updated.id) inspectedDevice.value = updated
    editingDevice.value = null
    deviceActionMessage.value = modeChanged
      ? updated.directModeEnabled
        ? '设备已切换为 IP 直连模式。'
        : '设备已切换为 Relay 中转模式。'
      : '设备名称已修改。'
  } catch (error) {
    editError.value = problemMessage(error, '无法修改设备，请稍后重试。')
  } finally {
    updatingDeviceId.value = null
  }
}

const closeDeviceMenu = (event: Event) => {
  const menu = (event.currentTarget as HTMLElement).closest('details')
  menu?.removeAttribute('open')
}

const showDeviceInfoFromMenu = (device: RemoteDevice, event: Event) => {
  closeDeviceMenu(event)
  showDeviceInfo(device)
}

const beginDeviceEditFromMenu = (device: RemoteDevice, event: Event) => {
  closeDeviceMenu(event)
  beginDeviceEdit(device)
}

const deleteDeviceFromMenu = (device: RemoteDevice, event: Event) => {
  closeDeviceMenu(event)
  void deleteDevice(device)
}

const revokeKey = async (key: DeviceAccessKey) => {
  if (!window.confirm(`吊销 ${key.label} 的设备 Access Key？已连接设备需要重新配置。`)) return
  try {
    await revokeDeviceAccessKey(key.id)
    keys.value = keys.value.map((item) =>
      item.id === key.id ? { ...item, status: 'revoked' } : item,
    )
  } catch (error) {
    keyError.value = problemMessage(error, '无法吊销该 Access Key。')
  }
}

const deleteKey = async (key: DeviceAccessKey) => {
  if (key.status !== 'revoked' || deletingKeyId.value) return
  if (!window.confirm(`永久删除 ${key.label} 的设备 Access Key 记录？此操作不可恢复。`)) return
  deletingKeyId.value = key.id
  keyError.value = ''
  try {
    await deleteDeviceAccessKey(key.id)
    keys.value = keys.value.filter((item) => item.id !== key.id)
    if (createdKey.value?.id === key.id) createdKey.value = null
  } catch (error) {
    keyError.value = problemMessage(error, '无法删除该 Access Key。')
  } finally {
    deletingKeyId.value = null
  }
}

const rotateKey = async (key: DeviceAccessKey) => {
  if (!window.confirm(`轮换 ${key.label} 的设备 Access Key？旧 Key 会立即失效。`)) return
  keyError.value = ''
  const idempotencyKey = pendingRotationKeys.get(key.id) ?? createRemoteIdempotencyKey()
  pendingRotationKeys.set(key.id, idempotencyKey)
  try {
    const replacement = await rotateDeviceAccessKey(key.id, idempotencyKey)
    pendingRotationKeys.delete(key.id)
    createdKey.value = replacement
    keys.value = [
      replacement,
      ...keys.value.map((item) =>
        item.id === key.id ? { ...item, status: 'revoked' as const } : item,
      ),
    ]
  } catch (error) {
    keyError.value = problemMessage(error, '无法轮换该 Access Key。')
  }
}

const copyKey = async () => {
  if (!createdKey.value?.key) return
  await navigator.clipboard.writeText(createdKey.value.key)
}

const downloadDeviceInstaller = () => {
  const key = createdKey.value?.key
  const asset = deviceInstallerAsset.value
  if (!key || !asset) {
    installerMessage.value = '当前正式版没有所选平台与架构的 Device Agent 部署包。'
    return
  }
  const assetURL = new URL(asset.downloadUrl, window.location.origin).toString()
  const script = createDeviceInstaller(
    asset,
    installTarget.value,
    assetURL,
    window.location.origin,
    key,
  )
  downloadInstallerFile(
    script,
    installerFileName('device-agent', installTarget.value, latestWebRelease.value!.version),
  )
  installerMessage.value = '一键安装脚本已生成；请在远程设备上以管理员权限运行。'
}

const downloadDeviceEnvironment = () => {
  const key = createdKey.value?.key
  if (!key) return
  downloadInstallerFile(
    createDeviceEnvironmentFile(window.location.origin, key),
    deviceEnvironmentFileName(),
  )
  installerMessage.value = 'Device Agent .env 已生成；请仅保存到可信设备。'
}

onMounted(() => {
  void Promise.all([
    load(),
    loadKeys(),
    getLatestRelease('web')
      .then((release) => (latestWebRelease.value = release))
      .catch(() => undefined),
  ])
})
</script>

<template>
  <section class="dashboard-page remote-devices-page">
    <div class="remote-page-heading">
      <div>
        <h1>设备</h1>
        <p class="dashboard-lead">集中查看、授权并进入你的远程设备。</p>
      </div>
    </div>

    <div class="remote-summary" aria-label="设备摘要">
      <article>
        <span>设备总数</span><strong>{{ devices.length }}</strong>
      </article>
      <article>
        <span>当前在线</span><strong>{{ onlineCount }}</strong>
      </article>
      <article>
        <span>最近同步</span><strong>{{ formatDate(observedAt) }}</strong>
      </article>
    </div>

    <div class="device-toolbar">
      <label class="device-search">
        <svg viewBox="0 0 24 24" aria-hidden="true">
          <circle cx="11" cy="11" r="7" />
          <path d="m20 20-4-4" />
        </svg>
        <span class="sr-only">搜索设备</span>
        <input
          v-model.trim="searchQuery"
          type="search"
          autocomplete="off"
          placeholder="搜索设备名称、平台、Agent 版本…"
        />
      </label>
      <span v-if="searchQuery" class="device-search-count">
        找到 {{ filteredDevices.length }} 台已加载设备
      </span>
      <button class="button device-add-button" type="button" @click="keyPanelOpen = !keyPanelOpen">
        <span aria-hidden="true">{{ keyPanelOpen ? '⌃' : '+' }}</span>
        {{ keyPanelOpen ? '收起接入设置' : '添加设备' }}
      </button>
    </div>

    <section v-if="keyPanelOpen" class="dashboard-card access-key-panel">
      <div class="panel-heading">
        <div>
          <h2>设备接入 Access Key</h2>
          <p>
            Key 仅用于设备向管理端交换短期凭证，不会发送给
            Relay；明文仅在创建、轮换或原请求安全重试时返回。Free 临时开放期间普通会员也可使用，
            设备数量上限以当前套餐配置为准。
          </p>
        </div>
      </div>
      <p v-if="keyError" class="form-message form-error" role="alert">{{ keyError }}</p>
      <form class="key-create-row" @submit.prevent="createKey">
        <label>
          <span>用途名称</span>
          <input v-model.trim="keyName" maxlength="120" autocomplete="off" />
        </label>
        <button class="button" type="submit" :disabled="keyPending || !keyName.trim()">
          {{ keyPending ? '生成中…' : '生成 Access Key' }}
        </button>
      </form>
      <p class="device-permission-note">
        新设备接入后会自动开启远程控制，并获得 Device Agent 的完整 RPC、Event、Stream、文件、终端与
        AI 权限；无需再次选择或启用。
      </p>
      <div v-if="createdKey?.key" class="one-time-key" role="status">
        <strong>请立即保存，此 Key 关闭页面后无法再次查看</strong>
        <code>{{ createdKey.key }}</code>
        <button type="button" @click="copyKey">复制 Key</button>
      </div>
      <div v-if="createdKey?.key" class="device-installer-panel">
        <div>
          <strong>远程设备一键安装</strong>
          <p>脚本会下载并校验 Device Agent 包、自动写入上方 Access Key，然后后台启动 Agent。</p>
        </div>
        <div class="device-installer-target">
          <label>
            <span>目标系统</span>
            <select v-model="installTarget.platform">
              <option value="linux">Linux</option>
              <option value="windows">Windows</option>
              <option value="macos">macOS</option>
            </select>
          </label>
          <label>
            <span>处理器架构</span>
            <select v-model="installTarget.architecture">
              <option value="x64">x64 / AMD64</option>
              <option value="arm64">ARM64</option>
            </select>
          </label>
          <button
            class="button"
            type="button"
            :disabled="!deviceInstallerAsset"
            @click="downloadDeviceInstaller"
          >
            下载一键安装脚本
          </button>
          <button class="button button-secondary" type="button" @click="downloadDeviceEnvironment">
            下载 .env 配置
          </button>
        </div>
        <p v-if="!deviceInstallerAsset" class="form-message form-error" role="alert">
          当前 Web 正式版尚未发布所选目标的 Device Agent 部署包。
        </p>
        <p v-if="installerMessage" class="form-message form-success" role="status">
          {{ installerMessage }}
        </p>
        <small
          >脚本与 .env 均含一次性 Access Key，请只在可信设备保存；GitHub Token 对公开 Release
          可留空。</small
        >
      </div>
      <ul v-if="keys.length" class="key-list">
        <li v-for="key in keys" :key="key.id">
          <div>
            <strong>{{ key.label }}</strong>
            <span>{{ key.keyPrefix }}… · 最近使用 {{ formatDate(key.lastUsedAt) }}</span>
          </div>
          <span class="key-status" :class="key.status">{{ key.status }}</span>
          <div v-if="key.status === 'active'" class="key-actions">
            <button type="button" @click="rotateKey(key)">轮换</button>
            <button type="button" @click="revokeKey(key)">吊销</button>
          </div>
          <div v-else-if="key.status === 'revoked'" class="key-actions">
            <button
              class="key-delete"
              type="button"
              :disabled="deletingKeyId === key.id"
              @click="deleteKey(key)"
            >
              {{ deletingKeyId === key.id ? '删除中…' : '删除' }}
            </button>
          </div>
        </li>
      </ul>
    </section>

    <p v-if="errorMessage" class="form-message form-error" role="alert">{{ errorMessage }}</p>
    <p v-if="deviceActionMessage" class="form-message form-success" role="status">
      {{ deviceActionMessage }}
    </p>
    <div v-if="loading" class="dashboard-card remote-empty">正在读取设备…</div>
    <div v-else-if="devices.length === 0" class="dashboard-card remote-empty">
      <strong>还没有远程设备</strong>
      <p>生成设备 Access Key，在目标设备启动 WenzWork Agent 后，它会出现在这里。</p>
    </div>
    <div v-else-if="filteredDevices.length === 0" class="dashboard-card remote-empty">
      <strong>没有匹配的设备</strong>
      <p>请尝试设备名称、平台或 Agent 版本中的其他关键词。</p>
    </div>
    <div v-else class="device-grid">
      <article v-for="device in filteredDevices" :key="device.id" class="device-card">
        <div class="device-card-top">
          <span class="device-platform-icon" aria-hidden="true">
            {{ platformLabel(device.platform).slice(0, 1) }}
          </span>
          <span class="presence" :class="[device.presence, device.status]">
            <i aria-hidden="true"></i>{{ presenceLabel(device) }}
          </span>
          <details class="device-card-menu">
            <summary aria-label="设备菜单">•••</summary>
            <div>
              <button type="button" @click="showDeviceInfoFromMenu(device, $event)">
                查看设备信息
              </button>
              <button
                type="button"
                :disabled="updatingDeviceId === device.id"
                @click="beginDeviceEditFromMenu(device, $event)"
              >
                修改设备
              </button>
              <button
                class="danger"
                type="button"
                :disabled="deletingDeviceId === device.id"
                @click="deleteDeviceFromMenu(device, $event)"
              >
                {{ deletingDeviceId === device.id ? '删除中…' : '删除设备' }}
              </button>
            </div>
          </details>
        </div>
        <a
          class="device-card-link"
          :href="deviceWorkspaceHref(device.id)"
          :target="REMOTE_MANAGER_WINDOW_NAME"
          :aria-label="`在独立窗口打开 ${device.deviceName}`"
          @click="openDeviceWorkspace(device.id, $event)"
        >
          <h2>{{ device.deviceName }}</h2>
          <p>{{ platformLabel(device.platform) }} · Agent {{ device.agentVersion }}</p>
          <dl>
            <div>
              <dt>最近在线</dt>
              <dd>{{ formatDate(device.lastSeenAt) }}</dd>
            </div>
            <div>
              <dt>连接方式</dt>
              <dd>{{ connectionModeLabel(device) }}</dd>
            </div>
          </dl>
          <span class="device-open">打开远程工作台 →</span>
        </a>
      </article>
    </div>

    <button
      v-if="nextCursor"
      class="load-more"
      type="button"
      :disabled="loadingMore"
      @click="load(true)"
    >
      {{ loadingMore ? '读取中…' : '加载更多设备' }}
    </button>

    <div
      v-if="inspectedDevice"
      class="device-dialog-backdrop"
      role="presentation"
      @click.self="inspectedDevice = null"
    >
      <section
        class="device-dialog"
        role="dialog"
        aria-modal="true"
        aria-labelledby="device-info-title"
        @keydown.esc="inspectedDevice = null"
      >
        <div class="device-dialog-heading">
          <div>
            <p class="section-kicker">设备信息</p>
            <h2 id="device-info-title">{{ inspectedDevice.deviceName }}</h2>
          </div>
          <span class="presence" :class="[inspectedDevice.presence, inspectedDevice.status]">
            <i aria-hidden="true"></i>{{ presenceLabel(inspectedDevice) }}
          </span>
        </div>
        <dl class="device-info-list">
          <div>
            <dt>设备 ID</dt>
            <dd>
              <code>{{ inspectedDevice.id }}</code>
            </dd>
          </div>
          <div>
            <dt>安装 ID</dt>
            <dd>
              <code>{{ inspectedDevice.installationDeviceId }}</code>
            </dd>
          </div>
          <div>
            <dt>平台</dt>
            <dd>{{ platformLabel(inspectedDevice.platform) }}</dd>
          </div>
          <div>
            <dt>Agent 版本</dt>
            <dd>{{ inspectedDevice.agentVersion }}</dd>
          </div>
          <div>
            <dt>状态</dt>
            <dd>{{ presenceLabel(inspectedDevice) }} · {{ inspectedDevice.status }}</dd>
          </div>
          <div>
            <dt>最近在线</dt>
            <dd>{{ formatDate(inspectedDevice.lastSeenAt) }}</dd>
          </div>
          <div>
            <dt>最近同步</dt>
            <dd>{{ formatDate(inspectedDevice.lastSyncAt) }}</dd>
          </div>
          <div>
            <dt>远程访问</dt>
            <dd>{{ inspectedDevice.remoteEnabledAt ? '已启用' : '未启用' }}</dd>
          </div>
          <div>
            <dt>连接方式</dt>
            <dd>{{ connectionModeLabel(inspectedDevice) }}</dd>
          </div>
          <div>
            <dt>直连端点</dt>
            <dd>
              <code>{{ directEndpointLabel(inspectedDevice) }}</code>
            </dd>
          </div>
        </dl>
        <div v-if="inspectedDevice.capabilities.length" class="device-capabilities">
          <strong>设备声明的能力</strong>
          <span v-for="capability in inspectedDevice.capabilities" :key="capability">
            {{ capability }}
          </span>
        </div>
        <div class="device-dialog-actions">
          <button class="button button-secondary" type="button" @click="inspectedDevice = null">
            关闭
          </button>
          <a
            class="button"
            :href="deviceWorkspaceHref(inspectedDevice.id)"
            :target="REMOTE_MANAGER_WINDOW_NAME"
            @click="openInspectedWorkspace"
          >
            打开工作台
          </a>
        </div>
      </section>
    </div>

    <div
      v-if="editingDevice"
      class="device-dialog-backdrop"
      role="presentation"
      @click.self="editingDevice = null"
    >
      <form
        class="device-dialog device-edit-dialog"
        role="dialog"
        aria-modal="true"
        aria-labelledby="device-edit-title"
        @submit.prevent="saveDeviceName"
        @keydown.esc="editingDevice = null"
      >
        <div>
          <p class="section-kicker">修改设备</p>
          <h2 id="device-edit-title">设备名称与连接方式</h2>
        </div>
        <label>
          <span>设备名称</span>
          <input v-model="editedDeviceName" maxlength="120" autocomplete="off" autofocus />
        </label>
        <section class="device-connection-setting" aria-labelledby="device-connection-title">
          <div>
            <strong id="device-connection-title">IP 直连模式</strong>
            <p>
              开启后，工作台通过 Agent 配置的 IP 和端口建立 Carrier；文件、终端、任务与 AI
              仍使用同一条端到端加密 Link。
            </p>
          </div>
          <label class="device-direct-toggle">
            <input
              v-model="editedDirectModeEnabled"
              type="checkbox"
              :disabled="!editingDevice.directAvailable && !editingDevice.directModeEnabled"
            />
            <span>{{ editedDirectModeEnabled ? '已开启' : '使用 Relay' }}</span>
          </label>
          <dl>
            <div>
              <dt>Agent 直连端点</dt>
              <dd>
                <code>{{ directEndpointLabel(editingDevice) }}</code>
              </dd>
            </div>
            <div>
              <dt>监听状态</dt>
              <dd>{{ editingDevice.directAvailable ? '心跳在线' : '不可用' }}</dd>
            </div>
          </dl>
          <p v-if="!editingDevice.directAvailable" class="device-direct-hint">
            请在 Device Agent 的 .env 中设置
            <code>WENZWORK_DEVICE_DIRECT_ENABLED=true</code>、
            <code>WENZWORK_DEVICE_DIRECT_IP</code> 和 <code>WENZWORK_DEVICE_DIRECT_PORT</code>，重启
            Agent 后再开启。
          </p>
          <p
            v-else-if="editedDirectModeEnabled && securePage && !editingDevice.directTlsEnabled"
            class="device-direct-hint warning"
          >
            浏览器会阻止 HTTPS 页面访问普通 ws:// IP。请使用 HTTP 管理页，或在 Agent 配置受信任的 IP
            证书以启用 WSS。
          </p>
        </section>
        <p v-if="editError" class="form-message form-error" role="alert">{{ editError }}</p>
        <div class="device-dialog-actions">
          <button class="button button-secondary" type="button" @click="editingDevice = null">
            取消
          </button>
          <button class="button" type="submit" :disabled="updatingDeviceId === editingDevice.id">
            {{ updatingDeviceId === editingDevice.id ? '保存中…' : '保存' }}
          </button>
        </div>
      </form>
    </div>
  </section>
</template>

<style scoped>
.remote-devices-page {
  margin-inline: auto;
}
.remote-page-heading {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 24px;
}
.remote-page-heading .dashboard-lead {
  margin-bottom: 24px;
}
.remote-summary {
  display: grid;
  grid-template-columns: repeat(3, minmax(0, 1fr));
  gap: 14px;
  margin-bottom: 22px;
}
.remote-summary article {
  display: grid;
  gap: 6px;
  border: 1px solid var(--line);
  border-radius: 14px;
  padding: 18px 20px;
  background: #fff;
}
.remote-summary span {
  color: var(--ink-soft);
  font-size: 0.82rem;
}
.remote-summary strong {
  font-size: 1.35rem;
}
.device-toolbar {
  display: flex;
  align-items: center;
  gap: 14px;
  margin-bottom: 22px;
}
.device-search {
  flex: 1;
  position: relative;
  display: flex;
  align-items: center;
}
.device-search svg {
  position: absolute;
  left: 14px;
  width: 18px;
  fill: none;
  stroke: var(--ink-faint);
  stroke-linecap: round;
  stroke-width: 1.8;
  pointer-events: none;
}
.device-search input {
  width: 100%;
  min-height: 48px;
  border: 1px solid var(--line-strong);
  border-radius: 12px;
  padding: 0 14px 0 43px;
  background: #fff;
}
.device-search input:focus {
  border-color: var(--teal);
  outline: 3px solid var(--mint);
}
.device-search-count {
  flex-shrink: 0;
  color: var(--ink-soft);
  font-size: 0.8rem;
}
.device-add-button {
  min-height: 44px;
  flex-shrink: 0;
}
.device-add-button span {
  font-size: 1.05rem;
}
.access-key-panel {
  margin-bottom: 22px;
}
.access-key-panel h2 {
  margin: 0 0 6px;
}
.access-key-panel p {
  margin: 0;
  color: var(--ink-soft);
}
.key-create-row {
  display: grid;
  grid-template-columns: minmax(0, 1fr) auto;
  align-items: end;
  gap: 14px;
  margin-top: 20px;
}
.key-create-row label {
  display: grid;
  gap: 7px;
  font-weight: 650;
}
.key-create-row input {
  min-height: 46px;
  padding: 0 13px;
}
.device-permission-note {
  margin-top: 14px;
  color: var(--ink-soft);
  line-height: 1.65;
  border-radius: 10px;
  padding: 10px 12px;
  background: var(--paper-soft);
}
.one-time-key {
  display: grid;
  gap: 10px;
  margin-top: 18px;
  border: 1px solid #e8a13c;
  border-radius: 12px;
  padding: 16px;
  background: #fff8e8;
}
.one-time-key code {
  overflow-wrap: anywhere;
}
.one-time-key button,
.key-list button {
  justify-self: start;
  border: 0;
  padding: 0;
  color: var(--teal-dark);
  background: none;
  font-weight: 700;
  cursor: pointer;
}
.device-installer-panel {
  display: grid;
  gap: 14px;
  margin-top: 18px;
  border: 1px solid var(--line);
  border-radius: 12px;
  padding: 16px;
  background: var(--paper-soft);
}
.device-installer-panel small {
  color: var(--ink-soft);
}
.device-installer-target {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr)) auto;
  align-items: end;
  gap: 12px;
}
.device-installer-target label {
  display: grid;
  gap: 6px;
}
.device-installer-target select {
  min-height: 44px;
}
.key-list {
  display: grid;
  gap: 8px;
  margin: 20px 0 0;
  padding: 0;
  list-style: none;
}
.key-list li {
  display: grid;
  grid-template-columns: minmax(0, 1fr) auto auto;
  align-items: center;
  gap: 14px;
  border-top: 1px solid var(--line);
  padding-top: 12px;
}
.key-list li > div {
  display: grid;
  gap: 3px;
}
.key-list li > .key-actions {
  display: flex;
  gap: 10px;
}
.key-list .key-delete {
  color: #9a3329;
}
.key-list button:disabled {
  cursor: wait;
  opacity: 0.65;
}
.key-list span {
  color: var(--ink-soft);
  font-size: 0.82rem;
}
.key-status {
  border-radius: 999px;
  padding: 4px 9px;
  background: var(--paper-soft);
}
.key-status.active {
  color: var(--teal-dark);
  background: var(--brand-tint);
}
.device-grid {
  display: grid;
  grid-template-columns: repeat(3, minmax(0, 1fr));
  gap: 18px;
}
.device-card {
  position: relative;
  border: 1px solid var(--line);
  border-radius: var(--radius-card);
  padding: 18px;
  background: #fff;
  box-shadow: var(--shadow-small);
  text-decoration: none;
  transition:
    transform 160ms ease,
    border-color 160ms ease,
    box-shadow 160ms ease;
}
.device-card-link {
  display: block;
  color: inherit;
  text-decoration: none;
}
.device-card:hover {
  transform: translateY(-3px);
  border-color: var(--mint);
  box-shadow: var(--shadow-medium);
}
.device-card-top {
  display: flex;
  align-items: center;
  gap: 8px;
}
.device-platform-icon {
  display: inline-grid;
  width: 42px;
  height: 42px;
  place-items: center;
  border-radius: 11px;
  color: var(--teal-dark);
  background: var(--brand-tint);
  font-size: 0.9rem;
  font-weight: 800;
}
.presence {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  border-radius: 999px;
  padding: 5px 9px;
  color: var(--ink-soft);
  background: var(--paper-soft);
  font-size: 0.76rem;
  font-weight: 700;
}
.device-card-top > .presence {
  margin-left: auto;
}
.presence i {
  width: 7px;
  height: 7px;
  border-radius: 50%;
  background: var(--ink-faint);
}
.presence.online.active {
  color: var(--teal-dark);
  background: var(--brand-tint);
}
.presence.online.active i {
  background: var(--teal);
  box-shadow: 0 0 0 3px var(--mint);
}
.presence.degraded i {
  background: var(--amber);
}
.device-card-menu {
  position: relative;
}
.device-card-menu summary {
  display: grid;
  width: 34px;
  height: 34px;
  place-items: center;
  border-radius: 9px;
  color: var(--ink-soft);
  cursor: pointer;
  list-style: none;
  letter-spacing: 0.08em;
}
.device-card-menu summary::-webkit-details-marker {
  display: none;
}
.device-card-menu summary:hover,
.device-card-menu[open] summary {
  color: var(--ink);
  background: var(--paper-soft);
}
.device-card-menu > div {
  position: absolute;
  z-index: 12;
  top: calc(100% + 5px);
  right: 0;
  display: grid;
  min-width: 168px;
  overflow: hidden;
  border: 1px solid var(--line);
  border-radius: 10px;
  padding: 5px;
  background: #fff;
  box-shadow: var(--shadow-medium);
}
.device-card-menu button {
  border: 0;
  border-radius: 7px;
  padding: 9px 10px;
  color: var(--ink);
  background: none;
  font: inherit;
  font-size: 0.82rem;
  font-weight: 650;
  text-align: left;
  cursor: pointer;
}
.device-card-menu button:hover {
  background: var(--paper-soft);
}
.device-card-menu button.danger {
  color: #9a3329;
}
.device-card-menu button:disabled {
  cursor: wait;
  opacity: 0.55;
}
.device-card h2 {
  margin: 14px 0 5px;
  font-size: 1.05rem;
}
.device-card-link > p {
  overflow: hidden;
  margin: 0 0 14px;
  color: var(--ink-soft);
  font-size: 0.86rem;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.device-delete {
  margin-top: 16px;
  border: 1px solid #e9b9b4;
  border-radius: 9px;
  padding: 8px 11px;
  color: #9a3329;
  background: #fff7f6;
  font: inherit;
  font-size: 0.84rem;
  font-weight: 700;
  cursor: pointer;
}
.device-delete:disabled {
  cursor: wait;
  opacity: 0.65;
}
.device-card dl {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 14px;
  margin: 0 0 16px;
  border-top: 1px solid var(--line);
  padding-top: 12px;
}
.device-card dl div {
  display: grid;
  gap: 4px;
}
.device-card dt {
  color: var(--ink-faint);
  font-size: 0.82rem;
}
.device-card dd {
  margin: 0;
  font-size: 0.82rem;
  font-weight: 650;
}
.device-open {
  color: var(--teal-dark);
  font-size: 0.86rem;
  font-weight: 750;
}
.remote-empty {
  text-align: center;
}
.remote-empty strong {
  display: block;
  margin-bottom: 5px;
}
.remote-empty p {
  margin: 0;
  color: var(--ink-soft);
}
.load-more {
  display: block;
  margin: 22px auto 0;
  border: 1px solid var(--line-strong);
  border-radius: 10px;
  padding: 10px 18px;
  color: var(--ink);
  background: #fff;
  cursor: pointer;
}
.device-dialog-backdrop {
  position: fixed;
  z-index: 80;
  inset: 0;
  display: grid;
  overflow: auto;
  place-items: center;
  padding: 24px;
  background: rgba(18, 35, 31, 0.45);
  backdrop-filter: blur(5px);
}
.device-dialog {
  width: min(560px, 100%);
  border: 1px solid var(--line);
  border-radius: 18px;
  padding: 26px;
  background: #fff;
  box-shadow: 0 24px 70px rgba(15, 34, 29, 0.24);
}
.device-dialog-heading {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 18px;
}
.device-dialog .section-kicker {
  margin: 0 0 5px;
}
.device-dialog h2 {
  margin: 0;
  font-size: 1.35rem;
}
.device-info-list {
  display: grid;
  margin: 20px 0 0;
}
.device-info-list > div {
  display: grid;
  grid-template-columns: 110px minmax(0, 1fr);
  gap: 18px;
  border-top: 1px solid var(--line);
  padding: 10px 0;
}
.device-info-list dt {
  color: var(--ink-soft);
  font-size: 0.82rem;
}
.device-info-list dd {
  min-width: 0;
  margin: 0;
  font-size: 0.84rem;
  text-align: right;
}
.device-info-list code {
  overflow-wrap: anywhere;
}
.device-capabilities {
  display: flex;
  flex-wrap: wrap;
  gap: 7px;
  margin-top: 16px;
}
.device-capabilities strong {
  width: 100%;
  color: var(--ink-soft);
  font-size: 0.8rem;
}
.device-capabilities span {
  border-radius: 999px;
  padding: 5px 8px;
  background: var(--paper-soft);
  font-size: 0.74rem;
}
.device-dialog-actions {
  display: flex;
  justify-content: flex-end;
  gap: 10px;
  margin-top: 22px;
}
.device-edit-dialog {
  display: grid;
  gap: 20px;
}
.device-edit-dialog > label {
  display: grid;
  gap: 7px;
  font-weight: 680;
}
.device-edit-dialog input {
  min-height: 46px;
  padding: 0 13px;
}
.device-connection-setting {
  display: grid;
  gap: 14px;
  border: 1px solid var(--line);
  border-radius: 12px;
  padding: 16px;
  background: var(--paper-soft);
}
.device-connection-setting strong {
  display: block;
  margin-bottom: 5px;
}
.device-connection-setting p {
  margin: 0;
  color: var(--ink-soft);
  font-size: 0.86rem;
  line-height: 1.6;
}
.device-direct-toggle {
  display: flex;
  align-items: center;
  gap: 9px;
  font-weight: 700;
}
.device-edit-dialog .device-direct-toggle input {
  width: 18px;
  min-height: 18px;
  margin: 0;
  padding: 0;
  accent-color: var(--teal);
}
.device-connection-setting dl {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 12px;
  margin: 0;
}
.device-connection-setting dl > div {
  display: grid;
  gap: 4px;
}
.device-connection-setting dt {
  color: var(--ink-faint);
  font-size: 0.78rem;
}
.device-connection-setting dd {
  overflow-wrap: anywhere;
  margin: 0;
  font-size: 0.84rem;
}
.device-direct-hint code {
  color: var(--ink);
}
.device-connection-setting .device-direct-hint.warning {
  color: #8a5a13;
}
.device-edit-dialog .form-message,
.device-edit-dialog .device-dialog-actions {
  margin-top: 0;
}
@media (max-width: 960px) {
  .device-grid {
    grid-template-columns: repeat(2, minmax(0, 1fr));
  }
}
@media (max-width: 640px) {
  .remote-page-heading {
    display: grid;
  }
  .remote-summary,
  .device-grid {
    grid-template-columns: 1fr;
  }
  .device-toolbar {
    align-items: stretch;
    flex-direction: column;
  }
  .device-add-button {
    align-self: flex-end;
  }
  .key-create-row {
    grid-template-columns: 1fr;
  }
  .device-installer-target {
    grid-template-columns: 1fr;
  }
  .key-list li {
    grid-template-columns: minmax(0, 1fr) auto;
  }
  .key-list button {
    grid-column: 1 / -1;
  }
  .device-dialog-backdrop {
    align-items: end;
    padding: 12px;
  }
  .device-dialog {
    max-height: calc(100vh - 24px);
    overflow: auto;
  }
}
</style>
