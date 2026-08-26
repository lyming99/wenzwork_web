import { flushPromises, mount } from '@vue/test-utils'
import { createHead } from '@unhead/vue/client'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import {
  getAdminBetaPromotion,
  getAdminTrialPromotion,
  listAdminBetaPromotionClaims,
  listAdminTrialPromotionClaims,
  removeAdminBetaPromotionGroupQRCode,
  updateAdminBetaPromotion,
  updateAdminTrialPromotion,
  uploadAdminBetaPromotionGroupQRCode,
  type BetaPromotionAdminOverview,
  type TrialPromotionAdminOverview,
} from '@/api/promotions'

import AdminBetaCodesPage from './AdminBetaCodesPage.vue'

vi.mock('@/api/promotions', () => ({
  getAdminBetaPromotion: vi.fn(),
  getAdminTrialPromotion: vi.fn(),
  listAdminBetaPromotionClaims: vi.fn(),
  listAdminTrialPromotionClaims: vi.fn(),
  removeAdminBetaPromotionGroupQRCode: vi.fn(),
  updateAdminBetaPromotion: vi.fn(),
  updateAdminTrialPromotion: vi.fn(),
  uploadAdminBetaPromotionGroupQRCode: vi.fn(),
}))

const overview: BetaPromotionAdminOverview = {
  code: 'beta-pro-launch',
  status: 'active',
  limit: 100,
  claimed: 20,
  remaining: 80,
  available: true,
  pendingDeliveryCount: 1,
  sentDeliveryCount: 18,
  failedDeliveryCount: 1,
  activeCodeCount: 15,
  redeemedCodeCount: 5,
  revokedCodeCount: 0,
  groupQRCodeConfigured: false,
  groupQRCodeUrl: null,
  groupQRCodeUpdatedAt: null,
  updatedAt: '2026-07-23T08:00:00Z',
}

const trialOverview: TrialPromotionAdminOverview = {
  enabled: true,
  dailyQuota: 100,
  today: '2026-07-25',
  todayLimit: 100,
  claimedToday: 6,
  remainingToday: 94,
  available: true,
  grantDays: 30,
  refreshesAt: '2026-07-25T16:00:00Z',
  totalClaimCount: 26,
  pendingDeliveryCount: 1,
  sentDeliveryCount: 24,
  failedDeliveryCount: 1,
  activeCodeCount: 20,
  redeemedCodeCount: 6,
  revokedCodeCount: 0,
  updatedAt: '2026-07-25T08:00:00Z',
}

