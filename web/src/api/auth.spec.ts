import { beforeEach, describe, expect, it, vi } from 'vitest'

import { apiClient } from './client'
import {
  beginTOTPEnrollment,
  changePassword,
  confirmTOTPEnrollment,
  disableTOTP,
  listSessions,
  loginAccount,
  registerAccount,
  revokeSession,
  verifyLoginMFA,
} from './auth'

vi.mock('./client', () => ({
  apiClient: { get: vi.fn(), post: vi.fn(), patch: vi.fn(), delete: vi.fn() },
}))

describe('authentication API client', () => {
  beforeEach(() => vi.clearAllMocks())

  it('uses the authentication endpoints without handling session tokens in JSON', async () => {
    vi.mocked(apiClient.post).mockResolvedValueOnce({ data: { message: 'accepted' } })
    await expect(
      registerAccount({
        email: 'reader@example.com',
        password: 'a secure passphrase',
        displayName: 'Reader',
      }),
    ).resolves.toEqual({ message: 'accepted' })
    expect(apiClient.post).toHaveBeenCalledWith('/auth/register', {
      email: 'reader@example.com',
      password: 'a secure passphrase',
      displayName: 'Reader',
    })

    vi.mocked(apiClient.post).mockResolvedValueOnce({ data: { user: { id: 'user-1' } } })
    const result = await loginAccount({
      email: 'reader@example.com',
      password: 'secret',
      rememberMe: false,
    })
    expect(result).not.toHaveProperty('token')
    expect(apiClient.post).toHaveBeenLastCalledWith('/auth/login', {
      email: 'reader@example.com',
      password: 'secret',
      rememberMe: false,
    })
  })

  it('supports password and session security operations', async () => {
    vi.mocked(apiClient.patch).mockResolvedValueOnce({ data: null })
    await changePassword('old password', 'new secure password', true)
    expect(apiClient.patch).toHaveBeenCalledWith('/me/password', {
      currentPassword: 'old password',
      newPassword: 'new secure password',
      revokeOthers: true,
    })

    vi.mocked(apiClient.get).mockResolvedValueOnce({ data: { items: [{ id: 'session-1' }] } })
    await expect(listSessions()).resolves.toEqual([{ id: 'session-1' }])

    vi.mocked(apiClient.delete).mockResolvedValueOnce({ data: null })
    await revokeSession('session/unsafe')
    expect(apiClient.delete).toHaveBeenCalledWith('/me/sessions/session%2Funsafe')
  })

  it('uses session-bound endpoints for MFA enrollment, verification, and disable', async () => {
    vi.mocked(apiClient.post)
      .mockResolvedValueOnce({
        data: { secret: 'BASE32', otpauthUri: 'otpauth://totp/WenzWork' },
      })
      .mockResolvedValueOnce({ data: { recoveryCodes: ['CODE-ONE'] } })
      .mockResolvedValueOnce({ data: { assuranceLevel: 2, user: { id: 'user-1' } } })

    await beginTOTPEnrollment('current password')
    await confirmTOTPEnrollment('123456')
    await verifyLoginMFA('ABCD-EFGH-JKLM-NPQR')

    expect(apiClient.post).toHaveBeenNthCalledWith(1, '/me/mfa/totp', {
      currentPassword: 'current password',
    })
    expect(apiClient.post).toHaveBeenNthCalledWith(2, '/me/mfa/totp/confirm', { code: '123456' })
    expect(apiClient.post).toHaveBeenNthCalledWith(3, '/auth/mfa/totp/verify', {
      code: 'ABCD-EFGH-JKLM-NPQR',
    })

    vi.mocked(apiClient.delete).mockResolvedValueOnce({ data: null })
    await disableTOTP('current password', '654321')
    expect(apiClient.delete).toHaveBeenCalledWith('/me/mfa/totp', {
      data: { currentPassword: 'current password', code: '654321' },
    })
  })
})
