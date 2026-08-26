import { beforeEach, describe, expect, it, vi } from 'vitest'

import { apiClient } from './client'
import {
  getAdminBetaPromotion,
  listAdminBetaPromotionClaims,
  removeAdminBetaPromotionGroupQRCode,
  updateAdminBetaPromotion,
  uploadAdminBetaPromotionGroupQRCode,
} from './promotions'

vi.mock('./client', () => ({
  apiClient: { delete: vi.fn(), get: vi.fn(), put: vi.fn(), post: vi.fn() },
}))

describe('beta promotion admin client', () => {
  beforeEach(() => {
    vi.mocked(apiClient.get).mockReset()
    vi.mocked(apiClient.put).mockReset()
    vi.mocked(apiClient.delete).mockReset()
  })

  it('reads the overview and filtered claim list', async () => {
    vi.mocked(apiClient.get)
      .mockResolvedValueOnce({ data: { remaining: 80 } })
      .mockResolvedValueOnce({ data: { items: [], total: 0, limit: 50, offset: 0 } })

    await expect(getAdminBetaPromotion()).resolves.toEqual({ remaining: 80 })
    await listAdminBetaPromotionClaims({
      q: 'member@example.test',
      deliveryStatus: 'sent',
      redemptionStatus: 'active',
      limit: 50,
      offset: 0,
    })

    expect(apiClient.get).toHaveBeenLastCalledWith('/admin/beta-promotion/claims', {
      params: {
        q: 'member@example.test',
        deliveryStatus: 'sent',
        redemptionStatus: 'active',
        limit: 50,
        offset: 0,
      },
    })
  })

  it('updates only the configured remaining quota', async () => {
    vi.mocked(apiClient.put).mockResolvedValue({ data: { remaining: 0, available: false } })

    await expect(updateAdminBetaPromotion(0)).resolves.toEqual({ remaining: 0, available: false })
    expect(apiClient.put).toHaveBeenCalledWith('/admin/beta-promotion', { remaining: 0 })
  })

  it('uploads and removes the configured group QR code', async () => {
    const file = new File(['qr-image'], 'group-qr.png', { type: 'image/png' })
    vi.mocked(apiClient.put).mockResolvedValue({
      data: { groupQRCodeConfigured: true },
    })
    vi.mocked(apiClient.delete).mockResolvedValue({
      data: { groupQRCodeConfigured: false },
    })

    await expect(uploadAdminBetaPromotionGroupQRCode(file)).resolves.toEqual({
      groupQRCodeConfigured: true,
    })
    expect(apiClient.put).toHaveBeenCalledWith('/admin/beta-promotion/group-qr', file, {
      headers: { 'Content-Type': 'image/png' },
    })

    await expect(removeAdminBetaPromotionGroupQRCode()).resolves.toEqual({
      groupQRCodeConfigured: false,
    })
    expect(apiClient.delete).toHaveBeenCalledWith('/admin/beta-promotion/group-qr')
  })
})
