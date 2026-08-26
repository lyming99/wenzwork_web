<script setup lang="ts">
import { useHead } from '@unhead/vue'
import { computed, nextTick, onBeforeUnmount, onMounted, reactive, ref } from 'vue'

import {
  activateRelayInstallation,
  createRelayAccessKey,
  createRelayInstallSession,
  createRelayInstallation,
  createRelayServerRelease,
  deleteRelayInstallation,
  deleteRelayServerRelease,
  drainRelayNode,
  getRelayOperation,
  getRelayInstallation,
  listRelayInstallations,
  listRelayServerReleases,
  listRelayTopology,
  publishRelayServerRelease,
  retireRelayServerRelease,
  revokeRelayInstallation,
  updateRelayCell,
  updateRelayInstallation,
  updateRelayServerRelease,
  type RelayCell,
  type RelayDeploymentChecklist,
  type RelayAccessKey,
  type RelayInstallation,
  type RelayOperation,
  type RelayServerRelease,
  type RelayTopology,
  type SaveRelayServerReleaseRequest,
} from '@/api/adminRelay'
import { problemMessage } from '@/api/auth'
import {
  createRelayBootstrapInstaller,
  downloadInstallerFile,
  installerFileName,
  type PortableTarget,
} from '@/utils/portableInstaller'

useHead({
  title: '中继主机｜WenzWork',
  meta: [{ name: 'robots', content: 'noindex, nofollow' }],
})

type HostState =
  'online' | 'pending' | 'pending_activation' | 'offline' | 'draining' | 'paused' | 'revoked'

type RelayPlatform = 'linux' | 'windows' | 'darwin'
type RelayArchitecture = 'amd64' | 'arm64'

type ReleaseEditor = {
  version: string
  platform: RelayPlatform
  architecture: RelayArchitecture
  protocolMin: number
  protocolMax: number
  buildCommit: string
  buildTime: string
  signingKeyId: string
  manifestSha256: string
  manifestSignature: string
  fileName: string
  fileSizeBytes: number
  sha256: string
  signature: string
  objectKey: string
}

const emptyReleaseEditor = (): ReleaseEditor => ({
  version: '',
  platform: 'linux',
  architecture: 'amd64',
  protocolMin: 2,
  protocolMax: 2,
  buildCommit: '',
  buildTime: new Date().toISOString(),
  signingKeyId: '',
  manifestSha256: '',
  manifestSignature: '',
  fileName: '',
  fileSizeBytes: 0,
  sha256: '',
  signature: '',
  objectKey: '',
})

const loading = ref(true)
const refreshing = ref(false)
const actionPending = ref(false)
const pageError = ref('')
const forbidden = ref(false)
const toast = ref('')
const installations = ref<RelayInstallation[]>([])
const releases = ref<RelayServerRelease[]>([])
const topology = ref<RelayTopology | null>(null)
const topologyLoading = ref(true)
const topologyError = ref('')
const cellActionPending = ref('')
const selected = ref<RelayInstallation | null>(null)
const detailLoading = ref(false)
const detailOpen = ref(false)
const addOpen = ref(false)
const addStep = ref<'form' | 'accessKey'>('form')
const hostName = ref('')
const hostRegion = ref('')
const hostGroup = ref('')
const hostPlatform = ref<RelayPlatform>('linux')
const hostArchitecture = ref<RelayArchitecture>('amd64')
const relayPublicEndpoint = ref('')
const relayListenerPort = ref('8443')
const createdHost = ref<RelayInstallation | null>(null)
const accessKey = ref<RelayAccessKey | null>(null)
const selectedReleaseId = ref('')
const installCommand = ref('')
const upgradeReleaseId = ref('')
const upgradeCommand = ref('')
const detailPublicEndpoint = ref('')
const detailListenerPort = ref('8443')
const releaseEditorOpen = ref(false)
const editingReleaseId = ref<string | null>(null)
const releaseEditor = reactive<ReleaseEditor>(emptyReleaseEditor())
const fingerprintConfirmed = ref(false)
const checklist = reactive<RelayDeploymentChecklist>({
  lb: false,
  dns: false,
  port: false,
  tls: false,
})
const nowMs = ref(Date.now())
const hostNameInput = ref<HTMLInputElement | null>(null)

let refreshTimer: ReturnType<typeof setInterval> | undefined
let clockTimer: ReturnType<typeof setInterval> | undefined
let toastTimer: ReturnType<typeof setTimeout> | undefined

const isValidPublicEndpoint = (value: string) => {
  try {
    const endpoint = new URL(value.trim())
    return (
      (endpoint.protocol === 'ws:' || endpoint.protocol === 'wss:') &&
      Boolean(endpoint.hostname) &&
      endpoint.pathname === '/v2/connect' &&
      endpoint.username === '' &&
      endpoint.password === '' &&
      endpoint.search === '' &&
      endpoint.hash === ''
    )
  } catch {
    return false
  }
}

const normalizedListenerPort = (value: string) => {
  const normalized = value.trim()
  if (!/^\d{1,5}$/.test(normalized)) return null
  const port = Number(normalized)
  return Number.isInteger(port) && port >= 1 && port <= 65_535 ? port : null
}

const relayEndpointHasInput = computed(() => Boolean(relayPublicEndpoint.value.trim()))
const relayListenerPortValue = computed(() => normalizedListenerPort(relayListenerPort.value))
const detailListenerPortValue = computed(() => normalizedListenerPort(detailListenerPort.value))

const resetRelayConfigurationFields = () => {
  relayPublicEndpoint.value = ''
  relayListenerPort.value = '8443'
}

const setRelayConfigurationFields = (installation: RelayInstallation) => {
  relayPublicEndpoint.value =
    installation.publicEndpoint || reportedPublicEndpoint(installation) || ''
  relayListenerPort.value = String(installation.listenerPort)
}

const relayEndpointConfigurationValid = computed(
  () =>
    relayListenerPortValue.value !== null &&
    (!relayEndpointHasInput.value || isValidPublicEndpoint(relayPublicEndpoint.value)),
)

const relayConfigurationValid = computed(
  () => relayListenerPortValue.value !== null && isValidPublicEndpoint(relayPublicEndpoint.value),
)

const detailRelayConfigurationValid = computed(
  () => detailListenerPortValue.value !== null && isValidPublicEndpoint(detailPublicEndpoint.value),
)

/*
 * The client-facing URL is deliberately independent from the listener. A
 * wss:// URL normally terminates at Nginx on 443 while Relay keeps serving
 * plaintext WebSocket on the configured local port.
 */
const relayTransportHint = (endpoint: string, listenerPort: string) => {
  if (!endpoint.trim().toLowerCase().startsWith('wss://')) {
    return `Relay 始终以 WS 监听 ${listenerPort || '指定端口'}；当前地址可由客户端直接访问。`
  }
  return `Relay 始终以 WS 监听 ${listenerPort || '指定端口'}；请在 Nginx 配置证书与私钥，并将 WSS 转发到该端口。`
}

const reportedPublicEndpoint = (installation: RelayInstallation | null | undefined) =>
  installation?.currentInstance?.addresses.find(isValidPublicEndpoint) ?? null

const displayedPublicEndpoint = (installation: RelayInstallation | null | undefined) =>
  reportedPublicEndpoint(installation) ?? (installation?.publicEndpoint || '等待管理端配置')

const hostState = (installation: RelayInstallation): HostState => {
  if (installation.status === 'revoked') return 'revoked'
  if (installation.status === 'draining' || installation.currentInstance?.status === 'draining') {
    return 'draining'
  }
  if (installation.status === 'disabled') return 'paused'
  if (installation.status === 'draft' || installation.status === 'pending_enrollment')
    return 'pending'
  if (installation.status === 'enrolled' || installation.status === 'pending_activation') {
    return 'pending_activation'
  }
  const instance = installation.currentInstance
  if (
    installation.status === 'active' &&
    instance?.status === 'ready' &&
    new Date(instance.leaseExpiresAt).getTime() > nowMs.value
  ) {
    return 'online'
  }
  return 'offline'
}

const stateMeta: Record<HostState, { label: string; symbol: string; help: string }> = {
  online: { label: '在线', symbol: '●', help: '当前进程已连接管理端并持续上报心跳。' },
  pending: { label: '等待连接', symbol: '○', help: '请把生成的 .env 放到目标服务器并启动 Relay。' },
  pending_activation: {
    label: '等待启用',
    symbol: '◐',
    help: '主机已注册，请核对身份指纹后启用。',
  },
  offline: { label: '离线', symbol: '×', help: '心跳租约已过期，请检查服务与网络。' },
  draining: { label: '排空中', symbol: '◒', help: '已停止新连接，正在等待现有连接退出。' },
  paused: { label: '暂停接入', symbol: 'Ⅱ', help: '当前不接收新的客户端连接。' },
  revoked: { label: '已吊销', symbol: '!', help: '主机身份已失效，不能恢复。' },
}

const stateOf = (installation: RelayInstallation) => stateMeta[hostState(installation)]
const relayPackagePattern =
  /^wenzwork-relay(?:-deployment)?-([A-Za-z0-9._+-]+)-(linux|windows|darwin)-(amd64|arm64)\.tar\.gz$/
const canonicalReleaseVersion = (value: string) => value.trim().replace(/^[vV](?=[0-9])/, '')
const releaseHasMatchingPackage = (release: RelayServerRelease) =>
  release.artifacts.some((artifact) => {
    const match = relayPackagePattern.exec(artifact.fileName)
    return (
      canonicalReleaseVersion(match?.[1] ?? '') === canonicalReleaseVersion(release.version) &&
      match?.[2] === release.platform &&
      match?.[3] === release.architecture
    )
  })
const totalConnections = computed(() =>
  installations.value.reduce(
    (total, item) => total + (item.currentInstance?.activeConnections ?? 0),
    0,
  ),
)
const onlineCount = computed(
  () => installations.value.filter((item) => hostState(item) === 'online').length,
)
const publishedReleases = computed(() =>
  releases.value.filter(
    (release) => release.status === 'published' && releaseHasMatchingPackage(release),
  ),
)
const publishedReleasesForTarget = (platform: RelayPlatform, architecture: RelayArchitecture) =>
  publishedReleases.value.filter(
    (release) => release.platform === platform && release.architecture === architecture,
  )
