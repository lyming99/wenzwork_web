import { ref, watch, type InjectionKey, type Ref } from 'vue'

import {
  readAgentEventCursor,
  removeAgentEventCursor,
  storeAgentEventCursor,
} from './agentEventCursor'
import { type RemoteAgentCapabilities, type RemotePeerRpcEvent } from './peerClient'
import type { RemoteRPCClient } from './rpcTypes'

export type AgentEventTopic =
  'agent' | 'capabilities' | 'conversation' | 'message' | 'task' | 'taskLog' | 'workflow'

export type AgentEventOperation = 'upsert' | 'delete' | 'status' | 'invalidate'

export interface AgentStateEvent {
  eventId: string
  sequence: number
  highWatermark: number
  occurredAt?: Date
  schemaVersion: 1
  projectId: string
  topic: AgentEventTopic
  type: string
  aggregateType: string
  aggregateId: string
  operation: AgentEventOperation
  revision: number
  cursor: { kind: string; value: number }
  data: Readonly<Record<string, string | number | boolean>>
  causationRequestId?: string
}

export interface AgentEventReset {
  reason: 'bootstrap' | 'retention' | 'sequenceGap' | 'slowConsumer' | 'schemaChanged'
  highWatermark: number
}

export type AgentEventConnectionState =
  'idle' | 'connecting' | 'ready' | 'unsupported' | 'reconnecting' | 'closed' | 'failed'

export interface AgentEventConnection {
  readonly state: Ref<AgentEventConnectionState>
  readonly error: Ref<string>
  readonly ready: Ref<boolean>
  start(): Promise<void>
  close(options?: { clearCursor?: boolean }): Promise<void>
  onEvent(handler: (event: AgentStateEvent) => void | Promise<void>): () => void
  onReset(handler: (reset: AgentEventReset) => void | Promise<void>): () => void
}

export const agentEventConnectionKey: InjectionKey<AgentEventConnection> =
  Symbol('agent-event-connection')

const SAFE_INTEGER = Number.MAX_SAFE_INTEGER
const MAXIMUM_PAYLOAD_BYTES = 4 * 1024
const MAXIMUM_INBOUND_QUEUE_COUNT = 256
const MAXIMUM_INBOUND_QUEUE_BYTES = 512 * 1024
const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/u
const RESET_REASONS = new Set<AgentEventReset['reason']>([
  'bootstrap',
  'retention',
  'sequenceGap',
  'slowConsumer',
  'schemaChanged',
])

interface AgentInboundQueue {
  run: number
  peer: RemoteRPCClient
  deviceId: string
  projectId: string
  tail: Promise<void>
  count: number
  bytes: number
}
const TOPICS = new Set<AgentEventTopic>([
  'agent',
  'capabilities',
  'conversation',
  'message',
  'task',
  'taskLog',
  'workflow',
])
const OPERATIONS = new Set<AgentEventOperation>(['upsert', 'delete', 'status', 'invalidate'])
const EVENT_TOPIC_BY_TYPE: Readonly<Record<string, AgentEventTopic>> = {
  'agent.status.changed': 'agent',
  'capabilities.changed': 'capabilities',
  'conversation.changed': 'conversation',
  'conversation.events.available': 'message',
  'task.changed': 'task',
  'task.logs.available': 'taskLog',
  'workflow.changed': 'workflow',
}
const EVENT_DATA_FIELDS: Readonly<Record<string, ReadonlySet<string>>> = {
  'agent.status.changed': new Set(['status', 'activeTaskCount', 'activeGenerationCount']),
  'capabilities.changed': new Set(['capabilitiesRevision']),
  'conversation.changed': new Set(['state', 'lastMessageSequence']),
  'conversation.events.available': new Set(['generationId']),
  'task.changed': new Set(['status']),
  'task.logs.available': new Set(['runId', 'generation', 'highWatermark']),
  'workflow.changed': new Set(),
}

const safeInteger = (value: unknown): value is number =>
  typeof value === 'number' && Number.isSafeInteger(value) && value >= 0 && value <= SAFE_INTEGER

