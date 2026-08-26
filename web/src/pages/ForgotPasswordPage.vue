<script setup lang="ts">
import { useHead } from '@unhead/vue'
import { ref } from 'vue'
import { RouterLink } from 'vue-router'

import { problemMessage, requestPasswordReset } from '@/api/auth'

useHead({ title: '找回密码｜WenzWork', meta: [{ name: 'robots', content: 'noindex, nofollow' }] })

const email = ref('')
const pending = ref(false)
const message = ref('')

const submit = async () => {
  pending.value = true
  message.value = ''
  try {
    message.value = (await requestPasswordReset(email.value)).message
  } catch (error) {
    message.value = problemMessage(error, '暂时无法提交请求，请稍后重试。')
  } finally {
    pending.value = false
  }
}
</script>

<template>
  <section class="auth-section">
    <div class="auth-card">
      <p class="section-kicker">密码恢复</p>
      <h1>找回你的账户</h1>
      <p class="auth-lead">输入注册邮箱；可重置时，我们会发送一小时内有效的链接。</p>
      <p v-if="message" class="form-message" role="status">{{ message }}</p>
      <form class="auth-form" @submit.prevent="submit">
        <label for="forgot-email">邮箱</label>
        <input
          id="forgot-email"
          v-model.trim="email"
          type="email"
          autocomplete="email"
          required
          maxlength="320"
        />
        <button class="button auth-submit" type="submit" :disabled="pending">
          {{ pending ? '正在发送…' : '发送重置邮件' }}
        </button>
      </form>
      <p class="auth-switch"><RouterLink to="/login">返回登录</RouterLink></p>
    </div>
  </section>
</template>
