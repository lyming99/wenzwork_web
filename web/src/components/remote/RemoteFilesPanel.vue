<script setup lang="ts">
import { computed, inject, onMounted, ref, useId } from 'vue'

import { remoteRPCKey, type RemoteFileEntry, type RemoteRPCPage } from '@/remote/rpcTypes'

const rpc = inject(remoteRPCKey)
if (!rpc) throw new Error('Remote RPC provider is required')
const props = defineProps<{ writable: boolean; projectId: string }>()
const headingId = `remote-files-heading-${useId()}`
const projectContext = computed(() => ({ projectId: props.projectId }))

const currentPath = ref('')
const entries = ref<RemoteFileEntry[]>([])
const nextCursor = ref<string | null>(null)
const loading = ref(true)
const errorMessage = ref('')
const selected = ref<RemoteFileEntry | null>(null)
const details = ref<Record<string, unknown> | null>(null)
const preview = ref('')
const previewRevision = ref(0)
const previewTruncated = ref(false)
const previewLoading = ref(false)
const saving = ref(false)
const transferProgress = ref('')
const uploadInput = ref<HTMLInputElement | null>(null)
const searchQuery = ref('')
const showingSearch = ref(false)
const searchTruncated = ref(false)
const viewMode = ref<'list' | 'grid'>('list')
const sortKey = ref<'name' | 'modified' | 'size' | 'type'>('name')
const sortAscending = ref(true)
const backStack = ref<string[]>([])
const forwardStack = ref<string[]>([])
const sortOptions = [
  ['name', '名称'],
  ['modified', '修改时间'],
  ['size', '大小'],
  ['type', '类型'],
] as const

const breadcrumbs = computed(() => {
  const parts = currentPath.value.split('/').filter(Boolean)
  return [
    { label: '项目根目录', path: '' },
    ...parts.map((part, index) => ({ label: part, path: parts.slice(0, index + 1).join('/') })),
  ]
})

const selectedEncoding = computed(() => {
  const text = details.value?.text
  if (!text || typeof text !== 'object') return 'binary'
  const encoding = (text as { encoding?: unknown }).encoding
  return typeof encoding === 'string' && encoding ? encoding : 'binary'
})

const sortedEntries = computed(() => {
  const direction = sortAscending.value ? 1 : -1
  return [...entries.value].sort((left, right) => {
    if (left.kind !== right.kind) return left.kind === 'directory' ? -1 : 1
    let comparison = 0
    if (sortKey.value === 'modified') {
      comparison = new Date(left.modifiedAt).getTime() - new Date(right.modifiedAt).getTime()
    } else if (sortKey.value === 'size') {
      comparison = left.size - right.size
    } else if (sortKey.value === 'type') {
      comparison = `${left.category}:${left.extension}`.localeCompare(
        `${right.category}:${right.extension}`,
        'zh-CN',
      )
    } else {
      comparison = left.name.localeCompare(right.name, 'zh-CN', {
        numeric: true,
        sensitivity: 'base',
      })
    }
    return comparison * direction
  })
})

