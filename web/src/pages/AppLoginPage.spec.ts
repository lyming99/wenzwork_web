import { createHead } from '@unhead/vue/client'
import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { createMemoryHistory, createRouter } from 'vue-router'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import {
  approveDeviceAuthorization,
  denyDeviceAuthorization,
  getDeviceAuthorizationForBrowser,
} from '@/api/deviceAuth'
import { useAuthStore } from '@/stores/auth'

import AppLoginPage from './AppLoginPage.vue'

vi.mock('@/api/deviceAuth', () => ({
  getDeviceAuthorizationForBrowser: vi.fn(),
  approveDeviceAuthorization: vi.fn(),
  denyDeviceAuthorization: vi.fn(),
}))

const pendingAuthorization = {
  requestId: '11111111-1111-4111-8111-111111111111',
  clientId: 'wenzwork-desktop',
  deviceId: '22222222-2222-4222-8222-222222222222',
  deviceName: 'DESKTOP-TEST',
  userCode: 'ABCD-EFGH',
  scope: 'profile.read membership.read',
  status: 'pending' as const,
  expiresAt: '2026-07-22T12:10:00Z',
}

describe('AppLoginPage', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    setActivePinia(createPinia())
    useAuthStore().user = {
      id: 'user-1',
      email: 'member@example.test',
      displayName: 'Member',
      status: 'active',
      emailVerifiedAt: '2026-07-21T00:00:00Z',
      roles: ['user'],
    }
    vi.mocked(getDeviceAuthorizationForBrowser).mockResolvedValue(pendingAuthorization)
    vi.mocked(approveDeviceAuthorization).mockResolvedValue({
      ...pendingAuthorization,
      status: 'approved',
    })
    vi.mocked(denyDeviceAuthorization).mockResolvedValue(undefined)
  })

  it('requires an explicit confirmation before authorizing and offers a credential-free app return link', async () => {
    const router = createRouter({
      history: createMemoryHistory(),
      routes: [{ path: '/app-login', component: AppLoginPage }],
    })
    await router.push('/app-login?code=ABCD-EFGH')
    await router.isReady()
    const wrapper = mount(AppLoginPage, { global: { plugins: [router, createHead()] } })
    await flushPromises()

    expect(getDeviceAuthorizationForBrowser).toHaveBeenCalledWith('ABCD-EFGH')
    expect(approveDeviceAuthorization).not.toHaveBeenCalled()
    expect(wrapper.text()).toContain('DESKTOP-TEST')
    expect(wrapper.text()).toContain('member@example.test')

    await wrapper.get('button.button').trigger('click')
    await flushPromises()

    expect(approveDeviceAuthorization).toHaveBeenCalledWith('ABCD-EFGH')
    const returnLink = wrapper.get('a[href^="wenzwork://"]')
    expect(returnLink.attributes('href')).toBe(
      'wenzwork://auth/return?request=11111111-1111-4111-8111-111111111111',
    )
    expect(returnLink.attributes('href')).not.toContain('token')
  })
})
