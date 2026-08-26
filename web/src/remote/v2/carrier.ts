import { create, fromBinary, toBinary } from '@bufbuild/protobuf'

import {
  CarrierHelloSchema,
  CarrierEnvelopeSchema,
  CarrierPongSchema,
  CarrierResumeSchema,
  type CarrierEnvelope,
  type CarrierGoAway,
  type CarrierReady,
  type CarrierStreamRejected,
} from '@/generated/remote/v2/carrier_pb'
import { StreamAckSchema } from '@/generated/remote/v2/channel_pb'
import { ProtocolErrorCode } from '@/generated/remote/v2/common_pb'
import { LinkEnvelopeSchema, type LinkEnvelope } from '@/generated/remote/v2/device_link_pb'

import { randomBytes, signCarrierProof } from './crypto'
import type { IssuedDeviceLink } from './deviceLink'

export const V2_RELAY_SUBPROTOCOL = 'wenzwork-relay.v2'

export class V2CarrierBackpressureError extends Error {
  constructor(readonly stream?: { linkId: string; channelId: string; streamId: string }) {
    super('The remote/v2 Carrier queue is full for this Stream.')
    this.name = 'V2CarrierBackpressureError'
  }
}

export class V2CarrierAdmissionError extends Error {
  constructor() {
    super('The remote/v2 Relay rejected Carrier authentication.')
    this.name = 'V2CarrierAdmissionError'
  }
}

type CarrierPriority = 'control' | 'interactive' | 'bulk'

interface QueuedBody {
  body: CarrierEnvelope['body']
  priority: CarrierPriority
  bytes: number
  resolve: () => void
  reject: (error: Error) => void
}

const queueLimits: Record<CarrierPriority, number> = {
  control: 512 << 10,
  interactive: 2 << 20,
  bulk: 8 << 20,
}
const queueFrameLimits: Record<CarrierPriority, number> = {
  control: 256,
  interactive: 256,
  bulk: 256,
}
const prioritySchedule: readonly CarrierPriority[] = [
  'control',
  'control',
  'control',
  'control',
  'interactive',
  'interactive',
  'bulk',
]
const maximumFrameBytes = 4 << 20
const socketHighWaterMark = 1 << 20
const socketLowWaterMark = 256 << 10
const interactiveWaterMark = socketHighWaterMark + (256 << 10)
const socketHardWaterMark = 16 << 20
const socketBackpressurePollMs = 25
const socketOpenTimeoutMs = 10_000
const carrierReadyTimeoutMs = 10_000
const drainFrameBudget = 16
const drainTimeBudgetMs = 4
const receiveYieldFrameBudget = 8
const receiveQueueFrameLimit = 64
const receiveQueueByteLimit = 16 << 20

const monotonicMillis = () =>
  typeof performance !== 'undefined' && typeof performance.now === 'function'
    ? Math.max(0, Math.floor(performance.now()))
    : Date.now()

const heartbeatTimeoutMillis = (seconds: number) => Math.max(15_000, seconds * 2_000 + 3_000)

const linkPriority = (link: LinkEnvelope): CarrierPriority => {
  if (link.body.case !== 'encrypted') return 'control'
  switch (link.body.value.frameType) {
    // FrameType.FILE_CHUNK is 12. Keeping the number out of a legacy enum
    // makes this scheduler depend only on generated v2 data.
    case 12:
      return 'bulk'
    case 5:
    case 8:
    case 9:
    case 10:
      return 'interactive'
    default:
      return 'control'
  }
}

const asArrayBuffer = async (value: unknown): Promise<Uint8Array> => {
  if (value instanceof ArrayBuffer) return new Uint8Array(value)
  if (ArrayBuffer.isView(value))
    return new Uint8Array(value.buffer, value.byteOffset, value.byteLength)
  if (value instanceof Blob) return new Uint8Array(await value.arrayBuffer())
  throw new Error('remote/v2 Relay sent a non-binary WebSocket frame.')
}

