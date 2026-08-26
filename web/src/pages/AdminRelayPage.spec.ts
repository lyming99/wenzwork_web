import { flushPromises, mount } from '@vue/test-utils'
import { createHead } from '@unhead/vue/client'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import {
  activateRelayInstallation,
  createRelayAccessKey,
  createRelayInstallSession,
  createRelayInstallation,
  createRelayServerRelease,
  deleteRelayInstallation,
  drainRelayNode,
  getRelayOperation,
  getRelayInstallation,
  listRelayInstallations,
  listRelayServerReleases,
  listRelayTopology,
  publishRelayServerRelease,
  revokeRelayInstallation,
  updateRelayCell,
  updateRelayInstallation,
  type RelayInstallation,
  type RelayOperation,
  type RelayServerRelease,
  type RelayTopology,
} from '@/api/adminRelay'

import AdminRelayPage from './AdminRelayPage.vue'

vi.mock('@/api/adminRelay', () => ({
  activateRelayInstallation: vi.fn(),
  createRelayAccessKey: vi.fn(),
  createRelayInstallSession: vi.fn(),
  createRelayInstallation: vi.fn(),
  createRelayServerRelease: vi.fn(),
  deleteRelayInstallation: vi.fn(),
  deleteRelayServerRelease: vi.fn(),
  drainRelayNode: vi.fn(),
  getRelayOperation: vi.fn(),
  getRelayInstallation: vi.fn(),
  listRelayInstallations: vi.fn(),
  listRelayServerReleases: vi.fn(),
  listRelayTopology: vi.fn(),
  publishRelayServerRelease: vi.fn(),
  retireRelayServerRelease: vi.fn(),
  revokeRelayInstallation: vi.fn(),
  updateRelayCell: vi.fn(),
  updateRelayInstallation: vi.fn(),
  updateRelayServerRelease: vi.fn(),
}))

const cellId = '01700000-0000-4000-8000-000000000017'
const installationId = '21700000-0000-4000-8000-000000000001'
const instanceId = '31700000-0000-4000-8000-000000000001'
const endpoint = 'wss://relay.example.test/v2/connect'
const now = '2026-08-07T10:00:00Z'
const releaseId = '61700000-0000-4000-8000-000000000001'
const arm64ReleaseId = '61700000-0000-4000-8000-000000000002'
const windowsArm64ReleaseId = '61700000-0000-4000-8000-000000000003'

const makeTopology = (
  status: 'draft' | 'active' | 'draining' | 'disabled' = 'draft',
  healthyInstances = 0,
): RelayTopology => ({
  defaultCellId: cellId,
  items: [
    {
      id: '11700000-0000-4000-8000-000000000001',
      code: 'cn-dev',
      name: '中国开发区',
      dataResidency: 'CN',
      status: 'active',
      pools: [
        {
          id: '11700000-0000-4000-8000-000000000002',
          code: 'standard',
          name: '标准资源池',
          status: 'active',
          cells: [
            {
              id: cellId,
              code: 'r017',
              name: '默认中继组',
              failureDomain: 'default',
              status,
              weight: 1,
              connectionSoftLimit: 1000,
              connectionHardLimit: 1200,
              protocolMin: 2,
              protocolMax: 2,
              activeEndpoint: null,
              installationCount: 1,
              healthyInstances,
            },
          ],
        },
      ],
    },
  ],
})

const makeOperation = (status: RelayOperation['status'] = 'succeeded'): RelayOperation => ({
  id: '81700000-0000-4000-8000-000000000001',
  type: 'cell_update',
  status,
  targetType: 'relay_cell',
  targetId: cellId,
  progressCompleted: status === 'succeeded' ? 1 : 0,
  progressTotal: 1,
  progressPercent: status === 'succeeded' ? 100 : 0,
  resultCode: status === 'succeeded' ? 'completed' : null,
  errorMessage: null,
  items: [],
  createdAt: now,
  updatedAt: now,
})

