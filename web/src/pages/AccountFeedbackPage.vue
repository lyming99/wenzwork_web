<script setup lang="ts">
import { useHead } from '@unhead/vue'
import { computed, onMounted, ref } from 'vue'

import { problemMessage } from '@/api/auth'
import {
  createMyFeedback,
  listMyFeedback,
  type CreateFeedbackRequest,
  type FeedbackEntry,
} from '@/api/feedback'
import { useAuthStore } from '@/stores/auth'

useHead({
  title: '意见反馈｜WenzWork',
  meta: [{ name: 'robots', content: 'noindex, nofollow' }],
})

const auth = useAuthStore()
const entries = ref<FeedbackEntry[]>([])
const loading = ref(true)
const pending = ref(false)
const errorMessage = ref('')
const message = ref('')
const category = ref<CreateFeedbackRequest['category']>('suggestion')
const subject = ref('')
const content = ref('')
const contactEmail = ref(auth.user?.email ?? '')

const canSubmit = computed(() => Boolean(subject.value.trim() && content.value.trim()))

const statusLabel = (status: FeedbackEntry['status']) =>
  ({ pending: '待处理', processing: '处理中', resolved: '已解决', closed: '已关闭' })[status]
const categoryLabel = (value: FeedbackEntry['category']) =>
  ({ suggestion: '功能建议', bug: '问题报告', question: '使用咨询', other: '其他' })[value]
const formatDate = (value: string) =>
  new Intl.DateTimeFormat('zh-CN', { dateStyle: 'medium', timeStyle: 'short' }).format(
    new Date(value),
  )

const load = async () => {
  loading.value = true
  errorMessage.value = ''
  try {
    entries.value = await listMyFeedback()
  } catch (error) {
    errorMessage.value = problemMessage(error, '暂时无法读取反馈记录。')
  } finally {
    loading.value = false
  }
}

const submit = async () => {
  if (!canSubmit.value) return
  pending.value = true
  errorMessage.value = ''
  message.value = ''
  try {
    const created = await createMyFeedback({
      category: category.value,
      subject: subject.value.trim(),
      content: content.value.trim(),
      ...(contactEmail.value.trim() ? { contactEmail: contactEmail.value.trim() } : {}),
    })
    entries.value = [created, ...entries.value]
    subject.value = ''
    content.value = ''
    message.value = '反馈已提交。处理进度和公开回复会显示在本页。'
  } catch (error) {
    errorMessage.value = problemMessage(error, '暂时无法提交反馈。')
  } finally {
    pending.value = false
  }
}

onMounted(load)
</script>

<template>
  <section class="dashboard-page feedback-page">
    <p class="section-kicker">会员支持</p>
    <h1>意见反馈</h1>
    <p class="dashboard-lead">
      告诉我们你的建议、遇到的问题或使用疑问。请勿提交密码、兑换码或其他敏感信息。
    </p>
    <p v-if="errorMessage" class="form-message form-error" role="alert">{{ errorMessage }}</p>
    <p v-if="message" class="form-message form-success" role="status">{{ message }}</p>

    <form class="dashboard-card feedback-form" @submit.prevent="submit">
      <p class="card-label">提交新反馈</p>
      <div class="form-grid">
        <div class="field-group">
          <label for="feedback-category">反馈类型</label>
          <select id="feedback-category" v-model="category">
            <option value="suggestion">功能建议</option>
            <option value="bug">问题报告</option>
            <option value="question">使用咨询</option>
            <option value="other">其他</option>
          </select>
        </div>
        <div class="field-group">
          <label for="feedback-email">联系邮箱（可选）</label>
          <input id="feedback-email" v-model="contactEmail" type="email" maxlength="320" />
        </div>
        <div class="field-group field-wide">
          <label for="feedback-subject">标题</label>
          <input id="feedback-subject" v-model="subject" required maxlength="160" />
        </div>
        <div class="field-group field-wide">
          <label for="feedback-content">详细说明</label>
          <textarea
            id="feedback-content"
            v-model="content"
            required
            maxlength="10000"
            rows="8"
            placeholder="问题报告建议包含操作步骤、预期结果、实际结果和软件版本。"
          ></textarea>
          <small>{{ content.length }} / 10000</small>
        </div>
      </div>
      <button class="button" type="submit" :disabled="pending || !canSubmit">
        {{ pending ? '正在提交…' : '提交反馈' }}
      </button>
    </form>

    <section class="feedback-history" aria-labelledby="feedback-history-title">
      <div class="section-heading-row">
        <div>
          <p class="section-kicker">处理进度</p>
          <h2 id="feedback-history-title">我的反馈</h2>
        </div>
        <button class="text-button" type="button" @click="load">刷新</button>
      </div>
      <p v-if="loading" class="inline-status" role="status">正在读取反馈记录…</p>
      <div v-else-if="entries.length" class="feedback-list">
        <article v-for="entry in entries" :key="entry.id" class="dashboard-card feedback-card">
          <div class="feedback-card-heading">
            <div>
              <div class="batch-title-row">
                <h3>{{ entry.subject }}</h3>
                <span class="tag">{{ statusLabel(entry.status) }}</span>
                <span class="tag tag-muted">{{ categoryLabel(entry.category) }}</span>
              </div>
              <small>提交于 {{ formatDate(entry.createdAt) }}</small>
            </div>
          </div>
          <p class="feedback-content">{{ entry.content }}</p>
          <div v-if="entry.adminReply" class="feedback-reply">
            <strong>WenzWork 回复</strong>
            <p>{{ entry.adminReply }}</p>
          </div>
        </article>
      </div>
      <p v-else class="inline-status">你还没有提交反馈。</p>
    </section>
  </section>
</template>
