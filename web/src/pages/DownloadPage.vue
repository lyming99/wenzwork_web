<script setup lang="ts">
import { computed, onMounted, reactive, ref, watch } from 'vue'

import { listReleases, type Release, type ReleaseAsset, type ReleaseProject } from '@/api/catalog'
import { usePageHead } from '@/composables/usePageHead'
import {
  createHostInstaller,
  createRelayInstaller,
  detectPortableTarget,
  downloadInstallerFile,
  findPortableAsset,
  installerFileName,
  portableAssetComponent,
  randomInstallerSecret,
  type HostInstallerCredentials,
  type PortableArchitecture,
  type PortableComponent,
  type PortablePlatform,
  type PortableTarget,
} from '@/utils/portableInstaller'

type PlatformId = 'web' | 'windows' | 'macos' | 'linux' | 'android' | 'ios'
type DownloadProjectId = ReleaseProject | 'device'

interface Platform {
  id: PlatformId
  name: string
  architectures: string
  requirement: string
  format: string
}

interface ProjectDefinition {
  id: DownloadProjectId
  releaseProject: ReleaseProject
  name: string
  description: string
  platforms: Platform[]
}

interface CatalogState {
  status: 'loading' | 'live' | 'empty' | 'error'
  latest?: Release
  history: Release[]
}

type InstallerComponent = Extract<PortableComponent, 'host' | 'relay'>

interface ServerProgramDefinition {
  id: InstallerComponent
  name: string
  shortName: string
  description: string
}

const serverProgramDefinitions: ServerProgramDefinition[] = [
  {
    id: 'host',
    name: 'Host 服务端',
    shortName: 'Host',
    description: '管理后台、Web 站点、API，以及 PostgreSQL 与 Redis 的便携部署入口。',
  },
  {
    id: 'relay',
    name: 'Relay 中继服务',
    shortName: 'Relay',
    description: '远程连接数据面；使用 Host 创建的 Relay Access Key 注册并拉取运行配置。',
  },
]

const serverPlatforms: Platform[] = [
  {
    id: 'linux',
    name: 'Linux',
    architectures: 'x64 / ARM64',
    requirement: '64 位 Linux',
    format: 'tar.gz',
  },
  {
    id: 'windows',
    name: 'Windows',
    architectures: 'x64 / ARM64',
    requirement: 'Windows Server / Windows 11',
    format: 'tar.gz',
  },
  {
    id: 'macos',
    name: 'macOS',
    architectures: 'Apple 芯片 / Intel',
    requirement: 'macOS 13 或更高版本',
    format: 'tar.gz',
  },
]

const projectDefinitions: ProjectDefinition[] = [
  {
    id: 'desktop',
    releaseProject: 'desktop',
    name: '桌面端',
    description: '适用于日常创作与远程管理的桌面客户端。',
    platforms: [
      {
        id: 'windows',
        name: 'Windows',
        architectures: 'x64 / ARM64',
        requirement: 'Windows 10 / 11 64 位',
        format: 'EXE / MSI / ZIP',
      },
      {
        id: 'macos',
        name: 'macOS',
        architectures: 'Apple 芯片 / Intel',
        requirement: '以版本公告为准',
        format: 'DMG / PKG / ZIP',
      },
      {
        id: 'linux',
        name: 'Linux',
        architectures: 'x64 / ARM64',
        requirement: '以版本公告为准',
        format: 'AppImage / DEB / RPM',
      },
    ],
  },
  {
    id: 'mobile',
    releaseProject: 'mobile',
    name: '手机端',
    description: 'Android 与 iOS 移动客户端。',
    platforms: [
      {
        id: 'android',
        name: 'Android',
        architectures: 'ARM64 / 通用',
        requirement: 'Android 版本要求以公告为准',
        format: 'APK / AAB',
      },
      {
        id: 'ios',
        name: 'iOS',
        architectures: 'ARM64 / 通用',
        requirement: 'iOS 版本要求以公告为准',
        format: 'IPA / App Store',
      },
    ],
  },
  {
    id: 'web',
    releaseProject: 'web',
    name: 'Web / 服务端',
    description: 'Host 管理服务与 Relay 中继服务的跨平台部署归档。',
    platforms: serverPlatforms,
  },
  {
    id: 'device',
    releaseProject: 'web',
    name: 'Device / 受控端',
    description: '部署到受控设备、持续在线提供远程能力的 Device Agent。',
    platforms: serverPlatforms,
  },
]