const record = (value: unknown, label: string): Record<string, unknown> => {
  if (!value || typeof value !== 'object' || Array.isArray(value)) {
    throw new Error(`Agent ${label} is invalid.`)
  }
  return value as Record<string, unknown>
}

const ensurePayloadSize = (value: unknown) => {
  try {
    if (new TextEncoder().encode(JSON.stringify(value)).byteLength > MAXIMUM_PAYLOAD_BYTES) {
      throw new Error('Agent event payload is too large.')
    }
  } catch (failure) {
    if (failure instanceof Error && failure.message === 'Agent event payload is too large.') {
      throw failure
    }
    throw new Error('Agent event payload is invalid.')
  }
}

const safeData = (type: string, value: Record<string, unknown>) => {
  const allowed = EVENT_DATA_FIELDS[type]
  if (!allowed) throw new Error('Agent state event type is invalid.')
  for (const [key, item] of Object.entries(value)) {
    if (
      !allowed.has(key) ||
      !(
        typeof item === 'string' ||
        typeof item === 'boolean' ||
        (typeof item === 'number' && safeInteger(item))
      ) ||
      (typeof item === 'string' && item.length > 160)
    ) {
      throw new Error('Agent state event payload is invalid.')
    }
  }
  return value as Record<string, string | number | boolean>
}

const supportsEvents = (capabilities: RemoteAgentCapabilities) =>
  capabilities.features['events.v1'] === true && (capabilities.featureVersions.events ?? 0) >= 1

const parseControl = (wire: RemotePeerRpcEvent, projectId: string) => {
  if (
    wire.eventKind !== 13 ||
    !UUID.test(wire.eventId) ||
    wire.sequence !== 0 ||
    !safeInteger(wire.highWatermark)
  ) {
    throw new Error('Agent event control frame is invalid.')
  }
  ensurePayloadSize(wire.payload)
  const value = record(wire.payload, 'event control')
  if (
    value.schemaVersion !== 1 ||
    value.projectId !== projectId ||
    typeof value.type !== 'string' ||
    value.highWatermark !== wire.highWatermark
  ) {
    throw new Error('Agent event control binding is invalid.')
  }
  if (value.type === 'subscription.ready') {
    if (
      !safeInteger(value.minimumAvailableSequence) ||
      (value.minimumAvailableSequence as number) > wire.highWatermark + 1 ||
      typeof value.resetRequired !== 'boolean' ||
      typeof value.resetReason !== 'string' ||
      !safeInteger(value.heartbeatSeconds) ||
      (value.heartbeatSeconds as number) < 15 ||
      (value.heartbeatSeconds as number) > 60 ||
      !Array.isArray(value.supportedTopics) ||
      value.supportedTopics.some(
        (topic) => typeof topic !== 'string' || !TOPICS.has(topic as AgentEventTopic),
      )
    ) {
      throw new Error('Agent subscription readiness is invalid.')
    }
    if (value.resetRequired && !RESET_REASONS.has(value.resetReason as AgentEventReset['reason'])) {
      throw new Error('Agent subscription reset reason is invalid.')
    }
  } else if (value.type === 'subscription.resetRequired') {
    if (
      value.resetRequired !== true ||
      typeof value.resetReason !== 'string' ||
      !RESET_REASONS.has(value.resetReason as AgentEventReset['reason'])
    ) {
      throw new Error('Agent subscription reset is invalid.')
    }
  } else if (value.type !== 'subscription.heartbeat' && value.type !== 'subscription.closing') {
    throw new Error('Agent event control type is unsupported.')
  }
  return value
}

