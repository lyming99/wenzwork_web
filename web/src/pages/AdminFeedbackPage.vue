<script setup lang="ts">
import { useHead } from '@unhead/vue'
import { onMounted, ref } from 'vue'

import {
  listAdminFeedback,
  updateAdminFeedback,
  type AdminFeedbackEntry,
  type UpdateFeedbackRequest,
} from '@/api/admin'
import { problemMessage } from '@/api/auth'

useHead({
  title: '反馈管理｜WenzWork',
  meta: [{ name: 'robots', content: 'noindex, nofollow' }],
})

const entries = ref<AdminFeedbackEntry[]>([])
const loading = ref(true)
const pending = ref(false)
const errorMessage = ref('')
const message = ref('')
const query = ref('')
const statusFilter = ref<'' | AdminFeedbackEntry['status']>('')
const categoryFilter = ref<'' | AdminFeedbackEntry['category']>('')
const editingId = ref<string | null>(null)
const editingStatus = ref<UpdateFeedbackRequest['status']>('processing')
const adminReply = ref('')
const internalNote = ref('')

const statusLabel = (status: AdminFeedbackEntry['status']) =>
  ({ pending: '待处理', processing: '处理中', resolved: '已解决', closed: '已关闭' })[status]
const categoryLabel = (value: AdminFeedbackEntry['category']) =>
  ({ suggestion: '功能建议', bug: '问题报告', question: '使用咨询', other: '其他' })[value]
const formatDate = (value: string) =>
  new Intl.DateTimeFormat('zh-CN', { dateStyle: 'medium', timeStyle: 'short' }).format(
    new Date(value),
  )

const load = async () => {
  loading.value = true
  errorMessage.value = ''
  try {
    const result = await listAdminFeedback({
      q: query.value.trim() || undefined,
      status: statusFilter.value,
      category: categoryFilter.value,
      limit: 100,
    })
    entries.value = result.items
  } catch (error) {
    errorMessage.value = problemMessage(error, '暂时无法读取反馈。')
  } finally {
    loading.value = false
  }
}

const edit = (entry: AdminFeedbackEntry) => {
  editingId.value = entry.id
  editingStatus.value = entry.status
  adminReply.value = entry.adminReply
  internalNote.value = entry.internalNote
}

const cancelEdit = () => {
  editingId.value = null
  adminReply.value = ''
  internalNote.value = ''
}

const save = async (entry: AdminFeedbackEntry) => {
  pending.value = true
  errorMessage.value = ''
  message.value = ''
  try {
    const updated = await updateAdminFeedback(entry.id, {
      status: editingStatus.value,
      adminReply: adminReply.value.trim(),
      internalNote: internalNote.value.trim(),
    })
    const index = entries.value.findIndex((item) => item.id === updated.id)
    if (index >= 0) entries.value[index] = updated
    message.value = `“${updated.subject}”的处理记录已更新。`
    cancelEdit()
  } catch (error) {
    errorMessage.value = problemMessage(error, '暂时无法更新反馈。')
  } finally {
    pending.value = false
  }
}

onMounted(load)
</script>

<template>
  <section class="dashboard-page admin-wide-page">
    <p class="section-kicker">会员支持</p>
    <h1>反馈管理</h1>
    <p class="dashboard-lead">
      查看会员提交的问题与建议，维护处理状态；公开回复会回显给会员，内部备注仅管理端可见。
    </p>
    <p v-if="errorMessage" class="form-message form-error" role="alert">{{ errorMessage }}</p>
    <p v-if="message" class="form-message form-success" role="status">{{ message }}</p>

    <section class="admin-list-section feedback-admin-list" aria-labelledby="admin-feedback-title">
      <div class="section-heading-row">
        <div>
          <p class="section-kicker">反馈队列</p>
          <h2 id="admin-feedback-title">会员反馈</h2>
        </div>
        <form class="admin-filter-row" @submit.prevent="load">
          <input v-model="query" type="search" placeholder="搜索标题、正文或邮箱" />
          <select v-model="categoryFilter">
            <option value="">全部类型</option>
            <option value="suggestion">功能建议</option>
            <option value="bug">问题报告</option>
            <option value="question">使用咨询</option>
            <option value="other">其他</option>
          </select>
          <select v-model="statusFilter">
            <option value="">全部状态</option>
            <option value="pending">待处理</option>
            <option value="processing">处理中</option>
            <option value="resolved">已解决</option>
            <option value="closed">已关闭</option>
          </select>
          <button class="button button-secondary" type="submit">筛选</button>
        </form>
      </div>
      <p v-if="loading" class="inline-status" role="status">正在读取反馈…</p>
      <div v-else-if="entries.length" class="feedback-list">
        <article v-for="entry in entries" :key="entry.id" class="dashboard-card feedback-card">
          <div class="feedback-card-heading">
            <div>
              <div class="batch-title-row">
                <h3>{{ entry.subject }}</h3>
                <span class="tag">{{ statusLabel(entry.status) }}</span>
                <span class="tag tag-muted">{{ categoryLabel(entry.category) }}</span>
              </div>
              <p>{{ entry.userName }} · {{ entry.userEmail }}</p>
              <small
                >提交于 {{ formatDate(entry.createdAt) }} · 更新于
                {{ formatDate(entry.updatedAt) }}</small
              >
            </div>
            <button
              v-if="editingId !== entry.id"
              class="button button-secondary"
              type="button"
              @click="edit(entry)"
            >
              处理
            </button>
          </div>
          <p class="feedback-content">{{ entry.content }}</p>
          <p v-if="entry.contactEmail" class="feedback-contact">
            联系邮箱：{{ entry.contactEmail }}
          </p>
          <div v-if="entry.adminReply && editingId !== entry.id" class="feedback-reply">
            <strong>当前公开回复</strong>
            <p>{{ entry.adminReply }}</p>
          </div>
          <div v-if="entry.internalNote && editingId !== entry.id" class="feedback-internal-note">
            <strong>内部备注</strong>
            <p>{{ entry.internalNote }}</p>
          </div>
          <form
            v-if="editingId === entry.id"
            class="feedback-admin-editor"
            @submit.prevent="save(entry)"
          >
            <div class="field-group">
              <label :for="`feedback-status-${entry.id}`">处理状态</label>
              <select :id="`feedback-status-${entry.id}`" v-model="editingStatus">
                <option value="pending">待处理</option>
                <option value="processing">处理中</option>
                <option value="resolved">已解决</option>
                <option value="closed">已关闭</option>
              </select>
            </div>
            <div class="field-group">
              <label :for="`feedback-reply-${entry.id}`">公开回复（会员可见）</label>
              <textarea
                :id="`feedback-reply-${entry.id}`"
                v-model="adminReply"
                rows="5"
                maxlength="5000"
              ></textarea>
            </div>
            <div class="field-group">
              <label :for="`feedback-note-${entry.id}`">内部备注（仅管理端）</label>
              <textarea
                :id="`feedback-note-${entry.id}`"
                v-model="internalNote"
                rows="4"
                maxlength="5000"
              ></textarea>
            </div>
            <div class="admin-row-actions">
              <button class="button" type="submit" :disabled="pending">
                {{ pending ? '正在保存…' : '保存处理结果' }}
              </button>
              <button class="button button-secondary" type="button" @click="cancelEdit">
                取消
              </button>
            </div>
          </form>
        </article>
      </div>
      <p v-else class="inline-status">没有符合条件的反馈。</p>
    </section>
  </section>
</template>
