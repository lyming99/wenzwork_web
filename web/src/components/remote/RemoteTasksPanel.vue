<script setup lang="ts">
import { computed, inject, nextTick, onBeforeUnmount, onMounted, ref, useId } from 'vue'

import { problemMessage } from '@/api/auth'
import { agentEventConnectionKey } from '@/remote/agentEvents'
import { PaginationBudget, PaginationBudgetExceededError } from '@/remote/paginationBudget'
import { agentSupportsTaskLogFiles } from '@/remote/peerClient'
import { remoteRPCKey, type RemoteRPCPage } from '@/remote/rpcTypes'

const aggregatePaginationLimits = {
  maximumPages: 16,
  maximumItems: 1_000,
  maximumBytes: 1024 * 1024,
  maximumCursorBytes: 512,
} as const

type TaskStatus =
  | 'queued'
  | 'waiting'
  | 'running'
  | 'awaitingAcceptance'
  | 'changesRequested'
  | 'completed'
  | 'succeeded'
  | 'failed'
  | 'blocked'
  | 'cancelled'
type TaskFilter = 'all' | 'queue' | 'errors' | 'acceptance' | 'completed'
type TaskView = 'list' | 'graph'
type TaskRelation = 'dependency' | 'sibling'
type TaskMode = 'serial' | 'parallel'

interface TaskExecution {
  relation: TaskRelation
  mode: TaskMode
  relatedTaskIds: string[]
  runImmediately: boolean
  scheduledAt?: string
  workflowId?: string
  cliSessionId?: string
  resumeCliSession: boolean
}

interface TaskDefinition {
  id: string
  projectId: string
  kind: string
  title: string
  cwd: string
  plan?: string
  config: Record<string, unknown>
  execution: TaskExecution
  scope: string
  ownerWorkflowTaskId?: string
  parentTaskId?: string
  rootTaskId?: string
  acceptanceFeedback?: string
  environment?: Record<string, string>
}

interface TaskRecord {
  definition: TaskDefinition
  definitionRevision: number
  status: TaskStatus
  revision: number
  changeSequence: number
  currentRunId?: string
  createdAt: string
  updatedAt: string
  startedAt?: string
  finishedAt?: string
  exitCode?: number
  resultCode?: string
  logAvailable: boolean
  logState: string
  logGeneration: number
  logSizeBytes: number
}

interface TaskRun {
  id: string
  taskId: string
  status: string
  attempt: number
  createdAt: string
  startedAt?: string
  finishedAt?: string
  exitCode?: number
  resultCode?: string
  cliSessionId?: string
  logAvailable: boolean
  logState: string
  logGeneration: number
  logFormatVersion: number
  logSizeBytes: number
}

interface TaskLog {
  taskId: string
  runId?: string
  sequence: number
  stream: string
  content?: string
  decodeWarning?: string
  occurredAt: string
}

interface TaskLogPage {
  items: TaskLog[]
  ackedThroughSequence: number
  highWatermark: number
  minimumAvailableSequence: number
  nextBeforeSequence?: number
  hasMore: boolean
  resetRequired: boolean
}

interface TaskLogSeekPage {
  taskId: string
  runId: string
  generation: number
  formatVersion: number
  logState: string
  content: string
  startOffset: number
  nextOffset: number
  fileSize: number
  eof: boolean
  hasMoreBefore: boolean
  sealed: boolean
  cursorAdjusted: boolean
  resetRequired: boolean
}

interface TaskLogWindow {
  runId: string
  generation: number
  formatVersion: number
  logState: string
  startOffset: number
  nextOffset: number
  fileSize: number
  text: string
  sealed: boolean
  hasMoreBefore: boolean
  resetRequired: boolean
}

interface TaskEditorState {
  title: string
  kind: 'codex' | 'script'
  cwd: string
  prompt: string
  model: string
  reasoningEffort: string
  goalMode: boolean
  relation: TaskRelation
  mode: TaskMode
  relatedTaskIds: string[]
  runImmediately: boolean
}

const props = defineProps<{
  deviceId: string
  deviceName: string
  projectId: string
  protocolVersion: number
  capabilityVersion: string
  online: boolean
  writable: boolean
}>()
const headingId = `remote-tasks-heading-${useId()}`
const injectedRPC = inject(remoteRPCKey)
if (!injectedRPC) throw new Error('Remote RPC client is required')
const rpc = injectedRPC
const agentEvents = inject(agentEventConnectionKey, null)
const context = computed(() => ({ projectId: props.projectId }))

const items = ref<TaskRecord[]>([])
const loading = ref(true)
const busy = ref(false)
const errorMessage = ref('')
const highWatermark = ref(0)
const query = ref('')
const filter = ref<TaskFilter>('all')
const view = ref<TaskView>('list')

const quickKind = ref<'codex' | 'script'>('codex')
const quickPrompt = ref('')
const quickModel = ref('')
const quickReasoning = ref('medium')
const quickGoalMode = ref(false)
const quickRelatedTaskIds = ref<string[]>([])

const selectedTaskId = ref<string>()
const runs = ref<Record<string, TaskRun[]>>({})
const selectedRunIds = ref<Record<string, string>>({})
const logs = ref<Record<string, TaskLog[]>>({})
const logPages = ref<Record<string, TaskLogPage>>({})
const logWindows = ref<Record<string, TaskLogWindow>>({})
const logLoading = ref(new Set<string>())
const fileTaskLogs = ref(false)
const taskLogDownloads = ref(false)
const tasksWithNewLogs = ref(new Set<string>())
const downloadingRunId = ref<string>()
const downloadReceived = ref(0)
const downloadTotal = ref(0)
const downloadPaused = ref(false)
const followLogs = ref(true)
const logScroller = ref<HTMLElement>()
const pendingLogRefreshes = new Set<string>()
const logRefreshTargets = new Map<string, number>()
const tasksWithLoadedLogHistory = new Set<string>()
const maximumLiveLogLines = 500
const maximumLiveLogBytes = 512 << 10
const taskLogSeekBytes = 32 << 10

const editorMode = ref<'create' | 'edit'>()
const editingTask = ref<TaskRecord>()
const familyAnchor = ref<TaskRecord>()
const editor = ref<TaskEditorState>(emptyEditor())
const editorError = ref('')

const rerunTask = ref<TaskRecord>()
const rerunTitle = ref('')
const rerunPrompt = ref('')
const rerunModel = ref('')
const rerunReasoning = ref('medium')
const rerunResume = ref(false)
const resumableSessionId = ref('')

const scheduleTargets = ref<TaskRecord[]>([])
const scheduleValue = ref('')
const scheduleIsQueue = ref(false)
const followUpTask = ref<TaskRecord>()
const followUpFeedback = ref('')
const prerequisiteTarget = ref<'quick' | 'editor'>()
const prerequisiteDraft = ref(new Set<string>())

let removeEventListener: (() => void) | undefined
let removeResetListener: (() => void) | undefined

const activeStatuses: TaskStatus[] = ['queued', 'waiting', 'running']
const errorStatuses: TaskStatus[] = ['failed', 'blocked', 'cancelled']
const completedStatuses: TaskStatus[] = ['completed', 'succeeded']
const successfulExecutionStatuses: TaskStatus[] = [
  'awaitingAcceptance',
  'changesRequested',
  'completed',
  'succeeded',
]
const clearableStatuses: TaskStatus[] = [...errorStatuses, ...completedStatuses]
const reasoningOptions = ['low', 'medium', 'high', 'xhigh', 'max', 'ultra']

const activeCount = computed(() => countStatus(activeStatuses))
const selectedTask = computed(() =>
  items.value.find((task) => task.definition.id === selectedTaskId.value),
)
const selectedTaskRun = computed(() =>
  selectedTask.value ? selectedRun(selectedTask.value) : undefined,
)
const selectedFileLogWindow = computed(() =>
  selectedTaskRun.value ? logWindows.value[selectedTaskRun.value.id] : undefined,
)
const hasNextAwaiting = computed(() =>
  items.value.some(
    (task) => task.status === 'awaitingAcceptance' && task.definition.id !== selectedTaskId.value,
  ),
)
const filterEntries = computed<Array<{ id: TaskFilter; label: string; count: number }>>(() => [
  { id: 'all', label: '全部', count: items.value.length },
  { id: 'queue', label: '执行队列', count: countStatus(activeStatuses) },
  { id: 'errors', label: '执行错误', count: countStatus(errorStatuses) },
  {
    id: 'acceptance',
    label: '待验收',
    count: items.value.filter((task) => task.status === 'awaitingAcceptance').length,
  },
  { id: 'completed', label: '已完成', count: countStatus(completedStatuses) },
])
const filteredItems = computed(() => {
  const needle = query.value.trim().toLocaleLowerCase()
  return items.value.filter((task) => {
    const statusMatch =
      filter.value === 'all' ||
      (filter.value === 'queue' && activeStatuses.includes(task.status)) ||
      (filter.value === 'errors' && errorStatuses.includes(task.status)) ||
      (filter.value === 'acceptance' && task.status === 'awaitingAcceptance') ||
      (filter.value === 'completed' && completedStatuses.includes(task.status))
    const text = `${task.definition.title} ${task.definition.kind} ${task.definition.cwd}`
    return statusMatch && (!needle || text.toLocaleLowerCase().includes(needle))
  })
})
const graphItems = computed(() => {
  const visible = new Map(filteredItems.value.map((task) => [task.definition.id, task]))
  const depth = (task: TaskRecord) => {
    let value = 0
    let parent = task.definition.parentTaskId
    const visited = new Set<string>()
    while (parent && visible.has(parent) && !visited.has(parent) && value < 6) {
      visited.add(parent)
      value += 1
      parent = visible.get(parent)?.definition.parentTaskId
    }
    return value
  }
  return filteredItems.value.map((task) => ({ task, depth: depth(task) }))
})

const statusLabel: Record<TaskStatus, string> = {
  queued: '待执行',
  waiting: '等待执行',
  running: '运行中',
  awaitingAcceptance: '待验收',
  changesRequested: '已转后续',
  completed: '已完成',
  succeeded: '已完成',
  failed: '失败',
  blocked: '已阻止',
  cancelled: '已取消',
}

function emptyEditor(): TaskEditorState {
  return {
    title: '',
    kind: 'codex',
    cwd: '',
    prompt: '',
    model: '',
    reasoningEffort: 'medium',
    goalMode: false,
    relation: 'dependency',
    mode: 'serial',
    relatedTaskIds: [],
    runImmediately: true,
  }
}

function countStatus(statuses: TaskStatus[]) {
  return items.value.filter((task) => statuses.includes(task.status)).length
}

function kindLabel(kind: string) {
  return kind === 'script' ? '脚本' : kind.charAt(0).toUpperCase() + kind.slice(1)
}

function taskContent(task: TaskRecord) {
  const value =
    task.definition.kind === 'script'
      ? task.definition.config.command
      : task.definition.config.promptText
  return typeof value === 'string' ? value : ''
}

function formatDate(value?: string) {
  return value
    ? new Intl.DateTimeFormat('zh-CN', {
        month: '2-digit',
        day: '2-digit',
        hour: '2-digit',
        minute: '2-digit',
      }).format(new Date(value))
    : '—'
}

function duration(task: TaskRecord) {
  if (!task.startedAt) return `创建于 ${formatDate(task.createdAt)}`
  const end = task.finishedAt ? Date.parse(task.finishedAt) : Date.now()
  const seconds = Math.max(0, Math.floor((end - Date.parse(task.startedAt)) / 1000))
  return `${task.finishedAt ? '用时' : '已运行'} ${Math.floor(seconds / 60)}:${String(seconds % 60).padStart(2, '0')}`
}

function relationText(task: TaskRecord) {
  const count = task.definition.execution.relatedTaskIds.length
  if (!count) return ''
  return task.definition.execution.relation === 'dependency'
    ? `前置 ${count}`
    : `兄弟${task.definition.execution.mode === 'parallel' ? '并行' : '串行'} ${count}`
}

function relationshipDependencyIds(task: TaskRecord, visiting = new Set<string>()) {
  if (visiting.has(task.definition.id)) return new Set<string>()
  visiting.add(task.definition.id)
  const dependencies = new Set<string>()
  if (task.definition.execution.relation === 'dependency') {
    task.definition.execution.relatedTaskIds.forEach((id) => dependencies.add(id))
  } else {
    for (const siblingId of task.definition.execution.relatedTaskIds) {
      const sibling = items.value.find((candidate) => candidate.definition.id === siblingId)
      if (!sibling) continue
      relationshipDependencyIds(sibling, visiting).forEach((id) => dependencies.add(id))
    }
  }
  visiting.delete(task.definition.id)
  dependencies.delete(task.definition.id)
  return dependencies
}

