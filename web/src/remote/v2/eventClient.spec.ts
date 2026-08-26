import { create, fromBinary, toBinary } from '@bufbuild/protobuf'
import { ref } from 'vue'
import { afterEach, describe, expect, it, vi } from 'vitest'

import { FrameType } from '@/generated/remote/v2/common_pb'
import {
  EventAckSchema,
  EventResetRequiredSchema,
  EventSubscribeSchema,
  RpcEventSchema,
} from '@/generated/remote/v2/message_pb'
import { createAgentEventConnection } from '@/remote/agentEvents'
import type { RemoteAgentCapabilities } from '@/remote/peerClient'
import type { RemoteRPCClient } from '@/remote/rpcTypes'

import { V2EventClient } from './eventClient'

const projectId = '11111111-1111-4111-8111-111111111111'
const eventId = '22222222-2222-4222-8222-222222222222'

const eventCapabilities: RemoteAgentCapabilities = {
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

interface FakeFrame {
  record: { streamId: string }
  plaintext: Uint8Array
}

class FakeEventLink {
  readonly listeners = new Map<number, Set<(frame: FakeFrame) => void>>()
  readonly channels = {
    openProject: async () => {
      this.openProjectCount += 1
      await this.openProjectGate
      return { id: 'event-channel' }
    },
    retain: () => true,
    setPinned: () => undefined,
    release: () => undefined,
  }
  eventAcks = 0
  eventSubscriptions = 0
  openProjectCount = 0
  readonly acknowledgedWatermarks: bigint[] = []
  subscriptionId = ''
  streamId = ''

  constructor(
    private readonly resetRequired: boolean,
    private readonly sendAck?: (highWatermark: bigint) => Promise<void>,
    private readonly openProjectGate: Promise<void> = Promise.resolve(),
    private readonly emitReady = true,
  ) {}

  on(frameType: FrameType, listener: (frame: FakeFrame) => void) {
    const listeners = this.listeners.get(frameType) ?? new Set<(frame: FakeFrame) => void>()
    listeners.add(listener)
    this.listeners.set(frameType, listeners)
    return () => listeners.delete(listener)
  }

  async sendEncrypted(
    frameType: FrameType,
    _channelId: string,
    streamId: string,
    plaintext: Uint8Array,
  ) {
    if (frameType === FrameType.EVENT_ACK) {
      const ack = fromBinary(EventAckSchema, plaintext)
      this.eventAcks += 1
      this.acknowledgedWatermarks.push(ack.highWatermark)
      return this.sendAck?.(ack.highWatermark)
    }
    if (frameType !== FrameType.EVENT_SUBSCRIBE) return
    this.eventSubscriptions += 1
    const subscription = fromBinary(EventSubscribeSchema, plaintext)
    this.subscriptionId = subscription.subscriptionId
    this.streamId = streamId
    if (!this.emitReady) return
    const payload = new TextEncoder().encode(
      JSON.stringify({
        schemaVersion: 1,
        type: 'subscription.ready',
        projectId,
        highWatermark: 0,
        minimumAvailableSequence: 1,
        resetRequired: this.resetRequired,
        resetReason: this.resetRequired ? 'bootstrap' : '',
        heartbeatSeconds: 60,
        supportedTopics: [
          'agent',
          'capabilities',
          'conversation',
          'message',
          'task',
          'taskLog',
          'workflow',
        ],
      }),
    )
    const event = toBinary(
      RpcEventSchema,
      create(RpcEventSchema, {
        operationId: subscription.subscriptionId,
        eventId,
        eventSequence: 0n,
        highWatermark: 0n,
        payload,
      }),
    )
    for (const listener of this.listeners.get(FrameType.RPC_EVENT) ?? []) {
      listener({ record: { streamId }, plaintext: event })
    }
  }

  emit(sequence: bigint) {
    const payload = new TextEncoder().encode(JSON.stringify({ sequence: Number(sequence) }))
    const event = toBinary(
      RpcEventSchema,
      create(RpcEventSchema, {
        operationId: this.subscriptionId,
        eventId: crypto.randomUUID(),
        eventSequence: sequence,
        highWatermark: sequence,
        payload,
      }),
    )
    for (const listener of this.listeners.get(FrameType.RPC_EVENT) ?? []) {
      listener({ record: { streamId: this.streamId }, plaintext: event })
    }
  }

  emitReset(highWatermark: bigint) {
    const reset = toBinary(
      EventResetRequiredSchema,
      create(EventResetRequiredSchema, {
        subscriptionId: this.subscriptionId,
        currentHighWatermark: highWatermark,
      }),
    )
    for (const listener of this.listeners.get(FrameType.EVENT_RESET_REQUIRED) ?? []) {
      listener({ record: { streamId: this.streamId }, plaintext: reset })
    }
  }
}

describe('remote/v2 event low-wakeup acknowledgements', () => {
  afterEach(() => vi.useRealTimers())

  it('ACKs a healthy control heartbeat in the receive task without a periodic timer', async () => {
    vi.useFakeTimers()
    const link = new FakeEventLink(false)
    const events = new V2EventClient(link as never)

    let settled = false
    const lifetime = events.subscribe({}, () => undefined, { projectId })
    void lifetime.then(
      () => (settled = true),
      () => (settled = true),
    )
    await vi.waitFor(() => expect(link.eventAcks).toBe(1))
    expect(settled).toBe(false)
    await vi.advanceTimersByTimeAsync(120_000)
    expect(link.eventAcks).toBe(1)

    events.dispose()
    await expect(lifetime).rejects.toThrow('已关闭')
  })

  it('does not ACK a Ready frame that retires the subscription for reset', async () => {
    const link = new FakeEventLink(true)
    const events = new V2EventClient(link as never)

    await events.subscribe({}, () => undefined, { projectId })
    expect(link.eventAcks).toBe(0)

    events.dispose()
  })

  it('keeps one ACK in flight and coalesces queued events to the latest watermark', async () => {
    let releaseFirstAck!: () => void
    const firstAck = new Promise<void>((resolve) => {
      releaseFirstAck = resolve
    })
    let ackAttempt = 0
    const link = new FakeEventLink(false, async () => {
      ackAttempt += 1
      if (ackAttempt === 1) await firstAck
    })
    const events = new V2EventClient(link as never)

    const lifetime = events.subscribe({}, () => undefined, { projectId })
    await vi.waitFor(() => expect(link.eventAcks).toBe(1))
    link.emit(1n)
    link.emit(2n)
    expect(link.acknowledgedWatermarks).toEqual([0n])

    releaseFirstAck()
    await vi.waitFor(() => expect(link.acknowledgedWatermarks).toEqual([0n, 2n]))

    events.dispose()
    await expect(lifetime).rejects.toThrow('已关闭')
  })

  it('retains a subscription after advisory ACK failure and retries it on resume', async () => {
    let fail = true
    const link = new FakeEventLink(false, async () => {
      if (fail) {
        fail = false
        throw new Error('carrier interrupted')
      }
    })
    const events = new V2EventClient(link as never)

    const lifetime = events.subscribe({}, () => undefined, { projectId })
    await vi.waitFor(() => expect(events.carrierAcks()).toHaveLength(1))
    await events.resumeAll()
    await vi.waitFor(() => expect(link.acknowledgedWatermarks).toEqual([0n, 0n]))

    events.dispose()
    await expect(lifetime).rejects.toThrow('已关闭')
  })

  it('shares one lifetime across concurrent owners and ends it only on reset', async () => {
    const link = new FakeEventLink(false)
    const events = new V2EventClient(link as never)

    const first = events.subscribe({}, () => undefined, { projectId })
    const second = events.subscribe({}, () => undefined, { projectId })
    expect(second).toBe(first)
    await vi.waitFor(() => expect(link.eventSubscriptions).toBe(1))

    link.emitReset(0n)
    await expect(first).resolves.toBeUndefined()
    await expect(second).resolves.toBeUndefined()
    expect(events.carrierAcks()).toHaveLength(0)
  })

  it('does not create a ghost Stream when cancellation wins the Channel-open race', async () => {
    let releaseChannel!: () => void
    const channelGate = new Promise<void>((resolve) => {
      releaseChannel = resolve
    })
    const link = new FakeEventLink(false, undefined, channelGate)
    const events = new V2EventClient(link as never)

    const lifetime = events.subscribe({}, () => undefined, { projectId })
    await vi.waitFor(() => expect(link.openProjectCount).toBe(1))
    await events.cancel({ projectId })
    releaseChannel()

    await expect(lifetime).rejects.toThrow('打开期间已取消')
    expect(link.eventSubscriptions).toBe(0)
    expect(events.carrierAcks()).toHaveLength(0)
  })

  it('fails a subscription that never receives its readiness frame', async () => {
    vi.useFakeTimers()
    const link = new FakeEventLink(false, undefined, Promise.resolve(), false)
    const events = new V2EventClient(link as never)

    const lifetime = events.subscribe({}, () => undefined, { projectId })
    const rejected = expect(lifetime).rejects.toThrow('就绪超时')
    await vi.advanceTimersByTimeAsync(15_001)

    await rejected
    expect(events.carrierAcks()).toHaveLength(0)
    events.dispose()
  })

  it('fails a silent Event Stream after the negotiated heartbeat grace', async () => {
    vi.useFakeTimers()
    const link = new FakeEventLink(false)
    const events = new V2EventClient(link as never)

    const lifetime = events.subscribe({}, () => undefined, { projectId })
    const rejected = expect(lifetime).rejects.toThrow('心跳超时')
    await vi.waitFor(() => expect(link.eventAcks).toBe(1))
    await vi.advanceTimersByTimeAsync(123_001)

    await rejected
    expect(events.carrierAcks()).toHaveLength(0)
    events.dispose()
  })

  it('rejects a durable state event that arrives before readiness', async () => {
    const link = new FakeEventLink(false, undefined, Promise.resolve(), false)
    const events = new V2EventClient(link as never)

    const lifetime = events.subscribe({}, () => undefined, { projectId })
    await vi.waitFor(() => expect(link.eventSubscriptions).toBe(1))
    link.emit(1n)

    await expect(lifetime).rejects.toThrow('就绪前')
    expect(events.carrierAcks()).toHaveLength(0)
    events.dispose()
  })

  it('keeps the real project-event connection alive after readiness', async () => {
    const link = new FakeEventLink(false)
    const events = new V2EventClient(link as never)
    const cancelAgentEventSubscriptions = vi.fn((context) => events.cancel(context))
    const peer = {
      connected: ref(true),
      reconnecting: ref(false),
      error: ref(''),
      connect: vi.fn(async () => undefined),
      close: vi.fn(async () => undefined),
      getCapabilities: vi.fn(async () => eventCapabilities),
      call: vi.fn(),
      stream: vi.fn(),
      startStream: vi.fn(),
      subscribeAgentEvents: (...args: Parameters<RemoteRPCClient['subscribeAgentEvents']>) =>
        events.subscribe(...args),
      cancelAgentEventSubscriptions,
      downloadFile: vi.fn(),
      downloadTaskLog: vi.fn(),
      uploadFile: vi.fn(),
    } as unknown as RemoteRPCClient
    const connection = createAgentEventConnection(peer, ref('device-1'), ref(projectId))

    const lifetime = connection.start()
    await vi.waitFor(() => expect(connection.state.value).toBe('ready'))
    expect(cancelAgentEventSubscriptions).not.toHaveBeenCalled()
    expect(link.eventSubscriptions).toBe(1)

    await connection.close()
    await expect(lifetime).resolves.toBeUndefined()
    expect(cancelAgentEventSubscriptions).toHaveBeenCalledTimes(1)
  })
})
