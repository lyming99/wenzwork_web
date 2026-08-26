import { createHead } from '@unhead/vue/client'
import { flushPromises, mount } from '@vue/test-utils'
import { createPinia } from 'pinia'
import { ref } from 'vue'
import { createMemoryHistory, createRouter } from 'vue-router'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import { getRemoteDevice, listRemoteDevices, type RemoteDevice } from '@/api/remote'
import { createRemotePeerClient } from '@/remote/peerClient'

import RemoteDevicePage from './RemoteDevicePage.vue'

vi.mock('@/api/remote', async (importOriginal) => {
  const original = await importOriginal<typeof import('@/api/remote')>()
  return {
    ...original,
    enableRemoteDeviceAccess: vi.fn(),
    getRemoteDevice: vi.fn(),
    listRemoteDevices: vi.fn(),
    revokeBrowserController: vi.fn(),
    revokeRemoteDeviceAccess: vi.fn(),
  }
})

vi.mock('@/remote/peerClient', () => ({
  agentCapabilityCacheVersion: vi.fn(() => 'test-capability-v2'),
  agentSupportsProjectMethod: vi.fn(() => false),
  createRemotePeerClient: vi.fn(),
}))

const device = (id: string, name: string, presence: RemoteDevice['presence']): RemoteDevice => ({
  id,
  installationDeviceId: `installation-${id}`,
  deviceName: name,
  platform: 'windows',
  agentVersion: '3.2.0',
  status: 'active',
  presence,
  capabilities: [],
  scopes: ['remote.project.read', 'remote.peer.query'],
  grantVersion: 1,
  lastSeenAt: '2026-08-08T00:00:00Z',
  lastSyncAt: '2026-08-08T00:00:00Z',
  remoteEnabledAt: '2026-08-08T00:00:00Z',
})

describe('RemoteDevicePage workbench header', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    const devices = [
      device('device-1', '研发工作站', 'online'),
      device('device-2', '构建服务器', 'offline'),
    ]
    vi.mocked(getRemoteDevice).mockImplementation(async (id) =>
      devices.find((item) => item.id === id)!,
    )
    vi.mocked(listRemoteDevices).mockResolvedValue({
      items: devices,
      nextCursor: null,
      observedAt: '2026-08-08T00:00:00Z',
    })
    vi.mocked(createRemotePeerClient).mockReturnValue({
      connected: ref(false),
      reconnecting: ref(false),
      error: ref(''),
      connect: vi.fn(),
      close: vi.fn(),
      getCapabilities: vi.fn(async () => ({
        protocolMinimum: 1,
        protocolMaximum: 1,
        featureVersions: { projects: 2, files: 2, ai: 2 },
        features: { 'project.v2': true, 'file.v2': true, 'ai.v2': true },
        operatingSystem: 'windows',
        architecture: 'amd64',
        shells: ['powershell'],
        taskRunners: ['script'],
        resourceLimits: {},
      })),
      call: vi.fn(),
      stream: vi.fn(),
      startStream: <TDelta, TResult = unknown>(
        method: string,
        input: Record<string, unknown>,
        onDelta: (delta: TDelta) => void,
      ) => {
        void method
        void input
        void onDelta
        return {
          result: Promise.resolve(undefined as TResult),
          detach: async () => undefined,
        }
      },
      subscribeAgentEvents: vi.fn(async () => undefined),
      cancelAgentEventSubscriptions: vi.fn(async () => undefined),
      downloadFile: vi.fn(),
      downloadTaskLog: vi.fn(),
      uploadFile: vi.fn(),
    })
  })

  it('lists account devices, switches routes, and explains the on-demand WSS state', async () => {
    const router = createRouter({
      history: createMemoryHistory(),
      routes: [
        { path: '/account/remote', component: { template: '<div />' } },
        {
          path: '/remote/:deviceId',
          name: 'remote-app-device',
          component: RemoteDevicePage,
        },
      ],
    })
    await router.push('/remote/device-1')
    await router.isReady()
    const wrapper = mount(RemoteDevicePage, {
      global: {
        plugins: [createHead(), createPinia(), router],
        stubs: {
          RemoteProjectsPanel: true,
          RemoteTasksPanel: true,
          RemoteAIConfigPanel: true,
          RemoteChatPanel: true,
          RemoteFilesPanel: true,
          RemoteTerminalPanel: true,
          RemoteSidePanel: true,
        },
      },
    })
    await flushPromises()

    expect(wrapper.text()).toContain('按需连接')
    const panes = wrapper.get<HTMLElement>('.workspace-panes')
    const sidebarSeparator = wrapper.get('button[aria-label="调整左侧栏宽度"]')
    expect(panes.attributes('style')).toContain('--workspace-sidebar-width: 272px')
    await sidebarSeparator.trigger('keydown', { key: 'ArrowRight' })
    expect(panes.attributes('style')).toContain('--workspace-sidebar-width: 288px')
    await sidebarSeparator.trigger('keydown', { key: 'Home' })
    expect(panes.attributes('style')).toContain('--workspace-sidebar-width: 272px')

    const switcher = wrapper.get('select[aria-label="切换设备"]')
    expect(switcher.findAll('option').map((option) => option.text())).toEqual([
      '研发工作站 · 在线',
      '构建服务器 · 离线',
    ])

    await switcher.setValue('device-2')
    await flushPromises()
    expect(router.currentRoute.value.params.deviceId).toBe('device-2')
    expect(getRemoteDevice).toHaveBeenCalledWith('device-2')

    wrapper.unmount()
  })
})
