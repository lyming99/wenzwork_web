import { create, fromBinary, toBinary } from '@bufbuild/protobuf'

import { StreamCloseSchema, StreamOpenSchema } from '@/generated/remote/v2/channel_pb'
import { FrameType, ProtocolErrorCode, StreamKind } from '@/generated/remote/v2/common_pb'
import {
  RpcEventSchema,
  RpcRequestSchema,
  RpcResponseSchema,
  type RpcResponse,
} from '@/generated/remote/v2/message_pb'
import type { RemoteScope } from '@/api/remote'
import { withRemoteEventContract } from '@/remote/eventContract'
import type { RemoteAgentCapabilities, RemotePeerRpcEvent } from '@/remote/peerClient'
import type { RemoteRPCContext, RemoteRPCStreamHandle } from '@/remote/rpcTypes'

import { V2_CHANNEL_CONTROL_STREAM_ID, bytesToBase64Url, decodeUtf8 } from './crypto'
import { V2Link } from './link'

const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/iu

export interface V2RPCDependencies {
  scopeForMethod(method: string, input?: Readonly<Record<string, unknown>>): RemoteScope
  parseCapabilities(value: unknown): RemoteAgentCapabilities
  timeoutFor(method: string, streaming: boolean): number
  projectRequired(scope: RemoteScope): boolean
}

export class V2RPCTransportRecoveryError extends Error {
  readonly possiblyCommitted: boolean

  constructor(message: string, possiblyCommitted: boolean) {
    super(message)
    this.name = 'V2RPCTransportRecoveryError'
    this.possiblyCommitted = possiblyCommitted
  }
}

interface PendingOperation {
  channelId: string
  streamId: string
  operationId: string
  attemptId: string
  method: string
  projectId?: string
  scope: RemoteScope
  streamKind: StreamKind
  channelRetained: boolean
  resolve: (value: unknown) => void
  reject: (error: Error) => void
  onEvent?: (event: RemotePeerRpcEvent) => void
  timer: ReturnType<typeof setTimeout>
}

interface StaleRPCStream {
  channelId: string
  streamId: string
}

const maximumStaleRPCStreams = 128

const jsonEncode = (value: unknown) => {
  const bytes = new TextEncoder().encode(JSON.stringify(value ?? {}))
  if (bytes.length === 0 || bytes.length > 56 * 1024) throw new Error('远程 RPC 请求过大。')
  return bytes
}

const jsonDecode = (value: Uint8Array) => {
  if (value.length > 56 * 1024) throw new Error('远程 RPC 响应过大。')
  try {
    return JSON.parse(decodeUtf8(value)) as unknown
  } catch {
    throw new Error('设备返回了无效的远程 RPC 数据。')
  }
}

const closeError = (response: RpcResponse) => {
  const code = response.safeErrorCode.trim() || 'remote_operation_failed'
  const retry = response.retryable ? ' 可重试。' : ''
  return new Error(`设备拒绝了远程操作（${code}）。${retry}`)
}

const newStreamHandle = <T>(
  result: Promise<T>,
  detach: () => Promise<void>,
): RemoteRPCStreamHandle<T> => ({ result, detach })

/** RPC multiplexer on top of v2 Link Channels and independent Streams. */
export class V2RPCClient {
  private readonly pending = new Map<string, PendingOperation>()
  private readonly staleStreams = new Map<string, StaleRPCStream>()
  private capabilities?: RemoteAgentCapabilities
  private capabilitiesCacheKey?: string
  private capabilitiesRequest?: Promise<RemoteAgentCapabilities>
  private readonly detachListeners: Array<() => void>
  private readonly capabilityCacheNamespace: string
  private disposed = false

  constructor(
    private readonly link: V2Link,
    private readonly dependencies: V2RPCDependencies,
    capabilityCacheNamespace?: string,
  ) {
    this.capabilityCacheNamespace =
      capabilityCacheNamespace ?? `${link.deviceId}:${link.deviceIdentityKeyVersion}`
    this.detachListeners = [
      link.on(FrameType.RPC_RESPONSE, (frame) =>
        this.handleResponse(frame.record.streamId, fromBinary(RpcResponseSchema, frame.plaintext)),
      ),
      link.on(FrameType.RPC_EVENT, (frame) =>
        this.handleEvent(frame.record.streamId, fromBinary(RpcEventSchema, frame.plaintext)),
      ),
      link.on(FrameType.STREAM_CLOSE, (frame) =>
        this.handleClose(frame.record.streamId, fromBinary(StreamCloseSchema, frame.plaintext)),
      ),
    ]
  }