const catalog = reactive<Record<ReleaseProject, CatalogState>>({
  web: { status: 'loading', history: [] },
  desktop: { status: 'loading', history: [] },
  mobile: { status: 'loading', history: [] },
})
const selectedProject = ref<DownloadProjectId>('desktop')
const preferredPlatform = ref<PlatformId>()

const currentProject = computed(() =>
  projectDefinitions.find((project) => project.id === selectedProject.value)!,
)
const deviceReleaseHistory = computed(() =>
  catalog.web.history
    .map((release) => ({
      ...release,
      assets: release.assets.filter((asset) => portableAssetComponent(asset) === 'device-agent'),
    }))
    .filter((release) => release.assets.length > 0),
)
const currentCatalog = computed<CatalogState>(() => {
  const source = catalog[currentProject.value.releaseProject]
  if (selectedProject.value !== 'device') return source
  const history = deviceReleaseHistory.value
  return {
    status:
      source.status === 'loading' || source.status === 'error'
        ? source.status
        : history.length
          ? 'live'
          : 'empty',
    latest: history[0],
    history,
  }
})
const latestRelease = computed(() => currentCatalog.value.latest)
const releaseHistory = computed(() => currentCatalog.value.history)
const platformLabel = computed(
  () =>
    currentProject.value.platforms.find((platform) => platform.id === preferredPlatform.value)
      ?.name,
)
const platformCards = computed(() =>
  currentProject.value.platforms.map((platform) => ({
    ...platform,
    assets: (latestRelease.value?.assets ?? []).filter((asset) => asset.platform === platform.id),
  })),
)
const serverProgramGroups = computed(() =>
  serverProgramDefinitions.map((program) => ({
    ...program,
    assetCount: (catalog.web.latest?.assets ?? []).filter(
      (asset) => portableAssetComponent(asset) === program.id,
    ).length,
    platforms: serverPlatforms.map((platform) => ({
      ...platform,
      assets: (catalog.web.latest?.assets ?? []).filter(
        (asset) => portableAssetComponent(asset) === program.id && asset.platform === platform.id,
      ),
    })),
  })),
)

usePageHead({
  title: '软件下载',
  description:
    '获取 WenzWork 桌面端、手机端、Host、Relay 和 Device Agent 官方程序，并按平台和参数生成一键安装脚本。',
  path: '/download',
})

const detectPlatform = (): PlatformId | undefined => {
  const value = navigator.userAgent.toLocaleLowerCase()
  if (value.includes('android')) return 'android'
  if (value.includes('iphone') || value.includes('ipad')) return 'ios'
  if (value.includes('win')) return 'windows'
  if (value.includes('mac')) return 'macos'
  if (value.includes('linux')) return 'linux'
  return undefined
}

const formatDate = (value: string) =>
  new Intl.DateTimeFormat('zh-CN', { dateStyle: 'long' }).format(new Date(value))
const formatSize = (bytes: number) => {
  const megabytes = bytes / 1024 / 1024
  if (megabytes < 1) return `${(bytes / 1024).toFixed(1)} KB`
  if (megabytes < 1024)
    return `${megabytes >= 100 ? megabytes.toFixed(0) : megabytes.toFixed(1)} MB`
  return `${(megabytes / 1024).toFixed(2)} GB`
}
const architectureLabel = (asset: ReleaseAsset) =>
  ({ x64: 'x64 / AMD64', arm64: 'ARM64', universal: '通用版本' })[asset.architecture]

const loadProject = async (project: ReleaseProject) => {
  const state = catalog[project]
  state.status = 'loading'
  try {
    const history = await listReleases(project)
    state.latest = history[0]
    state.history = history
    state.status = history.length ? 'live' : 'empty'
  } catch {
    state.latest = undefined
    state.history = []
    state.status = 'error'
  }
}

const installerPlatformOptions: Array<{ value: PortablePlatform; label: string }> = [
  { value: 'linux', label: 'Linux' },
  { value: 'windows', label: 'Windows' },
  { value: 'macos', label: 'macOS' },
]
const installerArchitectureOptions: Array<{
  value: PortableArchitecture
  label: string
}> = [
  { value: 'x64', label: 'x64 / AMD64' },
  { value: 'arm64', label: 'ARM64' },
]