const hostPublishedReleases = computed(() =>
  publishedReleasesForTarget(hostPlatform.value, hostArchitecture.value),
)
const selectedHostPublishedReleases = computed(() => {
  const platform = selected.value?.platform
  const architecture = selected.value?.architecture
  return (platform === 'linux' || platform === 'windows' || platform === 'darwin') &&
    (architecture === 'amd64' || architecture === 'arm64')
    ? publishedReleasesForTarget(platform, architecture)
    : []
})
const preferredUpgradeReleaseId = (installation: RelayInstallation) => {
  const compatible = publishedReleasesForTarget(
    installation.platform as RelayPlatform,
    installation.architecture,
  )
  return (
    compatible.find((release) => release.id === installation.releaseId)?.id ||
    compatible[0]?.id ||
    ''
  )
}
const attentionCount = computed(
  () =>
    installations.value.filter((item) =>
      ['pending', 'pending_activation', 'offline', 'draining'].includes(hostState(item)),
    ).length,
)
const schedulingCells = computed(() =>
  (topology.value?.items ?? []).flatMap((region) =>
    region.pools.flatMap((pool) =>
      pool.cells.map((cell) => ({
        cell,
        regionName: region.name,
        regionCode: region.code,
        poolName: pool.name,
        poolCode: pool.code,
      })),
    ),
  ),
)
const activeCellCount = computed(
  () => schedulingCells.value.filter(({ cell }) => cell.status === 'active').length,
)
const cellStatusMeta: Record<RelayCell['status'], { label: string; help: string; tone: string }> = {
  draft: {
    label: '未启用（draft）',
    help: 'Relay 可以正常心跳，但 Host 不会向该调度组分配 Device。',
    tone: 'draft',
  },
  active: { label: '已启用', help: '在线 Relay 可以接收新的 Device 分配。', tone: 'active' },
  draining: {
    label: '排空中',
    help: '停止新分配，等待已有连接退出。',
    tone: 'draining',
  },
  disabled: { label: '已停用', help: 'Host 不会向该调度组分配连接。', tone: 'disabled' },
}
const cellEndpoint = (cell: RelayCell) => {
  const installation = installations.value.find(
    (item) =>
      item.cellId === cell.id &&
      item.status === 'active' &&
      item.currentInstance?.status === 'ready' &&
      new Date(item.currentInstance.leaseExpiresAt).getTime() > nowMs.value &&
      displayedPublicEndpoint(item) !== '等待管理端配置',
  )
  if (installation) return displayedPublicEndpoint(installation)
  return cell.activeEndpoint?.publicEndpoint || '尚无可用地址'
}

const selectedState = computed(() => (selected.value ? hostState(selected.value) : null))
const canActivate = computed(
  () =>
    selectedState.value === 'pending_activation' &&
    fingerprintConfirmed.value &&
    checklist.lb &&
    checklist.dns &&
    checklist.port &&
    checklist.tls,
)
const relayEnv = computed(() => {
  if (!accessKey.value) return ''
  return `RELAY_ACCESS_KEY=${accessKey.value.key}`
})

const canEditCreatedConfiguration = computed(() =>
  createdHost.value
    ? ['draft', 'pending_enrollment', 'expired', 'enrolled', 'pending_activation'].includes(
        createdHost.value.status,
      )
    : false,
)

const relayPlatformLabel = (platform: string) => {
  if (platform === 'windows') return 'Windows'
  if (platform === 'darwin') return 'macOS'
  return 'Linux'
}

const installerTargetLabel = computed(() =>
  createdHost.value
    ? `${relayPlatformLabel(createdHost.value.platform)} / ${createdHost.value.architecture}`
    : '',
)

const showToast = (message: string) => {
  toast.value = message
  if (toastTimer) clearTimeout(toastTimer)
  toastTimer = setTimeout(() => (toast.value = ''), 2600)
}

const responseStatus = (error: unknown) =>
  (error as { response?: { status?: number } } | undefined)?.response?.status

const loadHosts = async (silent = false) => {
  if (silent) refreshing.value = true
  else loading.value = true
  try {
    installations.value = await listRelayInstallations()
    pageError.value = ''
    forbidden.value = false
  } catch (error) {
    if (responseStatus(error) === 403) {
      forbidden.value = true
      installations.value = []
      pageError.value = '当前账号没有中继主机管理权限，请联系超级管理员。'
    } else {
      pageError.value = problemMessage(error, '无法刷新中继主机，页面保留上一次成功数据。')
    }
  } finally {
    loading.value = false
    refreshing.value = false
  }
}

const loadTopology = async (silent = false) => {
  if (!silent) topologyLoading.value = true
  try {
    topology.value = await listRelayTopology()
    topologyError.value = ''
  } catch (error) {
    topologyError.value = problemMessage(error, '无法读取中继调度状态。')
  } finally {
    topologyLoading.value = false
  }
}

const refreshPage = async (silent = false) => {
  await Promise.all([loadHosts(silent), loadTopology(silent)])
}

const loadReleases = async (silent = false) => {
  try {
    releases.value = await listRelayServerReleases()
  } catch (error) {
    if (!silent) showToast(problemMessage(error, '无法读取 Relay 程序版本。'))
  }
}

const operationFinished = (operation: RelayOperation) =>
  operation.status === 'succeeded' ||
  operation.status === 'failed' ||
  operation.status === 'cancelled'

const waitForOperation = async (initial: RelayOperation) => {
  let operation = initial
  for (let attempt = 0; attempt < 10 && !operationFinished(operation); attempt += 1) {
    await new Promise<void>((resolve) => setTimeout(resolve, 250))
    operation = await getRelayOperation(operation.id)
  }
  return operation
}

const activateSchedulingCell = async (cell: RelayCell) => {
  if (cell.status !== 'draft' || actionPending.value || cellActionPending.value) return
  actionPending.value = true
  cellActionPending.value = cell.id
  try {
    const operation = await waitForOperation(await updateRelayCell(cell.id, { status: 'active' }))
    if (operation.status === 'failed' || operation.status === 'cancelled') {
      throw new Error(operation.errorMessage || '中继调度组启用失败。')
    }
    await loadTopology(true)
    showToast(
      operation.status === 'succeeded'
        ? '中继调度已启用，在线 Relay 现在可以接收 Device 分配。'
        : '启用请求已提交，Host 正在应用调度状态。',
    )
  } catch (error) {
    showToast(problemMessage(error, '无法启用中继调度组。'))
  } finally {
    cellActionPending.value = ''
    actionPending.value = false
  }
}

const resetActivation = (installation: RelayInstallation | null) => {
  fingerprintConfirmed.value = false
  Object.assign(
    checklist,
    installation?.deploymentChecklist ?? { lb: false, dns: false, port: false, tls: false },
  )
}

const openDetail = async (installation: RelayInstallation) => {
  detailOpen.value = true
  detailLoading.value = true
  selected.value = installation
  detailPublicEndpoint.value =
    installation.publicEndpoint || reportedPublicEndpoint(installation) || ''
  detailListenerPort.value = String(installation.listenerPort)
  upgradeReleaseId.value = preferredUpgradeReleaseId(installation)
  upgradeCommand.value = ''
  resetActivation(installation)
  try {
    const detail = await getRelayInstallation(installation.id)
    selected.value = detail
    detailPublicEndpoint.value = detail.publicEndpoint || reportedPublicEndpoint(detail) || ''
    detailListenerPort.value = String(detail.listenerPort)
    upgradeReleaseId.value = preferredUpgradeReleaseId(detail)
    resetActivation(detail)
  } catch (error) {
    showToast(problemMessage(error, '无法读取主机详情。'))
  } finally {
    detailLoading.value = false
  }
}

const refreshSelected = async () => {
  if (!selected.value) return
  try {
    selected.value = await getRelayInstallation(selected.value.id)
  } catch {
    // Keep the previously loaded troubleshooting data visible.
  }
}

const openAdd = async () => {
  await loadReleases(true)
  addStep.value = 'form'
  hostName.value = ''
  hostRegion.value = ''
  hostGroup.value = ''
  hostPlatform.value = 'linux'
  hostArchitecture.value = 'amd64'
  resetRelayConfigurationFields()
  createdHost.value = null
  accessKey.value = null
  selectedReleaseId.value = hostPublishedReleases.value[0]?.id || ''
  installCommand.value = ''
  addOpen.value = true
  await nextTick()
  hostNameInput.value?.focus()
}

const closeAdd = () => {
  addOpen.value = false
  accessKey.value = null
  createdHost.value = null
  hostName.value = ''
  hostRegion.value = ''
  hostGroup.value = ''
  hostPlatform.value = 'linux'
  hostArchitecture.value = 'amd64'
  resetRelayConfigurationFields()
  selectedReleaseId.value = ''
  installCommand.value = ''
  void loadHosts(true)
}

const createHost = async () => {
  const name = hostName.value.trim()
  if (!name || actionPending.value) return
  if (!relayEndpointConfigurationValid.value) {
    showToast('请输入 1–65535 的 WS 监听端口；访问链接应为以 /v2/connect 结尾的完整 WS/WSS 地址。')
    return
  }
  actionPending.value = true
  try {
    const installation = await createRelayInstallation({
      releaseId: selectedReleaseId.value || null,
      displayName: name,
      region: hostRegion.value.trim(),
      group: hostGroup.value.trim(),
      failureDomain: '',
      operationsNote: '',
      publicEndpoint: relayPublicEndpoint.value.trim(),
      listenerPort: relayListenerPortValue.value!,
      platform: hostPlatform.value,
      architecture: hostArchitecture.value,
    })
    createdHost.value = installation
    accessKey.value = await createRelayAccessKey(installation.id)
    addStep.value = 'accessKey'
    if (selectedReleaseId.value) {
      try {
        const install = await createRelayInstallSession(
          installation.id,
          selectedReleaseId.value,
          'script',
          'install',
        )
        installCommand.value = install.installCommand
      } catch (error) {
        showToast(problemMessage(error, '主机和 Access Key 已创建，但安装命令生成失败。'))
      }
    }
    try {
      createdHost.value = await getRelayInstallation(installation.id)
    } catch {
      // The Access Key remains available even when the follow-up refresh fails.
    }
    await loadHosts(true)
  } catch (error) {
    showToast(problemMessage(error, '无法创建中继主机。'))
  } finally {
    actionPending.value = false
  }
}

const rotateAccessKey = async () => {
  if (!selected.value || actionPending.value) return
  actionPending.value = true
  try {
    const installation = selected.value
    const key = await createRelayAccessKey(installation.id)
    try {
      createdHost.value = await getRelayInstallation(installation.id)
    } catch {
      createdHost.value = installation
    }
    setRelayConfigurationFields(createdHost.value)
    accessKey.value = key
    selectedReleaseId.value =
      createdHost.value.releaseId ||
      publishedReleasesForTarget(
        createdHost.value.platform as RelayPlatform,
        createdHost.value.architecture,
      )[0]?.id ||
      ''
    installCommand.value = ''
    addStep.value = 'accessKey'
    detailOpen.value = false
    addOpen.value = true
    showToast('旧 Access Key 已吊销。')
    if (
      selectedReleaseId.value &&
      ['draft', 'pending_enrollment'].includes(createdHost.value.status)
    ) {
      try {
        const install = await createRelayInstallSession(
          installation.id,
          selectedReleaseId.value,
          'script',
          'install',
        )
        installCommand.value = install.installCommand
      } catch (error) {
        showToast(problemMessage(error, 'Access Key 已更换，但安装命令生成失败。'))
      }
    }
  } catch (error) {
    showToast(problemMessage(error, '无法重新生成 Access Key。'))
  } finally {
    actionPending.value = false
  }
}

const saveRelayConfiguration = async () => {
  const installation = createdHost.value
  const publicEndpoint = relayPublicEndpoint.value.trim()
  if (
    !installation ||
    !canEditCreatedConfiguration.value ||
    !relayConfigurationValid.value ||
    actionPending.value
  )
    return
  actionPending.value = true
  try {
    const updated = await updateRelayInstallation(installation.id, {
      displayName: installation.displayName,
      region: installation.region,
      group: installation.group,
      failureDomain: installation.failureDomain,
      operationsNote: installation.operationsNote,
      publicEndpoint,
      listenerPort: relayListenerPortValue.value!,
      deploymentChecklist: installation.deploymentChecklist,
      expectedVersion: installation.version,
    })
    createdHost.value = updated
    setRelayConfigurationFields(updated)
    showToast('Relay 运行配置已保存到管理端。')
    await loadHosts(true)
  } catch (error) {
    showToast(problemMessage(error, '无法保存 Relay 运行配置。'))
  } finally {
    actionPending.value = false
  }
}

