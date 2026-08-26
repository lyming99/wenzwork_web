<script setup lang="ts">
import { inject, onMounted, ref } from 'vue'

import { listRemoteProjects, syncRemoteProjects, type RemoteProject } from '@/api/remote'
import { problemMessage } from '@/api/auth'
import { remoteRPCKey } from '@/remote/rpcTypes'

const rpc = inject(remoteRPCKey)
if (!rpc) throw new Error('Remote RPC provider is required')

const props = defineProps<{
  deviceId: string
  canSync: boolean
  canDelete: boolean
  selectedProjectId?: string
}>()
const emit = defineEmits<{
  loaded: [projects: RemoteProject[]]
  select: [projectId: string]
}>()

const items = ref<RemoteProject[]>([])
const loading = ref(true)
const loadingMore = ref(false)
const syncing = ref(false)
const deletingProjectId = ref('')
const locallyRemovedProjectRevisions = new Map<string, number>()
const nextCursor = ref<string | null>(null)
const stale = ref(false)
const deviceOnline = ref(false)
const observedAt = ref<string | null>(null)
const errorMessage = ref('')
const statusMessage = ref('')

const formatDate = (value?: string | null) =>
  value
    ? new Intl.DateTimeFormat('zh-CN', {
        month: '2-digit',
        day: '2-digit',
        hour: '2-digit',
        minute: '2-digit',
        hour12: false,
      }).format(new Date(value))
    : '—'

const load = async (append = false) => {
  if (append) loadingMore.value = true
  else loading.value = true
  try {
    const page = await listRemoteProjects(
      props.deviceId,
      append ? (nextCursor.value ?? undefined) : undefined,
    )
    const visible = page.items.filter((project) => {
      const removedRevision = locallyRemovedProjectRevisions.get(project.id)
      if (removedRevision === undefined) return true
      if (project.revision > removedRevision) {
        locallyRemovedProjectRevisions.delete(project.id)
        return true
      }
      return false
    })
    items.value = append ? [...items.value, ...visible] : visible
    emit(
      'loaded',
      items.value.filter((project) => project.state === 'available'),
    )
    nextCursor.value = page.nextCursor
    stale.value = page.stale
    deviceOnline.value = page.deviceOnline
    observedAt.value = page.observedAt
    errorMessage.value = ''
  } catch (error) {
    errorMessage.value = problemMessage(error, '暂时无法读取项目快照。')
  } finally {
    loading.value = false
    loadingMore.value = false
  }
}

const projectRemoveMessage = (error: unknown) => {
  const message = error instanceof Error ? error.message : String(error)
  if (message.includes('PROJECT_HAS_AI_CONVERSATIONS')) {
    return '该项目仍有正在生成的 AI 对话，请先停止生成后再删除项目记录。'
  }
  if (message.includes('PROJECT_HAS_TASKS')) {
    return '该项目仍有未结束的任务，请先结束或取消后再删除项目记录。'
  }
  if (message.includes('resource revision changed')) {
    return '项目记录已经改变，请刷新后重试。'
  }
  return problemMessage(error, '无法删除远程项目记录，请检查设备连接。')
}

const isProjectRemoveResult = (value: unknown, project: RemoteProject) => {
  if (value === null || typeof value !== 'object' || Array.isArray(value)) return false
  const result = value as Record<string, unknown>
  return (
    result.removed === true &&
    result.projectId === project.id &&
    result.state === 'removed' &&
    typeof result.revision === 'number' &&
    Number.isSafeInteger(result.revision) &&
    result.revision > project.revision
  )
}

const removeProject = async (project: RemoteProject) => {
  if (!props.canDelete || deletingProjectId.value) return
  if (
    !window.confirm(
      `确定删除“${project.displayName}”的项目记录吗？\n\n设备上的文件夹和 Git 仓库不会被删除。如果项目已有 AI 对话、任务等相关数据，这些数据会继续保留，但删除项目记录后将无法再从项目入口访问。\n\n正在生成的 AI 对话或未结束的任务会阻止删除。`,
    )
  ) {
    return
  }
  deletingProjectId.value = project.id
  errorMessage.value = ''
  statusMessage.value = ''
  try {
    const result = await rpc.call<unknown>('project.remove', {
      projectId: project.id,
      expectedRevision: project.revision,
    })
    if (!isProjectRemoveResult(result, project)) {
      throw new Error('设备返回了无效的项目删除结果。')
    }
    locallyRemovedProjectRevisions.set(project.id, (result as { revision: number }).revision)
    items.value = items.value.filter((candidate) => candidate.id !== project.id)
    emit(
      'loaded',
      items.value.filter((candidate) => candidate.state === 'available'),
    )
    statusMessage.value = '项目记录已删除；设备文件夹、Git 仓库及已有对话/任务数据均未删除。'
    if (props.canSync && deviceOnline.value) {
      try {
        await syncRemoteProjects(props.deviceId)
      } catch {
        statusMessage.value =
          '项目记录已删除；设备文件夹、Git 仓库及已有对话/任务数据均未删除，项目快照将在下次同步时更新。'
      }
    }
  } catch (error) {
    errorMessage.value = projectRemoveMessage(error)
  } finally {
    deletingProjectId.value = ''
  }
}