const installerComponent = ref<InstallerComponent>('host')
const installerTarget = ref<PortableTarget>({ platform: 'linux', architecture: 'x64' })
const installerMessage = ref('')
const hostInstallerCredentials = ref<HostInstallerCredentials | null>(null)
const hostConfiguration = reactive({
  administratorEmail: 'admin@wenzwork.local',
  administratorPassword: '',
  listenScope: 'network' as 'network' | 'loopback',
  port: 8080,
  publicBaseURL: 'http://localhost:8080',
})
const relayConfiguration = reactive({
  managementURL: 'https://wenzwork.com',
  accessKey: '',
})

const selectedInstallerAsset = computed(() =>
  findPortableAsset(catalog.web.latest, installerComponent.value, installerTarget.value),
)
const installerComponentDefinition = computed(() =>
  serverProgramDefinitions.find((item) => item.id === installerComponent.value)!,
)
const installerTargetLabel = computed(() => {
  const platform = { linux: 'Linux', windows: 'Windows', macos: 'macOS' }[
    installerTarget.value.platform
  ]
  const architecture = installerTarget.value.architecture === 'x64' ? 'x64 / AMD64' : 'ARM64'
  return platform + ' · ' + architecture
})
const hostHTTPAddress = computed(
  () =>
    `${hostConfiguration.listenScope === 'loopback' ? '127.0.0.1' : ''}:${hostConfiguration.port}`,
)

const isRootHTTPURL = (value: string) => {
  try {
    const parsed = new URL(value.trim())
    return (
      (parsed.protocol === 'http:' || parsed.protocol === 'https:') &&
      !parsed.username &&
      !parsed.password &&
      (parsed.pathname === '' || parsed.pathname === '/') &&
      !parsed.search &&
      !parsed.hash
    )
  } catch {
    return false
  }
}

const isRelayManagementURL = (value: string) => isRootHTTPURL(value)

const installerIssue = computed(() => {
  if (catalog.web.status === 'loading') return '正在同步服务端正式版本…'
  if (!selectedInstallerAsset.value) {
    return `当前正式版尚未发布 ${installerTargetLabel.value} 的 ${installerComponentDefinition.value.shortName} 部署包。`
  }
  if (installerComponent.value === 'host') {
    if (!/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(hostConfiguration.administratorEmail.trim())) {
      return '请输入有效的初始管理员邮箱。'
    }
    const passwordLength = Array.from(hostConfiguration.administratorPassword).length
    if (
      passwordLength < 8 ||
      passwordLength > 128 ||
      /[\r\n]/.test(hostConfiguration.administratorPassword)
    ) {
      return '初始管理员密码需为 8–128 个字符，且不能包含换行。'
    }
    if (
      !Number.isInteger(hostConfiguration.port) ||
      hostConfiguration.port < 1 ||
      hostConfiguration.port > 65535
    ) {
      return 'Host 端口必须是 1–65535 之间的整数。'
    }
    if (!isRootHTTPURL(hostConfiguration.publicBaseURL)) {
      return '首次访问地址必须是无路径、查询参数和账号信息的完整 HTTP(S) 地址。'
    }
    return ''
  }
  if (!isRelayManagementURL(relayConfiguration.managementURL)) {
    return 'Host 管理地址必须是无路径、查询参数和账号信息的完整 HTTP(S) 地址。'
  }
  if (!/^relay_[A-Za-z0-9_-]{43}$/.test(relayConfiguration.accessKey.trim())) {
    return '请输入 Host 中创建的有效 Relay Access Key。'
  }
  return ''
})

const regenerateHostPassword = () => {
  hostConfiguration.administratorPassword = randomInstallerSecret()
  hostInstallerCredentials.value = null
  installerMessage.value = ''
}

const downloadServerInstaller = () => {
  const asset = selectedInstallerAsset.value
  if (!asset || installerIssue.value) {
    installerMessage.value = installerIssue.value
    return
  }
  const assetURL = new URL(asset.downloadUrl, window.location.origin).toString()
  let contents: string
  if (installerComponent.value === 'host') {
    const credentials: HostInstallerCredentials = {
      administratorEmail: hostConfiguration.administratorEmail.trim(),
      administratorPassword: hostConfiguration.administratorPassword,
    }
    contents = createHostInstaller(asset, installerTarget.value, assetURL, {
      ...credentials,
      httpAddress: hostHTTPAddress.value,
      publicBaseURL: hostConfiguration.publicBaseURL.trim().replace(/\/$/, ''),
    })
    hostInstallerCredentials.value = credentials
  } else {
    contents = createRelayInstaller(asset, installerTarget.value, assetURL, {
      managementURL: relayConfiguration.managementURL.trim().replace(/\/$/, ''),
      accessKey: relayConfiguration.accessKey.trim(),
    })
    hostInstallerCredentials.value = null
  }
  downloadInstallerFile(
    contents,
    installerFileName(installerComponent.value, installerTarget.value, catalog.web.latest!.version),
  )
  installerMessage.value = `${installerComponentDefinition.value.shortName} 一键安装脚本已生成；请在目标主机以管理员权限运行。`
}