const makeRelease = (status: RelayServerRelease['status'] = 'published'): RelayServerRelease => ({
  id: releaseId,
  version: '1.2.3',
  platform: 'linux',
  architecture: 'amd64',
  protocolMin: 2,
  protocolMax: 2,
  buildCommit: 'a'.repeat(40),
  buildTime: now,
  signingKeyId: 'release-2026',
  manifestSha256: 'b'.repeat(64),
  manifestSignature: 'c'.repeat(64),
  status,
  artifacts: [
    {
      id: '71700000-0000-4000-8000-000000000001',
      fileName: 'wenzwork-relay-1.2.3-linux-amd64.tar.gz',
      fileSizeBytes: 4096,
      sha256: 'd'.repeat(64),
      signature: 'e'.repeat(64),
      objectKey: 'relay/1.2.3/wenzwork-relay-1.2.3-linux-amd64.tar.gz',
    },
  ],
})

const makeArm64Release = (): RelayServerRelease => {
  const release = makeRelease()
  return {
    ...release,
    id: arm64ReleaseId,
    architecture: 'arm64',
    artifacts: release.artifacts.map((artifact) => ({
      ...artifact,
      id: '71700000-0000-4000-8000-000000000002',
      fileName: 'wenzwork-relay-1.2.3-linux-arm64.tar.gz',
      objectKey: 'relay/1.2.3/wenzwork-relay-1.2.3-linux-arm64.tar.gz',
    })),
  }
}

const makeWindowsArm64Release = (): RelayServerRelease => {
  const release = makeArm64Release()
  return {
    ...release,
    id: windowsArm64ReleaseId,
    platform: 'windows',
    artifacts: release.artifacts.map((artifact) => ({
      ...artifact,
      id: '71700000-0000-4000-8000-000000000003',
      fileName: 'wenzwork-relay-1.2.3-windows-arm64.tar.gz',
      objectKey: 'relay/1.2.3/wenzwork-relay-1.2.3-windows-arm64.tar.gz',
    })),
  }
}

const makeInstallation = (
  status: RelayInstallation['status'],
  overrides: Partial<RelayInstallation> = {},
): RelayInstallation => {
  const registered = !['draft', 'pending_enrollment'].includes(status)
  const running = ['active', 'draining', 'disabled'].includes(status)
  return {
    id: installationId,
    cellId,
    cellCode: 'default',
    releaseId: null,
    displayName: 'relay-cn-01',
    region: '华东',
    group: '生产环境',
    failureDomain: '',
    operationsNote: '',
    publicEndpoint: '',
    listenerPort: 8443,
    platform: 'linux',
    architecture: 'amd64',
    status,
    identityThumbprint: registered ? 'c'.repeat(64) : null,
    deploymentChecklist: { lb: true, dns: true, port: true, tls: true },
    firstEnrolledAt: registered ? now : null,
    activatedAt: running ? now : null,
    revokedAt: status === 'revoked' ? now : null,
    version: 3,
    currentInstance: running
      ? {
          id: instanceId,
          installationId,
          cellId,
          status: status === 'draining' ? 'draining' : 'ready',
          version: '1.0.0',
          protocolVersion: 2,
          addresses: [endpoint],
          capabilities: {},
          activeConnections: 128,
          activeFileTransfers: 0,
          memoryBytes: 1024,
          ingressMbps: 1,
          egressMbps: 2,
          writeLoopLagMs: 0,
          startedAt: now,
          lastHeartbeatAt: new Date().toISOString(),
          leaseExpiresAt: '2099-08-07T10:01:00Z',
          stoppedAt: null,
        }
      : null,
    instances: [],
    createdAt: now,
    updatedAt: now,
    ...overrides,
  }
}

const mountPage = () => mount(AdminRelayPage, { global: { plugins: [createHead()] } })

const buttonWithText = (wrapper: ReturnType<typeof mountPage>, label: string) => {
  const button = wrapper.findAll('button').find((candidate) => candidate.text().includes(label))
  if (!button) throw new Error(`missing button: ${label}`)
  return button
}

