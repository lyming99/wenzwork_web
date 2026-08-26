<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'

import { listPublishedHelpDocuments, type HelpDocumentSummary } from '@/api/help'
import { helpArticles } from '@/content/help'

const props = withDefaults(defineProps<{ activeSlug?: string }>(), { activeSlug: '' })

const query = ref('')
const managedArticles = ref<HelpDocumentSummary[]>([])
const treeOpen = ref(false)

const articles = computed(() => {
  const merged = new Map(
    helpArticles.map((article) => [
      article.slug,
      {
        slug: article.slug,
        title: article.title,
        description: article.description,
        category: article.category,
        sortOrder: article.order,
        updatedAt: article.updatedAt,
        searchText: article.searchText,
      } satisfies HelpDocumentSummary,
    ]),
  )
  for (const article of managedArticles.value) merged.set(article.slug, article)
  return [...merged.values()].sort(
    (left, right) =>
      left.sortOrder - right.sortOrder || left.title.localeCompare(right.title, 'zh-CN'),
  )
})

const filteredArticles = computed(() => {
  const normalized = query.value.trim().toLocaleLowerCase('zh-CN')
  if (!normalized) return articles.value
  return articles.value.filter((article) =>
    `${article.title} ${article.description} ${article.category} ${article.searchText}`
      .toLocaleLowerCase('zh-CN')
      .includes(normalized),
  )
})

const groups = computed(() => {
  const grouped = new Map<string, HelpDocumentSummary[]>()
  for (const article of filteredArticles.value) {
    const items = grouped.get(article.category) ?? []
    items.push(article)
    grouped.set(article.category, items)
  }
  return [...grouped.entries()].map(([category, items]) => ({ category, items }))
})

onMounted(async () => {
  try {
    managedArticles.value = await listPublishedHelpDocuments()
  } catch {
    // 仓库内 Markdown 是构建期离线目录，帮助 API 不可用时仍保持导航可用。
  }
})
</script>

<template>
  <aside class="help-document-tree" aria-label="帮助文档目录">
    <div class="help-tree-heading">
      <RouterLink to="/help">WenzWork 文档</RouterLink>
      <div>
        <span>{{ articles.length }} 篇</span>
        <button
          class="help-tree-toggle"
          type="button"
          :aria-expanded="treeOpen"
          aria-controls="help-tree-panel"
          @click="treeOpen = !treeOpen"
        >
          {{ treeOpen ? '收起目录' : '展开目录' }}
        </button>
      </div>
    </div>
    <div id="help-tree-panel" :class="['help-tree-panel', { open: treeOpen }]">
      <form class="help-tree-search" role="search" @submit.prevent>
        <label class="sr-only" for="help-tree-query">搜索帮助文档</label>
        <span aria-hidden="true">⌕</span>
        <input
          id="help-tree-query"
          v-model="query"
          type="search"
          autocomplete="off"
          placeholder="搜索文档"
        />
      </form>
      <nav class="help-tree-groups" aria-label="分组文档树">
        <section v-for="group in groups" :key="group.category" class="help-tree-group">
          <h2>{{ group.category }}</h2>
          <ul>
            <li v-for="article in group.items" :key="article.slug">
              <RouterLink
                class="help-tree-link"
                :class="{ active: props.activeSlug === article.slug }"
                :aria-current="props.activeSlug === article.slug ? 'page' : undefined"
                :to="`/help/${article.slug}`"
                @click="treeOpen = false"
              >
                {{ article.title }}
              </RouterLink>
            </li>
          </ul>
        </section>
        <p v-if="groups.length === 0" class="help-tree-empty" role="status">
          没有匹配的文档，换个关键词试试。
        </p>
      </nav>
    </div>
  </aside>
</template>