const formatBytes = (bytes: number) => {
  if (bytes < 1024) return `${bytes} B`
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`
  return `${(bytes / 1024 / 1024).toFixed(1)} MB`
}

const load = async (append = false) => {
  if (searchQuery.value.trim()) {
    await search(append)
    return
  }
  loading.value = true
  errorMessage.value = ''
  try {
    await rpc.connect()
    let page = await rpc.call<RemoteRPCPage<RemoteFileEntry>>(
      'file.list',
      {
        path: currentPath.value,
        cursor: append ? nextCursor.value : undefined,
        limit: 100,
      },
      projectContext.value,
    )
    const cursorWasReset = append && Boolean(page.resetRequired)
    if (cursorWasReset) {
      page = await rpc.call<RemoteRPCPage<RemoteFileEntry>>(
        'file.list',
        {
          path: currentPath.value,
          limit: 100,
        },
        projectContext.value,
      )
    }
    entries.value = append && !cursorWasReset ? [...entries.value, ...page.items] : page.items
    nextCursor.value = page.nextCursor
    showingSearch.value = false
    searchTruncated.value = false
  } catch (error) {
    errorMessage.value = error instanceof Error ? error.message : '无法读取设备目录。'
  } finally {
    loading.value = false
  }
}

const search = async (append = false) => {
  const query = searchQuery.value.trim()
  if (!query) {
    showingSearch.value = false
    searchTruncated.value = false
    nextCursor.value = null
    await load()
    return
  }
  loading.value = true
  errorMessage.value = ''
  try {
    let page = await rpc.call<
      RemoteRPCPage<{ entry: RemoteFileEntry; parentPath: string; matchKind: string }>
    >(
      'file.search',
      { path: '', query, cursor: append ? nextCursor.value : undefined, limit: 100 },
      projectContext.value,
    )
    if (append && page.resetRequired) {
      page = await rpc.call('file.search', { path: '', query, limit: 100 }, projectContext.value)
      append = false
    }
    const incoming = page.items.map((item) => item.entry)
    entries.value = append ? [...entries.value, ...incoming] : incoming
    nextCursor.value = page.nextCursor
    showingSearch.value = true
    searchTruncated.value = Boolean(
      (page as RemoteRPCPage<unknown> & { truncated?: boolean }).truncated,
    )
    selected.value = null
    details.value = null
  } catch (error) {
    errorMessage.value = error instanceof Error ? error.message : '无法搜索设备文件。'
  } finally {
    loading.value = false
  }
}

const clearSearch = async () => {
  searchQuery.value = ''
  showingSearch.value = false
  searchTruncated.value = false
  nextCursor.value = null
  await load()
}

const navigate = async (path: string, recordHistory = true) => {
  if (recordHistory && path !== currentPath.value) {
    backStack.value.push(currentPath.value)
    forwardStack.value = []
  }
  currentPath.value = path
  selected.value = null
  details.value = null
  preview.value = ''
  previewTruncated.value = false
  searchQuery.value = ''
  showingSearch.value = false
  searchTruncated.value = false
  await load()
}

const goBack = async () => {
  const target = backStack.value.pop()
  if (target === undefined) return
  forwardStack.value.push(currentPath.value)
  await navigate(target, false)
}

const goForward = async () => {
  const target = forwardStack.value.pop()
  if (target === undefined) return
  backStack.value.push(currentPath.value)
  await navigate(target, false)
}

const changeSort = (key: typeof sortKey.value) => {
  if (sortKey.value === key) sortAscending.value = !sortAscending.value
  else {
    sortKey.value = key
    sortAscending.value = true
  }
}

const handleKeyboard = (event: KeyboardEvent) => {
  if (event.target instanceof HTMLInputElement || event.target instanceof HTMLTextAreaElement)
    return
  const entry = selected.value
  if (!entry) return
  if (event.key === 'F2' && entry.writable && props.writable) {
    event.preventDefault()
    void rename(entry)
  } else if (event.key === 'Delete' && entry.writable && props.writable) {
    event.preventDefault()
    void remove(entry)
  } else if (event.key === 'Enter') {
    event.preventDefault()
    void open(entry)
  }
}

const decodeRemoteText = (bytes: Uint8Array, category: RemoteFileEntry['category']) => {
  if (bytes.length >= 2 && bytes[0] === 0xff && bytes[1] === 0xfe)
    return new TextDecoder('utf-16le', { fatal: false }).decode(bytes.subarray(2))
  if (bytes.length >= 2 && bytes[0] === 0xfe && bytes[1] === 0xff)
    return new TextDecoder('utf-16be', { fatal: false }).decode(bytes.subarray(2))
  const offset =
    bytes.length >= 3 && bytes[0] === 0xef && bytes[1] === 0xbb && bytes[2] === 0xbf ? 3 : 0
  return new TextDecoder('utf-8', { fatal: category !== 'text' }).decode(bytes.subarray(offset))
}

const open = async (entry: RemoteFileEntry) => {
  if (entry.kind === 'directory') {
    await navigate(entry.relativePath)
    return
  }
  selected.value = entry
  preview.value = ''
  previewTruncated.value = false
  previewLoading.value = true
  try {
    const result = await rpc.call<{
      entry: RemoteFileEntry
      text?: { readable: boolean; encoding: string; maximumBytes: number }
    }>('file.details', { path: entry.relativePath }, projectContext.value)
    if (result.entry.size > 512 * 1024 || result.text?.readable !== true)
      throw new Error('该文件不是可编辑的 512 KiB 以内文本。')
    const blob = await rpc.downloadFile(
      entry.relativePath,
      undefined,
      projectContext.value,
      result.entry.revision,
    )
    preview.value = decodeRemoteText(
      new Uint8Array(await blob.arrayBuffer()),
      result.entry.category,
    )
    previewRevision.value = result.entry.revision
    previewTruncated.value = false
    details.value = result as unknown as Record<string, unknown>
  } catch (error) {
    errorMessage.value = error instanceof Error ? error.message : '该文件无法作为文本预览。'
  } finally {
    previewLoading.value = false
  }
}

const save = async () => {
  if (!selected.value || saving.value || !props.writable) return
  if (previewTruncated.value) {
    errorMessage.value =
      '当前仅加载了文件的一部分。为避免覆盖未显示的内容，请下载完整文件后再编辑。'
    return
  }
  saving.value = true
  try {
    const bytes = new TextEncoder().encode(preview.value)
    if (bytes.length > 512 * 1024) throw new Error('文本文件不能超过 512 KiB。')
    const result = await rpc.uploadFile(
      selected.value.relativePath,
      new File([bytes.slice().buffer], selected.value.name, { type: 'text/plain;charset=utf-8' }),
      undefined,
      projectContext.value,
      previewRevision.value,
    )
    previewRevision.value = result.revision
    await load()
  } catch (error) {
    errorMessage.value =
      error instanceof Error ? error.message : '无法保存文件；它可能已在设备上变化。'
  } finally {
    saving.value = false
  }
}

const createTextFile = async () => {
  if (!props.writable) return
  const name = window.prompt('新文本文件名称')?.trim()
  if (!name) return
  try {
    await rpc.call(
      'file.create-text',
      { parentPath: currentPath.value, name },
      projectContext.value,
    )
    await load()
  } catch (error) {
    errorMessage.value = error instanceof Error ? error.message : '无法创建文本文件。'
  }
}

const createDirectory = async () => {
  if (!props.writable) return
  const name = window.prompt('新文件夹名称')?.trim()
  if (!name) return
  try {
    await rpc.call('file.mkdir', { parentPath: currentPath.value, name }, projectContext.value)
    await load()
  } catch (error) {
    errorMessage.value = error instanceof Error ? error.message : '无法创建文件夹。'
  }
}

const rename = async (entry: RemoteFileEntry) => {
  if (!props.writable) return
  const name = window.prompt('新名称', entry.name)?.trim()
  if (!name || name === entry.name) return
  try {
    await rpc.call(
      'file.rename',
      {
        path: entry.relativePath,
        expectedRevision: entry.revision,
        name,
      },
      projectContext.value,
    )
    await load()
  } catch (error) {
    errorMessage.value = error instanceof Error ? error.message : '无法重命名。'
  }
}

const move = async (entry: RemoteFileEntry) => {
  if (!props.writable) return
  const target = window.prompt('目标文件夹的项目相对路径；输入 / 表示项目根目录')?.trim()
  if (target === undefined || target === null || target === '') return
  try {
    await rpc.call(
      'file.move',
      {
        path: entry.relativePath,
        targetDirectoryPath: target === '/' ? '' : target,
        expectedRevision: entry.revision,
      },
      projectContext.value,
    )
    if (selected.value?.id === entry.id) selected.value = null
    details.value = null
    await load()
  } catch (error) {
    errorMessage.value = error instanceof Error ? error.message : '无法移动文件。'
  }
}

const remove = async (entry: RemoteFileEntry) => {
  if (!props.writable) return
  let confirmationToken: string | undefined
  let confirmationDetail = ''
  if (entry.kind === 'directory') {
    try {
      const prepared = await rpc.call<{
        requiresConfirmation: boolean
        confirmationToken?: string
        itemCount?: number
        totalBytes?: number
      }>(
        'file.delete.prepare',
        { path: entry.relativePath, expectedRevision: entry.revision },
        projectContext.value,
      )
      if (prepared.requiresConfirmation) {
        confirmationToken = prepared.confirmationToken
        confirmationDetail = `，包括 ${(prepared.itemCount ?? 1) - 1} 个子项（${formatBytes(prepared.totalBytes ?? 0)}）`
      }
    } catch (error) {
      errorMessage.value = error instanceof Error ? error.message : '设备策略不允许递归删除。'
      return
    }
  }
  if (!window.confirm(`删除“${entry.name}”${confirmationDetail}？此操作不可恢复。`)) return
  try {
    await rpc.call(
      'file.delete',
      {
        path: entry.relativePath,
        expectedRevision: entry.revision,
        ...(confirmationToken ? { confirmationToken } : {}),
      },
      projectContext.value,
    )
    if (selected.value?.id === entry.id) selected.value = null
    details.value = null
    await load()
  } catch (error) {
    errorMessage.value = error instanceof Error ? error.message : '无法删除。'
  }
}

const download = async (entry: RemoteFileEntry) => {
  transferProgress.value = '正在建立加密文件通道…'
  try {
    const blob = await rpc.downloadFile(
      entry.relativePath,
      (received, total) => {
        transferProgress.value = `下载 ${formatBytes(received)} / ${formatBytes(total)}`
      },
      projectContext.value,
    )
    const url = URL.createObjectURL(blob)
    const anchor = document.createElement('a')
    anchor.href = url
    anchor.download = entry.name
    anchor.click()
    URL.revokeObjectURL(url)
    transferProgress.value = '下载完成并已校验。'
  } catch (error) {
    errorMessage.value = error instanceof Error ? error.message : '文件下载中断，可重新发起续传。'
    transferProgress.value = ''
  }
}

const chooseUpload = () => {
  if (props.writable) uploadInput.value?.click()
}

const upload = async (event: Event) => {
  if (!props.writable) return
  const file = (event.target as HTMLInputElement).files?.[0]
  if (!file) return
  const path = [currentPath.value, file.name].filter(Boolean).join('/')
  transferProgress.value = '正在建立加密文件通道…'
  try {
    await rpc.uploadFile(
      path,
      file,
      (sent, total) => {
        transferProgress.value = `上传 ${formatBytes(sent)} / ${formatBytes(total)}`
      },
      projectContext.value,
    )
    transferProgress.value = '上传完成，目标设备已校验并发布。'
    await load()
  } catch (error) {
    errorMessage.value =
      error instanceof Error ? error.message : '文件上传中断，可重新选择同一文件续传。'
  } finally {
    ;(event.target as HTMLInputElement).value = ''
  }
}

onMounted(() => void load())
</script>

<template>
  <section
    class="remote-panel remote-files-panel"
    :aria-labelledby="headingId"
    tabindex="0"
    @keydown="handleKeyboard"
  >
    <div class="remote-panel-heading">
      <div>
        <h2 :id="headingId">文件</h2>
        <p>仅允许访问设备已授权的项目根目录；路径和正文不会写入云端。</p>
      </div>
      <div class="file-heading-actions">
        <button type="button" :disabled="!writable" @click="createDirectory">新建文件夹</button>
        <button type="button" :disabled="!writable" @click="createTextFile">新建文本</button>
        <button type="button" :disabled="!writable" @click="chooseUpload">上传文件</button>
        <input ref="uploadInput" class="sr-only" type="file" @change="upload" />
      </div>
    </div>
    <p v-if="errorMessage" class="remote-notice error" role="alert">{{ errorMessage }}</p>
    <p v-if="!writable" class="remote-notice warning" role="status">
      当前授权为只读；可以浏览、预览和下载，但不能修改设备文件。
    </p>
    <p v-if="transferProgress" class="remote-notice success" role="status">
      {{ transferProgress }}
    </p>
    <div class="file-toolbar" aria-label="文件工具栏">
      <div class="file-history-actions">
        <button type="button" title="后退" :disabled="backStack.length === 0" @click="goBack">
          ←
        </button>
        <button type="button" title="前进" :disabled="forwardStack.length === 0" @click="goForward">
          →
        </button>
        <button type="button" title="刷新" :disabled="loading" @click="load(false)">↻</button>
      </div>
      <div class="file-sort-actions" aria-label="文件排序">
        <button
          v-for="option in sortOptions"
          :key="option[0]"
          type="button"
          :class="{ active: sortKey === option[0] }"
          @click="changeSort(option[0])"
        >
          {{ option[1] }}<span v-if="sortKey === option[0]">{{ sortAscending ? ' ↑' : ' ↓' }}</span>
        </button>
      </div>
      <div class="file-view-actions" aria-label="文件视图">
        <button
          type="button"
          title="列表视图"
          :class="{ active: viewMode === 'list' }"
          @click="viewMode = 'list'"
        >
          ☷
        </button>
        <button
          type="button"
          title="网格视图"
          :class="{ active: viewMode === 'grid' }"
          @click="viewMode = 'grid'"
        >
          ▦
        </button>
      </div>
    </div>
    <form class="file-search" role="search" @submit.prevent="search(false)">
      <input v-model="searchQuery" type="search" placeholder="搜索文件名、相对路径和文本内容" />
      <button type="submit">搜索</button>
      <button v-if="showingSearch" type="button" @click="clearSearch">清除</button>
    </form>
    <p v-if="searchTruncated" class="remote-notice warning" role="status">
      搜索已达到设备端安全扫描上限，请缩小关键词范围。
    </p>
    <nav class="file-breadcrumbs" aria-label="文件路径">
      <button
        v-for="crumb in breadcrumbs"
        :key="crumb.path"
        type="button"
        @click="navigate(crumb.path)"
      >
        {{ crumb.label }}
      </button>
    </nav>
    <div class="file-layout">
      <div class="file-list" :class="viewMode">
        <p v-if="loading" class="remote-panel-empty">正在读取目录…</p>
        <p v-else-if="entries.length === 0" class="remote-panel-empty">
          {{ showingSearch ? '没有匹配的文件。' : '目录为空。' }}
        </p>
        <article
          v-for="entry in sortedEntries"
          :key="entry.id"
          :class="{ selected: selected?.id === entry.id }"
        >
          <button class="file-open" type="button" @click="open(entry)">
            <span aria-hidden="true">{{ entry.kind === 'directory' ? '▰' : '▤' }}</span>
            <span
              ><strong>{{ entry.name }}</strong
              ><small
                >{{
                  entry.kind === 'directory'
                    ? '文件夹'
                    : `${entry.category} · ${formatBytes(entry.size)}`
                }}
                · {{ new Date(entry.modifiedAt).toLocaleString('zh-CN') }}</small
              ></span
            >
          </button>
          <div class="file-actions">
            <button v-if="entry.kind === 'file'" type="button" @click="download(entry)">
              下载
            </button>
            <button
              v-if="entry.writable"
              type="button"
              :disabled="!writable"
              @click="rename(entry)"
            >
              改名
            </button>
            <button v-if="entry.writable" type="button" :disabled="!writable" @click="move(entry)">
              移动
            </button>
            <button
              v-if="entry.writable"
              type="button"
              :disabled="!writable"
              @click="remove(entry)"
            >
              删除
            </button>
          </div>
        </article>
        <button v-if="nextCursor" class="remote-load-more" type="button" @click="load(true)">
          加载更多
        </button>
      </div>
      <div class="file-preview">
        <p v-if="previewLoading">正在读取加密内容…</p>
        <template v-else-if="selected">
          <div class="file-preview-head">
            <span>
              <strong>{{ selected.name }}</strong>
              <small v-if="details">
                {{ selected.category }} · {{ formatBytes(selected.size) }} ·
                {{ selectedEncoding }}
              </small>
            </span>
            <button
              type="button"
              :disabled="saving || !writable || !selected.writable || previewTruncated"
              @click="save"
            >
              {{ saving ? '保存中…' : '保存' }}
            </button>
          </div>
          <p v-if="previewTruncated" class="remote-notice error">
            这是截断预览，为防止数据丢失已禁止编辑和保存。
          </p>
          <textarea
            v-model="preview"
            :readonly="!writable || !selected.writable || previewTruncated"
            spellcheck="false"
          ></textarea>
        </template>
        <p v-else>选择文本文件可预览和编辑。</p>
      </div>
    </div>
  </section>
</template>

<style scoped>
.remote-files-panel:focus {
  outline: none;
}
.file-toolbar {
  display: grid;
  grid-template-columns: auto minmax(0, 1fr) auto;
  align-items: center;
  gap: 10px;
  margin: 4px 0 10px;
}
.file-history-actions,
.file-sort-actions,
.file-view-actions {
  display: flex;
  align-items: center;
  gap: 3px;
}
.file-toolbar button {
  min-width: 30px;
  min-height: 30px;
  border-color: transparent;
  padding: 5px 8px;
  color: var(--ink-soft);
  background: transparent;
  font-size: 0.72rem;
}
.file-toolbar button:hover:not(:disabled),
.file-toolbar button.active {
  color: var(--teal-dark);
  background: var(--brand-tint);
}
.file-sort-actions {
  justify-content: center;
  min-width: 0;
  overflow-x: auto;
}
.file-list.grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(150px, 1fr));
  align-content: start;
  gap: 8px;
  padding: 10px;
}
.file-list.grid > article {
  display: grid;
  grid-template-columns: minmax(0, 1fr);
  align-content: start;
  border: 1px solid var(--line);
  border-radius: 9px;
  padding: 8px;
}
.file-list.grid .file-open {
  display: grid;
  justify-items: start;
}
.file-list.grid .file-open > span:first-child {
  font-size: 1.5rem;
}
.file-list.grid .file-actions {
  flex-wrap: wrap;
  border-top: 1px solid var(--line);
  padding-top: 5px;
}
@media (max-width: 720px) {
  .file-toolbar {
    grid-template-columns: auto minmax(0, 1fr);
  }
  .file-sort-actions {
    grid-column: 1 / -1;
    grid-row: 2;
    justify-content: flex-start;
  }
}
</style>
