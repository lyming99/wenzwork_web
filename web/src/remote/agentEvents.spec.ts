import { ref } from 'vue'
import { afterEach, describe, expect, it, vi } from 'vitest'

import { readAgentEventCursor } from './agentEventCursor'
import { createAgentEventConnection, type AgentStateEvent } from './agentEvents'
import type { RemoteAgentCapabilities, RemotePeerRpcEvent } from './peerClient'
import type { RemoteRPCClient } from './rpcTypes'

const projectId = '11111111-1111-4111-8111-111111111111'
const replacementProjectId = '99999999-9999-4999-8999-999999999999'
const requestId = '22222222-2222-4222-8222-222222222222'
const controlId = '33333333-3333-4333-8333-333333333333'
const aggregateId = '44444444-4444-4444-8444-444444444444'

const capabilities: RemoteAgentCapabilities = {
  protocolMinimum: 1,
  protocolMaximum: 1,
  featureVersions: { projects: 2, events: 1 },
  features: { 'project.v2': true, 'events.v1': true },
  operatingSystem: 'test',
  architecture: 'test',
  shells: [],
  taskRunners: [],
  resourceLimits: {},
}

const ready = (): RemotePeerRpcEvent => ({
  eventId: controlId,
  eventKind: 13,
  requestId,
  sequence: 0,
  highWatermark: 0,
  payload: {
    schemaVersion: 1,
    type: 'subscription.ready',
    projectId,
    minimumAvailableSequence: 1,
    highWatermark: 0,
    resetRequired: false,
    resetReason: '',
    heartbeatSeconds: 20,
    supportedTopics: [
      'agent',
      'capabilities',
      'conversation',
      'message',
      'task',
      'taskLog',
      'workflow',
    ],
  },
})

const taskChanged = (
  data: Record<string, unknown> = { status: 'running' },
): RemotePeerRpcEvent => ({
  eventId: '55555555-5555-4555-8555-555555555555',
  eventKind: 14,
  requestId,
  sequence: 1,
  highWatermark: 1,
  payload: {
    schemaVersion: 1,
    projectId,
    topic: 'task',
    type: 'task.changed',
    aggregateType: 'task',
    aggregateId,
    operation: 'upsert',
    revision: 1,
    cursor: { kind: 'task_changes', value: 1 },
    data,
  },
})

const sequencedTaskChanged = (sequence: number): RemotePeerRpcEvent => {
  const event = taskChanged()
  return {
    ...event,
    eventId: `00000000-0000-4000-8000-${sequence.toString().padStart(12, '0')}`,
    sequence,
    highWatermark: sequence,
    payload: {
      ...(event.payload as Record<string, unknown>),
      revision: sequence,
      cursor: { kind: 'task_changes', value: sequence },
    },
  }
}

const taskLogBytes = (generation: unknown = 1, sequence = 1): RemotePeerRpcEvent => ({
  eventId:
    sequence === 1
      ? '66666666-6666-4666-8666-666666666666'
      : '88888888-8888-4888-8888-888888888888',
  eventKind: 14,
  requestId,
  sequence,
  highWatermark: sequence,
  payload: {
    schemaVersion: 1,
    projectId,
    topic: 'taskLog',
    type: 'task.logs.available',
    aggregateType: 'task',
    aggregateId,
    operation: 'status',
    revision: 32768,
    cursor: { kind: 'task_log_bytes', value: 32768 },
    data: {
      runId: '77777777-7777-4777-8777-777777777777',
      generation,
      highWatermark: 32768,
    },
  },
})

const settle = async () => {
  await Promise.resolve()
  await Promise.resolve()
  await new Promise((resolve) => setTimeout(resolve, 0))
}

