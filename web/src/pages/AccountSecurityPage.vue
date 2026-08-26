<script setup lang="ts">
import { useHead } from '@unhead/vue'
import QRCode from 'qrcode'
import { computed, onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'

import {
  beginTOTPEnrollment,
  changePassword,
  confirmTOTPEnrollment,
  disableTOTP,
  getMFAStatus,
  problemMessage,
  regenerateRecoveryCodes,
  verifyLoginMFA,
  type MFAEnrollment,
  type MFAStatus,
} from '@/api/auth'
import { useAuthStore } from '@/stores/auth'

useHead({ title: '安全设置｜WenzWork', meta: [{ name: 'robots', content: 'noindex, nofollow' }] })

const router = useRouter()
const auth = useAuthStore()
const status = ref<MFAStatus | null>(null)
const statusLoading = ref(true)
const actionPending = ref(false)
const errorMessage = ref('')
const message = ref('')

const currentPassword = ref('')
const newPassword = ref('')
const confirmPassword = ref('')
const revokeOthers = ref(true)

const challengeCode = ref('')
const enrollmentPassword = ref('')
const enrollment = ref<MFAEnrollment | null>(null)
const enrollmentQRCode = ref('')
const enrollmentCode = ref('')
const recoveryCodes = ref<string[]>([])
const recoveryPassword = ref('')
const disablePassword = ref('')
const disableCode = ref('')

const mfaRequired = computed(
  () => auth.mfaEnforced && Boolean(status.value?.enrolled) && auth.assuranceLevel < 2,
)

const resetMessages = () => {
  errorMessage.value = ''
  message.value = ''
}

const loadStatus = async () => {
  statusLoading.value = true
  try {
    status.value = await getMFAStatus()
  } catch (error) {
    errorMessage.value = problemMessage(error, '暂时无法读取二次验证状态。')
  } finally {
    statusLoading.value = false
  }
}

const submitPassword = async () => {
  resetMessages()
  if (newPassword.value !== confirmPassword.value) {
    errorMessage.value = '两次输入的新密码不一致。'
    return
  }
  actionPending.value = true
  try {
    await changePassword(currentPassword.value, newPassword.value, revokeOthers.value)
    currentPassword.value = ''
    newPassword.value = ''
    confirmPassword.value = ''
    message.value = '密码已更新。'
  } catch (error) {
    errorMessage.value = problemMessage(error, '暂时无法修改密码。')
  } finally {
    actionPending.value = false
  }
}

const submitChallenge = async () => {
  resetMessages()
  actionPending.value = true
  try {
    const account = await verifyLoginMFA(challengeCode.value)
    auth.applyAccount(account)
    challengeCode.value = ''
    message.value = '二次验证完成。'
    if (auth.isAdministrator) await router.push('/admin')
    else await router.replace({ name: 'account-security' })
  } catch (error) {
    errorMessage.value = problemMessage(error, '验证码无效，请重试。')
  } finally {
    actionPending.value = false
  }
}

const beginEnrollment = async () => {
  resetMessages()
  actionPending.value = true
  try {
    enrollment.value = await beginTOTPEnrollment(enrollmentPassword.value)
    enrollmentQRCode.value = await QRCode.toDataURL(enrollment.value.otpauthUri, {
      width: 260,
      margin: 1,
      color: { dark: '#102d2d', light: '#fffefa' },
    })
    enrollmentPassword.value = ''
  } catch (error) {
    errorMessage.value = problemMessage(error, '暂时无法开始配置验证器。')
  } finally {
    actionPending.value = false
  }
}

const confirmEnrollment = async () => {
  resetMessages()
  actionPending.value = true
  try {
    const result = await confirmTOTPEnrollment(enrollmentCode.value)
    recoveryCodes.value = result.recoveryCodes
    enrollment.value = null
    enrollmentQRCode.value = ''
    enrollmentCode.value = ''
    status.value = { enrolled: true, recoveryCodesRemaining: result.recoveryCodes.length }
    await auth.bootstrap(true)
    message.value = '验证器已启用。请立即保存恢复码。'
  } catch (error) {
    errorMessage.value = problemMessage(error, '验证码无效，请重试。')
  } finally {
    actionPending.value = false
  }
}

const regenerateCodes = async () => {
  resetMessages()
  actionPending.value = true
  try {
    recoveryCodes.value = (await regenerateRecoveryCodes(recoveryPassword.value)).recoveryCodes
    recoveryPassword.value = ''
    status.value = { enrolled: true, recoveryCodesRemaining: recoveryCodes.value.length }
    message.value = '旧恢复码已失效。请保存下面的新恢复码。'
  } catch (error) {
    errorMessage.value = problemMessage(error, '暂时无法生成恢复码。')
  } finally {
    actionPending.value = false
  }
}

const disable = async () => {
  resetMessages()
  actionPending.value = true
  try {
    await disableTOTP(disablePassword.value, disableCode.value)
    disablePassword.value = ''
    disableCode.value = ''
    recoveryCodes.value = []
    status.value = { enrolled: false, recoveryCodesRemaining: 0 }
    await auth.bootstrap(true)
    message.value = '多因素验证已停用，所有会话的管理权限保证级别已降低。'
  } catch (error) {
    errorMessage.value = problemMessage(error, '暂时无法停用验证器。')
  } finally {
    actionPending.value = false
  }
}

const copyRecoveryCodes = async () => {
  try {
    await navigator.clipboard.writeText(recoveryCodes.value.join('\n'))
    message.value = '恢复码已复制。请保存到安全的离线位置。'
  } catch {
    errorMessage.value = '浏览器未允许复制，请手动选择并保存恢复码。'
  }
}

onMounted(loadStatus)
</script>

<template>
  <section class="dashboard-page">
    <p class="section-kicker">账户安全</p>
    <h1>密码与二次验证</h1>
    <p v-if="!auth.mfaEnforced" class="form-message form-success" role="status">
      当前系统未强制管理员二次验证；仍可在本页预先配置，待管理员门禁开启后使用。
    </p>

    <p v-if="errorMessage" class="form-message form-error" role="alert">{{ errorMessage }}</p>
    <p v-if="message" class="form-message form-success" role="status">{{ message }}</p>
    <p v-if="statusLoading" class="inline-status" role="status">正在读取安全设置…</p>

    <div v-else class="security-stack">
      <form
        v-if="mfaRequired && status?.enrolled"
        class="dashboard-card settings-form mfa-challenge"
        @submit.prevent="submitChallenge"
      >
        <p class="card-label">管理员二次验证</p>
        <h2>输入验证器代码</h2>
        <p>也可以使用一个尚未使用的恢复码。验证成功后会轮换当前会话。</p>
        <label for="mfa-challenge-code">验证码或恢复码</label>
        <input
          id="mfa-challenge-code"
          v-model.trim="challengeCode"
          inputmode="numeric"
          autocomplete="one-time-code"
          required
          minlength="6"
          maxlength="19"
        />
        <button class="button" type="submit" :disabled="actionPending">完成验证</button>
      </form>

      <article v-if="recoveryCodes.length" class="dashboard-card recovery-card">
        <p class="card-label">仅显示一次</p>
        <h2>保存恢复码</h2>
        <p>每个恢复码只能使用一次。离开本页后无法再次查看；重新生成会立即废止旧码。</p>
        <ul class="recovery-code-list" aria-label="MFA 恢复码">
          <li v-for="code in recoveryCodes" :key="code">
            <code>{{ code }}</code>
          </li>
        </ul>
        <button class="button button-secondary" type="button" @click="copyRecoveryCodes">
          复制全部恢复码
        </button>
      </article>

      <article
        v-if="!status?.enrolled && !enrollment"
        class="dashboard-card mfa-card mfa-setup-card"
      >
        <div>
          <p class="card-label">多因素验证（TOTP）</p>
          <h2>连接验证器</h2>
          <p>
            支持 1Password、Microsoft Authenticator、Google Authenticator 等标准 TOTP
            应用。管理员账户必须配置。
          </p>
        </div>
        <form class="inline-security-form" @submit.prevent="beginEnrollment">
          <label for="mfa-enrollment-password">当前密码</label>
          <input
            id="mfa-enrollment-password"
            v-model="enrollmentPassword"
            type="password"
            autocomplete="current-password"
            required
          />
          <button class="button" type="submit" :disabled="actionPending">开始配置</button>
        </form>
      </article>

      <article v-if="enrollment" class="dashboard-card enrollment-card">
        <div class="enrollment-grid">
          <div>
            <p class="card-label">步骤 1</p>
            <h2>扫描二维码</h2>
            <img
              v-if="enrollmentQRCode"
              class="mfa-qr-code"
              :src="enrollmentQRCode"
              alt="用于添加 WenzWork 账户的 TOTP 二维码"
            />
          </div>
          <div>
            <p class="card-label">无法扫码？</p>
            <p>在验证器中选择手动输入，并使用下面的密钥。</p>
            <code class="mfa-secret">{{ enrollment.secret }}</code>
            <form class="inline-security-form" @submit.prevent="confirmEnrollment">
              <label for="mfa-enrollment-code">步骤 2：输入 6 位验证码</label>
              <input
                id="mfa-enrollment-code"
                v-model.trim="enrollmentCode"
                inputmode="numeric"
                autocomplete="one-time-code"
                required
                pattern="[0-9]{6}"
                maxlength="6"
              />
              <button class="button" type="submit" :disabled="actionPending">确认并启用</button>
            </form>
          </div>
        </div>
      </article>

      <article v-if="status?.enrolled && !enrollment" class="dashboard-card mfa-management-card">
        <div class="mfa-status-row">
          <div>
            <p class="card-label">多因素验证（TOTP）</p>
            <h2>验证器已启用</h2>
            <p>
              剩余 {{ status.recoveryCodesRemaining }} 个未使用恢复码 · 当前会话保证级别
              {{ auth.assuranceLevel }}
            </p>
          </div>
          <span class="tag">已保护</span>
        </div>
        <form
          class="inline-security-form recovery-regenerate-form"
          @submit.prevent="regenerateCodes"
        >
          <label for="mfa-recovery-password">重新生成恢复码（需当前密码）</label>
          <input
            id="mfa-recovery-password"
            v-model="recoveryPassword"
            type="password"
            autocomplete="current-password"
            required
          />
          <button
            class="button button-secondary"
            type="submit"
            :disabled="actionPending || auth.assuranceLevel < 2"
          >
            废止旧码并生成新码
          </button>
        </form>
        <details class="danger-zone">
          <summary>停用多因素验证</summary>
          <p>管理员停用后将无法进入后台，所有现有会话会立即降级。</p>
          <form class="inline-security-form" @submit.prevent="disable">
            <label for="mfa-disable-password">当前密码</label>
            <input
              id="mfa-disable-password"
              v-model="disablePassword"
              type="password"
              autocomplete="current-password"
              required
            />
            <label for="mfa-disable-code">当前验证码或恢复码</label>
            <input
              id="mfa-disable-code"
              v-model.trim="disableCode"
              autocomplete="one-time-code"
              required
              minlength="6"
              maxlength="19"
            />
            <button class="button danger-button" type="submit" :disabled="actionPending">
              确认停用
            </button>
          </form>
        </details>
      </article>

      <form class="dashboard-card settings-form" @submit.prevent="submitPassword">
        <h2>修改密码</h2>
        <p>保存后可以同时撤销其他设备上的登录会话。</p>
        <label for="current-password">当前密码</label>
        <input
          id="current-password"
          v-model="currentPassword"
          type="password"
          autocomplete="current-password"
          required
        />
        <label for="new-password">新密码</label>
        <input
          id="new-password"
          v-model="newPassword"
          type="password"
          autocomplete="new-password"
          required
          minlength="8"
          maxlength="128"
        />
        <label for="confirm-password">确认新密码</label>
        <input
          id="confirm-password"
          v-model="confirmPassword"
          type="password"
          autocomplete="new-password"
          required
          minlength="8"
          maxlength="128"
        />
        <label class="checkbox-row">
          <input v-model="revokeOthers" type="checkbox" />
          <span>撤销其他设备的会话</span>
        </label>
        <button class="button" type="submit" :disabled="actionPending">修改密码</button>
      </form>
    </div>
  </section>
</template>
