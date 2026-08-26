import { create, fromBinary, toBinary } from '@bufbuild/protobuf'

import { StreamCloseSchema, StreamOpenSchema } from '@/generated/remote/v2/channel_pb'
import { FrameType, ProtocolErrorCode, StreamKind } from '@/generated/remote/v2/common_pb'
import {
  EventAckSchema,
  EventResetRequiredSchema,
  EventResumeSchema,
  EventSubscribeSchema,
  RpcEventSchema,
} from '@/generated/remote/v2/message_pb'
import type { RemotePeerRpcEvent } from '@/remote/peerClient'
import type { RemoteRPCContext } from '@/remote/rpcTypes'

import { V2_CHANNEL_CONTROL_STREAM_ID, decodeUtf8 } from './crypto'
import { V2Link } from './link'

const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/iu

interface Subscription {
  id: string
  channelId: string
  streamId: string
  projectId: string
  highWatermark: bigint
  acknowledged: bigint
  ackTarget: bigint
  ackInFlight: boolean
  ready: boolean
  readyTimer: ReturnType<typeof setTimeout>
  heartbeatSeconds: number
  livenessTimer?: ReturnType<typeof setTimeout>
  done: Promise<void>
  onEvent: (event: RemotePeerRpcEvent) => void
  resolve: () => void
  reject: (error: Error) => void
}

interface OpeningSubscription {
  cancelled: boolean
  promise: Promise<void>
}

/** Durable project-event Streams with encrypted ACK and resume cursors. */
export class V2EventClient {
  private readonly subscriptions = new Map<string, Subscription>()
  private readonly openings = new Map<string, OpeningSubscription>()
  private readonly detachListeners: Array<() => void>
  private disposed = false

  constructor(private readonly link: V2Link) {
    this.detachListeners = [
      link.on(FrameType.RPC_EVENT, (frame) =>
        this.handleEvent(frame.record.streamId, fromBinary(RpcEventSchema, frame.plaintext)),
      ),
      link.on(FrameType.EVENT_RESET_REQUIRED, (frame) =>
        this.handleReset(
          frame.record.streamId,
          fromBinary(EventResetRequiredSchema, frame.plaintext),
        ),
      ),
      link.on(FrameType.STREAM_CLOSE, (frame) =>
        this.rejectByStream(frame.record.streamId, new Error('远程事件 Stream 已关闭。')),
      ),
    ]
  }

  subscribe(
    input: { afterSequence?: number; heartbeatSeconds?: number },
    onEvent: (event: RemotePeerRpcEvent) => void,
    context: RemoteRPCContext,
  ) {
    const projectId = context.projectId?.trim()
    if (!projectId) return Promise.reject(new Error('请先选择一个可用项目，再订阅事件。'))
    if (this.disposed) return Promise.reject(new Error('远程事件客户端已关闭。'))
    const existing = [...this.subscriptions.values()].find(
      (subscription) => subscription.projectId === projectId,
    )
    if (existing) return existing.done
    const activeOpening = this.openings.get(projectId)
    if (activeOpening) return activeOpening.promise
    const opening: OpeningSubscription = { cancelled: false, promise: Promise.resolve() }
    opening.promise = this.openSubscription(input, onEvent, projectId, opening)
    this.openings.set(projectId, opening)
    return opening.promise
  }

