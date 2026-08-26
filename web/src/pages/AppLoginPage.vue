<script setup lang="ts">
import { useHead } from '@unhead/vue'
import { computed, onMounted, ref } from 'vue'
import { useRoute } from 'vue-router'

import { problemMessage } from '@/api/auth'
import {
  approveDeviceAuthorization,
  denyDeviceAuthorization,
  getDeviceAuthorizationForBrowser,
  type BrowserDeviceAuthorization,
} from '@/api/deviceAuth'
import { useAuthStore } from '@/stores/auth'

useHead({
  title: '登录 WenzWork 客户端｜WenzWork',
  meta: [
    { name: 'robots', content: 'noindex, nofollow' },
    { name: 'referrer', content: 'no-referrer' },
  ],
})

type PageState = 'loading' | 'ready' | 'approved' | 'denied' | 'error'

const route = useRoute()
const auth = useAuthStore()
const userCode = typeof route.query.code === 'string' ? route.query.code.trim() : ''
const state = ref<PageState>('loading')
const authorization = ref<BrowserDeviceAuthorization | null>(null)
const message = ref('正在检查客户端登录窗口…')
const pending = ref(false)

const returnToAppURL = computed(() => {
  const requestID = authorization.value?.requestId
  return requestID
    ? `wenzwork://auth/return?request=${encodeURIComponent(requestID)}`
    : 'wenzwork://auth/return'
})

const expiresLabel = computed(() => {
  if (!authorization.value) return ''
  return new Intl.DateTimeFormat('zh-CN', {
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
  }).format(new Date(authorization.value.expiresAt))
})

const load = async () => {
  if (!userCode) {
    state.value = 'error'
    message.value = '登录链接缺少验证码，请返回客户端重新申请登录。'
    return
  }
  try {
    authorization.value = await getDeviceAuthorizationForBrowser(userCode)
    if (authorization.value.status === 'approved') {
      state.value = 'approved'
      message.value = '这个登录窗口已经确认，请返回 WenzWork。'
      return
    }
    state.value = 'ready'
    message.value = '请核对设备和验证码，再决定是否允许登录。'
  } catch (error) {
    state.value = 'error'
    message.value = problemMessage(error, '暂时无法读取客户端登录窗口，请稍后重试。')
  }
}

const approve = async () => {
  pending.value = true
  try {
    authorization.value = await approveDeviceAuthorization(userCode)
    state.value = 'approved'
    message.value = '账户授权成功，WenzWork 将在下一次轮询时取得登录凭证。'
  } catch (error) {
    state.value = 'error'
    message.value = problemMessage(error, '暂时无法确认客户端登录，请稍后重试。')
  } finally {
    pending.value = false
  }
}

const deny = async () => {
  pending.value = true
  try {
    await denyDeviceAuthorization(userCode)
    state.value = 'denied'
    message.value = '已拒绝这次客户端登录请求。'
  } catch (error) {
    state.value = 'error'
    message.value = problemMessage(error, '暂时无法拒绝客户端登录，请稍后重试。')
  } finally {
    pending.value = false
  }
}

onMounted(load)
</script>

<template>
  <section class="auth-section">
    <div class="auth-card device-login-card">
      <p class="section-kicker">客户端登录</p>
      <h1>{{ state === 'approved' ? '登录已确认' : '登录 WenzWork' }}</h1>

      <p
        :class="[
          'form-message',
          { 'form-success': state === 'approved', 'form-error': state === 'error' },
        ]"
        :role="state === 'error' ? 'alert' : 'status'"
      >
        {{ message }}
      </p>

      <div v-if="authorization && state !== 'denied'" class="device-login-details">
        <div>
          <span>当前账户</span>
          <strong>{{ auth.user?.email }}</strong>
        </div>
        <div>
          <span>申请设备</span>
          <strong>{{ authorization.deviceName }}</strong>
        </div>
        <div>
          <span>设备验证码</span>
          <strong class="device-user-code">{{ authorization.userCode }}</strong>
        </div>
        <p>窗口将在 {{ expiresLabel }} 失效。请确认验证码与 WenzWork 客户端中显示的一致。</p>
      </div>

      <div v-if="state === 'ready'" class="device-login-actions">
        <button class="button" type="button" :disabled="pending" @click="approve">
          {{ pending ? '正在确认…' : '确认登录' }}
        </button>
        <button class="button button-secondary" type="button" :disabled="pending" @click="deny">
          拒绝
        </button>
      </div>

      <div v-else-if="state === 'approved'" class="device-login-actions">
        <a class="button" :href="returnToAppURL">返回 WenzWork</a>
        <p>如果软件没有自动回到前台，请手动打开 WenzWork；客户端会继续轮询登录结果。</p>
      </div>

      <div v-else-if="state === 'denied'" class="device-login-actions">
        <p>可以关闭此页面并返回 WenzWork。</p>
      </div>
    </div>
  </section>
</template>
