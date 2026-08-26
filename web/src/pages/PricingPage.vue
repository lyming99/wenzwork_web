<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'

import { listPricingPlans, type PricingPlan } from '@/api/catalog'
import {
  betaPromotionGroupQRCodeURL,
  betaPromotionErrorMessage,
  claimBetaPromotion,
  claimTrialPromotion,
  getBetaPromotionStatus,
  getTrialPromotionStatus,
  trialPromotionErrorMessage,
  type BetaPromotionStatus,
  type TrialPromotionStatus,
} from '@/api/promotions'
import { usePageHead } from '@/composables/usePageHead'
import {
  billingPeriodLabels,
  freePlanServiceCopy,
  formatPricingMinorPrice,
  formatPublicPricingPlanPrice,
  publicPricingPlanDescription,
  usesLegacyProPromotionPrice,
} from '@/utils/pricing'
import wechatRewardCode from '@/assets/wechat-reward-code.jpg'

interface ViewPlan {
  code: string
  name: string
  eyebrow: string
  price: string
  originalPrice?: string
  unit: string
  description: string
  features: string[]
  action: string
  href: string
  featured: boolean
}

type PurchaseOption = 'annual' | 'lifetime'

const annualIntroPrice = '¥59'
const annualRenewalPrice = '¥99'
const lifetimePrice = '¥399'
const lifetimeMemberLimit = 50
const proCoreFeatures = [
  '首年赠送 WenzMark 会员',
  '每月 10 GB 远程流量（内测期间不限流量）',
  '最多 10 台设备同时在线',
]

const fallbackPlans: ViewPlan[] = [
  {
    code: 'free',
    name: 'Free',
    eyebrow: '自部署服务',
    price: '¥0',
    unit: '长期可用',
    description: freePlanServiceCopy,
    features: ['本机项目工作区', '文件、终端、任务与 AI 对话', '公开帮助与版本更新'],
    action: '立即下载',
    href: '/download',
    featured: false,
  },
  {
    code: 'pro',
    name: 'Pro',
    eyebrow: '进阶能力',
    price: annualIntroPrice,
    unit: `首年 · 续费 ${annualRenewalPrice}/年`,
    description: '首年 59 元并赠送 WenzMark 会员，次年起 99 元/年。',
    features: ['包含 Free 全部能力', ...proCoreFeatures],
    action: '开通 Pro',
    href: '/pricing#beta-benefits',
    featured: true,
  },
]

const plans = ref(fallbackPlans)
const catalogStatus = ref<'static' | 'loading' | 'live' | 'error'>('static')
const purchaseDialog = ref<HTMLDialogElement | null>(null)
const betaPromotion = ref<BetaPromotionStatus | null>(null)
const betaEmail = ref('')
const betaClaimState = ref<'idle' | 'submitting' | 'success' | 'error'>('idle')
const betaClaimMessage = ref('')
const betaGroupQRCodeUrl = ref<string | null>(betaPromotionGroupQRCodeURL)
const betaGroupQRCodeFailed = ref(false)
const trialPromotion = ref<TrialPromotionStatus | null>(null)
const trialEmail = ref('')
const trialClaimState = ref<'idle' | 'submitting' | 'success' | 'error'>('idle')
const trialClaimMessage = ref('')
const selectedPurchaseOption = ref<PurchaseOption>('annual')
const betaPromotionAvailable = computed(() => betaPromotion.value?.available === true)
const showBetaPromotionCard = computed(
  () => betaPromotionAvailable.value || betaClaimState.value === 'success',
)
const trialPromotionAvailable = computed(() => trialPromotion.value?.available === true)
const showTrialPromotionCard = computed(
  () =>
    !showBetaPromotionCard.value &&
    (trialPromotionAvailable.value || trialClaimState.value === 'success'),
)
const annualPurchasePrice = computed(
  () => plans.value.find((plan) => plan.code.toLowerCase() === 'pro')?.price ?? annualIntroPrice,
)
const selectedPurchase = computed(() =>
  selectedPurchaseOption.value === 'lifetime'
    ? {
        name: '永久 Pro',
        price: lifetimePrice,
        detail: `一次购买，永久有效；限量 ${lifetimeMemberLimit} 位。`,
      }
    : {
        name: '年费 Pro',
        price: annualPurchasePrice.value,
        detail: `首年开通，次年起 ${annualRenewalPrice}/年，并赠送 WenzMark 会员。`,
      },
)

