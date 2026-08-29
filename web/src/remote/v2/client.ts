import { sha256 } from '@noble/hashes/sha2.js'
import axios from 'axios'
import { ref, type Ref } from 'vue'

import {
  createRemoteIdempotencyKey,
  getBrowserController,
  registerBrowserController,
  type RemoteScope,
} from '@/api/remote'
import { useAuthStore } from '@/stores/auth'
import {
  loadBrowserControllerIdentity,
  nextConnectionEpoch,
  signControllerRegistration,
  type BrowserControllerIdentity,
} from '@/remote/peerIdentity'
import type { RemoteAgentCapabilities, RemotePeerRpcEvent } from '@/remote/peerClient'
import type { RemoteRPCClient, RemoteRPCContext, RemoteRPCStreamHandle } from '@/remote/rpcTypes'
import { ProtocolErrorCode } from '@/generated/remote/v2/common_pb'

import { V2Carrier, V2CarrierAdmissionError } from './carrier'
import { bytesToBase64Url } from './crypto'
import { issueDeviceLink, type IssuedDeviceLink, validateIssuedDeviceLink } from './deviceLink'
import { V2Link } from './link'
import { V2EventClient } from './eventClient'
import { V2FileTransferClient } from './fileTransfer'
import { V2RPCClient, type V2RPCDependencies } from './rpcClient'

const controllerScopes: RemoteScope[] = [
  'remote.peer.query',
  'remote.peer.ai.config',
  'remote.peer.ai.chat',
  'remote.peer.ai.tools',
  'remote.peer.terminal',
  'remote.peer.terminal.interactive',
  'remote.peer.file.send',
  'remote.peer.file.receive',
  'remote.peer.task.control',
  'remote.peer.events',
]

export type V2ClientDependencies = V2RPCDependencies

const v2MaximumRetryAfterMs = 0xffffffff
const v2MinimumReconnectDelayMs = 250
const v2MaximumTimerDelayMs = 0x7fffffff

const finiteNonNegativeInteger = (value: number, maximum: number) =>
  Number.isFinite(value) ? Math.min(maximum, Math.max(0, Math.floor(value))) : 0

/** Full-jitter reconnect delay shared by Carrier and HTTP allocation retries. */
export const v2FullJitterDelay = (
  attempt: number,
  retryAfterMs = 0,
  baseMs = 500,
  capMs = 30_000,
) => {
  const exponent = Number.isFinite(attempt) ? Math.min(16, Math.max(0, Math.floor(attempt))) : 0
  const safeBase = finiteNonNegativeInteger(baseMs, v2MaximumTimerDelayMs)
  const safeCap = finiteNonNegativeInteger(capMs, v2MaximumTimerDelayMs)
  const bound = Math.min(safeCap, safeBase * 2 ** exponent)
  const floor = Math.min(v2MinimumReconnectDelayMs, bound)
  const jitter = floor + Math.floor(Math.random() * Math.max(1, bound - floor + 1))
  const retryAfter = finiteNonNegativeInteger(retryAfterMs, v2MaximumRetryAfterMs)
  return retryAfter + jitter
}

type V2ReconnectDelay = (attempt: number, retryAfterMs: number) => number

/** A hard resource budget for one uninterrupted recovery episode. */
export class V2ReconnectPolicy {
  private startedAt: number | undefined
  private attemptCount = 0
  private backoffAttempt = 0

  constructor(
    // Zero means the five-minute time window is the only production recovery
    // budget. Tests and constrained embedders may still set an explicit cap.
    readonly maximumAttempts = 0,
    readonly maximumDurationMs = 5 * 60_000,
    private readonly now: () => number = Date.now,
    private readonly delayFor: V2ReconnectDelay = v2FullJitterDelay,
  ) {
    if (maximumAttempts < 0 || maximumDurationMs <= 0)
      throw new Error('remote/v2 reconnect policy is invalid.')
  }

  get attempts() {
    return this.attemptCount
  }

