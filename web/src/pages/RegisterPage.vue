<script setup lang="ts">
import { useHead } from '@unhead/vue'
import { ref } from 'vue'
import { RouterLink } from 'vue-router'

import { problemMessage, registerAccount } from '@/api/auth'

useHead({ title: '注册账户｜WenzWork', meta: [{ name: 'robots', content: 'noindex, nofollow' }] })

const displayName = ref('')
const email = ref('')
const password = ref('')
const passwordConfirm = ref('')
const pending = ref(false)
const errorMessage = ref('')
const successMessage = ref('')

const submit = async () => {
  errorMessage.value = ''
  successMessage.value = ''
  if (password.value !== passwordConfirm.value) {
    errorMessage.value = '两次输入的密码不一致。'
    return
  }
  pending.value = true
  try {
    const result = await registerAccount({
      displayName: displayName.value,
      email: email.value,
      password: password.value,
    })
    successMessage.value = result.message
    password.value = ''
    passwordConfirm.value = ''
  } catch (error) {
    errorMessage.value = problemMessage(error, '暂时无法提交注册，请稍后重试。')
  } finally {
    pending.value = false
  }
}
</script>

<template>
  <section class="auth-section">
    <div class="auth-card">
      <p class="section-kicker">创建账户</p>
      <h1>开始使用 WenzWork</h1>
      <p class="auth-lead">注册后请在 24 小时内完成邮箱验证。</p>

      <p v-if="errorMessage" class="form-message form-error" role="alert">{{ errorMessage }}</p>
      <p v-if="successMessage" class="form-message form-success" role="status">
        {{ successMessage }}
      </p>
      <form v-if="!successMessage" class="auth-form" @submit.prevent="submit">
        <label for="register-name">显示名称</label>
        <input
          id="register-name"
          v-model.trim="displayName"
          autocomplete="name"
          required
          maxlength="120"
        />
        <label for="register-email">邮箱</label>
        <input
          id="register-email"
          v-model.trim="email"
          type="email"
          autocomplete="email"
          required
          maxlength="320"
        />
        <label for="register-password">密码</label>
        <input
          id="register-password"
          v-model="password"
          type="password"
          autocomplete="new-password"
          required
          minlength="8"
          maxlength="128"
          aria-describedby="password-hint"
        />
        <p id="password-hint" class="field-hint">使用 8–128 个字符；建议使用独立的长密码。</p>
        <label for="register-confirm">确认密码</label>
        <input
          id="register-confirm"
          v-model="passwordConfirm"
          type="password"
          autocomplete="new-password"
          required
          minlength="8"
          maxlength="128"
        />
        <button class="button auth-submit" type="submit" :disabled="pending">
          {{ pending ? '正在创建…' : '创建账户' }}
        </button>
      </form>
      <p class="legal-note">提交即表示你已阅读 <RouterLink to="/privacy">隐私政策</RouterLink>。</p>
      <p class="auth-switch">已有账户？<RouterLink to="/login">直接登录</RouterLink></p>
    </div>
  </section>
</template>