const saveSelectedPublicEndpoint = async () => {
  const installation = selected.value
  const publicEndpoint = detailPublicEndpoint.value.trim()
  if (!installation || !detailRelayConfigurationValid.value || actionPending.value) return
  const listenerChanged = detailListenerPortValue.value !== installation.listenerPort
  actionPending.value = true
  try {
    const updated = await updateRelayInstallation(installation.id, {
      displayName: installation.displayName,
      region: installation.region,
      group: installation.group,
      failureDomain: installation.failureDomain,
      operationsNote: installation.operationsNote,
      publicEndpoint,
      listenerPort: detailListenerPortValue.value!,
      deploymentChecklist: installation.deploymentChecklist,
      expectedVersion: installation.version,
    })
    selected.value = {
      ...updated,
      currentInstance: installation.currentInstance,
      instances: installation.instances,
    }
    detailPublicEndpoint.value = updated.publicEndpoint
    detailListenerPort.value = String(updated.listenerPort)
    showToast(
      listenerChanged
        ? 'Relay 接入配置已保存；WS 监听端口会在下一次心跳后自动切换。'
        : 'Relay 客户端访问链接已保存。',
    )
    await loadHosts(true)
  } catch (error) {
    showToast(problemMessage(error, '无法保存 Relay 接入配置，请刷新版本后重试。'))
  } finally {
    actionPending.value = false
  }
}

const prepareUpgrade = async () => {
  const installation = selected.value
  if (!installation || !upgradeReleaseId.value || actionPending.value) return
  actionPending.value = true
  try {
    const result = await createRelayInstallSession(
      installation.id,
      upgradeReleaseId.value,
      'script',
      'upgrade',
    )
    upgradeCommand.value = result.installCommand
    showToast('升级命令已生成。')
  } catch (error) {
    showToast(problemMessage(error, '无法生成升级命令。'))
  } finally {
    actionPending.value = false
  }
}

const openReleaseEditor = (release?: RelayServerRelease) => {
  editingReleaseId.value = release?.id ?? null
  const artifact = release?.artifacts[0]
  Object.assign(
    releaseEditor,
    release
      ? {
          version: release.version,
          platform: release.platform,
          architecture: release.architecture,
          protocolMin: 2,
          protocolMax: 2,
          buildCommit: release.buildCommit,
          buildTime: release.buildTime,
          signingKeyId: release.signingKeyId,
          manifestSha256: release.manifestSha256,
          manifestSignature: release.manifestSignature,
          fileName: artifact?.fileName ?? '',
          fileSizeBytes: artifact?.fileSizeBytes ?? 0,
          sha256: artifact?.sha256 ?? '',
          signature: artifact?.signature ?? '',
          objectKey: artifact?.objectKey ?? '',
        }
      : emptyReleaseEditor(),
  )
  releaseEditorOpen.value = true
}

const releasePayload = (): SaveRelayServerReleaseRequest => ({
  version: releaseEditor.version.trim(),
  platform: releaseEditor.platform,
  architecture: releaseEditor.architecture,
  protocolMin: 2,
  protocolMax: 2,
  buildCommit: releaseEditor.buildCommit.trim(),
  buildTime: new Date(releaseEditor.buildTime).toISOString(),
  signingKeyId: releaseEditor.signingKeyId.trim(),
  manifestSha256: releaseEditor.manifestSha256.trim(),
  manifestSignature: releaseEditor.manifestSignature.trim(),
  artifacts: [
    {
      fileName: releaseEditor.fileName.trim(),
      fileSizeBytes: Number(releaseEditor.fileSizeBytes),
      sha256: releaseEditor.sha256.trim(),
      signature: releaseEditor.signature.trim(),
      objectKey: releaseEditor.objectKey.trim(),
    },
  ],
})

const syncSelectedReleaseToHost = () => {
  if (!hostPublishedReleases.value.some((release) => release.id === selectedReleaseId.value)) {
    selectedReleaseId.value = hostPublishedReleases.value[0]?.id || ''
  }
}

const saveRelease = async () => {
  if (actionPending.value) return
  actionPending.value = true
  try {
    const payload = releasePayload()
    if (editingReleaseId.value) {
      await updateRelayServerRelease(editingReleaseId.value, payload)
      showToast('Relay 版本草稿已更新。')
    } else {
      await createRelayServerRelease(payload)
      showToast('Relay 版本草稿已创建。')
    }
    releaseEditorOpen.value = false
    await loadReleases(true)
  } catch (error) {
    showToast(problemMessage(error, '版本元数据校验失败，请检查签名、摘要和 objectKey。'))
  } finally {
    actionPending.value = false
  }
}

const publishRelease = async (release: RelayServerRelease) => {
  if (
    actionPending.value ||
    !window.confirm(`发布 Relay ${release.version}？发布后元数据不可修改。`)
  )
    return
  actionPending.value = true
  try {
    await publishRelayServerRelease(release.id)
    await loadReleases(true)
    showToast('Relay 版本已发布。')
  } catch (error) {
    showToast(problemMessage(error, '无法发布 Relay 版本。'))
  } finally {
    actionPending.value = false
  }
}

const retireRelease = async (release: RelayServerRelease) => {
  if (actionPending.value || !window.confirm(`退役 Relay ${release.version}？`)) return
  actionPending.value = true
  try {
    await retireRelayServerRelease(release.id)
    await loadReleases(true)
    showToast('Relay 版本已退役。')
  } catch (error) {
    showToast(problemMessage(error, '无法退役 Relay 版本。'))
  } finally {
    actionPending.value = false
  }
}

const removeRelease = async (release: RelayServerRelease) => {
  if (actionPending.value || !window.confirm(`删除 Relay ${release.version} 的元数据？`)) return
  actionPending.value = true
  try {
    await deleteRelayServerRelease(release.id)
    await loadReleases(true)
    showToast('Relay 版本元数据已删除。')
  } catch (error) {
    showToast(problemMessage(error, '该版本仍被主机引用或当前状态不允许删除。'))
  } finally {
    actionPending.value = false
  }
}

const activateHost = async () => {
  const installation = selected.value
  if (!installation?.identityThumbprint || !canActivate.value || actionPending.value) return
  actionPending.value = true
  try {
    selected.value = await activateRelayInstallation(
      installation.id,
      installation.identityThumbprint,
      {
        lb: checklist.lb,
        dns: checklist.dns,
        port: checklist.port,
        tls: checklist.tls,
      },
    )
    showToast('主机已启用。')
    await loadHosts(true)
  } catch (error) {
    showToast(problemMessage(error, '主机尚未满足启用条件。'))
  } finally {
    actionPending.value = false
  }
}

const pauseHost = async () => {
  const installation = selected.value
  const nodeID = installation?.currentInstance?.id
  if (!installation || !nodeID || actionPending.value) return
  if (
    !window.confirm(
      `暂停 ${installation.displayName} 接收新连接？已有连接会收到退出通知并按策略排空。`,
    )
  )
    return
  actionPending.value = true
  try {
    await drainRelayNode(nodeID)
    selected.value = {
      ...installation,
      status: 'draining',
      currentInstance: installation.currentInstance
        ? { ...installation.currentInstance, status: 'draining' }
        : null,
    }
    showToast('已创建暂停接入操作。')
    await loadHosts(true)
  } catch (error) {
    showToast(problemMessage(error, '无法暂停主机接入。'))
  } finally {
    actionPending.value = false
  }
}

const resumeHost = async () => {
  const installation = selected.value
  if (!installation || actionPending.value) return
  if (!window.confirm(`恢复 ${installation.displayName} 接收新连接？`)) return
  actionPending.value = true
  try {
    selected.value = await activateRelayInstallation(
      installation.id,
      installation.identityThumbprint ?? '',
      installation.deploymentChecklist,
    )
    showToast('主机已恢复接入。')
    await loadHosts(true)
  } catch (error) {
    showToast(problemMessage(error, '无法恢复主机接入，请确认当前进程心跳正常。'))
  } finally {
    actionPending.value = false
  }
}

const revokeHost = async () => {
  const installation = selected.value
  if (!installation || selectedState.value === 'revoked' || actionPending.value) return
  if (
    !window.confirm(
      `吊销 ${installation.displayName} 的连接权限？Access Key 和证书会立即失效且无法恢复。`,
    )
  )
    return
  actionPending.value = true
  try {
    await revokeRelayInstallation(installation.id)
    selected.value = { ...installation, status: 'revoked', revokedAt: new Date().toISOString() }
    showToast('主机连接权限已吊销。')
    await loadHosts(true)
  } catch (error) {
    showToast(problemMessage(error, '无法吊销主机身份。'))
  } finally {
    actionPending.value = false
  }
}

const deleteHost = async () => {
  const installation = selected.value
  if (!installation || selectedState.value !== 'revoked' || actionPending.value) return
  if (
    !window.confirm(`永久删除 ${installation.displayName}？删除后必须创建新主机和新 Access Key。`)
  ) {
    return
  }
  actionPending.value = true
  try {
    await deleteRelayInstallation(installation.id)
    installations.value = installations.value.filter((item) => item.id !== installation.id)
    selected.value = null
    detailOpen.value = false
    showToast('已删除吊销的主机。')
    await loadHosts(true)
  } catch (error) {
    showToast(problemMessage(error, '无法删除该主机，请确认它已经吊销。'))
  } finally {
    actionPending.value = false
  }
}

const copyText = async (value: string, label: string) => {
  try {
    await navigator.clipboard.writeText(value)
    showToast(`${label}已复制。`)
  } catch {
    showToast(`无法自动复制${label}，请手动选择。`)
  }
}

const relayPortableTarget = (installation: RelayInstallation): PortableTarget | null => {
  const platform = installation.platform === 'darwin' ? 'macos' : installation.platform
  const architecture = installation.architecture === 'amd64' ? 'x64' : installation.architecture
  if (
    (platform !== 'linux' && platform !== 'windows' && platform !== 'macos') ||
    (architecture !== 'x64' && architecture !== 'arm64')
  ) {
    return null
  }
  return { platform, architecture }
}

const downloadRelayInstaller = () => {
  if (!createdHost.value || !installCommand.value) return
  const target = relayPortableTarget(createdHost.value)
  if (!target) {
    showToast('当前中继主机的平台或架构不支持生成部署脚本。')
    return
  }
  const releaseVersion = releases.value.find(
    (release) => release.id === (createdHost.value?.releaseId || selectedReleaseId.value),
  )?.version
  if (!releaseVersion) {
    showToast('无法确认当前部署脚本对应的 Relay 版本，请刷新后重试。')
    return
  }
  try {
    downloadInstallerFile(
      createRelayBootstrapInstaller(installCommand.value, target),
      installerFileName('relay', target, releaseVersion),
    )
    showToast('Relay 一键部署脚本已生成；运行时请在隐藏提示中粘贴 Access Key。')
  } catch {
    showToast('无法生成 Relay 一键部署脚本，请复制安装命令后手动执行。')
  }
}

const formatDateTime = (value?: string | null) => {
  if (!value) return '—'
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return '—'
  return new Intl.DateTimeFormat('zh-CN', {
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
    hour12: false,
  }).format(date)
}