function dependencySatisfied(task: TaskRecord, dependency: TaskRecord) {
  if (successfulExecutionStatuses.includes(dependency.status)) return true
  return (
    errorStatuses.includes(dependency.status) &&
    task.definition.parentTaskId === dependency.definition.id &&
    task.definition.execution.relation === 'dependency' &&
    task.definition.execution.relatedTaskIds.includes(dependency.definition.id)
  )
}

function canRunWaitingNow(task: TaskRecord) {
  const execution = task.definition.execution
  if (
    task.status !== 'waiting' ||
    task.definition.kind === 'workflow' ||
    task.definition.scope !== 'topLevel' ||
    Boolean(execution.workflowId) ||
    execution.mode === 'parallel' ||
    (execution.scheduledAt ? Date.parse(execution.scheduledAt) > Date.now() : false)
  ) {
    return false
  }
  for (const dependencyId of relationshipDependencyIds(task)) {
    const dependency = items.value.find((candidate) => candidate.definition.id === dependencyId)
    if (!dependency || !dependencySatisfied(task, dependency)) return false
  }
  return true
}

function primaryTitle(task: TaskRecord) {
  if (task.status === 'queued') return '启动任务'
  if (task.status === 'waiting' && canRunWaitingNow(task)) return '立即执行（切换为并发）'
  if (task.status === 'waiting' && task.definition.execution.mode === 'parallel') {
    return '已进入并发执行队列'
  }
  if (task.status === 'waiting') return '正在等待前置任务完成'
  if (task.status === 'running') return '停止任务'
  if (task.status === 'awaitingAcceptance') return '验收通过'
  return '重新执行'
}

function primaryIcon(task: TaskRecord) {
  if (task.status === 'waiting' && !canRunWaitingNow(task)) return '◷'
  if (task.status === 'running') return '■'
  if (task.status === 'awaitingAcceptance') return '✓'
  return '▶'
}

function primaryDisabled(task: TaskRecord) {
  return task.status === 'waiting' && !canRunWaitingNow(task)
}

function canEdit(task: TaskRecord) {
  return !['running', 'waiting', 'awaitingAcceptance'].includes(task.status)
}

function canFollowUp(task: TaskRecord) {
  return (
    task.definition.kind !== 'script' &&
    (task.status === 'awaitingAcceptance' || errorStatuses.includes(task.status))
  )
}

function titleFromContent(value: string) {
  const first = value.trim().split(/\r?\n/, 1)[0] ?? ''
  return first.slice(0, 80) || '新任务'
}

function cloneDefinition(definition: TaskDefinition): TaskDefinition {
  return JSON.parse(JSON.stringify(definition)) as TaskDefinition
}

function configFor(
  kind: 'codex' | 'script',
  prompt: string,
  model: string,
  reasoningEffort: string,
  goalMode: boolean,
  base?: Record<string, unknown>,
) {
  if (kind === 'script') {
    return { command: prompt, cwdChoice: 'workspace', customCwd: '' }
  }
  const config: Record<string, unknown> =
    base && !('command' in base)
      ? { ...base }
      : {
          promptSource: 'customText',
          attachedFilePaths: [],
          launchMode: 'cli',
        }
  config.promptSource = 'customText'
  config.promptText = prompt
  config.goalMode = goalMode
  config.reasoningEffort = reasoningEffort
  if (model.trim()) config.model = model.trim()
  else delete config.model
  delete config.promptFilePath
  return config
}

async function load() {
  loading.value = items.value.length === 0
  try {
    const capabilities = await rpc.getCapabilities()
    fileTaskLogs.value = agentSupportsTaskLogFiles(capabilities)
    taskLogDownloads.value =
      fileTaskLogs.value && capabilities.features['taskLogs.bulkDownload'] === true
    const collected: TaskRecord[] = []
    const budget = new PaginationBudget('任务列表', aggregatePaginationLimits)
    let collectedHighWatermark = highWatermark.value
    let cursor: string | undefined
    do {
      budget.assertCanRequestPage()
      const page = await rpc.call<RemoteRPCPage<TaskRecord>>(
        'task.list',
        { limit: 20, ...(cursor ? { cursor } : {}) },
        context.value,
      )
      budget.admitPage(page.items)
      collected.push(...page.items)
      collectedHighWatermark = page.highWatermark
      cursor = budget.admitCursor(page.nextCursor)
    } while (cursor)
    items.value = collected
    highWatermark.value = collectedHighWatermark
    if (
      selectedTaskId.value &&
      !collected.some((item) => item.definition.id === selectedTaskId.value)
    ) {
      closeDetail()
    }
    errorMessage.value = ''
  } catch (error) {
    errorMessage.value = taskProblemMessage(error, '暂时无法读取任务。')
  } finally {
    loading.value = false
  }
}

function taskProblemMessage(error: unknown, fallback: string) {
  return error instanceof PaginationBudgetExceededError
    ? error.message
    : problemMessage(error, fallback)
}

async function perform<T>(action: () => Promise<T>, fallback: string): Promise<T | undefined> {
  if (!props.writable || busy.value) return
  busy.value = true
  try {
    const value = await action()
    errorMessage.value = ''
    return value
  } catch (error) {
    errorMessage.value = problemMessage(error, fallback)
  } finally {
    busy.value = false
  }
}

async function mutate(method: string, input: Record<string, unknown>) {
  await perform(async () => {
    await rpc.call(method, input, context.value)
    await load()
  }, '任务操作失败，请刷新后重试。')
}

function act(task: TaskRecord, method: string, extra: Record<string, unknown> = {}) {
  return mutate(method, {
    taskId: task.definition.id,
    expectedRevision: task.revision,
    ...extra,
  })
}

function startAll(event?: Event) {
  closeActionMenu(event)
  return mutate('task.queue.start', { expectedHighWatermark: highWatermark.value })
}

function stopAll() {
  return mutate('task.queue.stop', { expectedHighWatermark: highWatermark.value })
}

async function clearFinished() {
  if (!window.confirm('清理全部已完成、执行错误和已取消任务及其日志？待验收任务会保留。')) return
  await mutate('task.clear', { expectedHighWatermark: highWatermark.value })
}

async function removeTask(task: TaskRecord) {
  if (!window.confirm(`删除“${task.definition.title}”？`)) return
  await act(task, 'task.delete')
}

function closeActionMenu(event?: Event) {
  const details = (event?.currentTarget as HTMLElement | undefined)?.closest('details')
  details?.removeAttribute('open')
}

async function runPrimary(task: TaskRecord) {
  if (task.status === 'queued') return act(task, 'task.start')
  if (task.status === 'waiting') {
    if (canRunWaitingNow(task)) return act(task, 'task.start')
    return
  }
  if (task.status === 'running') return act(task, 'task.stop')
  if (task.status === 'awaitingAcceptance') return acceptTask(task)
  return openRerun(task)
}

async function acceptTask(task: TaskRecord, openNext = false) {
  await act(task, 'task.accept', { evidence: '远程验收通过' })
  if (!openNext) return
  const next = items.value.find(
    (item) => item.definition.id !== task.definition.id && item.status === 'awaitingAcceptance',
  )
  if (next) await openDetail(next)
}

async function undoAcceptance(task: TaskRecord) {
  await act(task, 'task.undo-acceptance')
}

async function copyTask(task: TaskRecord) {
  try {
    await navigator.clipboard.writeText(taskContent(task) || task.definition.title)
  } catch (error) {
    errorMessage.value = problemMessage(error, '无法复制任务内容。')
  }
}

function openCreate(anchor?: TaskRecord, relation: TaskRelation = 'dependency', event?: Event) {
  closeActionMenu(event)
  familyAnchor.value = anchor
  editingTask.value = undefined
  editorMode.value = 'create'
  editorError.value = ''
  editor.value = {
    ...emptyEditor(),
    relation,
    relatedTaskIds: anchor ? [anchor.definition.id] : [],
  }
}

function openEdit(task: TaskRecord) {
  editingTask.value = task
  familyAnchor.value = undefined
  editorMode.value = 'edit'
  editorError.value = ''
  const kind = task.definition.kind === 'script' ? 'script' : 'codex'
  editor.value = {
    title: task.definition.title,
    kind,
    cwd: task.definition.cwd,
    prompt: taskContent(task),
    model: typeof task.definition.config.model === 'string' ? task.definition.config.model : '',
    reasoningEffort:
      typeof task.definition.config.reasoningEffort === 'string'
        ? task.definition.config.reasoningEffort
        : 'medium',
    goalMode: task.definition.config.goalMode === true,
    relation: task.definition.execution.relation,
    mode: task.definition.execution.mode,
    relatedTaskIds: [...task.definition.execution.relatedTaskIds],
    runImmediately: task.definition.execution.runImmediately,
  }
}

function closeEditor() {
  editorMode.value = undefined
  editingTask.value = undefined
  familyAnchor.value = undefined
  editorError.value = ''
}

async function submitEditor() {
  const state = editor.value
  const prompt = state.prompt.trim()
  const title = state.title.trim() || titleFromContent(prompt)
  if (!prompt) {
    editorError.value = '任务内容不能为空。'
    return
  }
  if (!title || new TextEncoder().encode(title).length > 200) {
    editorError.value = '任务名称不能为空，且最多 200 个 UTF-8 字节。'
    return
  }
  const cwd = state.cwd.trim().replaceAll('\\', '/')
  if (cwd.startsWith('/') || cwd.split('/').includes('..')) {
    editorError.value = '工作目录必须是项目内相对路径，且不能包含“..”。'
    return
  }
  const source = editingTask.value
  const anchor = familyAnchor.value
  let definition: TaskDefinition
  if (source) {
    definition = cloneDefinition(source.definition)
    definition.title = title
    definition.kind = state.kind
    definition.cwd = cwd
    definition.config = configFor(
      state.kind,
      prompt,
      state.model,
      state.reasoningEffort,
      state.goalMode,
      source.definition.config,
    )
    definition.execution = {
      ...definition.execution,
      relation: state.relation,
      mode: state.relation === 'dependency' ? 'serial' : state.mode,
      relatedTaskIds: [...state.relatedTaskIds],
      runImmediately: state.runImmediately,
    }
  } else {
    definition = {
      id: crypto.randomUUID(),
      projectId: props.projectId,
      kind: state.kind,
      title,
      cwd,
      config: configFor(state.kind, prompt, state.model, state.reasoningEffort, state.goalMode),
      execution: {
        relation: state.relation,
        mode: state.relation === 'dependency' ? 'serial' : state.mode,
        relatedTaskIds: [...state.relatedTaskIds],
        runImmediately: state.runImmediately,
        resumeCliSession: false,
      },
      scope: 'topLevel',
    }
    if (anchor && state.relatedTaskIds.includes(anchor.definition.id)) {
      definition.parentTaskId =
        state.relation === 'dependency' ? anchor.definition.id : anchor.definition.parentTaskId
      definition.rootTaskId =
        state.relation === 'dependency'
          ? anchor.definition.rootTaskId || anchor.definition.id
          : anchor.definition.rootTaskId
    }
  }
  const succeeded = await perform(
    async () => {
      if (source) {
        await rpc.call(
          'task.update',
          { definition, expectedRevision: source.revision },
          context.value,
        )
      } else {
        await rpc.call('task.create', { definition }, context.value)
      }
      await load()
      return true
    },
    source ? '无法保存任务修改。' : '无法创建任务，请检查任务配置。',
  )
  if (succeeded) closeEditor()
}

async function createQuickTask() {
  const prompt = quickPrompt.value.trim()
  if (!prompt) return
  const definition: TaskDefinition = {
    id: crypto.randomUUID(),
    projectId: props.projectId,
    kind: quickKind.value,
    title: titleFromContent(prompt),
    cwd: '',
    scope: 'topLevel',
    config: configFor(
      quickKind.value,
      prompt,
      quickModel.value,
      quickReasoning.value,
      quickGoalMode.value,
    ),
    execution: {
      relation: 'dependency',
      mode: 'serial',
      relatedTaskIds: [...quickRelatedTaskIds.value],
      runImmediately: true,
      resumeCliSession: false,
    },
  }
  const succeeded = await perform(async () => {
    await rpc.call('task.create', { definition }, context.value)
    await load()
    return true
  }, '无法创建任务，请检查任务配置。')
  if (succeeded) {
    quickPrompt.value = ''
    quickRelatedTaskIds.value = []
    quickGoalMode.value = false
  }
}