describe('AdminRelayPage', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    vi.mocked(listRelayInstallations).mockResolvedValue([])
    vi.mocked(listRelayServerReleases).mockResolvedValue([])
    vi.mocked(listRelayTopology).mockResolvedValue(makeTopology())
    vi.mocked(getRelayOperation).mockResolvedValue(makeOperation())
    Object.defineProperty(window, 'confirm', {
      value: vi.fn(() => true),
      configurable: true,
    })
  })

  it('presents the host inventory and the explicit scheduling state', async () => {
    const wrapper = mountPage()
    await flushPromises()

    expect(wrapper.get('h1').text()).toBe('中继主机')
    expect(wrapper.findAll('.summary-bar article')).toHaveLength(4)
    expect(wrapper.text()).toContain('还没有中继主机')
    expect(wrapper.text()).toContain('管理端不会登录服务器或远程执行命令')
    expect(wrapper.text()).toContain('中继调度状态')
    expect(wrapper.text()).toContain('未启用（draft）')
    expect(wrapper.text()).toContain('启用调度')
    expect(wrapper.find('select').exists()).toBe(false)
    expect(listRelayInstallations).toHaveBeenCalledWith()
    expect(listRelayTopology).toHaveBeenCalledWith()

    wrapper.unmount()
  })

  it('isolates primary and secondary actions from global button theme styles', async () => {
    const wrapper = mountPage()
    await flushPromises()

    const refreshButton = buttonWithText(wrapper, '刷新')
    const addButton = buttonWithText(wrapper, '添加主机')

    expect(refreshButton.classes()).toEqual(['relay-button', 'relay-button--secondary'])
    expect(addButton.classes()).toEqual(['relay-button', 'relay-button--primary'])
    expect(wrapper.find('.button').exists()).toBe(false)

    wrapper.unmount()
  })

  it('activates a draft scheduling Cell from the Relay management page', async () => {
    const online = makeInstallation('active', { publicEndpoint: endpoint })
    vi.mocked(listRelayInstallations).mockResolvedValue([online])
    vi.mocked(listRelayTopology)
      .mockResolvedValueOnce(makeTopology('draft', 1))
      .mockResolvedValue(makeTopology('active', 1))
    vi.mocked(updateRelayCell).mockResolvedValue(makeOperation('succeeded'))

    const wrapper = mountPage()
    await flushPromises()
    expect(wrapper.text()).toContain('Relay 已在线，但调度尚未启用')

    await buttonWithText(wrapper, '启用调度').trigger('click')
    await flushPromises()

    expect(updateRelayCell).toHaveBeenCalledWith(cellId, { status: 'active' })
    expect(getRelayOperation).not.toHaveBeenCalled()
    expect(wrapper.text()).toContain('已启用')
    expect(wrapper.text()).toContain('在线 Relay 现在可以接收 Device 分配')
    wrapper.unmount()
  })

  it('summarizes hosts and expresses state with a symbol and text', async () => {
    const online = makeInstallation('active')
    const waiting = makeInstallation('pending_enrollment', {
      id: '21700000-0000-4000-8000-000000000002',
      displayName: 'relay-cn-02',
    })
    vi.mocked(listRelayInstallations).mockResolvedValue([online, waiting])

    const wrapper = mountPage()
    await flushPromises()

    const summaries = wrapper.findAll('.summary-bar article')
    expect(summaries.map((item) => item.text())).toEqual([
      '主机总数2',
      '在线1',
      '活动连接128',
      '需要处理1',
    ])
    expect(wrapper.findAll('tbody tr')).toHaveLength(2)
    expect(wrapper.text()).toContain('●在线')
    expect(wrapper.text()).toContain('○等待连接')
    expect(wrapper.text()).toContain(endpoint)
    expect(wrapper.text()).toContain('华东')
    expect(wrapper.text()).toContain('生产环境')

    wrapper.unmount()
  })

  it('creates a Relay directly with region and group and returns a persistent Access Key', async () => {
    const draft = makeInstallation('draft')
    const pending = makeInstallation('pending_enrollment', { version: 4 })
    const configured = makeInstallation('pending_enrollment', {
      version: 5,
      publicEndpoint: endpoint,
      listenerPort: 18443,
    })
    const accessKey = `relay_${'x'.repeat(43)}`
    vi.mocked(createRelayInstallation).mockResolvedValue(draft)
    vi.mocked(getRelayInstallation).mockResolvedValue(pending)
    vi.mocked(updateRelayInstallation).mockResolvedValue(configured)
    vi.mocked(createRelayAccessKey).mockResolvedValue({
      id: '51700000-0000-4000-8000-000000000001',
      installationId,
      key: accessKey,
      keyPrefix: accessKey.slice(0, 16),
      createdAt: now,
    })

    const wrapper = mountPage()
    await flushPromises()
    await buttonWithText(wrapper, '添加主机').trigger('click')

    expect(wrapper.text()).toContain('地区（可选）')
    expect(wrapper.text()).toContain('分组（可选）')
    expect(wrapper.get('form.host-form').text()).not.toContain('默认中继组')
    expect(wrapper.find('select[name="platform"]').exists()).toBe(true)
    expect(wrapper.find('select[name="architecture"]').exists()).toBe(true)
    expect(wrapper.find('select[name="releaseId"]').exists()).toBe(false)
    await wrapper.get('.host-form input').setValue('relay-cn-01')
    await wrapper.get('input[placeholder="例如 华东"]').setValue('华东')
    await wrapper.get('input[placeholder="例如 生产环境"]').setValue('生产环境')
    await wrapper.get('form.host-form').trigger('submit')
    await flushPromises()

    expect(createRelayInstallation).toHaveBeenCalledWith({
      releaseId: null,
      displayName: 'relay-cn-01',
      region: '华东',
      group: '生产环境',
      failureDomain: '',
      operationsNote: '',
      publicEndpoint: '',
      listenerPort: 8443,
      platform: 'linux',
      architecture: 'amd64',
    })
    expect(createRelayAccessKey).toHaveBeenCalledWith(installationId)
    expect(wrapper.text()).toContain(accessKey)
    expect(wrapper.text()).toContain('Access Key 只显示一次')
    expect(wrapper.text()).toContain('Key 默认不过期')
    await wrapper.get('input[name="accessListenerPort"]').setValue('18443')
    await wrapper.get('input[name="accessPublicEndpoint"]').setValue(endpoint)
    await buttonWithText(wrapper, '保存运行配置').trigger('click')
    await flushPromises()
    expect(updateRelayInstallation).toHaveBeenCalledWith(installationId, {
      displayName: pending.displayName,
      region: pending.region,
      group: pending.group,
      failureDomain: pending.failureDomain,
      operationsNote: pending.operationsNote,
      publicEndpoint: endpoint,
      listenerPort: 18443,
      deploymentChecklist: pending.deploymentChecklist,
      expectedVersion: pending.version,
    })
    const envFile = wrapper.get('.enrollment-command').text()
    expect(envFile).toBe(`RELAY_ACCESS_KEY=${accessKey}`)
    expect(envFile).not.toContain('RELAY_MANAGEMENT_URL')
    expect(envFile).not.toContain('RELAY_PUBLIC_ENDPOINT')
    expect(envFile).not.toContain('PUBLIC_KEY_FILES')
    expect(envFile).not.toContain('relayctl enroll')

    await buttonWithText(wrapper, '完成').trigger('click')
    await flushPromises()
    expect(wrapper.text()).not.toContain(accessKey)
    wrapper.unmount()
  })

  it('keeps the Relay WS listener independent from the client-facing WSS URL', async () => {
    const created = makeInstallation('pending_enrollment', {
      publicEndpoint: endpoint,
      listenerPort: 18443,
    })
    const accessKey = `relay_${'p'.repeat(43)}`
    vi.mocked(createRelayInstallation).mockResolvedValue(created)
    vi.mocked(createRelayAccessKey).mockResolvedValue({
      id: '51700000-0000-4000-8000-000000000003',
      installationId,
      key: accessKey,
      keyPrefix: accessKey.slice(0, 16),
      createdAt: now,
    })
    vi.mocked(getRelayInstallation).mockResolvedValue(created)

    const wrapper = mountPage()
    await flushPromises()
    await buttonWithText(wrapper, '添加主机').trigger('click')

    await wrapper.get('input[placeholder="例如 relay-cn-01"]').setValue('relay-wss-01')
    await wrapper.get('input[name="listenerPort"]').setValue('70000')
    await wrapper.get('input[name="publicEndpoint"]').setValue(endpoint)
    expect(buttonWithText(wrapper, '创建并生成 Access Key').attributes('disabled')).toBeDefined()

    await wrapper.get('input[name="listenerPort"]').setValue('18443')
    expect(wrapper.text()).toContain('请在 Nginx 配置证书与私钥')
    expect(wrapper.find('textarea[name="tlsPrivateKeyPem"]').exists()).toBe(false)
    expect(buttonWithText(wrapper, '创建并生成 Access Key').attributes('disabled')).toBeUndefined()

    await wrapper.get('form.host-form').trigger('submit')
    await flushPromises()

    expect(createRelayInstallation).toHaveBeenCalledWith(
      expect.objectContaining({
        displayName: 'relay-wss-01',
        publicEndpoint: endpoint,
        listenerPort: 18443,
      }),
    )
    wrapper.unmount()
  })

  it('selects a published Release and generates an Access Key one-click install command', async () => {
    const release = makeRelease()
    const draft = makeInstallation('draft', { releaseId })
    const pending = makeInstallation('pending_enrollment', { releaseId })
    const accessKey = `relay_${'k'.repeat(43)}`
    const command = 'sudo bash install.sh --management-url https://control.test --access-key-stdin'
    vi.mocked(listRelayServerReleases).mockResolvedValue([release])
    vi.mocked(createRelayInstallation).mockResolvedValue(draft)
    vi.mocked(createRelayAccessKey).mockResolvedValue({
      id: '51700000-0000-4000-8000-000000000001',
      installationId,
      key: accessKey,
      keyPrefix: accessKey.slice(0, 16),
      createdAt: now,
    })
    vi.mocked(createRelayInstallSession).mockResolvedValue({
      session: {} as never,
      installCommand: command,
    })
    vi.mocked(getRelayInstallation).mockResolvedValue(pending)
    let generatedBlob: Blob | undefined
    let downloadedName = ''
    Object.defineProperty(URL, 'createObjectURL', {
      configurable: true,
      value: vi.fn((blob: Blob) => {
        generatedBlob = blob
        return 'blob:wenzwork-relay-installer'
      }),
    })
    Object.defineProperty(URL, 'revokeObjectURL', { configurable: true, value: vi.fn() })
    const click = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(function (
      this: HTMLAnchorElement,
    ) {
      downloadedName = this.download
    })

    const wrapper = mountPage()
    await flushPromises()
    await buttonWithText(wrapper, '添加主机').trigger('click')
    await flushPromises()

    expect((wrapper.get('select[name="releaseId"]').element as HTMLSelectElement).value).toBe(
      releaseId,
    )
    expect((wrapper.get('select[name="platform"]').element as HTMLSelectElement).value).toBe(
      'linux',
    )
    expect((wrapper.get('select[name="architecture"]').element as HTMLSelectElement).value).toBe(
      'amd64',
    )
    await wrapper.get('.host-form input').setValue('relay-cn-install')
    await wrapper.get('form.host-form').trigger('submit')
    await flushPromises()

    expect(createRelayInstallation).toHaveBeenCalledWith(
      expect.objectContaining({ releaseId, displayName: 'relay-cn-install' }),
    )
    expect(createRelayInstallSession).toHaveBeenCalledWith(
      installationId,
      releaseId,
      'script',
      'install',
    )
    expect(wrapper.text()).toContain(command)
    expect(command).toContain('--access-key-stdin')
    expect(command).not.toContain(accessKey)
    await buttonWithText(wrapper, '下载一键部署脚本').trigger('click')
    expect(generatedBlob).toBeDefined()
    const script = await generatedBlob!.text()
    expect(script).toContain('#!/usr/bin/env bash')
    expect(script).toContain(command)
    expect(script).not.toContain(accessKey)
    expect(downloadedName).toBe('wenzwork-relay-install-v1.2.3-linux-amd64.sh')
    click.mockRestore()
    wrapper.unmount()
  })

  it('filters install Releases by the selected host platform and architecture', async () => {
    const arm64Release = makeArm64Release()
    const windowsArm64Release = makeWindowsArm64Release()
    const mismatchedRelease: RelayServerRelease = {
      ...makeRelease(),
      id: '61700000-0000-4000-8000-000000000004',
      version: '9.9.9',
    }
    const draft = makeInstallation('draft', {
      releaseId: windowsArm64ReleaseId,
      platform: 'windows',
      architecture: 'arm64',
    })
    const pending = makeInstallation('pending_enrollment', {
      releaseId: windowsArm64ReleaseId,
      platform: 'windows',
      architecture: 'arm64',
    })
    vi.mocked(listRelayServerReleases).mockResolvedValue([
      mismatchedRelease,
      makeRelease(),
      arm64Release,
      windowsArm64Release,
    ])
    vi.mocked(createRelayInstallation).mockResolvedValue(draft)
    vi.mocked(createRelayAccessKey).mockResolvedValue({
      id: '51700000-0000-4000-8000-000000000002',
      installationId,
      key: `relay_${'a'.repeat(43)}`,
      keyPrefix: `relay_${'a'.repeat(10)}`,
      createdAt: now,
    })
    const command = "Write-Host 'install relay'"
    vi.mocked(createRelayInstallSession).mockResolvedValue({
      session: {} as never,
      installCommand: command,
    })
    vi.mocked(getRelayInstallation).mockResolvedValue(pending)
    let generatedBlob: Blob | undefined
    let downloadedName = ''
    Object.defineProperty(URL, 'createObjectURL', {
      configurable: true,
      value: vi.fn((blob: Blob) => {
        generatedBlob = blob
        return 'blob:wenzwork-relay-windows-installer'
      }),
    })
    Object.defineProperty(URL, 'revokeObjectURL', { configurable: true, value: vi.fn() })
    const click = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(function (
      this: HTMLAnchorElement,
    ) {
      downloadedName = this.download
    })

    const wrapper = mountPage()
    await flushPromises()
    await buttonWithText(wrapper, '添加主机').trigger('click')
    await flushPromises()

    const platform = wrapper.get('select[name="platform"]')
    const architecture = wrapper.get('select[name="architecture"]')
    const release = wrapper.get('select[name="releaseId"]')
    expect(release.text()).toContain('linux/amd64')
    expect(release.text()).not.toContain('linux/arm64')
    expect(release.text()).not.toContain('9.9.9')

    await architecture.setValue('arm64')
    await flushPromises()
    expect(release.text()).toContain('linux/arm64')
    expect(release.text()).not.toContain('linux/amd64')
    expect((release.element as HTMLSelectElement).value).toBe(arm64ReleaseId)

    await platform.setValue('windows')
    await flushPromises()
    expect(release.text()).toContain('windows/arm64')
    expect(release.text()).not.toContain('linux/arm64')
    expect((release.element as HTMLSelectElement).value).toBe(windowsArm64ReleaseId)

    await wrapper.get('.host-form input').setValue('relay-arm64-01')
    await wrapper.get('form.host-form').trigger('submit')
    await flushPromises()

    expect(createRelayInstallation).toHaveBeenCalledWith(
      expect.objectContaining({
        releaseId: windowsArm64ReleaseId,
        platform: 'windows',
        architecture: 'arm64',
      }),
    )
    expect(wrapper.text()).toContain('当前脚本目标为 Windows / arm64')
    await buttonWithText(wrapper, '下载一键部署脚本').trigger('click')
    expect(generatedBlob).toBeDefined()
    const script = await generatedBlob!.text()
    expect(script).toContain('#Requires -Version 5.1')
    expect(script).toContain(command)
    expect(downloadedName).toBe('wenzwork-relay-install-v1.2.3-windows-arm64.ps1')
    click.mockRestore()
    wrapper.unmount()
  })

  it('updates an active WSS address and generates a signed Release upgrade entry', async () => {
    const online = makeInstallation('active', {
      releaseId,
      publicEndpoint: endpoint,
    })
    const nextEndpoint = 'wss://relay-new.example.test/v2/connect'
    const updated = makeInstallation('active', {
      releaseId,
      publicEndpoint: nextEndpoint,
      listenerPort: 19443,
      version: online.version + 1,
    })
    vi.mocked(listRelayInstallations).mockResolvedValue([online])
    vi.mocked(listRelayServerReleases).mockResolvedValue([makeRelease()])
    vi.mocked(getRelayInstallation).mockResolvedValue(online)
    vi.mocked(updateRelayInstallation).mockResolvedValue(updated)
    vi.mocked(createRelayInstallSession).mockResolvedValue({
      session: {} as never,
      installCommand: 'sudo bash upgrade.sh --artifact-url https://downloads.test/relay.tar.gz',
    })

    const wrapper = mountPage()
    await flushPromises()
    await wrapper.get('.table-action').trigger('click')
    await flushPromises()
    await wrapper.get('input[name="detailPublicEndpoint"]').setValue(nextEndpoint)
    await wrapper.get('input[name="detailListenerPort"]').setValue('19443')
    await buttonWithText(wrapper, '保存接入配置').trigger('click')
    await flushPromises()

    expect(updateRelayInstallation).toHaveBeenCalledWith(
      installationId,
      expect.objectContaining({
        publicEndpoint: nextEndpoint,
        listenerPort: 19443,
        expectedVersion: online.version,
      }),
    )
    await buttonWithText(wrapper, '生成升级命令').trigger('click')
    await flushPromises()
    expect(createRelayInstallSession).toHaveBeenCalledWith(
      installationId,
      releaseId,
      'script',
      'upgrade',
    )
    expect(wrapper.text()).toContain('upgrade.sh')
    wrapper.unmount()
  })

  it('publishes a Relay Release draft from the metadata catalog', async () => {
    const draft = makeRelease('draft')
    vi.mocked(listRelayServerReleases).mockResolvedValue([draft])
    vi.mocked(publishRelayServerRelease).mockResolvedValue({ ...draft, status: 'published' })

    const wrapper = mountPage()
    await flushPromises()
    await buttonWithText(wrapper, '发布').trigger('click')
    await flushPromises()

    expect(publishRelayServerRelease).toHaveBeenCalledWith(releaseId)
    wrapper.unmount()
  })

  it('creates Relay Release metadata for a selected macOS arm64 target', async () => {
    const arm64Release = makeArm64Release()
    vi.mocked(createRelayServerRelease).mockResolvedValue({ ...arm64Release, platform: 'darwin' })

    const wrapper = mountPage()
    await flushPromises()
    await buttonWithText(wrapper, '添加版本元数据').trigger('click')
    await wrapper.get('select[name="releasePlatform"]').setValue('darwin')
    await wrapper.get('select[name="releaseArchitecture"]').setValue('arm64')
    await wrapper.get('.release-dialog form').trigger('submit')
    await flushPromises()

    expect(createRelayServerRelease).toHaveBeenCalledWith(
      expect.objectContaining({ platform: 'darwin', architecture: 'arm64' }),
    )
    wrapper.unmount()
  })

  it('requires the fingerprint plus all four checks before activation', async () => {
    const pending = makeInstallation('pending_activation', {
      deploymentChecklist: { lb: false, dns: false, port: false, tls: false },
    })
    const active = makeInstallation('active')
    vi.mocked(listRelayInstallations).mockResolvedValue([pending])
    vi.mocked(getRelayInstallation).mockResolvedValue(pending)
    vi.mocked(activateRelayInstallation).mockResolvedValue(active)

    const wrapper = mountPage()
    await flushPromises()
    await wrapper.get('.table-action').trigger('click')
    await flushPromises()

    const activation = wrapper.get('.activation-panel')
    const activateButton = buttonWithText(wrapper, '核对完成并启用')
    expect(activateButton.attributes('disabled')).toBeDefined()
    const confirmations = activation.findAll('input[type="checkbox"]')
    expect(confirmations).toHaveLength(5)
    for (const confirmation of confirmations) await confirmation.setValue(true)
    expect(activateButton.attributes('disabled')).toBeUndefined()
    await activateButton.trigger('click')
    await flushPromises()

    expect(activateRelayInstallation).toHaveBeenCalledWith(installationId, 'c'.repeat(64), {
      lb: true,
      dns: true,
      port: true,
      tls: true,
    })
    expect(wrapper.text()).toContain('主机已启用')
    wrapper.unmount()
  })

  it('supports pause, resume, and irreversible revocation from host details', async () => {
    const online = makeInstallation('active')
    vi.mocked(listRelayInstallations).mockResolvedValue([online])
    vi.mocked(getRelayInstallation).mockResolvedValue(online)
    vi.mocked(drainRelayNode).mockResolvedValue({} as never)
    vi.mocked(activateRelayInstallation).mockResolvedValue(online)
    vi.mocked(revokeRelayInstallation).mockResolvedValue(undefined)
    vi.mocked(deleteRelayInstallation).mockResolvedValue(undefined)

    const wrapper = mountPage()
    await flushPromises()
    await wrapper.get('.table-action').trigger('click')
    await flushPromises()

    await buttonWithText(wrapper, '暂停接入').trigger('click')
    await flushPromises()
    expect(drainRelayNode).toHaveBeenCalledWith(instanceId)
    expect(wrapper.text()).toContain('排空中')

    await buttonWithText(wrapper, '恢复接入').trigger('click')
    await flushPromises()
    expect(activateRelayInstallation).toHaveBeenCalledWith(
      installationId,
      'c'.repeat(64),
      online.deploymentChecklist,
    )

    await buttonWithText(wrapper, '吊销主机').trigger('click')
    await flushPromises()
    expect(revokeRelayInstallation).toHaveBeenCalledWith(installationId)
    expect(wrapper.text()).toContain('永久删除主机')

    await buttonWithText(wrapper, '永久删除主机').trigger('click')
    await flushPromises()
    expect(deleteRelayInstallation).toHaveBeenCalledWith(installationId)
    expect(window.confirm).toHaveBeenCalledTimes(4)
    wrapper.unmount()
  })

  it('retains old data on a refresh failure and hides it on 403', async () => {
    const online = makeInstallation('active')
    vi.mocked(listRelayInstallations).mockResolvedValue([online])
    const wrapper = mountPage()
    await flushPromises()

    vi.mocked(listRelayInstallations).mockRejectedValueOnce(new Error('network unavailable'))
    await buttonWithText(wrapper, '刷新').trigger('click')
    await flushPromises()
    expect(wrapper.get('[role="alert"]').text()).toContain('页面保留上一次成功数据')
    expect(wrapper.text()).toContain('relay-cn-01')

    vi.mocked(listRelayInstallations).mockRejectedValueOnce({ response: { status: 403 } })
    await buttonWithText(wrapper, '重试').trigger('click')
    await flushPromises()
    expect(wrapper.get('[role="alert"]').text()).toContain('没有中继主机管理权限')
    expect(wrapper.text()).not.toContain('relay-cn-01')
    expect(wrapper.text()).not.toContain(endpoint)
    wrapper.unmount()
  })
})
