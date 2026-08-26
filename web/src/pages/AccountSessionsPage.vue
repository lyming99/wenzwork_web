<script setup lang="ts">
import { useHead } from '@unhead/vue'
import { onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'

import { listSessions, problemMessage, revokeSession, type SessionSummary } from '@/api/auth'
import { useAuthStore } from '@/stores/auth'

useHead({ title: '登录会话｜WenzWork', meta: [{ name: 'robots', content: 'noindex, nofollow' }] })

const auth = useAuthStore()
const router = useRouter()
const sessions = ref<SessionSummary[]>([])
const loading = ref(true)
const errorMessage = ref('')
const revoking = ref<string | null>(null)

const formatDate = (value: string) =>
  new Intl.DateTimeFormat('zh-CN', { dateStyle: 'medium', timeStyle: 'short' }).format(
    new Date(value),
  )

const load = async () => {
  loading.value = true
  errorMessage.value = ''
  try {
    sessions.value = await listSessions()
  } catch (error) {
    errorMessage.value = problemMessage(error, '暂时无法读取登录会话。')
  } finally {
    loading.value = false
  }
}

const revoke = async (session: SessionSummary) => {
  revoking.value = session.id
  errorMessage.value = ''
  try {
    await revokeSession(session.id)
    if (session.current) {
      auth.clear()
      await router.push('/login')
      return
    }
    await load()
  } catch (error) {
    errorMessage.value = problemMessage(error, '暂时无法撤销该会话。')
  } finally {
    revoking.value = null
  }
}

onMounted(load)
</script>

<template>
  <section class="dashboard-page">
    <p class="section-kicker">账户安全</p>
    <h1>登录会话</h1>
    <p class="dashboard-lead">查看仍可访问账户的设备，并撤销不认识或不再使用的会话。</p>
    <p v-if="errorMessage" class="form-message form-error" role="alert">{{ errorMessage }}</p>
    <p v-if="loading" class="inline-status" role="status">正在读取会话…</p>
    <div v-else class="session-list">
      <article v-for="session in sessions" :key="session.id" class="dashboard-card session-card">
        <div>
          <div class="session-heading">
            <h2>{{ session.userAgentSummary }}</h2>
            <span v-if="session.current" class="tag">当前会话</span>
          </div>
          <p>最近活动：{{ formatDate(session.lastSeenAt) }}</p>
          <p>
            最晚到期：{{ formatDate(session.absoluteExpiresAt) }} ·
            {{ session.rememberMe ? '保持登录' : '浏览器会话' }}
          </p>
        </div>
        <button
          class="button button-secondary"
          type="button"
          :disabled="revoking === session.id"
          @click="revoke(session)"
        >
          {{
            revoking === session.id ? '正在撤销…' : session.current ? '退出当前会话' : '撤销会话'
          }}
        </button>
      </article>
      <p v-if="sessions.length === 0" class="inline-status">没有有效会话。</p>
    </div>
  </section>
</template>
