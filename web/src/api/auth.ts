import axios from 'axios'

import { apiClient } from './client'
import type { components } from './schema'

export type AuthMessage = components['schemas']['AuthMessage']
export type AuthUser = components['schemas']['AuthUser']
export type LoginResponse = components['schemas']['LoginResponse']
export type CurrentAccount = components['schemas']['CurrentAccountResponse']
export type SessionSummary = components['schemas']['SessionSummary']
export type MFAStatus = components['schemas']['MFAStatusResponse']
export type MFAEnrollment = components['schemas']['MFAEnrollmentResponse']
export type MFAConfirmation = components['schemas']['MFAConfirmationResponse']
export type RegisterRequest = components['schemas']['RegisterRequest']
export type LoginRequest = components['schemas']['LoginRequest']

interface ProblemResponse {
  code?: string
  detail?: string
  title?: string
}

export class LoginSessionUnavailableError extends Error {
  constructor(options?: ErrorOptions) {
    super(
      '登录凭据已通过验证，但浏览器未能建立会话。请通过 Host 配置的 HTTPS 地址访问，并确认反向代理保留 Set-Cookie 响应头。',
      options,
    )
    this.name = 'LoginSessionUnavailableError'
  }
}

export const problemDetails = (error: unknown) => {
  if (!axios.isAxiosError<ProblemResponse>(error)) return undefined
  return error.response?.data
}

export const problemMessage = (error: unknown, fallback: string) => {
  if (error instanceof LoginSessionUnavailableError) return error.message
  const problem = problemDetails(error)
  return problem?.detail || problem?.title || fallback
}

export const isAuthenticationError = (error: unknown) =>
  axios.isAxiosError(error) && error.response?.status === 401

export const registerAccount = async (request: RegisterRequest) =>
  (await apiClient.post<AuthMessage>('/auth/register', request)).data

export const resendVerification = async (email: string) =>
  (await apiClient.post<AuthMessage>('/auth/resend-verification', { email })).data

export const verifyEmail = async (token: string) =>
  (await apiClient.post<{ user: AuthUser }>('/auth/verify-email', { token })).data

export const loginAccount = async (request: LoginRequest) =>
  (await apiClient.post<LoginResponse>('/auth/login', request)).data

export const logoutAccount = async () => {
  await apiClient.post('/auth/logout')
}

export const requestPasswordReset = async (email: string) =>
  (await apiClient.post<AuthMessage>('/auth/forgot-password', { email })).data

export const resetPassword = async (token: string, newPassword: string) => {
  await apiClient.post('/auth/reset-password', { token, newPassword })
}

export const getCurrentAccount = async () => (await apiClient.get<CurrentAccount>('/me')).data

export const updateProfile = async (displayName: string) =>
  (await apiClient.patch<{ user: AuthUser }>('/me', { displayName })).data.user

export const changePassword = async (
  currentPassword: string,
  newPassword: string,
  revokeOthers: boolean,
) => {
  await apiClient.patch('/me/password', { currentPassword, newPassword, revokeOthers })
}

export const listSessions = async () =>
  (await apiClient.get<{ items: SessionSummary[] }>('/me/sessions')).data.items

export const revokeSession = async (sessionId: string) => {
  await apiClient.delete(`/me/sessions/${encodeURIComponent(sessionId)}`)
}

export const getMFAStatus = async () => (await apiClient.get<MFAStatus>('/me/mfa')).data

export const beginTOTPEnrollment = async (currentPassword: string) =>
  (await apiClient.post<MFAEnrollment>('/me/mfa/totp', { currentPassword })).data

export const confirmTOTPEnrollment = async (code: string) =>
  (await apiClient.post<MFAConfirmation>('/me/mfa/totp/confirm', { code })).data

export const verifyLoginMFA = async (code: string) =>
  (await apiClient.post<CurrentAccount>('/auth/mfa/totp/verify', { code })).data

export const regenerateRecoveryCodes = async (currentPassword: string) =>
  (
    await apiClient.post<components['schemas']['MFARecoveryCodesResponse']>(
      '/me/mfa/recovery-codes',
      { currentPassword },
    )
  ).data

export const disableTOTP = async (currentPassword: string, code: string) => {
  await apiClient.delete('/me/mfa/totp', { data: { currentPassword, code } })
}