const relativeTime = (value?: string | null, waiting = '等待首次心跳') => {
  if (!value) return waiting
  const delta = nowMs.value - new Date(value).getTime()
  if (!Number.isFinite(delta)) return '—'
  if (delta < 10_000) return '刚刚'
  if (delta < 60_000) return `${Math.floor(delta / 1000)} 秒前`
  if (delta < 3_600_000) return `${Math.floor(delta / 60_000)} 分钟前`
  return formatDateTime(value)
}

const leaseText = (value?: string | null) => {
  if (!value) return '—'
  const remaining = new Date(value).getTime() - nowMs.value
  if (remaining <= 0) return '已过期'
  if (remaining < 60_000) return `还有 ${Math.ceil(remaining / 1000)} 秒到期`
  return formatDateTime(value)
}

const shortID = (value: string) => `${value.slice(0, 8)}…${value.slice(-4)}`
const hostInitials = (name: string) =>
  name
    .replace(/[^A-Za-z0-9]/g, '')
    .slice(0, 2)
    .toUpperCase() || 'R'

const closeOnEscape = (event: KeyboardEvent) => {
  if (event.key !== 'Escape') return
  if (releaseEditorOpen.value) releaseEditorOpen.value = false
  else if (addOpen.value) closeAdd()
  else detailOpen.value = false
}

onMounted(() => {
  void refreshPage()
  void loadReleases()
  refreshTimer = setInterval(() => {
    if (!addOpen.value && !detailOpen.value) void refreshPage(true)
    else if (detailOpen.value) void refreshSelected()
  }, 15_000)
  clockTimer = setInterval(() => (nowMs.value = Date.now()), 1_000)
  window.addEventListener('keydown', closeOnEscape)
})

onBeforeUnmount(() => {
  if (refreshTimer) clearInterval(refreshTimer)
  if (clockTimer) clearInterval(clockTimer)
  if (toastTimer) clearTimeout(toastTimer)
  window.removeEventListener('keydown', closeOnEscape)
})
</script>

