import type { PricingPlan } from '@/api/catalog'

export const freePlanServiceCopy = '自部署服务免费'

export const billingPeriodLabels: Record<PricingPlan['billingPeriod'], string> = {
  free: '长期可用',
  month: '每月',
  year: '每年',
  one_time: '一次性',
  redemption: '通过兑换码开通',
}

export const formatPricingMinorPrice = (priceMinor: number, currency: string) =>
  new Intl.NumberFormat('zh-CN', {
    style: 'currency',
    currency,
    minimumFractionDigits: 0,
    maximumFractionDigits: 2,
  }).format(priceMinor / 100)

export const formatPricingPlanPrice = (plan: PricingPlan) => {
  if (plan.priceMinor === null) return '待公布'
  return formatPricingMinorPrice(plan.priceMinor, plan.currency)
}

const legacyProPromotionPriceMinor = 3900
const currentProBetaPriceMinor = 5900

export const usesLegacyProPromotionPrice = (plan: PricingPlan) =>
  plan.code.toLowerCase() === 'pro' && plan.priceMinor === legacyProPromotionPriceMinor

const usesOutdatedLifetimeProCopy = (plan: PricingPlan) =>
  plan.code.toLowerCase() === 'pro' &&
  plan.priceMinor === currentProBetaPriceMinor &&
  /(?:永久|一次解锁)/u.test(plan.description)

export const formatPublicPricingPlanPrice = (plan: PricingPlan) =>
  usesLegacyProPromotionPrice(plan)
    ? formatPricingMinorPrice(currentProBetaPriceMinor, plan.currency)
    : formatPricingPlanPrice(plan)

export const publicPricingPlanDescription = (plan: PricingPlan) =>
  usesLegacyProPromotionPrice(plan) || usesOutdatedLifetimeProCopy(plan)
    ? '首年 59 元，次年起 99 元/年，并赠送 WenzMark 会员。'
    : plan.description
