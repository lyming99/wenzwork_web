import axios from 'axios'

import { apiClient } from './client'
import type { components } from './schema'

export type PricingPlan = components['schemas']['PricingPlan']
export type Release = components['schemas']['Release']
export type ReleaseAsset = components['schemas']['ReleaseAsset']
export type ReleaseProject = 'web' | 'desktop' | 'mobile'

interface PricingPlanList {
  items: PricingPlan[]
}

interface ReleaseList {
  items: Release[]
}

const freshReleaseHeaders = {
  'Cache-Control': 'no-cache',
  Pragma: 'no-cache',
}

export const listPricingPlans = async () => {
  const response = await apiClient.get<PricingPlanList>('/pricing-plans')
  return response.data.items
}

export const getLatestRelease = async (project: ReleaseProject = 'desktop') => {
  const response = await apiClient.get<Release>('/releases/latest', {
    params: { project, channel: 'stable' },
    headers: freshReleaseHeaders,
  })
  return response.data
}

export const listReleases = async (project: ReleaseProject = 'desktop', limit = 10) => {
  const response = await apiClient.get<ReleaseList>('/releases', {
    params: { project, channel: 'stable', limit },
    headers: freshReleaseHeaders,
  })
  return response.data.items
}

export const isReleaseNotFound = (error: unknown) =>
  axios.isAxiosError(error) &&
  error.response?.status === 404 &&
  error.response.data?.code === 'release_not_found'
