import { beforeEach, describe, expect, it, vi } from 'vitest'

import { apiClient } from './client'
import {
  createRedemptionCodeBatch,
  getMembership,
  listLifetimeCodeDeliveries,
  listRedemptionCodeBatches,
  redeemMembershipCode,
  revokeRedemptionCodeBatch,
  sendLifetimeCode,
} from './membership'

vi.mock('./client', () => ({
  apiClient: { get: vi.fn(), post: vi.fn(), delete: vi.fn() },
}))

describe('membership API client', () => {
  beforeEach(() => vi.clearAllMocks())

  it('loads membership and submits a code only to the API', async () => {
    vi.mocked(apiClient.get).mockResolvedValueOnce({ data: { planCode: 'free' } })
    await expect(getMembership()).resolves.toEqual({ planCode: 'free' })

    vi.mocked(apiClient.post).mockResolvedValueOnce({ data: { codeHint: 'JKMN' } })
    await expect(redeemMembershipCode('WZM-2345-6789-ABCD-EFGH-JKMN')).resolves.toEqual({
      codeHint: 'JKMN',
    })
    expect(apiClient.post).toHaveBeenCalledWith('/me/redemptions', {
      code: 'WZM-2345-6789-ABCD-EFGH-JKMN',
    })
  })

  it('supports administrator batch creation, listing, and revocation', async () => {
    vi.mocked(apiClient.get).mockResolvedValueOnce({ data: { items: [] } })
    await expect(listRedemptionCodeBatches()).resolves.toEqual([])

    const request = {
      name: 'Launch',
      planCode: 'pro',
      grantType: 'duration' as const,
      grantDays: 30,
      quantity: 10,
    }
    vi.mocked(apiClient.post).mockResolvedValueOnce({
      data: { batch: { id: 'batch-1' }, codes: [] },
    })
    await createRedemptionCodeBatch(request)
    expect(apiClient.post).toHaveBeenCalledWith('/admin/redemption-code-batches', request)

    vi.mocked(apiClient.delete).mockResolvedValueOnce({ data: null })
    await revokeRedemptionCodeBatch('batch/unsafe')
    expect(apiClient.delete).toHaveBeenCalledWith('/admin/redemption-code-batches/batch%2Funsafe')
  })

  it('lists and sends permanent activation codes without handling plaintext', async () => {
    vi.mocked(apiClient.get).mockResolvedValueOnce({
      data: { items: [{ id: 'delivery-1', email: 'buyer@example.com', codeHint: 'ABCD' }] },
    })
    await expect(listLifetimeCodeDeliveries(20)).resolves.toEqual([
      { id: 'delivery-1', email: 'buyer@example.com', codeHint: 'ABCD' },
    ])
    expect(apiClient.get).toHaveBeenCalledWith('/admin/lifetime-code-deliveries', {
      params: { limit: 20 },
    })

    vi.mocked(apiClient.post).mockResolvedValueOnce({
      data: {
        delivery: { id: 'delivery-1', email: 'buyer@example.com', codeHint: 'ABCD' },
      },
    })
    await expect(sendLifetimeCode('buyer@example.com', 'delivery-1')).resolves.toEqual({
      id: 'delivery-1',
      email: 'buyer@example.com',
      codeHint: 'ABCD',
    })
    expect(apiClient.post).toHaveBeenCalledWith('/admin/lifetime-code-deliveries', {
      email: 'buyer@example.com',
      requestId: 'delivery-1',
    })
  })
})