const requestSync = async () => {
  if (syncing.value || !props.canSync) return
  syncing.value = true
  statusMessage.value = ''
  try {
    await syncRemoteProjects(props.deviceId)
    statusMessage.value = '已提交增量同步；设备确认后快照会自动更新。'
    window.setTimeout(() => void load(), 1200)
  } catch (error) {
    errorMessage.value = problemMessage(error, '无法请求项目同步。')
  } finally {
    syncing.value = false
  }
}

onMounted(() => void load())
</script>

<template>
  <section class="remote-panel" aria-labelledby="remote-projects-heading">
    <div class="remote-panel-heading">
      <div>
        <h2 id="remote-projects-heading">项目</h2>
        <p>
          {{ stale ? '当前显示上次成功同步的缓存。' : '当前显示设备确认的项目快照。' }}
          快照时间 {{ formatDate(observedAt) }}。
        </p>
      </div>
      <button type="button" :disabled="syncing || !deviceOnline || !canSync" @click="requestSync">
        {{ syncing ? '同步中…' : '同步设备项目' }}
      </button>
    </div>
    <p v-if="stale" class="remote-notice warning" role="status">
      设备当前{{ deviceOnline ? '在线但快照已过期' : '离线' }}；读取缓存不会触发重复远程查询。
    </p>
    <p v-if="!canSync" class="remote-notice warning" role="status">
      当前授权为只读；需要“同步项目”权限才能请求设备刷新快照。
    </p>
    <p v-if="statusMessage" class="remote-notice success" role="status">{{ statusMessage }}</p>
    <p v-if="errorMessage" class="remote-notice error" role="alert">{{ errorMessage }}</p>
    <p v-if="loading" class="remote-panel-empty">正在读取项目…</p>
    <p v-else-if="items.length === 0" class="remote-panel-empty">尚未同步到项目。</p>
    <div v-else class="remote-record-list">
      <div
        v-for="project in items"
        :key="project.id"
        class="remote-record project-record"
        :class="{ selected: project.id === selectedProjectId }"
        role="button"
        :tabindex="project.state === 'available' ? 0 : -1"
        :aria-disabled="project.state !== 'available'"
        @click="project.state === 'available' && emit('select', project.id)"
        @keydown.enter="project.state === 'available' && emit('select', project.id)"
      >
        <div>
          <span class="record-kind">PROJECT</span>
          <h3>{{ project.displayName }}</h3>
          <p>{{ project.capabilities.join(' · ') || '只读项目元数据' }}</p>
        </div>
        <dl>
          <div>
            <dt>修订</dt>
            <dd>{{ project.revision }}</dd>
          </div>
          <div>
            <dt>状态</dt>
            <dd>{{ project.state }}</dd>
          </div>
          <div>
            <dt>观测时间</dt>
            <dd>{{ formatDate(project.observedAt) }}</dd>
          </div>
        </dl>
        <span v-if="project.id === selectedProjectId" class="selected-label">当前项目</span>
        <button
          v-if="canDelete"
          class="project-delete"
          type="button"
          :disabled="Boolean(deletingProjectId)"
          @click.stop="removeProject(project)"
          @keydown.enter.stop="removeProject(project)"
        >
          {{ deletingProjectId === project.id ? '删除中…' : '删除记录' }}
        </button>
      </div>
    </div>
    <button
      v-if="nextCursor"
      class="remote-load-more"
      type="button"
      :disabled="loadingMore"
      @click="load(true)"
    >
      {{ loadingMore ? '读取中…' : '加载更多项目' }}
    </button>
  </section>
</template>

<style scoped>
.project-record {
  position: relative;
  width: 100%;
  padding-right: 110px;
  color: inherit;
  text-align: left;
  cursor: pointer;
}

.project-record.selected {
  border-color: var(--teal);
  box-shadow: 0 0 0 2px var(--mint);
}

.project-record[aria-disabled='true'] {
  cursor: not-allowed;
  opacity: 0.62;
}

.selected-label {
  position: absolute;
  top: 12px;
  right: 104px;
  color: var(--teal-dark);
  font-size: 0.72rem;
  font-weight: 750;
}

.project-delete {
  position: absolute;
  top: 10px;
  right: 12px;
  border-color: #efc0bc;
  color: #9b3028;
  background: #fff4f2;
  font-size: 0.76rem;
}
</style>
