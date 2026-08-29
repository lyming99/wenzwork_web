import { describe, expect, it } from 'vitest'

import type { PricingPlan } from '@/api/catalog'

import {
  formatPublicPricingPlanPrice,
  freePlanServiceCopy,
  publicPricingPlanDescription,
} from './pricing'

const proPlan = (overrides: Partial<PricingPlan> = {}): PricingPlan => ({
  code: 'pro',
  name: 'Pro',
  description: '内测期间，永久 Pro 仅需 59 元。',
  priceMinor: 5900,
  originalPriceMinor: null,
  currency: 'CNY',
  billingPeriod: 'year',
  features: [],
  remoteAccessEnabled: true,
  deviceLimit: 10,
  monthlyTrafficLimitGb: 10,
  ...overrides,
})

describe('public pricing compatibility', () => {
  it('keeps the Free self-hosted service copy in one shared source', () => {
    expect(freePlanServiceCopy).toBe('自部署服务免费')
  })

  it('converts the legacy 39-yuan promotion into the current first-year offer', () => {
    const plan = proPlan({
      priceMinor: 3900,
      originalPriceMinor: 5900,
      description: '内测期间限时 39 元。',
    })

    expect(formatPublicPricingPlanPrice(plan)).toBe('¥59')
    expect(publicPricingPlanDescription(plan)).toBe(
      '首年 59 元，次年起 99 元/年，并赠送 WenzMark 会员。',
    )
  })

  it('replaces already-published 59-yuan lifetime copy without rewriting custom plans', () => {
    expect(publicPricingPlanDescription(proPlan())).toContain('首年 59 元')
    expect(
      publicPricingPlanDescription(
        proPlan({ priceMinor: 12800, description: '管理员发布的自定义方案说明。' }),
      ),
    ).toBe('管理员发布的自定义方案说明。')
  })
})
