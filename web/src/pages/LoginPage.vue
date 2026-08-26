<script setup lang="ts">
import { useHead } from '@unhead/vue'
import { computed, ref } from 'vue'
import { RouterLink, useRoute, useRouter } from 'vue-router'

import { problemDetails, problemMessage, resendVerification } from '@/api/auth'
import { useAuthStore } from '@/stores/auth'

useHead({ title: '账户登录｜WenzWork', meta: [{ name: 'robots', content: 'noindex, nofollow' }] })

const route = useRoute()
const router = useRouter()
const auth = useAuthStore()
const email = ref('')
const password = ref('')
const rememberMe = ref(false)
const pending = ref(false)
const errorMessage = ref(route.query.unavailable === '1' ? '认证服务暂时不可用，请稍后重试。' : '')
const needsVerification = ref(false)
const resendMessage = ref('')

const destination = computed(() => {
  const candidate = typeof route.query.redirect === 'string' ? route.query.redirect : '/account'
  return candidate.startsWith('/') && !candidate.startsWith('//') ? candidate : '/account'
})

const submit = async () => {
  pending.value = true
  errorMessage.value = ''
  needsVerification.value = false
  resendMessage.value = ''
  try {
    const result = await auth.login({
      email: email.value,
      password: password.value,
      rememberMe: rememberMe.value,
    })
    if (result.mfaRequired && result.mfaEnforced) {
      await router.push({ name: 'account-security', query: { mfa: 'required' } })
      return
    }
    await router.push(destination.value)
  } catch (error) {
    needsVerification.value = problemDetails(error)?.code === 'email_not_verified'
    errorMessage.value = problemMessage(error, '暂时无法登录，请稍后重试。')
  } finally {
    pending.value = false
  }
}

const resend = async () => {
  resendMessage.value = ''
  try {
    const result = await resendVerification(email.value)
    resendMessage.value = result.message
  } catch (error) {
    resendMessage.value = problemMessage(error, '暂时无法发送验证邮件。')
  }
}
</script>

<template>
  <section class="auth-section">
    <div class="auth-card">
      <p class="section-kicker">账户登录</p>
      <h1>欢迎回来</h1>
      <p class="auth-lead">登录后管理会员权益、资料和设备会话。</p>

      <p v-if="errorMessage" class="form-message form-error" role="alert">{{ errorMessage }}</p>
      <form class="auth-form" @submit.prevent="submit">
        <label for="login-email">邮箱</label>
        <input
          id="login-email"
          v-model.trim="email"
          type="email"
          autocomplete="email"
          required
          maxlength="320"
        />

        <div class="label-row">
          <label for="login-password">密码</label>
          <RouterLink to="/forgot-password">忘记密码？</RouterLink>
        </div>
        <input
          id="login-password"
          v-model="password"
          type="password"
          autocomplete="current-password"
          required
        />

        <label class="checkbox-row">
          <input v-model="rememberMe" type="checkbox" />
          <span>在这台设备保持登录（最长 30 天）</span>
        </label>

        <button class="button auth-submit" type="submit" :disabled="pending">
          {{ pending ? '正在登录…' : '登录' }}
        </button>
      </form>

      <div v-if="needsVerification" class="auth-secondary-action">
        <button class="text-button" type="button" @click="resend">重新发送验证邮件</button>
        <p v-if="resendMessage" role="status">{{ resendMessage }}</p>
      </div>
      <p class="auth-switch">还没有账户？<RouterLink to="/register">免费注册</RouterLink></p>
    </div>
  </section>
</template>
