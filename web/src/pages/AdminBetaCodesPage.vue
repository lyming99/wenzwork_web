<script setup lang="ts">
import { useHead } from '@unhead/vue'
import { computed, onMounted, ref } from 'vue'

import { problemMessage } from '@/api/auth'
import {
  getAdminBetaPromotion,
  getAdminTrialPromotion,
  listAdminBetaPromotionClaims,
  listAdminTrialPromotionClaims,
  removeAdminBetaPromotionGroupQRCode,
  updateAdminBetaPromotion,
  updateAdminTrialPromotion,
  uploadAdminBetaPromotionGroupQRCode,
  type BetaPromotionAdminClaim,
  type BetaPromotionAdminOverview,
  type TrialPromotionAdminClaim,
  type TrialPromotionAdminOverview,
} from '@/api/promotions'

useHead({
  title: '内测码与试用码管理｜WenzWork',
  meta: [{ name: 'robots', content: 'noindex, nofollow' }],
})

const overview = ref<BetaPromotionAdminOverview | null>(null)
const claims = ref<BetaPromotionAdminClaim[]>([])
const total = ref(0)
const trialOverview = ref<TrialPromotionAdminOverview | null>(null)
const trialClaims = ref<TrialPromotionAdminClaim[]>([])
const trialTotal = ref(0)
const trialEnabled = ref(true)
const trialDailyQuota = ref(100)
const savingTrial = ref(false)
const loading = ref(true)
const saving = ref(false)
const errorMessage = ref('')
const message = ref('')
const remaining = ref(0)
const groupQRCodeInput = ref<HTMLInputElement | null>(null)
const selectedGroupQRCode = ref<File | null>(null)
const savingGroupQRCode = ref(false)
const removingGroupQRCode = ref(false)
const query = ref('')
const deliveryStatus = ref<'' | 'pending' | 'sent' | 'failed'>('')
const redemptionStatus = ref<'' | 'active' | 'redeemed' | 'revoked'>('')
const limit = 50
const offset = ref(0)

const maximumRemaining = computed(() => Math.max(0, 5000 - (overview.value?.claimed ?? 0)))
const canSave = computed(
  () =>
    Number.isInteger(remaining.value) &&
    remaining.value >= 0 &&
    remaining.value <= maximumRemaining.value &&
    remaining.value !== overview.value?.remaining,
)
const canSaveGroupQRCode = computed(
  () =>
    selectedGroupQRCode.value !== null && !savingGroupQRCode.value && !removingGroupQRCode.value,
)
const pageStart = computed(() => (total.value ? offset.value + 1 : 0))
const pageEnd = computed(() => Math.min(offset.value + limit, total.value))
const canSaveTrial = computed(
  () =>
    trialOverview.value !== null &&
    Number.isInteger(trialDailyQuota.value) &&
    trialDailyQuota.value >= 1 &&
    trialDailyQuota.value <= 5000 &&
    (trialEnabled.value !== trialOverview.value.enabled ||
      trialDailyQuota.value !== trialOverview.value.dailyQuota),
)

const formatDate = (value: string | null) =>
  value
    ? new Intl.DateTimeFormat('zh-CN', { dateStyle: 'medium', timeStyle: 'short' }).format(
        new Date(value),
      )
    : '—'

const campaignStatusLabel = (status: BetaPromotionAdminOverview['status']) =>
  ({ active: '领取中', exhausted: '已领完', disabled: '已停用' })[status]

const deliveryStatusLabel = (status: BetaPromotionAdminClaim['deliveryStatus']) =>
  ({ pending: '发送中', sent: '已发送', failed: '发送失败' })[status]

const redemptionStatusLabel = (status: BetaPromotionAdminClaim['redemptionStatus']) =>
  ({ active: '未兑换', redeemed: '已兑换', revoked: '已撤销' })[status]

