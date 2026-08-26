<script setup lang="ts">
import { createSHA256 } from 'hash-wasm'
import { useHead } from '@unhead/vue'
import { computed, onMounted, ref } from 'vue'

import {
  createAdminRelease,
  deleteAdminRelease,
  getLatestGitHubRelease,
  getReleaseAccessKeySettings,
  getReleaseDeliverySettings,
  getReleaseSourceSettings,
  importReleaseAsset,
  importLatestMirrorRelease,
  listAdminReleases,
  publishAdminRelease,
  updateReleaseAccessKeySettings,
  updateReleaseDeliverySettings,
  updateReleaseSourceSettings,
  updateAdminRelease,
  uploadReleaseAsset,
  withdrawAdminRelease,
  type AdminRelease,
  type ReleaseAccessKeySettings,
  type ReleaseDeliverySettings,
  type ReleaseProject,
  type ReleaseSourceSettings,
  type SaveAdminReleaseAsset,
  type SaveAdminReleaseRequest,
  type StoredReleaseAsset,
} from '@/api/admin'
import { problemMessage } from '@/api/auth'
import { parsePortableAssetFileName, portableAssetMatchesRelease } from '@/utils/portableInstaller'

useHead({
  title: '软件版本管理｜WenzWork',
  meta: [{ name: 'robots', content: 'noindex, nofollow' }],
})

const releases = ref<AdminRelease[]>([])
const loading = ref(true)
const pending = ref(false)
const errorMessage = ref('')
const message = ref('')
const editingId = ref<string | null>(null)
const editingWasPublished = ref(false)
type ReleaseManagementTab = 'settings' | 'publish' | 'list'
const activeTab = ref<ReleaseManagementTab>('settings')
const managementTabs: { value: ReleaseManagementTab; label: string }[] = [
  { value: 'settings', label: '基础配置' },
  { value: 'publish', label: '版本发布' },
  { value: 'list', label: '版本列表' },
]
type ReleaseSourceEditor = {
  settings: ReleaseSourceSettings
  githubRepository: string
  githubToken: string
  clearGitHubToken: boolean
  mirrorBaseUrl: string
  pending: boolean
}
const projectOptions: { value: ReleaseProject; label: string }[] = [
  { value: 'web', label: 'Web / 服务端' },
  { value: 'desktop', label: '桌面端' },
  { value: 'mobile', label: '手机端' },
]
const sourceEditors = ref<ReleaseSourceEditor[]>([])
const sourcePending = computed(() => sourceEditors.value.some((source) => source.pending))
const deliverySettings = ref<ReleaseDeliverySettings | null>(null)
const deliveryMode = ref<'proxy_cached' | 's3_redirect' | 'github_redirect'>('proxy_cached')
const s3UrlPrefix = ref('')
const deliveryPending = ref(false)
const accessKeySettings = ref<ReleaseAccessKeySettings | null>(null)
const releaseAccessKey = ref('')
const releaseAccessKeyConfirmation = ref('')
const accessKeyVisible = ref(false)
const accessKeyPending = ref(false)
const githubPending = ref(false)
const mirrorPending = ref(false)
const releaseImportPending = computed(() => githubPending.value || mirrorPending.value)

const releaseProject = ref<ReleaseProject>('desktop')
const version = ref('')
const channel = ref<'stable' | 'beta'>('stable')
const title = ref('')
const summary = ref('')
const releaseNotes = ref('')
const status = ref<'draft' | 'published'>('draft')

type ReleaseAssetSource = 'remote' | 'local' | 'github' | 'mirror' | 'pushed'
type UploadPhase =
  'idle' | 'selected' | 'importing' | 'hashing' | 'uploading' | 'uploaded' | 'error'
type ReleaseAssetEditor = Omit<SaveAdminReleaseAsset, 'source' | 'objectKey'> & {
  source: ReleaseAssetSource
  objectKey: string
  sourceURL: string
  selectedFile?: File
  uploadPhase: UploadPhase
  uploadProgress: number
  uploadError: string
}

const assets = ref<ReleaseAssetEditor[]>([])
const assetsExpanded = ref(false)
const maximumUploadBytes = 5 * 1024 * 1024 * 1024

const emptyAsset = (): ReleaseAssetEditor => ({
  platform: 'windows',
  architecture: 'x64',
  fileName: '',
  fileSizeBytes: 0,
  sha256: '',
  signatureStatus: 'unknown',
  source: 'local',
  objectKey: '',
  sourceURL: '',
  downloadUrl: '',
  uploadPhase: 'idle',
  uploadProgress: 0,
  uploadError: '',
})

const isHTTPURL = (value: string) => {
  try {
    return ['http:', 'https:'].includes(new URL(value).protocol)
  } catch {
    return false
  }
}

const isDownloadPrefix = (value: string) => {
  try {
    const parsed = new URL(value)
    return (
      ['http:', 'https:'].includes(parsed.protocol) &&
      Boolean(parsed.hostname) &&
      !parsed.username &&
      !parsed.password &&
      !parsed.search &&
      !parsed.hash
    )
  } catch {
    return false
  }
}

const isMirrorBaseURL = (value: string) => value === '' || isDownloadPrefix(value)

const isGitHubRepository = (value: string) =>
  value.length <= 200 && /^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/.test(value) && !value.includes('..')

const selectedSourceEditor = computed(() =>
  sourceEditors.value.find((source) => source.settings.project === releaseProject.value),
)
const hasUnsavedSourceSettings = computed(() => {
  const source = selectedSourceEditor.value
  return (
    !source ||
    source.githubRepository.trim() !== source.settings.githubRepository ||
    source.githubToken.trim() !== '' ||
    source.clearGitHubToken ||
    source.mirrorBaseUrl.trim().replace(/\/+$/, '') !== source.settings.mirrorBaseUrl
  )
})

const assetIssue = (asset: ReleaseAssetEditor, index: number) => {
  const label = `文件 ${index + 1}`
  if (!asset.fileName.trim()) return `${label}缺少文件名。`
  const deploymentAsset = parsePortableAssetFileName(asset.fileName.trim())
  if (
    /^wenzwork-(?:host|relay|device-agent)-deployment-/i.test(asset.fileName.trim()) &&
    (!deploymentAsset ||
      releaseProject.value !== 'web' ||
      !portableAssetMatchesRelease(asset, version.value))
  ) {
    return `${label}的部署包版本、平台或架构与当前 Web 发布记录不一致。`
  }
  if (asset.fileSizeBytes <= 0) return `${label}的文件大小无效。`
  if (!/^[0-9a-fA-F]{64}$/.test(asset.sha256.trim())) return `${label}需要有效的 SHA-256。`
  if (asset.source === 'pushed') {
    if (asset.downloadUrl.trim()) return `${label}的本地推送引用不应包含外部下载地址。`
  } else if (!isHTTPURL(asset.downloadUrl.trim())) {
    return `${label}需要有效的 HTTP(S) 下载地址。`
  }
  if (asset.source === 'github') {
    if (!asset.objectKey.startsWith('github/')) return `${label}缺少有效的 GitHub Asset 引用。`
  } else if (asset.source === 'mirror') {
    if (!asset.objectKey.startsWith('mirror/')) return `${label}缺少有效的镜像安装包引用。`
  } else if (asset.source === 'pushed') {
    if (!asset.objectKey.startsWith('local/')) return `${label}缺少有效的本地推送引用。`
  } else if (!asset.objectKey.startsWith('releases/')) {
    return `${label}尚未转存到 S3。`
  }
  return ''
}

