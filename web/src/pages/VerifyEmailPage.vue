<script setup lang="ts">
import { useHead } from '@unhead/vue'
import { onMounted, ref } from 'vue'
import { RouterLink, useRoute, useRouter } from 'vue-router'

import { problemMessage, verifyEmail } from '@/api/auth'

useHead({ title: '验证邮箱｜WenzWork', meta: [{ name: 'robots', content: 'noindex, nofollow' }] })

const route = useRoute()
const router = useRouter()
const token = typeof route.query.token === 'string' ? route.query.token : ''
const state = ref<'loading' | 'success' | 'error'>('loading')
const message = ref('正在验证邮箱…')

onMounted(async () => {
  if (token) await router.replace({ name: 'verify-email' })
  if (!token) {
    state.value = 'error'
    message.value = '验证链接缺少令牌，请重新发送验证邮件。'
    return
  }
  try {
    await verifyEmail(token)
    state.value = 'success'
    message.value = '邮箱验证成功，现在可以登录。'
  } catch (error) {
    state.value = 'error'
    message.value = problemMessage(error, '暂时无法验证邮箱，请稍后重试。')
  }
})
</script>

<template>
  <section class="auth-section">
    <div class="auth-card auth-result-card">
      <p class="section-kicker">邮箱验证</p>
      <h1>
        {{ state === 'success' ? '验证完成' : state === 'error' ? '验证未完成' : '正在验证' }}
      </h1>
      <p
        :class="[
          'form-message',
          { 'form-success': state === 'success', 'form-error': state === 'error' },
        ]"
        :role="state === 'error' ? 'alert' : 'status'"
      >
        {{ message }}
      </p>
      <RouterLink v-if="state === 'success'" class="button" to="/login">前往登录</RouterLink>
      <RouterLink v-else-if="state === 'error'" class="button button-secondary" to="/login"
        >返回登录</RouterLink
      >
    </div>
  </section>
</template>