const createPeer = () => {
  let emit: ((event: RemotePeerRpcEvent) => void) | undefined
  let finish!: () => void
  const done = new Promise<void>((resolve) => {
    finish = resolve
  })
  const cancelAgentEventSubscriptions = vi.fn(async () => finish())
  const peer = {
    connected: ref(false),
    reconnecting: ref(false),
    error: ref(''),
    connect: vi.fn(async () => undefined),
    close: vi.fn(async () => undefined),
    getCapabilities: vi.fn(async () => capabilities),
    call: vi.fn(),
    stream: vi.fn(),
    subscribeAgentEvents: vi.fn(async (_input, onEvent) => {
      emit = onEvent
      await done
    }),
    cancelAgentEventSubscriptions,
    downloadFile: vi.fn(),
    downloadTaskLog: vi.fn(),
    uploadFile: vi.fn(),
  } as unknown as RemoteRPCClient
  return { peer, emit: (event: RemotePeerRpcEvent) => emit?.(event), cancelAgentEventSubscriptions }
}

afterEach(() => localStorage.clear())

describe('Agent event connection', () => {
  it('requires ready before applying a state hint and isolates the event subscription', async () => {
    const fake = createPeer()
    const connection = createAgentEventConnection(fake.peer, ref('device-1'), ref(projectId))
    void connection.start()
    await settle()

    fake.emit(taskChanged())
    await settle()

    expect(connection.ready.value).toBe(false)
    expect(connection.error.value).toContain('readiness')
    expect(fake.cancelAgentEventSubscriptions).toHaveBeenCalledWith({ projectId })
    await connection.close()
  })

  it('accepts a safe ready/state sequence and rejects forbidden payload fields', async () => {
    const fake = createPeer()
    const connection = createAgentEventConnection(fake.peer, ref('device-1'), ref(projectId))
    const received: string[] = []
    connection.onEvent((event) => {
      received.push(event.type)
    })
    void connection.start()
    await settle()

    fake.emit(ready())
    fake.emit(taskChanged())
    await settle()

    expect(connection.ready.value).toBe(true)
    expect(received).toEqual(['task.changed'])

    fake.emit(taskChanged({ content: 'must never enter the generic event stream' }))
    await settle()

    expect(connection.error.value).toContain('payload')
    expect(fake.cancelAgentEventSubscriptions).toHaveBeenCalled()
    await connection.close()
  })

  it('validates generation and byte cursor binding for task-log hints', async () => {
    const fake = createPeer()
    const connection = createAgentEventConnection(fake.peer, ref('device-1'), ref(projectId))
    const received: AgentStateEvent[] = []
    connection.onEvent((event) => {
      received.push(event)
    })
    void connection.start()
    await settle()

    fake.emit(ready())
    fake.emit(taskLogBytes())
    await settle()
    expect(received[0]).toMatchObject({
      type: 'task.logs.available',
      cursor: { kind: 'task_log_bytes', value: 32768 },
      data: { generation: 1, highWatermark: 32768 },
    })

    fake.emit(taskLogBytes(0, 2))
    await settle()
    expect(connection.error.value).toContain('task log byte event')
    await connection.close()
  })

  it('bounds a slow inbound handler queue and discards the stale generation', async () => {
    let releaseHandler!: () => void
    const handlerBlocked = new Promise<void>((resolve) => {
      releaseHandler = resolve
    })
    const subscriptions: Array<{
      emit: (event: RemotePeerRpcEvent) => void
      finish: () => void
      finished: boolean
    }> = []
    const subscribeAgentEvents = vi.fn(
      async (
        input: { afterSequence?: number; heartbeatSeconds?: number },
        onEvent: (event: RemotePeerRpcEvent) => void,
        context: { projectId?: string },
      ) => {
        void input
        void context
        await new Promise<void>((resolve) => {
          subscriptions.push({ emit: onEvent, finish: resolve, finished: false })
        })
      },
    )
    const cancelAgentEventSubscriptions = vi.fn(async (context: { projectId?: string }) => {
      if (context.projectId !== projectId) return
      for (let index = subscriptions.length - 1; index >= 0; index -= 1) {
        const subscription = subscriptions[index]
        if (subscription && !subscription.finished) {
          subscription.finished = true
          subscription.finish()
          return
        }
      }
    })
    const peer = {
      connected: ref(false),
      reconnecting: ref(false),
      error: ref(''),
      connect: vi.fn(async () => undefined),
      close: vi.fn(async () => undefined),
      getCapabilities: vi.fn(async () => capabilities),
      call: vi.fn(),
      stream: vi.fn(),
      subscribeAgentEvents,
      cancelAgentEventSubscriptions,
      downloadFile: vi.fn(),
      downloadTaskLog: vi.fn(),
      uploadFile: vi.fn(),
    } as unknown as RemoteRPCClient
    const connection = createAgentEventConnection(peer, ref('device-1'), ref(projectId))
    const received: number[] = []
    connection.onEvent(async (event) => {
      received.push(event.sequence)
      if (received.length === 1) await handlerBlocked
    })
    void connection.start()
    await vi.waitFor(() => expect(subscriptions).toHaveLength(1))

    subscriptions[0]?.emit(ready())
    await vi.waitFor(() => expect(connection.ready.value).toBe(true))
    subscriptions[0]?.emit(sequencedTaskChanged(1))
    await vi.waitFor(() => expect(received).toEqual([1]))

    for (let sequence = 2; sequence <= 300; sequence += 1) {
      subscriptions[0]?.emit(sequencedTaskChanged(sequence))
    }

    await vi.waitFor(() => {
      expect(connection.ready.value).toBe(false)
      expect(connection.error.value).toContain('积压超过安全上限')
      expect(cancelAgentEventSubscriptions).toHaveBeenCalledTimes(1)
    })
    expect(peer.close).not.toHaveBeenCalled()

    // A replacement subscription must not sit behind the blocked old Promise
    // tail. It can reach ready before the stale handler is released.
    await vi.waitFor(() => expect(subscriptions).toHaveLength(2), { timeout: 2_000 })
    subscriptions[1]?.emit(ready())
    await vi.waitFor(() => expect(connection.ready.value).toBe(true))
    subscriptions[1]?.emit(sequencedTaskChanged(1))
    subscriptions[1]?.emit(sequencedTaskChanged(2))
    await vi.waitFor(() => expect(received).toEqual([1, 1, 2]))

    releaseHandler()
    await settle()
    expect(received).toEqual([1, 1, 2])
    expect(readAgentEventCursor('device-1', projectId)).toBe(2)
    await connection.close()
  })

  it('starts only the latest project after an opening subscription is cancelled', async () => {
    let releaseFirst!: () => void
    let releaseSecond!: () => void
    const firstLifetime = new Promise<void>((resolve) => {
      releaseFirst = resolve
    })
    const secondLifetime = new Promise<void>((resolve) => {
      releaseSecond = resolve
    })
    let calls = 0
    const subscribeAgentEvents = vi.fn(
      async (
        input: { afterSequence?: number; heartbeatSeconds?: number },
        onEvent: (event: RemotePeerRpcEvent) => void,
        context: { projectId?: string },
      ) => {
        void input
        void onEvent
        void context
        calls += 1
        await (calls === 1 ? firstLifetime : secondLifetime)
      },
    )
    const cancelAgentEventSubscriptions = vi.fn(async (context: { projectId?: string }) => {
      if (context.projectId === replacementProjectId) releaseSecond()
    })
    const peer = {
      connected: ref(false),
      reconnecting: ref(false),
      error: ref(''),
      connect: vi.fn(async () => undefined),
      close: vi.fn(async () => undefined),
      getCapabilities: vi.fn(async () => capabilities),
      call: vi.fn(),
      stream: vi.fn(),
      subscribeAgentEvents,
      cancelAgentEventSubscriptions,
      downloadFile: vi.fn(),
      downloadTaskLog: vi.fn(),
      uploadFile: vi.fn(),
    } as unknown as RemoteRPCClient
    const selectedProject = ref(projectId)
    const connection = createAgentEventConnection(peer, ref('device-1'), selectedProject)
    void connection.start()
    await vi.waitFor(() => expect(subscribeAgentEvents).toHaveBeenCalledTimes(1))

    selectedProject.value = replacementProjectId
    await settle()
    expect(subscribeAgentEvents).toHaveBeenCalledTimes(1)
    releaseFirst()

    await vi.waitFor(() => {
      expect(subscribeAgentEvents).toHaveBeenCalledTimes(2)
      expect(subscribeAgentEvents.mock.calls[1]?.[2]).toEqual({
        projectId: replacementProjectId,
      })
    })
    await connection.close()
  })
})
