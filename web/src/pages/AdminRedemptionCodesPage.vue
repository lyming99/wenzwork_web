<script setup lang="ts">
import { useHead } from '@unhead/vue'
import { computed, onMounted, ref, watch } from 'vue'

import { problemMessage } from '@/api/auth'
import {
  createRedemptionCodeBatch,
  listLifetimeCodeDeliveries,
  listRedemptionCodes,
  listRedemptionCodeBatches,
  revokeRedemptionCode,
  revokeRedemptionCodeBatch,
  sendLifetimeCode,
  type CreateRedemptionCodeBatchRequest,
  type LifetimeCodeDelivery,
  type RedemptionCodeBatch,
  type RedemptionCodeSummary,
} from '@/api/membership'

useHead({ title: '兑换码管理｜WenzWork', meta: [{ name: 'robots', content: 'noindex, nofollow' }] })

const batches = ref<RedemptionCodeBatch[]>([])
const codes = ref<RedemptionCodeSummary[]>([])
const lifetimeDeliveries = ref<LifetimeCodeDelivery[]>([])
const codeTotal = ref(0)
const loading = ref(true)
const pending = ref(false)
const deliveryPending = ref(false)
const errorMessage = ref('')
const message = ref('')
const plaintextCodes = ref<string[]>([])
const createdBatchName = ref('')
const deliveryEmail = ref('')
const deliveryRequestId = ref('')

const name = ref('')
const planCode = ref('pro')
const grantType = ref<'duration' | 'lifetime'>('duration')
const grantDays = ref(30)
const quantity = ref(10)
const redeemBefore = ref('')
const note = ref('')
const codeBatchFilter = ref('')
const codeStatusFilter = ref<'' | 'active' | 'redeemed' | 'revoked'>('')

const canSubmit = computed(
  () =>
    Boolean(name.value.trim()) &&
    quantity.value >= 1 &&
    quantity.value <= 5000 &&
    (grantType.value === 'lifetime' || grantDays.value > 0),
)
const canSendLifetimeCode = computed(
  () =>
    Boolean(deliveryEmail.value.trim()) &&
    deliveryEmail.value.trim().length <= 320 &&
    !deliveryPending.value,
)

const formatDate = (value: string) =>
  new Intl.DateTimeFormat('zh-CN', { dateStyle: 'medium', timeStyle: 'short' }).format(
    new Date(value),
  )

const load = async () => {
  loading.value = true
  errorMessage.value = ''
  try {
    batches.value = await listRedemptionCodeBatches()
  } catch (error) {
    errorMessage.value = problemMessage(error, '暂时无法读取兑换码批次。')
  } finally {
    loading.value = false
  }
}

const loadCodes = async () => {
  errorMessage.value = ''
  try {
    const result = await listRedemptionCodes({
      batchId: codeBatchFilter.value || undefined,
      status: codeStatusFilter.value || undefined,
      limit: 200,
    })
    codes.value = result.items
    codeTotal.value = result.total
  } catch (error) {
    errorMessage.value = problemMessage(error, '暂时无法读取兑换码状态。')
  }
}

const loadLifetimeDeliveries = async () => {
  try {
    lifetimeDeliveries.value = await listLifetimeCodeDeliveries(20)
  } catch (error) {
    if (!errorMessage.value) {
      errorMessage.value = problemMessage(error, '暂时无法读取永久激活码发送记录。')
    }
  }
}

const deliverLifetimeCode = async (existing?: LifetimeCodeDelivery) => {
  const email = existing?.email ?? deliveryEmail.value.trim()
  if (!email || deliveryPending.value) return

  const confirmed = window.confirm(
    existing
      ? `重新向 ${email} 发送尾号 ${existing.codeHint} 的同一个永久激活码？`
      : `确认生成 1 个永久 Pro 激活码并发送到 ${email}？`,
  )
  if (!confirmed) return

  const requestId = existing?.id ?? (deliveryRequestId.value || globalThis.crypto.randomUUID())
  if (!existing) deliveryRequestId.value = requestId
  deliveryPending.value = true
  errorMessage.value = ''
  message.value = ''
  try {
    const delivery = await sendLifetimeCode(email, requestId)
    message.value =
      delivery.deliveryStatus === 'sent'
        ? `永久 Pro 激活码（尾号 ${delivery.codeHint}）已发送至 ${delivery.email}。`
        : `永久 Pro 激活码正在发送至 ${delivery.email}，请稍后刷新状态。`
    if (!existing) {
      deliveryEmail.value = ''
      deliveryRequestId.value = ''
    }
    await Promise.all([load(), loadCodes(), loadLifetimeDeliveries()])
  } catch (error) {
    errorMessage.value = problemMessage(
      error,
      '永久激活码暂时未能发出；系统已保留同一个激活码，请在发送记录中重试。',
    )
    await loadLifetimeDeliveries()
  } finally {
    deliveryPending.value = false
  }
}