const openPurchaseDialog = (option: PurchaseOption = 'annual') => {
  selectedPurchaseOption.value = option
  const dialog = purchaseDialog.value
  if (!dialog || dialog.open) return

  if (typeof dialog.showModal === 'function') dialog.showModal()
  else dialog.setAttribute('open', '')
}

const closePurchaseDialog = () => {
  const dialog = purchaseDialog.value
  if (!dialog) return

  if (typeof dialog.close === 'function') dialog.close()
  else dialog.removeAttribute('open')
}

const closePurchaseDialogFromBackdrop = (event: MouseEvent) => {
  if (event.target === purchaseDialog.value) closePurchaseDialog()
}

const toViewPlan = (plan: PricingPlan): ViewPlan => {
  const isPro = plan.code.toLowerCase() === 'pro'
  const isFree = plan.code.toLowerCase() === 'free'

  return {
    code: plan.code,
    name: plan.name,
    eyebrow: isPro ? '进阶能力' : isFree ? '自部署服务' : '本机工作',
    price:
      isPro && plan.priceMinor === null ? annualIntroPrice : formatPublicPricingPlanPrice(plan),
    originalPrice:
      plan.originalPriceMinor === null || usesLegacyProPromotionPrice(plan)
        ? undefined
        : formatPricingMinorPrice(plan.originalPriceMinor, plan.currency),
    unit: isPro ? `首年 · 续费 ${annualRenewalPrice}/年` : billingPeriodLabels[plan.billingPeriod],
    description: isFree
      ? freePlanServiceCopy
      : isPro && plan.priceMinor === null
        ? '首年 59 元并赠送 WenzMark 会员，次年起 99 元/年。'
        : publicPricingPlanDescription(plan),
    features: isPro ? [...new Set([...plan.features, ...proCoreFeatures])] : plan.features,
    action: isPro ? '开通 Pro' : '立即下载',
    href: isPro ? '/pricing#beta-benefits' : '/download',
    featured: isPro,
  }
}

onMounted(async () => {
  await Promise.all([
    (async () => {
      catalogStatus.value = 'loading'
      try {
        const remotePlans = await listPricingPlans()
        if (remotePlans.length > 0) plans.value = remotePlans.map(toViewPlan)
        catalogStatus.value = 'live'
      } catch {
        catalogStatus.value = 'error'
      }
    })(),
    (async () => {
      try {
        betaPromotion.value = await getBetaPromotionStatus()
      } catch {
        betaPromotion.value = null
      }
    })(),
    (async () => {
      try {
        trialPromotion.value = await getTrialPromotionStatus()
      } catch {
        trialPromotion.value = null
      }
    })(),
  ])
})

const submitBetaClaim = async () => {
  if (betaClaimState.value === 'submitting' || !betaPromotionAvailable.value) return

  betaClaimState.value = 'submitting'
  betaClaimMessage.value = ''
  try {
    const response = await claimBetaPromotion(betaEmail.value.trim())
    betaPromotion.value = response.promotion
    betaClaimState.value = 'success'
    betaClaimMessage.value = response.message
    betaGroupQRCodeUrl.value = response.groupQRCodeUrl
    betaGroupQRCodeFailed.value = response.groupQRCodeUrl === null
    betaEmail.value = ''
  } catch (error) {
    betaClaimState.value = 'error'
    betaClaimMessage.value = betaPromotionErrorMessage(error)
    try {
      betaPromotion.value = await getBetaPromotionStatus()
    } catch {
      // Keep the last known quota when the status endpoint is temporarily unavailable.
    }
  }
}

const submitTrialClaim = async () => {
  if (trialClaimState.value === 'submitting' || !trialPromotionAvailable.value) return

  trialClaimState.value = 'submitting'
  trialClaimMessage.value = ''
  try {
    const response = await claimTrialPromotion(trialEmail.value.trim())
    trialPromotion.value = response.promotion
    trialClaimState.value = 'success'
    trialClaimMessage.value = response.message
    trialEmail.value = ''
  } catch (error) {
    trialClaimState.value = 'error'
    trialClaimMessage.value = trialPromotionErrorMessage(error)
    try {
      trialPromotion.value = await getTrialPromotionStatus()
    } catch {
      // Keep the last known daily quota when the status endpoint is temporarily unavailable.
    }
  }
}

