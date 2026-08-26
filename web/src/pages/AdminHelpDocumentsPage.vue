<script setup lang="ts">
import { useHead } from '@unhead/vue'
import { computed, onMounted, ref } from 'vue'

import {
  archiveAdminHelpDocument,
  createAdminHelpDocument,
  listAdminHelpDocuments,
  publishAdminHelpDocument,
  updateAdminHelpDocument,
  type AdminHelpDocument,
  type SaveHelpDocumentRequest,
} from '@/api/admin'
import { problemMessage } from '@/api/auth'
import { renderSafeMarkdown } from '@/content/help'

useHead({
  title: '帮助文档管理｜WenzWork',
  meta: [{ name: 'robots', content: 'noindex, nofollow' }],
})

const documents = ref<AdminHelpDocument[]>([])
const loading = ref(true)
const pending = ref(false)
const errorMessage = ref('')
const message = ref('')
const query = ref('')
const statusFilter = ref<'' | 'draft' | 'published' | 'archived'>('')
const editingId = ref<string | null>(null)
const version = ref<number | undefined>()
const slugLocked = ref(false)
const slug = ref('')
const title = ref('')
const description = ref('')
const category = ref('基础使用')
const sortOrder = ref(0)
const contentMarkdown = ref('')

const canSave = computed(
  () =>
    /^[a-z0-9]+(?:-[a-z0-9]+)*$/.test(slug.value.trim()) &&
    Boolean(title.value.trim() && category.value.trim() && contentMarkdown.value.trim()),
)
const previewHTML = computed(() =>
  contentMarkdown.value.trim() ? renderSafeMarkdown(contentMarkdown.value) : '',
)

const statusLabel = (status: AdminHelpDocument['status']) =>
  ({ draft: '草稿', published: '已发布', archived: '已归档' })[status]

const formatDate = (value: string) =>
  new Intl.DateTimeFormat('zh-CN', { dateStyle: 'medium', timeStyle: 'short' }).format(
    new Date(value),
  )

const load = async () => {
  loading.value = true
  errorMessage.value = ''
  try {
    const result = await listAdminHelpDocuments({
      q: query.value.trim() || undefined,
      status: statusFilter.value,
      limit: 100,
    })
    documents.value = result.items
  } catch (error) {
    errorMessage.value = problemMessage(error, '暂时无法读取帮助文档。')
  } finally {
    loading.value = false
  }
}

const resetForm = () => {
  editingId.value = null
  version.value = undefined
  slugLocked.value = false
  slug.value = ''
  title.value = ''
  description.value = ''
  category.value = '基础使用'
  sortOrder.value = 0
  contentMarkdown.value = ''
}

const edit = (document: AdminHelpDocument) => {
  editingId.value = document.id
  version.value = document.version
  slugLocked.value = document.publishedVersion !== null
  slug.value = document.slug
  title.value = document.title
  description.value = document.description
  category.value = document.category
  sortOrder.value = document.sortOrder
  contentMarkdown.value = document.contentMarkdown
  window.scrollTo({ top: 0, behavior: 'smooth' })
}

const save = async () => {
  if (!canSave.value) return
  pending.value = true
  errorMessage.value = ''
  message.value = ''
  const request: SaveHelpDocumentRequest = {
    slug: slug.value.trim(),
    title: title.value.trim(),
    description: description.value.trim(),
    category: category.value.trim(),
    sortOrder: sortOrder.value,
    contentMarkdown: contentMarkdown.value.trim(),
    ...(version.value ? { version: version.value } : {}),
  }
  try {
    const saved = editingId.value
      ? await updateAdminHelpDocument(editingId.value, request)
      : await createAdminHelpDocument(request)
    message.value = editingId.value
      ? '帮助文档草稿已保存；公开快照尚未改变。'
      : '帮助文档草稿已创建。'
    edit(saved)
    await load()
  } catch (error) {
    errorMessage.value = problemMessage(error, '暂时无法保存帮助文档。')
  } finally {
    pending.value = false
  }
}

const publish = async (document: AdminHelpDocument) => {
  if (
    !window.confirm(`确认发布“${document.title}”？系统会生成新的安全 HTML 静态快照并替换公开版本。`)
  )
    return
  pending.value = true
  errorMessage.value = ''
  message.value = ''
  try {
    const published = await publishAdminHelpDocument(document.id)
    message.value = `“${published.title}”已静态化并发布。`
    if (editingId.value === document.id) edit(published)
    await load()
  } catch (error) {
    errorMessage.value = problemMessage(error, '暂时无法发布帮助文档。')
  } finally {
    pending.value = false
  }
}

const archive = async (document: AdminHelpDocument) => {
  if (!window.confirm(`归档“${document.title}”？公开页面会立即停止提供这篇文章。`)) return
  pending.value = true
  errorMessage.value = ''
  message.value = ''
  try {
    await archiveAdminHelpDocument(document.id)
    if (editingId.value === document.id) resetForm()
    message.value = `“${document.title}”已归档。`
    await load()
  } catch (error) {
    errorMessage.value = problemMessage(error, '暂时无法归档帮助文档。')
  } finally {
    pending.value = false
  }
}

onMounted(load)
</script>