function openPrerequisites(target: 'quick' | 'editor') {
  prerequisiteTarget.value = target
  prerequisiteDraft.value = new Set(
    target === 'quick' ? quickRelatedTaskIds.value : editor.value.relatedTaskIds,
  )
}

function togglePrerequisite(taskId: string, checked: boolean) {
  const next = new Set(prerequisiteDraft.value)
  if (checked) next.add(taskId)
  else next.delete(taskId)
  prerequisiteDraft.value = next
}

function confirmPrerequisites() {
  const values = [...prerequisiteDraft.value]
  if (prerequisiteTarget.value === 'quick') quickRelatedTaskIds.value = values
  else if (prerequisiteTarget.value === 'editor') editor.value.relatedTaskIds = values
  prerequisiteTarget.value = undefined
}

async function listRuns(task: TaskRecord) {
  const collected: TaskRun[] = []
  const budget = new PaginationBudget('任务运行历史', aggregatePaginationLimits)
  let cursor: string | undefined
  do {
    budget.assertCanRequestPage()
    const page = await rpc.call<RemoteRPCPage<TaskRun>>(
      'task.runs',
      { taskId: task.definition.id, limit: 50, ...(cursor ? { cursor } : {}) },
      context.value,
    )
    budget.admitPage(page.items)
    collected.push(...page.items)
    cursor = budget.admitCursor(page.nextCursor)
  } while (cursor)
  runs.value = { ...runs.value, [task.definition.id]: collected }
  const selected = selectedRunIds.value[task.definition.id]
  const preferred = collected.find((run) => run.id === task.currentRunId) ?? collected[0]
  if (!selected || !collected.some((run) => run.id === selected)) {
    selectedRunIds.value = {
      ...selectedRunIds.value,
      ...(preferred ? { [task.definition.id]: preferred.id } : {}),
    }
  }
  return collected
}

async function openRerun(task: TaskRecord) {
  let history = runs.value[task.definition.id]
  if (!history) {
    try {
      history = await listRuns(task)
    } catch (error) {
      errorMessage.value = taskProblemMessage(error, '无法读取任务运行历史。')
      history = []
    }
  }
  const resumable = history.find((run) => run.cliSessionId?.trim())
  rerunTask.value = task
  rerunTitle.value = task.definition.title
  rerunPrompt.value = taskContent(task)
  rerunModel.value =
    typeof task.definition.config.model === 'string' ? task.definition.config.model : ''
  rerunReasoning.value =
    typeof task.definition.config.reasoningEffort === 'string'
      ? task.definition.config.reasoningEffort
      : 'medium'
  rerunResume.value = false
  resumableSessionId.value = resumable?.cliSessionId ?? ''
}

async function submitRerun() {
  const task = rerunTask.value
  if (!task) return
  const prompt = rerunPrompt.value.trim()
  const title = rerunTitle.value.trim()
  if (!rerunResume.value && (!prompt || !title)) return
  const definition = cloneDefinition(task.definition)
  delete definition.execution.scheduledAt
  if (rerunResume.value && resumableSessionId.value) {
    definition.execution.cliSessionId = resumableSessionId.value
    definition.execution.resumeCliSession = true
  } else {
    delete definition.execution.cliSessionId
    definition.execution.resumeCliSession = false
    definition.title = title
    definition.config = configFor(
      definition.kind === 'script' ? 'script' : 'codex',
      prompt,
      rerunModel.value,
      rerunReasoning.value,
      definition.config.goalMode === true,
      definition.config,
    )
  }
  const succeeded = await perform(async () => {
    const updated = await rpc.call<TaskRecord>(
      'task.update',
      { definition, expectedRevision: task.revision },
      context.value,
    )
    await rpc.call(
      'task.retry',
      { taskId: task.definition.id, expectedRevision: updated.revision },
      context.value,
    )
    await load()
    return true
  }, '无法重新执行任务。')
  if (succeeded) rerunTask.value = undefined
}

function openScheduleTask(task: TaskRecord) {
  scheduleTargets.value = [task]
  scheduleIsQueue.value = false
  scheduleValue.value = toLocalInput(task.definition.execution.scheduledAt)
}

function openQueueSchedule(event?: Event) {
  closeActionMenu(event)
  const queued = items.value.filter(
    (task) => task.status === 'queued' && task.definition.kind !== 'workflow',
  )
  if (!queued.length) return
  scheduleTargets.value = queued
  scheduleIsQueue.value = true
  scheduleValue.value = toLocalInput(new Date(Date.now() + 60 * 60 * 1000).toISOString())
}

function toLocalInput(value?: string) {
  if (!value) return ''
  const date = new Date(value)
  const offset = date.getTimezoneOffset() * 60_000
  return new Date(date.getTime() - offset).toISOString().slice(0, 16)
}

async function submitSchedule(clear = false) {
  const targets = scheduleTargets.value
  if (!targets.length) return
  const value = clear ? undefined : new Date(scheduleValue.value)
  if (
    !clear &&
    (!scheduleValue.value || Number.isNaN(value?.getTime()) || value!.getTime() <= Date.now())
  ) {
    errorMessage.value = '定时启动时间必须晚于当前时间。'
    return
  }
  const succeeded = await perform(async () => {
    for (const task of targets) {
      const definition = cloneDefinition(task.definition)
      if (clear) delete definition.execution.scheduledAt
      else definition.execution.scheduledAt = value!.toISOString()
      await rpc.call('task.update', { definition, expectedRevision: task.revision }, context.value)
    }
    await load()
    return true
  }, '无法设置定时启动。')
  if (succeeded) scheduleTargets.value = []
}

function openFollowUp(task: TaskRecord) {
  followUpTask.value = task
  followUpFeedback.value = ''
}

async function submitFollowUp() {
  const task = followUpTask.value
  const feedback = followUpFeedback.value.trim()
  if (!task || !feedback) return
  const succeeded = await perform(async () => {
    await rpc.call(
      'task.follow-up',
      {
        sourceTaskId: task.definition.id,
        taskId: crypto.randomUUID(),
        expectedRevision: task.revision,
        feedback,
      },
      context.value,
    )
    await load()
    return true
  }, '无法追加后续任务。')
  if (succeeded) followUpTask.value = undefined
}

async function openDetail(task: TaskRecord) {
  selectedTaskId.value = task.definition.id
  const nextUnread = new Set(tasksWithNewLogs.value)
  nextUnread.delete(task.definition.id)
  tasksWithNewLogs.value = nextUnread
  followLogs.value = true
  try {
    if (fileTaskLogs.value) {
      await listRuns(task)
      await loadLogs(task, 'initial')
    } else {
      await Promise.all([listRuns(task), loadLogs(task, 'initial')])
    }
  } catch (error) {
    errorMessage.value = taskProblemMessage(error, '无法读取任务详情。')
  }
}

function selectedRun(task: TaskRecord) {
  const runId = selectedRunIds.value[task.definition.id]
  return runs.value[task.definition.id]?.find((run) => run.id === runId)
}

async function selectRun(task: TaskRecord, runId: string) {
  selectedRunIds.value = { ...selectedRunIds.value, [task.definition.id]: runId }
  followLogs.value = true
  clearLogRefresh(task.definition.id)
  await loadLogs(task, 'initial')
}

function onRunSelection(task: TaskRecord, event: Event) {
  void selectRun(task, (event.target as HTMLSelectElement).value)
}

async function downloadSelectedLog(task: TaskRecord) {
  const run = selectedRun(task)
  const generation = run ? (logWindows.value[run.id]?.generation ?? run.logGeneration) : 0
  if (
    !taskLogDownloads.value ||
    !run ||
    !run.logAvailable ||
    generation < 1 ||
    downloadingRunId.value
  )
    return
  downloadingRunId.value = run.id
  downloadPaused.value = false
  downloadReceived.value = 0
  downloadTotal.value = run.logSizeBytes
  try {
    const result = await rpc.downloadTaskLog(
      task.definition.id,
      run.id,
      generation,
      (received, total) => {
        downloadReceived.value = received
        downloadTotal.value = total
      },
      context.value,
    )
    const url = URL.createObjectURL(result.blob)
    const anchor = document.createElement('a')
    anchor.href = url
    anchor.download = result.fileName
    anchor.click()
    setTimeout(() => URL.revokeObjectURL(url), 0)
  } catch (error) {
    errorMessage.value = problemMessage(error, '无法下载完整任务日志。')
  } finally {
    rpc.resumeDownloads?.()
    downloadPaused.value = false
    downloadingRunId.value = undefined
  }
}

function toggleDownloadPaused() {
  downloadPaused.value = !downloadPaused.value
  if (downloadPaused.value) rpc.pauseDownloads?.()
  else rpc.resumeDownloads?.()
}

function closeDetail() {
  const id = selectedTaskId.value
  if (id) clearLogRefresh(id)
  selectedTaskId.value = undefined
}

function clearLogRefresh(taskId: string) {
  pendingLogRefreshes.delete(taskId)
  logRefreshTargets.delete(taskId)
}

async function requestNewerLogs(task: TaskRecord, targetHighWatermark?: number) {
  if (fileTaskLogs.value) {
    return requestNewerFileLogs(task, targetHighWatermark)
  }
  const id = task.definition.id
  if (selectedTaskId.value !== id) return
  const acknowledged =
    logPages.value[id]?.ackedThroughSequence ?? logPages.value[id]?.highWatermark ?? 0
  if (targetHighWatermark !== undefined) {
    if (!Number.isSafeInteger(targetHighWatermark) || targetHighWatermark <= acknowledged) return
    logRefreshTargets.set(id, Math.max(logRefreshTargets.get(id) ?? 0, targetHighWatermark))
  }
  pendingLogRefreshes.add(id)
  if (logLoading.value.has(id)) return
  try {
    while (pendingLogRefreshes.delete(id) && selectedTaskId.value === id) {
      const mode = logPages.value[id] ? 'newer' : 'initial'
      const before =
        logPages.value[id]?.ackedThroughSequence ?? logPages.value[id]?.highWatermark ?? 0
      const more = await loadLogs(task, mode)
      const after =
        logPages.value[id]?.ackedThroughSequence ?? logPages.value[id]?.highWatermark ?? 0
      const target = logRefreshTargets.get(id) ?? 0
      if (after >= target) logRefreshTargets.delete(id)
      if (mode === 'newer' && after > before && (more || after < target)) {
        pendingLogRefreshes.add(id)
      }
    }
  } catch (error) {
    if (selectedTaskId.value === id) {
      errorMessage.value = problemMessage(error, '无法读取最新任务日志。')
    }
  }
}

async function requestNewerFileLogs(task: TaskRecord, targetHighWatermark?: number) {
  const id = task.definition.id
  const run = selectedRun(task)
  if (!run || selectedTaskId.value !== id) return
  const window = logWindows.value[run.id]
  const acknowledged = window?.nextOffset ?? 0
  if (targetHighWatermark !== undefined) {
    if (!Number.isSafeInteger(targetHighWatermark) || targetHighWatermark <= acknowledged) return
    logRefreshTargets.set(id, Math.max(logRefreshTargets.get(id) ?? 0, targetHighWatermark))
  }
  pendingLogRefreshes.add(id)
  if (logLoading.value.has(id)) return
  let deadline = performance.now() + 300
  let pages = 0
  try {
    while (
      pendingLogRefreshes.delete(id) &&
      selectedTaskId.value === id &&
      selectedRunIds.value[id] === run.id
    ) {
      const before = logWindows.value[run.id]?.nextOffset ?? 0
      await loadFileLogs(task, 'newer')
      pages++
      const current = logWindows.value[run.id]
      const target = logRefreshTargets.get(id) ?? 0
      if ((current?.nextOffset ?? 0) >= target) logRefreshTargets.delete(id)
      if ((current?.nextOffset ?? 0) < target && (current?.nextOffset ?? 0) > before) {
        pendingLogRefreshes.add(id)
        if (pages >= 8 || performance.now() >= deadline) {
          pages = 0
          deadline = performance.now() + 300
        }
        await new Promise<void>((resolve) => setTimeout(resolve, 0))
      }
    }
  } catch (error) {
    if (selectedTaskId.value === id) {
      errorMessage.value = problemMessage(error, '无法读取最新任务日志。')
    }
  }
}

