import axios from 'axios'

import { apiClient } from './client'
import type { components } from './schema'

export type BetaPromotionStatus = components['schemas']['BetaPromotionStatus']
export type BetaPromotionClaimResponse = components['schemas']['BetaPromotionClaimResponse']
export type BetaPromotionAdminOverview = components['schemas']['BetaPromotionAdminOverview']
export type BetaPromotionAdminClaim = components['schemas']['BetaPromotionAdminClaim']
export type BetaPromotionAdminClaimList = components['schemas']['BetaPromotionAdminClaimList']
export type TrialPromotionStatus = components['schemas']['TrialPromotionStatus']
export type TrialPromotionClaimResponse = components['schemas']['TrialPromotionClaimResponse']
export type TrialPromotionAdminOverview = components['schemas']['TrialPromotionAdminOverview']
export type TrialPromotionAdminClaim = components['schemas']['TrialPromotionAdminClaim']
export type TrialPromotionAdminClaimList = components['schemas']['TrialPromotionAdminClaimList']

export const betaPromotionGroupQRCodeURL = '/api/v1/promotions/beta-pro/group-qr'

export const getBetaPromotionStatus = async () => {
  const response = await apiClient.get<BetaPromotionStatus>('/promotions/beta-pro')
  return response.data
}

export const claimBetaPromotion = async (email: string) => {
  const response = await apiClient.post<BetaPromotionClaimResponse>('/promotions/beta-pro/claims', {
    email,
  })
  return response.data
}

export const getAdminBetaPromotion = async () =>
  (await apiClient.get<BetaPromotionAdminOverview>('/admin/beta-promotion')).data

export const updateAdminBetaPromotion = async (remaining: number) =>
  (
    await apiClient.put<BetaPromotionAdminOverview>('/admin/beta-promotion', {
      remaining,
    })
  ).data

export const uploadAdminBetaPromotionGroupQRCode = async (file: Blob) =>
  (
    await apiClient.put<BetaPromotionAdminOverview>('/admin/beta-promotion/group-qr', file, {
      headers: { 'Content-Type': file.type },
    })
  ).data

export const removeAdminBetaPromotionGroupQRCode = async () =>
  (await apiClient.delete<BetaPromotionAdminOverview>('/admin/beta-promotion/group-qr')).data

export const listAdminBetaPromotionClaims = async (params: {
  q?: string
  deliveryStatus?: '' | 'pending' | 'sent' | 'failed'
  redemptionStatus?: '' | 'active' | 'redeemed' | 'revoked'
  limit?: number
  offset?: number
}) =>
  (
    await apiClient.get<BetaPromotionAdminClaimList>('/admin/beta-promotion/claims', {
      params,
    })
  ).data

export const getTrialPromotionStatus = async () =>
  (await apiClient.get<TrialPromotionStatus>('/promotions/trial-pro')).data

export const claimTrialPromotion = async (email: string) =>
  (
    await apiClient.post<TrialPromotionClaimResponse>('/promotions/trial-pro/claims', {
      email,
    })
  ).data

export const getAdminTrialPromotion = async () =>
  (await apiClient.get<TrialPromotionAdminOverview>('/admin/trial-promotion')).data

export const updateAdminTrialPromotion = async (enabled: boolean, dailyQuota: number) =>
  (
    await apiClient.put<TrialPromotionAdminOverview>('/admin/trial-promotion', {
      enabled,
      dailyQuota,
    })
  ).data

export const listAdminTrialPromotionClaims = async (params: {
  q?: string
  deliveryStatus?: '' | 'pending' | 'sent' | 'failed'
  redemptionStatus?: '' | 'active' | 'redeemed' | 'revoked'
  limit?: number
  offset?: number
}) =>
  (
    await apiClient.get<TrialPromotionAdminClaimList>('/admin/trial-promotion/claims', {
      params,
    })
  ).data

export const betaPromotionErrorMessage = (error: unknown) => {
  if (!axios.isAxiosError(error)) return '暂时无法加入内测，请稍后重试。'

  switch (error.response?.data?.code) {
    case 'promotion_email_invalid':
      return '邮箱格式不正确，请输入可以正常收信的邮箱地址。'
    case 'promotion_exhausted':
      return '内测赠送名额已经领完，感谢关注。'
    case 'promotion_rate_limited':
      return '今日领取次数过多，请勿重复提交，或在 24 小时后重试。'
    case 'promotion_email_delivery_failed':
      return '名额已经保留，但邮件暂时未能发出。请再次提交相同邮箱重试。'
    default:
      return '暂时无法加入内测，请稍后使用相同邮箱重试。'
  }
}

export const trialPromotionErrorMessage = (error: unknown) => {
  if (!axios.isAxiosError(error)) return '暂时无法领取试用码，请稍后重试。'

  switch (error.response?.data?.code) {
    case 'trial_promotion_email_invalid':
      return '邮箱格式不正确，请输入可以正常收信的邮箱地址。'
    case 'trial_promotion_unavailable':
      return '今日试用码已经领完或活动已关闭，请在下一次刷新后重试。'
    case 'trial_promotion_rate_limited':
      return '今日领取次数过多，请勿重复提交，或在 24 小时后重试。'
    case 'trial_promotion_email_delivery_failed':
      return '名额已经保留，但邮件暂时未能发出。请再次提交相同邮箱重试。'
    default:
      return '暂时无法领取试用码，请稍后使用相同邮箱重试。'
  }
}