const formatTrialRefreshTime = (value?: string) =>
  value
    ? new Intl.DateTimeFormat('zh-CN', {
        timeZone: 'Asia/Shanghai',
        month: 'numeric',
        day: 'numeric',
        hour: '2-digit',
        minute: '2-digit',
      }).format(new Date(value))
    : '次日 00:00'

const comparisons = [
  { feature: '本机项目工作区', free: '包含', pro: '包含' },
  { feature: '文件、终端、任务与 AI 对话', free: '包含', pro: '包含' },
  { feature: '远程流量', free: '—', pro: '每月 10 GB（内测期间不限流量）' },
  { feature: '同时在线设备', free: '—', pro: '最多 10 台' },
  { feature: 'WenzMark 会员', free: '—', pro: '首年赠送' },
  {
    feature: '开通价格',
    free: freePlanServiceCopy,
    pro: `首年 ${annualIntroPrice}，续费 ${annualRenewalPrice}/年；永久 ${lifetimePrice}`,
  },
]

usePageHead({
  title: '产品价格',
  description:
    '对比 WenzWork Free 与 Pro 方案：Free 自部署服务免费；Pro 首年 59 元、续费 99 元/年、永久会员 399 元。',
  path: '/pricing',
})
</script>

<template>
  <section class="pricing-hero">
    <div class="shell pricing-hero-copy">
      <p class="section-kicker">产品价格</p>
      <h1>免费开始，<br />需要时再升级 <span>Pro</span>。</h1>
      <p class="page-lead">
        Free 自部署服务免费，可长期使用；Pro 首年 59 元并赠送 WenzMark 会员，次年起 99
        元/年。永久会员 399 元，限量 50 位。所有价格均为人民币。
      </p>
    </div>
  </section>

  <section class="content-section pricing-section">
    <div class="shell">
      <h1 class="sr-only">产品价格</h1>
      <p v-if="catalogStatus === 'loading'" class="data-source-status" role="status">
        正在同步最新价格目录…
      </p>
      <p v-else-if="catalogStatus === 'error'" class="data-source-status warning" role="status">
        价格服务暂时不可用，当前展示页面预置方案。
      </p>
    </div>
    <div class="shell pricing-grid">
      <article
        v-for="plan in plans"
        :key="plan.code"
        :class="['pricing-card', { featured: plan.featured }]"
      >
        <div class="pricing-card-heading">
          <span>{{ plan.eyebrow }}</span>
          <strong v-if="plan.featured" class="tag">推荐方案</strong>
        </div>
        <h2>{{ plan.name }}</h2>
        <div class="price-line">
          <strong>{{ plan.price }}</strong>
          <del v-if="plan.originalPrice" :aria-label="`原价 ${plan.originalPrice}`">
            原价 {{ plan.originalPrice }}
          </del>
          <span>{{ plan.unit }}</span>
        </div>
        <p>{{ plan.description }}</p>
        <ul class="check-list">
          <li v-for="feature in plan.features" :key="feature">
            <span aria-hidden="true">✓</span>{{ feature }}
          </li>
        </ul>
        <button
          v-if="plan.featured"
          type="button"
          class="button"
          @click="openPurchaseDialog('annual')"
        >
          {{ plan.action }}
        </button>
        <RouterLink v-else class="button button-secondary" :to="plan.href">
          {{ plan.action }}
        </RouterLink>
      </article>
    </div>

    <article v-if="showBetaPromotionCard" id="beta-membership" class="shell beta-member-card">
      <div class="beta-member-copy">
        <div class="beta-member-heading">
          <p class="section-kicker">内测会员</p>
          <span class="beta-member-badge">限量发放 · 手慢无</span>
        </div>
        <h2>填写邮箱，免费领取 1 年 Pro</h2>
        <p>
          提交后，系统会生成专属兑换码并发送到该邮箱。兑换时请使用同一邮箱注册并验证 WenzWork
          账号；邮件中也会附上内测群联系方式。
        </p>
        <ul class="beta-member-points">
          <li><span aria-hidden="true">✓</span>1 年 Pro 会员兑换码</li>
          <li><span aria-hidden="true">✓</span>每个邮箱限领、限用 1 份</li>
          <li><span aria-hidden="true">✓</span>仅限领取邮箱对应账号兑换</li>
          <li><span aria-hidden="true">✓</span>永久会员不可重复领取权益</li>
        </ul>
      </div>

      <div class="beta-member-claim">
        <p class="beta-member-quota" aria-live="polite">
          目前剩余 <strong>{{ betaPromotion?.remaining }}</strong> 份
        </p>
        <form class="beta-member-form" @submit.prevent="submitBetaClaim">
          <label for="beta-member-email">接收兑换码的邮箱</label>
          <div class="beta-member-input-row">
            <input
              id="beta-member-email"
              v-model="betaEmail"
              type="email"
              name="email"
              maxlength="320"
              autocomplete="email"
              inputmode="email"
              placeholder="name@example.com"
              required
              :disabled="betaClaimState === 'submitting' || !betaPromotionAvailable"
            />
            <button
              type="submit"
              class="button"
              :disabled="betaClaimState === 'submitting' || !betaPromotionAvailable"
            >
              <template v-if="betaClaimState === 'submitting'">正在发送…</template>
              <template v-else>领取兑换码</template>
            </button>
          </div>
        </form>
        <p class="beta-member-privacy">
          邮箱仅用于发送兑换码和校验领取资格。每个邮箱限领、限用一次，永久会员不可参与。
        </p>
        <p
          v-if="betaClaimMessage"
          :class="['beta-member-message', betaClaimState]"
          :role="betaClaimState === 'error' ? 'alert' : 'status'"
        >
          {{ betaClaimMessage }}
        </p>
        <figure class="beta-member-group-qr">
          <img
            v-if="betaGroupQRCodeUrl && !betaGroupQRCodeFailed"
            :src="betaGroupQRCodeUrl"
            alt="WenzWork 内测微信交流群二维码"
            decoding="async"
            @error="betaGroupQRCodeFailed = true"
            @load="betaGroupQRCodeFailed = false"
          />
          <div v-else class="beta-member-group-qr-expired" role="status">二维码失效</div>
          <figcaption>
            <strong>扫码加入 WenzWork 内测微信交流群</strong>
            <span>若二维码失效，请联系微信 lyming555 或 QQ 44185539。</span>
          </figcaption>
        </figure>
      </div>
    </article>

    <article
      v-else-if="showTrialPromotionCard"
      id="trial-membership"
      class="shell beta-member-card trial-member-card"
    >
      <div class="beta-member-copy">
        <div class="beta-member-heading">
          <p class="section-kicker">Pro 试用</p>
          <span class="beta-member-badge">每日限量 · 每天刷新</span>
        </div>
        <h2>留下邮箱，领取 30 天 Pro 试用码</h2>
        <p>
          内测码当前不可领取时，可以申请一枚专属 30 天 Pro
          试用码。兑换码会发送至邮箱，并且仅限该邮箱对应的 WenzWork 账号兑换。
        </p>
        <ul class="beta-member-points">
          <li><span aria-hidden="true">✓</span>30 天 Pro 完整试用</li>
          <li><span aria-hidden="true">✓</span>每日自动刷新领取名额</li>
          <li><span aria-hidden="true">✓</span>每个邮箱限领、限用 1 份</li>
          <li><span aria-hidden="true">✓</span>兑换码不在网页明文展示</li>
        </ul>
      </div>

      <div class="beta-member-claim">
        <p class="beta-member-quota" aria-live="polite">
          今日剩余 <strong>{{ trialPromotion?.remainingToday }}</strong> 份
        </p>
        <p class="trial-refresh-note">
          下一次刷新（北京时间）：{{ formatTrialRefreshTime(trialPromotion?.refreshesAt) }}
        </p>
        <form class="beta-member-form" @submit.prevent="submitTrialClaim">
          <label for="trial-member-email">接收试用码的邮箱</label>
          <div class="beta-member-input-row">
            <input
              id="trial-member-email"
              v-model="trialEmail"
              type="email"
              name="email"
              maxlength="320"
              autocomplete="email"
              inputmode="email"
              placeholder="name@example.com"
              required
              :disabled="trialClaimState === 'submitting' || !trialPromotionAvailable"
            />
            <button
              type="submit"
              class="button"
              :disabled="trialClaimState === 'submitting' || !trialPromotionAvailable"
            >
              <template v-if="trialClaimState === 'submitting'">正在发送…</template>
              <template v-else>领取试用码</template>
            </button>
          </div>
        </form>
        <p class="beta-member-privacy">
          邮箱用于发送兑换码及校验兑换资格；每个邮箱只能领取并使用一次试用码。
        </p>
        <p
          v-if="trialClaimMessage"
          :class="['beta-member-message', trialClaimState]"
          :role="trialClaimState === 'error' ? 'alert' : 'status'"
        >
          {{ trialClaimMessage }}
        </p>
        <figure class="beta-member-group-qr">
          <img
            v-if="betaGroupQRCodeUrl && !betaGroupQRCodeFailed"
            :src="betaGroupQRCodeUrl"
            alt="WenzWork 内测微信交流群二维码"
            decoding="async"
            @error="betaGroupQRCodeFailed = true"
            @load="betaGroupQRCodeFailed = false"
          />
          <div v-else class="beta-member-group-qr-expired" role="status">二维码失效</div>
          <figcaption>
            <strong>限时开放 · 扫码加入 WenzWork 内测微信交流群</strong>
            <span>若二维码失效，请联系微信 lyming555 或 QQ 44185539。</span>
          </figcaption>
        </figure>
      </div>
    </article>
  </section>

  <dialog
    ref="purchaseDialog"
    class="purchase-dialog"
    aria-labelledby="purchase-dialog-title"
    aria-describedby="purchase-dialog-description"
    @click="closePurchaseDialogFromBackdrop"
  >
    <div class="purchase-dialog-card">
      <button
        type="button"
        class="purchase-dialog-close"
        aria-label="关闭购买方式"
        @click="closePurchaseDialog"
      >
        ×
      </button>
      <div class="purchase-dialog-heading">
        <p class="section-kicker">购买 Pro</p>
        <h2 id="purchase-dialog-title">选择方案，再通过微信完成开通</h2>
        <p id="purchase-dialog-description">
          当前选择 <strong>{{ selectedPurchase.name }}</strong
          >，价格为 <strong>{{ selectedPurchase.price }}</strong
          >。{{ selectedPurchase.detail }} 请在赞赏留言中填写接收兑换码的邮箱。
        </p>
      </div>

      <div class="purchase-option-grid" role="group" aria-label="Pro 购买方案">
        <button
          type="button"
          :class="{ active: selectedPurchaseOption === 'annual' }"
          :aria-pressed="selectedPurchaseOption === 'annual'"
          @click="selectedPurchaseOption = 'annual'"
        >
          <span>年费 Pro</span>
          <strong>{{ annualPurchasePrice }} 首年</strong>
          <small>续费 {{ annualRenewalPrice }}/年 · 赠 WenzMark 会员</small>
        </button>
        <button
          type="button"
          :class="{ active: selectedPurchaseOption === 'lifetime' }"
          :aria-pressed="selectedPurchaseOption === 'lifetime'"
          @click="selectedPurchaseOption = 'lifetime'"
        >
          <span>永久 Pro</span>
          <strong>{{ lifetimePrice }} 一次性</strong>
          <small>永久有效 · 限量 {{ lifetimeMemberLimit }} 位</small>
        </button>
      </div>

      <div class="purchase-payment-grid">
        <figure class="wechat-reward-code-panel">
          <span>微信赞赏码</span>
          <img
            class="wechat-reward-code-image"
            :src="wechatRewardCode"
            alt="lyming 的微信赞赏码，赞赏时请在留言中填写接收兑换码的邮箱"
            decoding="async"
          />
          <figcaption>赞赏时，请在留言中填写接收兑换码的邮箱</figcaption>
        </figure>

        <div class="purchase-payment-copy">
          <ol class="purchase-step-list">
            <li>
              <div>
                <strong>微信扫码赞赏</strong>
                <span>请输入 {{ selectedPurchase.price }} 作为赞赏金额。</span>
              </div>
            </li>
            <li>
              <div>
                <strong>在留言中填写邮箱</strong>
                <span>请确认邮箱地址准确且能正常收信。</span>
              </div>
            </li>
            <li>
              <div>
                <strong>查收 {{ selectedPurchase.name }} 兑换码</strong>
                <span>系统会将对应方案的兑换码发送到留言中的邮箱，请留意收件箱。</span>
              </div>
            </li>
          </ol>

          <aside class="purchase-email-notice">
            <strong>别忘了在赞赏留言中填写邮箱</strong>
            <span>兑换码只会发送到这个邮箱；若未收到，请一并检查垃圾邮件。</span>
          </aside>

          <div class="purchase-support">
            <span>购买或接收兑换码遇到问题？</span>
            <p>联系客服：QQ <code>44185539</code> 或微信 <code>lyming555</code></p>
          </div>
        </div>
      </div>
      <button type="button" class="button purchase-dialog-confirm" @click="closePurchaseDialog">
        我知道了
      </button>
    </div>
  </dialog>

  <section class="comparison-section">
    <div class="shell">
      <div class="section-heading">
        <p class="section-kicker">方案比较</p>
        <h2>Free 与 Pro，差异一目了然。</h2>
      </div>
      <div class="comparison-table-wrap">
        <table class="comparison-table">
          <thead>
            <tr>
              <th scope="col">能力</th>
              <th scope="col">Free</th>
              <th scope="col">Pro</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="row in comparisons" :key="row.feature">
              <th scope="row">{{ row.feature }}</th>
              <td>{{ row.free }}</td>
              <td>{{ row.pro }}</td>
            </tr>
          </tbody>
        </table>
      </div>
      <p class="pricing-note">
        Pro 首年 59 元、续费 99 元/年；永久会员 399 元且限量 50 位。购买后通过兑换码开通。
      </p>
    </div>
  </section>

  <section class="beta-benefits-section" aria-labelledby="beta-benefits-title">
    <div id="beta-benefits" class="shell beta-benefits">
      <div class="beta-benefits-heading">
        <div>
          <p class="section-kicker">永久会员</p>
          <h2 id="beta-benefits-title">399 元，一次解锁永久 Pro</h2>
        </div>
        <p>永久会员限量 50 位；名额售完后不再按此方案开放。如需帮助，可通过下方联系方式咨询。</p>
      </div>
      <ul class="beta-benefit-list">
        <li>
          <strong>永久价格</strong>
          <span>399 元，一次购买无需续费</span>
        </li>
        <li>
          <strong>限量名额</strong>
          <span>仅开放 50 位永久会员</span>
        </li>
        <li>
          <strong>远程流量</strong>
          <span>每月 10 GB，内测期间不限流量</span>
        </li>
        <li>
          <strong>在线设备</strong>
          <span>最多 10 台设备同时在线</span>
        </li>
      </ul>
      <div class="beta-benefit-contact">
        <div>
          <span>购买与权益咨询</span>
          <strong>微信：<code>lyming555</code>　QQ：<code>44185539</code></strong>
        </div>
        <button type="button" class="button" @click="openPurchaseDialog('lifetime')">
          购买永久 Pro
        </button>
      </div>
    </div>
  </section>

  <section class="faq-section">
    <div class="shell faq-grid">
      <div>
        <p class="section-kicker">价格 FAQ</p>
        <h2>购买前，先了解这些关键信息。</h2>
      </div>
      <div class="faq-list">
        <details>
          <summary>现在可以购买 Pro 吗？</summary>
          <p>
            可以。年费 Pro 首年 59 元，次年起 99 元/年；永久 Pro 为 399 元且限量 50
            位。点击对应购买入口查看具体步骤。
          </p>
        </details>
        <details>
          <summary>Pro 包含多少远程流量和在线设备？</summary>
          <p>
            Pro 每月包含 10 GB 远程流量，内测期间暂不限制流量；同一账户最多支持 10 台设备同时在线。
          </p>
        </details>
        <details>
          <summary>赠送的 WenzMark 会员如何领取？</summary>
          <p>首年开通 Pro 会赠送 WenzMark 会员，兑换与到账方式会随 WenzWork 开通信息一并发送。</p>
        </details>
        <details>
          <summary>重复兑换会员有效期会怎样？</summary>
          <p>同等级会员的有效期会在当前到期日后继续顺延；永久会员不会因兑换较短方案而降级。</p>
        </details>
        <details v-if="betaPromotionAvailable">
          <summary>如何领取 1 年 Pro 内测兑换码？</summary>
          <p>
            在“内测会员”区域填写邮箱并提交，兑换码会发送到该邮箱，且仅限对应的 WenzWork
            账号使用。每个邮箱限领、限用一次，永久会员不可参与；如需加入内测群，可联系微信 lyming555
            或 QQ 44185539。
          </p>
        </details>
        <details v-else-if="trialPromotionAvailable">
          <summary>如何领取 30 天 Pro 试用码？</summary>
          <p>
            在“Pro 试用”卡片填写邮箱，试用码会直接发送到邮箱，并且仅限该邮箱对应的 WenzWork
            账号兑换。名额每天刷新，每个邮箱只能领取并使用一次。
          </p>
        </details>
      </div>
    </div>
  </section>
</template>