const releaseIssue = (targetStatus: 'draft' | 'published') => {
  if (!version.value.trim()) return '请先填写版本号。'
  if (!title.value.trim()) return '请先填写公告标题。'
  if (targetStatus === 'published' && assets.value.length === 0)
    return '发布版本至少需要一个安装文件。'
  const objectKeys = new Set<string>()
  for (const [index, asset] of assets.value.entries()) {
    const issue = assetIssue(asset, index)
    if (issue) return issue
    if (objectKeys.has(asset.objectKey)) return `文件 ${index + 1}与前面的文件引用重复。`
    objectKeys.add(asset.objectKey)
  }
  return ''
}

const submitIssue = computed(() => releaseIssue('published'))

const hasActiveUpload = computed(() =>
  assets.value.some(
    (asset) =>
      asset.uploadPhase === 'importing' ||
      asset.uploadPhase === 'hashing' ||
      asset.uploadPhase === 'uploading',
  ),
)

const formatDate = (value: string) =>
  new Intl.DateTimeFormat('zh-CN', { dateStyle: 'medium', timeStyle: 'short' }).format(
    new Date(value),
  )

const formatBytes = (bytes: number) => {
  if (bytes < 1024) return `${bytes} B`
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`
  if (bytes < 1024 * 1024 * 1024) return `${(bytes / 1024 / 1024).toFixed(1)} MB`
  return `${(bytes / 1024 / 1024 / 1024).toFixed(2)} GB`
}

const storedAssetStatusLabel = (asset: ReleaseAssetEditor) => {
  if (asset.source === 'github') return 'GitHub 已关联'
  if (asset.source === 'mirror') return '镜像引用已关联'
  if (asset.source === 'pushed') return '本地推送已就绪'
  return 'S3 已就绪'
}

const storedAssetURLLabel = (asset: ReleaseAssetEditor) => {
  if (asset.source === 'github') return 'GitHub Release 链接'
  if (asset.source === 'mirror') return '镜像站下载链接'
  if (asset.source === 'pushed') return '本地推送存储'
  return 'S3 存储链接'
}

const statusLabel = (value: AdminRelease['status']) =>
  ({ draft: '草稿', published: '已发布', withdrawn: '已下架' })[value]
const projectLabel = (value: ReleaseProject) =>
  projectOptions.find((project) => project.value === value)?.label ?? value

const load = async () => {
  loading.value = true
  errorMessage.value = ''
  try {
    const [releaseItems, delivery, sources, keySettings] = await Promise.all([
      listAdminReleases(),
      getReleaseDeliverySettings(),
      getReleaseSourceSettings(),
      getReleaseAccessKeySettings(),
    ])
    releases.value = releaseItems
    deliverySettings.value = delivery
    deliveryMode.value = delivery.downloadMode
    s3UrlPrefix.value = delivery.s3UrlPrefix
    sourceEditors.value = sources.map((settings) => ({
      settings,
      githubRepository: settings.githubRepository,
      githubToken: '',
      clearGitHubToken: false,
      mirrorBaseUrl: settings.mirrorBaseUrl,
      pending: false,
    }))
    accessKeySettings.value = keySettings
  } catch (error) {
    errorMessage.value = problemMessage(error, '暂时无法读取软件版本。')
  } finally {
    loading.value = false
  }
}

const resetForm = () => {
  editingId.value = null
  editingWasPublished.value = false
  releaseProject.value = 'desktop'
  version.value = ''
  channel.value = 'stable'
  title.value = ''
  summary.value = ''
  releaseNotes.value = ''
  status.value = 'draft'
  assets.value = []
  assetsExpanded.value = false
}

const edit = (release: AdminRelease) => {
  editingId.value = release.id
  editingWasPublished.value = release.status === 'published'
  releaseProject.value = release.project
  version.value = release.version
  channel.value = release.channel
  title.value = release.title
  summary.value = release.summary
  releaseNotes.value = release.releaseNotes
  status.value = release.status === 'published' ? 'published' : 'draft'
  assets.value = release.assets.map((asset) => {
    return {
      platform: asset.platform,
      architecture: asset.architecture,
      fileName: asset.fileName,
      fileSizeBytes: asset.fileSizeBytes,
      sha256: asset.sha256,
      signatureStatus: asset.signatureStatus,
      source:
        asset.source === 'github'
          ? ('github' as const)
          : asset.source === 'mirror'
            ? ('mirror' as const)
            : asset.source === 'local'
              ? ('pushed' as const)
              : ('local' as const),
      objectKey: asset.objectKey ?? '',
      sourceURL: '',
      downloadUrl: asset.downloadUrl,
      uploadPhase: 'uploaded' as const,
      uploadProgress: 100,
      uploadError: '',
    }
  })
  assetsExpanded.value = false
  activeTab.value = 'publish'
  window.scrollTo({ top: 0, behavior: 'smooth' })
}

const addAsset = () => {
  assets.value.push(emptyAsset())
  assetsExpanded.value = true
}
const removeAsset = (index: number) => assets.value.splice(index, 1)

const switchAssetSource = (asset: ReleaseAssetEditor, source: ReleaseAssetSource) => {
  if (
    asset.source === source ||
    asset.uploadPhase === 'importing' ||
    asset.uploadPhase === 'hashing' ||
    asset.uploadPhase === 'uploading'
  )
    return
  asset.source = source
  asset.fileName = ''
  asset.fileSizeBytes = 0
  asset.sha256 = ''
  asset.downloadUrl = ''
  asset.objectKey = ''
  asset.sourceURL = ''
  asset.selectedFile = undefined
  asset.uploadPhase = 'idle'
  asset.uploadProgress = 0
  asset.uploadError = ''
}

const selectUploadFile = (asset: ReleaseAssetEditor, event: Event) => {
  const input = event.target as HTMLInputElement
  const file = input.files?.[0]
  asset.uploadError = ''
  if (!file) {
    asset.selectedFile = undefined
    asset.uploadPhase = 'idle'
    return
  }
  if (file.size < 1 || file.size > maximumUploadBytes) {
    asset.selectedFile = undefined
    asset.uploadPhase = 'error'
    asset.uploadError = '安装文件必须大于 0 字节且不能超过 5 GB。'
    input.value = ''
    return
  }
  asset.selectedFile = file
  asset.fileName = file.name
  asset.fileSizeBytes = file.size
  asset.sha256 = ''
  asset.downloadUrl = ''
  asset.objectKey = ''
  asset.uploadPhase = 'selected'
  asset.uploadProgress = 0
}

const hashFile = async (file: File, onProgress: (progress: number) => void) => {
  const hasher = await createSHA256()
  const chunkSize = 4 * 1024 * 1024
  for (let offset = 0; offset < file.size; offset += chunkSize) {
    const chunk = new Uint8Array(await file.slice(offset, offset + chunkSize).arrayBuffer())
    hasher.update(chunk)
    onProgress(Math.round((Math.min(offset + chunkSize, file.size) / file.size) * 100))
  }
  return hasher.digest()
}

const applyStoredAsset = (asset: ReleaseAssetEditor, stored: StoredReleaseAsset) => {
  if (!stored.fileName && !asset.fileName) throw new Error('服务端没有返回安装文件名。')
  asset.fileName = stored.fileName ?? asset.fileName
  asset.fileSizeBytes = stored.fileSizeBytes
  asset.sha256 = stored.sha256
  asset.objectKey = stored.objectKey
  asset.downloadUrl = stored.downloadUrl
  if (stored.platform) asset.platform = stored.platform
  if (stored.architecture) asset.architecture = stored.architecture
  asset.uploadPhase = 'uploaded'
  asset.uploadProgress = 100
}

const uploadAsset = async (asset: ReleaseAssetEditor) => {
  const file = asset.selectedFile
  if (!version.value.trim()) {
    asset.uploadError = '请先填写版本号，再上传安装文件。'
    return
  }
  if (!file) {
    asset.uploadError = '请先选择要上传的安装文件。'
    return
  }
  asset.uploadError = ''
  try {
    asset.uploadPhase = 'hashing'
    asset.uploadProgress = 0
    const sha256 = await hashFile(file, (progress) => (asset.uploadProgress = progress))
    asset.sha256 = sha256
    asset.uploadPhase = 'uploading'
    asset.uploadProgress = 0
    const upload = await uploadReleaseAsset(
      {
        version: version.value.trim(),
        platform: asset.platform,
        architecture: asset.architecture,
        fileName: file.name,
        fileSizeBytes: file.size,
        sha256,
      },
      file,
      (progress) => (asset.uploadProgress = progress),
    )
    applyStoredAsset(asset, upload)
    message.value = `${file.name} 已上传，请保存版本以写入发布记录。`
  } catch (error) {
    asset.uploadPhase = 'error'
    asset.uploadError = problemMessage(
      error,
      error instanceof Error ? error.message : '安装文件上传失败。',
    )
  }
}

const importAsset = async (asset: ReleaseAssetEditor) => {
  if (!version.value.trim()) {
    asset.uploadError = '请先填写版本号，再导入安装文件。'
    return
  }
  if (!isHTTPURL(asset.sourceURL.trim())) {
    asset.uploadError = '请填写有效的 HTTP(S) 外链地址。'
    return
  }
  asset.uploadError = ''
  asset.uploadPhase = 'importing'
  asset.uploadProgress = 0
  try {
    const stored = await importReleaseAsset({
      version: version.value.trim(),
      platform: asset.platform,
      architecture: asset.architecture,
      downloadUrl: asset.sourceURL.trim(),
    })
    applyStoredAsset(asset, stored)
    message.value = `${asset.fileName} 已从外链检测并转存到 S3。`
  } catch (error) {
    asset.uploadPhase = 'error'
    asset.uploadError = problemMessage(error, '外链安装文件检测或转存失败。')
  }
}

const saveDeliverySettings = async () => {
  if (!deliverySettings.value) return
  if (deliveryMode.value === 's3_redirect' && !isDownloadPrefix(s3UrlPrefix.value.trim())) {
    errorMessage.value = 'S3 直链模式需要有效的 HTTP(S) 链接前缀。'
    return
  }
  deliveryPending.value = true
  errorMessage.value = ''
  message.value = ''
  try {
    const settings = await updateReleaseDeliverySettings({
      downloadMode: deliveryMode.value,
      s3UrlPrefix: s3UrlPrefix.value.trim(),
      expectedVersion: deliverySettings.value.version,
    })
    deliverySettings.value = settings
    deliveryMode.value = settings.downloadMode
    s3UrlPrefix.value = settings.s3UrlPrefix
    message.value =
      settings.downloadMode === 'proxy_cached'
        ? '下载方式已切换为直链；缓存缺失时会从 S3、GitHub 或镜像站自动拉取。'
        : settings.downloadMode === 's3_redirect'
          ? '下载方式已切换为 S3 链接。'
          : '下载方式已切换为 GitHub 链接；私有资产由服务端使用 Token 换取临时地址。'
  } catch (error) {
    errorMessage.value = problemMessage(error, '暂时无法保存安装包下载设置。')
  } finally {
    deliveryPending.value = false
  }
}

const validReleaseAccessKey = (value: string) => /^release_[A-Za-z0-9_-]{43}$/.test(value.trim())

const generateReleaseAccessKey = () => {
  const bytes = new Uint8Array(32)
  window.crypto.getRandomValues(bytes)
  let binary = ''
  for (const byte of bytes) binary += String.fromCharCode(byte)
  const key = `release_${window.btoa(binary).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '')}`
  releaseAccessKey.value = key
  releaseAccessKeyConfirmation.value = key
  accessKeyVisible.value = true
  errorMessage.value = ''
  message.value = '已在浏览器中生成新密钥；保存前请复制到构建环境，保存后系统无法取回明文。'
}

const saveReleaseAccessKey = async () => {
  if (!accessKeySettings.value) return
  const accessKey = releaseAccessKey.value.trim()
  if (!validReleaseAccessKey(accessKey)) {
    errorMessage.value = 'Release Access Key 必须以 release_ 开头并包含 43 位 URL 安全字符。'
    return
  }
  if (accessKey !== releaseAccessKeyConfirmation.value.trim()) {
    errorMessage.value = '两次输入的 Release Access Key 不一致。'
    return
  }
  if (
    !window.confirm(
      '确认更新 Release Access Key？旧密钥会立即失效，请确保所有构建环境已经准备使用新密钥。',
    )
  )
    return

  accessKeyPending.value = true
  errorMessage.value = ''
  message.value = ''
  try {
    accessKeySettings.value = await updateReleaseAccessKeySettings({
      accessKey,
      expectedVersion: accessKeySettings.value.version,
    })
    releaseAccessKey.value = ''
    releaseAccessKeyConfirmation.value = ''
    accessKeyVisible.value = false
    message.value = 'Release Access Key 已更新并立即生效；旧密钥已失效。'
  } catch (error) {
    errorMessage.value = problemMessage(error, '暂时无法保存 Release Access Key。')
  } finally {
    accessKeyPending.value = false
  }
}

const saveSourceSettings = async (source: ReleaseSourceEditor) => {
  const repository = source.githubRepository.trim()
  if (!isGitHubRepository(repository)) {
    errorMessage.value = 'GitHub 仓库必须使用 owner/repository 格式。'
    return
  }
  const token = source.githubToken.trim()
  if (token.length > 1000 || /\s/.test(token)) {
    errorMessage.value = 'GitHub Token 不能包含空白字符，且长度不能超过 1000 个字符。'
    return
  }
  if (token && source.clearGitHubToken) {
    errorMessage.value = '不能同时替换和清除 GitHub Token。'
    return
  }
  const mirrorBaseUrl = source.mirrorBaseUrl.trim().replace(/\/+$/, '')
  if (!isMirrorBaseURL(mirrorBaseUrl)) {
    errorMessage.value = '镜像站必须是无凭据、查询参数和片段的 HTTP(S) 地址。'
    return
  }
  source.pending = true
  errorMessage.value = ''
  message.value = ''
  try {
    const request = {
      project: source.settings.project,
      githubRepository: repository,
      clearGithubToken: source.clearGitHubToken,
      mirrorBaseUrl,
      expectedVersion: source.settings.version,
      ...(token ? { githubToken: token } : {}),
    }
    const settings = await updateReleaseSourceSettings(request)
    source.settings = settings
    source.githubRepository = settings.githubRepository
    source.githubToken = ''
    source.clearGitHubToken = false
    source.mirrorBaseUrl = settings.mirrorBaseUrl
    message.value = `Release 来源已更新：GitHub ${settings.githubRepository}，Token ${
      settings.githubTokenConfigured ? '已加密保存' : '未配置'
    }，镜像站${settings.mirrorBaseUrl ? `为 ${settings.mirrorBaseUrl}` : '未配置'}；后续查询立即使用新配置。`
  } catch (error) {
    errorMessage.value = problemMessage(error, '暂时无法保存 Release 来源设置。')
  } finally {
    source.pending = false
  }
}

const pullLatestMirrorRelease = async () => {
  const source = selectedSourceEditor.value
  if (!source || hasUnsavedSourceSettings.value) {
    errorMessage.value = '请先保存镜像站设置，再拉取最新版本。'
    return
  }
  if (!source.settings.mirrorBaseUrl) {
    errorMessage.value = '请先为当前项目配置镜像站地址。'
    return
  }
  const hasExistingContent =
    Boolean(version.value || title.value || summary.value || releaseNotes.value) ||
    assets.value.length > 0
  if (
    hasExistingContent &&
    !window.confirm('从镜像站拉取最新版本会替换当前尚未保存的版本表单，是否继续？')
  )
    return

  mirrorPending.value = true
  errorMessage.value = ''
  message.value = `正在从 ${source.settings.mirrorBaseUrl} 读取版本与安装包链接…`
  try {
    const latest = await importLatestMirrorRelease(releaseProject.value)
    if (latest.project !== source.settings.project || latest.assets.length === 0) {
      throw new Error('镜像站返回的项目或安装文件无效。')
    }

    editingId.value = null
    editingWasPublished.value = false
    releaseProject.value = latest.project
    version.value = latest.version.slice(0, 50)
    channel.value = latest.channel
    title.value = latest.title.slice(0, 120)
    summary.value = latest.summary.slice(0, 1000)
    releaseNotes.value = latest.releaseNotes.slice(0, 50000)
    status.value = 'draft'
    assets.value = latest.assets.map((candidate) => ({
      platform: candidate.platform,
      architecture: candidate.architecture,
      fileName: candidate.fileName,
      fileSizeBytes: candidate.fileSizeBytes,
      sha256: candidate.sha256,
      signatureStatus: candidate.signatureStatus,
      source: 'mirror' as const,
      objectKey: candidate.objectKey,
      sourceURL: candidate.downloadUrl,
      downloadUrl: candidate.downloadUrl,
      uploadPhase: 'uploaded' as const,
      uploadProgress: 100,
      uploadError: '',
    }))
    assetsExpanded.value = false
    message.value = `已从镜像站读取 ${latest.version} 并关联 ${latest.assets.length} 个安装包；首次下载会按大小和 SHA-256 校验后直接写入本机缓存。`
    window.scrollTo({ top: 0, behavior: 'smooth' })
  } catch (error) {
    errorMessage.value = problemMessage(
      error,
      error instanceof Error ? error.message : '暂时无法从镜像站拉取版本。',
    )
    message.value = ''
  } finally {
    mirrorPending.value = false
  }
}

const importLatestGitHubRelease = async () => {
  const source = selectedSourceEditor.value
  if (!source || hasUnsavedSourceSettings.value) {
    errorMessage.value = '请先保存 GitHub 仓库设置，再导入最新 Release。'
    return
  }
  const hasExistingContent =
    Boolean(version.value || title.value || summary.value || releaseNotes.value) ||
    assets.value.length > 0
  if (
    hasExistingContent &&
    !window.confirm('读取 GitHub 最新 Release 会替换当前尚未保存的版本表单，是否继续？')
  )
    return

  githubPending.value = true
  errorMessage.value = ''
  message.value = `正在读取 ${source.settings.githubRepository} 的最新 Release…`
  try {
    const latest = await getLatestGitHubRelease(releaseProject.value)
    const candidates = latest.assets.filter(
      (candidate) =>
        Boolean(candidate.platform && candidate.architecture) &&
        /^[0-9a-f]{64}$/i.test(candidate.sha256),
    )
    if (candidates.length === 0) {
      throw new Error('GitHub Release 中没有可识别平台和架构的安装文件。')
    }

    editingId.value = null
    editingWasPublished.value = false
    releaseProject.value = source.settings.project
    version.value = latest.version.slice(0, 50)
    channel.value = latest.prerelease ? 'beta' : 'stable'
    title.value = latest.name.slice(0, 120)
    summary.value = latest.summary.slice(0, 1000)
    releaseNotes.value = latest.body.slice(0, 50000)
    status.value = 'draft'
    assets.value = []

    for (const candidate of candidates) {
      const asset = emptyAsset()
      asset.source = 'github'
      asset.platform = candidate.platform!
      asset.architecture = candidate.architecture!
      asset.sourceURL = candidate.downloadUrl
      asset.fileName = candidate.fileName
      asset.fileSizeBytes = candidate.fileSizeBytes
      asset.sha256 = candidate.sha256
      asset.objectKey = candidate.objectKey
      asset.downloadUrl = candidate.downloadUrl
      asset.uploadPhase = 'uploaded'
      asset.uploadProgress = 100
      assets.value.push(asset)
    }
    assetsExpanded.value = false
    message.value = `已读取 ${latest.tagName} 并关联 ${candidates.length} 个 GitHub 安装包；直链模式会在首次下载时自动缓存。`
    window.scrollTo({ top: 0, behavior: 'smooth' })
  } catch (error) {
    errorMessage.value = problemMessage(
      error,
      error instanceof Error ? error.message : '暂时无法导入 GitHub Release。',
    )
    message.value = ''
  } finally {
    githubPending.value = false
  }
}

const save = async (publishNow = true) => {
  const targetStatus: 'draft' | 'published' =
    editingWasPublished.value || publishNow ? 'published' : 'draft'
  const issue = releaseIssue(targetStatus)
  if (issue) {
    errorMessage.value = issue
    return
  }
  status.value = targetStatus
  pending.value = true
  errorMessage.value = ''
  message.value = ''
  const request: SaveAdminReleaseRequest = {
    project: releaseProject.value,
    version: version.value.trim(),
    channel: channel.value,
    title: title.value.trim(),
    summary: summary.value.trim(),
    releaseNotes: releaseNotes.value.trim(),
    status: status.value,
    assets: assets.value.map((asset) => ({
      platform: asset.platform,
      architecture: asset.architecture,
      fileName: asset.fileName.trim(),
      fileSizeBytes: asset.fileSizeBytes,
      sha256: asset.sha256.trim().toLowerCase(),
      signatureStatus: asset.signatureStatus,
      source:
        asset.source === 'github'
          ? 'github'
          : asset.source === 'mirror'
            ? 'mirror'
            : asset.source === 'pushed'
              ? 'local'
              : 's3',
      objectKey: asset.objectKey,
      downloadUrl: asset.downloadUrl.trim(),
    })),
  }
  try {
    if (editingId.value) {
      await updateAdminRelease(editingId.value, request)
      message.value = '软件版本与更新公告已保存。'
    } else {
      await createAdminRelease(request)
      message.value = status.value === 'published' ? '软件版本已发布。' : '软件版本草稿已创建。'
    }
    resetForm()
    await load()
    activeTab.value = 'list'
  } catch (error) {
    errorMessage.value = problemMessage(error, '暂时无法保存软件版本。')
  } finally {
    pending.value = false
  }
}

const withdraw = async (release: AdminRelease) => {
  if (!window.confirm(`下架版本 ${release.version}？官网将不再展示该版本及其下载文件。`)) return
  pending.value = true
  errorMessage.value = ''
  message.value = ''
  try {
    await withdrawAdminRelease(release.id)
    if (editingId.value === release.id) resetForm()
    message.value = `版本 ${release.version} 已下架。`
    await load()
  } catch (error) {
    errorMessage.value = problemMessage(error, '暂时无法下架软件版本。')
  } finally {
    pending.value = false
  }
}

const removeRelease = async (release: AdminRelease) => {
  const publishedWarning =
    release.status === 'published' ? '该版本当前已发布，删除后官网会立即停止展示。\n\n' : ''
  if (
    !window.confirm(
      `永久删除版本 ${release.version}？\n\n${publishedWarning}系统会删除版本及安装文件关联记录，但不会删除 S3、GitHub、镜像站原始文件和已有本地缓存。此操作不可恢复。`,
    )
  )
    return
  pending.value = true
  errorMessage.value = ''
  message.value = ''
  try {
    await deleteAdminRelease(release.id)
    if (editingId.value === release.id) resetForm()
    message.value = `版本 ${release.version} 已永久删除。`
    await load()
  } catch (error) {
    errorMessage.value = problemMessage(error, '暂时无法删除软件版本。')
  } finally {
    pending.value = false
  }
}

const publish = async (release: AdminRelease) => {
  if (!window.confirm(`确认发布版本 ${release.version}？发布后官网会立即展示该版本及其下载文件。`))
    return
  pending.value = true
  errorMessage.value = ''
  message.value = ''
  try {
    await publishAdminRelease(release.id)
    message.value = `版本 ${release.version} 已确认发布。`
    await load()
  } catch (error) {
    errorMessage.value = problemMessage(error, '暂时无法发布软件版本。')
  } finally {
    pending.value = false
  }
}

onMounted(load)
</script>

<template>
  <section class="dashboard-page admin-wide-page">
    <p class="section-kicker">发布中心</p>
    <h1>软件版本管理</h1>
    <p class="dashboard-lead">
      维护安装文件列表与更新公告；草稿不会出现在官网，发布后立即进入下载页。
    </p>
    <p v-if="errorMessage" class="form-message form-error" role="alert">{{ errorMessage }}</p>
    <p v-if="message" class="form-message form-success" role="status">{{ message }}</p>

    <nav class="release-management-tabs" role="tablist" aria-label="软件版本管理">
      <button
        v-for="tab in managementTabs"
        :id="`release-management-tab-${tab.value}`"
        :key="tab.value"
        type="button"
        role="tab"
        :aria-selected="activeTab === tab.value"
        :aria-controls="`release-management-panel-${tab.value}`"
        :class="{ active: activeTab === tab.value }"
        @click="activeTab = tab.value"
      >
        {{ tab.label }}
      </button>
    </nav>

    <section
      id="release-management-panel-settings"
      v-show="activeTab === 'settings'"
      class="release-tab-panel release-settings-panel"
      role="tabpanel"
      aria-labelledby="release-management-tab-settings"
    >
      <section v-if="sourceEditors.length" class="dashboard-card release-source-card">
        <div class="batch-form-heading">
          <div>
            <p class="card-label">Release 来源</p>
            <h2>三类项目 GitHub 与镜像站</h2>
          </div>
        </div>
        <p class="field-hint">
          每类项目分别保存 GitHub 仓库、访问 Token 和可选镜像站；Token
          使用系统主密钥加密，明文不会通过 API 返回。镜像站须运行 WenzWork 公开版本 API；
          安装包链接会在首次下载时直接写入本机缓存，无需先转存 S3。
        </p>
        <div class="release-source-grid">
          <form
            v-for="source in sourceEditors"
            :key="source.settings.project"
            class="release-source-project"
            @submit.prevent="saveSourceSettings(source)"
          >
            <div class="batch-title-row">
              <h3>{{ projectLabel(source.settings.project) }}</h3>
              <span :class="['tag', { 'tag-muted': !source.settings.githubTokenConfigured }]">
                Token {{ source.settings.githubTokenConfigured ? '已配置' : '未配置' }}
              </span>
              <span class="tag tag-muted">配置版本 {{ source.settings.version }}</span>
            </div>
            <div class="field-group">
              <label :for="`release-github-repository-${source.settings.project}`">仓库地址</label>
              <input
                :id="`release-github-repository-${source.settings.project}`"
                v-model.trim="source.githubRepository"
                required
                maxlength="200"
                autocomplete="off"
                spellcheck="false"
                placeholder="owner/repository"
              />
            </div>
            <div class="field-group">
              <label :for="`release-github-token-${source.settings.project}`">访问 Token</label>
              <input
                :id="`release-github-token-${source.settings.project}`"
                v-model="source.githubToken"
                type="password"
                maxlength="1000"
                autocomplete="new-password"
                spellcheck="false"
                :disabled="source.clearGitHubToken"
                :placeholder="
                  source.settings.githubTokenConfigured
                    ? '留空则保留当前 Token'
                    : '公开仓库可留空；私有仓库需要 Contents 只读 Token'
                "
              />
            </div>
            <div class="field-group">
              <label :for="`release-mirror-url-${source.settings.project}`"
                >镜像站地址（可选）</label
              >
              <input
                :id="`release-mirror-url-${source.settings.project}`"
                v-model.trim="source.mirrorBaseUrl"
                type="url"
                maxlength="2048"
                autocomplete="off"
                spellcheck="false"
                placeholder="https://mirror.example.com"
              />
              <small
                >直接查询该站的公开版本目录并保存受控链接；安装包首次下载时校验并写入本机缓存，不经过
                S3。</small
              >
            </div>
            <div
              v-if="source.settings.githubTokenConfigured"
              class="field-group checkbox-field release-token-clear"
            >
              <label>
                <input
                  v-model="source.clearGitHubToken"
                  type="checkbox"
                  @change="source.githubToken = ''"
                />
                清除已保存的 Token
              </label>
            </div>
            <button class="button button-secondary" type="submit" :disabled="source.pending">
              {{
                source.pending ? '正在保存…' : `保存${projectLabel(source.settings.project)}来源`
              }}
            </button>
          </form>
        </div>
      </section>

      <form
        v-if="deliverySettings"
        class="dashboard-card release-delivery-card"
        @submit.prevent="saveDeliverySettings"
      >
        <div class="batch-form-heading">
          <div>
            <p class="card-label">下载方式</p>
            <h2>安装包交付策略</h2>
          </div>
          <span class="tag tag-muted">配置版本 {{ deliverySettings.version }}</span>
        </div>
        <div class="asset-source-switch" role="group" aria-label="安装包下载方式">
          <button
            type="button"
            :class="['asset-source-option', { active: deliveryMode === 'proxy_cached' }]"
            :aria-pressed="deliveryMode === 'proxy_cached'"
            @click="deliveryMode = 'proxy_cached'"
          >
            <strong>直链</strong>
            <small>主机中转；缓存缺失时从资产所属的 S3、GitHub 或镜像站拉取并校验</small>
          </button>
          <button
            type="button"
            :class="['asset-source-option', { active: deliveryMode === 's3_redirect' }]"
            :aria-pressed="deliveryMode === 's3_redirect'"
            @click="deliveryMode = 's3_redirect'"
          >
            <strong>S3 链接</strong>
            <small>下载请求直接跳转到 S3、OSS 或 CDN 的公开链接</small>
          </button>
          <button
            type="button"
            :class="['asset-source-option', { active: deliveryMode === 'github_redirect' }]"
            :aria-pressed="deliveryMode === 'github_redirect'"
            @click="deliveryMode = 'github_redirect'"
          >
            <strong>GitHub 链接</strong>
            <small>服务端携带 Token 换取临时下载地址，不向浏览器暴露 Token</small>
          </button>
        </div>
        <p class="field-hint">
          镜像站资产为避免暴露远端链接并确保完整性，始终通过主机缓存交付，不受 S3 或 GitHub
          跳转模式影响。
        </p>
        <div v-if="deliveryMode === 's3_redirect'" class="field-group">
          <label for="release-s3-url-prefix">S3 下载链接前缀</label>
          <input
            id="release-s3-url-prefix"
            v-model.trim="s3UrlPrefix"
            type="url"
            required
            placeholder="https://download.example.com/wenzwork-releases"
          />
          <small>系统会在该前缀后自动拼接安装包的 S3 object key。</small>
        </div>
        <p v-if="deliveryMode === 'github_redirect'" class="field-hint">
          仅适用于 GitHub Release 资产。公开和私有仓库都通过 GitHub Asset API
          解析下载地址；私有仓库使用上方加密保存的 Token。
        </p>
        <button class="button button-secondary" type="submit" :disabled="deliveryPending">
          {{ deliveryPending ? '正在保存…' : '保存下载设置' }}
        </button>
      </form>

      <form
        v-if="accessKeySettings"
        class="dashboard-card release-access-key-card"
        @submit.prevent="saveReleaseAccessKey"
      >
        <div class="batch-form-heading">
          <div>
            <p class="card-label">构建推送鉴权</p>
            <h2>Release Access Key</h2>
          </div>
          <div class="batch-title-row">
            <span :class="['tag', { 'tag-muted': !accessKeySettings.accessKeyConfigured }]">
              {{ accessKeySettings.accessKeyConfigured ? '已配置' : '未配置' }}
            </span>
            <span class="tag tag-muted">配置版本 {{ accessKeySettings.version }}</span>
          </div>
        </div>
        <p class="field-hint">
          本地构建脚本通过该密钥调用 Release 推送接口。系统只在数据库中保存 SHA-256
          摘要，查询接口不会返回明文；更新后旧密钥立即失效，无需重启服务。
        </p>
        <p v-if="accessKeySettings.accessKeyConfigured" class="field-hint">
          当前密钥前缀：<code>{{ accessKeySettings.keyPrefix }}…</code>
        </p>
        <div class="form-grid release-access-key-grid">
          <div class="field-group field-wide">
            <label for="release-access-key">新 Release Access Key</label>
            <input
              id="release-access-key"
              v-model="releaseAccessKey"
              :type="accessKeyVisible ? 'text' : 'password'"
              minlength="51"
              maxlength="51"
              autocomplete="new-password"
              spellcheck="false"
              placeholder="release_…"
            />
          </div>
          <div class="field-group field-wide">
            <label for="release-access-key-confirmation">再次输入新密钥</label>
            <input
              id="release-access-key-confirmation"
              v-model="releaseAccessKeyConfirmation"
              :type="accessKeyVisible ? 'text' : 'password'"
              minlength="51"
              maxlength="51"
              autocomplete="new-password"
              spellcheck="false"
              placeholder="再次输入相同密钥"
            />
          </div>
        </div>
        <label class="checkbox-field release-access-key-visibility">
          <input v-model="accessKeyVisible" type="checkbox" />
          显示正在输入的新密钥
        </label>
        <div class="admin-row-actions release-access-key-actions">
          <button class="button button-secondary" type="button" @click="generateReleaseAccessKey">
            生成安全密钥
          </button>
          <button
            class="button"
            type="submit"
            :disabled="accessKeyPending || !releaseAccessKey || !releaseAccessKeyConfirmation"
          >
            {{ accessKeyPending ? '正在更新…' : '更新并立即生效' }}
          </button>
        </div>
      </form>
    </section>

    <form
      id="release-management-panel-publish"
      v-show="activeTab === 'publish'"
      class="dashboard-card release-editor release-tab-panel"
      role="tabpanel"
      aria-labelledby="release-management-tab-publish"
      @submit.prevent="save(true)"
    >
      <div class="batch-form-heading">
        <div>
          <p class="card-label">{{ editingId ? '编辑版本' : '新版本' }}</p>
          <h2>{{ editingId ? `编辑 ${version}` : '创建软件版本' }}</h2>
        </div>
        <div class="admin-row-actions">
          <button
            class="button button-secondary"
            type="button"
            :disabled="
              releaseImportPending || sourcePending || hasActiveUpload || hasUnsavedSourceSettings
            "
            @click="importLatestGitHubRelease"
          >
            {{
              githubPending ? '正在读取 GitHub…' : `读取${projectLabel(releaseProject)}最新 Release`
            }}
          </button>
          <button
            class="button button-secondary"
            type="button"
            :disabled="
              releaseImportPending ||
              sourcePending ||
              hasActiveUpload ||
              hasUnsavedSourceSettings ||
              !selectedSourceEditor?.settings.mirrorBaseUrl
            "
            @click="pullLatestMirrorRelease"
          >
            {{
              mirrorPending
                ? '正在读取镜像版本…'
                : `从镜像拉取${projectLabel(releaseProject)}最新版本`
            }}
          </button>
          <button
            v-if="!editingWasPublished"
            class="button button-secondary"
            type="button"
            :disabled="pending || releaseImportPending || hasActiveUpload"
            @click="save(false)"
          >
            {{ pending ? '正在保存…' : '保存草稿' }}
          </button>
          <button
            class="button"
            type="submit"
            :disabled="pending || releaseImportPending || hasActiveUpload"
          >
            {{ pending ? '正在保存…' : editingWasPublished ? '保存已发布版本' : '发布版本' }}
          </button>
          <button
            v-if="editingId"
            class="text-button"
            type="button"
            :disabled="releaseImportPending || hasActiveUpload"
            @click="resetForm"
          >
            取消编辑
          </button>
        </div>
      </div>

      <div class="form-grid">
        <div class="field-group">
          <label for="release-project">项目类型</label>
          <select id="release-project" v-model="releaseProject" :disabled="Boolean(editingId)">
            <option v-for="project in projectOptions" :key="project.value" :value="project.value">
              {{ project.label }}
            </option>
          </select>
          <small>版本号只需在同一项目内唯一；编辑既有版本时不能切换项目。</small>
        </div>
        <div class="field-group">
          <label for="release-version">版本号</label>
          <input
            id="release-version"
            v-model.trim="version"
            required
            maxlength="50"
            placeholder="例如：1.4.0"
          />
        </div>
        <div class="field-group">
          <label for="release-channel">发布通道</label>
          <select id="release-channel" v-model="channel">
            <option value="stable">稳定版</option>
            <option value="beta">测试版</option>
          </select>
        </div>
        <div class="field-group field-wide">
          <label for="release-title">公告标题</label>
          <input
            id="release-title"
            v-model.trim="title"
            required
            maxlength="120"
            placeholder="例如：更快、更专注的写作体验"
          />
        </div>
        <div class="field-group field-wide">
          <label for="release-summary">公告摘要</label>
          <textarea
            id="release-summary"
            v-model.trim="summary"
            maxlength="1000"
            rows="3"
            placeholder="用于下载页的简短介绍"
          ></textarea>
        </div>
        <div class="field-group field-wide">
          <label for="release-notes">更新公告</label>
          <textarea
            id="release-notes"
            v-model="releaseNotes"
            maxlength="50000"
            rows="8"
            placeholder="逐条填写新增、改进与修复内容"
          ></textarea>
        </div>
        <div class="field-group">
          <span class="field-label">发布状态</span>
          <p class="field-hint">
            {{
              editingWasPublished
                ? '已发布版本，保存后继续保持公开。'
                : '可拉取文件后直接从页面顶部发布，也可以先保存草稿。'
            }}
          </p>
        </div>
      </div>

      <section class="release-assets-editor" aria-labelledby="release-assets-title">
        <div class="section-heading-row compact-heading-row">
          <div>
            <p class="card-label">文件列表</p>
            <h3 id="release-assets-title">安装文件（{{ assets.length }}）</h3>
          </div>
          <div class="admin-row-actions">
            <button
              class="text-button"
              type="button"
              :disabled="assets.length === 0"
              :aria-expanded="assetsExpanded"
              aria-controls="release-assets-content"
              @click="assetsExpanded = !assetsExpanded"
            >
              {{ assetsExpanded ? '折叠文件列表' : `展开文件列表（${assets.length}）` }}
            </button>
            <button class="button button-secondary" type="button" @click="addAsset">
              添加文件
            </button>
          </div>
        </div>
        <p v-if="assets.length === 0" class="inline-status">
          草稿可以暂不添加文件；发布时至少需要一个文件。
        </p>
        <p v-else-if="!assetsExpanded" class="release-assets-collapsed-summary" role="status">
          已折叠 {{ assets.length }} 个安装文件；发布前仍会自动校验文件信息。
        </p>
        <div id="release-assets-content" v-show="assetsExpanded" class="release-assets-content">
          <article v-for="(asset, index) in assets" :key="index" class="release-asset-editor">
            <div class="release-asset-heading">
              <div>
                <strong>文件 {{ index + 1 }}</strong>
                <span class="tag tag-muted">{{
                  asset.source === 'github'
                    ? 'GitHub Release'
                    : asset.source === 'mirror'
                      ? '镜像站链接'
                      : asset.source === 'pushed'
                        ? '本地构建推送'
                        : asset.uploadPhase === 'uploaded'
                          ? '已存储到 S3'
                          : asset.source === 'remote'
                            ? '外链转存'
                            : '本地上传'
                }}</span>
              </div>
              <button
                class="text-button danger-text-button"
                type="button"
                :disabled="
                  asset.uploadPhase === 'importing' ||
                  asset.uploadPhase === 'hashing' ||
                  asset.uploadPhase === 'uploading'
                "
                @click="removeAsset(index)"
              >
                移除
              </button>
            </div>
            <div class="form-grid release-asset-grid">
              <div class="field-group field-wide">
                <span class="field-label">安装文件来源</span>
                <div class="asset-source-switch" role="group" :aria-label="`文件 ${index + 1}来源`">
                  <button
                    v-if="asset.source === 'github'"
                    type="button"
                    class="asset-source-option active"
                    aria-pressed="true"
                    disabled
                  >
                    <strong>GitHub Release</strong>
                    <small>使用保存的 Token 读取私有资产，直链首次下载后写入本地缓存</small>
                  </button>
                  <button
                    v-if="asset.source === 'mirror'"
                    type="button"
                    class="asset-source-option active"
                    aria-pressed="true"
                    disabled
                  >
                    <strong>镜像站链接</strong>
                    <small>缓存缺失时从受控链接直接拉取，并校验文件大小和 SHA-256</small>
                  </button>
                  <button
                    v-if="asset.source === 'pushed'"
                    type="button"
                    class="asset-source-option active"
                    aria-pressed="true"
                    disabled
                  >
                    <strong>本地构建推送</strong>
                    <small>由 Release Access Key 脚本写入服务端持久发布目录</small>
                  </button>
                  <button
                    type="button"
                    :class="['asset-source-option', { active: asset.source === 'remote' }]"
                    :aria-pressed="asset.source === 'remote'"
                    @click="switchAssetSource(asset, 'remote')"
                  >
                    <strong>外链检测并转存</strong>
                    <small>自动下载检测文件名、大小和 SHA-256，再统一写入 S3</small>
                  </button>
                  <button
                    type="button"
                    :class="['asset-source-option', { active: asset.source === 'local' }]"
                    :aria-pressed="asset.source === 'local'"
                    @click="switchAssetSource(asset, 'local')"
                  >
                    <strong>上传本地文件</strong>
                    <small>经同源 API 流式写入 S3，无需配置存储桶 CORS</small>
                  </button>
                </div>
              </div>
              <div class="field-group">
                <label :for="`asset-platform-${index}`">系统</label>
                <select
                  :id="`asset-platform-${index}`"
                  v-model="asset.platform"
                  :disabled="
                    asset.uploadPhase === 'importing' ||
                    asset.uploadPhase === 'hashing' ||
                    asset.uploadPhase === 'uploading' ||
                    Boolean(asset.objectKey)
                  "
                >
                  <option value="web">Web 静态包</option>
                  <option value="windows">Windows</option>
                  <option value="macos">macOS</option>
                  <option value="linux">Linux</option>
                  <option value="android">Android</option>
                  <option value="ios">iOS</option>
                </select>
              </div>
              <div class="field-group">
                <label :for="`asset-arch-${index}`">架构</label>
                <select
                  :id="`asset-arch-${index}`"
                  v-model="asset.architecture"
                  :disabled="
                    asset.uploadPhase === 'importing' ||
                    asset.uploadPhase === 'hashing' ||
                    asset.uploadPhase === 'uploading' ||
                    Boolean(asset.objectKey)
                  "
                >
                  <option value="x64">x64</option>
                  <option value="arm64">ARM64</option>
                  <option value="universal">通用</option>
                </select>
              </div>
              <template v-if="asset.source === 'mirror'">
                <p class="field-hint field-wide">
                  文件引用来自镜像站公开版本目录；无需 S3，首次下载校验成功后会复用本机缓存。
                </p>
              </template>
              <template v-else-if="asset.source === 'github'">
                <p class="field-hint field-wide">
                  文件引用来自 GitHub Asset API；无需公开仓库或匿名外链，也不会把访问 Token
                  返回给下载者。
                </p>
              </template>
              <template v-else-if="asset.source === 'pushed'">
                <p class="field-hint field-wide">
                  文件由本机构建脚本完成大小和 SHA-256 双重校验后推送；不经过
                  S3，也不依赖外部下载源。
                </p>
              </template>
              <template v-else-if="asset.source === 'remote'">
                <div class="field-group field-wide">
                  <label :for="`asset-url-${index}`">外链安装包地址</label>
                  <input
                    :id="`asset-url-${index}`"
                    v-model.trim="asset.sourceURL"
                    type="url"
                    :required="!asset.objectKey"
                    :disabled="asset.uploadPhase === 'importing' || Boolean(asset.objectKey)"
                    placeholder="https://github.com/example/releases/download/v1.0.0/WenzWork.exe"
                  />
                  <small>服务端会实际下载文件、自动计算参数并直接转存到 S3。</small>
                  <button
                    v-if="asset.uploadPhase !== 'uploaded'"
                    class="button button-secondary"
                    type="button"
                    :disabled="
                      !version.trim() ||
                      !isHTTPURL(asset.sourceURL) ||
                      asset.uploadPhase === 'importing'
                    "
                    @click="importAsset(asset)"
                  >
                    {{
                      asset.uploadPhase === 'importing' ? '正在下载检测并转存…' : '检测并转存到 S3'
                    }}
                  </button>
                  <progress v-if="asset.uploadPhase === 'importing'"></progress>
                </div>
              </template>
              <template v-else>
                <div class="field-group field-wide">
                  <label :for="`asset-file-${index}`">选择本地安装文件</label>
                  <input
                    :id="`asset-file-${index}`"
                    class="file-input"
                    type="file"
                    :required="!asset.objectKey"
                    :disabled="asset.uploadPhase === 'hashing' || asset.uploadPhase === 'uploading'"
                    @change="selectUploadFile(asset, $event)"
                  />
                  <small
                    >文件经 WenzWork API 流式写入 S3，浏览器不再跨域访问对象存储，最大支持 5
                    GB。</small
                  >
                </div>
                <div
                  v-if="asset.selectedFile || asset.downloadUrl"
                  class="asset-upload-card field-wide"
                >
                  <div class="asset-upload-summary">
                    <div>
                      <strong>{{ asset.fileName }}</strong>
                      <small>{{ formatBytes(asset.fileSizeBytes) }}</small>
                    </div>
                    <span v-if="asset.uploadPhase === 'uploaded'" class="tag">已存储</span>
                  </div>
                  <template
                    v-if="asset.uploadPhase === 'hashing' || asset.uploadPhase === 'uploading'"
                  >
                    <div class="asset-upload-progress-row">
                      <span>{{
                        asset.uploadPhase === 'hashing'
                          ? '正在计算 SHA-256'
                          : '正在经服务端上传到 S3'
                      }}</span>
                      <strong>{{ asset.uploadProgress }}%</strong>
                    </div>
                    <progress :value="asset.uploadProgress" max="100"></progress>
                  </template>
                  <button
                    v-else-if="asset.selectedFile && asset.uploadPhase !== 'uploaded'"
                    class="button button-secondary"
                    type="button"
                    :disabled="!version.trim()"
                    @click="uploadAsset(asset)"
                  >
                    计算校验并上传到 S3
                  </button>
                  <p v-if="!version.trim() && asset.selectedFile" class="field-hint">
                    填写版本号后即可上传。
                  </p>
                </div>
              </template>
              <div v-if="asset.objectKey" class="asset-upload-card field-wide">
                <div class="asset-upload-summary">
                  <div>
                    <strong>{{ asset.fileName }}</strong>
                    <small>{{ formatBytes(asset.fileSizeBytes) }} · {{ asset.objectKey }}</small>
                  </div>
                  <span class="tag">{{ storedAssetStatusLabel(asset) }}</span>
                </div>
              </div>
              <div v-if="asset.downloadUrl" class="field-group field-wide">
                <label :for="`asset-generated-url-${index}`">{{
                  storedAssetURLLabel(asset)
                }}</label>
                <input :id="`asset-generated-url-${index}`" :value="asset.downloadUrl" readonly />
              </div>
              <div v-if="asset.sha256" class="field-group field-wide">
                <label :for="`asset-generated-sha-${index}`">自动检测的 SHA-256</label>
                <input
                  :id="`asset-generated-sha-${index}`"
                  :value="asset.sha256"
                  readonly
                  spellcheck="false"
                />
              </div>
              <p v-if="asset.uploadError" class="form-message form-error field-wide" role="alert">
                {{ asset.uploadError }}
              </p>
              <div class="field-group">
                <label :for="`asset-signature-${index}`">签名状态</label>
                <select :id="`asset-signature-${index}`" v-model="asset.signatureStatus">
                  <option value="unknown">未知</option>
                  <option value="unsigned">未签名</option>
                  <option value="valid">签名有效</option>
                </select>
              </div>
            </div>
          </article>
        </div>
      </section>

      <p v-if="submitIssue" class="release-submit-hint" role="status">{{ submitIssue }}</p>
    </form>

    <section
      id="release-management-panel-list"
      v-show="activeTab === 'list'"
      class="admin-list-section release-tab-panel"
      role="tabpanel"
      aria-labelledby="release-management-tab-list"
    >
      <div class="section-heading-row">
        <div>
          <p class="section-kicker">版本记录</p>
          <h2 id="release-list-title">全部软件版本</h2>
        </div>
        <button class="text-button" type="button" @click="load">刷新</button>
      </div>
      <p v-if="loading" class="inline-status" role="status">正在读取软件版本…</p>
      <div v-else-if="releases.length" class="release-admin-list">
        <article
          v-for="release in releases"
          :key="release.id"
          class="dashboard-card release-admin-card"
        >
          <div class="release-admin-heading">
            <div>
              <div class="batch-title-row">
                <h3>{{ release.version }}</h3>
                <span class="tag">{{ projectLabel(release.project) }}</span>
                <span :class="['tag', { 'tag-muted': release.status !== 'published' }]">{{
                  statusLabel(release.status)
                }}</span>
                <span class="tag tag-muted">{{
                  release.channel === 'stable' ? '稳定版' : '测试版'
                }}</span>
              </div>
              <strong>{{ release.title }}</strong>
              <small>{{
                release.publishedAt
                  ? `发布于 ${formatDate(release.publishedAt)}`
                  : `创建于 ${formatDate(release.createdAt)}`
              }}</small>
            </div>
            <div class="admin-row-actions">
              <template v-if="release.status !== 'withdrawn'">
                <button class="button button-secondary" type="button" @click="edit(release)">
                  编辑
                </button>
                <button
                  v-if="release.status === 'draft'"
                  class="button"
                  type="button"
                  :disabled="pending || release.assets.length === 0"
                  @click="publish(release)"
                >
                  确认发布
                </button>
                <button
                  class="text-button danger-text-button"
                  type="button"
                  :disabled="pending"
                  @click="withdraw(release)"
                >
                  下架
                </button>
              </template>
              <button
                class="text-button danger-text-button release-delete-button"
                type="button"
                :disabled="pending"
                @click="removeRelease(release)"
              >
                删除
              </button>
            </div>
          </div>
          <p v-if="release.summary" class="release-summary">{{ release.summary }}</p>
          <details class="release-notes-details">
            <summary>查看更新公告</summary>
            <p>{{ release.releaseNotes || '未填写更新公告。' }}</p>
          </details>
          <div class="release-file-list">
            <div v-for="asset in release.assets" :key="asset.id" class="release-file-row">
              <div>
                <strong>{{ asset.fileName }}</strong>
                <small
                  >{{ asset.platform }} · {{ asset.architecture }} ·
                  {{ formatBytes(asset.fileSizeBytes) }}</small
                >
              </div>
              <span class="tag tag-muted">{{ asset.signatureStatus }}</span>
            </div>
            <p v-if="release.assets.length === 0" class="inline-status">尚未添加安装文件。</p>
          </div>
        </article>
      </div>
      <p v-else class="inline-status">还没有软件版本。</p>
    </section>
  </section>
</template>
