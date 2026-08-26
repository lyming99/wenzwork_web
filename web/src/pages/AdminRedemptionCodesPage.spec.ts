import { flushPromises, mount } from '@vue/test-utils'
import { createHead } from '@unhead/vue/client'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import {
  createRedemptionCodeBatch,
  listLifetimeCodeDeliveries,
  listRedemptionCodeBatches,
  listRedemptionCodes,
  revokeRedemptionCode,
  revokeRedemptionCodeBatch,
  sendLifetimeCode,
  type LifetimeCodeDelivery,
} from '@/api/membership'

import AdminRedemptionCodesPage from './AdminRedemptionCodesPage.vue'

vi.mock('@/api/membership', () => ({
  createRedemptionCodeBatch: vi.fn(),
  listLifetimeCodeDeliveries: vi.fn(),
  listRedemptionCodeBatches: vi.fn(),
  listRedemptionCodes: vi.fn(),
  revokeRedemptionCode: vi.fn(),
  revokeRedemptionCodeBatch: vi.fn(),
  sendLifetimeCode: vi.fn(),
}))

const failedDelivery: LifetimeCodeDelivery = {
  id: '773f7f87-cdf2-405d-a79e-8274eeaf65ba',
  email: 'failed@example.test',
  codeHint: 'ABCD',
  deliveryStatus: 'failed',
  redemptionStatus: 'active',
  deliveryAttempts: 1,
  lastDeliveryAttemptAt: '2026-07-24T01:00:00Z',
  sentAt: null,
  createdAt: '2026-07-24T01:00:00Z',
  updatedAt: '2026-07-24T01:00:01Z',
}

const sentDelivery = (email: string, id = 'da587bb5-4cf2-4462-8d24-a4ccb61bb4c3') => ({
  ...failedDelivery,
  id,
  email,
  codeHint: 'WXYZ',
  deliveryStatus: 'sent' as const,
  deliveryAttempts: 1,
  sentAt: '2026-07-24T01:00:02Z',
  updatedAt: '2026-07-24T01:00:02Z',
})

describe('AdminRedemptionCodesPage', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    vi.mocked(listRedemptionCodeBatches).mockResolvedValue([])
    vi.mocked(listRedemptionCodes).mockResolvedValue({
      items: [],
      total: 0,
      limit: 200,
      offset: 0,
    })
    vi.mocked(listLifetimeCodeDeliveries).mockResolvedValue([failedDelivery])
    vi.mocked(createRedemptionCodeBatch).mockResolvedValue({
      batch: {} as never,
      codes: [],
    })
    vi.mocked(revokeRedemptionCode).mockResolvedValue()
    vi.mocked(revokeRedemptionCodeBatch).mockResolvedValue()
    Object.defineProperty(window, 'confirm', {
      configurable: true,
      value: vi.fn(() => true),
      writable: true,
    })
  })

  it('generates and emails a permanent Pro code without showing plaintext', async () => {
    vi.mocked(sendLifetimeCode).mockResolvedValue(sentDelivery('buyer@example.test'))
    const wrapper = mount(AdminRedemptionCodesPage, {
      global: { plugins: [createHead()] },
    })
    await flushPromises()

    expect(wrapper.text()).toContain('一键发送永久激活码')
    expect(wrapper.text()).toContain('不受内测码“一邮箱一次”的规则影响')
    expect(wrapper.text()).toContain('failed@example.test')

    await wrapper.get('#lifetime-code-email').setValue('buyer@example.test')
    await wrapper.get('.lifetime-code-delivery-card').trigger('submit')
    await flushPromises()

    expect(window.confirm).toHaveBeenCalledWith(
      expect.stringContaining('发送到 buyer@example.test'),
    )
    expect(sendLifetimeCode).toHaveBeenCalledWith(
      'buyer@example.test',
      expect.stringMatching(/^[0-9a-f-]{36}$/),
    )
    expect(wrapper.text()).toContain('永久 Pro 激活码（尾号 WXYZ）已发送至 buyer@example.test')
    expect(wrapper.text()).not.toContain('WZM-')
  })

  it('retries a failed email using the same delivery id and code', async () => {
    vi.mocked(sendLifetimeCode).mockResolvedValue(
      sentDelivery(failedDelivery.email, failedDelivery.id),
    )
    const wrapper = mount(AdminRedemptionCodesPage, {
      global: { plugins: [createHead()] },
    })
    await flushPromises()

    const retryButton = wrapper
      .findAll('button')
      .find((button) => button.text().includes('重试发送'))
    expect(retryButton).toBeDefined()
    await retryButton!.trigger('click')
    await flushPromises()

    expect(window.confirm).toHaveBeenCalledWith(
      expect.stringContaining('尾号 ABCD 的同一个永久激活码'),
    )
    expect(sendLifetimeCode).toHaveBeenCalledWith(failedDelivery.email, failedDelivery.id)
  })
})