const payloadByteLength = (value: unknown) => {
  if (value instanceof ArrayBuffer) return value.byteLength
  if (ArrayBuffer.isView(value)) return value.byteLength
  if (value instanceof Blob) return value.size
  return -1
}

const waitForSocketOpen = (
  socket: WebSocket,
  timeoutMs = socketOpenTimeoutMs,
  signal?: AbortSignal,
) =>
  new Promise<void>((resolve, reject) => {
    if (signal?.aborted) {
      reject(new Error('remote/v2 Carrier connection was cancelled.'))
      return
    }
    const timer = setTimeout(() => {
      cleanup()
      try {
        socket.close()
      } catch {
        // The browser may already have made the socket terminal.
      }
      reject(new Error('连接 remote/v2 Relay 超时。'))
    }, timeoutMs)
    const cleanup = () => {
      clearTimeout(timer)
      socket.removeEventListener('open', opened)
      socket.removeEventListener('error', failed)
      socket.removeEventListener('close', closed)
      signal?.removeEventListener('abort', aborted)
    }
    const opened = () => {
      cleanup()
      resolve()
    }
    const failed = () => {
      cleanup()
      reject(new Error('Unable to connect to the remote/v2 Relay.'))
    }
    const closed = () => {
      cleanup()
      reject(new Error('The remote/v2 Relay closed before Carrier authentication.'))
    }
    const aborted = () => {
      cleanup()
      try {
        socket.close()
      } catch {
        // The browser may already have made the socket terminal.
      }
      reject(new Error('remote/v2 Carrier connection was cancelled.'))
    }
    socket.addEventListener('open', opened, { once: true })
    socket.addEventListener('error', failed, { once: true })
    socket.addEventListener('close', closed, { once: true })
    signal?.addEventListener('abort', aborted, { once: true })
  })

/** Validates the public Carrier endpoint before a browser starts a WebSocket. */
export const validateV2CarrierEndpoint = (
  endpoint: URL,
  pageProtocol = typeof window === 'undefined' ? '' : window.location.protocol,
) => {
  if (
    (endpoint.protocol !== 'ws:' && endpoint.protocol !== 'wss:') ||
    !endpoint.hostname ||
    endpoint.username ||
    endpoint.password ||
    endpoint.pathname !== '/v2/connect' ||
    endpoint.search ||
    endpoint.hash
  ) {
    throw new Error('remote/v2 Relay 地址必须是无凭据的 ws(s)://…/v2/connect。')
  }
  // Browsers reject this as mixed content anyway, but fail before placing a
  // WebSocket attempt so the operator gets an actionable configuration error.
  if (pageProtocol === 'https:' && endpoint.protocol === 'ws:') {
    throw new Error('HTTPS 页面不能连接 ws:// Relay；请为远程连接配置 wss:// 地址。')
  }
}

export class V2Carrier {
  readonly id: string
  readonly epoch: bigint
  readonly socket: WebSocket

  private nextPacket = 0n
  private lastReceived = 0n
  private lastPeerAcknowledged = 0n
  private readonly queues: Record<CarrierPriority, QueuedBody[]> = {
    control: [],
    interactive: [],
    bulk: [],
  }
  private readonly queuedBytes: Record<CarrierPriority, number> = {
    control: 0,
    interactive: 0,
    bulk: 0,
  }
  private draining = false
  private scheduleIndex = 0
  private drainTimer: ReturnType<typeof setTimeout> | undefined
  private closed = false
  private readyValue: CarrierReady | undefined
  private readyResolve!: (value: CarrierReady) => void
  private readyReject!: (error: Error) => void
  private readonly readyPromise: Promise<CarrierReady>
  private heartbeatTimer: ReturnType<typeof setTimeout> | undefined
  private heartbeatTimeoutMs = 0
  private lastInboundAt = 0
  private lastPongAt = 0
  private lastAckProgressAt = 0
  private _lastRttMs = 0
  private receiveFramesSinceYield = 0
  private readonly receiveQueue: Array<{ payload: unknown; bytes: number }> = []
  private receiveQueuedBytes = 0
  private receiving = false
  private failure: Error | undefined
  private onLink?: (link: LinkEnvelope) => void | Promise<void>
  private onStreamRejected?: (message: CarrierStreamRejected) => void
  private onGoAway?: (message: CarrierGoAway) => void
  private onClose?: (error: Error) => void