  async getCapabilities(refresh = false) {
    // A ChannelAccept carries the device's capability revision. Resolve that
    // channel before consulting the fast path so a revision change cannot be
    // hidden behind a Link-local `_capabilities` value. A refresh rotates only
    // this idle query Channel; project Channels and their Streams are intact.
    if (this.capabilitiesRequest) return this.capabilitiesRequest
    const request = (async () => {
      let queryChannel = await this.link.channels.openDeviceQuery()
      if (refresh) {
        await this.link.channels.close(queryChannel.id)
        queryChannel = await this.link.channels.openDeviceQuery()
      }
      const revision = bytesToBase64Url(queryChannel.capabilityRevision)
      const cacheKey = `${this.capabilityCacheNamespace}:${this.link.deviceId}:${this.link.deviceIdentityKeyVersion}:${revision}`
      if (!refresh && this.capabilities && this.capabilitiesCacheKey === cacheKey)
        return this.capabilities
      this.capabilities = undefined
      this.capabilitiesCacheKey = cacheKey
      const value = await this.execute<unknown>(
        'agent.capabilities.get',
        {},
        undefined,
        undefined,
        true,
      )
      const capabilities = this.dependencies.parseCapabilities(value)
      this.capabilities = capabilities
      return capabilities
    })()
    this.capabilitiesRequest = request
    try {
      return await request
    } finally {
      if (this.capabilitiesRequest === request) this.capabilitiesRequest = undefined
    }
  }

  call<T>(
    method: string,
    input: Record<string, unknown> = {},
    context?: RemoteRPCContext,
    operationId?: string,
  ) {
    return this.execute<T>(method, input, context, undefined, false, undefined, operationId)
  }

  stream<T>(
    method: string,
    input: Record<string, unknown>,
    onDelta: (delta: T) => void,
    context?: RemoteRPCContext,
  ) {
    return this.startStream<T, unknown>(method, input, onDelta, context).result.then(
      () => undefined,
    )
  }

  startStream<TDelta, TResult = unknown>(
    method: string,
    input: Record<string, unknown>,
    onDelta: (delta: TDelta) => void,
    context?: RemoteRPCContext,
  ) {
    let detachRequested = false
    let detachOperation: (() => Promise<void>) | undefined
    const result = this.execute<TResult>(
      method,
      input,
      context,
      (event) => onDelta(event.payload as TDelta),
      false,
      (detach) => {
        detachOperation = detach
        if (detachRequested) void detach()
      },
    )
    return newStreamHandle(result, async () => {
      detachRequested = true
      await detachOperation?.()
    })
  }

  subscribeAgentEvents(
    input: { afterSequence?: number; heartbeatSeconds?: number },
    onEvent: (event: RemotePeerRpcEvent) => void,
    context: RemoteRPCContext,
  ) {
    return this.execute<unknown>('event.subscribe', input, context, onEvent).then(() => undefined)
  }

  cancelAgentEventSubscriptions(context: RemoteRPCContext) {
    const projectId = context.projectId?.trim()
    const operations = [...this.pending.values()].filter(
      (pending) => pending.method === 'event.subscribe' && pending.projectId === projectId,
    )
    return Promise.all(operations.map((pending) => this.cancel(pending))).then(() => undefined)
  }

  fail(error: Error) {
    for (const pending of [...this.pending.values()]) {
      const possiblyCommitted = !isReadOnlyMethod(pending.method)
      // send/regenerate own durable Agent work. On a Carrier failure this
      // client abandons only its observer; replaying StreamClose after resume
      // would otherwise look like an explicit cancellation to an older Agent.
      this.reject(
        pending,
        possiblyCommitted
          ? new V2RPCTransportRecoveryError(
              '远程操作状态未知（可能已提交）；恢复后请使用原 operation_id 查询。',
              true,
            )
          : error,
        !(possiblyCommitted && isDurableGenerationMethod(pending.method)),
      )
    }
  }