const loadOverview = async (syncInput = true) => {
  const result = await getAdminBetaPromotion()
  overview.value = result
  if (syncInput) remaining.value = result.remaining
}

const loadClaims = async () => {
  const result = await listAdminBetaPromotionClaims({
    q: query.value.trim() || undefined,
    deliveryStatus: deliveryStatus.value || undefined,
    redemptionStatus: redemptionStatus.value || undefined,
    limit,
    offset: offset.value,
  })
  claims.value = result.items
  total.value = result.total
}

const loadTrialOverview = async (syncInput = true) => {
  const result = await getAdminTrialPromotion()
  trialOverview.value = result
  if (syncInput) {
    trialEnabled.value = result.enabled
    trialDailyQuota.value = result.dailyQuota
  }
}

const loadTrialClaims = async () => {
  const result = await listAdminTrialPromotionClaims({ limit: 50, offset: 0 })
  trialClaims.value = result.items
  trialTotal.value = result.total
}

const load = async () => {
  loading.value = true
  errorMessage.value = ''
  try {
    await Promise.all([loadOverview(), loadClaims(), loadTrialOverview(), loadTrialClaims()])
  } catch (error) {
    errorMessage.value = problemMessage(error, '暂时无法读取内测码或试用码活动。')
  } finally {
    loading.value = false
  }
}

const saveTrialSettings = async () => {
  if (!canSaveTrial.value || savingTrial.value) return

  savingTrial.value = true
  errorMessage.value = ''
  message.value = ''
  try {
    trialOverview.value = await updateAdminTrialPromotion(trialEnabled.value, trialDailyQuota.value)
    trialEnabled.value = trialOverview.value.enabled
    trialDailyQuota.value = trialOverview.value.dailyQuota
    message.value = trialEnabled.value
      ? `试用码活动已开启，每天刷新 ${trialDailyQuota.value} 份。`
      : `试用码活动已关闭；每日刷新数量已保存为 ${trialDailyQuota.value} 份。`
    await loadTrialClaims()
  } catch (error) {
    errorMessage.value = problemMessage(error, '暂时无法更新试用码设置。')
  } finally {
    savingTrial.value = false
  }
}

const applyFilters = async () => {
  offset.value = 0
  errorMessage.value = ''
  try {
    await loadClaims()
  } catch (error) {
    errorMessage.value = problemMessage(error, '暂时无法读取内测码领取记录。')
  }
}

const changePage = async (nextOffset: number) => {
  offset.value = Math.max(0, nextOffset)
  await applyPage()
}

const applyPage = async () => {
  errorMessage.value = ''
  try {
    await loadClaims()
  } catch (error) {
    errorMessage.value = problemMessage(error, '暂时无法读取内测码领取记录。')
  }
}

const saveRemaining = async () => {
  if (!canSave.value || saving.value) return
  if (
    remaining.value === 0 &&
    !window.confirm('确认清空剩余内测码名额？保存后，官网将立即隐藏所有内测码领取入口。')
  ) {
    return
  }

  saving.value = true
  errorMessage.value = ''
  message.value = ''
  try {
    overview.value = await updateAdminBetaPromotion(remaining.value)
    remaining.value = overview.value.remaining
    message.value =
      remaining.value === 0
        ? '剩余名额已清空，官网内测码领取入口已停用。'
        : `剩余名额已更新为 ${remaining.value} 份。`
    await loadClaims()
  } catch (error) {
    errorMessage.value = problemMessage(error, '暂时无法更新内测码名额。')
  } finally {
    saving.value = false
  }
}

const selectGroupQRCode = (event: Event) => {
  errorMessage.value = ''
  message.value = ''
  const input = event.currentTarget as HTMLInputElement
  const file = input.files?.[0] ?? null
  selectedGroupQRCode.value = null
  if (!file) return

  if (!['image/png', 'image/jpeg'].includes(file.type)) {
    errorMessage.value = '请选择 PNG 或 JPEG 格式的内测群二维码。'
    input.value = ''
    return
  }
  if (file.size === 0 || file.size > 2 * 1024 * 1024) {
    errorMessage.value = '内测群二维码图片大小必须在 2 MiB 以内。'
    input.value = ''
    return
  }
  selectedGroupQRCode.value = file
}