  get durationExhausted() {
    return this.startedAt !== undefined && this.now() >= this.startedAt + this.maximumDurationMs
  }

  get exhausted() {
    return (
      (this.maximumAttempts > 0 && this.attemptCount >= this.maximumAttempts) ||
      this.durationExhausted
    )
  }

  nextDelay(retryAfterMs = 0) {
    if (this.exhausted) return undefined
    const now = this.now()
    this.startedAt ??= now
    const remaining = this.startedAt + this.maximumDurationMs - now
    if (remaining <= 0) return undefined
    const requested = finiteNonNegativeInteger(
      this.delayFor(this.backoffAttempt, retryAfterMs),
      v2MaximumRetryAfterMs + v2MaximumTimerDelayMs,
    )
    this.attemptCount += 1
    this.backoffAttempt = Math.min(16, this.backoffAttempt + 1)
    return Math.min(requested, remaining)
  }

  reset() {
    this.startedAt = undefined
    this.attemptCount = 0
    this.backoffAttempt = 0
  }
}

export const v2ReconnectAfterMillis = (value: unknown) => {
  const numeric = typeof value === 'bigint' ? Number(value) : Number(value)
  return finiteNonNegativeInteger(numeric, v2MaximumRetryAfterMs)
}

export const v2RetryAfterMillis = (failure: unknown) => {
  if (!axios.isAxiosError(failure)) return 0
  const value = failure.response?.headers?.['retry-after']
  const raw = Array.isArray(value) ? value[0] : value
  if (typeof raw !== 'string' && typeof raw !== 'number') return 0
  const text = String(raw).trim()
  if (/^\d+$/u.test(text)) {
    const seconds = Number.parseInt(text, 10)
    return Number.isSafeInteger(seconds) && seconds >= 0 && seconds <= 3600 ? seconds * 1000 : 0
  }
  const date = Date.parse(text)
  return Number.isFinite(date)
    ? finiteNonNegativeInteger(date - Date.now(), v2MaximumRetryAfterMs)
    : 0
}

export const isV2RetryableFailure = (failure: unknown) => {
  if (axios.isCancel(failure) || (axios.isAxiosError(failure) && failure.code === 'ERR_CANCELED'))
    return false
  if (axios.isAxiosError(failure)) {
    const status = failure.response?.status
    return (
      status === undefined ||
      status === 404 ||
      status === 408 ||
      status === 425 ||
      status === 429 ||
      (status >= 500 && status <= 599)
    )
  }
  if (!(failure instanceof Error)) return false
  return /Carrier|Relay|WebSocket|heartbeat|ack_timeout|route_stale|网络|连接|超时|timeout/iu.test(
    failure.message,
  )
}

export const shouldRetainV2IssueIdempotency = (failure: unknown) => {
  if (!axios.isAxiosError(failure) || axios.isCancel(failure)) return false
  const status = failure.response?.status
  return (
    status === undefined ||
    status === 408 ||
    status === 425 ||
    status === 429 ||
    (status >= 500 && status <= 599)
  )
}

export const isV2RetryableLinkFailure = (failure: Error) =>
  isV2RetryableFailure(failure) || /remote\/v2 rekey timed out/iu.test(failure.message)

export const isV2RecoverableLinkRejection = (reason: ProtocolErrorCode) =>
  reason === ProtocolErrorCode.ROUTE_STALE ||
  reason === ProtocolErrorCode.RESUME_EXPIRED ||
  reason === ProtocolErrorCode.STREAM_NOT_FOUND ||
  reason === ProtocolErrorCode.BACKPRESSURE

export const isV2GrantInvalidatingLinkRejection = (reason: ProtocolErrorCode) =>
  reason === ProtocolErrorCode.ROUTE_STALE ||
  reason === ProtocolErrorCode.GRANT_INVALID ||
  reason === ProtocolErrorCode.GRANT_REPLAYED ||
  reason === ProtocolErrorCode.REVOKED ||
  reason === ProtocolErrorCode.IDENTITY_INVALID ||
  reason === ProtocolErrorCode.AUTHENTICATION_FAILED

