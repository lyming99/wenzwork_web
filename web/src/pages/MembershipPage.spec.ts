import { createHead } from '@unhead/vue/client'
import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import { getMembership, listRedemptions } from '@/api/membership'
import { listRemoteDevices, type RemoteDevice } from '@/api/remote'

import MembershipPage from './MembershipPage.vue'

vi.mock('@/api/membership', () => ({
  getMembership: vi.fn(),
  listRedemptions: vi.fn(),
  redeemMembershipCode: vi.fn(),
}))

vi.mock('@/api/remote', () => ({ listRemoteDevices: vi.fn() }))

const device = (overrides: Partial<RemoteDevice> = {}): RemoteDevice => ({
  id: 'device-1',
  installationDeviceId: 'installation-1',
  deviceName: '工作站 DEV-01',
  platform: 'windows',
  agentVersion: '3.2.0',
  status: 'active',
  presence: 'online',
  capabilities: ['remote.peer.query'],
  scopes: ['remote.peer.query'],
  grantVersion: 1,
  lastSeenAt: '2026-08-08T00:00:00Z',
  lastSyncAt: '2026-08-08T00:00:00Z',
  remoteEnabledAt: '2026-08-08T00:00:00Z',
  connectionMode: 'relay',
  directModeEnabled: false,
  directAvailable: false,
  directTlsEnabled: false,
  directIp: null,
  directPort: null,
  ...overrides,
})

describe('MembershipPage device module', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    vi.mocked(getMembership).mockResolvedValue({
      planCode: 'pro',
      planName: 'Pro',
      startsAt: '2026-08-01T00:00:00Z',
      expiresAt: '2027-08-01T00:00:00Z',
      lifetime: false,
    })
    vi.mocked(listRedemptions).mockResolvedValue([])
    vi.mocked(listRemoteDevices).mockResolvedValue({
      items: [device(), device({ id: 'device-2', deviceName: '离线笔记本', presence: 'offline' })],
      nextCursor: null,
      observedAt: '2026-08-08T00:00:00Z',
    })
  })

  it('shows the current account devices and online summary inside membership', async () => {
    const wrapper = mount(MembershipPage, {
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
    await flushPromises()

    expect(listRemoteDevices).toHaveBeenCalledWith(undefined, 4)
    expect(wrapper.get('#member-devices-title').text()).toBe('我的设备')
    expect(wrapper.text()).toContain('共 2 台，1 台在线')
    expect(wrapper.text()).toContain('工作站 DEV-01')
    expect(wrapper.text()).toContain('离线笔记本')

    wrapper.unmount()
  })
})