  private async openSubscription(
    input: { afterSequence?: number; heartbeatSeconds?: number },
    onEvent: (event: RemotePeerRpcEvent) => void,
    projectId: string,
    opening: OpeningSubscription,
  ) {
    let channel: { id: string }
    try {
      channel = await this.link.channels.openProject(projectId, ['remote.peer.events'])
      if (this.disposed || opening.cancelled) {
        throw new Error('远程事件订阅在打开期间已取消。')
      }
    } finally {
      if (this.openings.get(projectId) === opening) this.openings.delete(projectId)
    }
    // A second owner can race the first lookup while the reusable Channel is
    // opening. Re-check after await so only one Event Stream owns a project.
    const raced = [...this.subscriptions.values()].find(
      (subscription) => subscription.projectId === projectId,
    )
    if (raced) return raced.done
    this.link.channels.retain(channel.id)
    this.link.channels.setPinned(channel.id, true)
    const id = crypto.randomUUID()
    const streamId = crypto.randomUUID()
    const after =
      Number.isSafeInteger(input.afterSequence) && (input.afterSequence ?? 0) >= 0
        ? (input.afterSequence ?? 0)
        : 0
    let resolveDone!: () => void
    let rejectDone!: (error: Error) => void
    const done = new Promise<void>((resolve, reject) => {
      resolveDone = resolve
      rejectDone = reject
    })
    const subscription: Subscription = {
      id,
      channelId: channel.id,
      streamId,
      projectId,
      highWatermark: BigInt(after),
      // Force an advisory ACK for the initial Ready/heartbeat frame even
      // when its high watermark equals the requested resume cursor.
      acknowledged: -1n,
      ackTarget: BigInt(after),
      ackInFlight: false,
      ready: false,
      readyTimer: setTimeout(
        () => this.reject(id, new Error('等待远程事件订阅就绪超时。')),
        15_000,
      ),
      heartbeatSeconds: 60,
      done,
      onEvent,
      resolve: resolveDone,
      reject: rejectDone,
    }
    this.subscriptions.set(id, subscription)
    try {
      await this.link.sendEncrypted(
        FrameType.STREAM_OPEN,
        channel.id,
        V2_CHANNEL_CONTROL_STREAM_ID,
        toBinary(
          StreamOpenSchema,
          create(StreamOpenSchema, {
            channelId: channel.id,
            streamId,
            kind: StreamKind.EVENT,
            operationId: id,
          }),
        ),
      )
      if (this.subscriptions.get(id) !== subscription) {
        throw new Error('远程事件订阅在打开期间已取消。')
      }
      await this.link.sendEncrypted(
        FrameType.EVENT_SUBSCRIBE,
        channel.id,
        streamId,
        toBinary(
          EventSubscribeSchema,
          create(EventSubscribeSchema, { subscriptionId: id, afterSequence: BigInt(after) }),
        ),
      )
      if (this.subscriptions.get(id) !== subscription) {
        throw new Error('远程事件订阅在打开期间已取消。')
      }
      return done
    } catch (error) {
      this.reject(id, error instanceof Error ? error : new Error('无法订阅远程事件。'))
      return done
    }
  }

  async resumeAll() {
    await Promise.all(
      [...this.subscriptions.values()].map(async (subscription) => {
        await this.link.sendEncrypted(
          FrameType.EVENT_RESUME,
          subscription.channelId,
          subscription.streamId,
          toBinary(
            EventResumeSchema,
            create(EventResumeSchema, {
              subscriptionId: subscription.id,
              afterSequence: subscription.highWatermark,
            }),
          ),
        )
        this.ack(subscription)
      }),
    )
  }

  carrierAcks() {
    return [...this.subscriptions.values()].map((subscription) => ({
      channelId: subscription.channelId,
      streamId: subscription.streamId,
      acknowledgedSequence: subscription.highWatermark,
    }))
  }

  async cancel(context: RemoteRPCContext) {
    const projectId = context.projectId?.trim()
    const opening = projectId ? this.openings.get(projectId) : undefined
    if (opening) opening.cancelled = true
    const subscriptions = [...this.subscriptions.values()].filter(
      (subscription) => subscription.projectId === projectId,
    )
    await Promise.all(
      subscriptions.map(async (subscription) => {
        this.reject(subscription.id, new Error('远程事件订阅已取消。'))
        await this.link
          .sendEncrypted(
            FrameType.STREAM_CLOSE,
            subscription.channelId,
            V2_CHANNEL_CONTROL_STREAM_ID,
            toBinary(
              StreamCloseSchema,
              create(StreamCloseSchema, {
                channelId: subscription.channelId,
                streamId: subscription.streamId,
                reason: ProtocolErrorCode.STREAM_CANCELLED,
              }),
            ),
          )
          .catch(() => undefined)
      }),
    )
  }

  dispose() {
    if (this.disposed) return
    this.disposed = true
    for (const listener of this.detachListeners) listener()
    for (const opening of this.openings.values()) opening.cancelled = true
    this.openings.clear()
    for (const subscription of [...this.subscriptions.values()]) {
      this.reject(subscription.id, new Error('远程事件客户端已关闭。'))
    }
  }

