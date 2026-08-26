import { apiClient } from './client'
import type { components } from './schema'

export type SystemSetupSettings = components['schemas']['SystemSetupSettings']
export type ApplySystemSetupRequest = components['schemas']['ApplySystemSetupRequest']
export type SystemSetupApplyResponse = components['schemas']['SystemSetupApplyResponse']

export const getSystemSetup = async () =>
  (await apiClient.get<SystemSetupSettings>('/admin/system-setup')).data

export const applySystemSetup = async (request: ApplySystemSetupRequest) =>
  (await apiClient.put<SystemSetupApplyResponse>('/admin/system-setup', request)).data
