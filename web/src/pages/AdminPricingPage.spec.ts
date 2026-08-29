import { flushPromises, mount } from '@vue/test-utils'
import { createHead } from '@unhead/vue/client'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import {
  archiveAdminPricingPlan,
  createAdminPricingPlan,
  listAdminPricingPlans,
  publishAdminPricingPlan,
  updateAdminPricingPlan,
  type AdminPricingPlan,
} from '@/api/admin'

import AdminPricingPage from './AdminPricingPage.vue'

vi.mock('@/api/admin', () => ({
  archiveAdminPricingPlan: vi.fn(),
  createAdminPricingPlan: vi.fn(),
  listAdminPricingPlans: vi.fn(),
  publishAdminPricingPlan: vi.fn(),
  updateAdminPricingPlan: vi.fn(),
}))

const listMock = vi.mocked(listAdminPricingPlans)
const updateMock = vi.mocked(updateAdminPricingPlan)

const publishedPlan: AdminPricingPlan = {
  id: 'a2ef88c1-f50d-4ff2-b4da-204dab3084b4',
  code: 'pro',
  name: 'Pro',
  description: 'For creators',
  priceMinor: 12800,
  originalPriceMinor: 19800,
  currency: 'CNY',
  billingPeriod: 'year',
  features: ['Fast'],
  remoteAccessEnabled: true,
  deviceLimit: 10,
  monthlyTrafficLimitGb: 10,
  status: 'published',
  sortOrder: 20,
  version: 4,
  publishedVersion: 3,
  hasUnpublishedChanges: true,
  publishedAt: '2026-07-21T08:00:00Z',
  createdAt: '2026-07-21T07:00:00Z',
  updatedAt: '2026-07-21T09:00:00Z',
}

describe('AdminPricingPage', () => {
  beforeEach(() => {
    vi.mocked(archiveAdminPricingPlan).mockReset()
    vi.mocked(createAdminPricingPlan).mockReset()
    listMock.mockReset()
    vi.mocked(publishAdminPricingPlan).mockReset()
    updateMock.mockReset()
    listMock.mockResolvedValue([publishedPlan])
    updateMock.mockResolvedValue({ ...publishedPlan, priceMinor: 16800, version: 5 })
    vi.spyOn(window, 'scrollTo').mockImplementation(() => undefined)
    Object.defineProperty(window, 'confirm', {
      configurable: true,
      value: vi.fn(() => true),
      writable: true,
    })
  })

  it('shows version state and explicitly confirms integer-minor-unit price changes', async () => {
    const wrapper = mount(AdminPricingPage, { global: { plugins: [createHead()] } })
    await flushPromises()

    expect(wrapper.text()).toContain('有待发布更改')
    expect(wrapper.text()).toContain('当前 v4')
    expect(wrapper.text()).toContain('原价 ¥198')
    const editButton = wrapper.findAll('button').find((button) => button.text() === '编辑')
    expect(editButton).toBeDefined()
    await editButton!.trigger('click')
    await wrapper.get('#pricing-price').setValue('16800')
    await wrapper.get('#pricing-original-price').setValue('21800')
    await wrapper.get('#pricing-device-limit').setValue('24')
    await wrapper.get('#pricing-traffic-limit').setValue('100')
    await wrapper.get('form').trigger('submit')
    await flushPromises()

    expect(window.confirm).toHaveBeenCalledWith(expect.stringContaining('确认修改“Pro”的价格条款'))
    expect(updateMock).toHaveBeenCalledWith(
      publishedPlan.id,
      expect.objectContaining({
        priceMinor: 16800,
        originalPriceMinor: 21800,
        remoteAccessEnabled: true,
        deviceLimit: 24,
        monthlyTrafficLimitGb: 100,
        expectedVersion: 4,
        confirmPriceChange: true,
      }),
    )
  })

  it('configures temporary Free access and its device and traffic limits', async () => {
    const freePlan: AdminPricingPlan = {
      ...publishedPlan,
      id: 'b53fddaf-2644-4d0f-b6ea-39d8fba312d6',
      code: 'free',
      name: 'Free',
      priceMinor: 0,
      originalPriceMinor: null,
      billingPeriod: 'free',
      remoteAccessEnabled: false,
      deviceLimit: 2,
      monthlyTrafficLimitGb: null,
      version: 2,
      publishedVersion: 2,
      hasUnpublishedChanges: false,
    }
    listMock.mockResolvedValue([freePlan])
    updateMock.mockResolvedValue({ ...freePlan, remoteAccessEnabled: true, version: 3 })
    const wrapper = mount(AdminPricingPage, { global: { plugins: [createHead()] } })
    await flushPromises()

    expect(wrapper.text()).toContain('设备接入未开放')
    await wrapper.get('button.button-secondary').trigger('click')
    expect(wrapper.text()).toContain('临时开放普通会员使用远程设备')
    await wrapper.get('#pricing-remote-access').setValue(true)
    await wrapper.get('#pricing-device-limit').setValue('3')
    await wrapper.get('#pricing-traffic-limit').setValue('20')
    await wrapper.get('form').trigger('submit')
    await flushPromises()

    expect(updateMock).toHaveBeenCalledWith(
      freePlan.id,
      expect.objectContaining({
        remoteAccessEnabled: true,
        deviceLimit: 3,
        monthlyTrafficLimitGb: 20,
        expectedVersion: 2,
      }),
    )
  })
})
