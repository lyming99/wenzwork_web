<script setup lang="ts">
import { useHead } from '@unhead/vue'
import { computed, onMounted, ref } from 'vue'

import {
  archiveAdminPricingPlan,
  createAdminPricingPlan,
  listAdminPricingPlans,
  publishAdminPricingPlan,
  updateAdminPricingPlan,
  type AdminPricingPlan,
  type CreateAdminPricingPlanRequest,
  type UpdateAdminPricingPlanRequest,
} from '@/api/admin'
import { problemMessage } from '@/api/auth'

useHead({
  title: '价格管理｜WenzWork',
  meta: [{ name: 'robots', content: 'noindex, nofollow' }],
})

type BillingPeriod = CreateAdminPricingPlanRequest['billingPeriod']

const plans = ref<AdminPricingPlan[]>([])
const loading = ref(true)
const pending = ref(false)
const errorMessage = ref('')
const message = ref('')
const editingPlan = ref<AdminPricingPlan | null>(null)

const code = ref('')
const name = ref('')
const description = ref('')
const priceMinor = ref(0)
const originalPriceMinor = ref<number | ''>('')
const priceUndecided = ref(false)
const currency = ref('CNY')
const billingPeriod = ref<BillingPeriod>('free')
const featuresText = ref('')
const remoteAccessEnabled = ref(false)
const deviceLimit = ref(10)
const monthlyTrafficLimitGb = ref<number | ''>('')
const sortOrder = ref(0)

const features = computed(() =>
  featuresText.value
    .split(/\r?\n/)
    .map((item) => item.trim())
    .filter(Boolean),
)

const originalPriceValid = computed(
  () =>
    priceUndecided.value ||
    originalPriceMinor.value === '' ||
    (Number.isSafeInteger(originalPriceMinor.value) &&
      originalPriceMinor.value >= 0 &&
      originalPriceMinor.value > priceMinor.value),
)

const canSubmit = computed(
  () =>
    /^[a-z][a-z0-9_-]{0,39}$/.test(code.value.trim().toLowerCase()) &&
    Boolean(name.value.trim()) &&
    /^[A-Za-z]{3}$/.test(currency.value.trim()) &&
    Number.isInteger(sortOrder.value) &&
    sortOrder.value >= -100000 &&
    sortOrder.value <= 100000 &&
    (priceUndecided.value || (Number.isSafeInteger(priceMinor.value) && priceMinor.value >= 0)) &&
    originalPriceValid.value &&
    Number.isInteger(deviceLimit.value) &&
    deviceLimit.value >= 1 &&
    deviceLimit.value <= 100000 &&
    (monthlyTrafficLimitGb.value === '' ||
      (Number.isSafeInteger(monthlyTrafficLimitGb.value) &&
        monthlyTrafficLimitGb.value >= 1 &&
        monthlyTrafficLimitGb.value <= 1000000)) &&
    features.value.length <= 30 &&
    features.value.every((item) => item.length <= 120),
)

const statusLabels: Record<AdminPricingPlan['status'], string> = {
  draft: '草稿',
  published: '已上架',
  archived: '已下架',
}

const billingLabels: Record<BillingPeriod, string> = {
  free: '免费',
  month: '按月',
  year: '按年',
  one_time: '一次性',
  redemption: '兑换码',
}

const formatMinorPrice = (price: number, currencyCode: string) =>
  new Intl.NumberFormat('zh-CN', {
    style: 'currency',
    currency: currencyCode,
    minimumFractionDigits: 0,
    maximumFractionDigits: 2,
  }).format(price / 100)

const formatPrice = (plan: AdminPricingPlan) => {
  if (plan.priceMinor === null) return '待公布'
  return formatMinorPrice(plan.priceMinor, plan.currency)
}

const formatDate = (value: string) =>
  new Intl.DateTimeFormat('zh-CN', { dateStyle: 'medium', timeStyle: 'short' }).format(
    new Date(value),
  )

const load = async () => {
  loading.value = true
  errorMessage.value = ''
  try {
    plans.value = await listAdminPricingPlans()
  } catch (error) {
    errorMessage.value = problemMessage(error, '暂时无法读取价格套餐。')
  } finally {
    loading.value = false
  }
}

const resetForm = () => {
  editingPlan.value = null
  code.value = ''
  name.value = ''
  description.value = ''
  priceMinor.value = 0
  originalPriceMinor.value = ''
  priceUndecided.value = false
  currency.value = 'CNY'
  billingPeriod.value = 'free'
  featuresText.value = ''
  remoteAccessEnabled.value = false
  deviceLimit.value = 10
  monthlyTrafficLimitGb.value = ''
  sortOrder.value = 0
}