const parseStateEvent = (wire: RemotePeerRpcEvent, projectId: string): AgentStateEvent => {
  if (
    wire.eventKind !== 14 ||
    !UUID.test(wire.eventId) ||
    !safeInteger(wire.sequence) ||
    wire.sequence < 1 ||
    !safeInteger(wire.highWatermark) ||
    wire.highWatermark < wire.sequence
  ) {
    throw new Error('Agent event envelope is invalid.')
  }
  ensurePayloadSize(wire.payload)
  const value = record(wire.payload, 'state event')
  const cursor = record(value.cursor, 'event cursor')
  const data = value.data === undefined ? {} : record(value.data, 'event data')
  if (
    value.schemaVersion !== 1 ||
    value.projectId !== projectId ||
    typeof value.topic !== 'string' ||
    !TOPICS.has(value.topic as AgentEventTopic) ||
    typeof value.type !== 'string' ||
    EVENT_TOPIC_BY_TYPE[value.type] !== value.topic ||
    typeof value.aggregateType !== 'string' ||
    typeof value.aggregateId !== 'string' ||
    !UUID.test(value.aggregateId) ||
    typeof value.operation !== 'string' ||
    !OPERATIONS.has(value.operation as AgentEventOperation) ||
    !safeInteger(value.revision) ||
    typeof cursor.kind !== 'string' ||
    cursor.kind.length === 0 ||
    !safeInteger(cursor.value) ||
    (value.causationRequestId !== undefined &&
      (typeof value.causationRequestId !== 'string' || !UUID.test(value.causationRequestId)))
  ) {
    throw new Error('Agent state event payload is invalid.')
  }
  const parsedData = safeData(value.type, data)
  if (value.type === 'task.logs.available' && 'generation' in parsedData) {
    if (
      typeof parsedData.runId !== 'string' ||
      !UUID.test(parsedData.runId) ||
      typeof parsedData.generation !== 'number' ||
      parsedData.generation < 1 ||
      typeof parsedData.highWatermark !== 'number' ||
      cursor.kind !== 'task_log_bytes' ||
      cursor.value !== parsedData.highWatermark ||
      value.revision !== parsedData.highWatermark
    ) {
      throw new Error('Agent task log byte event is invalid.')
    }
  }
  return {
    eventId: wire.eventId,
    sequence: wire.sequence,
    highWatermark: wire.highWatermark,
    occurredAt: wire.occurredAt,
    schemaVersion: 1,
    projectId,
    topic: value.topic as AgentEventTopic,
    type: value.type,
    aggregateType: value.aggregateType,
    aggregateId: value.aggregateId,
    operation: value.operation as AgentEventOperation,
    revision: value.revision,
    cursor: { kind: cursor.kind, value: cursor.value as number },
    data: parsedData,
    causationRequestId: value.causationRequestId as string | undefined,
  }
}