  private readonly socketMessageHandler = (event: MessageEvent) => this.enqueueReceive(event.data)
  private readonly socketCloseHandler = (event: CloseEvent) =>
    this.fail(
      !this.readyValue && event.reason.trim() === 'remote/v2 client authentication failed'
        ? new V2CarrierAdmissionError()
        : new Error('The remote/v2 Carrier closed.'),
    )
  private readonly socketErrorHandler = () => this.fail(new Error('The remote/v2 Carrier failed.'))

  get isOpen() {
    return !this.closed && this.socket.readyState === WebSocket.OPEN
  }

  private constructor(socket: WebSocket, id: string, epoch: bigint) {
    this.socket = socket
    this.id = id
    this.epoch = epoch
    this.readyPromise = new Promise<CarrierReady>((resolve, reject) => {
      this.readyResolve = resolve
      this.readyReject = reject
    })
    // A socket can fail before connect() reaches waitReady(). Mark the shared
    // promise as observed while preserving its rejection for every real waiter.
    void this.readyPromise.catch(() => undefined)
    this.lastInboundAt = monotonicMillis()
    this.lastPongAt = this.lastInboundAt
    this.lastAckProgressAt = this.lastInboundAt
    socket.binaryType = 'arraybuffer'
    socket.addEventListener('message', this.socketMessageHandler)
    socket.addEventListener('close', this.socketCloseHandler)
    socket.addEventListener('error', this.socketErrorHandler)
  }

  static async connect(input: {
    issued: IssuedDeviceLink
    clientPrivateKey: Uint8Array
    epoch: bigint
    signal?: AbortSignal
  }): Promise<V2Carrier> {
    const endpoint = new URL(input.issued.link.relayUrl)
    validateV2CarrierEndpoint(endpoint)
    const id = crypto.randomUUID()
    const epoch = input.epoch
    if (epoch < 1n) throw new Error('remote/v2 Carrier epoch is invalid.')
    const socket = new WebSocket(endpoint.toString(), V2_RELAY_SUBPROTOCOL)
    const carrier = new V2Carrier(socket, id, epoch)
    try {
      await waitForSocketOpen(socket, socketOpenTimeoutMs, input.signal)
      if (socket.protocol !== V2_RELAY_SUBPROTOCOL) {
        throw new Error('Relay 未协商 remote/v2 子协议。')
      }
      const challenge = randomBytes(32)
      const proof = signCarrierProof(input.clientPrivateKey, {
        grantId: input.issued.claims.grant_id,
        carrierId: id,
        carrierEpoch: epoch,
        challenge,
      })
      await carrier.sendBody(
        {
          case: 'hello',
          value: create(CarrierHelloSchema, {
            grant: input.issued.link.deviceConnectionGrant,
            grantId: input.issued.claims.grant_id,
            clientId: input.issued.claims.client_id,
            clientIdentityKeyVersion: BigInt(input.issued.claims.client_identity_key_version),
            clientChallenge: challenge,
            clientProof: proof,
          }),
        },
        'control',
      )
      // A grant is intentionally not saved on the Carrier or any reactive UI
      // state. It only lived in this local handshake expression.
      await carrier.waitReady(carrierReadyTimeoutMs, input.signal)
      return carrier
    } catch (error) {
      carrier.close('remote/v2 handshake failed')
      throw error
    }
  }

