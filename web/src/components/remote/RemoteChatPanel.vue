<script setup lang="ts">
import { computed, inject, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'

import {
  remoteRPCKey,
  type RemoteAIApproval,
  type RemoteAIAttachment,
  type RemoteAIEvent,
  type RemoteAIGoal,
  type RemoteAIProviderRun,
  type RemoteAIToolRun,
  type RemoteAIUsage,
  type RemoteAIWorkspaceMode,
  type RemoteChatMessage,
  type RemoteConversation,
  type RemoteConversationDetail,
  type RemoteConversationEventPage,
  type RemoteFileEntry,
  type RemoteGenerationAttachResult,
  type RemoteRPCPage,
  type RemoteRPCStreamHandle,
} from '@/remote/rpcTypes'
import {
  mergeRemoteDelta,
  RemoteDeltaGapError,
  RemoteIndexedCache,
  remoteCachePartition,
  type RemoteChange,
  type RevisionedRecord,
  type SyncState,
} from '@/remote/cache'
import { agentEventConnectionKey } from '@/remote/agentEvents'
import { isRemoteAIEvent } from '@/remote/aiContract'
import { PaginationBudget, PaginationBudgetExceededError } from '@/remote/paginationBudget'
import type { RemoteAgentCapabilities } from '@/remote/peerClient'
import { useAuthStore } from '@/stores/auth'

interface CompatibleRemoteChange<T extends RevisionedRecord> {
  sequence?: number
  deleted?: boolean
  tombstone?: boolean
  operation?: string
  id?: string
  revision?: number
  value?: T
  item?: T
  record?: T
}

interface RemoteJournalPage<T extends RevisionedRecord> extends RemoteRPCPage<T> {
  changes?: CompatibleRemoteChange<T>[]
  tombstones?: CompatibleRemoteChange<T>[]
}

interface RemoteMessageContentChunk {
  conversationId: string
  messageId: string
  field: 'content' | 'reasoning'
  offset: number
  nextOffset: number
  totalBytes: number
  hasMore: boolean
  revision: number
  content: string
}

interface RemoteCurrentGenerationState {
  conversationId: string
  generationId?: string
  pendingApproval?: RemoteAIApproval
}

const props = withDefaults(
  defineProps<{
    deviceId: string
    projectId: string
    protocolVersion: number
    capabilityVersion: string
    /** Allows the browser to answer a device Agent's tool-approval request. */
    toolsAuthorized?: boolean
    /** Allows selected browser files to be imported into the remote project. */
    attachmentsAuthorized?: boolean
    /** Desktop workspace mode renders the conversation catalog in the app sidebar. */
    showConversationList?: boolean
    workspace?: boolean
  }>(),
  { showConversationList: true },
)
const emit = defineEmits<{
  sidebarState: [
    value: {
      conversations: RemoteConversation[]
      activeId: string
      loading: boolean
      hasMore: boolean
    },
  ]
}>()

const rpc = inject(remoteRPCKey)
if (!rpc) throw new Error('Remote RPC provider is required')
const agentEvents = inject(agentEventConnectionKey, null)
const auth = useAuthStore()
const cache = new RemoteIndexedCache()

const conversations = ref<RemoteConversation[]>([])
const conversationCursor = ref<string | null>(null)
const active = ref<RemoteConversation | null>(null)
const messages = ref<RemoteChatMessage[]>([])
const olderCursor = ref<string | null>(null)
const loading = ref(true)
const loadingMessages = ref(false)
const sending = ref(false)
const prompt = ref('')
const errorMessage = ref('')
const capabilities = ref<RemoteAgentCapabilities | null>(null)
const subagents = ref<RemoteConversation[]>([])
const pendingAttachments = ref<RemoteAIAttachment[]>([])
const attachmentsUploading = ref(false)
const selectedWorkspaceMode = ref<RemoteAIWorkspaceMode>('readOnly')
const workspaceToolsEnabled = ref(false)
const pendingApproval = ref<RemoteAIApproval | null>(null)
const goalDraft = ref('')
const collaborationPending = ref(false)
const queuedNotice = ref('')
const conversationWatermark = ref(0)
const messageWatermark = ref(0)
const eventWatermark = ref(0)
const activeGenerationId = ref('')
const generationActive = computed(() => sending.value || activeGenerationId.value.length > 0)
let generationAttach: RemoteRPCStreamHandle<RemoteGenerationAttachResult> | undefined
let generationAttachHealthy = false
let generationAttachStarting: Promise<void> | undefined
let terminalRefresh: Promise<void> | undefined
let generationRecoveryAvailable: boolean | undefined
let generationAttachEpoch = 0
let legacyEventReplay: Promise<void> | undefined
let invalidEventRecovery: Promise<void> | undefined
let generationAttachTarget = ''
let sendRequestEpoch = 0
const promptBytes = computed(() => new TextEncoder().encode(prompt.value.trim()).byteLength)
const projectContext = computed(() => ({ projectId: props.projectId }))
const canUseWorkspaceTools = computed(() => {
  const current = capabilities.value
  return (
    props.toolsAuthorized === true &&
    current?.features['ai.workspaceTools'] === true &&
    current.features['ai.permissionModes'] === true &&
    (current.featureVersions.ai ?? 0) >= 4
  )
})
const supportsCollaboration = computed(() => {
  const current = capabilities.value
  return (
    current?.features['ai.planMode'] === true &&
    current.features['ai.todo'] === true &&
    current.features['ai.subagents'] === true &&
    (current.featureVersions.ai ?? 0) >= 7
  )
})
const supportsGoal = computed(
  () =>
    supportsCollaboration.value &&
    capabilities.value?.features['ai.goal'] === true &&
    (capabilities.value?.featureVersions.ai ?? 0) >= 8,
)
const supportsInbox = computed(
  () =>
    capabilities.value?.features['ai.agentLoop'] === true &&
    capabilities.value?.features['ai.durableInbox'] === true &&
    (capabilities.value?.featureVersions.ai ?? 0) >= 6,
)
const hasAgentState = computed(
  () =>
    active.value?.planModeActive === true ||
    (active.value?.todos?.length ?? 0) > 0 ||
    subagents.value.length > 0,
)
const canSubmit = computed(
  () =>
    !sending.value &&
    !attachmentsUploading.value &&
    (prompt.value.trim().length > 0 || pendingAttachments.value.length > 0) &&
    promptBytes.value <= 32 * 1024,
)

const releaseGenerationControls = () => {
  activeGenerationId.value = ''
  pendingApproval.value = null
  sending.value = false
}
const messageList = ref<HTMLElement | null>(null)
const followMessageTail = ref(true)
let expectedMessageScrollTop: number | undefined
let scrollTailScheduled = false
const attachmentInput = ref<HTMLInputElement | null>(null)
let initialized = false
let disposed = false
let reconnectSyncing = false
let removeAgentEventListener: (() => void) | undefined
let removeAgentResetListener: (() => void) | undefined
let subagentRequestEpoch = 0
const hydratingMessageBodies = new Set<string>()
const textEncoder = new TextEncoder()
const messageContentChunkBytes = 16 * 1024
const maximumMessageContentBytes = 1024 * 1024
const maximumMessageContentChunks = 128
const legacyEventReplayLimits = {
  maximumPages: 16,
  maximumItems: 1_000,
  maximumBytes: 4 * 1024 * 1024,
  maximumCursorBytes: 512,
  maximumDurationMilliseconds: 10_000,
} as const

const cachePartition = (resource: 'conversations' | `messages:${string}`) => {
  const userId = auth.user?.id
  return userId
    ? remoteCachePartition(
        {
          userId,
          deviceId: props.deviceId,
          projectId: props.projectId,
          protocolVersion: props.protocolVersion,
          capabilityVersion: props.capabilityVersion,
        },
        resource,
      )
    : null
}

const readReferencedMessageBody = async (
  conversationId: string,
  message: RemoteChatMessage,
  reference: NonNullable<RemoteChatMessage['contentRef']>,
  expectedField: 'content' | 'reasoning',
) => {
  if (
    !Number.isSafeInteger(reference.totalBytes) ||
    reference.totalBytes < 1 ||
    reference.totalBytes > maximumMessageContentBytes ||
    reference.field !== expectedField
  ) {
    throw new Error('设备返回了无效的消息正文引用。')
  }
  let offset = 0
  let decodedBytes = 0
  const chunks: string[] = []
  for (let count = 0; count < maximumMessageContentChunks; count += 1) {
    if (
      disposed ||
      active.value?.id !== conversationId ||
      !messages.value.some(
        (current) => current.id === message.id && current.revision === message.revision,
      )
    ) {
      throw new Error('消息正文读取已取消。')
    }
    const chunk = await rpc.call<RemoteMessageContentChunk>(
      'conversation.message.content',
      {
        conversationId,
        messageId: message.id,
        field: reference.field,
        offset,
        maxBytes: messageContentChunkBytes,
      },
      projectContext.value,
    )
    const contentBytes =
      typeof chunk.content === 'string' ? textEncoder.encode(chunk.content).byteLength : -1
    if (
      chunk.conversationId !== conversationId ||
      chunk.messageId !== message.id ||
      chunk.field !== reference.field ||
      typeof chunk.content !== 'string' ||
      !Number.isSafeInteger(chunk.offset) ||
      chunk.offset !== offset ||
      !Number.isSafeInteger(chunk.nextOffset) ||
      chunk.nextOffset < 0 ||
      chunk.nextOffset !== offset + contentBytes ||
      !Number.isSafeInteger(chunk.totalBytes) ||
      chunk.totalBytes !== reference.totalBytes ||
      typeof chunk.hasMore !== 'boolean' ||
      !Number.isSafeInteger(chunk.revision) ||
      chunk.revision !== message.revision ||
      chunk.nextOffset > chunk.totalBytes
    ) {
      throw new Error('设备返回的消息正文分块无效。')
    }
    chunks.push(chunk.content)
    decodedBytes += contentBytes
    if (!chunk.hasMore) {
      if (chunk.nextOffset !== chunk.totalBytes || decodedBytes !== chunk.totalBytes) {
        throw new Error('设备消息正文长度在读取期间发生了变化。')
      }
      return chunks.join('')
    }
    if (chunk.nextOffset <= offset) throw new Error('设备消息正文游标未前进。')
    offset = chunk.nextOffset
  }
  throw new Error('设备消息正文分块数量超过安全限制。')
}

const hydrateReferencedMessageBodies = async (
  conversationId: string,
  source: readonly RemoteChatMessage[],
) => {
  const pending = source
    .filter((message) => message.contentRef !== undefined || message.reasoningRef !== undefined)
    .sort((left, right) => right.sequence - left.sequence)
  for (const message of pending) {
    if (disposed || active.value?.id !== conversationId) return
    const key = `${conversationId}\u0000${message.id}\u0000${message.revision}`
    if (hydratingMessageBodies.has(key)) continue
    hydratingMessageBodies.add(key)
    try {
      const content = message.contentRef
        ? await readReferencedMessageBody(conversationId, message, message.contentRef, 'content')
        : message.content
      const reasoning = message.reasoningRef
        ? await readReferencedMessageBody(
            conversationId,
            message,
            message.reasoningRef,
            'reasoning',
          )
        : message.reasoning
      if (disposed || active.value?.id !== conversationId) return
      const index = messages.value.findIndex(
        (current) => current.id === message.id && current.revision === message.revision,
      )
      if (index < 0) continue
      const resolved: RemoteChatMessage = { ...messages.value[index]!, content, reasoning }
      delete resolved.contentRef
      delete resolved.reasoningRef
      messages.value.splice(index, 1, resolved)
      scrollToMessageTail()
    } catch {
      // Keep the bounded prefix visible. A later snapshot refresh retries this
      // read without failing the healthy AI generation or encrypted Link.
    } finally {
      hydratingMessageBodies.delete(key)
    }
  }
}

const hydrateConversations = async () => {
  const partition = cachePartition('conversations')
  if (!partition) return
  try {
    const state = await cache.read<RemoteConversation>(partition)
    conversationWatermark.value = state.highWatermark
    if (state.records.length) {
      conversations.value = state.records.sort(
        (left, right) => Date.parse(right.updatedAt) - Date.parse(left.updatedAt),
      )
      loading.value = false
    }
  } catch {
    // A cache failure must not block the live E2EE query.
  }
}

const persistConversations = async (highWatermark = conversationWatermark.value) => {
  const partition = cachePartition('conversations')
  if (!partition) return
  conversationWatermark.value = highWatermark
  await cache
    .replace(partition, {
      records: conversations.value,
      highWatermark: conversationWatermark.value,
    })
    .catch(() => undefined)
}

const hydrateMessages = async (conversationId: string) => {
  const partition = cachePartition(`messages:${conversationId}`)
  if (!partition) return
  try {
    const state = await cache.read<RemoteChatMessage>(partition)
    if (active.value?.id !== conversationId) return
    messages.value = state.records.sort((left, right) => left.sequence - right.sequence)
    messageWatermark.value = state.highWatermark
    void hydrateReferencedMessageBodies(conversationId, messages.value)
  } catch {
    // A cache failure must not block the live E2EE query.
  }
}

const sortedMessages = computed(() => [...messages.value].sort((a, b) => a.sequence - b.sequence))

const rootConversations = computed(() =>
  conversations.value.filter((conversation) => conversation.subagent === undefined),
)

watch(
  [rootConversations, active, loading, conversationCursor],
  () =>
    emit('sidebarState', {
      conversations: rootConversations.value,
      activeId: active.value?.id ?? '',
      loading: loading.value,
      hasMore: Boolean(conversationCursor.value),
    }),
  { immediate: true, deep: true },
)

const scrollToMessageTail = (force = false) => {
  if ((!force && !followMessageTail.value) || scrollTailScheduled) return
  scrollTailScheduled = true
  void nextTick(() => {
    scrollTailScheduled = false
    const target = messageList.value
    if (!target || (!force && !followMessageTail.value)) return
    target.scrollTop = target.scrollHeight
    expectedMessageScrollTop = target.scrollTop
  })
}

const handleMessageScroll = () => {
  const target = messageList.value
  if (!target) return
  const distance = target.scrollHeight - target.clientHeight - target.scrollTop
  if (distance <= 28) {
    followMessageTail.value = true
  } else if (
    expectedMessageScrollTop === undefined ||
    Math.abs(target.scrollTop - expectedMessageScrollTop) > 2
  ) {
    followMessageTail.value = false
  }
  expectedMessageScrollTop = undefined
}

watch(
  () =>
    messages.value
      .map((message) => `${message.id}:${message.revision}:${message.status}`)
      .join('|'),
  () => scrollToMessageTail(),
  { flush: 'post' },
)

const applyConversation = async (conversation: RemoteConversation) => {
  conversations.value = mergeById(conversations.value, [conversation]).sort(
    (left, right) => Date.parse(right.updatedAt) - Date.parse(left.updatedAt),
  )
  if (active.value?.id === conversation.id) active.value = conversation
  await persistConversations()
}

const loadSubagents = async (conversationId = active.value?.id) => {
  if (!conversationId || !supportsCollaboration.value) {
    subagents.value = []
    return
  }
  const epoch = ++subagentRequestEpoch
  try {
    const page = await rpc.call<RemoteRPCPage<RemoteConversation>>(
      'conversation.subagents.list',
      { conversationId, limit: 50 },
      projectContext.value,
    )
    if (disposed || epoch !== subagentRequestEpoch || active.value?.id !== conversationId) return
    subagents.value = [...(page.items ?? [])].sort(
      (left, right) => Date.parse(right.updatedAt) - Date.parse(left.updatedAt),
    )
  } catch (error) {
    if (!disposed && epoch === subagentRequestEpoch && active.value?.id === conversationId) {
      errorMessage.value = error instanceof Error ? error.message : '无法读取子 Agent 状态。'
    }
  }
}

const loadCapabilities = async () => {
  try {
    capabilities.value = await rpc.getCapabilities()
    if (!canUseWorkspaceTools.value) workspaceToolsEnabled.value = false
    if (active.value && supportsCollaboration.value) await loadSubagents(active.value.id)
  } catch {
    // The basic v2 transcript remains useful even when a capability probe is
    // temporarily unavailable. Individual advanced actions stay hidden.
  }
}

const emptyUsage = (): RemoteAIUsage => ({
  inputTokens: 0,
  outputTokens: 0,
  reasoningTokens: 0,
  cachedInputTokens: 0,
  totalTokens: 0,
})

const emptyProviderRun = (): RemoteAIProviderRun => ({
  provider: '',
  model: '',
  providerRequestId: '',
  finishReason: '',
  attemptCount: 0,
})

const mergeById = <T extends RevisionedRecord>(current: T[], incoming: T[]) => {
  const merged = new Map(current.map((item) => [item.id, item]))
  for (const item of incoming) {
    const previous = merged.get(item.id)
    if (!previous || item.revision >= previous.revision) merged.set(item.id, item)
  }
  return [...merged.values()]
}

const safeWatermark = (value: unknown, fallback: number) =>
  typeof value === 'number' && Number.isSafeInteger(value) && value >= 0 ? value : fallback

const normalizeChanges = <T extends RevisionedRecord>(page: RemoteJournalPage<T>) => {
  const changes: RemoteChange<T>[] = []
  const append = (change: CompatibleRemoteChange<T>, forceDeleted = false) => {
    const sequence = safeWatermark(change.sequence, -1)
    const deleted =
      forceDeleted ||
      change.deleted === true ||
      change.tombstone === true ||
      change.operation === 'delete'
    const value = change.value ?? change.item ?? change.record
    const fallback =
      deleted && change.id
        ? ({ id: change.id, revision: safeWatermark(change.revision, sequence) } as T)
        : undefined
    if (sequence < 0 || (!value && !fallback)) return
    changes.push({ sequence, deleted, value: value ?? fallback! })
  }
  for (const change of page.changes ?? []) append(change)
  for (const tombstone of page.tombstones ?? []) append(tombstone, true)
  return changes
}

const hasJournalPayload = <T extends RevisionedRecord>(page: RemoteJournalPage<T>) =>
  Array.isArray(page.changes) || Array.isArray(page.tombstones)

const replaceCachedState = async <T extends RevisionedRecord>(
  partition: string | null,
  state: SyncState<T>,
) => {
  if (partition) await cache.replace(partition, state).catch(() => undefined)
  return state
}

const reconcilePage = async <T extends RevisionedRecord>(
  partition: string | null,
  current: SyncState<T>,
  page: RemoteJournalPage<T>,
) => {
  const items = Array.isArray(page.items) ? page.items : []
  const highWatermark = safeWatermark(page.highWatermark, current.highWatermark)
  if (!hasJournalPayload(page)) {
    const records =
      page.resetRequired || current.highWatermark === 0
        ? items
        : items.length > 0
          ? mergeById(current.records, items)
          : current.records
    return replaceCachedState(partition, { records, highWatermark })
  }

  const delta = {
    changes: normalizeChanges(page),
    highWatermark,
    resetRequired: page.resetRequired,
  }
  let next =
    partition && RemoteIndexedCache.supported()
      ? await cache.merge<T>(partition, delta)
      : mergeRemoteDelta(current, delta)

  // A reset may carry a current page plus journal metadata. Page records are
  // safe to upsert by revision; history continuations never advance the
  // journal watermark and are handled separately below.
  if (items.length > 0) {
    next = { ...next, records: mergeById(next.records, items) }
    await replaceCachedState(partition, next)
  }
  return next
}

const applyConversationState = async (state: SyncState<RemoteConversation>) => {
  const previousActiveID = active.value?.id
  conversations.value = state.records.sort(
    (left, right) => Date.parse(right.updatedAt) - Date.parse(left.updatedAt),
  )
  conversationWatermark.value = state.highWatermark
  if (!previousActiveID) return
  const refreshed = conversations.value.find((conversation) => conversation.id === previousActiveID)
  if (refreshed) {
    active.value = refreshed
    return
  }
  const partition = cachePartition(`messages:${previousActiveID}`)
  if (partition) await cache.clearPartition(partition).catch(() => undefined)
  active.value = null
  messages.value = []
  olderCursor.value = null
  messageWatermark.value = 0
}

const assertConversationBindings = (page: RemoteJournalPage<RemoteConversation>) => {
  const candidates = [
    ...(page.items ?? []),
    ...(page.changes ?? []).flatMap((change) => {
      const value = change.value ?? change.item ?? change.record
      return value ? [value] : []
    }),
  ]
  if (candidates.some((conversation) => conversation.projectId !== props.projectId)) {
    throw new Error('设备返回了不兼容的 AI 项目绑定。')
  }
}

const loadConversations = async (append = false, forceFull = false): Promise<void> => {
  loading.value = conversations.value.length === 0
  try {
    await rpc.connect()
    const input: Record<string, unknown> = {
      cursor: append ? conversationCursor.value : undefined,
      limit: 30,
    }
    if (!append) {
      input.afterSequence = forceFull ? 0 : conversationWatermark.value
      if (!forceFull && conversationWatermark.value > 0) {
        // Kept during the rollout so older agents still provide their safe
        // reset-or-unchanged behavior while newer agents use afterSequence.
        input.afterRevision = conversationWatermark.value
      }
    }
    const page = await rpc.call<RemoteJournalPage<RemoteConversation>>(
      'conversation.list',
      input,
      projectContext.value,
    )
    assertConversationBindings(page)
    if (append) {
      const state = {
        records: mergeById(conversations.value, page.items ?? []),
        highWatermark: conversationWatermark.value,
      }
      await replaceCachedState(cachePartition('conversations'), state)
      await applyConversationState(state)
    } else {
      const state = await reconcilePage(
        cachePartition('conversations'),
        { records: conversations.value, highWatermark: conversationWatermark.value },
        page,
      )
      await applyConversationState(state)
    }
    if (Object.hasOwn(page, 'nextCursor') || page.resetRequired) {
      conversationCursor.value = page.nextCursor ?? null
    }
    if (!active.value && rootConversations.value[0]) {
      await openConversation(rootConversations.value[0])
    }
  } catch (error) {
    if (error instanceof RemoteDeltaGapError && !append && !forceFull) {
      await loadConversations(false, true)
      return
    }
    errorMessage.value = error instanceof Error ? error.message : '无法读取设备对话。'
  } finally {
    loading.value = false
  }
}

const openConversation = async (conversation: RemoteConversation) => {
  if (conversation.projectId !== props.projectId) {
    errorMessage.value = '会话不属于当前项目，已拒绝打开。'
    return
  }
  await detachGenerationAttach()
  active.value = conversation
  messages.value = []
  subagents.value = []
  pendingApproval.value = null
  olderCursor.value = null
  messageWatermark.value = 0
  eventWatermark.value = 0
  activeGenerationId.value = ''
  followMessageTail.value = true
  await hydrateMessages(conversation.id)
  try {
    await loadConversationSnapshotAndAttach(conversation.id)
    scrollToMessageTail(true)
  } catch (error) {
    // Retain the v2 message-page fallback only when the snapshot endpoint
    // itself is unavailable; a healthy v3 flow always attaches from it.
    await loadMessages(false)
    errorMessage.value =
      error instanceof Error ? error.message : 'Unable to read device conversation.'
  }
}

const loadMessages = async (older: boolean, forceFull = false): Promise<void> => {
  if (!active.value) return
  const conversationID = active.value.id
  const viewport =
    older && messageList.value
      ? { height: messageList.value.scrollHeight, top: messageList.value.scrollTop }
      : undefined
  loadingMessages.value = true
  try {
    const input: Record<string, unknown> = {
      conversationId: conversationID,
      cursor: older ? olderCursor.value : undefined,
      limit: 50,
    }
    if (!older) {
      input.afterSequence = forceFull ? 0 : messageWatermark.value
      if (!forceFull && messageWatermark.value > 0) input.afterRevision = messageWatermark.value
    }
    const page = await rpc.call<RemoteJournalPage<RemoteChatMessage>>(
      'conversation.messages.before',
      input,
      projectContext.value,
    )
    const partition = cachePartition(`messages:${conversationID}`)
    if (older) {
      const state = {
        records: mergeById(messages.value, page.items ?? []),
        highWatermark: messageWatermark.value,
      }
      await replaceCachedState(partition, state)
      if (active.value?.id === conversationID) {
        messages.value = state.records.sort((left, right) => left.sequence - right.sequence)
      }
    } else {
      const state = await reconcilePage(
        partition,
        { records: messages.value, highWatermark: messageWatermark.value },
        page,
      )
      if (active.value?.id === conversationID) {
        messages.value = state.records.sort((left, right) => left.sequence - right.sequence)
        messageWatermark.value = state.highWatermark
      }
    }
    if (
      active.value?.id === conversationID &&
      (Object.hasOwn(page, 'nextCursor') || page.resetRequired)
    ) {
      olderCursor.value = page.nextCursor ?? null
    }
    if (active.value?.id === conversationID) {
      void hydrateReferencedMessageBodies(conversationID, messages.value)
    }
    if (viewport && active.value?.id === conversationID) {
      await nextTick()
      const target = messageList.value
      if (target) {
        target.scrollTop = viewport.top + (target.scrollHeight - viewport.height)
        expectedMessageScrollTop = target.scrollTop
      }
    }
  } catch (error) {
    if (
      error instanceof RemoteDeltaGapError &&
      !older &&
      !forceFull &&
      active.value?.id === conversationID
    ) {
      await loadMessages(false, true)
      return
    }
    errorMessage.value = error instanceof Error ? error.message : '无法读取历史消息。'
  } finally {
    loadingMessages.value = false
  }
}

const supportsGenerationRecovery = async (): Promise<boolean> => {
  if (generationRecoveryAvailable !== undefined) return generationRecoveryAvailable
  try {
    const available = capabilities.value ?? (await rpc.getCapabilities())
    capabilities.value = available
    generationRecoveryAvailable =
      available.features['ai.generationRecovery'] === true &&
      (available.featureVersions.ai ?? 0) >= 3
  } catch {
    // Do not probe an unknown long-lived method when capability discovery is
    // unavailable. The existing encrypted event-page route remains safe.
    generationRecoveryAvailable = false
  }
  return generationRecoveryAvailable
}

const detachGenerationAttach = async () => {
  generationAttachEpoch += 1
  generationAttachStarting = undefined
  generationAttachHealthy = false
  generationAttachTarget = ''
  const handle = generationAttach
  generationAttach = undefined
  if (handle) await handle.detach().catch(() => undefined)
}

const snapshotEventCursor = (detail: RemoteConversationDetail) =>
  safeWatermark(detail.snapshotEventHighWatermark, safeWatermark(detail.eventHighWatermark, 0))

const persistSnapshotMessages = async (conversationId: string) => {
  const partition = cachePartition(`messages:${conversationId}`)
  if (!partition) return
  await cache
    .replace(partition, {
      records: messages.value,
      highWatermark: messageWatermark.value,
    })
    .catch(() => undefined)
}

const refreshAfterTerminal = (conversationId: string) => {
  if (terminalRefresh) return terminalRefresh
  const current = (async () => {
    if (!disposed && active.value?.id === conversationId) {
      await refreshActiveConversation(conversationId, false)
    }
    if (!disposed) await loadConversations()
  })()
    .catch((error: unknown) => {
      if (!disposed) {
        errorMessage.value =
          error instanceof Error ? error.message : 'Unable to refresh completed conversation.'
      }
    })
    .finally(() => {
      if (terminalRefresh === current) terminalRefresh = undefined
    })
  terminalRefresh = current
  return current
}

const applyAttachResult = (
  handle: RemoteRPCStreamHandle<RemoteGenerationAttachResult>,
  conversationId: string,
) => {
  void handle.result.then(
    (result) => {
      if (generationAttach !== handle) return
      generationAttach = undefined
      generationAttachHealthy = false
      generationAttachTarget = ''
      if (result.resetRequired && !disposed && active.value?.id === conversationId) {
        void refreshActiveConversation(conversationId)
      }
    },
    () => {
      // A reconnect or the next compact hint reopens from the durable snapshot
      // cursor. Do not call conversation.cancel for an observer failure.
      if (generationAttach === handle) {
        generationAttach = undefined
        generationAttachHealthy = false
        generationAttachTarget = ''
      }
    },
  )
}

const ensureGenerationAttach = async (detail: RemoteConversationDetail): Promise<void> => {
  const generationId = detail.conversation.generationId
  const conversationId = detail.conversation.id
  if (
    disposed ||
    sending.value ||
    detail.conversation.state !== 'generating' ||
    !generationId ||
    active.value?.id !== conversationId
  ) {
    return
  }
  const target = `${conversationId}\u0000${generationId}`
  if (generationAttach && generationAttachHealthy && generationAttachTarget === target) return
  if (generationAttachStarting && generationAttachTarget === target) return generationAttachStarting
  if (generationAttach || generationAttachStarting) await detachGenerationAttach()
  const epoch = generationAttachEpoch
  generationAttachTarget = target
  const current = (async () => {
    if (!(await supportsGenerationRecovery())) return
    if (disposed || epoch !== generationAttachEpoch || active.value?.id !== conversationId) return
    const handle = rpc.startStream<RemoteAIEvent, RemoteGenerationAttachResult>(
      'conversation.generation.attach',
      {
        conversationId,
        generationId,
        afterSequence: snapshotEventCursor(detail),
      },
      applyStreamEvent,
      projectContext.value,
    )
    if (disposed || epoch !== generationAttachEpoch || active.value?.id !== conversationId) {
      await handle.detach().catch(() => undefined)
      return
    }
    generationAttach = handle
    generationAttachHealthy = true
    applyAttachResult(handle, conversationId)
  })().finally(() => {
    if (generationAttachStarting === current) generationAttachStarting = undefined
    if (!generationAttach && epoch === generationAttachEpoch) generationAttachTarget = ''
  })
  generationAttachStarting = current
  return current
}

const loadConversationSnapshotAndAttach = async (
  conversationId: string,
  attach = true,
): Promise<void> => {
  const observedGenerationId = activeGenerationId.value
  const detail = await rpc.call<RemoteConversationDetail>(
    'conversation.get',
    { conversationId, limit: 50 },
    projectContext.value,
  )
  if (disposed || active.value?.id !== conversationId) return
  if (detail.conversation.projectId !== props.projectId) {
    throw new Error('设备返回了不兼容的 AI 项目绑定。')
  }
  await applyConversation(detail.conversation)
  messages.value = [...(detail.messages ?? [])].sort(
    (left, right) => left.sequence - right.sequence,
  )
  olderCursor.value = detail.nextCursor ?? null
  messageWatermark.value = safeWatermark(detail.highWatermark, messageWatermark.value)
  eventWatermark.value = snapshotEventCursor(detail)
  if (detail.conversation.state === 'generating') {
    activeGenerationId.value = detail.conversation.generationId ?? ''
  } else if (
    !sending.value ||
    (observedGenerationId.length > 0 && activeGenerationId.value === observedGenerationId)
  ) {
    // A terminal snapshot is authoritative when the live terminal event was
    // lost or rejected because of an event gap. Do not let an older refresh
    // release a newer send that started while this request was in flight.
    releaseGenerationControls()
  }
  await persistSnapshotMessages(conversationId)
  void hydrateReferencedMessageBodies(conversationId, messages.value)
  if (supportsCollaboration.value) await loadSubagents(conversationId)
  if (attach) await ensureGenerationAttach(detail)
}

const refreshActiveConversation = async (
  conversationId: string,
  attach = true,
  forceFullFallback = false,
): Promise<void> => {
  try {
    await loadConversationSnapshotAndAttach(conversationId, attach)
  } catch {
    // Keep old Agents functional during rollout. Their messages endpoint is
    // still authoritative for the visible projection when conversation.get
    // cannot supply the v3 consistent cursor.
    if (!disposed && active.value?.id === conversationId) {
      await loadMessages(false, forceFullFallback)
    }
  }
}

const replayLegacyConversationEvents = (conversationId: string): Promise<void> => {
  if (legacyEventReplay) return legacyEventReplay
  const current = (async () => {
    let afterSequence = eventWatermark.value
    const budget = new PaginationBudget('旧版对话事件回放', legacyEventReplayLimits)
    try {
      while (!disposed && active.value?.id === conversationId) {
        budget.assertCanRequestPage()
        const page = await rpc.call<RemoteConversationEventPage>(
          'conversation.events',
          { conversationId, afterSequence, limit: 100 },
          projectContext.value,
        )
        if (disposed || active.value?.id !== conversationId) return
        const pageItems = page.items ?? []
        budget.admitPage(pageItems)
        if (page.resetRequired) {
          await refreshActiveConversation(conversationId)
          return
        }
        const before = afterSequence
        for (const item of [...pageItems].sort((left, right) => left.sequence - right.sequence)) {
          applyStreamEvent(item)
        }
        afterSequence = Math.max(
          eventWatermark.value,
          safeWatermark(page.nextSequence, eventWatermark.value),
        )
        if (!page.hasMore) return
        if (afterSequence <= before) {
          await refreshActiveConversation(conversationId)
          return
        }
      }
    } catch (error) {
      if (
        error instanceof PaginationBudgetExceededError &&
        !disposed &&
        active.value?.id === conversationId
      ) {
        errorMessage.value = `${error.message} 已改从权威快照恢复。`
        await refreshActiveConversation(conversationId)
        return
      }
      throw error
    }
  })().finally(() => {
    if (legacyEventReplay === current) legacyEventReplay = undefined
  })
  legacyEventReplay = current
  return current
}

const createConversation = async () => {
  try {
    const conversation = await rpc.call<RemoteConversation>(
      'conversation.create',
      { title: '新对话' },
      projectContext.value,
    )
    if (conversation.projectId !== props.projectId) {
      throw new Error('设备返回了不兼容的 AI 项目绑定。')
    }
    conversations.value = [conversation, ...conversations.value]
    await persistConversations()
    await openConversation(conversation)
    await loadConversations()
  } catch (error) {
    errorMessage.value = error instanceof Error ? error.message : '无法创建对话。'
  }
}

const deleteConversationItem = async (conversation: RemoteConversation | null = active.value) => {
  if (!conversation || !window.confirm(`删除对话“${conversation.title}”及其本地消息？`)) return
  try {
    const deletingActive = active.value?.id === conversation.id
    if (deletingActive) await detachGenerationAttach()
    await rpc.call(
      'conversation.delete',
      { conversationId: conversation.id, expectedRevision: conversation.revision },
      projectContext.value,
    )
    conversations.value = conversations.value.filter((item) => item.id !== conversation.id)
    const messagePartition = cachePartition(`messages:${conversation.id}`)
    if (messagePartition) {
      await cache
        .replace(messagePartition, { records: [], highWatermark: 0 })
        .catch(() => undefined)
    }
    if (deletingActive) {
      active.value = null
      messages.value = []
      olderCursor.value = null
    }
    await persistConversations()
    await loadConversations()
  } catch (error) {
    errorMessage.value = error instanceof Error ? error.message : '无法删除对话。'
  }
}

const deleteConversation = () => deleteConversationItem()

const nextMessageSequence = () =>
  messages.value.reduce((highest, message) => Math.max(highest, message.sequence), 0) + 1

const ensureStreamingMessage = (event: RemoteAIEvent) => {
  const index = messages.value.findIndex((message) => message.id === event.messageId)
  if (index >= 0) return index
  messages.value.push({
    id: event.messageId,
    revision: 1,
    sequence: nextMessageSequence(),
    role: 'assistant',
    content: '',
    status: 'streaming',
    errorCode: '',
    attachments: [],
    reasoning: '',
    toolRuns: [],
    usage: emptyUsage(),
    providerRun: emptyProviderRun(),
    createdAt: event.occurredAt,
    generationId: event.generationId,
  })
  return messages.value.length - 1
}

const applyCollaborationEvent = (event: RemoteAIEvent) => {
  const current = active.value
  if (!current) return false
  if (event.kind === 'chat.plan_mode.changed') {
    const next = {
      ...current,
      planModeActive:
        typeof event.payload.active === 'boolean' ? event.payload.active : current.planModeActive,
    }
    void applyConversation(next)
    return true
  }
  if (event.kind === 'chat.todo.updated') {
    const next = {
      ...current,
      todos: event.payload.todos ? [...event.payload.todos] : current.todos,
    }
    void applyConversation(next)
    return true
  }
  if (event.kind === 'chat.goal.changed') {
    if (event.payload.goal === null) {
      void applyConversation({ ...current, goal: undefined, goalArmed: false })
    } else if (event.payload.goal) {
      void applyConversation({ ...current, goal: event.payload.goal })
    } else {
      // Some state changes intentionally keep the event compact. Re-read the
      // authoritative conversation snapshot instead of guessing a revision.
      void refreshActiveConversation(event.conversationId)
    }
    return true
  }
  if (
    event.kind === 'chat.subagent.started' ||
    event.kind === 'chat.subagent.status' ||
    event.kind === 'chat.subagent.message'
  ) {
    void loadSubagents(event.conversationId)
    return true
  }
  return false
}

const recoverReferencedApproval = async (event: RemoteAIEvent) => {
  const reference = event.payload.approvalRef
  if (!reference || props.toolsAuthorized !== true || active.value?.id !== event.conversationId) {
    return
  }
  try {
    const current = await rpc.call<RemoteCurrentGenerationState>(
      'conversation.generation.get',
      { conversationId: event.conversationId },
      projectContext.value,
    )
    const approval = current.pendingApproval
    if (
      disposed ||
      active.value?.id !== event.conversationId ||
      current.conversationId !== event.conversationId ||
      current.generationId !== event.generationId ||
      !approval ||
      approval.id !== reference.id ||
      approval.conversationId !== event.conversationId ||
      approval.generationId !== event.generationId ||
      approval.messageId !== event.messageId
    ) {
      return
    }
    pendingApproval.value = approval
  } catch (error) {
    if (!disposed && active.value?.id === event.conversationId) {
      errorMessage.value = error instanceof Error ? error.message : '无法恢复设备端工具审批。'
    }
  }
}

const recoverInvalidAIEvent = () => {
  if (invalidEventRecovery || disposed) return
  const conversationId = active.value?.id
  if (!conversationId) return
  errorMessage.value = '设备返回了无效的 AI 对话事件，正在从权威快照恢复。'
  const current = detachGenerationAttach()
    .then(() => refreshActiveConversation(conversationId))
    .catch((error: unknown) => {
      if (!disposed) {
        errorMessage.value =
          error instanceof Error ? error.message : 'Unable to recover invalid AI event.'
      }
    })
    .finally(() => {
      if (invalidEventRecovery === current) invalidEventRecovery = undefined
    })
  invalidEventRecovery = current
}

const applyStreamEvent = (candidate: RemoteAIEvent) => {
  if (!isRemoteAIEvent(candidate)) {
    // Runtime JSON validation must happen before the durable cursor advances
    // or a partially valid object reaches the Vue reducer.
    recoverInvalidAIEvent()
    return
  }
  const event = candidate
  if (
    disposed ||
    event.conversationId !== active.value?.id ||
    event.sequence <= eventWatermark.value
  ) {
    return
  }
  if (event.sequence > eventWatermark.value + 1) {
    // Never advance across a missing durable sequence. Close only this attach
    // query and rebuild from the authoritative messages + event cursor.
    void detachGenerationAttach()
      .then(() => refreshActiveConversation(event.conversationId))
      .catch((error: unknown) => {
        errorMessage.value =
          error instanceof Error ? error.message : 'Unable to recover conversation stream.'
      })
    return
  }
  eventWatermark.value = event.sequence
  // Conversation-level collaboration events may intentionally have no
  // generation binding. They must not erase the active turn's cancel/recovery
  // identity while that generation is still running.
  if (event.generationId) activeGenerationId.value = event.generationId
  if (event.kind === 'chat.approval.requested') {
    if (event.payload.approval && props.toolsAuthorized === true) {
      pendingApproval.value = event.payload.approval
    } else if (event.payload.approvalRef && props.toolsAuthorized === true) {
      void recoverReferencedApproval(event)
    } else {
      errorMessage.value =
        'Agent 正等待工具审批，但当前浏览器会话未获 AI 工具授权；该请求会在设备端超时。'
    }
    return
  }
  if (applyCollaborationEvent(event)) return
  const terminal =
    event.kind === 'chat.completed' ||
    event.kind === 'chat.failed' ||
    event.kind === 'chat.cancelled'
  if (terminal) {
    if (event.payload.message) {
      const index = messages.value.findIndex((message) => message.id === event.messageId)
      if (index < 0) messages.value.push(event.payload.message)
      else messages.value.splice(index, 1, event.payload.message)
      messageWatermark.value = Math.max(messageWatermark.value, event.payload.message.sequence)
    }
    // The durable terminal event is enough to make the composer usable. The
    // final RPC response can be lost even though this event was committed.
    releaseGenerationControls()
    void refreshAfterTerminal(event.conversationId)
    return
  }
  const index = ensureStreamingMessage(event)
  const current = messages.value[index]!
  if (event.kind === 'chat.text.delta') {
    messages.value.splice(index, 1, {
      ...current,
      revision: current.revision + 1,
      content: current.content + (event.payload.delta ?? ''),
    })
  } else if (event.kind === 'chat.reasoning.delta') {
    messages.value.splice(index, 1, {
      ...current,
      revision: current.revision + 1,
      reasoning: current.reasoning + (event.payload.delta ?? ''),
    })
  } else if (event.kind === 'chat.tool.status' && event.payload.toolRun) {
    const runs = [...current.toolRuns]
    const runIndex = runs.findIndex((run) => run.id === event.payload.toolRun!.id)
    if (runIndex < 0) runs.push(event.payload.toolRun)
    else runs.splice(runIndex, 1, event.payload.toolRun)
    messages.value.splice(index, 1, { ...current, revision: current.revision + 1, toolRuns: runs })
  } else if (event.kind === 'chat.usage' && event.payload.usage) {
    messages.value.splice(index, 1, { ...current, usage: event.payload.usage })
  }
}

const setPlanMode = async (enabled: boolean) => {
  const conversation = active.value
  if (!conversation || !supportsCollaboration.value || generationActive.value) return
  collaborationPending.value = true
  try {
    const updated = await rpc.call<RemoteConversation>(
      'conversation.plan.set',
      { conversationId: conversation.id, active: enabled },
      projectContext.value,
    )
    await applyConversation(updated)
    queuedNotice.value = enabled
      ? 'Plan 模式已开启：Agent 将先调研并提出方案。'
      : 'Plan 模式已关闭。'
  } catch (error) {
    errorMessage.value = error instanceof Error ? error.message : '无法切换 Plan 模式。'
  } finally {
    collaborationPending.value = false
  }
}

const createGoal = async () => {
  const conversation = active.value
  const objective = goalDraft.value.trim()
  if (!conversation || !objective || !supportsGoal.value || generationActive.value) return
  collaborationPending.value = true
  try {
    const updated = await rpc.call<RemoteConversation>(
      'conversation.goal.create',
      {
        conversationId: conversation.id,
        objective,
        enableWorkspaceTools: workspaceToolsEnabled.value && canUseWorkspaceTools.value,
      },
      projectContext.value,
    )
    await applyConversation(updated)
    goalDraft.value = ''
    queuedNotice.value = 'Goal 已创建，设备 Agent 会在空闲时继续推进。'
    void refreshActiveConversation(conversation.id)
  } catch (error) {
    errorMessage.value = error instanceof Error ? error.message : '无法创建 Goal。'
  } finally {
    collaborationPending.value = false
  }
}

const updateGoal = async (action: 'pause' | 'resume' | 'clear') => {
  const conversation = active.value
  const goal = conversation?.goal
  if (!conversation || !goal || !supportsGoal.value || collaborationPending.value) return
  collaborationPending.value = true
  try {
    const updated = await rpc.call<RemoteConversation>(
      `conversation.goal.${action}`,
      {
        conversationId: conversation.id,
        goalId: goal.id,
        revision: goal.revision,
        enableWorkspaceTools: workspaceToolsEnabled.value && canUseWorkspaceTools.value,
      },
      projectContext.value,
    )
    await applyConversation(updated)
    if (action === 'clear') goalDraft.value = ''
    void refreshActiveConversation(conversation.id)
  } catch (error) {
    errorMessage.value = error instanceof Error ? error.message : '无法更新 Goal。'
  } finally {
    collaborationPending.value = false
  }
}

const editGoal = async (goal: RemoteAIGoal) => {
  const conversation = active.value
  if (!conversation || !supportsGoal.value || collaborationPending.value) return
  const objective = window.prompt('更新长期目标', goal.objective)?.trim()
  if (!objective || objective === goal.objective) return
  collaborationPending.value = true
  try {
    const updated = await rpc.call<RemoteConversation>(
      'conversation.goal.edit',
      {
        conversationId: conversation.id,
        goalId: goal.id,
        revision: goal.revision,
        objective,
        enableWorkspaceTools: workspaceToolsEnabled.value && canUseWorkspaceTools.value,
      },
      projectContext.value,
    )
    await applyConversation(updated)
  } catch (error) {
    errorMessage.value = error instanceof Error ? error.message : '无法编辑 Goal。'
  } finally {
    collaborationPending.value = false
  }
}

const openSubagent = async (conversation: RemoteConversation) => {
  await applyConversation(conversation)
  await openConversation(conversation)
}

const messageSubagent = async (conversation: RemoteConversation) => {
  const parent = active.value
  if (!parent || !conversation.subagent || collaborationPending.value) return
  const message = window
    .prompt(`发送给子 Agent「${conversation.subagent.label}」的补充说明`)
    ?.trim()
  if (!message) return
  collaborationPending.value = true
  try {
    await rpc.call(
      'conversation.subagent.message',
      {
        conversationId: parent.id,
        agentId: conversation.id,
        message,
        enableWorkspaceTools: workspaceToolsEnabled.value && canUseWorkspaceTools.value,
      },
      projectContext.value,
    )
    queuedNotice.value = '补充说明已放入子 Agent 的下一步队列。'
    await loadSubagents(parent.id)
  } catch (error) {
    errorMessage.value = error instanceof Error ? error.message : '无法向子 Agent 发送消息。'
  } finally {
    collaborationPending.value = false
  }
}

const interruptSubagent = async (conversation: RemoteConversation) => {
  const parent = active.value
  if (!parent || !conversation.subagent || collaborationPending.value) return
  collaborationPending.value = true
  try {
    await rpc.call(
      'conversation.subagent.interrupt',
      { conversationId: parent.id, agentId: conversation.id },
      projectContext.value,
    )
    await loadSubagents(parent.id)
  } catch (error) {
    errorMessage.value = error instanceof Error ? error.message : '无法停止子 Agent。'
  } finally {
    collaborationPending.value = false
  }
}

const respondToApproval = async (decision: 'deny' | 'allowOnce' | 'allowForSession') => {
  const approval = pendingApproval.value
  if (!approval || props.toolsAuthorized !== true) return
  collaborationPending.value = true
  try {
    await rpc.call(
      'conversation.approval.respond',
      {
        approvalId: approval.id,
        conversationId: approval.conversationId,
        generationId: approval.generationId,
        toolCallId: approval.toolCallId,
        decision,
      },
      projectContext.value,
    )
    pendingApproval.value = null
  } catch (error) {
    errorMessage.value = error instanceof Error ? error.message : '无法提交工具审批。'
  } finally {
    collaborationPending.value = false
  }
}

const confirmWorkspaceMode = (event: Event) => {
  const target = event.target as HTMLSelectElement
  const next = target.value as RemoteAIWorkspaceMode
  if (
    next === 'fullAccess' &&
    !window.confirm('完全访问会让 Agent 绕过单次审批。确认仅在受信任的项目中使用？')
  ) {
    selectedWorkspaceMode.value = 'workspaceWrite'
    return
  }
  selectedWorkspaceMode.value = next
}

const attachmentDirectory = '.wenzwork-ai-attachments'
let attachmentDirectoryReady = false

const safeAttachmentName = (value: string) => {
  const leaf = value.trim().replaceAll('\\', '/').split('/').at(-1) ?? ''
  const safe = leaf.replace(/[<>:"/\\|?*\x00-\x1f]/gu, '_').replace(/[. ]+$/u, '')
  return safe && safe !== '.' && safe !== '..' ? safe.slice(0, 180) : 'attachment.txt'
}

const attachmentMimeType = (name: string) => {
  const lower = name.toLowerCase()
  if (lower.endsWith('.png')) return 'image/png'
  if (lower.endsWith('.jpg') || lower.endsWith('.jpeg')) return 'image/jpeg'
  if (lower.endsWith('.webp')) return 'image/webp'
  if (lower.endsWith('.gif')) return 'image/gif'
  if (lower.endsWith('.json')) return 'application/json'
  if (lower.endsWith('.html') || lower.endsWith('.htm')) return 'text/html'
  if (lower.endsWith('.css')) return 'text/css'
  if (lower.endsWith('.csv')) return 'text/csv'
  if (lower.endsWith('.xml')) return 'application/xml'
  if (lower.endsWith('.md') || lower.endsWith('.markdown')) return 'text/markdown'
  return 'text/plain'
}

const isSupportedAttachment = (file: File) => {
  const name = file.name.toLowerCase()
  return (
    /\.(png|jpe?g|webp|gif|txt|md|markdown|json|html?|css|csv|xml|ya?ml|js|ts|dart|go|py|java|kt|rs|c|cc|cpp|h|hpp|sh|ps1)$/u.test(
      name,
    ) || file.type.startsWith('text/')
  )
}

const ensureAttachmentDirectory = async () => {
  if (attachmentDirectoryReady) return
  try {
    const existing = await rpc.call<RemoteFileEntry>(
      'file.stat',
      { path: attachmentDirectory },
      projectContext.value,
    )
    if (existing.kind !== 'directory') throw new Error('远程附件目录被同名文件占用。')
  } catch (error) {
    if (error instanceof Error && error.message.includes('同名文件')) throw error
    await rpc.call(
      'file.mkdir',
      { parentPath: '', name: attachmentDirectory },
      projectContext.value,
    )
  }
  attachmentDirectoryReady = true
}

const digestAttachment = async (blob: Blob) => {
  if (!globalThis.crypto?.subtle) throw new Error('当前浏览器无法校验附件完整性。')
  const digest = new Uint8Array(
    await globalThis.crypto.subtle.digest('SHA-256', await blob.arrayBuffer()),
  )
  let text = ''
  for (const byte of digest) text += String.fromCharCode(byte)
  return btoa(text).replaceAll('+', '-').replaceAll('/', '_').replaceAll('=', '')
}

const importAttachment = async (file: File): Promise<RemoteAIAttachment> => {
  const importId = globalThis.crypto.randomUUID()
  const name = safeAttachmentName(file.name)
  await ensureAttachmentDirectory()
  await rpc.call(
    'file.mkdir',
    { parentPath: attachmentDirectory, name: importId },
    projectContext.value,
  )
  const relativePath = `${attachmentDirectory}/${importId}/${name}`
  const uploaded = await rpc.uploadFile(relativePath, file, undefined, projectContext.value)
  if (uploaded.entry.relativePath !== relativePath || uploaded.entry.size !== file.size) {
    throw new Error('设备未能确认附件上传完整性。')
  }
  const details = await rpc.call<{ entry: RemoteFileEntry }>(
    'file.details',
    { path: relativePath },
    projectContext.value,
  )
  const entry = details.entry
  if (entry.kind !== 'file' || entry.size !== file.size)
    throw new Error('远程附件在校验期间发生了变化。')
  const blob = await rpc.downloadFile(relativePath, undefined, projectContext.value, entry.revision)
  if (blob.size !== file.size) throw new Error('远程附件在校验期间发生了变化。')
  return {
    id: globalThis.crypto.randomUUID(),
    relativePath,
    name: entry.name,
    mimeType: attachmentMimeType(entry.name),
    size: entry.size,
    sha256: await digestAttachment(blob),
    revision: entry.revision,
  }
}

const addAttachments = async (event: Event) => {
  const input = event.target as HTMLInputElement
  const files = Array.from(input.files ?? [])
  input.value = ''
  if (!files.length || attachmentsUploading.value) return
  if (props.attachmentsAuthorized !== true) {
    errorMessage.value = '当前远程会话未获文件上传授权。'
    return
  }
  const total =
    pendingAttachments.value.reduce((sum, attachment) => sum + attachment.size, 0) +
    files.reduce((sum, file) => sum + file.size, 0)
  if (pendingAttachments.value.length + files.length > 8) {
    errorMessage.value = '每条消息最多可添加 8 个附件。'
    return
  }
  if (files.some((file) => file.size > 8 * 1024 * 1024) || total > 32 * 1024 * 1024) {
    errorMessage.value = '单个附件不能超过 8 MiB，单条消息附件总量不能超过 32 MiB。'
    return
  }
  if (files.some((file) => !isSupportedAttachment(file))) {
    errorMessage.value = '仅支持图片和常见文本/代码文件作为对话附件。'
    return
  }
  attachmentsUploading.value = true
  try {
    const imported: RemoteAIAttachment[] = []
    for (const file of files) imported.push(await importAttachment(file))
    pendingAttachments.value = [...pendingAttachments.value, ...imported]
    errorMessage.value = ''
  } catch (error) {
    errorMessage.value = error instanceof Error ? error.message : '无法导入附件。'
  } finally {
    attachmentsUploading.value = false
  }
}

const removeAttachment = (id: string) => {
  pendingAttachments.value = pendingAttachments.value.filter((attachment) => attachment.id !== id)
}

const openAttachmentPicker = () => attachmentInput.value?.click()

const enqueueDuringGeneration = async (destination: 'nextTurn' | 'nextStep') => {
  const conversation = active.value
  const content = prompt.value.trim()
  if (
    !conversation ||
    !supportsInbox.value ||
    attachmentsUploading.value ||
    (!content && pendingAttachments.value.length === 0) ||
    promptBytes.value > 32 * 1024
  ) {
    return
  }
  try {
    await rpc.call(
      'conversation.send',
      {
        conversationId: conversation.id,
        messageId: globalThis.crypto.randomUUID(),
        content,
        attachments: pendingAttachments.value,
        workspaceMode: workspaceToolsEnabled.value ? selectedWorkspaceMode.value : 'readOnly',
        enableWorkspaceTools: workspaceToolsEnabled.value && canUseWorkspaceTools.value,
        destination,
      },
      projectContext.value,
    )
    prompt.value = ''
    pendingAttachments.value = []
    queuedNotice.value =
      destination === 'nextStep' ? '已插入当前 Agent 的下一步。' : '已加入下一轮队列。'
  } catch (error) {
    errorMessage.value = error instanceof Error ? error.message : '无法将消息加入 Agent 队列。'
  }
}

const goalPhaseLabel = (goal: RemoteAIGoal, armed: boolean | undefined) => {
  if (goal.phase === 'active') return armed === false ? '待恢复' : '进行中'
  if (goal.phase === 'paused') return '已暂停'
  if (goal.phase === 'blocked') return '受阻'
  return '已完成'
}

const subagentStatusLabel = (conversation: RemoteConversation) => {
  const status = conversation.subagent?.status
  return (
    {
      running: '运行中',
      ready: '就绪',
      completed: '已完成',
      failed: '失败',
      interrupted: '已中断',
    } as const
  )[status ?? 'ready']
}

const toolRunLabel = (run: RemoteAIToolRun) => {
  const status =
    run.status === 'running'
      ? '执行中'
      : run.status === 'succeeded'
        ? '已完成'
        : run.status === 'cancelled'
          ? '已取消'
          : '失败'
  return `${run.description || run.name} · ${status}`
}

const toolRunDetail = (run: RemoteAIToolRun) =>
  run.output || run.errorCode || (run.result ? JSON.stringify(run.result, null, 2) : '')

type RemoteMessageTimelineItem =
  | { key: string; kind: 'reasoning'; content: string }
  | { key: string; kind: 'content'; content: string }
  | { key: string; kind: 'tool'; run: RemoteAIToolRun }

const messageTimeline = (message: RemoteChatMessage): RemoteMessageTimelineItem[] => {
  const timeline: RemoteMessageTimelineItem[] = []
  if (message.reasoning) {
    timeline.push({ key: `${message.id}:reasoning`, kind: 'reasoning', content: message.reasoning })
  }

  const contentLength = message.content.length
  const orderedRuns = message.toolRuns
    .map((run, index) => ({ run, index }))
    .sort((left, right) => {
      const leftOffset = Number.isSafeInteger(left.run.contentOffset)
        ? Math.max(0, Math.min(contentLength, left.run.contentOffset!))
        : contentLength
      const rightOffset = Number.isSafeInteger(right.run.contentOffset)
        ? Math.max(0, Math.min(contentLength, right.run.contentOffset!))
        : contentLength
      if (leftOffset !== rightOffset) return leftOffset - rightOffset
      const timeOrder = left.run.startedAt.localeCompare(right.run.startedAt)
      return timeOrder || left.index - right.index
    })

  let cursor = 0
  for (const { run } of orderedRuns) {
    const rawOffset = Number.isSafeInteger(run.contentOffset) ? run.contentOffset! : contentLength
    const offset = Math.max(cursor, Math.min(contentLength, Math.max(0, rawOffset)))
    if (offset > cursor) {
      timeline.push({
        key: `${message.id}:content:${cursor}:${offset}`,
        kind: 'content',
        content: message.content.slice(cursor, offset),
      })
    }
    timeline.push({ key: `${message.id}:tool:${run.id}`, kind: 'tool', run })
    cursor = offset
  }
  if (cursor < contentLength) {
    timeline.push({
      key: `${message.id}:content:${cursor}:${contentLength}`,
      kind: 'content',
      content: message.content.slice(cursor),
    })
  }
  return timeline
}

const usageLabel = (usage: RemoteAIUsage) => {
  if (!usage.totalTokens) return ''
  const parts = [`${usage.totalTokens.toLocaleString()} tokens`]
  if (usage.inputTokens || usage.outputTokens) {
    parts.push(
      `输入 ${usage.inputTokens.toLocaleString()} · 输出 ${usage.outputTokens.toLocaleString()}`,
    )
  }
  if (usage.cachedInputTokens) parts.push(`缓存 ${usage.cachedInputTokens.toLocaleString()}`)
  return parts.join(' · ')
}

const send = async () => {
  if (
    sending.value ||
    attachmentsUploading.value ||
    (!prompt.value.trim() && pendingAttachments.value.length === 0)
  ) {
    return
  }
  if (promptBytes.value > 32 * 1024) {
    errorMessage.value = '单条消息按 UTF-8 编码后不能超过 32 KiB。'
    return
  }
  if (!active.value) await createConversation()
  if (!active.value) return
  const content = prompt.value.trim()
  const attachments = [...pendingAttachments.value]
  const conversationID = active.value.id
  const messageID = globalThis.crypto.randomUUID()
  const requestEpoch = ++sendRequestEpoch
  const workspaceMode =
    workspaceToolsEnabled.value && canUseWorkspaceTools.value
      ? selectedWorkspaceMode.value
      : 'readOnly'
  const enableWorkspaceTools = workspaceToolsEnabled.value && canUseWorkspaceTools.value
  messages.value.push({
    id: messageID,
    revision: 1,
    sequence: nextMessageSequence(),
    role: 'user',
    content,
    status: 'complete',
    errorCode: '',
    attachments,
    reasoning: '',
    toolRuns: [],
    usage: emptyUsage(),
    providerRun: emptyProviderRun(),
    createdAt: new Date().toISOString(),
  })
  prompt.value = ''
  sending.value = true
  errorMessage.value = ''
  queuedNotice.value = ''
  let accepted = false
  const applySendEvent = (event: RemoteAIEvent) => {
    // Any request-bound durable event proves that the user message was
    // committed, even if the final RPC response is later lost or reports the
    // provider failure represented by a terminal event.
    accepted = true
    pendingAttachments.value = []
    applyStreamEvent(event)
  }
  try {
    await rpc.stream<RemoteAIEvent>(
      'conversation.send',
      {
        conversationId: conversationID,
        messageId: messageID,
        content,
        attachments,
        workspaceMode,
        enableWorkspaceTools,
      },
      applySendEvent,
      projectContext.value,
    )
    accepted = true
    pendingAttachments.value = []
  } catch (error) {
    if (requestEpoch === sendRequestEpoch) {
      errorMessage.value =
        error instanceof Error ? error.message : 'AI 对话中断，可重新发送或刷新历史。'
      if (!accepted && !prompt.value) prompt.value = content
    }
  } finally {
    if (!disposed && requestEpoch === sendRequestEpoch) {
      sending.value = false
      let terminalWasRefreshed = false
      if (active.value?.id === conversationID) {
        if (terminalRefresh) {
          await terminalRefresh
          terminalWasRefreshed = true
        } else {
          await refreshActiveConversation(conversationID)
        }
      }
      if (!accepted && messages.value.some((message) => message.id === messageID)) {
        // A lost RPC response is weaker than the Device's authoritative
        // message snapshot. Reconcile by the stable client message ID so the
        // restored draft cannot be sent again under a new ID. Preserve edits
        // or new attachments added while recovery was in progress.
        accepted = true
        if (prompt.value === content) prompt.value = ''
        const committedAttachmentIDs = new Set(attachments.map((attachment) => attachment.id))
        pendingAttachments.value = pendingAttachments.value.filter(
          (attachment) => !committedAttachmentIDs.has(attachment.id),
        )
      }
      if (!terminalWasRefreshed) await loadConversations()
      if (!accepted) scrollToMessageTail()
    }
  }
}

const cancel = async () => {
  if (!active.value) return
  try {
    await rpc.call(
      'conversation.cancel',
      {
        conversationId: active.value.id,
        ...(activeGenerationId.value ? { generationId: activeGenerationId.value } : {}),
      },
      projectContext.value,
    )
  } finally {
    sending.value = false
  }
}

const reconcileAfterReconnect = async () => {
  if (reconnectSyncing || disposed) return
  reconnectSyncing = true
  const activeID = active.value?.id
  try {
    await loadCapabilities()
    await loadConversations()
    if (activeID && active.value?.id === activeID) {
      await refreshActiveConversation(activeID)
    }
  } catch (error) {
    if (!disposed) {
      errorMessage.value =
        error instanceof Error ? error.message : 'Unable to reconcile remote conversation.'
    }
  } finally {
    reconnectSyncing = false
  }
}

const reconcileAfterAgentEvent = async (eventType: string, conversationId: string) => {
  if (disposed) return
  const activeID = active.value?.id
  // Event payloads intentionally contain no message text or AI tool data.
  // Reconcile through the existing encrypted read APIs before acknowledging
  // the cursor, which keeps cache and render state authoritative.
  if (eventType === 'conversation.changed') {
    await loadConversations()
    if (activeID && active.value?.id === activeID) {
      await refreshActiveConversation(activeID)
    }
    return
  }
  if (eventType === 'conversation.events.available') {
    // Availability is a compact wake-up hint, never a content transport. A
    // healthy request-bound attach already has the committed deltas.
    if (conversationId !== activeID || sending.value || generationAttachHealthy) return
    if (generationRecoveryAvailable === false) {
      await replayLegacyConversationEvents(conversationId)
      return
    }
    await refreshActiveConversation(conversationId)
  }
}

watch(
  () => rpc.connected.value,
  (connected, previous) => {
    if (initialized && connected && previous === false) void reconcileAfterReconnect()
  },
)

defineExpose({
  createConversation,
  deleteConversation: deleteConversationItem,
  loadMoreConversations: () => loadConversations(true),
  openConversation,
})

onMounted(() => {
  removeAgentEventListener = agentEvents?.onEvent(async (event) => {
    if (
      event.projectId !== props.projectId ||
      (event.type !== 'conversation.changed' && event.type !== 'conversation.events.available')
    ) {
      return
    }
    await reconcileAfterAgentEvent(event.type, event.aggregateId)
  })
  removeAgentResetListener = agentEvents?.onReset(async () => {
    conversationWatermark.value = 0
    messageWatermark.value = 0
    await loadCapabilities()
    await loadConversations(false, true)
    if (active.value) await refreshActiveConversation(active.value.id, true, true)
  })
  void loadCapabilities()
  void hydrateConversations()
    .then(() => loadConversations())
    .finally(() => {
      initialized = true
    })
})

onBeforeUnmount(() => {
  disposed = true
  removeAgentEventListener?.()
  removeAgentResetListener?.()
  // Navigation only detaches this observer's attach query. Explicit Stop is
  // the sole path that invokes conversation.cancel on the device.
  void detachGenerationAttach()
})
</script>

<template>
  <section
    class="remote-panel chat-panel"
    :class="{ workspace }"
    :aria-labelledby="workspace ? undefined : 'remote-chat-heading'"
    :aria-label="workspace ? 'AI 对话' : undefined"
  >
    <div v-if="!workspace" class="remote-panel-heading">
      <div>
        <h2 id="remote-chat-heading">AI 对话</h2>
        <p>历史、附件与流式回复只在浏览器和目标设备之间解密。</p>
      </div>
      <div class="chat-heading-actions">
        <button
          v-if="active"
          type="button"
          :disabled="generationActive"
          @click="deleteConversation"
        >
          删除对话
        </button>
        <button type="button" @click="createConversation">新对话</button>
      </div>
    </div>
    <p v-if="errorMessage" class="remote-notice error" role="alert">{{ errorMessage }}</p>
    <p v-else-if="queuedNotice" class="remote-notice info" role="status">{{ queuedNotice }}</p>
    <div class="chat-layout" :class="{ 'without-sidebar': showConversationList === false }">
      <aside v-if="showConversationList !== false" aria-label="对话列表">
        <p v-if="loading">正在读取…</p>
        <button
          v-for="conversation in rootConversations"
          :key="conversation.id"
          type="button"
          :class="{ active: active?.id === conversation.id }"
          @click="openConversation(conversation)"
        >
          <strong>{{ conversation.title }}</strong>
          <span>{{ conversation.messageCount }} 条 · {{ conversation.modelBinding.model }}</span>
        </button>
        <button v-if="conversationCursor" type="button" @click="loadConversations(true)">
          更多对话
        </button>
      </aside>
      <div class="chat-main">
        <div v-if="!active" class="remote-panel-empty">选择或创建一个对话。</div>
        <template v-else>
          <div class="chat-session-toolbar">
            <div class="chat-session-model">
              <strong>{{ active.modelBinding.model }}</strong>
              <span>{{ active.modelBinding.provider }}</span>
            </div>
            <button
              v-if="supportsCollaboration"
              type="button"
              :disabled="generationActive || collaborationPending"
              :aria-pressed="active.planModeActive === true"
              @click="setPlanMode(active.planModeActive !== true)"
            >
              {{ active.planModeActive ? '退出 Plan' : 'Plan 模式' }}
            </button>
            <label
              class="workspace-tools-toggle"
              :title="canUseWorkspaceTools ? '' : '此会话未获设备工作区工具授权'"
            >
              <input
                v-model="workspaceToolsEnabled"
                type="checkbox"
                :disabled="!canUseWorkspaceTools || generationActive"
              />
              使用工作区工具
            </label>
            <select
              v-if="workspaceToolsEnabled"
              v-model="selectedWorkspaceMode"
              :disabled="generationActive"
              aria-label="Agent 工作区权限"
              @change="confirmWorkspaceMode"
            >
              <option value="readOnly">只读</option>
              <option value="workspaceWrite">可写工作区</option>
              <option value="fullAccess">完全访问</option>
            </select>
          </div>

          <section v-if="hasAgentState" class="agent-state-panel" aria-label="Agent 计划与子任务">
            <header>
              <strong>{{ active.planModeActive ? 'Plan 模式' : '执行计划' }}</strong>
              <span v-if="active.todos?.length"
                >{{ active.todos.filter((item) => item.status === 'completed').length }}/{{
                  active.todos.length
                }}
                已完成</span
              >
            </header>
            <p v-if="active.planModeActive" class="agent-state-hint">
              当前仅调研和制定方案；实施前会等待你的批准。
            </p>
            <ul v-if="active.todos?.length" class="agent-todos">
              <li v-for="item in active.todos" :key="item.content" :class="item.status">
                <span aria-hidden="true">{{
                  item.status === 'completed' ? '✓' : item.status === 'in_progress' ? '◌' : '○'
                }}</span>
                {{ item.content }}
              </li>
            </ul>
            <div v-if="subagents.length" class="agent-subagents">
              <strong>子 Agent</strong>
              <article v-for="child in subagents" :key="child.id">
                <button type="button" class="subagent-open" @click="openSubagent(child)">
                  {{ child.subagent?.label || child.title }} · {{ subagentStatusLabel(child) }}
                </button>
                <span v-if="child.subagent?.summary">{{ child.subagent.summary }}</span>
                <span v-else-if="child.subagent?.error" class="error">{{
                  child.subagent.error
                }}</span>
                <div class="subagent-actions">
                  <button
                    type="button"
                    :disabled="collaborationPending"
                    @click="messageSubagent(child)"
                  >
                    补充
                  </button>
                  <button
                    v-if="child.subagent?.status === 'running'"
                    type="button"
                    :disabled="collaborationPending"
                    @click="interruptSubagent(child)"
                  >
                    停止
                  </button>
                </div>
              </article>
            </div>
          </section>

          <section v-if="active.goal" class="agent-goal" aria-label="长期目标">
            <div>
              <strong>Goal · {{ goalPhaseLabel(active.goal, active.goalArmed) }}</strong>
              <span>{{ active.goal.roundsStarted }}/{{ active.goal.maxGoalRounds }} 回合</span>
            </div>
            <p>{{ active.goal.objective }}</p>
            <p v-if="active.goal.blockedReason" class="agent-state-hint">
              {{ active.goal.blockedReason.message }}
            </p>
            <div class="goal-actions">
              <button
                v-if="active.goal.phase === 'active' && active.goalArmed !== false"
                type="button"
                :disabled="collaborationPending"
                @click="updateGoal('pause')"
              >
                暂停
              </button>
              <button
                v-if="
                  active.goal.phase !== 'complete' &&
                  (active.goal.phase !== 'active' || active.goalArmed === false)
                "
                type="button"
                :disabled="collaborationPending"
                @click="updateGoal('resume')"
              >
                恢复
              </button>
              <button type="button" :disabled="collaborationPending" @click="editGoal(active.goal)">
                编辑
              </button>
              <button type="button" :disabled="collaborationPending" @click="updateGoal('clear')">
                清除
              </button>
            </div>
          </section>
          <form v-else-if="supportsGoal" class="goal-composer" @submit.prevent="createGoal">
            <input v-model="goalDraft" maxlength="32768" placeholder="创建一个可持续推进的 Goal…" />
            <button
              type="submit"
              :disabled="!goalDraft.trim() || generationActive || collaborationPending"
            >
              创建 Goal
            </button>
          </form>

          <button
            v-if="olderCursor"
            class="chat-older"
            type="button"
            :disabled="loadingMessages"
            @click="loadMessages(true)"
          >
            加载更早消息
          </button>
          <button
            v-if="!followMessageTail"
            class="chat-scroll-tail"
            type="button"
            @click="scrollToMessageTail(true)"
          >
            回到底部
          </button>
          <div
            ref="messageList"
            class="message-list"
            aria-live="polite"
            aria-relevant="additions text"
            @scroll="handleMessageScroll"
          >
            <article v-for="message in sortedMessages" :key="message.id" :class="message.role">
              <header class="message-meta">
                <span>{{ message.role }}</span>
                <small v-if="message.providerRun.model">{{ message.providerRun.model }}</small>
              </header>
              <div class="message-timeline">
                <template v-for="item in messageTimeline(message)" :key="item.key">
                  <details v-if="item.kind === 'reasoning'" class="message-reasoning">
                    <summary>思考过程</summary>
                    <pre>{{ item.content }}</pre>
                  </details>
                  <p v-else-if="item.kind === 'content'" class="message-content">
                    {{ item.content }}
                  </p>
                  <details v-else class="message-tool" :open="item.run.status === 'running'">
                    <summary>{{ toolRunLabel(item.run) }}</summary>
                    <pre v-if="toolRunDetail(item.run)">{{ toolRunDetail(item.run) }}</pre>
                  </details>
                </template>
              </div>
              <ul
                v-if="message.attachments.length"
                class="message-attachments"
                aria-label="消息附件"
              >
                <li v-for="attachment in message.attachments" :key="attachment.id">
                  {{ attachment.name }} · {{ Math.ceil(attachment.size / 1024) }} KiB
                </li>
              </ul>
              <small v-if="message.status !== 'complete'" class="message-state">
                {{
                  message.status === 'streaming'
                    ? '正在生成…'
                    : message.status === 'stopped'
                      ? '已停止'
                      : '生成失败'
                }}
              </small>
              <small v-if="usageLabel(message.usage)" class="message-usage">{{
                usageLabel(message.usage)
              }}</small>
            </article>
          </div>
          <section v-if="pendingApproval" class="agent-approval" aria-live="assertive">
            <strong>{{
              pendingApproval.preview.title || `允许 ${pendingApproval.toolName}？`
            }}</strong>
            <p>{{ pendingApproval.reason || pendingApproval.preview.description }}</p>
            <pre v-if="pendingApproval.preview.command">{{ pendingApproval.preview.command }}</pre>
            <div>
              <button
                type="button"
                :disabled="collaborationPending"
                @click="respondToApproval('deny')"
              >
                拒绝
              </button>
              <button
                type="button"
                :disabled="collaborationPending"
                @click="respondToApproval('allowOnce')"
              >
                仅此一次允许
              </button>
              <button
                v-if="pendingApproval.allowForSession"
                type="button"
                :disabled="collaborationPending"
                @click="respondToApproval('allowForSession')"
              >
                本次会话允许
              </button>
            </div>
          </section>
          <form v-else class="chat-composer" @submit.prevent="send">
            <input
              ref="attachmentInput"
              class="chat-file-input"
              type="file"
              multiple
              accept="image/png,image/jpeg,image/webp,image/gif,text/plain,text/markdown,application/json,.md,.txt,.json,.csv,.xml,.yaml,.yml,.html,.css,.js,.ts,.dart,.go,.py"
              @change="addAttachments"
            />
            <div v-if="pendingAttachments.length" class="composer-attachments">
              <span v-for="attachment in pendingAttachments" :key="attachment.id">
                {{ attachment.name }}
                <button type="button" title="移除附件" @click="removeAttachment(attachment.id)">
                  ×
                </button>
              </span>
            </div>
            <textarea
              v-model="prompt"
              rows="3"
              maxlength="32768"
              placeholder="向目标设备上的 AI 发送消息…"
            ></textarea>
            <button
              v-if="attachmentsAuthorized"
              type="button"
              :disabled="attachmentsUploading || generationActive"
              @click="openAttachmentPicker"
            >
              {{ attachmentsUploading ? '导入附件…' : '添加附件' }}
            </button>
            <small v-if="promptBytes > 32 * 1024" class="remote-notice error"
              >消息超过 32 KiB，请缩短后发送。</small
            >
            <div v-if="generationActive" class="composer-generation-actions">
              <button
                v-if="supportsInbox"
                type="button"
                :disabled="!prompt.trim() && pendingAttachments.length === 0"
                @click="enqueueDuringGeneration('nextStep')"
              >
                插入下一步
              </button>
              <button
                v-if="supportsInbox"
                type="button"
                :disabled="!prompt.trim() && pendingAttachments.length === 0"
                @click="enqueueDuringGeneration('nextTurn')"
              >
                加入队列
              </button>
              <button type="button" @click="cancel">停止</button>
            </div>
            <button v-else type="submit" :disabled="!canSubmit">发送</button>
          </form>
        </template>
      </div>
    </div>
  </section>
</template>

<style scoped>
.chat-panel.workspace {
  box-sizing: border-box;
  min-height: 0;
  height: 100%;
  border: 0;
  border-radius: 0;
  padding: 0;
  box-shadow: none;
}

.chat-panel.workspace .chat-layout {
  min-height: 0;
  height: 100%;
  border: 0;
  border-radius: 0;
}

.chat-layout.without-sidebar {
  grid-template-columns: minmax(0, 1fr);
}

.chat-layout.without-sidebar .chat-main {
  padding: 14px 28px 20px;
}

.chat-session-toolbar,
.chat-session-model,
.workspace-tools-toggle,
.agent-state-panel header,
.agent-goal > div,
.goal-actions,
.subagent-actions,
.composer-generation-actions,
.message-meta,
.message-attachments,
.composer-attachments {
  display: flex;
  align-items: center;
  gap: 8px;
}

.chat-session-toolbar {
  flex-wrap: wrap;
  justify-content: space-between;
  padding: 0 0 10px;
  border-bottom: 1px solid var(--line-soft, #e6e9ef);
}

.chat-session-model {
  min-width: 0;
  margin-right: auto;
}

.chat-session-model strong {
  overflow: hidden;
  max-width: 15rem;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.chat-session-model span,
.workspace-tools-toggle,
.agent-state-panel header span,
.agent-goal span,
.message-usage,
.agent-state-hint {
  color: var(--ink-faint);
  font-size: 0.76rem;
}

.workspace-tools-toggle {
  cursor: pointer;
  font-weight: 650;
}

.workspace-tools-toggle input {
  accent-color: var(--mint);
}

.chat-session-toolbar select,
.goal-composer input {
  min-width: 0;
  padding: 7px 9px;
}

.agent-state-panel,
.agent-goal,
.goal-composer,
.agent-approval {
  margin: 12px 0 2px;
  border: 1px solid var(--line-soft, #e6e9ef);
  border-radius: 12px;
  padding: 10px 12px;
  background: color-mix(in srgb, var(--paper-soft, #f8fafc) 88%, var(--brand-tint, #eef9f4));
}

.agent-state-panel header,
.agent-goal > div {
  justify-content: space-between;
}

.agent-state-hint,
.agent-goal p,
.agent-approval p {
  margin: 7px 0 0;
  white-space: pre-wrap;
}

.agent-todos {
  display: grid;
  gap: 5px;
  margin: 9px 0 0;
  padding: 0;
  list-style: none;
}

.agent-todos li {
  display: flex;
  gap: 7px;
  align-items: flex-start;
}

.agent-todos li.completed {
  color: var(--ink-faint);
  text-decoration: line-through;
}

.agent-todos li.in_progress span {
  color: var(--mint, #169873);
}

.agent-subagents {
  display: grid;
  gap: 7px;
  margin-top: 12px;
}

.agent-subagents article {
  display: grid;
  grid-template-columns: minmax(0, 1fr) auto;
  gap: 4px 10px;
  align-items: center;
  border-top: 1px solid var(--line-soft, #e6e9ef);
  padding-top: 7px;
}

.agent-subagents article > span {
  overflow: hidden;
  grid-column: 1;
  color: var(--ink-faint);
  font-size: 0.75rem;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.subagent-open {
  justify-self: start;
  overflow: hidden;
  max-width: 100%;
  border: 0;
  padding: 0;
  background: transparent;
  color: var(--ink, #1d2733);
  font-weight: 700;
  text-align: left;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.subagent-actions {
  grid-column: 2;
  grid-row: 1 / span 2;
}

.subagent-actions button,
.goal-actions button,
.agent-approval button,
.composer-attachments button {
  padding: 4px 7px;
  font-size: 0.75rem;
}

.goal-actions,
.agent-approval > div {
  justify-content: flex-end;
  margin-top: 9px;
}

.goal-composer {
  display: grid;
  grid-template-columns: minmax(0, 1fr) auto;
  gap: 8px;
}

.chat-scroll-tail {
  position: absolute;
  z-index: 1;
  align-self: center;
  right: 30px;
  margin-top: 8px;
  box-shadow: 0 3px 12px rgb(19 34 48 / 16%);
}

.message-list {
  position: relative;
  min-height: 260px;
  max-height: min(56vh, 640px);
  scroll-behavior: smooth;
}

.message-meta {
  justify-content: space-between;
}

.message-meta small {
  color: var(--ink-faint);
  font-size: 0.67rem;
}

.message-attachments {
  flex-wrap: wrap;
  margin: 8px 0 0;
  padding: 0;
  list-style: none;
}

.message-attachments li,
.composer-attachments > span {
  border-radius: 999px;
  padding: 3px 8px;
  background: rgb(22 152 115 / 11%);
  color: var(--ink-soft);
  font-size: 0.72rem;
}

.message-reasoning,
.message-tool {
  margin-top: 8px;
  border-radius: 8px;
  background: rgb(20 33 48 / 4%);
  padding: 5px 8px;
}

.message-content {
  margin: 8px 0 0;
  white-space: pre-wrap;
  overflow-wrap: anywhere;
}

.message-reasoning summary,
.message-tool summary {
  cursor: pointer;
  color: var(--ink-soft);
  font-size: 0.75rem;
  font-weight: 700;
}

.message-reasoning pre,
.message-tool pre,
.agent-approval pre {
  overflow: auto;
  max-height: 16rem;
  margin: 7px 0 2px;
  white-space: pre-wrap;
  word-break: break-word;
}

.message-usage {
  display: block;
  margin-top: 7px;
}

.chat-file-input {
  position: absolute;
  width: 1px;
  height: 1px;
  overflow: hidden;
  clip: rect(0 0 0 0);
  white-space: nowrap;
}

.composer-attachments {
  grid-column: 1 / -1;
  flex-wrap: wrap;
}

.composer-attachments > span {
  display: inline-flex;
  align-items: center;
  gap: 4px;
}

.composer-attachments button {
  border: 0;
  padding: 0;
  background: transparent;
  color: inherit;
  line-height: 1;
}

.composer-generation-actions {
  flex-wrap: wrap;
  justify-content: flex-end;
}

.remote-notice.info {
  color: var(--ink-soft);
}

@media (max-width: 720px) {
  .chat-session-toolbar,
  .goal-composer {
    grid-template-columns: 1fr;
  }

  .goal-composer {
    display: grid;
  }

  .chat-session-toolbar {
    align-items: flex-start;
  }

  .chat-scroll-tail {
    right: 18px;
  }

  .agent-subagents article {
    grid-template-columns: minmax(0, 1fr);
  }

  .subagent-actions {
    grid-column: 1;
    grid-row: auto;
  }
}
</style>