  get staleStreamCount() {
    return this.staleStreams.size
  }

  /** Delivers StreamClose frames that were lost with the previous Carrier.
   * This is called only after the Device has signed the Link recovery ACK. */
  async closeStaleStreams() {
    if (this.disposed || !this.link.isActive) {
      this.staleStreams.clear()
      return
    }
    for (const stale of [...this.staleStreams.values()]) {
      await this.sendStreamClose(stale.channelId, stale.streamId)
      this.staleStreams.delete(stale.streamId)
    }
  }

  dispose() {
    if (this.disposed) return
    this.disposed = true
    for (const listener of this.detachListeners) listener()
    this.fail(new Error('远程客户端已关闭。'))
    this.staleStreams.clear()
  }

  private async execute<T>(
    method: string,
    input: Record<string, unknown>,
    context?: RemoteRPCContext,
    onEvent?: (event: RemotePeerRpcEvent) => void,
    skipCapabilities = false,
    bindDetach?: (detach: () => Promise<void>) => void,
    requestedOperationId?: string,
  ): Promise<T> {
    if (!skipCapabilities && method !== 'agent.capabilities.get') await this.getCapabilities()
    input = withRemoteEventContract(method, input)
    const scope = this.dependencies.scopeForMethod(method, input)
    const projectId = context?.projectId?.trim()
    const requiresProject = this.dependencies.projectRequired(scope)
    if (requiresProject && !projectId) throw new Error('请先选择一个可用项目，再使用该远程能力。')
    if (!requiresProject && projectId) throw new Error('此设备级操作不能绑定项目。')
    const channel = requiresProject
      ? await this.link.channels.openProject(projectId!, [scope])
      : await this.link.channels.openDeviceScope(scope)
    const streamId = crypto.randomUUID()
    const operationId = requestedOperationId ?? crypto.randomUUID()
    if (!UUID.test(operationId)) throw new Error('远程操作 ID 无效。')
    const attemptId = crypto.randomUUID()
    this.link.channels.retain(channel.id)
    const deadline = new Date(Date.now() + this.dependencies.timeoutFor(method, Boolean(onEvent)))
    const timeout = this.dependencies.timeoutFor(method, Boolean(onEvent))
    return new Promise<T>((resolve, reject) => {
      const pending: PendingOperation = {
        channelId: channel.id,
        streamId,
        operationId,
        attemptId,
        method,
        projectId,
        scope,
        streamKind: StreamKind.RPC,
        channelRetained: true,
        resolve: (value) => resolve(value as T),
        reject,
        onEvent,
        timer: setTimeout(() => this.reject(pending, new Error('远程操作超时。')), timeout + 1000),
      }
      this.pending.set(streamId, pending)
      bindDetach?.(() => this.cancel(pending))
      void this.link
        .sendEncrypted(
          FrameType.STREAM_OPEN,
          channel.id,
          V2_CHANNEL_CONTROL_STREAM_ID,
          toBinary(
            StreamOpenSchema,
            create(StreamOpenSchema, {
              channelId: channel.id,
              streamId,
              kind: StreamKind.RPC,
              operationId,
            }),
          ),
        )
        .then(() =>
          this.link.sendEncrypted(
            FrameType.RPC_REQUEST,
            channel.id,
            streamId,
            toBinary(
              RpcRequestSchema,
              create(RpcRequestSchema, {
                operationId,
                attemptId,
                method,
                deadline: {
                  seconds: BigInt(Math.floor(deadline.getTime() / 1000)),
                  nanos: (deadline.getTime() % 1000) * 1_000_000,
                },
                payload: jsonEncode(input),
              }),
            ),
          ),
        )
        .catch((error) =>
          this.reject(pending, error instanceof Error ? error : new Error('无法发送远程操作。')),
        )
    })
  }