watch(
  () => hostConfiguration.port,
  (port, previousPort) => {
    if (hostConfiguration.publicBaseURL === `http://localhost:${previousPort}`) {
      hostConfiguration.publicBaseURL = `http://localhost:${port}`
    }
  },
)

watch(
  [
    installerComponent,
    () => installerTarget.value.platform,
    () => installerTarget.value.architecture,
  ],
  () => {
    installerMessage.value = ''
    hostInstallerCredentials.value = null
  },
)

onMounted(() => {
  preferredPlatform.value = detectPlatform()
  installerTarget.value = detectPortableTarget()
  regenerateHostPassword()
  if (isRelayManagementURL(window.location.origin)) {
    relayConfiguration.managementURL = window.location.origin
  }
  void Promise.all(
    (['desktop', 'mobile', 'web'] satisfies ReleaseProject[]).map((project) =>
      loadProject(project),
    ),
  )
})
</script>

<template>
  <section class="content-hero download-hero">
    <div class="shell content-hero-grid">
      <div>
        <p class="section-kicker">软件下载</p>
        <h1>四类程序，<br />一个可信下载入口。</h1>
        <p class="page-lead">
          桌面端、手机端、Web / 服务端与 Device / 受控端按用途分区，并展示完整文件校验信息。
        </p>
      </div>
      <aside class="release-summary" aria-label="当前项目最新版本状态">
        <span>{{ currentProject.name }}最新稳定版</span>
        <strong>{{ latestRelease ? `v${latestRelease.version}` : '尚未发布' }}</strong>
        <p v-if="latestRelease">
          {{ latestRelease.title }} · {{ formatDate(latestRelease.publishedAt) }}
        </p>
        <p v-if="latestRelease?.summary" class="release-introduction">
          <b>版本简介</b><span>{{ latestRelease.summary }}</span>
        </p>
        <p v-if="platformLabel">检测到 {{ platformLabel }}，已为你标出推荐平台。</p>
      </aside>
    </div>
  </section>

  <section class="content-section download-section">
    <div class="shell">
      <div class="download-project-switch" role="tablist" aria-label="下载项目类型">
        <button
          v-for="project in projectDefinitions"
          :key="project.id"
          type="button"
          role="tab"
          :aria-selected="selectedProject === project.id"
          :class="['download-project-option', { active: selectedProject === project.id }]"
          @click="selectedProject = project.id"
        >
          <strong>{{ project.name }}</strong
          ><small>{{ project.description }}</small>
        </button>
      </div>
      <div v-if="currentCatalog.status === 'loading'" class="inline-status" role="status">
        正在同步最新版本目录…
      </div>
      <div
        v-else-if="currentCatalog.status === 'live'"
        class="notice-banner notice-success"
        role="status"
      >
        <strong>已获取{{ currentProject.name }}最新版本</strong
        ><span>下载前请核对平台、架构、签名状态和完整 SHA-256。</span>
      </div>
      <div v-else-if="currentCatalog.status === 'error'" class="notice-banner" role="status">
        <strong>版本服务暂时不可用</strong
        ><span>当前仍展示全部支持平台和安全说明；稍后刷新可重新同步。</span>
      </div>
      <div v-else class="notice-banner" role="status">
        <strong>{{ currentProject.name }}正式版正在准备中</strong
        ><span>当前没有可安全下载的正式文件；页面不会提供占位文件或虚假链接。</span>
      </div>

      <div v-if="selectedProject === 'web'" class="server-program-accordion">
        <details
          v-for="program in serverProgramGroups"
          :key="program.id"
          class="server-program-details"
          name="server-program"
          :open="program.id === 'host'"
        >
          <summary>
            <span class="server-program-summary-copy">
              <span class="platform-icon" aria-hidden="true">{{ program.shortName[0] }}</span>
              <span
                ><strong>{{ program.name }}</strong
                ><small>{{ program.description }}</small></span
              >
            </span>
            <span :class="['tag', { 'tag-muted': !program.assetCount }]">
              {{ program.assetCount ? `${program.assetCount} 个安装包` : '暂未发布' }}
            </span>
          </summary>
          <div class="platform-grid download-platform-grid server-platform-grid">
            <article
              v-for="platform in program.platforms"
              :key="`${program.id}-${platform.id}`"
              :class="['platform-card', { recommended: preferredPlatform === platform.id }]"
            >
              <div class="platform-card-top">
                <span class="platform-icon" aria-hidden="true">{{
                  platform.name.slice(0, 1)
                }}</span>
                <span :class="['tag', { 'tag-muted': !platform.assets.length }]">
                  {{ platform.assets.length ? '已发布' : '暂不可用' }}
                </span>
                <span v-if="preferredPlatform === platform.id" class="tag">推荐</span>
              </div>
              <h3>{{ platform.name }}</h3>
              <dl v-if="!platform.assets.length">
                <div>
                  <dt>架构</dt>
                  <dd>{{ platform.architectures }}</dd>
                </div>
                <div>
                  <dt>系统</dt>
                  <dd>{{ platform.requirement }}</dd>
                </div>
                <div>
                  <dt>格式</dt>
                  <dd>{{ platform.format }}</dd>
                </div>
              </dl>
              <div v-else class="asset-list">
                <div v-for="asset in platform.assets" :key="asset.id" class="asset-item">
                  <dl>
                    <div>
                      <dt>架构</dt>
                      <dd>{{ architectureLabel(asset) }}</dd>
                    </div>
                    <div>
                      <dt>大小</dt>
                      <dd>{{ formatSize(asset.fileSizeBytes) }}</dd>
                    </div>
                    <div>
                      <dt>签名</dt>
                      <dd>{{ asset.signatureStatus === 'valid' ? '签名有效' : '请核验' }}</dd>
                    </div>
                  </dl>
                  <a class="button" :href="asset.downloadUrl">下载 {{ asset.fileName }}</a>
                  <div class="checksum">
                    <span>SHA-256</span><code>{{ asset.sha256 }}</code>
                  </div>
                </div>
              </div>
              <span
                v-if="!platform.assets.length"
                class="button button-disabled"
                aria-disabled="true"
                >安装包暂不可下载</span
              >
            </article>
          </div>
        </details>
      </div>
      <div v-else class="platform-grid download-platform-grid">
        <article
          v-for="platform in platformCards"
          :key="platform.id"
          :class="['platform-card', { recommended: preferredPlatform === platform.id }]"
        >
          <div class="platform-card-top">
            <span class="platform-icon" aria-hidden="true">{{ platform.name.slice(0, 1) }}</span
            ><span :class="['tag', { 'tag-muted': !platform.assets.length }]">{{
              platform.assets.length ? '已发布' : '暂不可用'
            }}</span
            ><span v-if="preferredPlatform === platform.id" class="tag">推荐</span>
          </div>
          <h2>{{ platform.name }}</h2>
          <dl v-if="!platform.assets.length">
            <div>
              <dt>架构</dt>
              <dd>{{ platform.architectures }}</dd>
            </div>
            <div>
              <dt>系统</dt>
              <dd>{{ platform.requirement }}</dd>
            </div>
            <div>
              <dt>格式</dt>
              <dd>{{ platform.format }}</dd>
            </div>
          </dl>
          <div v-else class="asset-list">
            <div v-for="asset in platform.assets" :key="asset.id" class="asset-item">
              <dl>
                <div>
                  <dt>架构</dt>
                  <dd>{{ architectureLabel(asset) }}</dd>
                </div>
                <div>
                  <dt>大小</dt>
                  <dd>{{ formatSize(asset.fileSizeBytes) }}</dd>
                </div>
                <div>
                  <dt>签名</dt>
                  <dd>{{ asset.signatureStatus === 'valid' ? '签名有效' : '请核验' }}</dd>
                </div>
              </dl>
              <a class="button" :href="asset.downloadUrl">下载 {{ asset.fileName }}</a>
              <div class="checksum">
                <span>SHA-256</span><code>{{ asset.sha256 }}</code>
              </div>
            </div>
          </div>
          <span v-if="!platform.assets.length" class="button button-disabled" aria-disabled="true"
            >安装包暂不可下载</span
          >
        </article>
      </div>
    </div>
  </section>

  <section v-if="selectedProject === 'web'" class="content-section deployment-builder-section">
    <div class="shell deployment-builder-grid">
      <div>
        <p class="section-kicker">一键安装脚本</p>
        <h2>选择组件和平台，配置完成后直接安装。</h2>
        <p>
          Host 脚本会写入监听端口与初始管理员信息，并在需要时通过 Docker 准备 PostgreSQL 和
          Redis；Relay 脚本会写入 Host 管理地址与一次性 Access
          Key。两类脚本都会下载匹配的正式包、校验 SHA-256、解压并后台启动。
        </p>
        <div class="notice-banner">
          <strong>
            当前选择：{{ installerComponentDefinition.name }} · {{ installerTargetLabel
            }}<template v-if="catalog.web.latest"> · v{{ catalog.web.latest.version }}</template>
          </strong>
          <span
            >Linux/macOS 下载 Bash 脚本，Windows 下载 PowerShell 脚本；也可以手动切换架构。</span
          >
        </div>
        <p class="installer-local-note">
          所填密码和 Access Key 只在当前浏览器中写入下载文件，不会提交给版本目录
          API。请将脚本视为敏感文件，安装后及时删除。
        </p>
      </div>
      <div class="deployment-builder-card official-installer-card">
        <div class="form-grid installer-target-grid">
          <div class="field-group">
            <label for="deploy-component">安装组件</label>
            <select id="deploy-component" v-model="installerComponent">
              <option
                v-for="program in serverProgramDefinitions"
                :key="program.id"
                :value="program.id"
              >
                {{ program.name }}
              </option>
            </select>
          </div>
          <div class="field-group">
            <label for="deploy-platform">目标平台</label>
            <select id="deploy-platform" v-model="installerTarget.platform">
              <option
                v-for="platform in installerPlatformOptions"
                :key="platform.value"
                :value="platform.value"
              >
                {{ platform.label }}
              </option>
            </select>
          </div>
          <div class="field-group">
            <label for="deploy-architecture">处理器架构</label>
            <select id="deploy-architecture" v-model="installerTarget.architecture">
              <option
                v-for="architecture in installerArchitectureOptions"
                :key="architecture.value"
                :value="architecture.value"
              >
                {{ architecture.label }}
              </option>
            </select>
          </div>
        </div>

        <div v-if="installerComponent === 'host'" class="installer-component-fields">
          <div class="installer-form-heading">
            <strong>Host 初始化参数</strong>
            <small>正式数据库、邮件、对象存储等配置仍可在首次登录的系统设置页完成。</small>
          </div>
          <div class="form-grid">
            <div class="field-group">
              <label for="host-listen-scope">监听范围</label>
              <select id="host-listen-scope" v-model="hostConfiguration.listenScope">
                <option value="network">所有网卡（远程访问）</option>
                <option value="loopback">仅本机（反向代理）</option>
              </select>
            </div>
            <div class="field-group">
              <label for="host-port">Host 端口</label>
              <input
                id="host-port"
                v-model.number="hostConfiguration.port"
                type="number"
                min="1"
                max="65535"
                inputmode="numeric"
              />
              <small>脚本将写入 HTTP_ADDR={{ hostHTTPAddress }}</small>
            </div>
            <div class="field-group field-wide">
              <label for="host-public-url">首次访问地址</label>
              <input
                id="host-public-url"
                v-model.trim="hostConfiguration.publicBaseURL"
                type="url"
                placeholder="http://服务器地址:8080"
                autocomplete="url"
              />
              <small>使用反向代理时可填写外部 HTTPS 地址；地址应包含对外端口。</small>
            </div>
            <div class="field-group field-wide">
              <label for="host-admin-email">初始管理员邮箱</label>
              <input
                id="host-admin-email"
                v-model.trim="hostConfiguration.administratorEmail"
                type="email"
                autocomplete="email"
              />
            </div>
            <div class="field-group field-wide">
              <label for="host-admin-password">初始管理员密码</label>
              <div class="installer-secret-row">
                <input
                  id="host-admin-password"
                  v-model="hostConfiguration.administratorPassword"
                  type="password"
                  autocomplete="new-password"
                />
                <button
                  class="button button-secondary"
                  type="button"
                  @click="regenerateHostPassword"
                >
                  重新生成
                </button>
              </div>
            </div>
          </div>
        </div>

        <div v-else class="installer-component-fields">
          <div class="installer-form-heading">
            <strong>Relay 注册参数</strong>
            <small>监听地址、Redis、容量和验签公钥由 Host 在注册成功后下发。</small>
          </div>
          <div class="form-grid">
            <div class="field-group field-wide">
              <label for="relay-management-url">Host 管理地址</label>
              <input
                id="relay-management-url"
                v-model.trim="relayConfiguration.managementURL"
                type="url"
                placeholder="http://host.example.com:8080"
                autocomplete="url"
              />
              <small
                >可自行选择 HTTP 或 HTTPS；HTTP 会以明文传输 Relay Access
                Key，请仅用于可信网络。</small
              >
            </div>
            <div class="field-group field-wide">
              <label for="relay-access-key">Relay Access Key</label>
              <input
                id="relay-access-key"
                v-model.trim="relayConfiguration.accessKey"
                type="password"
                placeholder="relay_…"
                autocomplete="off"
                spellcheck="false"
              />
              <small>请先在 Host 管理后台创建中继节点安装记录；Access Key 只显示一次。</small>
            </div>
          </div>
        </div>

        <p v-if="installerIssue" class="form-message installer-validation-message" role="status">
          {{ installerIssue }}
        </p>
        <button
          class="button installer-download-button"
          type="button"
          :disabled="Boolean(installerIssue)"
          @click="downloadServerInstaller"
        >
          下载 {{ installerTarget.platform === 'windows' ? 'PowerShell' : 'Bash' }} 一键安装脚本
        </button>
        <p v-if="installerMessage" class="form-message form-success" role="status">
          {{ installerMessage }}
        </p>
        <div v-if="hostInstallerCredentials" class="one-time-installer-credentials">
          <strong>Host 首次登录凭据</strong>
          <code>{{ hostInstallerCredentials.administratorEmail }}</code>
          <code>{{ hostInstallerCredentials.administratorPassword }}</code>
          <small>凭据同时写入下载脚本；完成首次系统初始化后，管理员明文密码会从 .env 清除。</small>
        </div>
      </div>
    </div>
  </section>
  <section class="verification-section">
    <div class="shell verification-grid">
      <div>
        <p class="section-kicker">下载安全</p>
        <h2>发布记录与文件校验同样重要。</h2>
      </div>
      <div class="verification-copy">
        <p>
          正式资源只会从本页列出的官方地址提供。下载后，请在运行前比较完整
          SHA-256；不一致的文件应立即删除。
        </p>
        <RouterLink class="text-link" to="/help/verify-download"
          >学习如何校验文件 <span aria-hidden="true">→</span></RouterLink
        >
      </div>
    </div>
  </section>
  <section class="content-section compact-section">
    <div class="shell release-history">
      <div>
        <p class="section-kicker">版本记录</p>
        <h2>
          {{ releaseHistory.length ? `${currentProject.name}已发布版本` : '还没有已发布版本' }}
        </h2>
      </div>
      <ol v-if="releaseHistory.length" class="release-list">
        <li v-for="release in releaseHistory" :key="release.id">
          <div>
            <strong>v{{ release.version }}</strong
            ><time :datetime="release.publishedAt">{{ formatDate(release.publishedAt) }}</time>
          </div>
          <div class="release-history-content">
            <p class="release-history-title">{{ release.title }}</p>
            <p class="release-history-introduction">
              <strong>版本简介</strong><span>{{ release.summary || '暂无版本简介。' }}</span>
            </p>
            <details class="release-notes-details">
              <summary>查看更新内容</summary>
              <div class="release-notes-copy">
                {{ release.releaseNotes || '暂无更新内容详情。' }}
              </div>
            </details>
          </div>
        </li>
      </ol>
      <p v-else>版本发布后，这里会保留版本号、发布日期、变更摘要以及每个平台的独立校验信息。</p>
    </div>
  </section>
</template>

<style scoped>
.download-project-switch {
  grid-template-columns: repeat(4, minmax(0, 1fr));
}

@media (max-width: 900px) {
  .download-project-switch {
    grid-template-columns: repeat(2, minmax(0, 1fr));
  }
}

@media (max-width: 640px) {
  .download-project-switch {
    grid-template-columns: 1fr;
  }
}
</style>
