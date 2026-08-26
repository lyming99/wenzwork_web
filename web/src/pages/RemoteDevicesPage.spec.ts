import { createHead } from '@unhead/vue/client'
import { flushPromises, mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import {
  createDeviceAccessKey,
  createRemoteIdempotencyKey,
  deleteDeviceAccessKey,
  deleteRemoteDevice,
  listDeviceAccessKeys,
  listRemoteDevices,
  updateRemoteDevice,
  type RemoteDevice,
} from '@/api/remote'
import { getLatestRelease } from '@/api/catalog'
import { REMOTE_MANAGER_WINDOW_NAME } from '@/utils/remoteManagerWindow'

import RemoteDevicesPage from './RemoteDevicesPage.vue'

vi.mock('@/api/remote', async (importOriginal) => {
  const original = await importOriginal<typeof import('@/api/remote')>()
  return {
    ...original,
    createDeviceAccessKey: vi.fn(),
    createRemoteIdempotencyKey: vi.fn(),
    deleteDeviceAccessKey: vi.fn(),
    deleteRemoteDevice: vi.fn(),
    listDeviceAccessKeys: vi.fn(),
    listRemoteDevices: vi.fn(),
    revokeDeviceAccessKey: vi.fn(),
    rotateDeviceAccessKey: vi.fn(),
    updateRemoteDevice: vi.fn(),
  }
})

vi.mock('@/api/catalog', () => ({ getLatestRelease: vi.fn() }))

const mountPage = () =>
  mount(RemoteDevicesPage, {
    global: {
      plugins: [createHead()],
      stubs: {
        RouterLink: {
          props: ['to'],
          template: '<a :data-to="to"><slot /></a>',
        },
      },
    },
  })

const buttonWithText = (wrapper: ReturnType<typeof mountPage>, label: string) => {
  const button = wrapper.findAll('button').find((candidate) => candidate.text().includes(label))
  if (!button) throw new Error(`missing button: ${label}`)
  return button
}

const device = (overrides: Partial<RemoteDevice> = {}): RemoteDevice => ({
  id: 'device-1',
  installationDeviceId: 'installation-1',
  deviceName: '研发工作站',
  platform: 'windows',
  agentVersion: '3.2.0',
  status: 'active',
  presence: 'online',
  capabilities: [],
  scopes: ['remote.peer.query'],
  grantVersion: 1,
  lastSeenAt: '2026-08-08T00:00:00Z',
  lastSyncAt: '2026-08-08T00:00:00Z',
  remoteEnabledAt: '2026-08-08T00:00:00Z',
  ...overrides,
})

describe('RemoteDevicesPage', () => {
  afterEach(() => {
    vi.unstubAllGlobals()
    vi.restoreAllMocks()
  })

  beforeEach(() => {
    vi.clearAllMocks()
    vi.mocked(listRemoteDevices).mockResolvedValue({
      items: [],
      nextCursor: null,
      observedAt: '2026-08-08T00:00:00Z',
    })
    vi.mocked(listDeviceAccessKeys).mockResolvedValue([])
    vi.mocked(createRemoteIdempotencyKey).mockReturnValue('device-create-retry-1')
    vi.mocked(getLatestRelease).mockRejectedValue(new Error('not published'))
  })

  it('reuses the same idempotency key after an ambiguous Access Key creation failure', async () => {
    vi.mocked(createDeviceAccessKey)
      .mockRejectedValueOnce(new Error('response lost'))
      .mockResolvedValueOnce({
        id: 'key-1',
        label: '我的设备',
        key: `device_${'A'.repeat(43)}`,
        keyPrefix: 'device_AAAAA',
        scopes: ['remote.connect', 'remote.peer.query'],
        status: 'active',
        expiresAt: null,
        lastUsedAt: null,
        createdAt: '2026-08-08T00:00:00Z',
      })

    const wrapper = mountPage()
    await flushPromises()
    await buttonWithText(wrapper, '添加设备').trigger('click')
    const form = wrapper.get('form.key-create-row')
    await form.trigger('submit')
    await flushPromises()
    expect(wrapper.get('[role="alert"]').text()).toContain('无法创建设备 Access Key')

    await form.trigger('submit')
    await flushPromises()
    expect(createRemoteIdempotencyKey).toHaveBeenCalledTimes(1)
    expect(createDeviceAccessKey).toHaveBeenCalledTimes(2)
    expect(vi.mocked(createDeviceAccessKey).mock.calls[0]?.[1]).toBe('device-create-retry-1')
    expect(vi.mocked(createDeviceAccessKey).mock.calls[1]?.[1]).toBe('device-create-retry-1')
    expect(wrapper.text()).toContain(`device_${'A'.repeat(43)}`)

    wrapper.unmount()
  })

  it('creates a full-access device without exposing permission choices', async () => {
    vi.mocked(createDeviceAccessKey).mockResolvedValue({
      id: 'key-optional',
      label: '我的设备',
      key: `device_${'S'.repeat(43)}`,
      keyPrefix: 'device_SSSSS',
      scopes: [],
      status: 'active',
      expiresAt: null,
      lastUsedAt: null,
      createdAt: '2026-08-23T00:00:00Z',
    })

    const wrapper = mountPage()
    await flushPromises()
    await buttonWithText(wrapper, '添加设备').trigger('click')
    expect(wrapper.find('.key-scope-options').exists()).toBe(false)
    expect(wrapper.text()).toContain('自动开启远程控制')
    await wrapper.get('form.key-create-row').trigger('submit')
    await flushPromises()

    expect(createDeviceAccessKey).toHaveBeenCalledWith(
      { label: '我的设备' },
      'device-create-retry-1',
    )

    wrapper.unmount()
  })

  it('filters the loaded device cards by name, platform, and agent version', async () => {
    vi.mocked(listRemoteDevices).mockResolvedValue({
      items: [
        device(),
        device({
          id: 'device-2',
          deviceName: '构建服务器',
          platform: 'linux',
          agentVersion: '4.0.0',
          presence: 'offline',
        }),
      ],
      nextCursor: null,
      observedAt: '2026-08-08T00:00:00Z',
    })

    const wrapper = mountPage()
    await flushPromises()
    const search = wrapper.get('input[type="search"]')

    await search.setValue('Linux')
    expect(wrapper.text()).toContain('构建服务器')
    expect(wrapper.text()).not.toContain('研发工作站')
    expect(wrapper.text()).toContain('找到 1 台已加载设备')

    await search.setValue('9.9.9')
    expect(wrapper.text()).toContain('没有匹配的设备')

    wrapper.unmount()
  })

  it('opens a concrete device in the reusable standalone workspace window', async () => {
    vi.mocked(listRemoteDevices).mockResolvedValue({
      items: [device()],
      nextCursor: null,
      observedAt: '2026-08-08T00:00:00Z',
    })
    const focus = vi.fn()
    const open = vi.spyOn(window, 'open').mockReturnValue({ focus } as unknown as Window)

    const wrapper = mountPage()
    await flushPromises()
    await wrapper.get('a.device-card-link').trigger('click')

    expect(open).toHaveBeenCalledWith(
      '/remote/device-1',
      REMOTE_MANAGER_WINDOW_NAME,
      expect.stringContaining('popup=yes'),
    )
    expect(focus).toHaveBeenCalledOnce()
    wrapper.unmount()
  })

  it('shows desktop-style device details and persists a renamed device', async () => {
    vi.mocked(listRemoteDevices).mockResolvedValue({
      items: [device({ capabilities: ['project.v2', 'ai.v2'] })],
      nextCursor: null,
      observedAt: '2026-08-08T00:00:00Z',
    })
    vi.mocked(updateRemoteDevice).mockResolvedValue(
      device({ deviceName: '设计工作站', capabilities: ['project.v2', 'ai.v2'] }),
    )

    const wrapper = mountPage()
    await flushPromises()

    await buttonWithText(wrapper, '查看设备信息').trigger('click')
    expect(wrapper.get('[role="dialog"]').text()).toContain('installation-1')
    expect(wrapper.get('[role="dialog"]').text()).toContain('project.v2')
    await buttonWithText(wrapper, '关闭').trigger('click')

    await buttonWithText(wrapper, '修改设备').trigger('click')
    await wrapper.get('.device-edit-dialog input').setValue('  设计工作站  ')
    await wrapper.get('form.device-edit-dialog').trigger('submit')
    await flushPromises()

    expect(updateRemoteDevice).toHaveBeenCalledWith('device-1', '设计工作站')
    expect(wrapper.text()).toContain('设备名称已修改')
    expect(wrapper.text()).toContain('设计工作站')

    wrapper.unmount()
  })

  it('permanently deletes a confirmed revoked Access Key', async () => {
    vi.mocked(listDeviceAccessKeys).mockResolvedValue([
      {
        id: 'key-revoked',
        label: '旧工作站',
        keyPrefix: 'device_AAAAA',
        scopes: ['remote.connect'],
        status: 'revoked',
        expiresAt: null,
        lastUsedAt: null,
        createdAt: '2026-08-08T00:00:00Z',
      },
    ])
    vi.mocked(deleteDeviceAccessKey).mockResolvedValue(undefined)
    vi.stubGlobal(
      'confirm',
      vi.fn(() => true),
    )

    const wrapper = mountPage()
    await flushPromises()
    await buttonWithText(wrapper, '添加设备').trigger('click')
    await wrapper.get('button.key-delete').trigger('click')
    await flushPromises()

    expect(deleteDeviceAccessKey).toHaveBeenCalledWith('key-revoked')
    expect(wrapper.text()).not.toContain('旧工作站')

    wrapper.unmount()
  })

  it('permanently deletes a confirmed device and removes it from the list', async () => {
    vi.mocked(listRemoteDevices).mockResolvedValue({
      items: [device()],
      nextCursor: null,
      observedAt: '2026-08-08T00:00:00Z',
    })
    vi.mocked(deleteRemoteDevice).mockResolvedValue(undefined)
    vi.stubGlobal(
      'confirm',
      vi.fn(() => true),
    )

    const wrapper = mountPage()
    await flushPromises()
    await buttonWithText(wrapper, '删除设备').trigger('click')
    await flushPromises()

    expect(deleteRemoteDevice).toHaveBeenCalledWith('device-1')
    expect(wrapper.text()).toContain('还没有远程设备')

    wrapper.unmount()
  })

  it('downloads a Device Agent script with the new one-time Access Key', async () => {
    vi.mocked(getLatestRelease).mockResolvedValue({
      id: 'release-web-1',
      project: 'web',
      version: '2.0.0',
      channel: 'stable',
      title: 'Device Agent',
      summary: '',
      releaseNotes: '',
      publishedAt: '2026-08-21T00:00:00Z',
      assets: [
        {
          id: 'asset-device-1',
          platform: 'windows',
          architecture: 'x64',
          fileName: 'wenzwork-device-agent-deployment-2.0.0-windows-amd64.tar.gz',
          fileSizeBytes: 10_000,
          sha256: 'd'.repeat(64),
          signatureStatus: 'valid',
          downloadUrl: '/api/v1/release-assets/device/download',
        },
      ],
    })
    const plaintextKey = `device_${'D'.repeat(43)}`
    vi.mocked(createDeviceAccessKey).mockResolvedValue({
      id: 'key-device-1',
      label: '我的设备',
      key: plaintextKey,
      keyPrefix: 'device_DDDDD',
      scopes: ['remote.connect'],
      status: 'active',
      expiresAt: null,
      lastUsedAt: null,
      createdAt: '2026-08-21T00:00:00Z',
    })
    let generatedBlob: Blob | undefined
    let downloadedName = ''
    Object.defineProperty(URL, 'createObjectURL', {
      configurable: true,
      value: vi.fn((blob: Blob) => {
        generatedBlob = blob
        return 'blob:wenzwork-device-installer'
      }),
    })
    Object.defineProperty(URL, 'revokeObjectURL', { configurable: true, value: vi.fn() })
    vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(function (
      this: HTMLAnchorElement,
    ) {
      downloadedName = this.download
    })

    const wrapper = mountPage()
    await flushPromises()
    await buttonWithText(wrapper, '添加设备').trigger('click')
    await wrapper.get('form.key-create-row').trigger('submit')
    await flushPromises()
    const targetSelectors = wrapper.findAll<HTMLSelectElement>('.device-installer-target select')
    expect(targetSelectors.map((select) => select.element.value)).toEqual(['windows', 'x64'])
    await buttonWithText(wrapper, '下载一键安装脚本').trigger('click')

    expect(generatedBlob).toBeDefined()
    const script = await generatedBlob!.text()
    expect(script).toContain(`WENZWORK_DEVICE_ACCESS_KEY=${plaintextKey}`)
    expect(script).toContain('WENZWORK_CONTROL_URL=http://localhost:3000')
    expect(script).toContain("Join-Path $InstallRoot 'Start.ps1'")
    expect(script).toContain('-Background')
    expect(script).toContain('GITHUB_ACCESS_TOKEN=')
    expect(downloadedName).toBe('wenzwork-device-agent-install-v2.0.0-windows-amd64.ps1')

    await buttonWithText(wrapper, '下载 .env 配置').trigger('click')
    expect(generatedBlob).toBeDefined()
    const environment = await generatedBlob!.text()
    expect(environment).toContain(`WENZWORK_DEVICE_ACCESS_KEY=${plaintextKey}`)
    expect(environment).toContain('WENZWORK_CONTROL_URL=http://localhost:3000')
    expect(environment).toContain('WENZWORK_AGENT_SECRET_STORE=file')
    expect(downloadedName).toBe('.env')

    wrapper.unmount()
  })
})