const edit = (plan: AdminPricingPlan) => {
  editingPlan.value = plan
  code.value = plan.code
  name.value = plan.name
  description.value = plan.description
  priceUndecided.value = plan.priceMinor === null
  priceMinor.value = plan.priceMinor ?? 0
  originalPriceMinor.value = plan.originalPriceMinor ?? ''
  currency.value = plan.currency
  billingPeriod.value = plan.billingPeriod
  featuresText.value = plan.features.join('\n')
  remoteAccessEnabled.value = plan.remoteAccessEnabled
  deviceLimit.value = plan.deviceLimit
  monthlyTrafficLimitGb.value = plan.monthlyTrafficLimitGb ?? ''
  sortOrder.value = plan.sortOrder
  window.scrollTo({ top: 0, behavior: 'smooth' })
}

const normalizedRequest = (): CreateAdminPricingPlanRequest => ({
  code: code.value.trim().toLowerCase(),
  name: name.value.trim(),
  description: description.value.trim(),
  priceMinor: priceUndecided.value ? null : priceMinor.value,
  originalPriceMinor:
    priceUndecided.value || originalPriceMinor.value === '' ? null : originalPriceMinor.value,
  currency: currency.value.trim().toUpperCase(),
  billingPeriod: billingPeriod.value,
  features: features.value,
  remoteAccessEnabled: remoteAccessEnabled.value,
  deviceLimit: deviceLimit.value,
  monthlyTrafficLimitGb: monthlyTrafficLimitGb.value === '' ? null : monthlyTrafficLimitGb.value,
  sortOrder: sortOrder.value,
})

const save = async () => {
  if (!canSubmit.value) return
  const request = normalizedRequest()
  let confirmPriceChange = false
  if (editingPlan.value) {
    const current = editingPlan.value
    const termsChanged =
      current.priceMinor !== request.priceMinor ||
      current.originalPriceMinor !== request.originalPriceMinor ||
      current.currency !== request.currency ||
      current.billingPeriod !== request.billingPeriod
    if (
      termsChanged &&
      !window.confirm(
        `确认修改“${current.name}”的价格条款？本次保存会生成新版本，但仍需再次发布才会更新官网。`,
      )
    ) {
      return
    }
    confirmPriceChange = termsChanged
  }

  pending.value = true
  errorMessage.value = ''
  message.value = ''
  try {
    if (editingPlan.value) {
      const updateRequest: UpdateAdminPricingPlanRequest = {
        ...request,
        expectedVersion: editingPlan.value.version,
        confirmPriceChange,
      }
      await updateAdminPricingPlan(editingPlan.value.id, updateRequest)
      message.value = '价格套餐新版本已保存；如需更新官网，请执行“确认发布”。'
    } else {
      await createAdminPricingPlan(request)
      message.value = '价格套餐草稿已创建。'
    }
    resetForm()
    await load()
  } catch (error) {
    errorMessage.value = problemMessage(error, '暂时无法保存价格套餐。')
  } finally {
    pending.value = false
  }
}

const publish = async (plan: AdminPricingPlan) => {
  const verb = plan.status === 'published' ? '发布这个新版本' : '上架此套餐'
  const accessImpact = plan.remoteAccessEnabled
    ? `${plan.code === 'free' ? '普通会员将可使用远程设备' : '远程设备接入已开放'}，每账号最多 ${plan.deviceLimit} 台设备，月流量${plan.monthlyTrafficLimitGb === null ? '不限' : `上限 ${plan.monthlyTrafficLimitGb} GB（暂不执行）`}`
    : '该套餐账号将不能使用远程设备'
  if (
    !window.confirm(
      `确认${verb}“${plan.name}”（${formatPrice(plan)} · ${billingLabels[plan.billingPeriod]}）？官网价格目录与设备接入权限会立即更新。${accessImpact}。`,
    )
  ) {
    return
  }
  pending.value = true
  errorMessage.value = ''
  message.value = ''
  try {
    await publishAdminPricingPlan(plan.id, plan.version)
    message.value = `“${plan.name}”当前版本已发布。`
    if (editingPlan.value?.id === plan.id) resetForm()
    await load()
  } catch (error) {
    errorMessage.value = problemMessage(error, '暂时无法发布价格套餐。')
  } finally {
    pending.value = false
  }
}

const archive = async (plan: AdminPricingPlan) => {
  if (
    !window.confirm(`确认下架“${plan.name}”？官网将立即停止展示，但全部版本与审计记录都会保留。`)
  ) {
    return
  }
  pending.value = true
  errorMessage.value = ''
  message.value = ''
  try {
    await archiveAdminPricingPlan(plan.id, plan.version)
    message.value = `“${plan.name}”已下架。`
    if (editingPlan.value?.id === plan.id) resetForm()
    await load()
  } catch (error) {
    errorMessage.value = problemMessage(error, '暂时无法下架价格套餐。')
  } finally {
    pending.value = false
  }
}