async function loadLogs(
  task: TaskRecord,
  mode: 'initial' | 'older' | 'newer',
  scroller?: HTMLElement,
): Promise<boolean> {
  if (fileTaskLogs.value) return loadFileLogs(task, mode, scroller)
  const id = task.definition.id
  if (logLoading.value.has(id)) return false
  const nextLoading = new Set(logLoading.value)
  nextLoading.add(id)
  logLoading.value = nextLoading
  const previousPage = logPages.value[id]
  const oldHeight = scroller?.scrollHeight ?? 0
  try {
    const input: Record<string, unknown> = { taskId: id, limitBytes: 102400 }
    if (mode === 'initial') input.limitLines = 100
    if (mode === 'older') {
      input.limitLines = 100
      input.beforeSequence = previousPage?.nextBeforeSequence
    }
    if (mode === 'newer') {
      input.afterSequence = previousPage?.ackedThroughSequence ?? previousPage?.highWatermark ?? 0
    }
    const page = await rpc.call<TaskLogPage>('task.logs', input, context.value)
    const existing = mode === 'initial' ? [] : (logs.value[id] ?? [])
    const merged = new Map([...existing, ...page.items].map((line) => [line.sequence, line]))
    let mergedLines = [...merged.values()].sort((left, right) => left.sequence - right.sequence)
    if (mode === 'initial') tasksWithLoadedLogHistory.delete(id)
    if (mode === 'older' && page.items.length > 0) tasksWithLoadedLogHistory.add(id)
    let trimmedBeforeSequence: number | undefined
    if (
      mode === 'newer' &&
      !tasksWithLoadedLogHistory.has(id) &&
      mergedLines.length > maximumLiveLogLines
    ) {
      mergedLines = mergedLines.slice(-maximumLiveLogLines)
      trimmedBeforeSequence = Math.max(0, mergedLines[0]!.sequence - 1)
    }
    logs.value = {
      ...logs.value,
      [id]: mergedLines,
    }
    const previousAck = previousPage?.ackedThroughSequence ?? previousPage?.highWatermark ?? 0
    logPages.value = {
      ...logPages.value,
      [id]: {
        ...page,
        ackedThroughSequence:
          mode === 'newer'
            ? Math.max(previousAck, page.ackedThroughSequence)
            : mode === 'older'
              ? previousAck
              : page.ackedThroughSequence,
        highWatermark: Math.max(previousPage?.highWatermark ?? 0, page.highWatermark),
        hasMore:
          trimmedBeforeSequence !== undefined
            ? trimmedBeforeSequence > 0
            : mode === 'newer'
              ? (previousPage?.hasMore ?? false)
              : page.hasMore,
        nextBeforeSequence:
          trimmedBeforeSequence ??
          (mode === 'newer' ? previousPage?.nextBeforeSequence : page.nextBeforeSequence),
      },
    }
    await nextTick()
    const element = scroller ?? logScroller.value
    if (!element) return mode === 'newer' && page.hasMore
    if (mode === 'older') element.scrollTop += element.scrollHeight - oldHeight
    else if (mode === 'initial' || followLogs.value) element.scrollTop = element.scrollHeight
    return mode === 'newer' && page.hasMore
  } finally {
    const remaining = new Set(logLoading.value)
    remaining.delete(id)
    logLoading.value = remaining
    if (mode !== 'newer' && pendingLogRefreshes.has(id) && selectedTaskId.value === id) {
      void requestNewerLogs(task)
    }
  }
}

const utf8Encoder = new TextEncoder()
const utf8Decoder = new TextDecoder('utf-8', { fatal: true })

function trimTaskLogWindow(window: TaskLogWindow, side: 'start' | 'end') {
  let bytes = utf8Encoder.encode(window.text)
  let lines = window.text ? window.text.split('\n').length - 1 : 0
  let removed = 0
  while ((bytes.length > maximumLiveLogBytes || lines > maximumLiveLogLines) && window.text) {
    if (side === 'start') {
      const newline = window.text.indexOf('\n')
      if (newline < 0) break
      const prefix = window.text.slice(0, newline + 1)
      const prefixBytes = utf8Encoder.encode(prefix).length
      window.text = window.text.slice(newline + 1)
      removed += prefixBytes
    } else {
      const previousNewline = window.text.lastIndexOf('\n', window.text.length - 2)
      const suffixStart = previousNewline + 1
      const suffix = window.text.slice(suffixStart)
      const suffixBytes = utf8Encoder.encode(suffix).length
      window.text = window.text.slice(0, suffixStart)
      removed += suffixBytes
    }
    lines--
    bytes = utf8Encoder.encode(window.text)
  }
  if (removed > 0) {
    if (side === 'start') {
      window.startOffset += removed
      window.hasMoreBefore = true
    } else {
      window.nextOffset -= removed
    }
  }
  return window
}

function validateTaskLogSeekPage(page: TaskLogSeekPage, taskId: string, runId: string) {
  const integers = [
    page.generation,
    page.formatVersion,
    page.startOffset,
    page.nextOffset,
    page.fileSize,
  ]
  if (
    page.taskId !== taskId ||
    page.runId !== runId ||
    integers.some((value) => !Number.isSafeInteger(value) || value < 0) ||
    page.nextOffset < page.startOffset ||
    page.nextOffset > page.fileSize ||
    utf8Encoder.encode(page.content).length !== page.nextOffset - page.startOffset
  ) {
    throw new Error('设备返回的任务日志窗口无效。')
  }
  return page
}

async function loadFileLogs(
  task: TaskRecord,
  mode: 'initial' | 'older' | 'newer',
  scroller?: HTMLElement,
): Promise<boolean> {
  const id = task.definition.id
  const run = selectedRun(task)
  if (!run || logLoading.value.has(id)) return false
  logLoading.value = new Set(logLoading.value).add(id)
  const oldHeight = scroller?.scrollHeight ?? 0
  try {
    let existing: TaskLogWindow | undefined = logWindows.value[run.id]
    const input: Record<string, unknown> = {
      taskId: id,
      runId: run.id,
      limitBytes: taskLogSeekBytes,
    }
    if ((existing?.generation ?? run.logGeneration) > 0) {
      input.generation = existing?.generation ?? run.logGeneration
    }
    if (mode === 'initial') input.tailBytes = taskLogSeekBytes
    if (mode === 'older') input.beforeOffset = existing?.startOffset ?? 0
    if (mode === 'newer') input.offset = existing?.nextOffset ?? 0
    let page = validateTaskLogSeekPage(
      await rpc.call<TaskLogSeekPage>('task.logs', input, context.value),
      id,
      run.id,
    )
    let resetNotice = false
    if (page.resetRequired || (existing && page.generation !== existing.generation)) {
      resetNotice = true
      page = validateTaskLogSeekPage(
        await rpc.call<TaskLogSeekPage>(
          'task.logs',
          { taskId: id, runId: run.id, tailBytes: taskLogSeekBytes, limitBytes: taskLogSeekBytes },
          context.value,
        ),
        id,
        run.id,
      )
      existing = undefined
      mode = 'initial'
    }
    let next: TaskLogWindow
    if (!existing || mode === 'initial') {
      next = {
        runId: run.id,
        generation: page.generation,
        formatVersion: page.formatVersion,
        logState: page.logState,
        startOffset: page.startOffset,
        nextOffset: page.nextOffset,
        fileSize: page.fileSize,
        text: page.content,
        sealed: page.sealed,
        hasMoreBefore: page.hasMoreBefore,
        resetRequired: resetNotice,
      }
    } else if (mode === 'older') {
      if (page.nextOffset !== existing.startOffset) throw new Error('任务日志向前窗口不连续。')
      next = {
        ...existing,
        startOffset: page.startOffset,
        fileSize: Math.max(existing.fileSize, page.fileSize),
        text: page.content + existing.text,
        hasMoreBefore: page.hasMoreBefore,
      }
    } else {
      const overlap = Math.max(0, existing.nextOffset - page.startOffset)
      const pageBytes = utf8Encoder.encode(page.content)
      if (overlap > pageBytes.length) throw new Error('任务日志增量窗口不连续。')
      const suffix = utf8Decoder.decode(pageBytes.slice(overlap))
      next = {
        ...existing,
        nextOffset: page.nextOffset,
        fileSize: page.fileSize,
        text: existing.text + suffix,
        sealed: page.sealed,
        logState: page.logState,
      }
    }
    logWindows.value = {
      ...logWindows.value,
      [run.id]: trimTaskLogWindow(next, mode === 'older' ? 'end' : 'start'),
    }
    await nextTick()
    const element = scroller ?? logScroller.value
    if (element) {
      if (mode === 'older') element.scrollTop += element.scrollHeight - oldHeight
      else if (mode === 'initial' || followLogs.value) element.scrollTop = element.scrollHeight
    }
    return mode === 'newer' && page.nextOffset < page.fileSize
  } finally {
    const remaining = new Set(logLoading.value)
    remaining.delete(id)
    logLoading.value = remaining
    if (mode !== 'newer' && pendingLogRefreshes.has(id) && selectedTaskId.value === id) {
      void requestNewerLogs(task)
    }
  }
}

function onLogScroll(task: TaskRecord, event: Event) {
  const element = event.currentTarget as HTMLElement
  followLogs.value = element.scrollHeight - element.scrollTop - element.clientHeight < 36
  const hasMoreBefore = fileTaskLogs.value
    ? (selectedFileLogWindow.value?.hasMoreBefore ?? false)
    : (logPages.value[task.definition.id]?.hasMore ?? false)
  if (element.scrollTop <= 12 && hasMoreBefore) {
    void loadLogs(task, 'older', element)
  }
}

function jumpToLatestLog() {
  followLogs.value = true
  if (logScroller.value) logScroller.value.scrollTop = logScroller.value.scrollHeight
}

function emptyMessage() {
  if (query.value.trim()) return '没有匹配的任务'
  if (!items.value.length) return '当前项目暂无任务'
  return {
    all: '暂无任务，点击下方 ＋ 创建',
    queue: '执行队列为空',
    errors: '暂无执行错误',
    acceptance: '暂无待验收任务',
    completed: '暂无已完成任务',
  }[filter.value]
}

onMounted(() => {
  void load()
  removeEventListener = agentEvents?.onEvent(async (event) => {
    if (event.projectId !== props.projectId) return
    if (event.type === 'task.changed' || event.type === 'workflow.changed') await load()
    if (event.type !== 'task.logs.available') return
    const task = selectedTask.value
    if (task?.definition.id !== event.aggregateId) {
      tasksWithNewLogs.value = new Set(tasksWithNewLogs.value).add(event.aggregateId)
      return
    }
    if (fileTaskLogs.value) {
      const runId = event.data.runId
      const generation = event.data.generation
      const target = event.data.highWatermark
      if (typeof runId !== 'string' || typeof generation !== 'number' || typeof target !== 'number')
        return
      if (selectedTaskRun.value?.id !== runId) {
        tasksWithNewLogs.value = new Set(tasksWithNewLogs.value).add(event.aggregateId)
        return
      }
      const window = logWindows.value[runId]
      if (event.operation === 'invalidate' || (window && window.generation !== generation)) {
        const next = { ...logWindows.value }
        delete next[runId]
        logWindows.value = next
        await loadLogs(task, 'initial')
        return
      }
      await requestNewerLogs(task, target)
      return
    }
    const target = event.data.highWatermark
    if (typeof target === 'number') await requestNewerLogs(task, target)
  })
  removeResetListener = agentEvents?.onReset(load)
})
onBeforeUnmount(() => {
  removeEventListener?.()
  removeResetListener?.()
  pendingLogRefreshes.clear()
  logRefreshTargets.clear()
  tasksWithLoadedLogHistory.clear()
})
</script>

