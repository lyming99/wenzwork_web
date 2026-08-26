import { apiClient } from './client'
import type { components } from './schema'

export type Membership = components['schemas']['Membership']
export type RedemptionRecord = components['schemas']['RedemptionRecord']
export type RedemptionCodeBatch = components['schemas']['RedemptionCodeBatch']
export type CreateRedemptionCodeBatchRequest =
  components['schemas']['CreateRedemptionCodeBatchRequest']
export type CreateRedemptionCodeBatchResponse =
  components['schemas']['CreateRedemptionCodeBatchResponse']
export type RedemptionCodeSummary = components['schemas']['RedemptionCodeSummary']
export type RedemptionCodeList = components['schemas']['RedemptionCodeList']
export type LifetimeCodeDelivery = components['schemas']['LifetimeCodeDelivery']
export type LifetimeCodeDeliveryList = components['schemas']['LifetimeCodeDeliveryList']
export type SendLifetimeCodeResponse = components['schemas']['SendLifetimeCodeResponse']

export const getMembership = async () => (await apiClient.get<Membership>('/me/membership')).data

export const listRedemptions = async () =>
  (await apiClient.get<{ items: RedemptionRecord[] }>('/me/redemptions')).data.items

export const redeemMembershipCode = async (code: string) =>
  (await apiClient.post<components['schemas']['RedeemCodeResponse']>('/me/redemptions', { code }))
    .data

export const listRedemptionCodeBatches = async () =>
  (await apiClient.get<{ items: RedemptionCodeBatch[] }>('/admin/redemption-code-batches')).data
    .items

export const createRedemptionCodeBatch = async (request: CreateRedemptionCodeBatchRequest) =>
  (
    await apiClient.post<CreateRedemptionCodeBatchResponse>(
      '/admin/redemption-code-batches',
      request,
    )
  ).data

export const revokeRedemptionCodeBatch = async (batchId: string) => {
  await apiClient.delete(`/admin/redemption-code-batches/${encodeURIComponent(batchId)}`)
}

export const listRedemptionCodes = async (params: {
  batchId?: string
  status?: '' | 'active' | 'redeemed' | 'revoked'
  limit?: number
  offset?: number
}) => (await apiClient.get<RedemptionCodeList>('/admin/redemption-codes', { params })).data

export const revokeRedemptionCode = async (codeId: string) => {
  await apiClient.delete(`/admin/redemption-codes/${encodeURIComponent(codeId)}`)
}

export const listLifetimeCodeDeliveries = async (limit = 20) =>
  (
    await apiClient.get<LifetimeCodeDeliveryList>('/admin/lifetime-code-deliveries', {
      params: { limit },
    })
  ).data.items

export const sendLifetimeCode = async (email: string, requestId: string) =>
  (
    await apiClient.post<SendLifetimeCodeResponse>('/admin/lifetime-code-deliveries', {
      email,
      requestId,
    })
  ).data.delivery
