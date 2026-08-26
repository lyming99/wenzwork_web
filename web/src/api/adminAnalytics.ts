import { apiClient } from './client'
import type { components } from './schema'

export type AdminAnalyticsOverview = components['schemas']['AdminAnalyticsOverview']
export type AdminLoginEvent = components['schemas']['AdminLoginEvent']
export type AdminLoginEventList = components['schemas']['AdminLoginEventList']

export type AnalyticsRangeParams = {
  from: string
  to: string
}

export type AnalyticsGranularity = 'hour' | 'day'
export type AnalyticsOverviewParams = AnalyticsRangeParams & {
  granularity: AnalyticsGranularity
}

export const getAdminAnalyticsOverview = async (params: AnalyticsOverviewParams) =>
  (await apiClient.get<AdminAnalyticsOverview>('/admin/analytics/overview', { params })).data

export const listAdminLoginEvents = async (
  params: AnalyticsRangeParams & { q?: string; limit?: number; offset?: number },
) => (await apiClient.get<AdminLoginEventList>('/admin/analytics/login-events', { params })).data