  setHandlers(input: {
    onLink?: (link: LinkEnvelope) => void | Promise<void>
    onStreamRejected?: (message: CarrierStreamRejected) => void
    onGoAway?: (message: CarrierGoAway) => void
    onClose?: (error: Error) => void
  }) {
    this.onLink = input.onLink
    this.onStreamRejected = input.onStreamRejected
    this.onGoAway = input.onGoAway
    this.onClose = input.onClose
    if (this.failure && this.onClose) {
      const failure = this.failure
      queueMicrotask(() => {
        if (this.failure === failure) this.onClose?.(failure)
      })
    }
  }

  get lastPongAgeMs() {
    return Math.max(0, monotonicMillis() - Math.max(this.lastInboundAt, this.lastPongAt))
  }

  get lastRttMs() {
    return this._lastRttMs
  }

  get queueDepths() {
    return {
      control: this.queues.control.length,
      interactive: this.queues.interactive.length,
      bulk: this.queues.bulk.length,
    }
  }

  async waitReady(timeoutMs = carrierReadyTimeoutMs, signal?: AbortSignal) {
    if (this.readyValue) return this.readyValue
    if (signal?.aborted) throw new Error('remote/v2 Carrier connection was cancelled.')
    let timer: ReturnType<typeof setTimeout> | undefined
    let aborted: (() => void) | undefined
    try {
      return await Promise.race([
        this.readyPromise,
        new Promise<CarrierReady>((_, reject) => {
          timer = setTimeout(
            () => reject(new Error('等待 remote/v2 Carrier Ready 超时。')),
            timeoutMs,
          )
        }),
        new Promise<CarrierReady>((_, reject) => {
          if (!signal) return
          aborted = () => reject(new Error('remote/v2 Carrier connection was cancelled.'))
          signal.addEventListener('abort', aborted, { once: true })
        }),
      ])
    } finally {
      if (timer) clearTimeout(timer)
      if (aborted) signal?.removeEventListener('abort', aborted)
    }
  }

  async sendLink(link: LinkEnvelope) {
    const bytes = toBinary(LinkEnvelopeSchema, link).length
    if (bytes === 0 || bytes > maximumFrameBytes) throw new V2CarrierBackpressureError()
    return this.sendBody({ case: 'link', value: link }, linkPriority(link))
  }

  async resume(
    linkId: string,
    lastAckByStream: Array<{ channelId: string; streamId: string; acknowledgedSequence: bigint }>,
  ) {
    return this.sendBody(
      {
        case: 'resume',
        value: create(CarrierResumeSchema, {
          linkId,
          lastAckByStream: lastAckByStream.map((ack) => create(StreamAckSchema, ack)),
        }),
      },
      'control',
    )
  }

  close(reason = 'remote/v2 client closed') {
    if (this.closed) return
    this.closed = true
    if (this.heartbeatTimer) clearTimeout(this.heartbeatTimer)
    if (this.drainTimer) clearTimeout(this.drainTimer)
    this.clearReceiveQueue()
    this.removeSocketListeners()
    if (
      this.socket.readyState === WebSocket.OPEN ||
      this.socket.readyState === WebSocket.CONNECTING
    ) {
      this.socket.close(1000, reason.slice(0, 80))
    }
    this.rejectQueued(new Error('The remote/v2 Carrier is closed.'))
  }

  private sendBody(body: CarrierEnvelope['body'], priority: CarrierPriority): Promise<void> {
    if (this.closed || this.socket.readyState !== WebSocket.OPEN) {
      return Promise.reject(new Error('The remote/v2 Carrier is not open.'))
    }
    const preview = toBinary(
      CarrierEnvelopeSchema,
      create(CarrierEnvelopeSchema, {
        protocolMajor: 2,
        carrierId: this.id,
        carrierEpoch: this.epoch,
        packetSequence: 1n,
        body,
      }),
    )
    if (preview.length === 0 || preview.length > maximumFrameBytes) {
      return Promise.reject(new V2CarrierBackpressureError())
    }
    const queue = this.queues[priority]
    if (
      queue.length >= queueFrameLimits[priority] ||
      this.queuedBytes[priority] + preview.length > queueLimits[priority]
    ) {
      return Promise.reject(new V2CarrierBackpressureError())
    }
    return new Promise<void>((resolve, reject) => {
      queue.push({ body, priority, bytes: preview.length, resolve, reject })
      this.queuedBytes[priority] += preview.length
      this.scheduleDrain()
    })
  }