<template>
  <div class="relay-host-page">
    <header class="page-head">
      <div>
        <p class="eyebrow">Relay hosts</p>
        <h1>中继主机</h1>
        <p class="lead">
          管理客户端之间实时通信所使用的中继服务器。主机主动注册并上报状态，管理端不会登录服务器或远程执行命令。
        </p>
      </div>
      <div class="page-actions">
        <button
          class="relay-button relay-button--secondary"
          type="button"
          :disabled="refreshing"
          @click="refreshPage(true)"
        >
          <span aria-hidden="true">↻</span>{{ refreshing ? '刷新中' : '刷新' }}
        </button>
        <button
          class="relay-button relay-button--primary"
          type="button"
          :disabled="forbidden"
          @click="openAdd"
        >
          <span aria-hidden="true">＋</span>添加主机
        </button>
      </div>
    </header>

    <div class="principle-note" role="note">
      <span class="note-icon" aria-hidden="true">i</span>
      <span
        >Relay 只提供指定端口的 WS 服务；公网 WSS 由 Nginx 等反向代理终止
        TLS。消息只在中继内存实时转发，不写入管理数据库。</span
      >
    </div>

    <div v-if="pageError" class="page-alert" :class="{ forbidden }" role="alert">
      <span aria-hidden="true">{{ forbidden ? '!' : '↻' }}</span>
      <span>{{ pageError }}</span>
      <button v-if="!forbidden" type="button" @click="refreshPage(true)">重试</button>
    </div>

    <section class="summary-bar" aria-label="中继主机摘要">
      <article>
        <span>主机总数</span><strong>{{ installations.length }}</strong>
      </article>
      <article>
        <span>在线</span><strong>{{ onlineCount }}</strong>
      </article>
      <article>
        <span>活动连接</span><strong>{{ totalConnections }}</strong>
      </article>
      <article>
        <span>需要处理</span><strong class="attention">{{ attentionCount }}</strong>
      </article>
    </section>

    <section v-if="!forbidden" class="scheduler-panel" aria-labelledby="scheduler-heading">
      <header class="panel-head">
        <div>
          <p class="eyebrow">Host scheduling</p>
          <h2 id="scheduler-heading">中继调度状态</h2>
          <p>Relay 上线后仍需显式启用所属调度组；未启用的 draft 组不会收到 Device 分配。</p>
        </div>
        <span class="scheduler-count"
          >{{ activeCellCount }} / {{ schedulingCells.length }} 已启用</span
        >
      </header>

      <div v-if="topologyLoading" class="loading-state" aria-live="polite">
        <span class="spinner" aria-hidden="true"></span>正在读取调度状态…
      </div>
      <div v-else-if="topologyError" class="scheduler-error" role="alert">
        <span>{{ topologyError }}</span>
        <button type="button" @click="loadTopology()">重试</button>
      </div>
      <div v-else-if="schedulingCells.length === 0" class="scheduler-empty">
        尚未初始化中继调度组，请先完成 Host 数据库迁移。
      </div>
      <div v-else class="scheduler-grid">
        <article v-for="item in schedulingCells" :key="item.cell.id" class="scheduler-card">
          <header class="scheduler-card-head">
            <div>
              <span
                class="cell-status"
                :class="`cell-status--${cellStatusMeta[item.cell.status].tone}`"
              >
                {{ cellStatusMeta[item.cell.status].label }}
              </span>
              <h3>
                {{ item.cell.name }} <code>{{ item.cell.code }}</code>
              </h3>
              <p>
                {{ item.regionName }}（{{ item.regionCode }}）· {{ item.poolName }}（{{
                  item.poolCode
                }}）
              </p>
            </div>
            <button
              v-if="item.cell.status === 'draft'"
              class="relay-button relay-button--primary relay-button--small"
              type="button"
              :disabled="Boolean(cellActionPending)"
              @click="activateSchedulingCell(item.cell)"
            >
              {{ cellActionPending === item.cell.id ? '启用中…' : '启用调度' }}
            </button>
          </header>
          <p class="scheduler-help">{{ cellStatusMeta[item.cell.status].help }}</p>
          <dl class="scheduler-metrics">
            <div>
              <dt>健康主机</dt>
              <dd>{{ item.cell.healthyInstances }}</dd>
            </div>
            <div>
              <dt>绑定主机</dt>
              <dd>{{ item.cell.installationCount }}</dd>
            </div>
            <div class="scheduler-endpoint">
              <dt>当前接入地址</dt>
              <dd>
                <code>{{ cellEndpoint(item.cell) }}</code>
              </dd>
            </div>
          </dl>
          <div
            v-if="item.cell.status === 'draft' && item.cell.healthyInstances > 0"
            class="scheduler-warning"
            role="note"
          >
            Relay 已在线，但调度尚未启用；这会导致 Device 持续收到 relay_unavailable。
          </div>
        </article>
      </div>
    </section>

    <section class="hosts-panel" aria-labelledby="host-list-heading">
      <header class="panel-head">
        <div>
          <h2 id="host-list-heading">主机列表</h2>
          <p>状态根据当前进程和 45 秒心跳租约计算。</p>
        </div>
        <span class="auto-refresh"><span aria-hidden="true">◷</span> 每 15 秒自动刷新</span>
      </header>

      <div v-if="loading" class="loading-state" aria-live="polite">
        <span class="spinner" aria-hidden="true"></span>正在读取主机状态…
      </div>

      <div v-else-if="!forbidden && installations.length === 0" class="empty-state">
        <span class="empty-icon" aria-hidden="true">⇄</span>
        <h3>还没有中继主机</h3>
        <p>添加第一台 Linux、Windows 或 macOS 主机，在宿主机完成注册后即可开始上报心跳。</p>
        <button class="relay-button relay-button--primary" type="button" @click="openAdd">
          添加第一台主机
        </button>
      </div>

      <div v-else-if="installations.length" class="table-wrap">
        <table>
          <thead>
            <tr>
              <th scope="col">主机</th>
              <th scope="col">地区</th>
              <th scope="col">分组</th>
              <th scope="col">状态</th>
              <th scope="col">公网接入地址</th>
              <th scope="col">活动连接</th>
              <th scope="col">版本</th>
              <th scope="col">最近心跳</th>
              <th scope="col"><span class="sr-only">操作</span></th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="installation in installations" :key="installation.id">
              <td data-label="主机">
                <div class="host-cell">
                  <span class="host-avatar" aria-hidden="true">{{
                    hostInitials(installation.displayName)
                  }}</span>
                  <span>
                    <strong>{{ installation.displayName }}</strong>
                    <small>主机编号 · {{ shortID(installation.id) }}</small>
                  </span>
                </div>
              </td>
              <td data-label="地区">
                <span class="label-value">{{ installation.region || '未设置' }}</span>
              </td>
              <td data-label="分组">
                <span class="label-value">{{ installation.group || '未分组' }}</span>
              </td>
              <td data-label="状态">
                <span class="status-badge" :class="hostState(installation)">
                  <span aria-hidden="true">{{ stateOf(installation).symbol }}</span
                  >{{ stateOf(installation).label }}
                </span>
              </td>
              <td data-label="公网接入地址">
                <code class="endpoint">{{ displayedPublicEndpoint(installation) }}</code>
              </td>
              <td data-label="活动连接" class="number">
                {{ installation.currentInstance?.activeConnections ?? '—' }}
              </td>
              <td data-label="版本" class="number">
                {{ installation.currentInstance?.version || '—' }}
              </td>
              <td data-label="最近心跳">
                {{ relativeTime(installation.currentInstance?.lastHeartbeatAt) }}
              </td>
              <td data-label="操作">
                <button class="table-action" type="button" @click="openDetail(installation)">
                  查看
                </button>
              </td>
            </tr>
          </tbody>
        </table>
      </div>

      <footer v-if="installations.length" class="panel-foot">
        <span>共 {{ installations.length }} 台主机</span>
        <span>地区与分组仅用于管理和筛选，不影响 Relay 连接。</span>
      </footer>
    </section>

    <section class="release-panel" aria-labelledby="relay-release-title">
      <header class="panel-head">
        <div>
          <p class="eyebrow">Release catalog</p>
          <h2 id="relay-release-title">Relay 程序版本</h2>
          <p>只登记已签名制品的元数据与 objectKey，不在此页面上传二进制文件。</p>
        </div>
        <button
          class="relay-button relay-button--secondary"
          type="button"
          @click="openReleaseEditor()"
        >
          添加版本元数据
        </button>
      </header>
      <div v-if="!releases.length" class="release-empty">尚未配置 Relay 程序版本。</div>
      <div v-else class="release-list">
        <article v-for="release in releases" :key="release.id" class="release-item">
          <div>
            <strong>{{ release.version }}</strong>
            <span class="release-status" :class="`release-status--${release.status}`">{{
              release.status
            }}</span>
            <p>
              {{ release.platform }}/{{ release.architecture }} · protocol v2 ·
              {{ release.artifacts[0]?.fileName || '无安装包' }}
            </p>
          </div>
          <div class="release-actions">
            <button
              v-if="release.status === 'draft'"
              type="button"
              @click="openReleaseEditor(release)"
            >
              编辑
            </button>
            <button
              v-if="release.status === 'draft'"
              type="button"
              @click="publishRelease(release)"
            >
              发布
            </button>
            <button
              v-if="release.status === 'published'"
              type="button"
              @click="retireRelease(release)"
            >
              退役
            </button>
            <button
              v-if="release.status !== 'published'"
              class="release-delete"
              type="button"
              @click="removeRelease(release)"
            >
              删除
            </button>
          </div>
        </article>
      </div>
    </section>

    <div
      v-if="releaseEditorOpen"
      class="dialog-backdrop"
      @mousedown.self="releaseEditorOpen = false"
    >
      <section class="relay-dialog release-dialog" role="dialog" aria-modal="true">
        <header class="dialog-head">
          <div>
            <h2>{{ editingReleaseId ? '编辑 Relay 版本草稿' : '添加 Relay 版本元数据' }}</h2>
            <p>服务端会严格校验摘要、签名、制品文件名与 objectKey。</p>
          </div>
          <button
            class="close-button"
            type="button"
            aria-label="关闭"
            @click="releaseEditorOpen = false"
          >
            ×
          </button>
        </header>
        <form class="host-form" @submit.prevent="saveRelease">
          <div class="dialog-body release-form-grid">
            <label class="field">
              <span>版本</span>
              <input v-model="releaseEditor.version" required maxlength="64" placeholder="1.2.3" />
            </label>
            <label class="field">
              <span>目标平台</span>
              <select v-model="releaseEditor.platform" name="releasePlatform">
                <option value="linux">Linux（systemd）</option>
                <option value="windows">Windows（Windows Service）</option>
                <option value="darwin">macOS（launchd）</option>
              </select>
            </label>
            <label class="field">
              <span>目标架构</span>
              <select v-model="releaseEditor.architecture" name="releaseArchitecture">
                <option value="amd64">amd64（x86_64）</option>
                <option value="arm64">arm64（aarch64 / Apple silicon）</option>
              </select>
            </label>
            <label class="field">
              <span>构建时间（ISO 8601）</span>
              <input
                v-model="releaseEditor.buildTime"
                required
                placeholder="2026-08-08T12:00:00Z"
              />
            </label>
            <label class="field">
              <span>协议版本</span>
              <input
                v-model.number="releaseEditor.protocolMin"
                required
                min="2"
                max="2"
                readonly
                type="number"
              />
            </label>
            <label class="field">
              <span>协议版本上限</span>
              <input
                v-model.number="releaseEditor.protocolMax"
                required
                min="2"
                max="2"
                readonly
                type="number"
              />
            </label>
            <label class="field release-form-wide">
              <span>构建 Commit（40–64 位小写十六进制）</span>
              <input
                v-model="releaseEditor.buildCommit"
                required
                minlength="40"
                maxlength="64"
                pattern="[0-9a-f]{40,64}"
                spellcheck="false"
              />
            </label>
            <label class="field">
              <span>签名 Key ID</span>
              <input v-model="releaseEditor.signingKeyId" required maxlength="120" />
            </label>
            <label class="field">
              <span>Manifest SHA-256</span>
              <input
                v-model="releaseEditor.manifestSha256"
                required
                maxlength="64"
                spellcheck="false"
              />
            </label>
            <label class="field release-form-wide">
              <span>Manifest 签名</span>
              <textarea
                v-model="releaseEditor.manifestSignature"
                required
                minlength="16"
                maxlength="4096"
                rows="2"
              ></textarea>
            </label>
            <label class="field">
              <span>安装包文件名（.tar.gz）</span>
              <input v-model="releaseEditor.fileName" required maxlength="255" />
            </label>
            <label class="field">
              <span>安装包字节数</span>
              <input v-model.number="releaseEditor.fileSizeBytes" required min="1" type="number" />
            </label>
            <label class="field release-form-wide">
              <span>安装包 objectKey</span>
              <input
                v-model="releaseEditor.objectKey"
                required
                maxlength="1024"
                spellcheck="false"
              />
            </label>
            <label class="field release-form-wide">
              <span>安装包 SHA-256</span>
              <input v-model="releaseEditor.sha256" required maxlength="64" spellcheck="false" />
            </label>
            <label class="field release-form-wide">
              <span>安装包签名</span>
              <textarea
                v-model="releaseEditor.signature"
                required
                minlength="16"
                maxlength="4096"
                rows="2"
              ></textarea>
            </label>
          </div>
          <footer class="dialog-actions">
            <button
              class="relay-button relay-button--secondary"
              type="button"
              @click="releaseEditorOpen = false"
            >
              取消
            </button>
            <button
              class="relay-button relay-button--primary"
              type="submit"
              :disabled="actionPending"
            >
              {{ actionPending ? '保存中…' : '保存草稿' }}
            </button>
          </footer>
        </form>
      </section>
    </div>

    <div v-if="addOpen" class="dialog-backdrop" @mousedown.self="closeAdd">
      <section
        class="relay-dialog"
        role="dialog"
        aria-modal="true"
        aria-labelledby="add-host-title"
      >
        <template v-if="addStep === 'form'">
          <header class="dialog-head">
            <div>
              <h2 id="add-host-title">添加中继主机</h2>
              <p>选择已发布版本后，生成 Access Key 模式的一键安装命令。</p>
            </div>
            <button class="close-button" type="button" aria-label="关闭" @click="closeAdd">
              ×
            </button>
          </header>
          <form class="host-form" @submit.prevent="createHost">
            <div class="dialog-body">
              <label class="field">
                <span>主机名称</span>
                <input
                  ref="hostNameInput"
                  v-model="hostName"
                  required
                  maxlength="120"
                  autocomplete="off"
                  placeholder="例如 relay-cn-01"
                />
                <small>使用服务器用途或机房命名，创建后仍可修改。</small>
              </label>
              <div class="field-pair">
                <label class="field">
                  <span>地区（可选）</span>
                  <input
                    v-model="hostRegion"
                    maxlength="80"
                    autocomplete="off"
                    placeholder="例如 华东"
                  />
                </label>
                <label class="field">
                  <span>分组（可选）</span>
                  <input
                    v-model="hostGroup"
                    maxlength="80"
                    autocomplete="off"
                    placeholder="例如 生产环境"
                  />
                </label>
              </div>
              <label class="field">
                <span>宿主机平台</span>
                <select v-model="hostPlatform" name="platform" @change="syncSelectedReleaseToHost">
                  <option value="linux">Linux（systemd）</option>
                  <option value="windows">Windows（Windows Service）</option>
                  <option value="darwin">macOS（launchd）</option>
                </select>
              </label>
              <label class="field">
                <span>宿主机架构</span>
                <select
                  v-model="hostArchitecture"
                  name="architecture"
                  @change="syncSelectedReleaseToHost"
                >
                  <option value="amd64">amd64（x86_64）</option>
                  <option value="arm64">arm64（aarch64 / Apple silicon）</option>
                </select>
                <small>安装包、原生服务脚本与宿主机平台和架构必须完全一致。</small>
              </label>
              <label v-if="hostPublishedReleases.length" class="field">
                <span>安装版本（可选）</span>
                <select v-model="selectedReleaseId" name="releaseId">
                  <option value="">暂不安装，仅创建主机</option>
                  <option
                    v-for="release in hostPublishedReleases"
                    :key="release.id"
                    :value="release.id"
                  >
                    {{ release.version }} · {{ release.platform }}/{{ release.architecture }}
                  </option>
                </select>
                <small>选择版本会同时生成经过签名校验的一键安装命令。</small>
              </label>
              <div v-else class="principle-note compact" role="note">
                <span class="note-icon" aria-hidden="true">i</span>
                <span
                  >当前架构没有已发布的 Relay
                  版本；可先创建主机，再到下方配置并发布对应架构版本。</span
                >
              </div>
              <fieldset class="relay-endpoint-fieldset">
                <legend>Relay 网络配置（访问链接可稍后填写）</legend>
                <div class="relay-endpoint-fields relay-endpoint-fields--simple">
                  <label class="field">
                    <span>Relay WS 监听端口</span>
                    <input
                      v-model="relayListenerPort"
                      name="listenerPort"
                      required
                      maxlength="5"
                      autocomplete="off"
                      inputmode="numeric"
                      spellcheck="false"
                      placeholder="8443"
                    />
                    <small>Relay 只在此端口提供明文 WS，不直接加载 TLS 证书。</small>
                  </label>
                  <label class="field relay-endpoint-host">
                    <span>客户端访问链接（可选）</span>
                    <input
                      v-model="relayPublicEndpoint"
                      name="publicEndpoint"
                      maxlength="255"
                      autocomplete="off"
                      autocapitalize="none"
                      spellcheck="false"
                      placeholder="wss://relay.example.com/v2/connect"
                    />
                    <small>填写客户端实际使用的完整 ws:// 或 wss:// 链接。</small>
                  </label>
                </div>
                <small class="relay-endpoint-help">
                  {{ relayTransportHint(relayPublicEndpoint, relayListenerPort) }}
                </small>
              </fieldset>
              <div class="principle-note compact" role="note">
                <span class="note-icon" aria-hidden="true">i</span>
                <span>页面只登记身份和状态，不会连接 SSH，也不会在服务器上自动安装程序。</span>
              </div>
            </div>
            <footer class="dialog-actions">
              <button class="relay-button relay-button--secondary" type="button" @click="closeAdd">
                取消
              </button>
              <button
                class="relay-button relay-button--primary"
                type="submit"
                :disabled="actionPending || !hostName.trim() || !relayEndpointConfigurationValid"
              >
                {{ actionPending ? '创建中…' : '创建并生成 Access Key' }}
              </button>
            </footer>
          </form>
        </template>

        <template v-else>
          <header class="dialog-head">
            <div>
              <h2 id="add-host-title">配置 {{ createdHost?.displayName }}</h2>
              <p>把以下环境变量保存到目标服务器，Relay 启动后会自动连接管理端。</p>
            </div>
            <button class="close-button" type="button" aria-label="关闭" @click="closeAdd">
              ×
            </button>
          </header>
          <div class="dialog-body">
            <div class="one-time-warning" role="alert">
              <strong>Access Key 只显示一次：</strong>
              <span>Key 默认不过期，可随时在管理端吊销或重新生成。</span>
            </div>
            <div class="secret-block">
              <div class="block-head">
                <span>中继主机 Access Key</span>
                <button type="button" @click="copyText(accessKey?.key || '', 'Access Key')">
                  复制 Key
                </button>
              </div>
              <code class="secret-value">{{ accessKey?.key }}</code>
              <small>Key 标识：{{ accessKey?.keyPrefix }}</small>
            </div>
            <fieldset class="relay-endpoint-fieldset">
              <legend>管理端下发的 Relay 网络配置</legend>
              <div class="relay-endpoint-fields relay-endpoint-fields--simple">
                <label class="field">
                  <span>Relay WS 监听端口</span>
                  <input
                    v-model="relayListenerPort"
                    name="accessListenerPort"
                    required
                    maxlength="5"
                    autocomplete="off"
                    inputmode="numeric"
                    spellcheck="false"
                    placeholder="8443"
                    :readonly="!canEditCreatedConfiguration"
                  />
                </label>
                <label class="field relay-endpoint-host">
                  <span>客户端访问链接（WS / WSS）</span>
                  <input
                    v-model="relayPublicEndpoint"
                    name="accessPublicEndpoint"
                    maxlength="255"
                    autocomplete="off"
                    autocapitalize="none"
                    spellcheck="false"
                    placeholder="wss://relay.example.com/v2/connect"
                    :readonly="!canEditCreatedConfiguration"
                  />
                </label>
              </div>
              <small class="relay-endpoint-help">
                {{ relayTransportHint(relayPublicEndpoint, relayListenerPort) }}
              </small>
            </fieldset>
            <button
              v-if="canEditCreatedConfiguration"
              class="relay-button relay-button--secondary relay-button--small"
              type="button"
              :disabled="actionPending || !relayConfigurationValid"
              @click="saveRelayConfiguration"
            >
              {{ actionPending ? '保存中…' : '保存运行配置' }}
            </button>
            <div class="command-block">
              <div class="block-head">
                <span>relay.env</span>
                <button type="button" @click="copyText(relayEnv, '.env 配置')">复制 .env</button>
              </div>
              <pre class="enrollment-command">{{ relayEnv }}</pre>
            </div>
            <div v-if="installCommand" class="command-block">
              <div class="block-head">
                <span>一键部署命令</span>
                <div class="block-actions">
                  <button type="button" @click="copyText(installCommand, '安装命令')">
                    复制命令
                  </button>
                  <button type="button" @click="downloadRelayInstaller">
                    下载一键部署脚本（{{ installerTargetLabel }}）
                  </button>
                </div>
              </div>
              <pre class="enrollment-command">{{ installCommand }}</pre>
              <small
                >脚本与命令均不包含 Access Key；运行时会通过隐藏输入读取上方 Key。当前脚本目标为
                {{ installerTargetLabel }}，与创建主机时选择的平台和架构一致。</small
              >
            </div>
            <ol class="enrollment-steps">
              <li>
                <span>1</span>下载并在目标宿主机以管理员权限运行一键部署脚本（也可复制命令）。
              </li>
              <li><span>2</span>在隐藏提示中粘贴 Access Key；脚本会使用平台原生权限保护凭据。</li>
              <li><span>3</span>Relay 自动启动、拉取配置、注册并持续心跳。</li>
            </ol>
          </div>
          <footer class="dialog-actions">
            <button class="relay-button relay-button--primary" type="button" @click="closeAdd">
              完成
            </button>
          </footer>
        </template>
      </section>
    </div>

    <div v-if="detailOpen" class="dialog-backdrop" @mousedown.self="detailOpen = false">
      <section
        class="relay-dialog detail-dialog"
        role="dialog"
        aria-modal="true"
        aria-labelledby="detail-title"
      >
        <header class="dialog-head">
          <div>
            <h2 id="detail-title">{{ selected?.displayName }}</h2>
            <p>Installation ID · {{ selected?.id }}</p>
          </div>
          <button class="close-button" type="button" aria-label="关闭" @click="detailOpen = false">
            ×
          </button>
        </header>

        <div v-if="detailLoading" class="loading-state">
          <span class="spinner" aria-hidden="true"></span>读取详情…
        </div>
        <div v-else-if="selected" class="dialog-body">
          <div class="detail-status-row">
            <div>
              <span class="status-badge" :class="selectedState">
                <span aria-hidden="true">{{ stateOf(selected).symbol }}</span
                >{{ stateOf(selected).label }}
              </span>
              <p>{{ stateOf(selected).help }}</p>
            </div>
            <div class="detail-primary-actions">
              <button
                v-if="selectedState === 'online'"
                class="relay-button relay-button--secondary relay-button--small"
                type="button"
                :disabled="actionPending"
                @click="pauseHost"
              >
                暂停接入
              </button>
              <button
                v-else-if="selectedState === 'draining' || selectedState === 'paused'"
                class="relay-button relay-button--secondary relay-button--small"
                type="button"
                :disabled="actionPending"
                @click="resumeHost"
              >
                恢复接入
              </button>
              <button
                v-if="selectedState !== 'revoked'"
                class="relay-button relay-button--secondary relay-button--small"
                type="button"
                :disabled="actionPending"
                @click="rotateAccessKey"
              >
                {{ selectedState === 'pending' ? '重新生成 Access Key' : '更换 Access Key' }}
              </button>
            </div>
          </div>

          <div class="metric-grid" aria-label="当前运行指标">
            <article>
              <span>活动连接</span
              ><strong>{{ selected.currentInstance?.activeConnections ?? '—' }}</strong>
            </article>
            <article>
              <span>版本</span><strong>{{ selected.currentInstance?.version || '—' }}</strong>
            </article>
            <article>
              <span>最近心跳</span
              ><strong>{{ relativeTime(selected.currentInstance?.lastHeartbeatAt) }}</strong>
            </article>
          </div>

          <section class="detail-section">
            <h3>主机信息</h3>
            <dl class="detail-list">
              <div>
                <dt>主机名称</dt>
                <dd>{{ selected.displayName }}</dd>
              </div>
              <div>
                <dt>地区</dt>
                <dd>{{ selected.region || '未设置' }}</dd>
              </div>
              <div>
                <dt>分组</dt>
                <dd>{{ selected.group || '未分组' }}</dd>
              </div>
              <div>
                <dt>Relay 上报的公网连接</dt>
                <dd>
                  <code>{{ displayedPublicEndpoint(selected) }}</code>
                </dd>
              </div>
              <div>
                <dt>Relay WS 监听端口</dt>
                <dd>{{ selected.listenerPort }}</dd>
              </div>
              <div>
                <dt>当前 Instance ID</dt>
                <dd>{{ selected.currentInstance?.id || '—' }}</dd>
              </div>
              <div>
                <dt>认证方式</dt>
                <dd class="fingerprint">{{ selected.identityThumbprint || 'Access Key' }}</dd>
              </div>
              <div>
                <dt>首次注册</dt>
                <dd>{{ formatDateTime(selected.firstEnrolledAt) }}</dd>
              </div>
              <div>
                <dt>心跳租约</dt>
                <dd>{{ leaseText(selected.currentInstance?.leaseExpiresAt) }}</dd>
              </div>
              <div>
                <dt>最近更新</dt>
                <dd>{{ formatDateTime(selected.updatedAt) }}</dd>
              </div>
              <div>
                <dt>运行配置</dt>
                <dd>
                  {{
                    selected.currentInstance?.capabilities.restartRequired === true
                      ? '存在非热更新变更，需要重启'
                      : '已应用或无需重启'
                  }}
                </dd>
              </div>
            </dl>
            <div v-if="selectedState !== 'revoked'" class="endpoint-editor">
              <label class="field">
                <span>Relay WS 监听端口</span>
                <input
                  v-model="detailListenerPort"
                  name="detailListenerPort"
                  required
                  maxlength="5"
                  autocomplete="off"
                  inputmode="numeric"
                  spellcheck="false"
                  placeholder="8443"
                />
                <small>端口变化后 Relay 会在下一次心跳自动重启本地 WS 监听。</small>
              </label>
              <label class="field">
                <span>客户端访问链接（WS / WSS）</span>
                <input
                  v-model="detailPublicEndpoint"
                  name="detailPublicEndpoint"
                  maxlength="255"
                  autocomplete="off"
                  spellcheck="false"
                  placeholder="wss://relay.example.com/v2/connect"
                />
                <small>{{ relayTransportHint(detailPublicEndpoint, detailListenerPort) }}</small>
              </label>
              <button
                class="relay-button relay-button--secondary relay-button--small"
                type="button"
                :disabled="actionPending || !detailRelayConfigurationValid"
                @click="saveSelectedPublicEndpoint"
              >
                保存接入配置
              </button>
            </div>
          </section>

          <section
            v-if="
              ['online', 'offline', 'draining', 'paused'].includes(selectedState || '') &&
              selectedHostPublishedReleases.length
            "
            class="detail-section upgrade-panel"
          >
            <h3>安装 / 升级</h3>
            <p>选择已发布版本，生成校验签名并复用现有配置的一键升级命令。</p>
            <div class="upgrade-controls">
              <select v-model="upgradeReleaseId" name="upgradeReleaseId">
                <option
                  v-for="release in selectedHostPublishedReleases"
                  :key="release.id"
                  :value="release.id"
                >
                  {{ release.version }} · {{ release.platform }}/{{ release.architecture }}
                </option>
              </select>
              <button
                class="relay-button relay-button--secondary relay-button--small"
                type="button"
                :disabled="actionPending || !upgradeReleaseId"
                @click="prepareUpgrade"
              >
                生成升级命令
              </button>
            </div>
            <div v-if="upgradeCommand" class="command-block">
              <div class="block-head">
                <span>一键升级命令</span>
                <button type="button" @click="copyText(upgradeCommand, '升级命令')">
                  复制命令
                </button>
              </div>
              <pre class="enrollment-command">{{ upgradeCommand }}</pre>
            </div>
          </section>

          <section v-if="selectedState === 'pending_activation'" class="activation-panel">
            <h3>核对后启用</h3>
            <p>逐字核对服务器上的身份指纹，并确认外部接入条件已经完成。</p>
            <label
              ><input v-model="fingerprintConfirmed" type="checkbox" />
              身份指纹与服务器输出完全一致</label
            >
            <div class="check-grid">
              <label><input v-model="checklist.lb" type="checkbox" /> 已加入负载均衡</label>
              <label><input v-model="checklist.dns" type="checkbox" /> DNS 已验证</label>
              <label><input v-model="checklist.port" type="checkbox" /> 端口与安全组已开放</label>
              <label
                ><input v-model="checklist.tls" type="checkbox" /> 外部 WS/WSS 接入与 mTLS
                已验证</label
              >
            </div>
            <button
              class="relay-button relay-button--primary relay-button--small"
              type="button"
              :disabled="!canActivate || actionPending"
              @click="activateHost"
            >
              核对完成并启用
            </button>
          </section>

          <section class="danger-zone">
            <strong>{{
              selectedState === 'revoked' ? '删除已吊销主机' : '吊销主机 Access Key'
            }}</strong>
            <p>
              {{
                selectedState === 'revoked'
                  ? '删除后主机将从列表移除；如需重新部署，必须创建新主机和新 Access Key。'
                  : '吊销后 Relay 会失去连接权限，现有实例会被强制下线。'
              }}
            </p>
            <button
              v-if="selectedState !== 'revoked'"
              class="relay-button relay-button--danger relay-button--small"
              type="button"
              :disabled="actionPending"
              @click="revokeHost"
            >
              吊销主机
            </button>
            <button
              v-else
              class="relay-button relay-button--danger relay-button--small"
              type="button"
              :disabled="actionPending"
              @click="deleteHost"
            >
              永久删除主机
            </button>
          </section>
        </div>

        <footer class="dialog-actions">
          <button
            class="relay-button relay-button--secondary"
            type="button"
            @click="detailOpen = false"
          >
            关闭
          </button>
        </footer>
      </section>
    </div>

    <div v-if="toast" class="toast" role="status" aria-live="polite">{{ toast }}</div>
  </div>