  private handleResponse(streamId: string, response: RpcResponse) {
    const pending = this.pending.get(streamId)
    if (
      !pending ||
      response.operationId !== pending.operationId ||
      response.attemptId !== pending.attemptId
    )
      return
    if (response.errorCode !== ProtocolErrorCode.UNSPECIFIED || response.safeErrorCode) {
      this.reject(pending, closeError(response))
      return
    }
    try {
      this.resolve(pending, jsonDecode(response.payload))
    } catch (error) {
      this.reject(pending, error instanceof Error ? error : new Error('设备返回了无效响应。'))
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
    const pending = this.pending.get(streamId)
    if (!pending || event.operationId !== pending.operationId || !pending.onEvent) return
    try {
      const sequence = Number(event.eventSequence)
      const highWatermark = Number(event.highWatermark)
      if (
        !UUID.test(event.eventId) ||
        !Number.isSafeInteger(sequence) ||
        sequence < 1 ||
        !Number.isSafeInteger(highWatermark) ||
        highWatermark < sequence
      ) {
        throw new Error('远程事件序号无效。')
      }
      pending.onEvent({
        eventId: event.eventId,
        eventKind: 14,
        requestId: pending.attemptId,
        sequence,
        highWatermark,
        payload: jsonDecode(event.payload),
      })
    } catch (error) {
      this.reject(pending, error instanceof Error ? error : new Error('设备返回了无效事件。'))
    }
  }

  private handleClose(
    streamId: string,
    close: { channelId: string; streamId: string; reason: ProtocolErrorCode },
  ) {
    const pending = this.pending.get(streamId)
    if (!pending || close.streamId !== pending.streamId) return
    this.reject(
      pending,
      new Error(`远程 Stream 已关闭（${ProtocolErrorCode[close.reason] ?? 'unknown'}）。`),
    )
  }

  private resolve(pending: PendingOperation, value: unknown) {
    if (this.pending.get(pending.streamId) !== pending) return
    this.pending.delete(pending.streamId)
    clearTimeout(pending.timer)
    this.releaseChannel(pending)
    void this.closeStream(pending)
    pending.resolve(value)
  }

  private reject(pending: PendingOperation, error: Error, closeStream = true) {
    if (this.pending.get(pending.streamId) !== pending) return
    this.pending.delete(pending.streamId)
    clearTimeout(pending.timer)
    this.releaseChannel(pending)
    if (closeStream) void this.closeStream(pending)
    pending.reject(error)
  }

  private cancel(pending: PendingOperation) {
    this.reject(pending, new Error('远程 Stream 已取消。'))
    return Promise.resolve()
  }

  private async closeStream(pending: PendingOperation) {
    try {
      await this.sendStreamClose(pending.channelId, pending.streamId)
      this.staleStreams.delete(pending.streamId)
    } catch {
      if (!this.disposed && this.link.isActive) {
        this.staleStreams.delete(pending.streamId)
        this.staleStreams.set(pending.streamId, {
          channelId: pending.channelId,
          streamId: pending.streamId,
        })
        while (this.staleStreams.size > maximumStaleRPCStreams) {
          const oldest = this.staleStreams.keys().next().value
          if (oldest === undefined) break
          this.staleStreams.delete(oldest)
        }
      }
    }
  }

  private sendStreamClose(channelId: string, streamId: string) {
    return this.link.sendEncrypted(
      FrameType.STREAM_CLOSE,
      channelId,
      V2_CHANNEL_CONTROL_STREAM_ID,
      toBinary(
        StreamCloseSchema,
        create(StreamCloseSchema, {
          channelId,
          streamId,
          reason: ProtocolErrorCode.STREAM_CANCELLED,
        }),
      ),
    )
  }

  private releaseChannel(pending: PendingOperation) {
    if (!pending.channelRetained) return
    pending.channelRetained = false
    this.link.channels.release(pending.channelId)
  }
}

const READ_ONLY_METHODS = new Set([
  'agent.capabilities.get',
  'conversation.search',
  'conversation.messages.before',
  'conversation.message.content',
  'conversation.events',
  'conversation.generation.attach',
  'event.subscribe',
  'terminal.attach',
  'task.logs',
  'task.logs.download.prepare',
])

const isReadOnlyMethod = (method: string) =>
  method.endsWith('.get') ||
  method.endsWith('.list') ||
  method.endsWith('.stat') ||
  method.endsWith('.query') ||
  READ_ONLY_METHODS.has(method)

const isDurableGenerationMethod = (method: string) =>
  method === 'conversation.send' ||
  method === 'conversation.chat.send' ||
  method === 'chat.send' ||
  method === 'conversation.regenerate'