  private scheduleDrain() {
    if (this.draining) return
    if (this.drainTimer) {
      clearTimeout(this.drainTimer)
      this.drainTimer = undefined
    }
    this.draining = true
    queueMicrotask(() => this.drain())
  }

  private drain() {
    this.draining = false
    const startedAt = monotonicMillis()
    let sentFrames = 0
    while (!this.closed && this.socket.readyState === WebSocket.OPEN) {
      const buffered = this.socket.bufferedAmount
      if (buffered > socketHardWaterMark) {
        this.fail(new Error('remote/v2 socket_backpressure'))
        return
      }
      const scheduled = this.nextSendablePriority(buffered)
      if (!scheduled) break
      // A bulk frame may leave the application queue only while the browser's
      // own WebSocket buffer is below the high-water mark. Control frames keep
      // their reserved lane; interactive traffic is allowed a small bounded
      // allowance so an RPC is not starved behind a large file.
      const item = this.dequeue(scheduled)
      if (!item) break
      try {
        const hadNoOutstandingPackets = this.lastPeerAcknowledged === this.nextPacket
        this.nextPacket += 1n
        if (hadNoOutstandingPackets) this.lastAckProgressAt = monotonicMillis()
        const envelope = create(CarrierEnvelopeSchema, {
          protocolMajor: 2,
          carrierId: this.id,
          carrierEpoch: this.epoch,
          packetSequence: this.nextPacket,
          acknowledgedSequence: this.lastReceived,
          body: item.body,
        })
        const bytes = toBinary(CarrierEnvelopeSchema, envelope)
        if (bytes.length === 0 || bytes.length > maximumFrameBytes)
          throw new Error('remote/v2 Carrier frame is invalid.')
        this.socket.send(bytes)
        item.resolve()
      } catch (error) {
        item.reject(
          error instanceof Error ? error : new Error('Unable to send remote/v2 Carrier frame.'),
        )
        this.fail(
          error instanceof Error ? error : new Error('Unable to send remote/v2 Carrier frame.'),
        )
        return
      }
      sentFrames += 1
      if (sentFrames >= drainFrameBudget || monotonicMillis() - startedAt >= drainTimeBudgetMs) {
        break
      }
    }
    if (
      !this.closed &&
      (this.queues.control.length || this.queues.interactive.length || this.queues.bulk.length)
    ) {
      // The browser owns the actual TCP backpressure signal. Do not spin while
      // it has a large pending WebSocket buffer; retain bounded per-class work.
      const delay =
        this.socket.bufferedAmount >= socketHighWaterMark && this.queues.bulk.length > 0
          ? socketBackpressurePollMs
          : this.socket.bufferedAmount > socketLowWaterMark
            ? 4
            : 0
      this.drainTimer = setTimeout(() => {
        this.drainTimer = undefined
        if (!this.closed && this.socket.bufferedAmount <= socketLowWaterMark) this.scheduleDrain()
        else if (!this.closed) this.scheduleDrain()
      }, delay)
    }
  }

  private nextSendablePriority(
    bufferedAmount: number,
  ): { priority: CarrierPriority; index: number } | undefined {
    for (let offset = 0; offset < prioritySchedule.length; offset += 1) {
      const index = (this.scheduleIndex + offset) % prioritySchedule.length
      const priority = prioritySchedule[index]
      if (this.queues[priority].length === 0) continue
      if (priority === 'bulk' && bufferedAmount >= socketHighWaterMark) continue
      if (priority === 'interactive' && bufferedAmount >= interactiveWaterMark) continue
      return { priority, index }
    }
    return undefined
  }

  private dequeue(scheduled: { priority: CarrierPriority; index: number }) {
    const item = this.queues[scheduled.priority].shift()
    if (!item) return undefined
    this.scheduleIndex = (scheduled.index + 1) % prioritySchedule.length
    this.queuedBytes[scheduled.priority] -= item.bytes
    return item
  }

