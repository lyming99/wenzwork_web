import type { RemoteAIEvent, RemoteAIEventKind, RemoteChatMessage } from './rpcTypes'

const eventKinds = new Set<RemoteAIEventKind>([
  'chat.text.delta',
  'chat.reasoning.delta',
  'chat.tool.status',
  'chat.approval.requested',
  'chat.usage',
  'chat.goal.changed',
  'chat.plan_mode.changed',
  'chat.todo.updated',
  'chat.subagent.started',
  'chat.subagent.status',
  'chat.subagent.message',
  'chat.completed',
  'chat.failed',
  'chat.cancelled',
])

const collaborationEventKinds = new Set<RemoteAIEventKind>([
  'chat.goal.changed',
  'chat.plan_mode.changed',
  'chat.todo.updated',
  'chat.subagent.started',
  'chat.subagent.status',
  'chat.subagent.message',
])

const isRecord = (value: unknown): value is Record<string, unknown> =>
  typeof value === 'object' && value !== null && !Array.isArray(value)

const isBoundedString = (value: unknown, maximum: number, allowEmpty = false) =>
  typeof value === 'string' && value.length <= maximum && (allowEmpty || value.length > 0)

const isSafeNonNegativeInteger = (value: unknown) =>
  Number.isSafeInteger(value) && (value as number) >= 0

const isSafePositiveInteger = (value: unknown) =>
  Number.isSafeInteger(value) && (value as number) > 0

const isTimestamp = (value: unknown) =>
  isBoundedString(value, 64) && Number.isFinite(Date.parse(value as string))

const isUsage = (value: unknown) => {
  if (!isRecord(value)) return false
  return [
    value.inputTokens,
    value.outputTokens,
    value.reasoningTokens,
    value.cachedInputTokens,
    value.totalTokens,
  ].every(isSafeNonNegativeInteger)
}

const isToolRun = (value: unknown) => {
  if (!isRecord(value)) return false
  if (
    !isBoundedString(value.id, 256) ||
    !isBoundedString(value.name, 128) ||
    !['running', 'succeeded', 'failed', 'cancelled'].includes(String(value.status)) ||
    !isTimestamp(value.startedAt)
  ) {
    return false
  }
  return (
    (value.contentOffset === undefined || isSafeNonNegativeInteger(value.contentOffset)) &&
    (value.finishedAt === undefined || isTimestamp(value.finishedAt))
  )
}

const isMessage = (value: unknown, event: Record<string, unknown>): value is RemoteChatMessage => {
  if (!isRecord(value)) return false
  return (
    value.id === event.messageId &&
    isBoundedString(value.id, 80) &&
    isSafePositiveInteger(value.revision) &&
    isSafePositiveInteger(value.sequence) &&
    ['user', 'assistant', 'system', 'tool'].includes(String(value.role)) &&
    ['complete', 'streaming', 'failed', 'stopped'].includes(String(value.status)) &&
    isBoundedString(value.content, 1024 * 1024, true) &&
    isBoundedString(value.reasoning, 1024 * 1024, true) &&
    Array.isArray(value.attachments) &&
    value.attachments.length <= 8 &&
    Array.isArray(value.toolRuns) &&
    value.toolRuns.length <= 512 &&
    value.toolRuns.every(isToolRun) &&
    isUsage(value.usage) &&
    isRecord(value.providerRun) &&
    isTimestamp(value.createdAt) &&
    (value.generationId === undefined || value.generationId === event.generationId)
  )
}

const isMessageReference = (value: unknown, event: Record<string, unknown>) =>
  isRecord(value) &&
  value.id === event.messageId &&
  value.generationId === event.generationId &&
  isSafePositiveInteger(value.revision) &&
  isSafePositiveInteger(value.sequence) &&
  ['complete', 'failed', 'stopped'].includes(String(value.status))

const hasValidPayloadForKind = (
  event: Record<string, unknown>,
  payload: Record<string, unknown>,
) => {
  switch (event.kind) {
    case 'chat.text.delta':
    case 'chat.reasoning.delta':
      return isBoundedString(payload.delta, 64 * 1024)
    case 'chat.tool.status':
      return isToolRun(payload.toolRun)
    case 'chat.approval.requested':
      return isRecord(payload.approval) || isRecord(payload.approvalRef)
    case 'chat.usage':
      return isUsage(payload.usage)
    case 'chat.plan_mode.changed':
      return typeof payload.active === 'boolean'
    case 'chat.todo.updated':
      return (
        Array.isArray(payload.todos) &&
        payload.todos.length <= 100 &&
        payload.todos.every(
          (todo) =>
            isRecord(todo) &&
            isBoundedString(todo.content, 4 * 1024) &&
            ['pending', 'in_progress', 'completed'].includes(String(todo.status)),
        )
      )
    case 'chat.goal.changed':
      return payload.goal === undefined || payload.goal === null || isRecord(payload.goal)
    case 'chat.subagent.started':
    case 'chat.subagent.status':
    case 'chat.subagent.message':
      return payload.agentId === undefined || isBoundedString(payload.agentId, 80)
    case 'chat.completed':
    case 'chat.failed':
    case 'chat.cancelled':
      return isMessage(payload.message, event) || isMessageReference(payload.messageRef, event)
    default:
      return false
  }
}

/** Validates the business JSON before a stream event can advance the durable
 * cursor or mutate the Vue projection. Unknown additive fields are retained,
 * while malformed required fields fail closed and trigger snapshot recovery. */
export const isRemoteAIEvent = (value: unknown): value is RemoteAIEvent => {
  if (!isRecord(value) || !eventKinds.has(value.kind as RemoteAIEventKind)) return false
  if (
    !isBoundedString(value.eventId, 80) ||
    !isBoundedString(value.conversationId, 80) ||
    !isBoundedString(value.generationId, 80, true) ||
    !isBoundedString(value.messageId, 80, true) ||
    !isSafePositiveInteger(value.sequence) ||
    !isTimestamp(value.occurredAt) ||
    !isRecord(value.payload)
  ) {
    return false
  }
  if (
    !collaborationEventKinds.has(value.kind as RemoteAIEventKind) &&
    (value.generationId === '' || value.messageId === '')
  ) {
    return false
  }
  return hasValidPayloadForKind(value, value.payload)
}
