import { flushPromises, mount } from '@vue/test-utils'
import { createHead } from '@unhead/vue/client'
import { createMemoryHistory, createRouter } from 'vue-router'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import { listPricingPlans } from '@/api/catalog'
import {
  claimBetaPromotion,
  claimTrialPromotion,
  getBetaPromotionStatus,
  getTrialPromotionStatus,
} from '@/api/promotions'

import PricingPage from './PricingPage.vue'

vi.mock('@/api/catalog', () => ({
  listPricingPlans: vi.fn(),
}))

vi.mock('@/api/promotions', () => ({
  betaPromotionGroupQRCodeURL: '/api/v1/promotions/beta-pro/group-qr',
  getBetaPromotionStatus: vi.fn(),
  claimBetaPromotion: vi.fn(),
  betaPromotionErrorMessage: vi.fn(() => '暂时无法加入内测，请稍后重试。'),
  getTrialPromotionStatus: vi.fn(),
  claimTrialPromotion: vi.fn(),
  trialPromotionErrorMessage: vi.fn(() => '暂时无法领取试用码，请稍后重试。'),
}))

const pricingMock = vi.mocked(listPricingPlans)
const promotionStatusMock = vi.mocked(getBetaPromotionStatus)
const promotionClaimMock = vi.mocked(claimBetaPromotion)
const trialStatusMock = vi.mocked(getTrialPromotionStatus)
const trialClaimMock = vi.mocked(claimTrialPromotion)