/**
 * Browser-facing v2 client. The carrier has exactly one Link for one device;
 * project changes only open or close encrypted Channels on that Link.
 */
export const createRemotePeerClientV2 = (
  targetDeviceId: Readonly<Ref<string>>,
  dependencies: V2ClientDependencies,
): RemoteRPCClient => {
  const auth = useAuthStore()
  const connected = ref(false)
  const reconnecting = ref(false)
  const error = ref('')
  let identity: BrowserControllerIdentity | undefined
  let carrier: V2Carrier | undefined
  let link: V2Link | undefined
  let rpc: V2RPCClient | undefined
  let fileTransfer: V2FileTransferClient | undefined
  let eventClient: V2EventClient | undefined
  let cachedIssuedLink: IssuedDeviceLink | undefined
  let connecting: Promise<void> | undefined
  let routeRecovery: Promise<void> | undefined
  let reconnectTimer: ReturnType<typeof setTimeout> | undefined
  let reconnectWaiter:
    | {
        promise: Promise<void>
        resolve: () => void
        reject: (failure: unknown) => void
      }
    | undefined
  const reconnectPolicy = new V2ReconnectPolicy()
  let retryAfterMs = 0
  let permanentlyClosed = false
  let recoveryFailed = false
  let ownershipKey = ''
  let ownershipReady: Promise<void> | undefined
  let ownershipRelease: (() => void) | undefined
  let ownershipAbort: AbortController | undefined
  let connectionAbort: AbortController | undefined
  let onlineHandler: (() => void) | undefined
  let pendingIssueIdempotencyKey = ''

  const browserIsOffline = () => typeof navigator !== 'undefined' && navigator.onLine === false

  const clearOnlineWaiter = () => {
    if (onlineHandler && typeof window !== 'undefined') {
      window.removeEventListener('online', onlineHandler)
    }
    onlineHandler = undefined
  }

  const assertConnectionAttempt = (attempt: AbortController) => {
    if (permanentlyClosed || attempt.signal.aborted || connectionAbort !== attempt) {
      throw new Error('remote/v2 connection attempt was cancelled.')
    }
  }

  const ensureControllerIdentity = async () => {
    const userId = auth.user?.id
    if (!userId || !targetDeviceId.value)
      throw new Error('Authenticated account or target device is unavailable.')
    if (identity) return identity
    const loaded = await loadBrowserControllerIdentity(userId)
    let controller
    try {
      controller = await getBrowserController(loaded.controllerId)
    } catch (failure) {
      if (!axios.isAxiosError(failure) || failure.response?.status !== 404) throw failure
      controller = await registerBrowserController({
        controllerId: loaded.controllerId,
        identityAlgorithm: 'Ed25519',
        identityPublicKey: loaded.identityPublicKey,
        proof: signControllerRegistration(loaded),
        scopes: controllerScopes,
      })
    }
    const thumbprint = bytesToBase64Url(sha256(loaded.publicKey))
    if (
      controller.id !== loaded.controllerId ||
      controller.identityAlgorithm !== 'Ed25519' ||
      controller.identityPublicKey !== loaded.identityPublicKey ||
      controller.publicKeyThumbprint !== thumbprint ||
      controller.keyVersion !== loaded.keyVersion ||
      controller.status !== 'active'
    ) {
      throw new Error('Controller identity validation failed.')
    }
    identity = loaded
    return loaded
  }

  const ensureConnectionOwnership = async (controllerId: string, target: string) => {
    const locks = typeof navigator === 'undefined' ? undefined : navigator.locks
    if (!locks) return
    const key = `wenzwork:remote-v2:${controllerId}:${target}`
    if (ownershipReady) {
      if (ownershipKey !== key) throw new Error('远程连接所有权与目标设备不匹配。')
      return ownershipReady
    }
    ownershipKey = key
    ownershipAbort = new AbortController()
    let resolveReady!: () => void
    let rejectReady!: (error: unknown) => void
    ownershipReady = new Promise<void>((resolve, reject) => {
      resolveReady = resolve
      rejectReady = reject
    })
    void locks
      .request(key, { mode: 'exclusive', signal: ownershipAbort.signal }, async () => {
        if (permanentlyClosed) throw new Error('远程客户端已关闭。')
        let release!: () => void
        const held = new Promise<void>((resolve) => {
          release = resolve
        })
        ownershipRelease = release
        resolveReady()
        await held
      })
      .catch((failure: unknown) => {
        ownershipRelease = undefined
        ownershipReady = undefined
        ownershipKey = ''
        rejectReady(failure)
      })
    return ownershipReady
  }

  const handleCarrierClose = (failedCarrier: V2Carrier, failure: Error) => {
    if (carrier !== failedCarrier || permanentlyClosed) return
    link?.handleCarrierInterrupted(failedCarrier, failure)
    carrier = undefined
    connected.value = false
    fileTransfer?.handleCarrierInterrupted()
    rpc?.fail(new Error('远程 Carrier 已中断，正在恢复连接。'))
    error.value = failure.message
    scheduleReconnect()
  }

  const handleLinkFailure = (failedLink: V2Link, failure: Error) => {
    if (link !== failedLink || permanentlyClosed) return
    connected.value = false
    fileTransfer?.handleCarrierInterrupted()
    rpc?.fail(new Error('远程加密 Link 已中断，正在重新握手。'))
    error.value = failure.message
    const failedCarrier = carrier
    carrier = undefined
    failedCarrier?.close('remote/v2 Link failed')
    if (isV2RetryableLinkFailure(failure)) scheduleReconnect()
    else settleRecoveryFailure(failure.message)
  }

  const recoverLinkRouteOnCurrentCarrier = (currentCarrier: V2Carrier, currentLink: V2Link) => {
    if (permanentlyClosed || carrier !== currentCarrier || link !== currentLink || routeRecovery)
      return
    connected.value = false
    reconnecting.value = true
    error.value = '设备路由已切换，正在当前 Relay WebSocket 上恢复加密 Link。'
    fileTransfer?.handleCarrierInterrupted()
    rpc?.fail(new Error('设备路由已切换，正在恢复缓存 Link。'))
    currentLink.attachCarrier(currentCarrier)

    const recovery = (async () => {
      try {
        await currentLink.resume(eventClient?.carrierAcks() ?? [])
        if (permanentlyClosed || carrier !== currentCarrier || link !== currentLink) return
        await rpc?.closeStaleStreams()
        await eventClient?.resumeAll()
        reconnectPolicy.reset()
        recoveryFailed = false
        connected.value = true
        reconnecting.value = false
        error.value = ''
      } catch (failure) {
        if (!permanentlyClosed && carrier === currentCarrier && link === currentLink) {
          handleLinkFailure(
            currentLink,
            failure instanceof Error ? failure : new Error('远程 Link 路由恢复失败。'),
          )
        }
      }
    })()
    routeRecovery = recovery
    void recovery.finally(() => {
      if (routeRecovery === recovery) routeRecovery = undefined
    })
  }

  const establish = async () => {
    if (permanentlyClosed) throw new Error('远程客户端已关闭。')
    const attempt = new AbortController()
    connectionAbort = attempt
    let nextCarrier: V2Carrier | undefined
    let issued: IssuedDeviceLink | undefined
    let canResume = false
    try {
      const controller = await ensureControllerIdentity()
      assertConnectionAttempt(attempt)
      const target = targetDeviceId.value
      if (!target) throw new Error('Target device is unavailable.')
      reconnecting.value = true
      await ensureConnectionOwnership(controller.controllerId, target)
      assertConnectionAttempt(attempt)
      if (!pendingIssueIdempotencyKey) pendingIssueIdempotencyKey = createRemoteIdempotencyKey()
      issued = cachedIssuedLink
      if (issued) {
        try {
          validateIssuedDeviceLink(issued.link, issued.claims, {
            controllerId: controller.controllerId,
            targetDeviceId: target,
            keyVersion: controller.keyVersion,
          })
        } catch {
          cachedIssuedLink = undefined
          issued = undefined
        }
      }
      if (!issued) {
        try {
          issued = await issueDeviceLink({
            controllerId: controller.controllerId,
            targetDeviceId: target,
            keyVersion: controller.keyVersion,
            signal: attempt.signal,
            idempotencyKey: pendingIssueIdempotencyKey,
          })
          cachedIssuedLink = issued
          pendingIssueIdempotencyKey = ''
        } catch (failure) {
          if (!shouldRetainV2IssueIdempotency(failure)) pendingIssueIdempotencyKey = ''
          throw failure
        }
      }
      assertConnectionAttempt(attempt)
      nextCarrier = await V2Carrier.connect({
        issued,
        clientPrivateKey: controller.privateKey,
        epoch: BigInt(await nextConnectionEpoch(controller)),
        signal: attempt.signal,
      })
      assertConnectionAttempt(attempt)
      carrier = nextCarrier
      nextCarrier.setHandlers({
        onLink: async (envelope) => {
          if (carrier !== nextCarrier) return
          await link?.handleLinkEnvelope(envelope)
        },
        onClose: (failure) => handleCarrierClose(nextCarrier!, failure),
        onGoAway: (message) => {
          retryAfterMs = Math.max(
            retryAfterMs,
            v2ReconnectAfterMillis(message.reconnectAfterMillis),
          )
          if (Number(message.reason) === 5) retryAfterMs = Math.max(retryAfterMs, 1000)
        },
        onStreamRejected: (rejection) => {
          if (
            carrier === nextCarrier &&
            rejection.linkId === link?.id &&
            !rejection.channelId &&
            !rejection.streamId
          ) {
            if (rejection.reason === ProtocolErrorCode.ROUTE_STALE && link?.isActive === true) {
              if (link.isResumePending) {
                const pendingLink = link
                void pendingLink
                  .retryPendingResume(eventClient?.carrierAcks() ?? [])
                  .catch((failure: unknown) => {
                    if (!permanentlyClosed && carrier === nextCarrier && link === pendingLink) {
                      handleLinkFailure(
                        pendingLink,
                        failure instanceof Error ? failure : new Error('远程 Link 恢复重发失败。'),
                      )
                    }
                  })
                return
              }
              recoverLinkRouteOnCurrentCarrier(nextCarrier!, link)
              return
            }
            if (isV2GrantInvalidatingLinkRejection(rejection.reason)) {
              cachedIssuedLink = undefined
            }
            // The Device has expired or discarded only this Link. Tear down the
            // local key material and reconnect for a fresh, PoP-bound handshake.
            const recoverable = isV2RecoverableLinkRejection(rejection.reason)
            error.value = recoverable
              ? '远程 Link 的恢复窗口已过期，正在重新握手。'
              : '远程 Link 验证失败，已停止自动重连。'
            fileTransfer?.handleCarrierInterrupted()
            link?.close(new Error(error.value))
            carrier = undefined
            connected.value = false
            nextCarrier?.close('remote/v2 Link recovery expired')
            if (recoverable) scheduleReconnect()
            else settleRecoveryFailure(error.value)
            return
          }
          if (link?.handleCarrierStreamRejected(rejection)) {
            error.value = `远程 Stream 背压（${rejection.streamId}）。`
          }
        },
      })
      if (!nextCarrier.isOpen) throw new Error('The remote/v2 Carrier closed during setup.')

      canResume =
        Boolean(link?.isActive) &&
        issued.link.deviceIdentityKeyVersion === link?.deviceIdentityKeyVersion &&
        issued.claims.device_id === link?.deviceId
      if (canResume && link) {
        link.attachCarrier(nextCarrier)
        await link.resume(eventClient?.carrierAcks() ?? [])
        await rpc?.closeStaleStreams()
        await eventClient?.resumeAll()
      } else {
        link?.close(new Error('设备身份或路由已变化，需要重新握手。'))
        eventClient?.dispose()
        fileTransfer?.dispose()
        rpc?.dispose()
        const created = new V2Link(
          nextCarrier,
          issued,
          {
            controllerId: controller.controllerId,
            keyVersion: controller.keyVersion,
            privateKey: controller.privateKey,
          },
          (failure) => handleLinkFailure(created, failure),
        )
        link = created
        rpc = new V2RPCClient(
          created,
          dependencies,
          `${auth.user?.id ?? controller.controllerId}:${target}`,
        )
        fileTransfer = new V2FileTransferClient(created, rpc)
        eventClient = new V2EventClient(created)
        await created.begin()
      }
      assertConnectionAttempt(attempt)
      connected.value = true
      reconnecting.value = false
      reconnectPolicy.reset()
      recoveryFailed = false
      error.value = ''
      clearOnlineWaiter()
    } catch (failure) {
      if (failure instanceof V2CarrierAdmissionError && issued && cachedIssuedLink === issued) {
        cachedIssuedLink = undefined
      }
      // A direct endpoint is runtime-selected by the Control Plane. Its
      // short-lived Grant can outlive an account switch back to Relay (or an
      // Agent restart with a new endpoint), while a browser WebSocket failure
      // does not expose the HTTP 503 admission response that would otherwise
      // identify the stale route. Discard a failed cached direct link so the
      // next attempt asks the Control Plane for the current connection layer.
      if (issued?.link.connectionMode === 'direct' && cachedIssuedLink === issued && !canResume) {
        cachedIssuedLink = undefined
      }
      if (nextCarrier && carrier === nextCarrier) carrier = undefined
      nextCarrier?.close('remote/v2 establishment failed')
      connected.value = false
      if (!canResume && nextCarrier) {
        link?.close(failure instanceof Error ? failure : new Error('远程 Link 建立失败。'))
        eventClient?.dispose()
        fileTransfer?.dispose()
        rpc?.dispose()
        eventClient = undefined
        fileTransfer = undefined
        rpc = undefined
        link = undefined
      }
      throw failure
    } finally {
      if (connectionAbort === attempt) connectionAbort = undefined
    }
  }

  const ensureConnected = async () => {
    if (permanentlyClosed) throw new Error('远程客户端已关闭。')
    if (recoveryFailed) throw new Error('自动恢复预算已耗尽，请刷新页面或重新进入设备后显式重连。')
    if (connecting) return connecting
    if (connected.value && carrier && link?.isActive && rpc) return
    if (reconnectTimer) return waitForScheduledReconnect()
    if (browserIsOffline()) {
      error.value = '网络已离线，恢复后将自动重连。'
      reconnecting.value = true
      scheduleReconnect()
      throw new Error(error.value)
    }
    const request = establish().finally(() => {
      if (connecting === request) connecting = undefined
    })
    connecting = request
    try {
      await request
    } catch (failure) {
      if (!permanentlyClosed) {
        retryAfterMs = Math.max(retryAfterMs, v2RetryAfterMillis(failure))
        error.value = failure instanceof Error ? failure.message : '远程连接恢复失败。'
        if (isV2RetryableFailure(failure)) scheduleReconnect()
        else settleRecoveryFailure(error.value)
      }
      throw failure
    }
  }

  const scheduleReconnect = () => {
    if (permanentlyClosed || recoveryFailed || reconnectTimer) return
    reconnecting.value = true
    if (browserIsOffline()) {
      error.value = '网络已离线，恢复后将自动重连。'
      if (!onlineHandler && typeof window !== 'undefined') {
        onlineHandler = () => {
          clearOnlineWaiter()
          scheduleReconnect()
        }
        window.addEventListener('online', onlineHandler, { once: true })
      }
      return
    }
    const delay = reconnectPolicy.nextDelay(retryAfterMs)
    retryAfterMs = 0
    if (delay === undefined) {
      settleRecoveryFailure('自动恢复预算已耗尽，请刷新页面或重新进入设备后显式重连。')
      return
    }
    armReconnectTimer(delay)
  }

  const armReconnectTimer = (remainingMs: number) => {
    if (permanentlyClosed || recoveryFailed || reconnectTimer) return
    const chunk = Math.min(v2MaximumTimerDelayMs, Math.max(0, remainingMs))
    reconnectTimer = setTimeout(() => {
      reconnectTimer = undefined
      if (remainingMs > chunk) {
        armReconnectTimer(remainingMs - chunk)
        return
      }
      if (reconnectPolicy.durationExhausted) {
        settleRecoveryFailure('自动恢复预算已耗尽，请刷新页面或重新进入设备后显式重连。')
        return
      }
      const waiter = reconnectWaiter
      reconnectWaiter = undefined
      void ensureConnected().then(
        () => waiter?.resolve(),
        (failure) => waiter?.reject(failure),
      )
    }, chunk)
  }

  const waitForScheduledReconnect = () => {
    if (reconnectWaiter) return reconnectWaiter.promise
    let resolve!: () => void
    let reject!: (failure: unknown) => void
    const promise = new Promise<void>((onResolve, onReject) => {
      resolve = onResolve
      reject = onReject
    })
    reconnectWaiter = { promise, resolve, reject }
    return promise
  }

  const retireLinkState = (failure: Error) => {
    link?.close(failure)
    eventClient?.dispose()
    fileTransfer?.dispose()
    rpc?.dispose()
    eventClient = undefined
    fileTransfer = undefined
    rpc = undefined
    link = undefined
  }

  const settleRecoveryFailure = (message: string) => {
    if (permanentlyClosed || recoveryFailed) return
    recoveryFailed = true
    if (reconnectTimer) clearTimeout(reconnectTimer)
    reconnectTimer = undefined
    clearOnlineWaiter()
    reconnectWaiter?.reject(new Error(message))
    reconnectWaiter = undefined
    pendingIssueIdempotencyKey = ''
    ownershipRelease?.()
    ownershipRelease = undefined
    ownershipAbort?.abort()
    ownershipAbort = undefined
    ownershipReady = undefined
    ownershipKey = ''
    const failedCarrier = carrier
    carrier = undefined
    failedCarrier?.close('remote/v2 recovery stopped')
    retireLinkState(new Error(message))
    connected.value = false
    reconnecting.value = false
    error.value = message
  }

  const currentRPC = async () => {
    await ensureConnected()
    if (!rpc) throw new Error('远程 Link 不可用。')
    return rpc
  }

  const currentFileTransfer = async () => {
    await ensureConnected()
    if (!fileTransfer) throw new Error('远程文件传输不可用。')
    return fileTransfer
  }

  const currentEventClient = async () => {
    await ensureConnected()
    if (!eventClient) throw new Error('远程事件订阅不可用。')
    return eventClient
  }

  const downloadFile: RemoteRPCClient['downloadFile'] = async (
    relativePath,
    onProgress,
    context,
    expectedRevision,
  ) => {
    if (!context?.projectId) throw new Error('请先选择一个可用项目，再下载文件。')
    return (await currentFileTransfer()).download(
      relativePath,
      context,
      expectedRevision,
      onProgress,
    )
  }

  const uploadFile: RemoteRPCClient['uploadFile'] = async (
    relativePath,
    file,
    onProgress,
    context,
    expectedRevision,
  ) => {
    if (!context?.projectId) throw new Error('请先选择一个可用项目，再上传文件。')
    return (await currentFileTransfer()).upload(
      relativePath,
      file,
      context,
      expectedRevision,
      onProgress,
    )
  }

  const downloadTaskLog: RemoteRPCClient['downloadTaskLog'] = async (
    taskId,
    runId,
    generation,
    onProgress,
    context,
  ) => {
    if (!context?.projectId) throw new Error('请先选择一个可用项目，再下载任务日志。')
    if (!Number.isSafeInteger(generation) || generation < 1)
      throw new Error('任务日志 generation 无效。')
    const result = await (
      await currentFileTransfer()
    ).downloadPreparedSource(
      {
        prepareMethod: 'task.logs.download.prepare',
        prepareInput: { taskId, runId, generation },
        scope: 'remote.peer.task.control',
        expectedRevision: generation,
        maximumBytes: 64 << 20,
        requireFileName: true,
        expectedPreparedValues: { generation, snapshot: true },
      },
      context,
      onProgress,
    )
    if (!result.fileName) throw new Error('设备未返回安全的任务日志文件名。')
    return { blob: result.blob, fileName: result.fileName }
  }

  return {
    connected,
    reconnecting,
    error,
    connect: ensureConnected,
    async close() {
      permanentlyClosed = true
      connectionAbort?.abort()
      connectionAbort = undefined
      pendingIssueIdempotencyKey = ''
      cachedIssuedLink = undefined
      if (reconnectTimer) clearTimeout(reconnectTimer)
      reconnectTimer = undefined
      reconnectWaiter?.reject(new Error('远程客户端已关闭。'))
      reconnectWaiter = undefined
      reconnectPolicy.reset()
      clearOnlineWaiter()
      ownershipRelease?.()
      ownershipRelease = undefined
      ownershipAbort?.abort()
      ownershipAbort = undefined
      ownershipReady = undefined
      ownershipKey = ''
      link?.close(new Error('远程客户端已关闭。'))
      eventClient?.dispose()
      fileTransfer?.dispose()
      rpc?.dispose()
      carrier?.close()
      carrier = undefined
      rpc = undefined
      eventClient = undefined
      fileTransfer = undefined
      link = undefined
      connected.value = false
      reconnecting.value = false
    },
    getCapabilities: async (refresh = false): Promise<RemoteAgentCapabilities> =>
      (await currentRPC()).getCapabilities(refresh),
    call: async <T>(
      method: string,
      input: Record<string, unknown> = {},
      context?: RemoteRPCContext,
    ) => (await currentRPC()).call<T>(method, input, context),
    stream: async <T>(
      method: string,
      input: Record<string, unknown>,
      onDelta: (delta: T) => void,
      context?: RemoteRPCContext,
    ) => (await currentRPC()).stream<T>(method, input, onDelta, context),
    startStream: <TDelta, TResult = unknown>(
      method: string,
      input: Record<string, unknown>,
      onDelta: (delta: TDelta) => void,
      context?: RemoteRPCContext,
    ) => {
      let detached = false
      let handle: RemoteRPCStreamHandle<TResult> | undefined
      const result = currentRPC().then((client) => {
        handle = client.startStream<TDelta, TResult>(method, input, onDelta, context)
        if (detached) return handle.detach().then(() => handle!.result)
        return handle.result
      })
      return {
        result,
        async detach() {
          detached = true
          await handle?.detach()
        },
      }
    },
    subscribeAgentEvents: async (
      input: { afterSequence?: number; heartbeatSeconds?: number },
      onEvent: (event: RemotePeerRpcEvent) => void,
      context: RemoteRPCContext,
    ) => (await currentEventClient()).subscribe(input, onEvent, context),
    cancelAgentEventSubscriptions: async (context: RemoteRPCContext) =>
      (await currentEventClient()).cancel(context),
    downloadFile,
    downloadTaskLog,
    pauseDownloads: () => fileTransfer?.pauseDownloads(),
    resumeDownloads: () => fileTransfer?.resumeDownloads(),
    uploadFile,
  }
}