describe('AdminBetaCodesPage', () => {
  beforeEach(() => {
    vi.mocked(getAdminBetaPromotion).mockReset()
    vi.mocked(getAdminTrialPromotion).mockReset()
    vi.mocked(listAdminBetaPromotionClaims).mockReset()
    vi.mocked(listAdminTrialPromotionClaims).mockReset()
    vi.mocked(removeAdminBetaPromotionGroupQRCode).mockReset()
    vi.mocked(updateAdminBetaPromotion).mockReset()
    vi.mocked(updateAdminTrialPromotion).mockReset()
    vi.mocked(uploadAdminBetaPromotionGroupQRCode).mockReset()
    vi.mocked(getAdminBetaPromotion).mockResolvedValue(overview)
    vi.mocked(getAdminTrialPromotion).mockResolvedValue(trialOverview)
    vi.mocked(listAdminBetaPromotionClaims).mockResolvedValue({
      items: [
        {
          id: '773f7f87-cdf2-405d-a79e-8274eeaf65ba',
          email: 'member@example.test',
          codeHint: 'ABCD',
          deliveryStatus: 'sent',
          redemptionStatus: 'active',
          deliveryAttempts: 1,
          lastDeliveryAttemptAt: '2026-07-23T08:00:00Z',
          sentAt: '2026-07-23T08:00:01Z',
          createdAt: '2026-07-23T08:00:00Z',
          redeemedAt: null,
        },
      ],
      total: 1,
      limit: 50,
      offset: 0,
    })
    vi.mocked(updateAdminBetaPromotion).mockResolvedValue({
      ...overview,
      status: 'disabled',
      limit: 20,
      remaining: 0,
      available: false,
    })
    vi.mocked(listAdminTrialPromotionClaims).mockResolvedValue({
      items: [
        {
          id: '74de3034-2cd8-4ceb-a0f1-dd61424bc17d',
          email: 'trial@example.test',
          claimDate: '2026-07-25',
          codeHint: 'EFGH',
          deliveryStatus: 'sent',
          redemptionStatus: 'active',
          deliveryAttempts: 1,
          lastDeliveryAttemptAt: '2026-07-25T08:00:00Z',
          sentAt: '2026-07-25T08:00:01Z',
          createdAt: '2026-07-25T08:00:00Z',
          redeemedAt: null,
        },
      ],
      total: 1,
      limit: 50,
      offset: 0,
    })
    vi.mocked(updateAdminTrialPromotion).mockResolvedValue({
      ...trialOverview,
      enabled: false,
      dailyQuota: 120,
      available: false,
    })
    vi.mocked(uploadAdminBetaPromotionGroupQRCode).mockResolvedValue({
      ...overview,
      groupQRCodeConfigured: true,
      groupQRCodeUrl: '/api/v1/promotions/beta-pro/group-qr?v=123',
      groupQRCodeUpdatedAt: '2026-07-24T08:00:00Z',
    })
    vi.mocked(removeAdminBetaPromotionGroupQRCode).mockResolvedValue(overview)
    Object.defineProperty(window, 'confirm', {
      configurable: true,
      value: vi.fn(() => true),
      writable: true,
    })
  })

  it('shows quota and claim states without exposing a plaintext code', async () => {
    const wrapper = mount(AdminBetaCodesPage, { global: { plugins: [createHead()] } })
    await flushPromises()

    expect(wrapper.text()).toContain('剩余名额')
    expect(wrapper.text()).toContain('80')
    expect(wrapper.text()).toContain('member@example.test')
    expect(wrapper.text()).toContain('•••• ABCD')
    expect(wrapper.text()).not.toContain('WZM-')
    expect(wrapper.text()).toContain('不影响普通兑换码')
    expect(wrapper.text()).toContain('30 天 Pro 试用活动')
    expect(wrapper.text()).toContain('trial@example.test')
    expect(wrapper.text()).toContain('今日剩余')
    expect(wrapper.text()).toContain('94')
  })

  it('confirms zero quota and disables only new beta claims', async () => {
    const wrapper = mount(AdminBetaCodesPage, { global: { plugins: [createHead()] } })
    await flushPromises()

    await wrapper.get('#beta-promotion-remaining').setValue('0')
    await wrapper.get('.beta-quota-settings').trigger('submit')
    await flushPromises()

    expect(window.confirm).toHaveBeenCalledWith(expect.stringContaining('官网将立即隐藏'))
    expect(updateAdminBetaPromotion).toHaveBeenCalledWith(0)
    expect(wrapper.text()).toContain('官网内测码领取入口已停用')
    expect(wrapper.text()).toContain('不会使已发出的内测码失效')
  })

  it('updates the trial daily refresh quantity and enabled state', async () => {
    const wrapper = mount(AdminBetaCodesPage, { global: { plugins: [createHead()] } })
    await flushPromises()

    await wrapper.get<HTMLInputElement>('.trial-enabled-control input').setValue(false)
    await wrapper.get('#trial-promotion-daily-quota').setValue('120')
    await wrapper.get('.trial-promotion-settings').trigger('submit')
    await flushPromises()

    expect(updateAdminTrialPromotion).toHaveBeenCalledWith(false, 120)
    expect(wrapper.text()).toContain('试用码活动已关闭')
    expect(wrapper.text()).toContain('每日刷新数量已保存为 120 份')
  })

  it('passes email and status filters to the admin API', async () => {
    const wrapper = mount(AdminBetaCodesPage, { global: { plugins: [createHead()] } })
    await flushPromises()

    await wrapper.get('input[type="search"]').setValue('member@example.test')
    const selects = wrapper.findAll('.beta-claim-filter select')
    await selects[0]!.setValue('sent')
    await selects[1]!.setValue('active')
    await wrapper.get('.beta-claim-filter').trigger('submit')
    await flushPromises()

    expect(listAdminBetaPromotionClaims).toHaveBeenLastCalledWith({
      q: 'member@example.test',
      deliveryStatus: 'sent',
      redemptionStatus: 'active',
      limit: 50,
      offset: 0,
    })
  })

  it('uploads, previews and removes the beta group QR code', async () => {
    const wrapper = mount(AdminBetaCodesPage, { global: { plugins: [createHead()] } })
    await flushPromises()

    expect(wrapper.text()).toContain('内测微信交流群二维码')
    expect(wrapper.text()).toContain('尚未上传二维码')
    expect(wrapper.find('.beta-group-qr-preview img').exists()).toBe(false)

    const file = new File(['qr-image'], 'beta-group.png', { type: 'image/png' })
    const fileInput = wrapper.get<HTMLInputElement>('#beta-group-qr-file')
    Object.defineProperty(fileInput.element, 'files', {
      configurable: true,
      value: [file],
    })
    await fileInput.trigger('change')
    await wrapper.get('.beta-group-qr-form').trigger('submit')
    await flushPromises()

    expect(uploadAdminBetaPromotionGroupQRCode).toHaveBeenCalledWith(file)
    expect(wrapper.get('.beta-group-qr-preview img').attributes('src')).toContain(
      '/api/v1/promotions/beta-pro/group-qr?v=123',
    )
    expect(wrapper.text()).toContain('后续用户领取成功后会立即看到')

    await wrapper.get('.beta-group-qr-actions .button-secondary').trigger('click')
    await flushPromises()

    expect(window.confirm).toHaveBeenCalledWith(expect.stringContaining('确认移除'))
    expect(removeAdminBetaPromotionGroupQRCode).toHaveBeenCalledOnce()
    expect(wrapper.text()).toContain('内测微信交流群二维码已移除')
    expect(wrapper.find('.beta-group-qr-preview img').exists()).toBe(false)
  })

  it('rejects unsupported or oversized group QR files before upload', async () => {
    const wrapper = mount(AdminBetaCodesPage, { global: { plugins: [createHead()] } })
    await flushPromises()

    const fileInput = wrapper.get<HTMLInputElement>('#beta-group-qr-file')
    const unsupported = new File(['not-an-image'], 'group.gif', { type: 'image/gif' })
    Object.defineProperty(fileInput.element, 'files', {
      configurable: true,
      value: [unsupported],
    })
    await fileInput.trigger('change')

    expect(wrapper.text()).toContain('请选择 PNG 或 JPEG')
    expect(uploadAdminBetaPromotionGroupQRCode).not.toHaveBeenCalled()
    expect(wrapper.get<HTMLButtonElement>('.beta-group-qr-form .button').element.disabled).toBe(
      true,
    )
  })
})