  private enqueueReceive(payload: unknown) {
    if (this.closed) return
    const bytes = payloadByteLength(payload)
    if (
      bytes <= 0 ||
      bytes > maximumFrameBytes ||
      this.receiveQueue.length >= receiveQueueFrameLimit ||
      this.receiveQueuedBytes + bytes > receiveQueueByteLimit
    ) {
      this.fail(new Error('remote/v2 receive_backpressure'))
      return
    }
    this.receiveQueue.push({ payload, bytes })
    this.receiveQueuedBytes += bytes
    if (!this.receiving) void this.drainReceiveQueue()
  }

  private async drainReceiveQueue() {
    if (this.receiving || this.closed) return
    this.receiving = true
    try {
      while (!this.closed) {
        const item = this.receiveQueue.shift()
        if (!item) return
        this.receiveQueuedBytes -= item.bytes
        await this.receive(item.payload)
      }
    } catch (error) {
      this.fail(error instanceof Error ? error : new Error('remote/v2 Carrier receive failed.'))
    } finally {
      this.receiving = false
      if (!this.closed && this.receiveQueue.length > 0) void this.drainReceiveQueue()
    }
  }

  private async receive(payload: unknown) {
    try {
      if (this.closed) return
      if (this.receiveFramesSinceYield >= receiveYieldFrameBudget) {
        this.receiveFramesSinceYield = 0
        await new Promise<void>((resolve) => setTimeout(resolve, 0))
      }
      const bytes = await asArrayBuffer(payload)
      if (this.closed) return
      if (bytes.length === 0 || bytes.length > maximumFrameBytes)
        throw new Error('remote/v2 Carrier frame is invalid.')
      const envelope = fromBinary(CarrierEnvelopeSchema, bytes)
      if (
        envelope.protocolMajor !== 2 ||
        envelope.carrierId !== this.id ||
        envelope.carrierEpoch !== this.epoch ||
        envelope.packetSequence === 0n ||
        envelope.packetSequence !== this.lastReceived + 1n ||
        envelope.acknowledgedSequence > this.nextPacket ||
        envelope.body.case === undefined
      ) {
        throw new Error('remote/v2 Carrier sequence is invalid.')
      }
      this.lastReceived = envelope.packetSequence
      const now = monotonicMillis()
      this.lastInboundAt = now
      if (envelope.acknowledgedSequence > this.lastPeerAcknowledged) {
        this.lastPeerAcknowledged = envelope.acknowledgedSequence
        this.lastAckProgressAt = now
      }
      this.receiveFramesSinceYield += 1
      switch (envelope.body.case) {
        case 'ready':
          if (
            this.readyValue ||
            envelope.body.value.carrierId !== this.id ||
            envelope.body.value.carrierEpoch !== this.epoch ||
            !Number.isSafeInteger(envelope.body.value.heartbeatIntervalSeconds) ||
            envelope.body.value.heartbeatIntervalSeconds < 1 ||
            envelope.body.value.heartbeatIntervalSeconds > 300 ||
            envelope.body.value.controlQueueByteLimit < 1 ||
            envelope.body.value.interactiveQueueByteLimit < 1 ||
            envelope.body.value.bulkQueueByteLimit < 1
          ) {
            throw new Error('remote/v2 Carrier ready frame is invalid.')
          }
          this.readyValue = envelope.body.value
          this.readyResolve(envelope.body.value)
          this.startHeartbeat(envelope.body.value.heartbeatIntervalSeconds)
          return
        case 'ping':
          await this.sendBody(
            {
              case: 'pong',
              value: create(CarrierPongSchema, {
                monotonicMillis: envelope.body.value.monotonicMillis,
              }),
            },
            'control',
          )
          return
        case 'pong':
          this.lastPongAt = monotonicMillis()
          return
        case 'link':
          await this.onLink?.(envelope.body.value)
          return
        case 'streamRejected':
          this.onStreamRejected?.(envelope.body.value)
          return
        case 'goAway':
          this.onGoAway?.(envelope.body.value)
          this.fail(
            new Error(
              envelope.body.value.reason === ProtocolErrorCode.ROUTE_STALE
                ? 'remote/v2 route_stale; reconnect requested.'
                : 'The remote/v2 Relay requested reconnect.',
            ),
          )
          return
        default:
          throw new Error('remote/v2 Carrier frame is invalid.')
      }
    } catch (error) {
      this.fail(error instanceof Error ? error : new Error('remote/v2 Carrier frame is invalid.'))
    }
  }