const create = async () => {
  pending.value = true
  errorMessage.value = ''
  message.value = ''
  plaintextCodes.value = []
  const request: CreateRedemptionCodeBatchRequest = {
    name: name.value.trim(),
    planCode: planCode.value,
    grantType: grantType.value,
    grantDays: grantType.value === 'duration' ? grantDays.value : null,
    quantity: quantity.value,
    redeemBefore: redeemBefore.value ? new Date(redeemBefore.value).toISOString() : null,
    note: note.value.trim(),
  }
  try {
    const result = await createRedemptionCodeBatch(request)
    plaintextCodes.value = result.codes
    createdBatchName.value = result.batch.name
    message.value = `已创建 ${result.codes.length} 个兑换码。明文只显示这一次。`
    name.value = ''
    note.value = ''
    await Promise.all([load(), loadCodes()])
  } catch (error) {
    errorMessage.value = problemMessage(error, '暂时无法创建兑换码批次。')
  } finally {
    pending.value = false
  }
}

const downloadCSV = () => {
  if (!plaintextCodes.value.length) return
  const rows = ['code', ...plaintextCodes.value].join('\r\n')
  const blob = new Blob([`\uFEFF${rows}\r\n`], { type: 'text/csv;charset=utf-8' })
  const url = URL.createObjectURL(blob)
  const anchor = document.createElement('a')
  anchor.href = url
  anchor.download = `${createdBatchName.value.replace(/[^a-zA-Z0-9_-]+/g, '-') || 'wenzwork-codes'}.csv`
  anchor.click()
  URL.revokeObjectURL(url)
}

const revoke = async (batch: RedemptionCodeBatch) => {
  if (!window.confirm(`撤销批次“${batch.name}”中所有未使用的兑换码？已兑换权益不会受影响。`)) return
  errorMessage.value = ''
  try {
    await revokeRedemptionCodeBatch(batch.id)
    message.value = '批次已撤销。'
    await Promise.all([load(), loadCodes()])
  } catch (error) {
    errorMessage.value = problemMessage(error, '暂时无法撤销该批次。')
  }
}

const deleteCode = async (code: RedemptionCodeSummary) => {
  if (!window.confirm(`删除尾号 ${code.codeHint} 的兑换码？删除后不能恢复。`)) return
  errorMessage.value = ''
  message.value = ''
  try {
    await revokeRedemptionCode(code.id)
    message.value = '兑换码已删除。'
    await Promise.all([load(), loadCodes()])
  } catch (error) {
    errorMessage.value = problemMessage(error, '暂时无法删除兑换码。')
  }
}

const codeStatusLabel = (status: RedemptionCodeSummary['status']) =>
  ({ active: '未使用', redeemed: '已兑换', revoked: '已删除' })[status]

const deliveryStatusLabel = (status: LifetimeCodeDelivery['deliveryStatus']) =>
  ({ pending: '发送中', sent: '已发送', failed: '发送失败' })[status]

watch(deliveryEmail, () => {
  if (!deliveryPending.value) deliveryRequestId.value = ''
})

onMounted(() => Promise.all([load(), loadCodes(), loadLifetimeDeliveries()]))
</script>