describe('PricingPage', () => {
  beforeEach(() => {
    pricingMock.mockReset()
    promotionStatusMock.mockReset()
    promotionClaimMock.mockReset()
    trialStatusMock.mockReset()
    trialClaimMock.mockReset()
    promotionStatusMock.mockResolvedValue({
      limit: 100,
      claimed: 0,
      remaining: 100,
      available: true,
    })
    trialStatusMock.mockResolvedValue({
      enabled: true,
      available: true,
      dailyLimit: 100,
      claimedToday: 0,
      remainingToday: 100,
      grantDays: 30,
      refreshesAt: '2026-07-26T16:00:00Z',
    })
  })

  it('retains safe build-time plans when the API fails', async () => {
    pricingMock.mockRejectedValue(new Error('offline'))
    const router = createRouter({
      history: createMemoryHistory(),
      routes: [{ path: '/:pathMatch(.*)*', component: { template: '<div />' } }],
    })
    const wrapper = mount(PricingPage, {
      global: { plugins: [router, createHead()] },
    })

    await flushPromises()

    expect(wrapper.text()).toContain('价格服务暂时不可用')
    expect(wrapper.text()).toContain('Free')
    expect(wrapper.text()).toContain('自部署服务免费')
    expect(wrapper.text()).toContain('Pro')
    expect(wrapper.text()).toContain('¥59')
    expect(wrapper.text()).not.toContain('原价 ¥59')
    expect(wrapper.text()).toContain('首年 59 元并赠送 WenzMark 会员，次年起 99 元/年。')
    expect(wrapper.text()).toContain('永久会员 399 元，限量 50 位')
    expect(wrapper.text()).toContain('每月 10 GB 远程流量（内测期间不限流量）')
    expect(wrapper.text()).toContain('最多 10 台设备同时在线')
    expect(wrapper.text()).not.toContain('直播间')
    expect(wrapper.text()).not.toContain('果冻橙橙君')
    expect(wrapper.text()).toContain('立即下载')
    expect(wrapper.text()).toContain('开通 Pro')
    expect(wrapper.text()).toContain('填写邮箱，免费领取 1 年 Pro')
    expect(wrapper.text()).toContain('每个邮箱限领、限用 1 份')
    expect(wrapper.text()).toContain('永久会员不可重复领取权益')
    expect(wrapper.text()).toContain('微信：lyming555')
    expect(wrapper.text()).toContain('QQ：44185539')
    expect(wrapper.findAll('.pricing-card')).toHaveLength(2)
  })

  it('renders published integer-minor-unit pricing from the API', async () => {
    pricingMock.mockResolvedValue([
      {
        code: 'free',
        name: 'Free',
        description: '旧的 Free 说明',
        priceMinor: 0,
        originalPriceMinor: null,
        currency: 'CNY',
        billingPeriod: 'free',
        features: ['Local feature'],
        remoteAccessEnabled: false,
        deviceLimit: 2,
        monthlyTrafficLimitGb: null,
      },
      {
        code: 'pro',
        name: 'Pro',
        description: 'Published plan',
        priceMinor: 12800,
        originalPriceMinor: 16800,
        currency: 'CNY',
        billingPeriod: 'year',
        features: ['Feature'],
        remoteAccessEnabled: true,
        deviceLimit: 10,
        monthlyTrafficLimitGb: 10,
      },
    ])
    const router = createRouter({
      history: createMemoryHistory(),
      routes: [{ path: '/:pathMatch(.*)*', component: { template: '<div />' } }],
    })
    const wrapper = mount(PricingPage, {
      global: { plugins: [router, createHead()] },
    })

    await flushPromises()

    expect(wrapper.text()).not.toContain('已同步服务端最新价格目录')
    expect(wrapper.text()).not.toContain('简单的方案')
    expect(wrapper.find('.content-hero').exists()).toBe(false)
    expect(wrapper.text()).toContain('自部署服务免费')
    expect(wrapper.text()).not.toContain('旧的 Free 说明')
    expect(wrapper.text()).toContain('原价 ¥168')
    expect(wrapper.text()).toContain('¥128')
    expect(wrapper.text()).toContain('首年 · 续费 ¥99/年')
    expect(wrapper.text()).toContain('开通 Pro')
    expect(wrapper.text()).toContain('每月 10 GB 远程流量（内测期间不限流量）')
    expect(wrapper.text()).toContain('最多 10 台设备同时在线')

    await wrapper.get('.pricing-card.featured .button').trigger('click')
    expect(wrapper.get('.purchase-dialog-heading').text()).toContain(
      '当前选择 年费 Pro，价格为 ¥128',
    )
  })

  it('opens the WeChat reward-code purchase flow from the Pro action', async () => {
    pricingMock.mockRejectedValue(new Error('offline'))
    const router = createRouter({
      history: createMemoryHistory(),
      routes: [{ path: '/:pathMatch(.*)*', component: { template: '<div />' } }],
    })
    const wrapper = mount(PricingPage, {
      global: { plugins: [router, createHead()] },
    })

    await flushPromises()
    await wrapper.get('.pricing-card.featured .button').trigger('click')

    const dialog = wrapper.get('dialog')
    expect(dialog.attributes('open')).toBeDefined()
    expect(dialog.text()).toContain('购买 Pro')
    expect(dialog.text()).toContain('当前选择 年费 Pro，价格为 ¥59')
    expect(dialog.text()).toContain('续费 ¥99/年 · 赠 WenzMark 会员')
    expect(dialog.text()).toContain('¥399 一次性')
    expect(dialog.text()).not.toContain('直播间')
    expect(dialog.text()).not.toContain('果冻橙橙君')
    expect(dialog.find('a[href*="bilibili.com"]').exists()).toBe(false)
    expect(dialog.text()).toContain('在留言中填写邮箱')
    expect(dialog.text()).toContain('系统会将对应方案的兑换码发送到留言中的邮箱')
    expect(dialog.text()).toContain('QQ 44185539')
    expect(dialog.text()).toContain('微信 lyming555')
    expect(dialog.get('.wechat-reward-code-image').attributes('alt')).toContain('微信赞赏码')
    expect(dialog.text()).not.toContain('暂时无法通过网站购买')

    await dialog.get('.purchase-dialog-confirm').trigger('click')
    expect(dialog.attributes('open')).toBeUndefined()
  })

  it('places the limited lifetime membership card immediately before the FAQ', async () => {
    pricingMock.mockRejectedValue(new Error('offline'))
    const router = createRouter({
      history: createMemoryHistory(),
      routes: [{ path: '/:pathMatch(.*)*', component: { template: '<div />' } }],
    })
    const wrapper = mount(PricingPage, {
      global: { plugins: [router, createHead()] },
    })

    await flushPromises()

    const sectionOrder = wrapper
      .findAll('.comparison-section, .beta-benefits-section, .faq-section')
      .map((section) => section.classes()[0])
    expect(sectionOrder).toEqual(['comparison-section', 'beta-benefits-section', 'faq-section'])
    expect(wrapper.get('.beta-benefits-section').text()).toContain('399 元，一次解锁永久 Pro')
    expect(wrapper.get('.beta-benefits-section').text()).toContain('仅开放 50 位永久会员')
    expect(wrapper.get('.beta-benefits-section').text()).not.toContain('直播间')

    await wrapper.get('.beta-benefit-contact .button').trigger('click')
    expect(wrapper.get('.purchase-dialog-heading').text()).toContain(
      '当前选择 永久 Pro，价格为 ¥399',
    )
  })

  it('claims the limited beta membership from the email card', async () => {
    pricingMock.mockRejectedValue(new Error('offline'))
    promotionClaimMock.mockResolvedValue({
      message: '领取成功，1 年 Pro 兑换码已发送至邮箱。',
      promotion: { limit: 100, claimed: 1, remaining: 99, available: true },
      deliveryStatus: 'sent',
      alreadyClaimed: false,
      groupQRCodeUrl: '/api/v1/promotions/beta-pro/group-qr?v=123',
    })
    const router = createRouter({
      history: createMemoryHistory(),
      routes: [{ path: '/:pathMatch(.*)*', component: { template: '<div />' } }],
    })
    const wrapper = mount(PricingPage, {
      global: { plugins: [router, createHead()] },
    })

    await flushPromises()
    expect(wrapper.get('.beta-member-group-qr img').attributes('src')).toBe(
      '/api/v1/promotions/beta-pro/group-qr',
    )
    expect(wrapper.get('.beta-member-group-qr').text()).toContain(
      '扫码加入 WenzWork 内测微信交流群',
    )
    expect(wrapper.get('.beta-member-group-qr').text()).toContain(
      '若二维码失效，请联系微信 lyming555 或 QQ 44185539。',
    )
    expect(wrapper.find('.beta-member-contact').exists()).toBe(false)
    await wrapper.get('#beta-member-email').setValue('member@example.com')
    await wrapper.get('.beta-member-form').trigger('submit')
    await flushPromises()

    expect(promotionClaimMock).toHaveBeenCalledWith('member@example.com')
    expect(wrapper.get('.beta-member-message').text()).toContain('兑换码已发送至邮箱')
    expect(wrapper.get('.beta-member-badge').text()).toBe('限量发放 · 手慢无')
    expect(wrapper.get('.beta-member-quota').text()).toBe('目前剩余 99 份')
    expect(wrapper.get<HTMLInputElement>('#beta-member-email').element.value).toBe('')
    expect(wrapper.get('.beta-member-group-qr img').attributes('src')).toBe(
      '/api/v1/promotions/beta-pro/group-qr?v=123',
    )
    expect(wrapper.find('.beta-member-contact').exists()).toBe(false)

    await wrapper.get('.beta-member-group-qr img').trigger('error')
    expect(wrapper.get('.beta-member-group-qr-expired').text()).toBe('二维码失效')
  })

  it('keeps the success result and group QR visible when the final quota is claimed', async () => {
    pricingMock.mockRejectedValue(new Error('offline'))
    promotionStatusMock.mockResolvedValue({
      limit: 1,
      claimed: 0,
      remaining: 1,
      available: true,
    })
    promotionClaimMock.mockResolvedValue({
      message: '领取成功，1 年 Pro 兑换码已发送至邮箱。',
      promotion: { limit: 1, claimed: 1, remaining: 0, available: false },
      deliveryStatus: 'sent',
      alreadyClaimed: false,
      groupQRCodeUrl: '/api/v1/promotions/beta-pro/group-qr?v=456',
    })
    const router = createRouter({
      history: createMemoryHistory(),
      routes: [{ path: '/:pathMatch(.*)*', component: { template: '<div />' } }],
    })
    const wrapper = mount(PricingPage, {
      global: { plugins: [router, createHead()] },
    })

    await flushPromises()
    await wrapper.get('#beta-member-email').setValue('last@example.com')
    await wrapper.get('.beta-member-form').trigger('submit')
    await flushPromises()

    expect(wrapper.find('.beta-member-card').exists()).toBe(true)
    expect(wrapper.get('.beta-member-message').text()).toContain('领取成功')
    expect(wrapper.get('.beta-member-group-qr img').attributes('src')).toContain('v=456')
    expect(wrapper.get<HTMLButtonElement>('.beta-member-form .button').element.disabled).toBe(true)
  })

  it('shows an expired QR state before claiming when the public image is unavailable', async () => {
    pricingMock.mockRejectedValue(new Error('offline'))
    promotionClaimMock.mockResolvedValue({
      message: '领取成功，1 年 Pro 兑换码已发送至邮箱。',
      promotion: { limit: 100, claimed: 1, remaining: 99, available: true },
      deliveryStatus: 'sent',
      alreadyClaimed: false,
      groupQRCodeUrl: null,
    })
    const router = createRouter({
      history: createMemoryHistory(),
      routes: [{ path: '/:pathMatch(.*)*', component: { template: '<div />' } }],
    })
    const wrapper = mount(PricingPage, {
      global: { plugins: [router, createHead()] },
    })

    await flushPromises()
    expect(wrapper.get('.beta-member-group-qr img').attributes('src')).toBe(
      '/api/v1/promotions/beta-pro/group-qr',
    )
    await wrapper.get('.beta-member-group-qr img').trigger('error')
    expect(wrapper.get('.beta-member-group-qr-expired').text()).toBe('二维码失效')
    expect(wrapper.get('.beta-member-group-qr').text()).toContain(
      '若二维码失效，请联系微信 lyming555 或 QQ 44185539。',
    )

    await wrapper.get('#beta-member-email').setValue('member@example.com')
    await wrapper.get('.beta-member-form').trigger('submit')
    await flushPromises()

    expect(wrapper.get('.beta-member-message').text()).toContain('领取成功')
    expect(wrapper.get('.beta-member-group-qr-expired').text()).toBe('二维码失效')
    expect(wrapper.find('.beta-member-group-qr img').exists()).toBe(false)
    expect(wrapper.find('.beta-member-contact').exists()).toBe(false)
  })

  it('shows the daily 30-day trial fallback when no beta-code quota remains', async () => {
    pricingMock.mockRejectedValue(new Error('offline'))
    promotionStatusMock.mockResolvedValue({
      limit: 100,
      claimed: 100,
      remaining: 0,
      available: false,
    })
    const router = createRouter({
      history: createMemoryHistory(),
      routes: [{ path: '/:pathMatch(.*)*', component: { template: '<div />' } }],
    })
    const wrapper = mount(PricingPage, {
      global: { plugins: [router, createHead()] },
    })

    await flushPromises()

    expect(wrapper.find('.trial-member-card').exists()).toBe(true)
    expect(wrapper.find('#beta-member-email').exists()).toBe(false)
    expect(wrapper.find('#trial-member-email').exists()).toBe(true)
    expect(wrapper.text()).toContain('留下邮箱，领取 30 天 Pro 试用码')
    expect(wrapper.get('.trial-member-card .beta-member-badge').text()).toBe('每日限量 · 每天刷新')
    expect(wrapper.get('.trial-member-card .beta-member-quota').text()).toBe('今日剩余 100 份')
    expect(wrapper.get('.trial-member-card .beta-member-group-qr img').attributes('src')).toBe(
      '/api/v1/promotions/beta-pro/group-qr',
    )
    expect(wrapper.get('.trial-member-card .beta-member-group-qr').text()).toContain(
      '限时开放 · 扫码加入 WenzWork 内测微信交流群',
    )
    expect(wrapper.get('.trial-member-card .beta-member-group-qr').text()).toContain(
      '若二维码失效，请联系微信 lyming555 或 QQ 44185539。',
    )
    expect(wrapper.text()).not.toContain('填写邮箱，免费领取 1 年 Pro 兑换码')
    expect(wrapper.text()).not.toContain('如何领取 1 年 Pro 内测兑换码？')

    await wrapper.get('.trial-member-card .beta-member-group-qr img').trigger('error')
    expect(wrapper.get('.trial-member-card .beta-member-group-qr-expired').text()).toBe(
      '二维码失效',
    )

    await wrapper.get('.pricing-card.featured .button').trigger('click')
    expect(wrapper.get('dialog').text()).not.toContain('填写邮箱加入内测')
  })

  it('claims the daily trial code from the fallback card', async () => {
    pricingMock.mockRejectedValue(new Error('offline'))
    promotionStatusMock.mockResolvedValue({
      limit: 100,
      claimed: 100,
      remaining: 0,
      available: false,
    })
    trialClaimMock.mockResolvedValue({
      message: '领取成功，30 天 Pro 试用码已发送至邮箱。',
      promotion: {
        enabled: true,
        available: true,
        dailyLimit: 100,
        claimedToday: 1,
        remainingToday: 99,
        grantDays: 30,
        refreshesAt: '2026-07-26T16:00:00Z',
      },
      deliveryStatus: 'sent',
      alreadyClaimed: false,
    })
    const router = createRouter({
      history: createMemoryHistory(),
      routes: [{ path: '/:pathMatch(.*)*', component: { template: '<div />' } }],
    })
    const wrapper = mount(PricingPage, {
      global: { plugins: [router, createHead()] },
    })

    await flushPromises()
    await wrapper.get('#trial-member-email').setValue('trial@example.com')
    await wrapper.get('.trial-member-card .beta-member-form').trigger('submit')
    await flushPromises()

    expect(trialClaimMock).toHaveBeenCalledWith('trial@example.com')
    expect(wrapper.get('.trial-member-card .beta-member-message').text()).toContain(
      '30 天 Pro 试用码已发送至邮箱',
    )
    expect(wrapper.get('.trial-member-card .beta-member-quota').text()).toBe('今日剩余 99 份')
    expect(wrapper.get('.trial-member-card .beta-member-group-qr img').attributes('src')).toBe(
      '/api/v1/promotions/beta-pro/group-qr',
    )
  })

  it('fails closed when both promotion status endpoints cannot be loaded', async () => {
    pricingMock.mockRejectedValue(new Error('offline'))
    promotionStatusMock.mockRejectedValue(new Error('promotion offline'))
    trialStatusMock.mockRejectedValue(new Error('trial promotion offline'))
    const router = createRouter({
      history: createMemoryHistory(),
      routes: [{ path: '/:pathMatch(.*)*', component: { template: '<div />' } }],
    })
    const wrapper = mount(PricingPage, {
      global: { plugins: [router, createHead()] },
    })

    await flushPromises()

    expect(wrapper.find('.beta-member-card').exists()).toBe(false)
    expect(wrapper.text()).not.toContain('填写邮箱，免费领取 1 年 Pro 兑换码')
    expect(wrapper.find('#trial-member-email').exists()).toBe(false)
  })
})
