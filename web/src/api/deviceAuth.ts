import { apiClient } from './client'
import type { components } from './schema'

export type BrowserDeviceAuthorization = components['schemas']['BrowserDeviceAuthorization']

export const getDeviceAuthorizationForBrowser = async (userCode: string) =>
  (
    await apiClient.get<BrowserDeviceAuthorization>('/oauth/device-authorization', {
      params: { userCode },
    })
  ).data

export const approveDeviceAuthorization = async (userCode: string) =>
  (
    await apiClient.post<BrowserDeviceAuthorization>('/oauth/device-authorization/approve', {
      userCode,
    })
  ).data

export const denyDeviceAuthorization = async (userCode: string) => {
  await apiClient.post('/oauth/device-authorization/deny', { userCode })
}