<template>
  <section class="dashboard-page admin-redemption-page">
    <p class="section-kicker">会员运营</p>
    <h1>兑换码管理</h1>
    <p class="dashboard-lead">
      一键向付费用户发送永久 Pro 激活码，也可按批次创建高熵兑换码并追踪核销状态。
    </p>
    <p v-if="errorMessage" class="form-message form-error" role="alert">{{ errorMessage }}</p>
    <p v-if="message" class="form-message form-success" role="status">{{ message }}</p>

    <form
      class="dashboard-card lifetime-code-delivery-card"
      @submit.prevent="deliverLifetimeCode()"
    >
      <div class="batch-form-heading">
        <div>
          <p class="card-label">付费用户发码</p>
          <h2>一键发送永久激活码</h2>
          <p>
            输入用户留下的收件邮箱，系统会生成一个永久 Pro 激活码并直接发送，管理端不会显示明文。
          </p>
        </div>
        <span class="tag">永久 Pro</span>
      </div>
      <div class="lifetime-code-delivery-action">
        <div class="field-group">
          <label for="lifetime-code-email">收件邮箱</label>
          <input
            id="lifetime-code-email"
            v-model.trim="deliveryEmail"
            type="email"
            maxlength="320"
            autocomplete="email"
            placeholder="例如：buyer@example.com"
            required
          />
        </div>
        <button class="button" type="submit" :disabled="!canSendLifetimeCode">
          {{ deliveryPending ? '正在发送…' : '生成并发送永久激活码' }}
        </button>
      </div>
      <p class="lifetime-code-delivery-note">
        该功能生成普通永久兑换码，不受内测码“一邮箱一次”的规则影响。发送失败时可重试同一个激活码，不会因重试重复生成。
      </p>

      <section class="lifetime-delivery-history" aria-labelledby="lifetime-delivery-title">
        <div class="lifetime-delivery-heading">
          <h3 id="lifetime-delivery-title">最近发送记录</h3>
          <button class="text-button" type="button" @click="loadLifetimeDeliveries">刷新</button>
        </div>
        <div v-if="lifetimeDeliveries.length" class="lifetime-delivery-list">
          <article
            v-for="delivery in lifetimeDeliveries"
            :key="delivery.id"
            class="lifetime-delivery-row"
          >
            <div>
              <strong>{{ delivery.email }}</strong>
              <small>
                激活码尾号 {{ delivery.codeHint }} ·
                {{ codeStatusLabel(delivery.redemptionStatus) }} ·
                {{ formatDate(delivery.createdAt) }}
              </small>
            </div>
            <div class="lifetime-delivery-state">
              <span :class="['tag', { 'tag-muted': delivery.deliveryStatus !== 'sent' }]">
                {{ deliveryStatusLabel(delivery.deliveryStatus) }}
              </span>
              <small>尝试 {{ delivery.deliveryAttempts }} 次</small>
            </div>
            <button
              v-if="delivery.deliveryStatus !== 'sent' && delivery.redemptionStatus === 'active'"
              class="button button-secondary"
              type="button"
              :disabled="deliveryPending"
              @click="deliverLifetimeCode(delivery)"
            >
              重试发送
            </button>
          </article>
        </div>
        <p v-else class="inline-status">还没有永久激活码发送记录。</p>
      </section>
    </form>

    <article v-if="plaintextCodes.length" class="dashboard-card one-time-export-card">
      <div>
        <p class="card-label">一次性明文</p>
        <h2>立即导出并安全保存</h2>
        <p>刷新或离开页面后无法找回这些兑换码。服务端只保存 HMAC 摘要。</p>
      </div>
      <button class="button" type="button" @click="downloadCSV">下载 CSV</button>
      <details>
        <summary>在页面中查看 {{ plaintextCodes.length }} 个兑换码</summary>
        <ol class="plaintext-code-list">
          <li v-for="code in plaintextCodes" :key="code">
            <code>{{ code }}</code>
          </li>
        </ol>
      </details>
    </article>

    <form class="dashboard-card batch-form" @submit.prevent="create">
      <div class="batch-form-heading">
        <div>
          <p class="card-label">新批次</p>
          <h2>生成兑换码</h2>
        </div>
        <span class="tag">管理员权限已验证</span>
      </div>
      <div class="form-grid">
        <div class="field-group field-wide">
          <label for="batch-name">批次名称</label>
          <input
            id="batch-name"
            v-model.trim="name"
            required
            maxlength="120"
            placeholder="例如：2026 秋季内测"
          />
        </div>
        <div class="field-group">
          <label for="batch-plan">会员方案</label>
          <select id="batch-plan" v-model="planCode">
            <option value="pro">Pro</option>
          </select>
        </div>
        <div class="field-group">
          <label for="batch-grant-type">权益类型</label>
          <select id="batch-grant-type" v-model="grantType">
            <option value="duration">限时会员</option>
            <option value="lifetime">长期会员</option>
          </select>
        </div>
        <div v-if="grantType === 'duration'" class="field-group">
          <label for="batch-days">会员天数</label>
          <input
            id="batch-days"
            v-model.number="grantDays"
            type="number"
            required
            min="1"
            max="36500"
          />
        </div>
        <div class="field-group">
          <label for="batch-quantity">生成数量</label>
          <input
            id="batch-quantity"
            v-model.number="quantity"
            type="number"
            required
            min="1"
            max="5000"
          />
        </div>
        <div class="field-group">
          <label for="batch-deadline">最晚兑换时间（可选）</label>
          <input id="batch-deadline" v-model="redeemBefore" type="datetime-local" />
        </div>
        <div class="field-group field-wide">
          <label for="batch-note">内部备注（可选）</label>
          <textarea id="batch-note" v-model.trim="note" maxlength="2000" rows="3"></textarea>
        </div>
      </div>
      <button class="button" type="submit" :disabled="pending || !canSubmit">
        {{ pending ? '正在生成…' : '生成并一次性显示' }}
      </button>
    </form>

    <section class="batch-list-section" aria-labelledby="batch-list-title">
      <div class="section-heading-row">
        <div>
          <p class="section-kicker">批次记录</p>
          <h2 id="batch-list-title">最近创建的批次</h2>
        </div>
        <button class="text-button" type="button" @click="load">刷新</button>
      </div>
      <p v-if="loading" class="inline-status" role="status">正在读取批次…</p>
      <div v-else-if="batches.length" class="batch-list">
        <article v-for="batch in batches" :key="batch.id" class="dashboard-card batch-row">
          <div>
            <div class="batch-title-row">
              <h3>{{ batch.name }}</h3>
              <span :class="['tag', { 'tag-muted': batch.status !== 'active' }]">{{
                batch.status
              }}</span>
            </div>
            <p>
              {{ batch.planCode.toUpperCase() }} ·
              {{ batch.grantType === 'lifetime' ? '长期' : `${batch.grantDays} 天` }} ·
              {{ batch.activeCount }} 可用 · {{ batch.redeemedCount }} 已兑换 ·
              {{ batch.revokedCount }} 已删除
            </p>
            <small
              >创建于 {{ formatDate(batch.createdAt)
              }}<template v-if="batch.redeemBefore">
                · 截止 {{ formatDate(batch.redeemBefore) }}</template
              ></small
            >
          </div>
          <button
            v-if="batch.status === 'active'"
            class="button button-secondary"
            type="button"
            @click="revoke(batch)"
          >
            撤销未用码
          </button>
        </article>
      </div>
      <p v-else class="inline-status">还没有兑换码批次。</p>
    </section>

    <section class="admin-list-section" aria-labelledby="code-status-list-title">
      <div class="section-heading-row">
        <div>
          <p class="section-kicker">兑换码状态</p>
          <h2 id="code-status-list-title">共 {{ codeTotal }} 个兑换码</h2>
        </div>
        <form class="admin-filter-row" @submit.prevent="loadCodes">
          <select v-model="codeBatchFilter" aria-label="兑换码批次">
            <option value="">全部批次</option>
            <option v-for="batch in batches" :key="batch.id" :value="batch.id">
              {{ batch.name }}
            </option>
          </select>
          <select v-model="codeStatusFilter" aria-label="兑换码状态">
            <option value="">全部状态</option>
            <option value="active">未使用</option>
            <option value="redeemed">已兑换</option>
            <option value="revoked">已删除</option>
          </select>
          <button class="button button-secondary" type="submit">筛选</button>
        </form>
      </div>
      <div v-if="codes.length" class="code-status-grid">
        <article v-for="code in codes" :key="code.id" class="dashboard-card code-status-card">
          <div>
            <div class="batch-title-row">
              <code>••••-{{ code.codeHint }}</code>
              <span :class="['tag', { 'tag-muted': code.status !== 'active' }]">{{
                codeStatusLabel(code.status)
              }}</span>
            </div>
            <p>{{ code.batchName }}</p>
            <small v-if="code.redeemedAt">
              {{ code.redeemedEmail || '未知账户' }} · {{ formatDate(code.redeemedAt) }}
            </small>
            <small v-else>创建于 {{ formatDate(code.createdAt) }}</small>
          </div>
          <button
            v-if="code.status === 'active'"
            class="text-button danger-text-button"
            type="button"
            @click="deleteCode(code)"
          >
            删除兑换码
          </button>
        </article>
      </div>
      <p v-else class="inline-status">没有符合条件的兑换码。</p>
    </section>
  </section>
</template>