const saveGroupQRCode = async () => {
  if (!canSaveGroupQRCode.value || !selectedGroupQRCode.value) return

  savingGroupQRCode.value = true
  errorMessage.value = ''
  message.value = ''
  try {
    overview.value = await uploadAdminBetaPromotionGroupQRCode(selectedGroupQRCode.value)
    selectedGroupQRCode.value = null
    if (groupQRCodeInput.value) groupQRCodeInput.value.value = ''
    message.value = '内测微信交流群二维码已保存，后续用户领取成功后会立即看到。'
  } catch (error) {
    errorMessage.value = problemMessage(error, '暂时无法保存内测群二维码。')
  } finally {
    savingGroupQRCode.value = false
  }
}

const removeGroupQRCode = async () => {
  if (!overview.value?.groupQRCodeConfigured || removingGroupQRCode.value) return
  if (
    !window.confirm(
      '确认移除内测微信交流群二维码？移除后，新领取用户将不会看到二维码，但仍可通过邮件中的联系方式入群。',
    )
  ) {
    return
  }

  removingGroupQRCode.value = true
  errorMessage.value = ''
  message.value = ''
  try {
    overview.value = await removeAdminBetaPromotionGroupQRCode()
    selectedGroupQRCode.value = null
    if (groupQRCodeInput.value) groupQRCodeInput.value.value = ''
    message.value = '内测微信交流群二维码已移除。'
  } catch (error) {
    errorMessage.value = problemMessage(error, '暂时无法移除内测群二维码。')
  } finally {
    removingGroupQRCode.value = false
  }
}

onMounted(load)
</script>