onMounted(load)
</script>

<template>
  <section class="dashboard-page admin-wide-page">
    <p class="section-kicker">内容运营</p>
    <h1>价格管理</h1>
    <p class="dashboard-lead">
      统一管理价格、设备接入和套餐限额。编辑已上架套餐只会生成待发布版本，确认发布后官网与设备权限才会更新。
    </p>
    <p v-if="errorMessage" class="form-message form-error" role="alert">{{ errorMessage }}</p>
    <p v-if="message" class="form-message form-success" role="status">{{ message }}</p>

    <form class="dashboard-card pricing-editor" @submit.prevent="save">
      <div class="batch-form-heading">
        <div>
          <p class="card-label">{{ editingPlan ? '编辑套餐' : '新套餐' }}</p>
          <h2>{{ editingPlan ? `编辑 ${editingPlan.name}` : '创建价格套餐草稿' }}</h2>
        </div>
        <button v-if="editingPlan" class="text-button" type="button" @click="resetForm">
          取消编辑
        </button>
      </div>

      <div class="form-grid">
        <div class="field-group">
          <label for="pricing-code">套餐代码</label>
          <input
            id="pricing-code"
            v-model.trim="code"
            required
            maxlength="40"
            pattern="[a-z][a-z0-9_-]{0,39}"
            placeholder="例如：team"
            :disabled="editingPlan?.publishedVersion !== null && editingPlan !== null"
          />
          <small
            v-if="editingPlan?.publishedVersion !== null && editingPlan !== null"
            class="field-hint"
          >
            已发布套餐的代码用于稳定链接，不能再修改。
          </small>
        </div>
        <div class="field-group">
          <label for="pricing-name">显示名称</label>
          <input id="pricing-name" v-model.trim="name" required maxlength="80" />
        </div>
        <div class="field-group field-wide">
          <label for="pricing-description">套餐说明</label>
          <textarea
            id="pricing-description"
            v-model="description"
            maxlength="500"
            rows="3"
          ></textarea>
        </div>
        <div class="field-group">
          <label for="pricing-price">金额（最小货币单位）</label>
          <input
            id="pricing-price"
            v-model.number="priceMinor"
            type="number"
            min="0"
            step="1"
            :disabled="priceUndecided"
            required
          />
          <small class="field-hint">人民币 ¥128.00 填 12800，禁止使用浮点金额。</small>
        </div>
        <div class="field-group">
          <label for="pricing-original-price">原价（最小货币单位，可选）</label>
          <input
            id="pricing-original-price"
            v-model.number="originalPriceMinor"
            type="number"
            min="0"
            step="1"
            placeholder="例如：5900"
            :disabled="priceUndecided"
          />
          <small class="field-hint">留空则官网不显示划线原价；填写时必须高于当前金额。</small>
        </div>
        <div class="field-group checkbox-field">
          <label><input v-model="priceUndecided" type="checkbox" /> 价格待公布</label>
          <small class="field-hint">启用后服务端保存为 null，官网明确显示“待公布”。</small>
        </div>
        <div class="field-group">
          <label for="pricing-currency">币种</label>
          <input
            id="pricing-currency"
            v-model.trim="currency"
            required
            minlength="3"
            maxlength="3"
          />
        </div>
        <div class="field-group">
          <label for="pricing-period">计费周期</label>
          <select id="pricing-period" v-model="billingPeriod">
            <option value="free">免费</option>
            <option value="month">按月</option>
            <option value="year">按年</option>
            <option value="one_time">一次性</option>
            <option value="redemption">兑换码</option>
          </select>
        </div>
        <div class="field-group">
          <label for="pricing-order">展示顺序</label>
          <input
            id="pricing-order"
            v-model.number="sortOrder"
            type="number"
            min="-100000"
            max="100000"
            step="1"
            required
          />
          <small class="field-hint">数值越小越靠前；保存并发布后官网顺序才会改变。</small>
        </div>
        <div class="field-group checkbox-field field-wide">
          <label for="pricing-remote-access">
            <input id="pricing-remote-access" v-model="remoteAccessEnabled" type="checkbox" />
            {{
              code.trim().toLowerCase() === 'free'
                ? '临时开放普通会员使用远程设备'
                : '允许此套餐使用远程设备'
            }}
          </label>
          <small class="field-hint">
            {{
              code.trim().toLowerCase() === 'free'
                ? '发布后，无有效付费会员的普通注册用户也可使用；关闭并发布即可结束免费开放。'
                : '关闭并发布后，此套餐账号将不能新接入或继续使用远程设备。'
            }}
          </small>
        </div>
        <div class="field-group">
          <label for="pricing-device-limit">设备数量限制</label>
          <input
            id="pricing-device-limit"
            v-model.number="deviceLimit"
            type="number"
            min="1"
            max="100000"
            step="1"
            required
          />
          <small class="field-hint">发布后立即用于新设备接入；调低不会自动删除已有设备。</small>
        </div>
        <div class="field-group">
          <label for="pricing-traffic-limit">每月流量限制（GB，可选）</label>
          <input
            id="pricing-traffic-limit"
            v-model.number="monthlyTrafficLimitGb"
            type="number"
            min="1"
            max="1000000"
            step="1"
            placeholder="留空表示不限流量"
          />
          <small class="field-hint">当前仅保存和展示该配置，系统暂不执行流量拦截。</small>
        </div>
        <div class="field-group field-wide">
          <label for="pricing-features">功能列表</label>
          <textarea
            id="pricing-features"
            v-model="featuresText"
            rows="7"
            placeholder="每行一项，最多 30 项"
          ></textarea>
          <small class="field-hint">已填写 {{ features.length }} / 30 项。</small>
        </div>
      </div>

      <button class="button" type="submit" :disabled="pending || !canSubmit">
        {{ pending ? '正在保存…' : editingPlan ? '保存新版本' : '创建套餐草稿' }}
      </button>
    </form>

    <section class="admin-list-section" aria-labelledby="pricing-list-title">
      <div class="section-heading-row">
        <div>
          <p class="section-kicker">版本与状态</p>
          <h2 id="pricing-list-title">全部价格套餐</h2>
        </div>
        <button class="text-button" type="button" @click="load">刷新</button>
      </div>
      <p v-if="loading" class="inline-status" role="status">正在读取价格套餐…</p>
      <div v-else-if="plans.length" class="pricing-admin-list">
        <article v-for="plan in plans" :key="plan.id" class="dashboard-card pricing-admin-card">
          <div class="pricing-admin-heading">
            <div>
              <div class="batch-title-row">
                <h3>{{ plan.name }}</h3>
                <span :class="['tag', { 'tag-muted': plan.status !== 'published' }]">
                  {{ statusLabels[plan.status] }}
                </span>
                <span v-if="plan.hasUnpublishedChanges" class="tag tag-warning">有待发布更改</span>
                <span :class="['tag', { 'tag-muted': !plan.remoteAccessEnabled }]">
                  {{
                    plan.remoteAccessEnabled
                      ? plan.code === 'free'
                        ? '普通会员临时开放'
                        : '设备接入已开放'
                      : '设备接入未开放'
                  }}
                </span>
              </div>
              <strong>
                {{ formatPrice(plan) }}
                <template v-if="plan.originalPriceMinor !== null">
                  · 原价 {{ formatMinorPrice(plan.originalPriceMinor, plan.currency) }}
                </template>
                · {{ billingLabels[plan.billingPeriod] }}
              </strong>
              <small>
                {{ plan.code }} · 顺序 {{ plan.sortOrder }} · 当前 v{{ plan.version }}
                <template v-if="plan.publishedVersion">
                  · 线上 v{{ plan.publishedVersion }}</template
                >
              </small>
            </div>
            <div class="admin-row-actions">
              <button
                class="button button-secondary"
                type="button"
                :disabled="pending"
                @click="edit(plan)"
              >
                编辑
              </button>
              <button
                v-if="plan.status !== 'published' || plan.hasUnpublishedChanges"
                class="button"
                type="button"
                :disabled="pending"
                @click="publish(plan)"
              >
                {{ plan.status === 'published' ? '确认发布' : '确认上架' }}
              </button>
              <button
                v-if="plan.status === 'published'"
                class="text-button danger-text-button"
                type="button"
                :disabled="pending"
                @click="archive(plan)"
              >
                确认下架
              </button>
            </div>
          </div>
          <p>{{ plan.description || '未填写套餐说明。' }}</p>
          <p class="form-hint">
            设备上限 {{ plan.deviceLimit }} 台 · 月流量
            {{ plan.monthlyTrafficLimitGb === null ? '不限' : `${plan.monthlyTrafficLimitGb} GB` }}
            <span>（流量暂不执行拦截）</span>
          </p>
          <ul v-if="plan.features.length" class="compact-feature-list">
            <li v-for="feature in plan.features" :key="feature">{{ feature }}</li>
          </ul>
          <small>最后更新：{{ formatDate(plan.updatedAt) }}</small>
        </article>
      </div>
      <p v-else class="inline-status">还没有价格套餐。</p>
    </section>
  </section>
</template>
