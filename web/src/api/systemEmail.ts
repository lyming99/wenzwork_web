import { apiClient } from './client'
import type { components } from './schema'

export type SystemEmailSettings = components['schemas']['SystemEmailSettings']
export type UpdateSystemEmailSettingsRequest =
  components['schemas']['UpdateSystemEmailSettingsRequest']
export type TestSystemEmailSettingsRequest = components['schemas']['TestSystemEmailSettingsRequest']

export const getSystemEmailSettings = async () =>
  (await apiClient.get<SystemEmailSettings>('/admin/system-email')).data

export const updateSystemEmailSettings = async (request: UpdateSystemEmailSettingsRequest) =>
  (await apiClient.put<SystemEmailSettings>('/admin/system-email', request)).data

export const testSystemEmailSettings = async (request: TestSystemEmailSettingsRequest) => {
  await apiClient.post('/admin/system-email/test', request)
}

export const resetSystemEmailSettings = async (expectedVersion: number) =>
  (
    await apiClient.post<SystemEmailSettings>('/admin/system-email/reset', {
      expectedVersion,
    })
  ).data