  private handleEvent(
    streamId: string,
    event: {
      operationId: string
      eventSequence: bigint
      payload: Uint8Array
      eventId: string
      highWatermark: bigint
    },
  ) {
    const subscription = [...this.subscriptions.values()].find(
      (candidate) => candidate.streamId === streamId && candidate.id === event.operationId,
    )
    if (!subscription) return
    let payload: unknown
    try {
      payload = JSON.parse(decodeUtf8(event.payload)) as unknown
    } catch {
      this.reject(subscription.id, new Error('设备返回了无效事件数据。'))
      return
    }
    const sequence = Number(event.eventSequence)
    const highWatermark = Number(event.highWatermark)
    if (
      !UUID.test(event.eventId) ||
      !Number.isSafeInteger(sequence) ||
      sequence < 0 ||
      !Number.isSafeInteger(highWatermark) ||
      highWatermark < sequence
    ) {
      this.reject(subscription.id, new Error('设备返回了无效事件序号。'))
      return
    }
    if (sequence === 0) {
      if (!isControlHighWatermark(payload, highWatermark)) {
        this.reject(subscription.id, new Error('设备返回了无效事件控制水位。'))
        return
      }
      const type = controlEventType(payload)
      if (
        (!subscription.ready && type !== 'subscription.ready') ||
        (subscription.ready && type === 'subscription.ready')
      ) {
        this.reject(subscription.id, new Error('设备返回了无效的事件就绪顺序。'))
        return
      }
      try {
        subscription.onEvent({
          eventId: event.eventId,
          eventKind: 13,
          requestId: subscription.id,
          sequence,
          highWatermark,
          payload,
        })
      } catch (error) {
        this.reject(
          subscription.id,
          error instanceof Error ? error : new Error('远程事件处理失败。'),
        )
        return
      }
      if (type === 'subscription.ready') {
        const heartbeatSeconds = controlHeartbeatSeconds(payload)
        if (heartbeatSeconds === undefined) {
          this.reject(subscription.id, new Error('设备返回了无效的事件心跳间隔。'))
          return
        }
        subscription.heartbeatSeconds = heartbeatSeconds
        subscription.ready = true
        clearTimeout(subscription.readyTimer)
      }
      if (subscription.ready) this.armLiveness(subscription)
      // The Agent control heartbeat already defines the event-liveness
      // cadence. ACK it in the same task instead of waking a second timer.
      const resetRequired =
        !!payload &&
        typeof payload === 'object' &&
        !Array.isArray(payload) &&
        (payload as Record<string, unknown>).resetRequired === true
      if (!resetRequired && subscription.highWatermark <= BigInt(highWatermark)) {
        this.ack(subscription)
      } else if (resetRequired) {
        this.complete(subscription)
        subscription.resolve()
      }
      return
    }
    if (!subscription.ready) {
      this.reject(subscription.id, new Error('设备在订阅就绪前返回了状态事件。'))
      return
    }
    this.armLiveness(subscription)
    if (event.eventSequence <= subscription.highWatermark) {
      this.ack(subscription)
      return
    }
    if (event.eventSequence !== subscription.highWatermark + 1n) {
      this.reject(subscription.id, new Error('远程事件序号不连续。'))
      return
    }
    subscription.highWatermark = event.eventSequence
    try {
      subscription.onEvent({
        eventId: event.eventId,
        eventKind: sequence === 0 ? 13 : 14,
        requestId: subscription.id,
        sequence,
        highWatermark,
        payload,
      })
    } catch (error) {
      this.reject(subscription.id, error instanceof Error ? error : new Error('远程事件处理失败。'))
      return
    }
    this.ack(subscription)
  }

