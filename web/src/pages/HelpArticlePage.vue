<script setup lang="ts">
import axios from 'axios'
import { useHead } from '@unhead/vue'
import { computed, onMounted, ref, watch } from 'vue'
import { useRoute } from 'vue-router'

import { getPublishedHelpDocument, type PublicHelpDocument } from '@/api/help'
import HelpDocumentTree from '@/components/HelpDocumentTree.vue'
import { absoluteUrl, socialPreviewUrl } from '@/composables/usePageHead'
import { getHelpArticle } from '@/content/help'

const route = useRoute()
const slug = computed(() => String(route.params.slug ?? ''))
const managedArticle = ref<PublicHelpDocument | null>(null)
const remoteState = ref<'idle' | 'loading' | 'ready' | 'missing' | 'error'>('idle')
const article = computed(() => managedArticle.value ?? getHelpArticle(slug.value))
const formatDate = (value: string) =>
  new Intl.DateTimeFormat('zh-CN', { dateStyle: 'long' }).format(new Date(value))

const loadManagedArticle = async () => {
  managedArticle.value = null
  remoteState.value = 'loading'
  try {
    managedArticle.value = await getPublishedHelpDocument(slug.value)
    remoteState.value = 'ready'
  } catch (error) {
    if (axios.isAxiosError(error) && error.response?.status === 404) {
      remoteState.value = 'missing'
    } else {
      remoteState.value = 'error'
    }
  }
}

onMounted(loadManagedArticle)
watch(slug, loadManagedArticle)

useHead({
  title: () =>
    article.value ? `${article.value.title}｜使用帮助｜WenzWork` : '文章未找到｜WenzWork',
  link: [
    {
      rel: 'canonical',
      href: () => absoluteUrl(article.value ? `/help/${article.value.slug}` : route.path),
    },
  ],
  meta: [
    {
      name: 'description',
      content: () => article.value?.description ?? '请求的 WenzWork 帮助文章不存在。',
    },
    { property: 'og:type', content: 'article' },
    {
      property: 'og:title',
      content: () => (article.value ? `${article.value.title}｜WenzWork` : '文章未找到｜WenzWork'),
    },
    {
      property: 'og:description',
      content: () => article.value?.description ?? '请求的 WenzWork 帮助文章不存在。',
    },
    { property: 'og:url', content: () => absoluteUrl(route.path) },
    { property: 'og:image', content: socialPreviewUrl },
    { property: 'og:image:width', content: '1736' },
    { property: 'og:image:height', content: '906' },
    {
      property: 'og:image:alt',
      content: 'WenzWork 本机与远程 AI 项目工作台',
    },
    { name: 'twitter:card', content: 'summary_large_image' },
    { name: 'twitter:image', content: socialPreviewUrl },
    { name: 'robots', content: () => (article.value ? 'index, follow' : 'noindex') },
  ],
})
</script>

<template>
  <section class="help-docs-page">
    <div class="shell help-docs-layout">
      <HelpDocumentTree :active-slug="slug" />

      <article v-if="article" class="help-doc-content help-article-content">
        <nav class="help-breadcrumbs" aria-label="面包屑">
          <RouterLink to="/help">使用帮助</RouterLink><span aria-hidden="true">/</span
          ><span>{{ article.category }}</span>
        </nav>
        <header class="help-document-header">
          <p class="section-kicker">{{ article.category }}</p>
          <h1>{{ article.title }}</h1>
          <p>{{ article.description }}</p>
          <time :datetime="article.updatedAt">更新于 {{ formatDate(article.updatedAt) }}</time>
        </header>

        <!-- Markdown 在构建或发布管线中忽略原始 HTML，并经 rehype-sanitize 白名单清洗。 -->
        <!-- eslint-disable-next-line vue/no-v-html -->
        <div class="prose" v-html="article.html"></div>

        <nav class="article-next" aria-label="文章后续导航">
          <div>
            <strong>还需要帮助？</strong>
            <p>微信 lyming555 · 邮箱 44185539@qq.com</p>
          </div>
          <RouterLink class="button button-secondary" to="/help">返回文档首页</RouterLink>
        </nav>
      </article>

      <article
        v-else-if="remoteState === 'loading'"
        class="help-doc-content help-state"
        aria-live="polite"
      >
        <p class="section-kicker">使用帮助</p>
        <h1>正在读取文章…</h1>
      </article>

      <article v-else-if="remoteState === 'error'" class="help-doc-content help-state">
        <p class="section-kicker">使用帮助</p>
        <h1>文章暂时无法读取。</h1>
        <p>帮助服务暂时不可用，请稍后重试。</p>
        <button class="button" type="button" @click="loadManagedArticle">重新加载</button>
      </article>

      <article v-else class="help-doc-content help-state">
        <p class="section-kicker">404 · 使用帮助</p>
        <h1>这篇帮助文章不存在。</h1>
        <p>链接可能已经变化，或者文章尚未发布。请从左侧文档树选择其他内容。</p>
        <RouterLink class="button" to="/help">返回文档首页</RouterLink>
      </article>
    </div>
  </section>
</template>