// The event subscription is a pinned logical Peer session on the shared Relay
// carrier. It therefore cannot close Chat, Task, or File sessions when it
// reconnects after a reset, deadline, or background transition.
export const createAgentEventConnection = (
  peer: RemoteRPCClient,
  targetDeviceId: Readonly<Ref<string>>,
  projectId: Readonly<Ref<string>>,
): AgentEventConnection => {
  const state = ref<AgentEventConnectionState>('idle')
  const error = ref('')
  const ready = ref(false)
  const eventHandlers = new Set<(event: AgentStateEvent) => void | Promise<void>>()
  const resetHandlers = new Set<(reset: AgentEventReset) => void | Promise<void>>()
  let closed = false
  let generation = 0
  let pending: Promise<void> | undefined
  let reconnectTimer: ReturnType<typeof setTimeout> | undefined
  let reconnectAttempt = 0
  let lastReceived = 0
  let activeDeviceId = ''
  let activeProjectId = ''
  let subscriptionReady = false
  let heartbeatSeconds = 20
  let heartbeatGapHighWatermark = 0
  let heartbeatGapGeneration = 0
  let heartbeatGapTimer: ReturnType<typeof setTimeout> | undefined

  const clearReconnect = () => {
    if (reconnectTimer) clearTimeout(reconnectTimer)
    reconnectTimer = undefined
  }

  const clearHeartbeatGap = () => {
    if (heartbeatGapTimer) clearTimeout(heartbeatGapTimer)
    heartbeatGapTimer = undefined
    heartbeatGapHighWatermark = 0
  }

  const dispatchReset = async (reset: AgentEventReset) => {
    await Promise.all([...resetHandlers].map((handler) => handler(reset)))
  }

  const dispatchEvent = async (event: AgentStateEvent) => {
    await Promise.all([...eventHandlers].map((handler) => handler(event)))
  }

  const scheduleHeartbeatGapReconcile = (highWatermark: number, queue: AgentInboundQueue) => {
    if (closed || queue.run !== generation) return
    if (highWatermark <= lastReceived) {
      clearHeartbeatGap()
      return
    }
    heartbeatGapHighWatermark = Math.max(heartbeatGapHighWatermark, highWatermark)
    heartbeatGapGeneration = generation
    if (heartbeatGapTimer) return
    heartbeatGapTimer = setTimeout(
      () => {
        heartbeatGapTimer = undefined
        const expectedHighWatermark = heartbeatGapHighWatermark
        heartbeatGapHighWatermark = 0
        const expectedGeneration = heartbeatGapGeneration
        queue.tail = queue.tail
          .then(async () => {
            if (
              closed ||
              expectedGeneration !== generation ||
              queue.run !== generation ||
              lastReceived >= expectedHighWatermark
            ) {
              return
            }
            ready.value = false
            await dispatchReset({ reason: 'sequenceGap', highWatermark: expectedHighWatermark })
            if (closed || queue.run !== generation) return
            storeAgentEventCursor(queue.deviceId, queue.projectId, expectedHighWatermark)
            lastReceived = expectedHighWatermark
            await queue.peer.cancelAgentEventSubscriptions({ projectId: queue.projectId })
          })
          .catch((failure: unknown) => {
            if (closed || expectedGeneration !== generation || queue.run !== generation) return
            error.value =
              failure instanceof Error
                ? failure.message
                : 'Agent event heartbeat reconciliation failed.'
            ready.value = false
            void queue.peer
              .cancelAgentEventSubscriptions({ projectId: queue.projectId })
              .finally(scheduleReconnect)
          })
      },
      Math.max(1000, heartbeatSeconds * 1000),
    )
  }

  const scheduleReconnect = () => {
    if (closed || reconnectTimer || !activeDeviceId || !activeProjectId) return
    state.value = 'reconnecting'
    const delay = Math.min(15_000, 500 * 2 ** Math.min(reconnectAttempt++, 5))
    reconnectTimer = setTimeout(() => {
      reconnectTimer = undefined
      void launchSubscription()
    }, delay)
  }

  const processWireEvent = async (wire: RemotePeerRpcEvent, queue: AgentInboundQueue) => {
    if (closed || queue.run !== generation || queue.projectId !== projectId.value.trim()) {
      return
    }
    if (wire.eventKind === 13) {
      const control = parseControl(wire, queue.projectId)
      if (!subscriptionReady && control.type !== 'subscription.ready') {
        throw new Error('Agent subscription did not begin with readiness.')
      }
      if (control.type === 'subscription.ready') {
        if (subscriptionReady) throw new Error('Agent subscription repeated readiness.')
        subscriptionReady = true
        const highWatermark = control.highWatermark
        const resetRequired = control.resetRequired
        const resetReason = control.resetReason
        if (!safeInteger(highWatermark) || typeof resetRequired !== 'boolean') {
          throw new Error('Agent subscription readiness is invalid.')
        }
        if (resetRequired) {
          if (
            typeof resetReason !== 'string' ||
            !RESET_REASONS.has(resetReason as AgentEventReset['reason'])
          ) {
            throw new Error('Agent subscription reset reason is invalid.')
          }
          ready.value = false
          await dispatchReset({
            reason: resetReason as AgentEventReset['reason'],
            highWatermark,
          })
          if (closed || queue.run !== generation) return
          storeAgentEventCursor(queue.deviceId, queue.projectId, highWatermark)
          lastReceived = highWatermark
          clearHeartbeatGap()
          return
        }
        ready.value = true
        state.value = 'ready'
        reconnectAttempt = 0
        heartbeatSeconds = control.heartbeatSeconds as number
        lastReceived = readAgentEventCursor(queue.deviceId, queue.projectId) ?? 0
      } else if (control.type === 'subscription.resetRequired') {
        const highWatermark = control.highWatermark
        const reason = control.resetReason
        if (
          !safeInteger(highWatermark) ||
          typeof reason !== 'string' ||
          !RESET_REASONS.has(reason as AgentEventReset['reason'])
        ) {
          throw new Error('Agent subscription reset is invalid.')
        }
        ready.value = false
        await dispatchReset({ reason: reason as AgentEventReset['reason'], highWatermark })
        if (closed || queue.run !== generation) return
        storeAgentEventCursor(queue.deviceId, queue.projectId, highWatermark)
        lastReceived = highWatermark
        clearHeartbeatGap()
      } else if (control.type === 'subscription.heartbeat') {
        if (!safeInteger(control.highWatermark)) {
          throw new Error('Agent event heartbeat is invalid.')
        }
        scheduleHeartbeatGapReconcile(control.highWatermark, queue)
      } else if (control.type === 'subscription.closing') {
        ready.value = false
      } else {
        throw new Error('Agent event control type is unsupported.')
      }
      return
    }
    if (!subscriptionReady) {
      throw new Error('Agent event arrived before subscription readiness.')
    }
    const event = parseStateEvent(wire, queue.projectId)
    const persisted = readAgentEventCursor(queue.deviceId, queue.projectId) ?? 0
    if (event.sequence <= persisted) {
      lastReceived = Math.max(lastReceived, event.sequence)
      return
    }
    if (event.sequence !== lastReceived + 1) {
      ready.value = false
      await dispatchReset({ reason: 'sequenceGap', highWatermark: event.highWatermark })
      throw new Error('Agent event sequence has a gap.')
    }
    lastReceived = event.sequence
    await dispatchEvent(event)
    // Cursor persistence happens only once every registered domain handler
    // has completed its authoritative reconcile successfully. A slow handler
    // from a retired generation may finish after its replacement has advanced;
    // it must never move that replacement's cursor backwards.
    const current = readAgentEventCursor(queue.deviceId, queue.projectId)
    if (current === undefined || event.sequence > current) {
      storeAgentEventCursor(queue.deviceId, queue.projectId, event.sequence)
    }
    if (closed || queue.run !== generation) return
    if (lastReceived >= heartbeatGapHighWatermark) clearHeartbeatGap()
  }

  const wireEventBytes = (wire: RemotePeerRpcEvent) => {
    try {
      return new TextEncoder().encode(JSON.stringify(wire.payload)).byteLength + 192
    } catch {
      return MAXIMUM_INBOUND_QUEUE_BYTES + 1
    }
  }

  const failInboundRun = (queue: AgentInboundQueue, message: string) => {
    if (closed || queue.run !== generation) return
    generation += 1
    error.value = message
    ready.value = false
    subscriptionReady = false
    clearHeartbeatGap()
    void queue.peer
      .cancelAgentEventSubscriptions({ projectId: queue.projectId })
      .finally(scheduleReconnect)
  }

  const enqueueWireEvent = (wire: RemotePeerRpcEvent, queue: AgentInboundQueue) => {
    if (closed || queue.run !== generation) return
    const bytes = wireEventBytes(wire)
    if (
      queue.count + 1 > MAXIMUM_INBOUND_QUEUE_COUNT ||
      queue.bytes + bytes > MAXIMUM_INBOUND_QUEUE_BYTES
    ) {
      failInboundRun(queue, 'Agent 事件处理积压超过安全上限，正在从权威游标恢复。')
      return
    }
    queue.count += 1
    queue.bytes += bytes
    queue.tail = queue.tail
      .then(async () => {
        if (closed || queue.run !== generation) return
        await processWireEvent(wire, queue)
      })
      .catch((failure: unknown) => {
        failInboundRun(
          queue,
          failure instanceof Error ? failure.message : 'Agent event processing failed.',
        )
      })
      .finally(() => {
        queue.count = Math.max(0, queue.count - 1)
        queue.bytes = Math.max(0, queue.bytes - bytes)
      })
  }

  const startSubscription = async (): Promise<void> => {
    clearReconnect()
    const deviceId = targetDeviceId.value.trim()
    const selectedProject = projectId.value.trim()
    if (closed || !deviceId || !UUID.test(selectedProject)) {
      ready.value = false
      state.value = closed ? 'closed' : 'idle'
      return
    }
    const run = ++generation
    const queue: AgentInboundQueue = {
      run,
      peer,
      deviceId,
      projectId: selectedProject,
      tail: Promise.resolve(),
      count: 0,
      bytes: 0,
    }
    subscriptionReady = false
    clearHeartbeatGap()
    activeDeviceId = deviceId
    activeProjectId = selectedProject
    state.value = 'connecting'
    error.value = ''
    const activePeer = peer
    try {
      const capabilities = await activePeer.getCapabilities(true)
      if (closed || run !== generation) {
        // This is a shared physical Relay carrier. A stale event start must
        // never close the Chat, Task, or File logical sessions beside it.
        await activePeer.cancelAgentEventSubscriptions({ projectId: selectedProject })
        return
      }
      if (!supportsEvents(capabilities)) {
        ready.value = false
        state.value = 'unsupported'
        return
      }
      lastReceived = readAgentEventCursor(deviceId, selectedProject) ?? 0
      await activePeer.subscribeAgentEvents(
        lastReceived === 0
          ? { heartbeatSeconds: 20 }
          : { afterSequence: lastReceived, heartbeatSeconds: 20 },
        (wire) => {
          enqueueWireEvent(wire, queue)
        },
        { projectId: selectedProject },
      )
      if (!closed && run === generation) {
        // A completed long stream is no longer entitled to keep its pinned
        // logical session. This also gives a reset/deadline renewal fresh
        // key material and a clean pending-correlation map.
        await activePeer.cancelAgentEventSubscriptions({ projectId: selectedProject })
        scheduleReconnect()
      }
    } catch (failure) {
      if (!closed && run === generation) {
        error.value = failure instanceof Error ? failure.message : 'Agent event connection failed.'
        ready.value = false
        scheduleReconnect()
      }
    }
  }

  function launchSubscription() {
    if (pending) return pending
    const request = startSubscription().finally(() => {
      if (pending === request) pending = undefined
    })
    pending = request
    return request
  }

  const start = () => {
    closed = false
    return launchSubscription()
  }

  const stopCurrent = async () => {
    const stoppedGeneration = ++generation
    clearReconnect()
    clearHeartbeatGap()
    ready.value = false
    if (activeProjectId) {
      await peer.cancelAgentEventSubscriptions({ projectId: activeProjectId })
    }
    return stoppedGeneration
  }

  const unwatch = watch(
    () => `${targetDeviceId.value}\u0000${projectId.value}`,
    () => {
      if (!closed) {
        const previous = pending
        void (async () => {
          const stoppedGeneration = await stopCurrent()
          await previous?.catch(() => undefined)
          if (!closed && generation === stoppedGeneration) void launchSubscription()
        })()
      }
    },
  )

  return {
    state,
    error,
    ready,
    start,
    close: async (options) => {
      closed = true
      state.value = 'closed'
      clearReconnect()
      unwatch()
      if (options?.clearCursor && activeDeviceId && activeProjectId) {
        removeAgentEventCursor(activeDeviceId, activeProjectId)
      }
      await stopCurrent()
    },
    onEvent: (handler) => {
      eventHandlers.add(handler)
      return () => eventHandlers.delete(handler)
    },
    onReset: (handler) => {
      resetHandlers.add(handler)
      return () => resetHandlers.delete(handler)
    },
  }
}