<template>
  <section class="dashboard-page admin-wide-page">
    <p class="section-kicker">内容中心</p>
    <h1>帮助文档管理</h1>
    <p class="dashboard-lead">
      编辑只更新草稿；确认发布时才生成经过清洗的不可变 HTML 快照，避免未审阅改动直接上线。
    </p>
    <p v-if="errorMessage" class="form-message form-error" role="alert">{{ errorMessage }}</p>
    <p v-if="message" class="form-message form-success" role="status">{{ message }}</p>

    <form class="dashboard-card help-document-editor" @submit.prevent="save">
      <div class="batch-form-heading">
        <div>
          <p class="card-label">{{ editingId ? '编辑草稿' : '新文档' }}</p>
          <h2>{{ editingId ? title || '未命名文档' : '创建帮助文档' }}</h2>
        </div>
        <button v-if="editingId" class="text-button" type="button" @click="resetForm">
          新建另一篇
        </button>
      </div>
      <div class="form-grid">
        <div class="field-group">
          <label for="help-doc-title">标题</label>
          <input id="help-doc-title" v-model="title" required maxlength="160" />
        </div>
        <div class="field-group">
          <label for="help-doc-slug">公开路径 slug</label>
          <input
            id="help-doc-slug"
            v-model="slug"
            required
            :disabled="slugLocked"
            maxlength="120"
            pattern="[a-z0-9]+(?:-[a-z0-9]+)*"
            placeholder="getting-started"
          />
          <small v-if="slugLocked">已发布文章的公开路径会永久保留，不能在草稿中改名。</small>
        </div>
        <div class="field-group">
          <label for="help-doc-category">分类</label>
          <input id="help-doc-category" v-model="category" required maxlength="80" />
        </div>
        <div class="field-group">
          <label for="help-doc-order">排序值</label>
          <input
            id="help-doc-order"
            v-model.number="sortOrder"
            type="number"
            min="-100000"
            max="100000"
          />
        </div>
        <div class="field-group field-wide">
          <label for="help-doc-description">摘要</label>
          <textarea
            id="help-doc-description"
            v-model="description"
            rows="2"
            maxlength="500"
          ></textarea>
        </div>
      </div>
      <div class="help-editor-grid">
        <div class="field-group">
          <label for="help-doc-markdown">Markdown 正文</label>
          <textarea
            id="help-doc-markdown"
            v-model="contentMarkdown"
            class="markdown-editor"
            rows="20"
            required
            maxlength="100000"
            spellcheck="false"
          ></textarea>
        </div>
        <section class="help-preview" aria-labelledby="help-preview-title">
          <span class="field-label" id="help-preview-title">安全预览</span>
          <!-- 本地预览与构建期文章共用 rehype-sanitize；发布时服务端再次清洗。 -->
          <!-- eslint-disable-next-line vue/no-v-html -->
          <div v-if="previewHTML" class="prose" v-html="previewHTML"></div>
          <p v-else class="inline-status">输入 Markdown 后在这里预览。</p>
        </section>
      </div>
      <button class="button" type="submit" :disabled="pending || !canSave">
        {{ pending ? '正在保存…' : editingId ? '保存草稿' : '创建草稿' }}
      </button>
    </form>

    <section class="admin-list-section" aria-labelledby="help-document-list-title">
      <div class="section-heading-row">
        <div>
          <p class="section-kicker">文档记录</p>
          <h2 id="help-document-list-title">全部帮助文档</h2>
        </div>
        <form class="admin-filter-row" @submit.prevent="load">
          <input v-model="query" type="search" placeholder="搜索标题、路径或分类" />
          <select v-model="statusFilter">
            <option value="">全部状态</option>
            <option value="draft">草稿</option>
            <option value="published">已发布</option>
            <option value="archived">已归档</option>
          </select>
          <button class="button button-secondary" type="submit">筛选</button>
        </form>
      </div>
      <p v-if="loading" class="inline-status" role="status">正在读取帮助文档…</p>
      <div v-else-if="documents.length" class="admin-record-list">
        <article
          v-for="document in documents"
          :key="document.id"
          class="dashboard-card admin-record-row"
        >
          <div class="admin-record-main">
            <div class="batch-title-row">
              <h3>{{ document.title }}</h3>
              <span :class="['tag', { 'tag-muted': document.status !== 'published' }]">
                {{ statusLabel(document.status) }}
              </span>
              <span v-if="document.hasUnpublishedChanges" class="tag tag-warning"
                >有未发布改动</span
              >
            </div>
            <p>
              /help/{{ document.slug }} · {{ document.category }} · 排序 {{ document.sortOrder }}
            </p>
            <small>
              草稿 v{{ document.version
              }}<template v-if="document.publishedVersion">
                · 公开 v{{ document.publishedVersion }}</template
              >
              · 更新于 {{ formatDate(document.updatedAt) }}
            </small>
          </div>
          <div class="admin-row-actions">
            <button class="button button-secondary" type="button" @click="edit(document)">
              编辑
            </button>
            <button
              v-if="document.status !== 'archived' && document.hasUnpublishedChanges"
              class="button"
              type="button"
              :disabled="pending"
              @click="publish(document)"
            >
              确认发布
            </button>
            <button
              v-if="document.status !== 'archived'"
              class="text-button danger-text-button"
              type="button"
              :disabled="pending"
              @click="archive(document)"
            >
              归档
            </button>
          </div>
        </article>
      </div>
      <p v-else class="inline-status">没有符合条件的帮助文档。</p>
    </section>
  </section>
</template>
