import { createPinia, setActivePinia } from 'pinia'
import { ref } from 'vue'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import type { RemoteScope } from '@/api/remote'
import type { RemoteAgentCapabilities } from '@/remote/peerClient'
import { useAuthStore } from '@/stores/auth'

const mocks = vi.hoisted(() => ({
  loadIdentity: vi.fn(),
  getController: vi.fn(),
  registerController: vi.fn(),
  issueDeviceLink: vi.fn(),
}))

vi.mock('@/remote/peerIdentity', () => ({
  loadBrowserControllerIdentity: mocks.loadIdentity,
  nextConnectionEpoch: vi.fn().mockResolvedValue(1),
  signControllerRegistration: vi.fn().mockReturnValue('proof'),
}))

vi.mock('@/api/remote', () => ({
  createRemoteIdempotencyKey: vi.fn().mockReturnValue('idempotency-key'),
  getBrowserController: mocks.getController,
  registerBrowserController: mocks.registerController,
}))

vi.mock('./deviceLink', () => ({ issueDeviceLink: mocks.issueDeviceLink }))

import { createRemotePeerClientV2, type V2ClientDependencies } from './client'

const dependencies: V2ClientDependencies = {
  scopeForMethod: (): RemoteScope => 'remote.peer.query',
  parseCapabilities: (value) => value as RemoteAgentCapabilities,
  timeoutFor: () => 1000,
  projectRequired: () => false,
}

describe('remote/v2 offline reconnect lifecycle', () => {
  beforeEach(() => {
    vi.useFakeTimers()
    setActivePinia(createPinia())
    const auth = useAuthStore()
    auth.applyAccount({
      user: {
        id: crypto.randomUUID(),
        email: 'offline@example.test',
        displayName: 'Offline test',
        status: 'active',
        emailVerifiedAt: '2026-08-19T00:00:00Z',
        roles: ['user'],
      },
      permissions: ['remote.connect'],
      mfaEnforced: false,
      assuranceLevel: 1,
      absoluteExpiresAt: '2026-08-20T00:00:00Z',
    })
    mocks.loadIdentity.mockReset().mockResolvedValue({ controllerId: crypto.randomUUID() })
    mocks.getController.mockReset().mockRejectedValue({
      isAxiosError: true,
      response: { status: 403, headers: {} },
    })
    mocks.registerController.mockReset()
    mocks.issueDeviceLink.mockReset()
    Object.defineProperty(navigator, 'onLine', { configurable: true, value: false })
  })

  afterEach(() => {
    vi.useRealTimers()
    vi.restoreAllMocks()
    Object.defineProperty(navigator, 'onLine', { configurable: true, value: true })
  })

  it('waits for the online signal without issuing network work while offline', async () => {
    const client = createRemotePeerClientV2(ref(crypto.randomUUID()), dependencies)

    await expect(client.connect()).rejects.toThrow('网络已离线')
    expect(client.reconnecting.value).toBe(true)
    expect(mocks.loadIdentity).not.toHaveBeenCalled()
    expect(mocks.issueDeviceLink).not.toHaveBeenCalled()

    await client.close()
    Object.defineProperty(navigator, 'onLine', { configurable: true, value: true })
    window.dispatchEvent(new Event('online'))
    await vi.advanceTimersByTimeAsync(1000)
    expect(mocks.loadIdentity).not.toHaveBeenCalled()
  })

  it('wakes exactly one jittered connection attempt when the network returns', async () => {
    const client = createRemotePeerClientV2(ref(crypto.randomUUID()), dependencies)
    await expect(client.connect()).rejects.toThrow('网络已离线')

    Object.defineProperty(navigator, 'onLine', { configurable: true, value: true })
    window.dispatchEvent(new Event('online'))
    window.dispatchEvent(new Event('online'))
    await vi.advanceTimersByTimeAsync(1000)

    expect(mocks.loadIdentity).toHaveBeenCalledTimes(1)
    expect(mocks.getController).toHaveBeenCalledTimes(1)
    expect(mocks.issueDeviceLink).not.toHaveBeenCalled()
    expect(client.reconnecting.value).toBe(false)
    await client.close()
  })
})