  private handleReset(
    streamId: string,
    reset: { subscriptionId: string; currentHighWatermark: bigint },
  ) {
    const subscription = this.subscriptions.get(reset.subscriptionId)
    if (!subscription || subscription.streamId !== streamId) return
    const highWatermark = Number(reset.currentHighWatermark)
    if (!Number.isSafeInteger(highWatermark) || highWatermark < 0) {
      this.reject(subscription.id, new Error('设备返回了无效事件重置水位。'))
      return
    }
    try {
      subscription.onEvent({
        eventId: crypto.randomUUID(),
        eventKind: 13,
        requestId: subscription.id,
        sequence: 0,
        highWatermark,
        payload: {
          schemaVersion: 1,
          type: 'subscription.resetRequired',
          projectId: subscription.projectId,
          highWatermark,
          resetRequired: true,
          resetReason: 'sequenceGap',
        },
      })
    } catch (error) {
      this.reject(subscription.id, error instanceof Error ? error : new Error('远程事件处理失败。'))
      return
    }
    if (!subscription.ready) {
      subscription.ready = true
    }
    this.complete(subscription)
    subscription.resolve()
  }

  private rejectByStream(streamId: string, error: Error) {
    const subscription = [...this.subscriptions.values()].find(
      (candidate) => candidate.streamId === streamId,
    )
    if (subscription) this.reject(subscription.id, error)
  }

  private reject(id: string, error: Error) {
    const subscription = this.subscriptions.get(id)
    if (!subscription) return
    this.complete(subscription)
    subscription.reject(error)
  }

  private complete(subscription: Subscription) {
    clearTimeout(subscription.readyTimer)
    if (subscription.livenessTimer) clearTimeout(subscription.livenessTimer)
    this.subscriptions.delete(subscription.id)
    this.link.channels.setPinned(subscription.channelId, false)
    this.link.channels.release(subscription.channelId)
  }

  private ack(subscription: Subscription) {
    if (this.subscriptions.get(subscription.id) !== subscription) return
    if (subscription.highWatermark > subscription.ackTarget) {
      subscription.ackTarget = subscription.highWatermark
    }
    if (subscription.ackInFlight || subscription.ackTarget <= subscription.acknowledged) return
    subscription.ackInFlight = true
    void this.flushAck(subscription)
  }

  private armLiveness(subscription: Subscription) {
    if (this.subscriptions.get(subscription.id) !== subscription || !subscription.ready) return
    if (subscription.livenessTimer) clearTimeout(subscription.livenessTimer)
    subscription.livenessTimer = setTimeout(
      () => this.reject(subscription.id, new Error('远程事件订阅心跳超时。')),
      Math.max(15_000, subscription.heartbeatSeconds * 2_000 + 3_000),
    )
  }

  private async flushAck(subscription: Subscription) {
    while (this.subscriptions.get(subscription.id) === subscription) {
      const target = subscription.ackTarget
      try {
        await this.link.sendEncrypted(
          FrameType.EVENT_ACK,
          subscription.channelId,
          subscription.streamId,
          toBinary(
            EventAckSchema,
            create(EventAckSchema, {
              subscriptionId: subscription.id,
              highWatermark: target,
            }),
          ),
        )
      } catch {
        // EVENT_ACK is advisory. The local high watermark is carried by both
        // Carrier resume and EVENT_RESUME, so a transient ACK failure must not
        // destroy an otherwise recoverable subscription.
        subscription.ackInFlight = false
        return
      }
      if (this.subscriptions.get(subscription.id) !== subscription) {
        subscription.ackInFlight = false
        return
      }
      if (target > subscription.acknowledged) subscription.acknowledged = target
      if (subscription.ackTarget <= subscription.acknowledged) {
        subscription.ackInFlight = false
        return
      }
    }
    subscription.ackInFlight = false
  }
}

const isControlHighWatermark = (payload: unknown, highWatermark: number) =>
  !!payload &&
  typeof payload === 'object' &&
  !Array.isArray(payload) &&
  Number.isSafeInteger((payload as Record<string, unknown>).highWatermark) &&
  (payload as Record<string, unknown>).highWatermark === highWatermark

const controlEventType = (payload: unknown) =>
  !!payload && typeof payload === 'object' && !Array.isArray(payload)
    ? (payload as Record<string, unknown>).type
    : undefined

const controlHeartbeatSeconds = (payload: unknown) => {
  if (!payload || typeof payload !== 'object' || Array.isArray(payload)) return undefined
  const value = (payload as Record<string, unknown>).heartbeatSeconds
  return Number.isSafeInteger(value) && (value as number) >= 15 && (value as number) <= 60
    ? (value as number)
    : undefined
}
