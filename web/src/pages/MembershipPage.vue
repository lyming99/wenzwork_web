<script setup lang="ts">
import { useHead } from '@unhead/vue'
import { computed, onMounted, ref } from 'vue'

import { problemMessage } from '@/api/auth'
import {
  getMembership,
  listRedemptions,
  redeemMembershipCode,
  type Membership,
  type RedemptionRecord,
} from '@/api/membership'
import { listRemoteDevices, type RemoteDevice } from '@/api/remote'

useHead({ title: '会员中心｜WenzWork', meta: [{ name: 'robots', content: 'noindex, nofollow' }] })

const membership = ref<Membership | null>(null)
const redemptions = ref<RedemptionRecord[]>([])
const code = ref('')
const loading = ref(true)
const pending = ref(false)
const message = ref('')
const errorMessage = ref('')
const devices = ref<RemoteDevice[]>([])
const devicesLoading = ref(true)
const devicesError = ref('')

const onlineDeviceCount = computed(
  () =>
    devices.value.filter((device) => device.status === 'active' && device.presence === 'online')
      .length,
)

const expiryLabel = computed(() => {
  if (!membership.value) return '正在读取…'
  if (membership.value.lifetime || !membership.value.expiresAt) return '长期有效'
  return `有效至 ${formatDate(membership.value.expiresAt)}`
})

const formatDate = (value: string) =>
  new Intl.DateTimeFormat('zh-CN', { dateStyle: 'medium', timeStyle: 'short' }).format(
    new Date(value),
  )

const formatDeviceDate = (value: string | null) => (value ? formatDate(value) : '从未在线')

const platformLabel = (platform: RemoteDevice['platform']) =>
  ({ windows: 'Windows', macos: 'macOS', linux: 'Linux' })[platform]

const presenceLabel = (device: RemoteDevice) => {
  if (device.status === 'revoked') return '已吊销'
  if (device.status !== 'active') return '未启用'
  return { online: '在线', offline: '离线', degraded: '连接不稳定' }[device.presence]
}

const load = async () => {
  loading.value = true
  errorMessage.value = ''
  try {
    ;[membership.value, redemptions.value] = await Promise.all([getMembership(), listRedemptions()])
  } catch (error) {
    errorMessage.value = problemMessage(error, '暂时无法读取会员信息。')
  } finally {
    loading.value = false
  }
}

const loadDevices = async () => {
  devicesLoading.value = true
  devicesError.value = ''
  try {
    const page = await listRemoteDevices(undefined, 4)
    devices.value = page.items
  } catch (error) {
    devicesError.value = problemMessage(error, '暂时无法读取设备列表。')
  } finally {
    devicesLoading.value = false
  }
}

const redeem = async () => {
  pending.value = true
  message.value = ''
  errorMessage.value = ''
  const submittedCode = code.value
  try {
    const result = await redeemMembershipCode(submittedCode)
    membership.value = result.membership
    message.value = `兑换成功，兑换码尾号 ${result.codeHint}。`
    redemptions.value = await listRedemptions()
  } catch (error) {
    errorMessage.value = problemMessage(error, '暂时无法兑换，请稍后重试。')
  } finally {
    code.value = ''
    pending.value = false
  }
}

onMounted(() => {
  void Promise.all([load(), loadDevices()])
})
</script>

