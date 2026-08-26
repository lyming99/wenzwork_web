<script setup lang="ts">
import { useHead } from '@unhead/vue'
import { onMounted, ref } from 'vue'
import { RouterLink, useRoute, useRouter } from 'vue-router'

import { problemMessage, resetPassword } from '@/api/auth'

useHead({ title: '重置密码｜WenzWork', meta: [{ name: 'robots', content: 'noindex, nofollow' }] })

const route = useRoute()
const router = useRouter()
const token = typeof route.query.token === 'string' ? route.query.token : ''
const password = ref('')
const passwordConfirm = ref('')
const pending = ref(false)
const errorMessage = ref(token ? '' : '重置链接缺少令牌，请重新申请。')
const completed = ref(false)

const submit = async () => {
  errorMessage.value = ''
  if (!token) {
    errorMessage.value = '重置链接缺少令牌，请重新申请。'
    return
  }
  if (password.value !== passwordConfirm.value) {
    errorMessage.value = '两次输入的密码不一致。'
    return
  }
  pending.value = true
  try {
    await resetPassword(token, password.value)
    completed.value = true
    password.value = ''
    passwordConfirm.value = ''
  } catch (error) {
    errorMessage.value = problemMessage(error, '暂时无法重置密码，请稍后重试。')
  } finally {
    pending.value = false
  }
}

onMounted(() => {
  if (token) void router.replace({ name: 'reset-password' })
})
</script>

<template>
  <section class="auth-section">
    <div class="auth-card">
      <p class="section-kicker">设置新密码</p>
      <h1>{{ completed ? '密码已更新' : '重置密码' }}</h1>
      <p v-if="completed" class="form-message form-success" role="status">
        旧登录会话已全部撤销，请使用新密码登录。
      </p>
      <template v-else>
        <p class="auth-lead">新密码应为 8–128 个字符，且不要与其他网站共用。</p>
        <p v-if="errorMessage" class="form-message form-error" role="alert">{{ errorMessage }}</p>
        <form class="auth-form" @submit.prevent="submit">
          <label for="reset-password">新密码</label>
          <input
            id="reset-password"
            v-model="password"
            type="password"
            autocomplete="new-password"
            required
            minlength="8"
            maxlength="128"
          />
          <label for="reset-confirm">确认新密码</label>
          <input
            id="reset-confirm"
            v-model="passwordConfirm"
            type="password"
            autocomplete="new-password"
            required
            minlength="8"
            maxlength="128"
          />
          <button class="button auth-submit" type="submit" :disabled="pending || !token">
            {{ pending ? '正在保存…' : '保存新密码' }}
          </button>
        </form>
      </template>
      <p class="auth-switch"><RouterLink to="/login">返回登录</RouterLink></p>
    </div>
  </section>
</template>