</template>

<style scoped>
.relay-host-page {
  width: min(1180px, 100%);
  margin: 0 auto;
  padding: 42px clamp(20px, 4vw, 52px) 72px;
}

.page-head {
  display: flex;
  align-items: flex-end;
  justify-content: space-between;
  gap: 28px;
  margin-bottom: 22px;
}

.eyebrow {
  margin: 0 0 7px;
  color: var(--teal-dark);
  font-size: 0.76rem;
  font-weight: 800;
  letter-spacing: 0.14em;
  text-transform: uppercase;
}

h1,
h2,
h3,
p {
  margin-top: 0;
}

h1 {
  margin-bottom: 10px;
  color: var(--night);
  font-size: clamp(2rem, 4vw, 3rem);
  letter-spacing: -0.055em;
}

.lead {
  max-width: 760px;
  margin-bottom: 0;
  color: var(--ink-soft);
  line-height: 1.75;
}

.page-actions,
.dialog-actions,
.detail-primary-actions {
  display: flex;
  align-items: center;
  gap: 10px;
}

.relay-button {
  min-height: 42px;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  gap: 7px;
  padding: 0 17px;
  border: 1px solid transparent;
  border-radius: 11px;
  cursor: pointer;
  font-weight: 760;
  transition: 160ms ease;
}