<template>
  <section class="dashboard-page">
    <p class="section-kicker">会员中心</p>
    <h1>会员权益与兑换</h1>
    <p v-if="errorMessage" class="form-message form-error" role="alert">{{ errorMessage }}</p>
    <p v-if="message" class="form-message form-success" role="status">{{ message }}</p>
    <p v-if="loading" class="inline-status" role="status">正在读取会员状态…</p>

    <template v-else>
      <div class="membership-grid">
        <article class="status-card">
          <span>当前等级</span>
          <strong>{{ membership?.planName ?? 'Free' }}</strong>
          <p>{{ expiryLabel }}</p>
          <small v-if="membership">生效时间：{{ formatDate(membership.startsAt) }}</small>
        </article>
        <form class="redeem-card" @submit.prevent="redeem">
          <label for="redemption-code">会员兑换码</label>
          <div class="redeem-row">
            <input
              id="redemption-code"
              v-model.trim="code"
              autocomplete="off"
              autocapitalize="characters"
              maxlength="32"
              required
              placeholder="WZM-XXXX-XXXX-XXXX-XXXX-XXXX"
            />
            <button class="button" type="submit" :disabled="pending">
              {{ pending ? '正在兑换…' : '立即兑换' }}
            </button>
          </div>
          <small
            >兑换码仅发送到安全
            API；内测码需匹配领取邮箱、每个邮箱限用一次，永久会员不可使用。</small
          >
        </form>
      </div>

      <section class="member-devices" aria-labelledby="member-devices-title">
        <div class="member-devices-heading">
          <div>
            <p class="section-kicker">远程工作台</p>
            <h2 id="member-devices-title">我的设备</h2>
            <p>共 {{ devices.length }} 台，{{ onlineDeviceCount }} 台在线</p>
          </div>
          <RouterLink class="button button-secondary" to="/account/remote">管理全部设备</RouterLink>
        </div>

        <p v-if="devicesError" class="form-message form-error" role="alert">
          {{ devicesError }}
          <button type="button" @click="loadDevices">重新读取</button>
        </p>
        <div v-else-if="devicesLoading" class="member-devices-empty" role="status">
          正在读取设备…
        </div>
        <div v-else-if="devices.length === 0" class="member-devices-empty">
          <strong>还没有已接入设备</strong>
          <p>创建设备 Access Key 并启动 WenzWork Agent 后，即可从网页安全访问设备。</p>
          <RouterLink to="/account/remote">接入第一台设备 →</RouterLink>
        </div>
        <div v-else class="member-device-grid">
          <RouterLink
            v-for="device in devices"
            :key="device.id"
            class="member-device-card"
            :to="`/account/remote/${device.id}`"
          >
            <div class="member-device-card-top">
              <span class="member-device-icon" aria-hidden="true">
                <svg viewBox="0 0 24 24">
                  <rect x="3" y="4" width="18" height="12" rx="2" />
                  <path d="M8 20h8M12 16v4" />
                </svg>
              </span>
              <span class="member-device-presence" :class="[device.presence, device.status]">
                <i aria-hidden="true"></i>{{ presenceLabel(device) }}
              </span>
            </div>
            <h3>{{ device.deviceName }}</h3>
            <p>{{ platformLabel(device.platform) }} · Agent {{ device.agentVersion }}</p>
            <div class="member-device-card-foot">
              <span>最近在线 {{ formatDeviceDate(device.lastSeenAt) }}</span>
              <strong>进入设备 →</strong>
            </div>
          </RouterLink>
        </div>
      </section>

      <section class="redemption-history" aria-labelledby="redemption-history-title">
        <div>
          <p class="section-kicker">兑换记录</p>
          <h2 id="redemption-history-title">最近的权益变更</h2>
        </div>
        <div v-if="redemptions.length" class="history-list">
          <article v-for="item in redemptions" :key="item.id">
            <div>
              <strong>{{ item.planCode.toUpperCase() }}</strong
              ><span>尾号 {{ item.codeHint }}</span>
            </div>
            <p>兑换于 {{ formatDate(item.redeemedAt) }}</p>
            <p>
              {{
                item.resultExpiresAt ? `结果有效至 ${formatDate(item.resultExpiresAt)}` : '长期权益'
              }}
            </p>
          </article>
        </div>
        <p v-else class="inline-status">还没有兑换记录。</p>
      </section>
    </template>
  </section>
</template>

<style scoped>
.member-devices {
  margin-top: 28px;
  border: 1px solid var(--line);
  border-radius: var(--radius-card);
  padding: 26px;
  background: #fff;
  box-shadow: var(--shadow-small);
}
.member-devices-heading {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 20px;
  margin-bottom: 20px;
}
.member-devices-heading h2 {
  margin: 2px 0 5px;
}
.member-devices-heading p:not(.section-kicker) {
  margin: 0;
  color: var(--ink-soft);
}
.member-device-grid {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 14px;
}
.member-device-card {
  display: grid;
  gap: 9px;
  border: 1px solid var(--line);
  border-radius: 14px;
  padding: 18px;
  color: inherit;
  background: var(--paper-soft);
  text-decoration: none;
  transition:
    border-color 160ms ease,
    transform 160ms ease,
    box-shadow 160ms ease;
}
.member-device-card:hover {
  transform: translateY(-2px);
  border-color: var(--mint);
  background: #fff;
  box-shadow: var(--shadow-small);
}
.member-device-card-top,
.member-device-card-foot {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
}
.member-device-icon {
  display: grid;
  place-items: center;
  width: 38px;
  height: 38px;
  border-radius: 11px;
  color: var(--teal-dark);
  background: var(--brand-tint);
}
.member-device-icon svg {
  width: 21px;
  fill: none;
  stroke: currentColor;
  stroke-linecap: round;
  stroke-linejoin: round;
  stroke-width: 1.7;
}
.member-device-presence {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  border-radius: 999px;
  padding: 5px 9px;
  color: var(--ink-soft);
  background: #fff;
  font-size: 0.75rem;
  font-weight: 750;
}
.member-device-presence i {
  width: 7px;
  height: 7px;
  border-radius: 50%;
  background: var(--ink-faint);
}
.member-device-presence.online.active {
  color: var(--teal-dark);
}
.member-device-presence.online.active i {
  background: var(--teal);
  box-shadow: 0 0 0 3px var(--mint);
}
.member-device-presence.degraded i {
  background: var(--amber);
}
.member-device-card h3 {
  margin: 3px 0 0;
  font-size: 1rem;
}
.member-device-card > p,
.member-device-card-foot span {
  margin: 0;
  color: var(--ink-soft);
  font-size: 0.78rem;
}
.member-device-card-foot {
  border-top: 1px dashed var(--line);
  padding-top: 10px;
}
.member-device-card-foot strong {
  color: var(--teal-dark);
  font-size: 0.8rem;
}
.member-devices-empty {
  border: 1px dashed var(--line-strong);
  border-radius: 12px;
  padding: 24px;
  color: var(--ink-soft);
  text-align: center;
}
.member-devices-empty strong {
  display: block;
  margin-bottom: 5px;
  color: var(--ink);
}
.member-devices-empty p {
  margin: 0 0 8px;
}
.member-devices-empty a,
.form-message button {
  border: 0;
  padding: 0;
  color: var(--teal-dark);
  background: transparent;
  font-weight: 750;
  cursor: pointer;
}
@media (max-width: 760px) {
  .member-devices-heading,
  .member-device-card-foot {
    align-items: flex-start;
    flex-direction: column;
  }
  .member-device-grid {
    grid-template-columns: 1fr;
  }
}
</style>
