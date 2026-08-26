<script setup lang="ts">
import { useHead } from '@unhead/vue'
import { ref, watch } from 'vue'

import { problemMessage, updateProfile } from '@/api/auth'
import { useAuthStore } from '@/stores/auth'

useHead({ title: '账户资料｜WenzWork', meta: [{ name: 'robots', content: 'noindex, nofollow' }] })

const auth = useAuthStore()
const displayName = ref(auth.user?.displayName ?? '')
const pending = ref(false)
const message = ref('')
const errorMessage = ref('')

watch(
  () => auth.user?.displayName,
  (value) => {
    displayName.value = value ?? ''
  },
)

const submit = async () => {
  pending.value = true
  message.value = ''
  errorMessage.value = ''
  try {
    await updateProfile(displayName.value)
    await auth.bootstrap(true)
    message.value = '账户资料已更新。'
  } catch (error) {
    errorMessage.value = problemMessage(error, '暂时无法更新资料。')
  } finally {
    pending.value = false
  }
}
</script>

<template>
  <section class="dashboard-page">
    <p class="section-kicker">账户中心</p>
    <h1>账户资料</h1>
    <div class="dashboard-grid">
      <article class="dashboard-card account-summary-card">
        <span class="card-label">登录邮箱</span>
        <strong>{{ auth.user?.email }}</strong>
        <p>{{ auth.user?.emailVerifiedAt ? '邮箱已验证' : '邮箱尚未验证' }}</p>
        <dl class="account-details">
          <div>
            <dt>账户状态</dt>
            <dd>{{ auth.user?.status === 'active' ? '正常' : auth.user?.status }}</dd>
          </div>
          <div>
            <dt>角色</dt>
            <dd>{{ auth.user?.roles.join('、') }}</dd>
          </div>
        </dl>
      </article>
      <form class="dashboard-card settings-form" @submit.prevent="submit">
        <h2>公开显示名称</h2>
        <p>用于账户中心和后续协作功能，不会改变登录邮箱。</p>
        <p v-if="errorMessage" class="form-message form-error" role="alert">{{ errorMessage }}</p>
        <p v-if="message" class="form-message form-success" role="status">{{ message }}</p>
        <label for="account-display-name">显示名称</label>
        <input
          id="account-display-name"
          v-model.trim="displayName"
          autocomplete="name"
          required
          maxlength="120"
        />
        <button class="button" type="submit" :disabled="pending">
          {{ pending ? '正在保存…' : '保存资料' }}
        </button>
      </form>
    </div>
  </section>
</template>