  private startHeartbeat(seconds: number) {
    if (!Number.isSafeInteger(seconds) || seconds < 1 || seconds > 300) {
      this.fail(new Error('remote/v2 Relay heartbeat is invalid.'))
      return
    }
    this.lastInboundAt = monotonicMillis()
    this.lastPongAt = this.lastInboundAt
    this.lastAckProgressAt = this.lastInboundAt
    this.heartbeatTimeoutMs = heartbeatTimeoutMillis(seconds)
    // Relay owns the periodic probe. One authenticated Ping/Pong exchange
    // keeps the route open and proves both directions; a second browser Ping
    // only doubles wakeups. This one-shot deadline still detects a silent or
    // legacy Relay without periodically encoding and writing client probes.
    this.scheduleHeartbeatWatchdog(this.lastInboundAt)
  }

  private scheduleHeartbeatWatchdog(now: number) {
    if (this.heartbeatTimer) clearTimeout(this.heartbeatTimer)
    this.heartbeatTimer = undefined
    if (this.closed || this.heartbeatTimeoutMs <= 0) return
    const activityAge = Math.max(0, now - Math.max(this.lastInboundAt, this.lastPongAt))
    let remaining = this.heartbeatTimeoutMs - activityAge
    if (this.nextPacket > this.lastPeerAcknowledged) {
      remaining = Math.min(
        remaining,
        this.heartbeatTimeoutMs - Math.max(0, now - this.lastAckProgressAt),
      )
    }
    this.heartbeatTimer = setTimeout(
      () => {
        this.heartbeatTimer = undefined
        if (this.closed) return
        const current = monotonicMillis()
        if (current - Math.max(this.lastInboundAt, this.lastPongAt) > this.heartbeatTimeoutMs) {
          this.fail(new Error('remote/v2 heartbeat_timeout'))
          return
        }
        if (
          this.nextPacket > this.lastPeerAcknowledged &&
          current - this.lastAckProgressAt > this.heartbeatTimeoutMs
        ) {
          this.fail(new Error('remote/v2 ack_timeout'))
          return
        }
        this.scheduleHeartbeatWatchdog(current)
      },
      Math.max(1, remaining),
    )
  }

  private fail(error: Error) {
    if (this.closed) return
    this.closed = true
    this.failure = error
    if (this.heartbeatTimer) clearTimeout(this.heartbeatTimer)
    if (this.drainTimer) clearTimeout(this.drainTimer)
    this.clearReceiveQueue()
    this.removeSocketListeners()
    this.readyReject(error)
    this.rejectQueued(error)
    try {
      this.socket.close()
    } catch {
      // Best effort only; WebSocket can already be terminal.
    }
    this.onClose?.(error)
  }

  private clearReceiveQueue() {
    this.receiveQueue.length = 0
    this.receiveQueuedBytes = 0
  }

  private removeSocketListeners() {
    this.socket.removeEventListener('message', this.socketMessageHandler)
    this.socket.removeEventListener('close', this.socketCloseHandler)
    this.socket.removeEventListener('error', this.socketErrorHandler)
  }

  private rejectQueued(error: Error) {
    for (const priority of ['control', 'interactive', 'bulk'] as const) {
      for (const item of this.queues[priority].splice(0)) item.reject(error)
      this.queuedBytes[priority] = 0
    }
  }
}