.relay-button:hover:not(:disabled) {
  transform: translateY(-1px);
}

.relay-button:focus-visible,
button:focus-visible,
input:focus-visible {
  outline: 3px solid rgba(12, 166, 120, 0.24);
  outline-offset: 2px;
}

.relay-button:disabled {
  cursor: not-allowed;
  opacity: 0.5;
}

.relay-button--primary {
  background: var(--night);
  color: white;
  box-shadow: 0 8px 18px rgba(14, 42, 34, 0.16);
}

.relay-button--primary:hover:not(:disabled) {
  background: var(--teal-dark);
}

.relay-button--secondary {
  border-color: var(--line-strong);
  background: var(--paper);
  color: var(--ink);
}

.relay-button--danger {
  border-color: #efb7b3;
  background: #fff7f6;
  color: #a52d28;
}

.relay-button--small {
  min-height: 36px;
  padding-inline: 13px;
  font-size: 0.88rem;
}

.principle-note {
  display: flex;
  align-items: flex-start;
  gap: 11px;
  margin-bottom: 24px;
  padding: 14px 16px;
  border: 1px solid #cfe7dc;
  border-radius: 13px;
  background: var(--paper-tint);
  color: #315f50;
  font-size: 0.9rem;
  line-height: 1.6;
}

.principle-note.compact {
  margin: 0;
}

.note-icon {
  width: 21px;
  height: 21px;
  display: grid;
  flex: 0 0 auto;
  place-items: center;
  border-radius: 50%;
  background: var(--teal);
  color: white;
  font-size: 0.75rem;
  font-weight: 900;
}

.page-alert {
  display: flex;
  align-items: center;
  gap: 10px;
  margin: 0 0 20px;
  padding: 13px 15px;
  border: 1px solid #e8c58f;
  border-radius: 12px;
  background: #fff9ed;
  color: #765019;
}

.page-alert.forbidden {
  border-color: #eab9b5;
  background: #fff7f6;
  color: #912d28;
}

.page-alert button {
  margin-left: auto;
  border: 0;
  background: transparent;
  color: inherit;
  cursor: pointer;
  font-weight: 800;
}

.summary-bar {
  display: grid;
  grid-template-columns: repeat(4, 1fr);
  margin-bottom: 20px;
  overflow: hidden;
  border: 1px solid var(--line);
  border-radius: var(--radius-card);
  background: var(--paper);
  box-shadow: var(--shadow-small);
}

.summary-bar article {
  padding: 19px 22px;
  border-right: 1px solid var(--line);
}

.summary-bar article:last-child {
  border-right: 0;
}

.summary-bar span,
.metric-grid span {
  display: block;
  margin-bottom: 6px;
  color: var(--ink-soft);
  font-size: 0.78rem;
  font-weight: 700;
}

.summary-bar strong {
  color: var(--night);
  font-size: 1.65rem;
}

.summary-bar .attention {
  color: #b56b12;
}

.scheduler-panel {
  margin-bottom: 20px;
  overflow: hidden;
  border: 1px solid var(--line);
  border-radius: var(--radius-card);
  background: var(--paper);
  box-shadow: var(--shadow-small);
}

.scheduler-count {
  color: var(--ink-soft);
  font-size: 0.82rem;
  font-weight: 800;
  white-space: nowrap;
}

.scheduler-grid {
  display: grid;
  gap: 14px;
  padding: 18px 24px 24px;
}

.scheduler-card {
  padding: 18px;
  border: 1px solid var(--line);
  border-radius: 14px;
  background: #fbfdfc;
}

.scheduler-card-head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 20px;
}

.scheduler-card h3 {
  margin: 9px 0 4px;
  color: var(--night);
  font-size: 1rem;
}

.scheduler-card h3 code,
.scheduler-metrics code {
  color: var(--teal-dark);
  font-size: 0.78rem;
}

.scheduler-card-head p,
.scheduler-help {
  margin: 0;
  color: var(--ink-soft);
  font-size: 0.82rem;
  line-height: 1.55;
}

.scheduler-help {
  margin-top: 14px;
}

.cell-status {
  display: inline-flex;
  padding: 4px 8px;
  border-radius: 999px;
  font-size: 0.72rem;
  font-weight: 850;
}

.cell-status--draft,
.cell-status--draining {
  background: #fff3d9;
  color: #8b5b13;
}

.cell-status--active {
  background: #e4f6ed;
  color: #087451;
}

.cell-status--disabled {
  background: #f1efed;
  color: #685d57;
}

.scheduler-metrics {
  display: grid;
  grid-template-columns: 120px 120px minmax(0, 1fr);
  gap: 12px;
  margin: 16px 0 0;
}

.scheduler-metrics div {
  padding: 11px 13px;
  border-radius: 10px;
  background: var(--paper-soft);
}

.scheduler-metrics dt {
  color: var(--ink-faint);
  font-size: 0.72rem;
  font-weight: 750;
}

.scheduler-metrics dd {
  margin: 5px 0 0;
  color: var(--ink);
  font-size: 0.88rem;
  font-weight: 800;
}

.scheduler-endpoint dd {
  overflow-wrap: anywhere;
}

.scheduler-warning {
  margin-top: 14px;
  padding: 10px 12px;
  border-radius: 10px;
  background: #fff6e5;
  color: #81561a;
  font-size: 0.8rem;
  line-height: 1.55;
}

.scheduler-error,
.scheduler-empty {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  padding: 20px 24px;
  color: var(--ink-soft);
}

.scheduler-error {
  color: #912d28;
}

.scheduler-error button {
  border: 0;
  background: transparent;
  color: inherit;
  cursor: pointer;
  font-weight: 800;
}

.hosts-panel {
  overflow: hidden;
  border: 1px solid var(--line);
  border-radius: var(--radius-card);
  background: var(--paper);
  box-shadow: var(--shadow-medium);
}

.release-panel {
  margin-top: 20px;
  overflow: hidden;
  border: 1px solid var(--line);
  border-radius: var(--radius-card);
  background: var(--paper);
  box-shadow: var(--shadow-small);
}

.release-empty {
  padding: 24px;
  color: var(--ink-soft);
}

.release-list {
  display: grid;
}

.release-item {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 18px;
  padding: 17px 24px;
  border-bottom: 1px solid var(--line);
}

.release-item:last-child {
  border-bottom: 0;
}

.release-item strong {
  margin-right: 9px;
  color: var(--night);
}

.release-item p {
  margin: 6px 0 0;
  color: var(--ink-soft);
  font-size: 0.8rem;
}

.release-status {
  padding: 3px 7px;
  border-radius: 999px;
  background: var(--paper-soft);
  color: var(--ink-soft);
  font-size: 0.7rem;
  font-weight: 800;
}

.release-status--published {
  background: #e3f5ed;
  color: #17634d;
}

.release-status--retired,
.release-status--revoked {
  background: #f3efec;
  color: #725f53;
}

.release-actions {
  display: flex;
  flex: 0 0 auto;
  gap: 8px;
}

.release-actions button,
.block-head button {
  border: 0;
  background: transparent;
  color: var(--teal-dark);
  cursor: pointer;
  font-weight: 800;
}

.release-actions .release-delete {
  color: #a33b35;
}

.panel-head,
.panel-foot {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 20px;
  padding: 21px 24px;
}

.panel-head {
  border-bottom: 1px solid var(--line);
}

.panel-head h2 {
  margin-bottom: 4px;
  font-size: 1.08rem;
}

.panel-head p,
.dialog-head p,
.detail-status-row p,
.activation-panel p,
.danger-zone p {
  margin-bottom: 0;
  color: var(--ink-soft);
  font-size: 0.84rem;
  line-height: 1.55;
}

.auto-refresh {
  color: var(--ink-faint);
  font-size: 0.8rem;
  white-space: nowrap;
}

.table-wrap {
  overflow-x: auto;
}

table {
  width: 100%;
  border-collapse: collapse;
  table-layout: fixed;
}

th,
td {
  padding: 16px 18px;
  border-bottom: 1px solid var(--line);
  text-align: left;
  vertical-align: middle;
}

th {
  background: var(--paper-soft);
  color: var(--ink-soft);
  font-size: 0.74rem;
  font-weight: 800;
  letter-spacing: 0.03em;
}

th:nth-child(1) {
  width: 18%;
}

th:nth-child(2) {
  width: 8%;
}

th:nth-child(3) {
  width: 9%;
}

th:nth-child(4) {
  width: 10%;
}

th:nth-child(5) {
  width: 22%;
}

th:nth-child(6),
th:nth-child(7) {
  width: 8%;
}

th:nth-child(8) {
  width: 12%;
}

th:nth-child(9) {
  width: 5%;
}

tbody tr:last-child td {
  border-bottom: 0;
}

tbody tr:hover {
  background: #fbfdfc;
}

.host-cell {
  display: flex;
  align-items: center;
  gap: 11px;
}

.host-cell strong,
.host-cell small {
  display: block;
}