<template>
  <section class="dashboard-page admin-beta-codes-page">
    <p class="section-kicker">会员运营</p>
    <h1>内测码与试用码管理</h1>
    <p class="dashboard-lead">
      管理官网 1 年 Pro 内测码和每日 30 天 Pro
      试用码，查看邮件投递与兑换状态。页面不会展示兑换码明文。
    </p>
    <p v-if="errorMessage" class="form-message form-error" role="alert">{{ errorMessage }}</p>
    <p v-if="message" class="form-message form-success" role="status">{{ message }}</p>
    <p v-if="loading" class="inline-status" role="status">正在读取会员活动…</p>

    <template v-if="overview">
      <section v-if="trialOverview" class="trial-admin-section" aria-labelledby="trial-admin-title">
        <div class="section-heading-row">
          <div>
            <p class="section-kicker">每日试用码</p>
            <h2 id="trial-admin-title">30 天 Pro 试用活动</h2>
          </div>
          <span :class="['tag', { 'tag-muted': !trialOverview.enabled }]">
            {{ trialOverview.enabled ? '已开启' : '已关闭' }}
          </span>
        </div>

        <div class="beta-admin-summary-grid" aria-label="试用码统计">
          <article class="dashboard-card beta-admin-summary-card">
            <span>今日剩余</span>
            <strong>{{ trialOverview.remainingToday }}</strong>
            <small>今日上限 {{ trialOverview.todayLimit }}</small>
          </article>
          <article class="dashboard-card beta-admin-summary-card">
            <span>今日已领取</span>
            <strong>{{ trialOverview.claimedToday }}</strong>
            <small>每天配置 {{ trialOverview.dailyQuota }} 份</small>
          </article>
          <article class="dashboard-card beta-admin-summary-card">
            <span>累计领取</span>
            <strong>{{ trialOverview.totalClaimCount }}</strong>
            <small>
              已发送 {{ trialOverview.sentDeliveryCount }} · 失败
              {{ trialOverview.failedDeliveryCount }}
            </small>
          </article>
          <article class="dashboard-card beta-admin-summary-card">
            <span>累计已兑换</span>
            <strong>{{ trialOverview.redeemedCodeCount }}</strong>
            <small>
              未兑换 {{ trialOverview.activeCodeCount }} · 已撤销
              {{ trialOverview.revokedCodeCount }}
            </small>
          </article>
        </div>

        <form
          class="dashboard-card beta-promotion-settings trial-promotion-settings"
          @submit.prevent="saveTrialSettings"
        >
          <div>
            <p class="card-label">自动刷新</p>
            <h2>设置每日试用码数量</h2>
            <p>
              试用码按北京时间每天 00:00
              自动刷新，领取时即时生成并通过邮件发送。关闭后前台不会显示试用码；
              已发出的试用码仍可兑换。若当天领取数已超过新上限，新数量会从次日完整生效。
            </p>
          </div>
          <div class="beta-promotion-settings-action">
            <label class="trial-enabled-control">
              <input v-model="trialEnabled" type="checkbox" />
              <span>开启每日试用码</span>
            </label>
            <label for="trial-promotion-daily-quota">每日刷新数量</label>
            <input
              id="trial-promotion-daily-quota"
              v-model.number="trialDailyQuota"
              type="number"
              min="1"
              max="5000"
              step="1"
              required
            />
            <small> 允许设置 1–5000 份；每份为 {{ trialOverview.grantDays }} 天 Pro。 </small>
            <button class="button" type="submit" :disabled="savingTrial || !canSaveTrial">
              {{ savingTrial ? '正在保存…' : '保存试用码设置' }}
            </button>
          </div>
        </form>

        <section class="admin-list-section" aria-labelledby="trial-claim-list-title">
          <div class="section-heading-row">
            <div>
              <p class="section-kicker">试用码记录</p>
              <h2 id="trial-claim-list-title">累计 {{ trialTotal }} 条</h2>
            </div>
            <small>显示最近 {{ Math.min(50, trialTotal) }} 条，不展示兑换码明文</small>
          </div>
          <div v-if="trialClaims.length" class="beta-claim-list">
            <article
              v-for="claim in trialClaims"
              :key="claim.id"
              class="dashboard-card beta-claim-row"
            >
              <div class="beta-claim-main">
                <div class="batch-title-row">
                  <h3>{{ claim.email }}</h3>
                  <code>•••• {{ claim.codeHint }}</code>
                </div>
                <p>
                  {{ claim.claimDate }} 领取 · 最近发送
                  {{ formatDate(claim.lastDeliveryAttemptAt) }} · 尝试
                  {{ claim.deliveryAttempts }} 次
                </p>
                <small v-if="claim.redeemedAt">兑换于 {{ formatDate(claim.redeemedAt) }}</small>
                <small v-else-if="claim.sentAt">发送于 {{ formatDate(claim.sentAt) }}</small>
              </div>
              <div class="beta-claim-state">
                <span :class="['tag', { 'tag-muted': claim.deliveryStatus !== 'sent' }]">
                  {{ deliveryStatusLabel(claim.deliveryStatus) }}
                </span>
                <span :class="['tag', { 'tag-muted': claim.redemptionStatus !== 'redeemed' }]">
                  {{ redemptionStatusLabel(claim.redemptionStatus) }}
                </span>
              </div>
            </article>
          </div>
          <p v-else class="inline-status">还没有试用码领取记录。</p>
        </section>
      </section>

      <div class="section-heading-row beta-admin-section-heading">
        <div>
          <p class="section-kicker">限量内测码</p>
          <h2>1 年 Pro 内测活动</h2>
        </div>
      </div>

      <div class="beta-admin-summary-grid" aria-label="内测码统计">
        <article class="dashboard-card beta-admin-summary-card">
          <span>剩余名额</span>
          <strong>{{ overview.remaining }}</strong>
          <small>总名额 {{ overview.limit }}</small>
        </article>
        <article class="dashboard-card beta-admin-summary-card">
          <span>已领取</span>
          <strong>{{ overview.claimed }}</strong>
          <small
            >已发送 {{ overview.sentDeliveryCount }} · 发送中
            {{ overview.pendingDeliveryCount }}</small
          >
        </article>
        <article class="dashboard-card beta-admin-summary-card">
          <span>已兑换</span>
          <strong>{{ overview.redeemedCodeCount }}</strong>
          <small
            >未兑换 {{ overview.activeCodeCount }} · 已撤销 {{ overview.revokedCodeCount }}</small
          >
        </article>
        <article class="dashboard-card beta-admin-summary-card">
          <span>活动状态</span>
          <strong class="beta-admin-status-value">{{
            campaignStatusLabel(overview.status)
          }}</strong>
          <small>失败邮件 {{ overview.failedDeliveryCount }}</small>
        </article>
      </div>

      <form
        class="dashboard-card beta-promotion-settings beta-quota-settings"
        @submit.prevent="saveRemaining"
      >
        <div>
          <p class="card-label">活动名额</p>
          <h2>设置剩余可领取数量</h2>
          <p>
            设为 0 会立即隐藏官网的内测会员卡片及相关领取文案。该设置只控制
            <code>beta-pro-launch</code> 自动发码，不影响普通兑换码，也不会使已发出的内测码失效。
          </p>
        </div>
        <div class="beta-promotion-settings-action">
          <label for="beta-promotion-remaining">剩余名额</label>
          <input
            id="beta-promotion-remaining"
            v-model.number="remaining"
            type="number"
            min="0"
            :max="maximumRemaining"
            step="1"
            required
          />
          <small>当前最多可设置 {{ maximumRemaining }} 份；活动总量不超过 5000 份。</small>
          <button class="button" type="submit" :disabled="saving || !canSave">
            {{ saving ? '正在保存…' : remaining === 0 ? '清空并停用领取' : '保存剩余名额' }}
          </button>
        </div>
      </form>

      <section class="dashboard-card beta-group-qr-settings" aria-labelledby="beta-group-qr-title">
        <div class="beta-group-qr-copy">
          <p class="card-label">入群引导</p>
          <h2 id="beta-group-qr-title">内测微信交流群二维码</h2>
          <p>
            上传后，只有完成内测码领取的用户才会在成功结果中看到二维码。支持 PNG、JPEG，图片不超过 2
            MiB；替换图片后会自动刷新缓存。
          </p>
          <span :class="['tag', { 'tag-muted': !overview.groupQRCodeConfigured }]">
            {{ overview.groupQRCodeConfigured ? '已配置' : '未配置' }}
          </span>
        </div>

        <div class="beta-group-qr-preview">
          <figure v-if="overview.groupQRCodeUrl">
            <img
              :src="overview.groupQRCodeUrl"
              alt="当前配置的内测微信交流群二维码"
              decoding="async"
            />
            <figcaption>
              当前二维码
              <span v-if="overview.groupQRCodeUpdatedAt">
                · 更新于 {{ formatDate(overview.groupQRCodeUpdatedAt) }}
              </span>
            </figcaption>
          </figure>
          <div v-else class="beta-group-qr-placeholder">
            <span aria-hidden="true">＋</span>
            <strong>尚未上传二维码</strong>
            <small>用户领取成功后仍会看到邮件内的微信与 QQ 联系方式。</small>
          </div>
        </div>

        <form class="beta-group-qr-form" @submit.prevent="saveGroupQRCode">
          <label for="beta-group-qr-file">选择二维码图片</label>
          <input
            id="beta-group-qr-file"
            ref="groupQRCodeInput"
            type="file"
            accept="image/png,image/jpeg"
            :disabled="savingGroupQRCode || removingGroupQRCode"
            @change="selectGroupQRCode"
          />
          <small v-if="selectedGroupQRCode">
            已选择：{{ selectedGroupQRCode.name }}（{{
              Math.max(1, Math.ceil(selectedGroupQRCode.size / 1024))
            }}
            KiB）
          </small>
          <small v-else>推荐使用清晰的正方形原图，最大尺寸 4096×4096。</small>
          <div class="beta-group-qr-actions">
            <button class="button" type="submit" :disabled="!canSaveGroupQRCode">
              {{
                savingGroupQRCode
                  ? '正在保存…'
                  : overview.groupQRCodeConfigured
                    ? '替换二维码'
                    : '上传二维码'
              }}
            </button>
            <button
              v-if="overview.groupQRCodeConfigured"
              class="button button-secondary"
              type="button"
              :disabled="savingGroupQRCode || removingGroupQRCode"
              @click="removeGroupQRCode"
            >
              {{ removingGroupQRCode ? '正在移除…' : '移除二维码' }}
            </button>
          </div>
        </form>
      </section>

      <section class="admin-list-section" aria-labelledby="beta-claim-list-title">
        <div class="section-heading-row">
          <div>
            <p class="section-kicker">领取记录</p>
            <h2 id="beta-claim-list-title">共 {{ total }} 条记录</h2>
          </div>
          <form class="admin-filter-row beta-claim-filter" @submit.prevent="applyFilters">
            <input v-model.trim="query" type="search" maxlength="320" placeholder="搜索邮箱" />
            <select v-model="deliveryStatus" aria-label="邮件状态">
              <option value="">全部邮件状态</option>
              <option value="pending">发送中</option>
              <option value="sent">已发送</option>
              <option value="failed">发送失败</option>
            </select>
            <select v-model="redemptionStatus" aria-label="兑换状态">
              <option value="">全部兑换状态</option>
              <option value="active">未兑换</option>
              <option value="redeemed">已兑换</option>
              <option value="revoked">已撤销</option>
            </select>
            <button class="button button-secondary" type="submit">筛选</button>
          </form>
        </div>

        <div v-if="claims.length" class="beta-claim-list">
          <article v-for="claim in claims" :key="claim.id" class="dashboard-card beta-claim-row">
            <div class="beta-claim-main">
              <div class="batch-title-row">
                <h3>{{ claim.email }}</h3>
                <code>•••• {{ claim.codeHint }}</code>
              </div>
              <p>
                领取于 {{ formatDate(claim.createdAt) }} · 最近发送
                {{ formatDate(claim.lastDeliveryAttemptAt) }} · 尝试 {{ claim.deliveryAttempts }} 次
              </p>
              <small v-if="claim.redeemedAt">兑换于 {{ formatDate(claim.redeemedAt) }}</small>
              <small v-else-if="claim.sentAt">发送于 {{ formatDate(claim.sentAt) }}</small>
            </div>
            <div class="beta-claim-state">
              <span :class="['tag', { 'tag-muted': claim.deliveryStatus !== 'sent' }]">
                {{ deliveryStatusLabel(claim.deliveryStatus) }}
              </span>
              <span :class="['tag', { 'tag-muted': claim.redemptionStatus !== 'redeemed' }]">
                {{ redemptionStatusLabel(claim.redemptionStatus) }}
              </span>
            </div>
          </article>
        </div>
        <p v-else class="inline-status">没有符合条件的内测码记录。</p>

        <div v-if="total > limit" class="analytics-pagination">
          <span>显示 {{ pageStart }}–{{ pageEnd }} / {{ total }}</span>
          <div>
            <button
              class="button button-secondary"
              type="button"
              :disabled="offset === 0"
              @click="changePage(offset - limit)"
            >
              上一页
            </button>
            <button
              class="button button-secondary"
              type="button"
              :disabled="offset + limit >= total"
              @click="changePage(offset + limit)"
            >
              下一页
            </button>
          </div>
        </div>
      </section>
    </template>
  </section>
</template>