<template>
  <section class="task-center" :aria-labelledby="headingId">
    <header class="task-head">
      <div class="task-title">
        <span v-if="activeCount" class="running-dot" aria-hidden="true"></span>
        <h2 :id="headingId">任务中心 · {{ deviceName }}</h2>
      </div>
      <div class="head-actions">
        <details class="action-menu queue-menu">
          <summary
            title="启动任务队列（立即或定时）"
            :aria-disabled="!writable || busy"
            @click="!writable || busy ? $event.preventDefault() : undefined"
          >
            <span aria-hidden="true">▶⌄</span>
          </summary>
          <div class="action-popover">
            <button type="button" @click="startAll($event)">▶ 立即启动任务队列</button>
            <button
              type="button"
              :disabled="!items.some((task) => task.status === 'queued')"
              @click="openQueueSchedule($event)"
            >
              ◷ 定时启动任务队列
            </button>
          </div>
        </details>
        <button
          class="header-icon"
          type="button"
          title="全部停止"
          :disabled="!writable || busy || !activeCount"
          @click="stopAll"
        >
          ■
        </button>
      </div>
    </header>

    <p v-if="!online" class="task-banner warning">
      设备离线；完整任务正文只在设备与 E2EE 通道中保存，重新上线后才能读取。
    </p>
    <p v-else-if="!writable" class="task-banner warning">当前授权为只读，任务操作已停用。</p>
    <p v-if="errorMessage" class="task-banner error" role="alert">{{ errorMessage }}</p>

    <div class="task-body">
      <div class="task-toolbar">
        <label class="task-search">
          <span aria-hidden="true">⌕</span>
          <input v-model="query" aria-label="搜索任务" placeholder="搜索任务…" />
          <button v-if="query" type="button" title="清除搜索" @click="query = ''">×</button>
        </label>
        <div class="toolbar-row">
          <div class="task-filters" role="tablist" aria-label="任务状态筛选">
            <button
              v-for="entry in filterEntries"
              :key="entry.id"
              type="button"
              role="tab"
              :aria-selected="filter === entry.id"
              :class="{ active: filter === entry.id }"
              @click="filter = entry.id"
            >
              {{ entry.label }} <span>{{ entry.count }}</span>
            </button>
          </div>
          <div class="toolbar-tools">
            <div class="view-switch" aria-label="任务视图">
              <button
                type="button"
                title="列表视图"
                :class="{ active: view === 'list' }"
                @click="view = 'list'"
              >
                ☷
              </button>
              <button
                type="button"
                title="任务导图"
                :class="{ active: view === 'graph' }"
                @click="view = 'graph'"
              >
                ⑂
              </button>
            </div>
            <button
              class="clear-button"
              type="button"
              title="清理已结束任务"
              :disabled="
                !writable || busy || !items.some((task) => clearableStatuses.includes(task.status))
              "
              @click="clearFinished"
            >
              ♲
            </button>
          </div>
        </div>
      </div>

      <div v-if="loading" class="task-empty"><span>◌</span>正在读取任务…</div>
      <div v-else-if="!filteredItems.length" class="task-empty">
        <span>✓</span>{{ emptyMessage() }}
      </div>

      <div v-else-if="view === 'list'" class="task-list">
        <article
          v-for="task in filteredItems"
          :key="task.definition.id"
          class="task-card"
          tabindex="0"
          @click="openDetail(task)"
          @keydown.enter="openDetail(task)"
        >
          <div class="task-card-top">
            <span class="kind-icon" :class="task.definition.kind" aria-hidden="true">
              {{ task.definition.kind === 'script' ? '>_' : '✦' }}
            </span>
            <div class="task-main">
              <h3>{{ task.definition.title }}</h3>
              <p>
                <span>{{ task.definition.cwd || '项目根目录' }}</span> ·
                {{ kindLabel(task.definition.kind) }} · {{ duration(task) }}
                <template v-if="relationText(task)"> · {{ relationText(task) }}</template>
              </p>
            </div>
            <span class="status-chip" :class="task.status">
              <i v-if="task.status === 'running'"></i>{{ statusLabel[task.status] }}
            </span>
            <span
              v-if="tasksWithNewLogs.has(task.definition.id)"
              class="new-log-badge"
              aria-label="有新日志"
              >新日志</span
            >
            <button
              class="round-action"
              :class="{ stop: task.status === 'running' }"
              type="button"
              :title="primaryTitle(task)"
              :disabled="!writable || busy || primaryDisabled(task)"
              @click.stop="runPrimary(task)"
            >
              {{ primaryIcon(task) }}
            </button>
          </div>
          <div v-if="task.status === 'running'" class="progress"><span></span></div>
          <div class="task-card-actions" @click.stop>
            <button type="button" title="复制任务内容" @click="copyTask(task)">▣</button>
            <button
              v-if="task.status === 'waiting'"
              class="danger"
              type="button"
              title="停止等待"
              @click="act(task, 'task.stop')"
            >
              ■
            </button>
            <button
              v-if="task.status === 'queued'"
              type="button"
              :title="task.definition.execution.scheduledAt ? '修改或取消定时启动' : '定时启动'"
              @click="openScheduleTask(task)"
            >
              ◷
            </button>
            <button
              v-if="canFollowUp(task)"
              type="button"
              title="追加后续任务"
              @click="openFollowUp(task)"
            >
              ↪
            </button>
            <details class="action-menu related-menu">
              <summary title="添加子节点或兄弟节点">＋</summary>
              <div class="action-popover">
                <button type="button" @click="openCreate(task, 'dependency', $event)">
                  添加子节点
                </button>
                <button type="button" @click="openCreate(task, 'sibling', $event)">
                  添加兄弟节点
                </button>
              </div>
            </details>
            <button v-if="canEdit(task)" type="button" title="编辑任务" @click="openEdit(task)">
              ✎
            </button>
            <button
              v-if="clearableStatuses.includes(task.status)"
              class="danger"
              type="button"
              title="删除任务"
              @click="removeTask(task)"
            >
              ⌫
            </button>
          </div>
          <p v-if="task.definition.execution.scheduledAt" class="scheduled-line">
            ◷ 定时启动 {{ formatDate(task.definition.execution.scheduledAt) }}
          </p>
        </article>
      </div>

      <div v-else class="task-graph" aria-label="任务导图">
        <div class="graph-help">全局任务导图 · 点击节点查看详情</div>
        <div class="graph-canvas">
          <article
            v-for="entry in graphItems"
            :key="entry.task.definition.id"
            class="graph-node"
            :style="{ marginLeft: `${entry.depth * 46}px` }"
            @click="openDetail(entry.task)"
          >
            <span class="graph-line" aria-hidden="true"></span>
            <span class="kind-icon" :class="entry.task.definition.kind">
              {{ entry.task.definition.kind === 'script' ? '>_' : '✦' }}
            </span>
            <div>
              <strong>{{ entry.task.definition.title }}</strong>
              <small>{{ relationText(entry.task) || kindLabel(entry.task.definition.kind) }}</small>
            </div>
            <span class="status-chip" :class="entry.task.status">
              {{ statusLabel[entry.task.status] }}
            </span>
            <button
              type="button"
              :title="primaryTitle(entry.task)"
              :disabled="!writable || busy || primaryDisabled(entry.task)"
              @click.stop="runPrimary(entry.task)"
            >
              {{ primaryIcon(entry.task) }}
            </button>
            <button
              v-if="entry.task.status === 'waiting'"
              type="button"
              title="停止等待"
              :disabled="!writable || busy"
              @click.stop="act(entry.task, 'task.stop')"
            >
              ■
            </button>
          </article>
        </div>
      </div>

      <form class="quick-composer" @submit.prevent="createQuickTask">
        <textarea
          v-model="quickPrompt"
          :disabled="!writable || busy"
          rows="2"
          :placeholder="
            quickKind === 'script'
              ? '输入要执行的脚本命令，回车创建并执行…'
              : '输入任务要求；创建后立即加入执行队列…'
          "
          @keydown.ctrl.enter.prevent="createQuickTask"
        ></textarea>
        <div class="composer-tools">
          <button
            type="button"
            title="查询前置任务"
            :disabled="!writable || busy"
            @click="openPrerequisites('quick')"
          >
            ⑂<b v-if="quickRelatedTaskIds.length">{{ quickRelatedTaskIds.length }}</b>
          </button>
          <select v-model="quickKind" aria-label="任务运行器" :disabled="!writable || busy">
            <option value="codex">✦ Codex</option>
            <option value="script">&gt;_ 脚本</option>
          </select>
          <label v-if="quickKind === 'codex'" class="compact-check">
            <input v-model="quickGoalMode" type="checkbox" />目标
          </label>
          <input
            v-if="quickKind === 'codex'"
            v-model="quickModel"
            class="model-input"
            aria-label="任务模型"
            placeholder="默认模型"
          />
          <select v-if="quickKind === 'codex'" v-model="quickReasoning" aria-label="思考深度">
            <option v-for="effort in reasoningOptions" :key="effort" :value="effort">
              {{ effort }}
            </option>
          </select>
          <span class="composer-spacer"></span>
          <button
            type="button"
            title="新建任务"
            :disabled="!writable || busy"
            @click="openCreate()"
          >
            ＋
          </button>
          <button
            class="quick-submit"
            type="submit"
            title="创建并执行任务"
            :disabled="!writable || busy || !quickPrompt.trim()"
          >
            ↑
          </button>
        </div>
      </form>
    </div>

    <div v-if="editorMode" class="modal-backdrop" @mousedown.self="closeEditor">
      <form
        class="task-modal editor-modal"
        role="dialog"
        aria-modal="true"
        @submit.prevent="submitEditor"
      >
        <header>
          <div>
            <span>任务配置</span>
            <h3>{{ editorMode === 'edit' ? '编辑任务' : '新建远程任务' }}</h3>
          </div>
          <button type="button" title="关闭" @click="closeEditor">×</button>
        </header>
        <div class="modal-body form-grid">
          <label class="wide"
            >任务名称<input v-model="editor.title" placeholder="留空时使用任务要求第一行"
          /></label>
          <label
            >运行器<select v-model="editor.kind">
              <option value="codex">Codex</option>
              <option value="script">脚本</option>
            </select></label
          >
          <label
            >项目内工作目录<input v-model="editor.cwd" placeholder="留空表示项目根目录"
          /></label>
          <label v-if="editor.kind === 'codex'"
            >模型<input v-model="editor.model" placeholder="留空使用设备默认模型"
          /></label>
          <label v-if="editor.kind === 'codex'"
            >思考深度<select v-model="editor.reasoningEffort">
              <option v-for="effort in reasoningOptions" :key="effort" :value="effort">
                {{ effort }}
              </option>
            </select></label
          >
          <label
            >任务关系<select
              v-model="editor.relation"
              @change="editor.relation === 'dependency' ? (editor.mode = 'serial') : undefined"
            >
              <option value="dependency">依赖 / 子节点</option>
              <option value="sibling">兄弟节点</option>
            </select></label
          >
          <label
            >执行方式<select v-model="editor.mode" :disabled="editor.relation === 'dependency'">
              <option value="serial">串行执行</option>
              <option value="parallel">并行执行</option>
            </select></label
          >
          <div class="wide relation-picker">
            <span
              ><b>{{ editor.relation === 'dependency' ? '前置任务' : '兄弟节点锚点' }}</b
              ><small>{{
                editor.relatedTaskIds.length ? `已选择 ${editor.relatedTaskIds.length} 项` : '可选'
              }}</small></span
            ><button type="button" @click="openPrerequisites('editor')">选择 / 管理</button>
          </div>
          <label class="wide"
            >{{ editor.kind === 'script' ? '脚本命令' : '任务要求'
            }}<textarea v-model="editor.prompt" rows="7" required></textarea>
          </label>
          <label v-if="editor.kind === 'codex'" class="wide switch-row"
            ><input v-model="editor.goalMode" type="checkbox" /><span
              ><b>持续目标模式</b><small>让 Codex 持续工作直到目标完整实现</small></span
            ></label
          >
          <label class="wide switch-row"
            ><input v-model="editor.runImmediately" type="checkbox" /><span
              ><b>创建后加入串行队列</b><small>关闭后仅保存任务，不会开始调度</small></span
            ></label
          >
          <p v-if="editorError" class="form-error wide">{{ editorError }}</p>
        </div>
        <footer>
          <button type="button" @click="closeEditor">取消</button
          ><button class="primary" type="submit" :disabled="busy">
            {{ editorMode === 'edit' ? '保存修改' : '加入串行队列' }}
          </button>
        </footer>
      </form>
    </div>

    <div v-if="rerunTask" class="modal-backdrop" @mousedown.self="rerunTask = undefined">
      <form
        class="task-modal rerun-modal"
        role="dialog"
        aria-modal="true"
        @submit.prevent="submitRerun"
      >
        <header>
          <div>
            <span>重新执行</span>
            <h3>修改任务配置后重跑</h3>
          </div>
          <button type="button" @click="rerunTask = undefined">×</button>
        </header>
        <div class="modal-body form-grid">
          <label class="wide">任务名称<input v-model="rerunTitle" :disabled="rerunResume" /></label>
          <label class="wide"
            >{{ rerunTask.definition.kind === 'script' ? '脚本命令' : '任务要求'
            }}<textarea v-model="rerunPrompt" :disabled="rerunResume" rows="7"></textarea>
          </label>
          <label v-if="rerunTask.definition.kind !== 'script'"
            >模型<input
              v-model="rerunModel"
              :disabled="rerunResume"
              placeholder="留空使用设备默认模型"
          /></label>
          <label v-if="rerunTask.definition.kind !== 'script'"
            >思考深度<select v-model="rerunReasoning" :disabled="rerunResume">
              <option v-for="effort in reasoningOptions" :key="effort" :value="effort">
                {{ effort }}
              </option>
            </select></label
          >
          <label
            v-if="resumableSessionId && rerunTask.definition.kind !== 'script'"
            class="wide switch-row"
            ><input v-model="rerunResume" type="checkbox" /><span
              ><b>继续上次 CLI 会话</b><small>会话 {{ resumableSessionId }}</small></span
            ></label
          >
        </div>
        <footer>
          <button type="button" @click="rerunTask = undefined">取消</button
          ><button class="primary" type="submit" :disabled="busy">
            {{ rerunResume ? '继续会话' : '重新执行' }}
          </button>
        </footer>
      </form>
    </div>

    <div
      v-if="scheduleTargets.length"
      class="modal-backdrop"
      @mousedown.self="scheduleTargets = []"
    >
      <form
        class="task-modal small-modal"
        role="dialog"
        aria-modal="true"
        @submit.prevent="submitSchedule()"
      >
        <header>
          <div>
            <span>执行计划</span>
            <h3>{{ scheduleIsQueue ? '定时启动任务队列' : '定时启动任务' }}</h3>
          </div>
          <button type="button" @click="scheduleTargets = []">×</button>
        </header>
        <div class="modal-body">
          <label>启动时间<input v-model="scheduleValue" type="datetime-local" required /></label>
          <p>到点后任务会进入现有执行队列，仍会遵守前置依赖和并发上限。</p>
        </div>
        <footer>
          <button
            v-if="!scheduleIsQueue && scheduleTargets[0]?.definition.execution.scheduledAt"
            type="button"
            class="danger-text"
            @click="submitSchedule(true)"
          >
            取消定时</button
          ><span></span><button type="button" @click="scheduleTargets = []">取消</button
          ><button class="primary" type="submit" :disabled="busy">确认定时</button>
        </footer>
      </form>
    </div>

    <div v-if="followUpTask" class="modal-backdrop" @mousedown.self="followUpTask = undefined">
      <form
        class="task-modal small-modal"
        role="dialog"
        aria-modal="true"
        @submit.prevent="submitFollowUp"
      >
        <header>
          <div>
            <span>验收反馈</span>
            <h3>追加后续任务</h3>
          </div>
          <button type="button" @click="followUpTask = undefined">×</button>
        </header>
        <div class="modal-body">
          <p>
            {{
              followUpTask.status === 'awaitingAcceptance'
                ? '填写未通过验收后的修改要求。'
                : '基于执行错误补充后续要求。'
            }}
          </p>
          <textarea
            v-model="followUpFeedback"
            rows="6"
            required
            placeholder="请填写具体补充要求…"
          ></textarea>
        </div>
        <footer>
          <button type="button" @click="followUpTask = undefined">取消</button
          ><button class="primary" type="submit" :disabled="busy || !followUpFeedback.trim()">
            创建并执行后续任务
          </button>
        </footer>
      </form>
    </div>

    <div
      v-if="prerequisiteTarget"
      class="modal-backdrop modal-top"
      @mousedown.self="prerequisiteTarget = undefined"
    >
      <div class="task-modal prerequisite-modal" role="dialog" aria-modal="true">
        <header>
          <div>
            <span>任务关系</span>
            <h3>选择前置任务</h3>
          </div>
          <button type="button" @click="prerequisiteTarget = undefined">×</button>
        </header>
        <div class="modal-body prerequisite-list">
          <p v-if="!items.length">暂无可选任务。</p>
          <label
            v-for="task in items.filter(
              (item) => item.definition.id !== editingTask?.definition.id,
            )"
            :key="task.definition.id"
            ><input
              type="checkbox"
              :checked="prerequisiteDraft.has(task.definition.id)"
              @change="
                togglePrerequisite(task.definition.id, ($event.target as HTMLInputElement).checked)
              "
            /><span
              ><b>{{ task.definition.title }}</b
              ><small
                >{{ statusLabel[task.status] }} · {{ kindLabel(task.definition.kind) }}</small
              ></span
            ></label
          >
        </div>
        <footer>
          <button type="button" @click="prerequisiteTarget = undefined">取消</button
          ><button class="primary" type="button" @click="confirmPrerequisites">
            确认（{{ prerequisiteDraft.size }}）
          </button>
        </footer>
      </div>
    </div>

    <div v-if="selectedTask" class="modal-backdrop detail-backdrop" @mousedown.self="closeDetail">
      <section
        class="task-modal detail-modal"
        role="dialog"
        aria-modal="true"
        aria-labelledby="task-detail-title"
      >
        <header>
          <div>
            <span>任务详情 · 仅经 E2EE 读取</span>
            <h3 id="task-detail-title">{{ selectedTask.definition.title }}</h3>
          </div>
          <button type="button" title="关闭" @click="closeDetail">×</button>
        </header>
        <div class="detail-scroll">
          <div class="detail-summary">
            <div>
              <span class="status-chip" :class="selectedTask.status">{{
                statusLabel[selectedTask.status]
              }}</span
              ><em>{{ kindLabel(selectedTask.definition.kind) }}</em>
              <h4>{{ selectedTask.definition.title }}</h4>
              <p v-if="selectedTask.definition.plan">{{ selectedTask.definition.plan }}</p>
            </div>
            <div class="detail-actions">
              <button
                v-if="selectedTask.status === 'queued' || canRunWaitingNow(selectedTask)"
                type="button"
                @click="act(selectedTask, 'task.start')"
              >
                ▶ {{ selectedTask.status === 'waiting' ? '立即执行（并发）' : '启动' }}</button
              ><button
                v-if="['waiting', 'running'].includes(selectedTask.status)"
                type="button"
                @click="act(selectedTask, 'task.stop')"
              >
                ■ 停止</button
              ><button
                v-if="selectedTask.status === 'awaitingAcceptance'"
                class="primary"
                type="button"
                @click="acceptTask(selectedTask)"
              >
                ✓ 验收通过</button
              ><button
                v-if="selectedTask.status === 'awaitingAcceptance' && hasNextAwaiting"
                type="button"
                @click="acceptTask(selectedTask, true)"
              >
                验收并进入下一项</button
              ><button
                v-if="selectedTask.status === 'awaitingAcceptance' && canFollowUp(selectedTask)"
                type="button"
                @click="openFollowUp(selectedTask)"
              >
                要求修改</button
              ><button
                v-if="completedStatuses.includes(selectedTask.status)"
                type="button"
                @click="undoAcceptance(selectedTask)"
              >
                撤销验收</button
              ><button
                v-if="
                  [
                    'failed',
                    'blocked',
                    'cancelled',
                    'changesRequested',
                    'completed',
                    'succeeded',
                  ].includes(selectedTask.status)
                "
                type="button"
                @click="openRerun(selectedTask)"
              >
                ↻ 重新执行</button
              ><button
                v-if="clearableStatuses.includes(selectedTask.status)"
                class="danger-text"
                type="button"
                @click="removeTask(selectedTask)"
              >
                删除
              </button>
            </div>
          </div>
          <section class="detail-section">
            <h5>任务详情</h5>
            <dl>
              <div>
                <dt>任务 ID</dt>
                <dd>{{ selectedTask.definition.id }}</dd>
              </div>
              <div>
                <dt>项目内目录</dt>
                <dd>{{ selectedTask.definition.cwd || '项目根目录' }}</dd>
              </div>
              <div>
                <dt>状态 / 定义修订</dt>
                <dd>{{ selectedTask.revision }} / {{ selectedTask.definitionRevision }}</dd>
              </div>
              <div>
                <dt>创建时间</dt>
                <dd>{{ formatDate(selectedTask.createdAt) }}</dd>
              </div>
              <div>
                <dt>更新时间</dt>
                <dd>{{ formatDate(selectedTask.updatedAt) }}</dd>
              </div>
              <div v-if="selectedTask.exitCode !== undefined">
                <dt>退出码 / 结果码</dt>
                <dd>{{ selectedTask.exitCode }} / {{ selectedTask.resultCode || '—' }}</dd>
              </div>
              <div v-if="selectedTask.logAvailable">
                <dt>日志状态</dt>
                <dd>{{ selectedTask.logState }} · {{ selectedTask.logSizeBytes }} bytes</dd>
              </div>
            </dl>
          </section>
          <section class="detail-section">
            <h5>任务内容</h5>
            <pre class="task-content">{{ taskContent(selectedTask) || '暂无任务正文。' }}</pre>
          </section>
          <section class="detail-section">
            <div class="section-title">
              <h5>
                运行历史 <small>{{ runs[selectedTask.definition.id]?.length || 0 }} 次</small>
              </h5>
              <div
                v-if="fileTaskLogs && runs[selectedTask.definition.id]?.length"
                class="run-log-actions"
              >
                <select
                  :value="selectedRunIds[selectedTask.definition.id]"
                  aria-label="选择日志运行"
                  @change="onRunSelection(selectedTask, $event)"
                >
                  <option
                    v-for="run in runs[selectedTask.definition.id]"
                    :key="run.id"
                    :value="run.id"
                  >
                    #{{ run.attempt }} · {{ run.status }} · {{ run.logState }}
                  </option>
                </select>
                <button
                  v-if="taskLogDownloads"
                  type="button"
                  :disabled="!selectedTaskRun?.logAvailable || !!downloadingRunId"
                  @click="downloadSelectedLog(selectedTask)"
                >
                  {{ downloadingRunId ? '下载中…' : '下载完整日志' }}
                </button>
                <button v-if="downloadingRunId" type="button" @click="toggleDownloadPaused">
                  {{ downloadPaused ? '继续' : '暂停' }}
                </button>
              </div>
            </div>
            <p v-if="downloadingRunId" class="download-progress">
              {{ downloadPaused ? '已暂停' : '已校验' }} {{ downloadReceived }} /
              {{ downloadTotal }} bytes（Carrier 重连后自动续传）
            </p>
            <div v-if="!runs[selectedTask.definition.id]?.length" class="section-empty">
              暂无运行记录。
            </div>
            <div v-else class="run-list">
              <div v-for="run in runs[selectedTask.definition.id]" :key="run.id">
                <b>#{{ run.attempt }} · {{ run.status }}</b
                ><span>{{ formatDate(run.startedAt || run.createdAt) }}</span
                ><span>exit {{ run.exitCode ?? '—' }} / {{ run.resultCode || '—' }}</span
                ><small v-if="run.cliSessionId">CLI {{ run.cliSessionId }}</small
                ><small v-if="run.logAvailable"
                  >日志 {{ run.logState }} · {{ run.logSizeBytes }} bytes</small
                >
              </div>
            </div>
          </section>
          <section class="detail-section log-section">
            <div class="section-title">
              <h5>
                设备端日志
                <small v-if="fileTaskLogs">32 KiB 字节窗口 · 向上滚动加载更早内容</small>
                <small v-else>最新 100 行 · 向上滚动加载更早内容</small>
              </h5>
              <button type="button" title="刷新日志" @click="loadLogs(selectedTask, 'initial')">
                ↻
              </button>
            </div>
            <p
              v-if="
                fileTaskLogs
                  ? selectedFileLogWindow?.resetRequired
                  : logPages[selectedTask.definition.id]?.resetRequired
              "
              class="log-warning"
            >
              {{
                fileTaskLogs
                  ? '日志文件已更新，已从当前文件尾部重新加载。'
                  : '较早日志已被设备回收，以下从可用序列继续。'
              }}
            </p>
            <div ref="logScroller" class="task-log" @scroll="onLogScroll(selectedTask, $event)">
              <p
                v-if="
                  logLoading.has(selectedTask.definition.id) &&
                  !logs[selectedTask.definition.id]?.length
                "
              >
                正在读取日志…
              </p>
              <template v-if="fileTaskLogs">
                <p v-if="!selectedFileLogWindow?.text">暂无日志。</p>
                <pre v-else class="file-log-text">{{ selectedFileLogWindow.text }}</pre>
              </template>
              <template v-else>
                <p v-if="!logs[selectedTask.definition.id]?.length">暂无日志。</p>
                <template v-for="line in logs[selectedTask.definition.id]" :key="line.sequence"
                  ><small
                    >[{{ String(line.sequence).padStart(6, '0') }} {{ line.stream }}]<template
                      v-if="line.decodeWarning"
                    >
                      · {{ line.decodeWarning }}</template
                    ></small
                  >
                  <pre :class="line.stream">{{ line.content }}</pre>
                </template>
              </template>
            </div>
            <button
              v-if="
                !followLogs &&
                (fileTaskLogs
                  ? !!selectedFileLogWindow?.text
                  : !!logs[selectedTask.definition.id]?.length)
              "
              class="tail-button"
              type="button"
              @click="jumpToLatestLog"
            >
              ↓ 回到最新日志
            </button>
          </section>
          <section class="detail-section">
            <h5>完整定义</h5>
            <pre class="definition-json">{{
              JSON.stringify(selectedTask.definition, null, 2)
            }}</pre>
          </section>
        </div>
      </section>
    </div>
  </section>