.host-cell strong {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.host-cell small {
  margin-top: 4px;
  color: var(--ink-faint);
  font-size: 0.72rem;
}

.host-avatar {
  width: 36px;
  height: 36px;
  display: grid;
  flex: 0 0 auto;
  place-items: center;
  border-radius: 10px;
  background: var(--brand-tint);
  color: var(--teal-dark);
  font-size: 0.72rem;
  font-weight: 900;
}

.status-badge {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  padding: 5px 9px;
  border-radius: 999px;
  background: #eef2f0;
  color: #52615c;
  font-size: 0.76rem;
  font-weight: 800;
  white-space: nowrap;
}

.status-badge.online {
  background: #e8f7f0;
  color: #087451;
}

.status-badge.offline,
.status-badge.revoked {
  background: #fff0ef;
  color: #a33631;
}

.status-badge.draining,
.status-badge.paused,
.status-badge.pending_activation {
  background: #fff5df;
  color: #956015;
}

.endpoint,
.fingerprint,
.secret-value,
.enrollment-command,
.detail-list dd {
  overflow-wrap: anywhere;
}

.endpoint {
  display: block;
  overflow: hidden;
  color: #315c50;
  font-size: 0.78rem;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.label-value {
  display: block;
  overflow: hidden;
  color: var(--ink-soft);
  font-size: 0.8rem;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.number {
  font-variant-numeric: tabular-nums;
}

.table-action,
.block-head button {
  border: 0;
  background: transparent;
  color: var(--teal-dark);
  cursor: pointer;
  font-weight: 800;
}

.panel-foot {
  border-top: 1px solid var(--line);
  background: var(--paper-soft);
  color: var(--ink-faint);
  font-size: 0.76rem;
}

.loading-state,
.empty-state {
  display: flex;
  align-items: center;
  justify-content: center;
  gap: 10px;
  min-height: 220px;
  padding: 32px;
  color: var(--ink-soft);
}

.spinner {
  width: 20px;
  height: 20px;
  border: 2px solid var(--line-strong);
  border-top-color: var(--teal);
  border-radius: 50%;
  animation: spin 700ms linear infinite;
}

@keyframes spin {
  to {
    transform: rotate(360deg);
  }
}

.empty-state {
  flex-direction: column;
  text-align: center;
}

.empty-state h3 {
  margin: 2px 0 0;
}

.empty-state p {
  max-width: 520px;
  color: var(--ink-soft);
}

.empty-icon {
  width: 54px;
  height: 54px;
  display: grid;
  place-items: center;
  border-radius: 16px;
  background: var(--brand-tint);
  color: var(--teal-dark);
  font-size: 1.5rem;
}

.dialog-backdrop {
  position: fixed;
  z-index: 80;
  inset: 0;
  display: grid;
  place-items: center;
  padding: 18px;
  background: rgba(8, 28, 22, 0.52);
  backdrop-filter: blur(4px);
}

.relay-dialog {
  width: min(680px, 100%);
  max-height: min(860px, calc(100vh - 36px));
  overflow: auto;
  border-radius: 20px;
  background: var(--paper);
  box-shadow: 0 28px 80px rgba(7, 28, 21, 0.28);
}

.detail-dialog {
  width: min(760px, 100%);
}

.release-dialog {
  width: min(820px, 100%);
}

.dialog-head,
.dialog-actions {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 20px;
  padding: 22px 24px;
}

.dialog-head {
  border-bottom: 1px solid var(--line);
}

.dialog-head h2 {
  margin-bottom: 6px;
  font-size: 1.25rem;
}

.dialog-head p {
  overflow-wrap: anywhere;
}

.close-button {
  width: 34px;
  height: 34px;
  border: 1px solid var(--line);
  border-radius: 10px;
  background: var(--paper-soft);
  color: var(--ink-soft);
  cursor: pointer;
  font-size: 1.25rem;
}

.dialog-body {
  padding: 24px;
}

.dialog-actions {
  align-items: center;
  justify-content: flex-end;
  border-top: 1px solid var(--line);
  background: var(--paper-soft);
}

.field,
.field span,
.field small {
  display: block;
}

.field span {
  margin-bottom: 8px;
  font-weight: 800;
}

.field input,
.field select,
.field textarea,
.upgrade-controls select {
  width: 100%;
  min-height: 44px;
  padding: 0 13px;
  border: 1px solid var(--line-strong);
  border-radius: 11px;
  background: var(--paper);
  color: var(--ink);
}

.field textarea {
  padding-block: 10px;
  resize: vertical;
}

.release-form-grid {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: 16px;
}

.release-form-wide {
  grid-column: 1 / -1;
}

.endpoint-editor {
  display: grid;
  grid-template-columns: minmax(150px, 0.55fr) minmax(0, 2fr) auto;
  align-items: end;
  gap: 12px;
  margin-top: 18px;
}

.upgrade-panel > p {
  color: var(--ink-soft);
  font-size: 0.84rem;
}

.upgrade-controls {
  display: grid;
  grid-template-columns: minmax(0, 1fr) auto;
  gap: 12px;
  margin-top: 12px;
}

.field small {
  margin-top: 7px;
  color: var(--ink-faint);
}

.relay-endpoint-fieldset {
  min-width: 0;
  margin: 20px 0 0;
  padding: 0;
  border: 0;
}

.relay-endpoint-fieldset legend {
  margin-bottom: 8px;
  padding: 0;
  font-weight: 800;
}

.relay-endpoint-fields {
  display: grid;
  grid-template-columns: minmax(96px, 0.55fr) minmax(0, 2fr) minmax(92px, 0.65fr);
  gap: 12px;
  align-items: end;
}

.relay-endpoint-fields--simple {
  grid-template-columns: minmax(150px, 0.55fr) minmax(0, 2fr);
}

.relay-endpoint-fields .field span {
  color: var(--ink-soft);
  font-size: 0.78rem;
}

.relay-endpoint-help {
  display: block;
  margin-top: 7px;
  color: var(--ink-faint);
  line-height: 1.5;
}

.field-pair {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: 14px;
  margin: 20px 0;
}

.one-time-warning {
  display: flex;
  gap: 6px;
  margin-bottom: 18px;
  padding: 13px 14px;
  border: 1px solid #efcf91;
  border-radius: 11px;
  background: #fff8e8;
  color: #7d5819;
  font-size: 0.88rem;
}

.secret-block,
.command-block {
  margin-top: 14px;
  padding: 15px;
  border: 1px solid var(--line);
  border-radius: 12px;
  background: var(--paper-soft);
}

.secret-block small {
  display: block;
  margin-top: 8px;
  color: var(--ink-faint);
}

.block-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  margin-bottom: 12px;
  color: var(--ink-soft);
  font-size: 0.78rem;
  font-weight: 800;
}

.block-actions {
  display: flex;
  flex-wrap: wrap;
  justify-content: flex-end;
  gap: 10px;
}

.secret-value {
  display: block;
  padding: 11px;
  border-radius: 9px;
  background: var(--night);
  color: #d7f7e9;
  user-select: all;
}

.enrollment-command {
  margin: 0;
  overflow-x: auto;
  color: #244c40;
  font-size: 0.78rem;
  line-height: 1.7;
  white-space: pre-wrap;
  user-select: all;
}

.enrollment-steps {
  display: grid;
  gap: 11px;
  margin: 20px 0 0;
  padding: 0;
  list-style: none;
}

.enrollment-steps li {
  display: flex;
  align-items: flex-start;
  gap: 10px;
  color: var(--ink-soft);
  font-size: 0.86rem;
  line-height: 1.6;
}

.enrollment-steps li > span {
  width: 23px;
  height: 23px;
  display: grid;
  flex: 0 0 auto;
  place-items: center;
  border-radius: 50%;
  background: var(--brand-tint);
  color: var(--teal-dark);
  font-size: 0.72rem;
  font-weight: 900;
}

.detail-status-row {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 16px;
}

.detail-status-row p {
  margin-top: 8px;
}

.metric-grid {
  display: grid;
  grid-template-columns: repeat(3, 1fr);
  gap: 10px;
  margin: 20px 0;
}

.metric-grid article {
  min-width: 0;
  padding: 15px;
  border: 1px solid var(--line);
  border-radius: 12px;
  background: var(--paper-soft);
}

.metric-grid strong {
  display: block;
  overflow: hidden;
  color: var(--night);
  font-size: 1.05rem;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.detail-section,
.activation-panel,
.danger-zone {
  margin-top: 18px;
  padding-top: 18px;
  border-top: 1px solid var(--line);
}

.detail-section h3,
.activation-panel h3 {
  margin-bottom: 13px;
  font-size: 0.96rem;
}

.detail-list {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: 0 22px;
  margin: 0;
}

.detail-list > div {
  min-width: 0;
  padding: 11px 0;
  border-bottom: 1px solid var(--line);
}

.detail-list dt {
  margin-bottom: 5px;
  color: var(--ink-faint);
  font-size: 0.73rem;
}

.detail-list dd {
  margin: 0;
  color: var(--ink);
  font-size: 0.84rem;
}

.activation-panel {
  display: grid;
  gap: 11px;
}

.activation-panel > label,
.check-grid label {
  display: flex;
  align-items: flex-start;
  gap: 8px;
  color: var(--ink-soft);
  font-size: 0.84rem;
}

.activation-panel input {
  width: 16px;
  height: 16px;
  margin-top: 2px;
  accent-color: var(--teal-dark);
}

.check-grid {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: 9px;
}

.activation-panel .relay-button {
  justify-self: start;
}

.danger-zone {
  padding: 17px;
  border: 1px solid #f0c7c4;
  border-radius: 12px;
  background: #fff9f8;
}

.danger-zone strong {
  color: #9d342f;
}

.danger-zone p {
  margin: 7px 0 12px;
}

.danger-zone > span {
  color: #9d342f;
  font-size: 0.84rem;
  font-weight: 700;
}

.toast {
  position: fixed;
  z-index: 120;
  right: 24px;
  bottom: 24px;
  max-width: min(420px, calc(100vw - 32px));
  padding: 12px 16px;
  border-radius: 11px;
  background: var(--night);
  color: white;
  box-shadow: var(--shadow);
  font-size: 0.86rem;
}

@media (max-width: 900px) {
  .relay-host-page {
    padding-top: 28px;
  }

  .page-head {
    align-items: flex-start;
    flex-direction: column;
  }

  .summary-bar {
    grid-template-columns: 1fr 1fr;
  }

  .summary-bar article:nth-child(2) {
    border-right: 0;
  }

  .summary-bar article:nth-child(-n + 2) {
    border-bottom: 1px solid var(--line);
  }

  .scheduler-metrics {
    grid-template-columns: 1fr 1fr;
  }

  .scheduler-endpoint {
    grid-column: 1 / -1;
  }

  table {
    min-width: 1050px;
  }
}

@media (max-width: 620px) {
  .relay-host-page {
    padding: 22px 14px 52px;
  }

  .page-actions,
  .page-actions .relay-button {
    width: 100%;
  }

  .summary-bar article {
    padding: 15px;
  }

  .panel-head,
  .panel-foot {
    align-items: flex-start;
    flex-direction: column;
  }

  .scheduler-grid {
    padding: 14px;
  }

  .scheduler-card-head {
    flex-direction: column;
  }

  .scheduler-card-head .relay-button {
    width: 100%;
  }

  .scheduler-metrics {
    grid-template-columns: 1fr;
  }

  .scheduler-endpoint {
    grid-column: auto;
  }

  .table-wrap {
    padding: 10px;
  }

  table,
  thead,
  tbody,
  tr,
  td {
    display: block;
    width: 100%;
  }

  table {
    min-width: 0;
  }

  thead {
    display: none;
  }

  tbody tr {
    margin-bottom: 10px;
    padding: 12px 14px;
    border: 1px solid var(--line);
    border-radius: 14px;
  }

  tbody tr:last-child {
    margin-bottom: 0;
  }

  td {
    display: grid;
    grid-template-columns: minmax(104px, 35%) 1fr;
    gap: 10px;
    padding: 9px 0;
    border-bottom: 1px solid var(--line);
    text-align: right;
  }

  td::before {
    content: attr(data-label);
    color: var(--ink-faint);
    font-size: 0.75rem;
    font-weight: 750;
    text-align: left;
  }

  td:first-child {
    display: block;
    padding-top: 2px;
    text-align: left;
  }

  td:first-child::before {
    display: none;
  }

  td:last-child {
    border-bottom: 0;
  }

  .endpoint {
    white-space: normal;
  }

  .dialog-backdrop {
    align-items: end;
    padding: 0;
  }

  .relay-dialog {
    max-height: 94vh;
    border-radius: 20px 20px 0 0;
  }

  .dialog-head,
  .dialog-body,
  .dialog-actions {
    padding: 18px;
  }

  .dialog-actions .relay-button {
    flex: 1;
  }

  .field-pair,
  .relay-endpoint-fields,
  .metric-grid,
  .detail-list,
  .check-grid,
  .release-form-grid,
  .endpoint-editor,
  .upgrade-controls {
    grid-template-columns: 1fr;
  }

  .release-form-wide {
    grid-column: auto;
  }

  .release-item {
    align-items: flex-start;
    flex-direction: column;
  }

  .detail-status-row {
    align-items: flex-start;
    flex-direction: column;
  }

  .one-time-warning {
    flex-direction: column;
  }
}
</style>
