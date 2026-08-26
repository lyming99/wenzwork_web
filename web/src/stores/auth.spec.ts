import { createPinia, setActivePinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import {
  getCurrentAccount,
  isAuthenticationError,
  LoginSessionUnavailableError,
  loginAccount,
} from '@/api/auth'

import { useAuthStore } from './auth'

vi.mock('@/api/auth', () => ({
  getCurrentAccount: vi.fn(),
  isAuthenticationError: vi.fn(),
  LoginSessionUnavailableError: class LoginSessionUnavailableError extends Error {},
  loginAccount: vi.fn(),
  logoutAccount: vi.fn(),
}))

const user = {
  id: 'bd594d6b-d280-4b6e-a9f5-e969cd060ed0',
  email: 'admin@example.test',
  displayName: 'Admin',
  status: 'active',
  emailVerifiedAt: '2026-07-21T00:00:00Z',
  roles: ['super_admin'],
}

const account = {
  user,
  permissions: ['account.read', 'admin.super'],
  mfaEnforced: true,
  assuranceLevel: 1,
  absoluteExpiresAt: '2026-07-22T00:00:00Z',
  systemSetupRequired: true,
}

describe('auth store administrator detection', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    vi.clearAllMocks()
    vi.mocked(isAuthenticationError).mockReturnValue(false)
  })

  it('recognizes the dotted admin permission namespace used by the API', () => {
    const auth = useAuthStore()
    auth.applyAccount({
      user: {
        id: 'bd594d6b-d280-4b6e-a9f5-e969cd060ed0',
        email: 'admin@example.test',
        displayName: 'Admin',
        status: 'active',
        emailVerifiedAt: '2026-07-21T00:00:00Z',
        roles: ['support_admin'],
      },
      permissions: ['account.read', 'admin.users.read'],
      mfaEnforced: true,
      assuranceLevel: 2,
      absoluteExpiresAt: '2026-07-22T00:00:00Z',
    })

    expect(auth.isAdministrator).toBe(true)
    expect(auth.hasPermission('admin.users.read')).toBe(true)
  })

  it('does not treat an ordinary account as an administrator', () => {
    const auth = useAuthStore()
    auth.applyAccount({
      user: {
        id: 'c892e79a-e4fa-46eb-b8ef-08fdc9b8d84a',
        email: 'user@example.test',
        displayName: 'User',
        status: 'active',
        emailVerifiedAt: '2026-07-21T00:00:00Z',
        roles: ['user'],
      },
      permissions: ['account.read', 'membership.redeem'],
      mfaEnforced: true,
      assuranceLevel: 1,
      absoluteExpiresAt: '2026-07-22T00:00:00Z',
    })

    expect(auth.isAdministrator).toBe(false)
  })

  it('confirms the browser session before exposing authenticated state', async () => {
    const loginResponse = { ...account, mfaRequired: true }
    vi.mocked(loginAccount).mockResolvedValue(loginResponse as never)
    vi.mocked(getCurrentAccount).mockResolvedValue(account as never)
    const auth = useAuthStore()

    await expect(
      auth.login({ email: 'admin@example.test', password: 'correct password', rememberMe: false }),
    ).resolves.toBe(loginResponse)

    expect(getCurrentAccount).toHaveBeenCalledTimes(1)
    expect(auth.user?.email).toBe('admin@example.test')
    expect(auth.systemSetupRequired).toBe(true)
  })

  it('clears the optimistic login when the session cookie is unavailable', async () => {
    const loginResponse = { ...account, mfaRequired: true }
    const authenticationFailure = new Error('401')
    vi.mocked(loginAccount).mockResolvedValue(loginResponse as never)
    vi.mocked(getCurrentAccount).mockRejectedValue(authenticationFailure)
    vi.mocked(isAuthenticationError).mockImplementation((error) => error === authenticationFailure)
    const auth = useAuthStore()

    await expect(
      auth.login({ email: 'admin@example.test', password: 'correct password', rememberMe: false }),
    ).rejects.toBeInstanceOf(LoginSessionUnavailableError)

    expect(auth.user).toBeNull()
    expect(auth.permissions).toEqual([])
    expect(auth.bootstrapStatus).toBe('ready')
  })
})
