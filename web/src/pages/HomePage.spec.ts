import { flushPromises, mount } from '@vue/test-utils'
import { createHead } from '@unhead/vue/client'
import { createMemoryHistory, createRouter } from 'vue-router'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import { listPricingPlans } from '@/api/catalog'

import HomePage from './HomePage.vue'

vi.mock('@/api/catalog', () => ({
  listPricingPlans: vi.fn(),
}))

const pricingMock = vi.mocked(listPricingPlans)

describe('HomePage', () => {
  beforeEach(() => {
    pricingMock.mockReset()
  })

  it('renders the product value and current published pricing', async () => {
    pricingMock.mockResolvedValue([
      {
        code: 'free',
        name: 'Free',
        description: '自部署服务免费',
        priceMinor: 0,
        originalPriceMinor: null,
        currency: 'CNY',
        billingPeriod: 'free',
        features: ['基础编辑'],
        remoteAccessEnabled: false,
        deviceLimit: 2,
        monthlyTrafficLimitGb: null,
      },
      {
        code: 'pro',
        name: 'Pro',
        description: '内测期间限时39元。',
        priceMinor: 3900,
        originalPriceMinor: 5900,
        currency: 'CNY',
        billingPeriod: 'redemption',
        features: ['更多会员专属功能'],
        remoteAccessEnabled: true,
        deviceLimit: 10,
        monthlyTrafficLimitGb: 10,
      },
    ])
    const router = createRouter({ history: createMemoryHistory(), routes: [] })
    const wrapper = mount(HomePage, {
      global: {
        plugins: [router, createHead()],
      },
    })

    await flushPromises()

    expect(pricingMock).toHaveBeenCalledOnce()
    expect(wrapper.get('h1').text()).toContain('让 AI 进入真实项目，随时接着做')
    expect(wrapper.get('.hero-actions a.button').attributes('href')).toBe('/download')
    expect(wrapper.text()).toContain('一个工作台，四种核心能力')
    expect(wrapper.text()).toContain('真实终端，不是模拟输出')
    expect(wrapper.text()).toContain('项目正文留在设备之间')
    expect(wrapper.text()).toContain('Pro¥59')
    expect(wrapper.find('.home-price-summary').text()).toContain('Free¥0 · 自部署服务免费')
    expect(wrapper.find('.home-price-summary').text()).not.toContain('通过兑换码开通')
    expect(wrapper.text()).toContain('首年 59 元，次年起 99 元/年，并赠送 WenzMark 会员。')
    expect(wrapper.text()).toContain('永久 Pro¥399 · 一次购买 · 限量 50 位')
    expect(wrapper.text()).toContain('每月含 10 GB 远程流量（内测期间不限流量）')
    expect(wrapper.text()).toContain('最多 10 台设备在线')
    expect(wrapper.text()).toContain('项目优势')
    expect(wrapper.text()).toContain('多设备')
    expect(wrapper.text()).toContain('多项目')
    expect(wrapper.text()).toContain('远程控制')
    expect(wrapper.text()).toContain('24 小时在线')
    expect(wrapper.text()).toContain('加密通信')
    expect(wrapper.text()).toContain('DSH 同款 Agent 算法')
    expect(wrapper.get('a[href="https://github.com/lyming99/wenzwork"]').text()).toContain(
      '查看 WenzWork 开源代码',
    )
    expect(wrapper.text()).not.toContain('直播间')
    expect(wrapper.text()).not.toContain('价格待公布')
    expect(wrapper.text()).toContain('桌面端深入工作，浏览器与手机端远程接续')
    expect(wrapper.text()).toContain('可用平台、版本与校验信息以下载页为准')
    expect(wrapper.text()).toContain(
      '可以。产品仍在内测，可用平台、版本与校验信息以下载页为准；如果遇到问题，欢迎加入 QQ 交流群 1026582431 反馈。',
    )
    expect(wrapper.text()).toContain('1026582431')
    expect(wrapper.find('.workspace-window').exists()).toBe(true)
    expect(wrapper.find('.workspace-sidebar').exists()).toBe(true)
    expect(wrapper.find('.workspace-goal').text()).toContain('3 / 4 已完成')
    expect(wrapper.find('img.product-hero-image').exists()).toBe(false)
  })
})