</template>

<style scoped>
.task-center {
  --task-primary: #16897a;
  --task-blue: #3478d4;
  --task-danger: #c24141;
  --task-amber: #b7791f;
  display: grid;
  grid-template-rows: 36px auto minmax(0, 1fr);
  min-height: 680px;
  overflow: hidden;
  border: 1px solid var(--line);
  border-radius: 12px;
  color: var(--ink);
  background: #fff;
  box-shadow: var(--shadow-small);
}
.task-head,
.task-title,
.head-actions,
.toolbar-row,
.toolbar-tools,
.view-switch,
.task-card-top,
.task-card-actions,
.composer-tools,
.relation-picker,
.switch-row,
.detail-summary,
.detail-actions,
.section-title {
  display: flex;
  align-items: center;
}
.task-head {
  justify-content: space-between;
  height: 36px;
  border-bottom: 1px solid var(--line);
  padding: 0 10px;
  background: #fff;
}
.task-title {
  min-width: 0;
  gap: 8px;
}
.task-title h2 {
  overflow: hidden;
  margin: 0;
  font-size: 15px;
  font-weight: 600;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.running-dot {
  flex: none;
  width: 7px;
  height: 7px;
  border-radius: 50%;
  background: var(--task-blue);
  animation: blink 1.2s infinite;
}
.head-actions {
  gap: 2px;
}
.header-icon,
.queue-menu summary {
  display: grid;
  place-items: center;
  width: 30px;
  height: 30px;
  border: 0;
  border-radius: 7px;
  padding: 0;
  color: var(--ink-soft);
  background: transparent;
  cursor: pointer;
}
.header-icon:hover,
.queue-menu summary:hover {
  background: var(--paper-soft);
}
button:disabled {
  cursor: not-allowed !important;
  opacity: 0.38;
}
.task-banner {
  margin: 8px 12px 0;
  border-radius: 7px;
  padding: 8px 11px;
  font-size: 0.76rem;
}
.task-banner.warning {
  color: #8a6218;
  background: #fff7e8;
}
.task-banner.error {
  color: #a33143;
  background: #fff0f2;
}
.task-body {
  min-height: 0;
  display: grid;
  grid-template-rows: auto minmax(220px, 1fr) auto;
  background: #fbfcfd;
}
.task-toolbar {
  display: grid;
  gap: 8px;
  padding: 10px 14px 4px;
  background: #fff;
}
.task-search {
  display: flex;
  align-items: center;
  gap: 7px;
  height: 34px;
  border: 1px solid transparent;
  border-radius: 8px;
  padding: 0 10px;
  color: var(--ink-faint);
  background: var(--paper-soft);
}
.task-search:focus-within {
  border-color: var(--task-primary);
}
.task-search input {
  min-width: 0;
  flex: 1;
  border: 0;
  outline: 0;
  color: var(--ink);
  background: transparent;
  font-size: 12.5px;
}
.task-search button {
  border: 0;
  padding: 2px 5px;
  color: var(--ink-faint);
  background: transparent;
}
.toolbar-row {
  min-width: 0;
  justify-content: space-between;
  gap: 8px;
}
.task-filters {
  min-width: 0;
  display: flex;
  gap: 2px;
  overflow-x: auto;
  scrollbar-width: none;
}
.task-filters button {
  flex: none;
  border: 0;
  border-bottom: 2px solid transparent;
  padding: 6px 8px 5px;
  color: var(--ink-soft);
  background: transparent;
  font-size: 11px;
  white-space: nowrap;
}
.task-filters button span {
  margin-left: 2px;
  color: var(--ink-faint);
  font-size: 10px;
}
.task-filters button.active {
  border-bottom-color: var(--task-primary);
  color: var(--task-primary);
  font-weight: 700;
}
.toolbar-tools {
  flex: none;
  gap: 2px;
}
.view-switch {
  border-radius: 6px;
  padding: 2px;
  background: var(--paper-soft);
}
.view-switch button,
.clear-button {
  width: 27px;
  height: 27px;
  border: 0;
  border-radius: 5px;
  padding: 0;
  color: var(--ink-faint);
  background: transparent;
}
.view-switch button.active {
  color: var(--task-primary);
  background: #fff;
  box-shadow: 0 1px 3px #0002;
}
.task-list {
  min-height: 0;
  overflow-y: auto;
  display: grid;
  align-content: start;
  gap: 8px;
  padding: 8px 14px 14px;
}
.task-card {
  position: relative;
  border: 1px solid var(--line);
  border-radius: 9px;
  padding: 10px 11px 8px;
  background: #fff;
  transition: 0.15s;
  cursor: pointer;
}
.task-card:hover,
.task-card:focus-visible {
  border-color: #cbd4d8;
  outline: none;
  box-shadow: 0 3px 12px #102a3810;
}
.task-card-top {
  align-items: flex-start;
  gap: 9px;
}
.kind-icon {
  display: grid;
  place-items: center;
  flex: none;
  width: 32px;
  height: 32px;
  border-radius: 8px;
  color: var(--task-primary);
  background: #16897a1c;
  font-weight: 700;
}
.kind-icon.script {
  color: var(--task-blue);
  background: #3478d41c;
  font-family: Consolas, monospace;
}
.task-main {
  min-width: 0;
  flex: 1;
}
.task-main h3 {
  overflow: hidden;
  margin: 1px 0 0;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-size: 0.84rem;
}
.task-main p {
  overflow: hidden;
  margin: 3px 0 0;
  color: var(--ink-faint);
  font-size: 0.68rem;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.task-main p span {
  font-family: Consolas, monospace;
}
.status-chip {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  flex: none;
  border-radius: 999px;
  padding: 3px 7px;
  color: var(--ink-soft);
  background: var(--paper-soft);
  font-size: 0.66rem;
  font-style: normal;
}
.new-log-badge {
  flex: none;
  border-radius: 999px;
  padding: 3px 7px;
  color: #0f766e;
  background: #ccfbf1;
  font-size: 0.64rem;
  font-weight: 700;
}
.status-chip.waiting,
.status-chip.awaitingAcceptance {
  color: var(--task-amber);
  background: #b7791f1d;
}
.status-chip.running {
  color: var(--task-blue);
  background: #3478d41c;
}
.status-chip.completed,
.status-chip.succeeded {
  color: var(--task-primary);
  background: #16897a1c;
}
.status-chip.failed,
.status-chip.blocked {
  color: var(--task-danger);
  background: #c241411a;
}
.status-chip.changesRequested {
  color: #7446a8;
  background: #7446a817;
}
.status-chip i {
  width: 9px;
  height: 9px;
  border: 1.5px solid currentColor;
  border-top-color: transparent;
  border-radius: 50%;
  animation: spin 0.8s linear infinite;
}
.round-action {
  display: grid;
  place-items: center;
  flex: none;
  width: 28px;
  height: 28px;
  border: 0;
  border-radius: 50%;
  padding: 0;
  color: var(--task-primary);
  background: #16897a1c;
  font-size: 0.63rem;
}
.round-action.stop {
  color: var(--task-danger);
  background: #c241411a;
}
.progress {
  height: 3px;
  overflow: hidden;
  margin-top: 8px;
  border-radius: 2px;
  background: var(--paper-soft);
}
.progress span {
  display: block;
  width: 40%;
  height: 100%;
  background: var(--task-blue);
  animation: slide 1.4s linear infinite;
}
.task-card-actions {
  justify-content: flex-end;
  gap: 1px;
  margin-top: 6px;
  border-top: 1px dashed var(--line);
  padding-top: 5px;
}
.task-card-actions > button,
.related-menu > summary {
  display: grid;
  place-items: center;
  width: 26px;
  height: 25px;
  border: 0;
  border-radius: 6px;
  padding: 0;
  color: var(--ink-faint);
  background: transparent;
  font-size: 12px;
  cursor: pointer;
}
.task-card-actions > button:hover,
.related-menu > summary:hover {
  color: var(--ink);
  background: var(--paper-soft);
}
.task-card-actions > button.danger:hover {
  color: var(--task-danger);
  background: #c2414113;
}
.scheduled-line {
  margin: 4px 0 0 41px;
  color: var(--task-amber);
  font-size: 10px;
}
.task-empty {
  display: grid;
  place-items: center;
  align-content: center;
  gap: 8px;
  min-height: 220px;
  margin: 10px 14px;
  border: 1px dashed var(--line);
  border-radius: 9px;
  color: var(--ink-faint);
  font-size: 12px;
}
.task-empty span {
  font-size: 30px;
}
.action-menu {
  position: relative;
}
.action-menu summary {
  list-style: none;
}
.action-menu summary::-webkit-details-marker {
  display: none;
}
.action-popover {
  position: absolute;
  z-index: 20;
  top: calc(100% + 5px);
  right: 0;
  min-width: 190px;
  border: 1px solid var(--line);
  border-radius: 9px;
  padding: 5px;
  background: #fff;
  box-shadow: 0 10px 30px #102a3825;
}
.action-popover button {
  display: block;
  width: 100%;
  border: 0;
  border-radius: 6px;
  padding: 8px 10px;
  color: var(--ink);
  background: transparent;
  text-align: left;
  font-size: 12px;
}
.action-popover button:hover {
  background: var(--paper-soft);
}
.task-graph {
  min-height: 0;
  overflow: auto;
  padding: 8px 14px 16px;
}
.graph-help {
  margin-bottom: 8px;
  color: var(--ink-faint);
  font-size: 10.5px;
}
.graph-canvas {
  min-width: 430px;
  display: grid;
  align-content: start;
  gap: 8px;
  border-radius: 10px;
  padding: 14px;
  background-image: radial-gradient(#b9c2c8 0.65px, transparent 0.65px);
  background-size: 16px 16px;
}
.graph-node {
  position: relative;
  display: grid;
  grid-template-columns: 30px minmax(150px, 1fr) auto 27px;
  align-items: center;
  gap: 8px;
  max-width: 620px;
  border: 1px solid var(--line);
  border-radius: 9px;
  padding: 8px;
  background: #fff;
  box-shadow: 0 2px 8px #102a3810;
  cursor: pointer;
}
.graph-node .kind-icon {
  width: 30px;
  height: 30px;
}
.graph-node div {
  min-width: 0;
  display: grid;
}
.graph-node strong {
  overflow: hidden;
  font-size: 12px;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.graph-node small {
  color: var(--ink-faint);
  font-size: 10px;
}
.graph-node > button {
  width: 26px;
  height: 26px;
  border: 0;
  border-radius: 50%;
  color: var(--task-primary);
  background: #16897a1c;
}
.graph-line {
  position: absolute;
  top: 50%;
  left: -24px;
  width: 24px;
  border-top: 1px solid #aeb9bf;
}
.quick-composer {
  z-index: 2;
  display: grid;
  gap: 3px;
  margin: 8px 14px 14px;
  border: 1px solid var(--line);
  border-radius: 14px;
  padding: 9px 10px 8px;
  background: #fff;
  box-shadow: 0 5px 16px #102a3812;
}
.quick-composer:focus-within {
  border-color: var(--task-primary);
  box-shadow:
    0 0 0 3px #16897a15,
    0 5px 16px #102a3812;
}
.quick-composer textarea {
  width: 100%;
  min-height: 48px;
  box-sizing: border-box;
  border: 0;
  outline: 0;
  padding: 2px;
  color: var(--ink);
  background: transparent;
  font: 13px/1.45 inherit;
  resize: vertical;
}
.composer-tools {
  min-width: 0;
  gap: 4px;
  overflow-x: auto;
}
.composer-tools button {
  position: relative;
  flex: none;
  min-width: 28px;
  height: 28px;
  border: 0;
  border-radius: 6px;
  padding: 0 6px;
  color: var(--ink-soft);
  background: transparent;
}
.composer-tools button:hover {
  background: var(--paper-soft);
}
.composer-tools button b {
  position: absolute;
  top: -3px;
  right: -3px;
  min-width: 13px;
  border-radius: 8px;
  padding: 1px 3px;
  color: #fff;
  background: var(--task-primary);
  font-size: 8px;
}
.composer-tools select,
.model-input {
  flex: none;
  height: 27px;
  max-width: 110px;
  border: 0;
  border-radius: 6px;
  padding: 0 6px;
  color: var(--ink-soft);
  background: var(--paper-soft);
  font-size: 10.5px;
}
.model-input {
  width: 94px;
}
.compact-check {
  flex: none;
  display: flex;
  align-items: center;
  gap: 3px;
  color: var(--ink-soft);
  font-size: 10px;
}
.compact-check input {
  margin: 0;
}
.composer-spacer {
  flex: 1;
  min-width: 4px;
}
.composer-tools .quick-submit {
  width: 31px;
  border-radius: 50%;
  color: #fff;
  background: var(--task-primary);
  font-size: 17px;
}
.modal-backdrop {
  position: fixed;
  z-index: 1000;
  inset: 0;
  display: grid;
  place-items: center;
  padding: 24px;
  background: #0d1b24a6;
}
.modal-top {
  z-index: 1020;
}
.task-modal {
  display: grid;
  grid-template-rows: auto minmax(0, 1fr) auto;
  width: min(620px, calc(100vw - 32px));
  max-height: min(760px, calc(100vh - 40px));
  overflow: hidden;
  border: 1px solid var(--line);
  border-radius: 14px;
  color: var(--ink);
  background: #fff;
  box-shadow: 0 24px 80px #06121a55;
}
.task-modal > header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 16px;
  border-bottom: 1px solid var(--line);
  padding: 13px 16px;
}
.task-modal > header div {
  min-width: 0;
}
.task-modal > header span {
  color: var(--task-primary);
  font-size: 10px;
  font-weight: 800;
  letter-spacing: 0.08em;
  text-transform: uppercase;
}
.task-modal > header h3 {
  overflow: hidden;
  margin: 2px 0 0;
  font-size: 16px;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.task-modal > header > button {
  width: 30px;
  height: 30px;
  border: 0;
  border-radius: 7px;
  color: var(--ink-soft);
  background: var(--paper-soft);
  font-size: 18px;
}
.modal-body {
  min-height: 0;
  overflow-y: auto;
  padding: 16px;
}
.task-modal > footer {
  display: flex;
  align-items: center;
  justify-content: flex-end;
  gap: 8px;
  border-top: 1px solid var(--line);
  padding: 11px 16px;
}
.task-modal > footer span {
  flex: 1;
}
.task-modal button {
  cursor: pointer;
}
.task-modal > footer button,
.detail-actions button,
.relation-picker button,
.tail-button {
  min-height: 34px;
  border: 1px solid var(--line);
  border-radius: 8px;
  padding: 0 13px;
  color: var(--ink);
  background: #fff;
  font-size: 12px;
}
.task-modal button.primary,
.task-modal > footer button.primary {
  border-color: var(--task-primary);
  color: #fff;
  background: var(--task-primary);
}
.danger-text {
  color: var(--task-danger) !important;
}
.form-grid {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 13px;
}
.form-grid label,
.small-modal .modal-body label {
  display: grid;
  gap: 6px;
  color: var(--ink-soft);
  font-size: 11px;
  font-weight: 650;
}
.form-grid .wide {
  grid-column: 1 / -1;
}
.form-grid input,
.form-grid select,
.form-grid textarea,
.small-modal input,
.small-modal textarea {
  width: 100%;
  box-sizing: border-box;
  border: 1px solid var(--line);
  border-radius: 8px;
  padding: 9px 10px;
  outline: 0;
  color: var(--ink);
  background: #fff;
  font: inherit;
}
.form-grid input:focus,
.form-grid select:focus,
.form-grid textarea:focus,
.small-modal input:focus,
.small-modal textarea:focus {
  border-color: var(--task-primary);
  box-shadow: 0 0 0 3px #16897a17;
}
.relation-picker {
  justify-content: space-between;
  border: 1px solid var(--line);
  border-radius: 8px;
  padding: 9px 10px;
}
.relation-picker span,
.switch-row span {
  display: grid;
  gap: 2px;
}
.relation-picker small,
.switch-row small {
  color: var(--ink-faint);
  font-weight: 400;
}
.switch-row {
  grid-template-columns: auto 1fr !important;
  align-items: center;
  gap: 10px !important;
  border: 1px solid var(--line);
  border-radius: 8px;
  padding: 9px 10px;
}
.switch-row input {
  width: auto;
}
.form-error {
  margin: 0;
  color: var(--task-danger);
  font-size: 11px;
}
.small-modal {
  width: min(460px, calc(100vw - 32px));
}
.small-modal .modal-body {
  display: grid;
  gap: 10px;
}
.small-modal .modal-body p {
  margin: 0;
  color: var(--ink-soft);
  font-size: 12px;
  line-height: 1.55;
}
.prerequisite-modal {
  width: min(520px, calc(100vw - 32px));
}
.prerequisite-list {
  display: grid;
  align-content: start;
  gap: 5px;
}
.prerequisite-list > label {
  display: flex;
  align-items: center;
  gap: 10px;
  border: 1px solid var(--line);
  border-radius: 8px;
  padding: 9px 10px;
}
.prerequisite-list > label span {
  min-width: 0;
  display: grid;
  gap: 2px;
}
.prerequisite-list b {
  overflow: hidden;
  font-size: 12px;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.prerequisite-list small {
  color: var(--ink-faint);
  font-size: 10px;
}
.detail-backdrop {
  padding: 14px;
}
.detail-modal {
  width: min(780px, calc(100vw - 28px));
  height: min(720px, calc(100vh - 28px));
  max-height: none;
}
.detail-modal {
  grid-template-rows: auto minmax(0, 1fr);
}
.detail-scroll {
  min-height: 0;
  overflow-y: auto;
  display: grid;
  align-content: start;
  gap: 14px;
  padding: 18px 20px 28px;
}
.detail-summary {
  align-items: flex-start;
  justify-content: space-between;
  gap: 18px;
}
.detail-summary > div:first-child {
  min-width: 0;
}
.detail-summary em {
  margin-left: 7px;
  color: var(--task-primary);
  font-size: 11px;
  font-style: normal;
}
.detail-summary h4 {
  margin: 9px 0 0;
  font-size: 20px;
}
.detail-summary p {
  margin: 6px 0 0;
  color: var(--ink-soft);
}
.detail-actions {
  max-width: 360px;
  flex-wrap: wrap;
  justify-content: flex-end;
  gap: 6px;
}
.detail-actions button {
  min-height: 31px;
  padding: 0 10px;
}
.detail-section {
  border: 1px solid var(--line);
  border-radius: 10px;
  padding: 13px;
  background: #fff;
}
.detail-section h5 {
  margin: 0 0 10px;
  font-size: 12px;
}
.detail-section h5 small {
  margin-left: 5px;
  color: var(--ink-faint);
  font-weight: 400;
}
.detail-section dl {
  display: grid;
  grid-template-columns: repeat(3, minmax(0, 1fr));
  gap: 11px 18px;
  margin: 0;
}
.detail-section dl div {
  min-width: 0;
  display: grid;
  gap: 2px;
}
.detail-section dt {
  color: var(--ink-faint);
  font-size: 9.5px;
}
.detail-section dd {
  overflow-wrap: anywhere;
  margin: 0;
  font-size: 11px;
}
.path-value {
  font-family: Consolas, monospace;
}
.task-content,
.definition-json {
  overflow: auto;
  max-height: 260px;
  margin: 0;
  border-radius: 8px;
  padding: 11px;
  color: var(--ink);
  background: var(--paper-soft);
  font:
    11px/1.55 Consolas,
    monospace;
  white-space: pre-wrap;
  overflow-wrap: anywhere;
}
.definition-json {
  max-height: 360px;
}
.run-list {
  display: grid;
  gap: 6px;
}
.run-list > div {
  display: grid;
  grid-template-columns: minmax(100px, 1fr) auto auto;
  gap: 8px;
  border-bottom: 1px solid var(--line);
  padding: 6px 0;
  font-size: 10.5px;
}
.run-list small {
  grid-column: 1 / -1;
  color: var(--ink-faint);
  overflow-wrap: anywhere;
}
.section-empty {
  color: var(--ink-faint);
  font-size: 11px;
}
.section-title {
  justify-content: space-between;
}
.section-title h5 {
  margin: 0;
}
.section-title button {
  width: 28px;
  height: 28px;
  border: 0;
  border-radius: 6px;
  color: var(--ink-soft);
  background: var(--paper-soft);
}
.run-log-actions {
  display: flex;
  gap: 6px;
  align-items: center;
}
.run-log-actions select {
  max-width: 260px;
}
.section-title .run-log-actions button {
  width: auto;
  padding: 0 10px;
}
.download-progress {
  margin: 6px 0;
  color: var(--ink-soft);
  font-size: 10.5px;
}
.task-log {
  height: 360px;
  overflow: auto;
  border-radius: 8px;
  padding: 11px 12px;
  color: #d0d5dd;
  background: #101828;
  font-family: Consolas, monospace;
}
.task-log .file-log-text {
  margin: 0;
  white-space: pre-wrap;
  overflow-wrap: anywhere;
}
.task-log > small {
  display: block;
  margin-top: 5px;
  color: #98a2b3;
  font-size: 9.5px;
}
.task-log pre {
  margin: 2px 0 7px;
  color: #d0d5dd;
  font:
    11px/1.45 Consolas,
    monospace;
  white-space: pre-wrap;
  overflow-wrap: anywhere;
}
.task-log pre.stderr {
  color: #f9a8a8;
}
.task-log p {
  color: #98a2b3;
  font-size: 11px;
}
.log-warning {
  margin: 0 0 7px;
  color: var(--task-amber);
  font-size: 10.5px;
}
.tail-button {
  display: block;
  min-height: 29px;
  margin: 7px 0 0 auto;
}
@keyframes blink {
  50% {
    opacity: 0.25;
  }
}
@keyframes spin {
  to {
    transform: rotate(360deg);
  }
}
@keyframes slide {
  from {
    margin-left: -40%;
  }
  to {
    margin-left: 100%;
  }
}
@media (max-width: 700px) {
  .task-center {
    min-height: 620px;
  }
  .toolbar-row {
    align-items: flex-start;
  }
  .task-card-top {
    flex-wrap: wrap;
  }
  .task-main {
    order: 1;
    flex-basis: calc(100% - 44px);
  }
  .status-chip {
    order: 2;
    margin-left: 41px;
  }
  .round-action {
    order: 2;
  }
  .form-grid {
    grid-template-columns: 1fr;
  }
  .form-grid .wide {
    grid-column: auto;
  }
  .detail-summary {
    display: grid;
  }
  .detail-actions {
    justify-content: flex-start;
  }
  .detail-section dl {
    grid-template-columns: repeat(2, minmax(0, 1fr));
  }
  .run-list > div {
    grid-template-columns: 1fr;
  }
  .run-list small {
    grid-column: auto;
  }
}
</style>
