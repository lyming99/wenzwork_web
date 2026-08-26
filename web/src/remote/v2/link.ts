import {
  create,
  fromBinary,
  toBinary,
  type DescMessage,
  type MessageShape,
} from '@bufbuild/protobuf'
import { ed25519 } from '@noble/curves/ed25519.js'

import {
  ChannelAcceptSchema,
  ChannelCloseSchema,
  StreamCloseSchema,
  StreamOpenSchema,
} from '@/generated/remote/v2/channel_pb'
import {
  Direction,
  EncryptedRecordSchema,
  FrameType,
  ProtocolErrorCode,
  type EncryptedRecord,
} from '@/generated/remote/v2/common_pb'
import {
  LinkConfirmSchema,
  LinkEnvelopeSchema,
  LinkInitSchema,
  LinkLeaseRenewSchema,
  LinkLeaseRenewedSchema,
  LinkReadySchema,
  RekeyAckSchema,
  RekeyCommitSchema,
  RekeyInitSchema,
  StreamKeyBoundarySchema,
  type LinkAccept,
  type LinkEnvelope,
  type LinkLeaseRenewed,
  type RekeyAck,
  type RekeyCommit,
  type RekeyInit,
} from '@/generated/remote/v2/device_link_pb'
import type { CarrierStreamRejected } from '@/generated/remote/v2/carrier_pb'

import { V2Carrier, V2CarrierBackpressureError } from './carrier'
import { V2ChannelManager } from './channelManager'
import {
  V2_CONTROL_CHANNEL_ID,
  V2_CONTROL_STREAM_ID,
  V2_CHANNEL_CONTROL_STREAM_ID,
  V2RecordSequencer,
  canonicalRekey,
  createX25519Ephemeral,
  deriveRekeyRoot,
  deriveRootKey,
  linkConfirmationMac,
  openRecord,
  recordKeyFor,
  sealRecord,
  signLinkInit,
  x25519SharedSecret,
  zero,
  base64UrlToBytes,
  type V2HandshakeBinding,
  type V2RecordMetadata,
  verifyLinkAccept,
} from './crypto'
import type { IssuedDeviceLink } from './deviceLink'

export interface V2LinkIdentity {
  controllerId: string
  keyVersion: number
  privateKey: Uint8Array
}

export interface V2LinkFrame {
  record: EncryptedRecord
  plaintext: Uint8Array
}

interface PendingRekey {
  id: string
  nextKeyId: bigint
  oldKeyId: bigint
  privateKey?: Uint8Array
  nextRoot?: Uint8Array
  initiated: boolean
  init?: RekeyInit
  ack?: RekeyAck
  commit?: RekeyCommit
}

interface CompletedRekey {
  id: string
  nextKeyId: bigint
  oldKeyId: bigint
  initiated: boolean
  init: RekeyInit
  ack?: RekeyAck
  commit: RekeyCommit
}

const cloneRekeyInit = (value: RekeyInit) =>
  fromBinary(RekeyInitSchema, toBinary(RekeyInitSchema, value))
const cloneRekeyAck = (value: RekeyAck) =>
  fromBinary(RekeyAckSchema, toBinary(RekeyAckSchema, value))
const cloneRekeyCommit = (value: RekeyCommit) =>
  fromBinary(RekeyCommitSchema, toBinary(RekeyCommitSchema, value))

const sameRekeyInitMessage = (left?: RekeyInit, right?: RekeyInit) =>
  Boolean(
    left &&
    right &&
    left.linkId === right.linkId &&
    left.rekeyId === right.rekeyId &&
    left.nextKeyId === right.nextKeyId &&
    equalBytes(left.ephemeralPublicKey, right.ephemeralPublicKey) &&
    equalBytes(left.identitySignature, right.identitySignature),
  )

const sameRekeyAckMessage = (left?: RekeyAck, right?: RekeyAck) =>
  Boolean(
    left &&
    right &&
    left.linkId === right.linkId &&
    left.rekeyId === right.rekeyId &&
    left.nextKeyId === right.nextKeyId &&
    equalBytes(left.ephemeralPublicKey, right.ephemeralPublicKey) &&
    equalBytes(left.identitySignature, right.identitySignature),
  )

const sameRekeyCommitMessage = (left: RekeyCommit, right: RekeyCommit) => {
  if (
    left.linkId !== right.linkId ||
    left.rekeyId !== right.rekeyId ||
    left.nextKeyId !== right.nextKeyId
  )
    return false
  const a = [...left.boundaries].sort((x, y) => x.streamId.localeCompare(y.streamId))
  const b = [...right.boundaries].sort((x, y) => x.streamId.localeCompare(y.streamId))
  return (
    a.length === b.length &&
    a.every(
      (value, index) =>
        value.streamId === b[index].streamId && value.nextSequence === b[index].nextSequence,
    )
  )
}

interface CarrierWaiter {
  resolve: () => void
  reject: (error: Error) => void
  timer: ReturnType<typeof setTimeout>
}

interface PendingLeaseRenewal {
  sequence: bigint
  promise: Promise<LinkLeaseRenewed>
  resolve: (value: LinkLeaseRenewed) => void
  reject: (error: Error) => void
  timer: ReturnType<typeof setTimeout>
}

const controlFrames = new Set<FrameType>([
  FrameType.LINK_CONFIRM,
  FrameType.LINK_READY,
  FrameType.LINK_LEASE_RENEW,
  FrameType.LINK_LEASE_RENEWED,
  FrameType.REKEY_INIT,
  FrameType.REKEY_ACK,
  FrameType.REKEY_COMMIT,
])

const defaultLinkError = '远程加密 Link 已中断。'
const linkReadyTimeoutMs = 15_000
const rekeyTimeoutMs = 30_000
const rekeyBaseDelayMs = 55 * 60_000
const rekeyJitterRangeMs = 11 * 60_000
const leaseRenewalIntervalMs = 30 * 60_000
const leaseDurationMs = 90 * 60_000
const leaseAcknowledgementTimeoutMs = 15_000
const leaseRetryDelayMs = 30_000

export class V2CarrierInterruptedError extends Error {
  constructor(message = 'remote/v2 Carrier was interrupted.') {
    super(message)
    this.name = 'V2CarrierInterruptedError'
  }
}

const waitWithTimeout = async <T>(promise: Promise<T>, timeoutMs: number, message: string) => {
  let timer: ReturnType<typeof setTimeout> | undefined
  try {
    return await Promise.race([
      promise,
      new Promise<T>((_, reject) => {
        timer = setTimeout(() => reject(new Error(message)), timeoutMs)
      }),
    ])
  } finally {
    if (timer) clearTimeout(timer)
  }
}

const secureRandomOffset = (range: number) => {
  if (!Number.isSafeInteger(range) || range <= 0) return 0
  const value = crypto.getRandomValues(new Uint32Array(1))[0]
  return Number(value % (range + 1)) - Math.floor(range / 2)
}

// A Link must rotate before one root generation carries an unbounded amount
// of traffic. These limits deliberately count plaintext before encryption so
// both clients make the same decision independent of protobuf overhead.
export const V2_REKEY_RECORD_LIMIT = 16_384
export const V2_REKEY_PLAINTEXT_BYTE_LIMIT = 64 * 1024 * 1024

const asTimestamp = (value: Date) => ({
  seconds: BigInt(Math.floor(value.getTime() / 1000)),
  nanos: (value.getTime() % 1000) * 1_000_000,
})

const dateFromTimestamp = (value: { seconds: bigint; nanos: number }) =>
  new Date(Number(value.seconds) * 1000 + Math.floor(value.nanos / 1_000_000))

/**
 * The in-memory E2EE Link. Carrier reconnects rebind this object instead of
 * recreating its root keys, Channels or per-Stream sequence state.
 */
export class V2Link {
  readonly id = crypto.randomUUID()
  readonly channels: V2ChannelManager
  readonly deviceIdentityKeyVersion: number
  readonly deviceId: string

  private carrier: V2Carrier
  // The signed grant is handshake-only material. Keep it in a mutable,
  // transient slot until LINK_INIT is queued, then drop the reference so a
  // long-lived Link/Channel object cannot accidentally expose a bearer.
  private handshakeGrant?: string
  private readonly grantId: string
  private readonly relayNodeId: string
  private readonly relayCellId: string
  private readonly targetConnectionEpoch: bigint
  private readonly expiresAt: string
  private readonly sequencer = new V2RecordSequencer()
  private readonly roots = new Map<bigint, Uint8Array>()
  private activeKeyId = 0n
  private binding: V2HandshakeBinding | undefined
  private devicePublicKey: Uint8Array
  private initialPrivate?: Uint8Array
  private active = false
  private carrierReady = true
  private closed = false
  private pendingAccept?: {
    resolve: (value: LinkAccept) => void
    reject: (error: Error) => void
    timer: ReturnType<typeof setTimeout>
  }
  private readyResolve!: () => void
  private readyReject!: (error: Error) => void
  private readonly readyPromise = new Promise<void>((resolve, reject) => {
    this.readyResolve = resolve
    this.readyReject = reject
  })
  private readonly listeners = new Map<FrameType, Set<(frame: V2LinkFrame) => void>>()
  private readonly carrierWaiters = new Set<CarrierWaiter>()
  private readonly activeStreams = new Map<string, string>()
  private readonly completedRekeys = new Map<string, CompletedRekey>()
  private readonly completedRekeyOrder: string[] = []
  private pendingRekey?: PendingRekey
  private rekeyTimer?: ReturnType<typeof setTimeout>
  private rekeyDeadlineTimer?: ReturnType<typeof setTimeout>
  private leaseRenewalTimer?: ReturnType<typeof setTimeout>
  private leaseExpiryTimer?: ReturnType<typeof setTimeout>
  private pendingLeaseRenewal?: PendingLeaseRenewal
  private leaseRenewalInterval?: number
  private leaseDuration?: number
  private leaseExpiresAt?: number
  private leaseRenewalSequence = 0n
  private trafficRecordCount = 0
  private trafficPlaintextBytes = 0
  private trafficRekeyRequested = false

  constructor(
    carrier: V2Carrier,
    issued: IssuedDeviceLink,
    private readonly identity: V2LinkIdentity,
    private readonly failureHandler?: (error: Error) => void,
  ) {
    this.carrier = carrier
    this.handshakeGrant = issued.link.deviceConnectionGrant
    this.grantId = issued.claims.grant_id
    this.relayNodeId = issued.link.relayNodeId
    this.relayCellId = issued.link.relayCellId
    this.targetConnectionEpoch = BigInt(issued.link.targetConnectionEpoch)
    this.expiresAt = issued.link.expiresAt
    this.devicePublicKey = base64UrlToBytes(issued.link.deviceIdentityPublicKey)
    this.deviceIdentityKeyVersion = issued.link.deviceIdentityKeyVersion
    this.deviceId = issued.claims.device_id
    this.channels = new V2ChannelManager(this)
  }

  get isActive() {
    return this.active && !this.closed
  }

  get currentKeyId() {
    return this.activeKeyId
  }

  get usesRenewableLease() {
    return this.leaseRenewalInterval !== undefined
  }

  get currentLeaseRenewalSequence() {
    return this.leaseRenewalSequence
  }

  get currentLeaseExpiresAt() {
    return this.leaseExpiresAt === undefined ? undefined : new Date(this.leaseExpiresAt)
  }

  async begin() {
    if (this.closed) throw new Error(defaultLinkError)
    const handshakeGrant = this.handshakeGrant
    if (!handshakeGrant) throw new Error('设备连接授权已不可用。')
    const ephemeral = createX25519Ephemeral()
    this.initialPrivate = ephemeral.privateKey
    const expiresAt = new Date(this.expiresAt)
    if (!Number.isFinite(expiresAt.getTime()) || expiresAt.getTime() <= Date.now())
      throw new Error('设备连接授权已过期。')
    const challenge = crypto.getRandomValues(new Uint8Array(32))
    const initialBinding: V2HandshakeBinding = {
      grantId: this.grantId,
      linkId: this.id,
      clientId: this.identity.controllerId,
      deviceId: this.deviceId,
      relayNodeId: this.relayNodeId,
      relayCellId: this.relayCellId,
      targetConnectionEpoch: this.targetConnectionEpoch,
      clientIdentityVersion: BigInt(this.identity.keyVersion),
      deviceIdentityVersion: 0n,
      clientEphemeralPublic: ephemeral.publicKey,
      deviceEphemeralPublic: new Uint8Array(),
      clientChallenge: challenge,
      deviceChallenge: new Uint8Array(),
      expiresAtUnixMilli: BigInt(expiresAt.getTime()),
    }
    const signature = signLinkInit(this.identity.privateKey, initialBinding)
    const accept = await this.waitAccept(async () => {
      const message = create(LinkInitSchema, {
        grantId: initialBinding.grantId,
        linkId: initialBinding.linkId,
        clientId: initialBinding.clientId,
        deviceId: initialBinding.deviceId,
        relayNodeId: initialBinding.relayNodeId,
        relayCellId: initialBinding.relayCellId,
        targetConnectionEpoch: initialBinding.targetConnectionEpoch,
        clientIdentityKeyVersion: initialBinding.clientIdentityVersion,
        clientEphemeralPublicKey: initialBinding.clientEphemeralPublic,
        clientChallenge: initialBinding.clientChallenge,
        expiresAt: asTimestamp(expiresAt),
        identitySignature: signature,
        deviceConnectionGrant: handshakeGrant,
      })
      try {
        await this.carrier.sendLink(
          create(LinkEnvelopeSchema, {
            linkId: this.id,
            body: { case: 'linkInit', value: message },
          }),
        )
      } finally {
        this.handshakeGrant = undefined
      }
    })
    const fullBinding = this.validateAccept(initialBinding, accept)
    const shared = x25519SharedSecret(ephemeral.privateKey, fullBinding.deviceEphemeralPublic)
    const root = deriveRootKey(shared, fullBinding)
    zero(shared)
    zero(this.initialPrivate)
    this.initialPrivate = undefined
    this.binding = fullBinding
    this.roots.set(1n, root)
    this.activeKeyId = 1n
    await this.sendEncrypted(
      FrameType.LINK_CONFIRM,
      V2_CONTROL_CHANNEL_ID,
      V2_CONTROL_STREAM_ID,
      toBinary(
        LinkConfirmSchema,
        create(LinkConfirmSchema, {
          linkId: this.id,
          transcriptMac: linkConfirmationMac(root, fullBinding),
        }),
      ),
    )
    await waitWithTimeout(this.readyPromise, linkReadyTimeoutMs, '等待 remote/v2 Link Ready 超时。')
    if (this.leaseRenewalInterval !== undefined) await this.renewLease()
    this.scheduleRekey()
  }

  attachCarrier(carrier: V2Carrier) {
    if (this.closed) throw new Error(defaultLinkError)
    this.carrier = carrier
    this.carrierReady = false
  }

  private markCarrierReady() {
    if (this.closed) return
    this.carrierReady = true
    for (const waiter of this.carrierWaiters) {
      clearTimeout(waiter.timer)
      waiter.resolve()
    }
    this.carrierWaiters.clear()
  }

  /** Waits for a replacement Carrier without rebuilding Link keys or Streams. */
  waitForCarrier(timeout = 30_000) {
    if (this.closed) return Promise.reject(new Error(defaultLinkError))
    if (this.carrier.isOpen && this.carrierReady) return Promise.resolve()
    return new Promise<void>((resolve, reject) => {
      const waiter: CarrierWaiter = {
        resolve: () => resolve(),
        reject,
        timer: setTimeout(() => {
          this.carrierWaiters.delete(waiter)
          reject(new Error('等待远程 Carrier 恢复超时。'))
        }, timeout),
      }
      this.carrierWaiters.add(waiter)
    })
  }

  async resume(
    lastAckByStream: Array<{
      channelId: string
      streamId: string
      acknowledgedSequence: bigint
    }> = [],
    confirmationTimeoutMs = linkReadyTimeoutMs,
  ) {
    if (!this.isActive) throw new Error(defaultLinkError)
    if (!Number.isFinite(confirmationTimeoutMs) || confirmationTimeoutMs <= 0)
      throw new Error('remote/v2 Link resume confirmation timeout is invalid.')
    const carrier = this.carrier
    const binding = this.binding
    if (!binding) throw new Error(defaultLinkError)
    let accept: LinkAccept
    try {
      // Queueing CarrierResume only proves that the browser socket accepted a
      // frame. The Device Agent replays the original signed LinkAccept after
      // it has found and rebound the Link; that is the recovery acknowledgement.
      accept = await this.waitAccept(
        () => carrier.resume(this.id, lastAckByStream),
        confirmationTimeoutMs,
        '等待 remote/v2 Link 恢复确认超时。',
      )
    } catch (failure) {
      if (
        failure instanceof V2CarrierInterruptedError ||
        this.carrier !== carrier ||
        !carrier.isOpen
      ) {
        throw failure instanceof V2CarrierInterruptedError
          ? failure
          : new V2CarrierInterruptedError(
              failure instanceof Error ? failure.message : 'remote/v2 Carrier was interrupted.',
            )
      }
      const error = failure instanceof Error ? failure : new Error(defaultLinkError)
      this.close(error)
      throw error
    }
    try {
      this.validateAccept(binding, accept)
      if (this.carrier !== carrier || !carrier.isOpen)
        throw new V2CarrierInterruptedError('remote/v2 Carrier changed during Link recovery.')
      this.markCarrierReady()
    } catch (failure) {
      if (failure instanceof V2CarrierInterruptedError) throw failure
      const error = failure instanceof Error ? failure : new Error(defaultLinkError)
      this.close(error)
      throw error
    }
    await this.retryPendingRekey()
    if (this.leaseRenewalInterval !== undefined) await this.renewLease()
  }

  get isResumePending() {
    return this.isActive && this.pendingAccept !== undefined
  }

  /** Re-sends only the idempotent CarrierResume while the original signed
   * LinkAccept waiter remains authoritative. This handles a Device route that
   * becomes resident after the first Resume reached an empty Relay route. */
  retryPendingResume(
    lastAckByStream: Array<{
      channelId: string
      streamId: string
      acknowledgedSequence: bigint
    }> = [],
  ) {
    if (!this.isResumePending) {
      return Promise.reject(new Error('remote/v2 Link resume is not pending.'))
    }
    return this.carrier.resume(this.id, lastAckByStream)
  }

  /** Ends only work waiting on the current physical Carrier. Link key and
   * Stream state remain available for the next recovery attempt. */
  handleCarrierInterrupted(carrier: V2Carrier, failure?: Error) {
    if (this.closed || this.carrier !== carrier) return
    this.carrierReady = false
    const pending = this.pendingAccept
    if (pending) {
      clearTimeout(pending.timer)
      this.pendingAccept = undefined
      pending.reject(new V2CarrierInterruptedError(failure?.message))
    }
    const renewal = this.pendingLeaseRenewal
    if (renewal) renewal.reject(new V2CarrierInterruptedError(failure?.message))
  }

  on(frameType: FrameType, listener: (frame: V2LinkFrame) => void) {
    const listeners = this.listeners.get(frameType) ?? new Set<(frame: V2LinkFrame) => void>()
    listeners.add(listener)
    this.listeners.set(frameType, listeners)
    return () => listeners.delete(listener)
  }

  async sendEncrypted(
    frameType: FrameType,
    channelId: string,
    streamId: string,
    plaintext: Uint8Array,
    keyIdOverride?: bigint,
  ) {
    const carrier = this.carrier
    if (
      this.closed ||
      !this.carrierReady ||
      !carrier.isOpen ||
      !this.binding ||
      this.activeKeyId === 0n
    )
      throw new Error(defaultLinkError)
    const keyId = keyIdOverride ?? this.activeKeyId
    const sequence = this.sequencer.next(keyId, Direction.CLIENT_TO_DEVICE, streamId)
    const metadata: V2RecordMetadata = {
      linkId: this.id,
      channelId,
      streamId,
      keyId,
      direction: Direction.CLIENT_TO_DEVICE,
      frameType,
      streamSequence: sequence,
    }
    const root = this.roots.get(keyId)
    if (!root) throw new Error(defaultLinkError)
    const recordKey = recordKeyFor(root, metadata)
    const ciphertext = sealRecord(recordKey, plaintext, metadata)
    zero(recordKey)
    if (this.closed || !this.carrierReady || this.carrier !== carrier || !carrier.isOpen)
      throw new V2CarrierInterruptedError('remote/v2 Carrier changed while encrypting a record.')
    await carrier.sendLink(
      create(LinkEnvelopeSchema, {
        linkId: this.id,
        body: {
          case: 'encrypted',
          value: create(EncryptedRecordSchema, {
            linkId: this.id,
            channelId,
            streamId,
            keyId,
            direction: Direction.CLIENT_TO_DEVICE,
            frameType,
            streamSequence: sequence,
            ciphertext,
          }),
        },
      }),
    )
    this.recordTrafficForRekey(frameType, plaintext.length)
    this.trackOutboundStream(frameType, channelId, streamId, plaintext)
  }

  async sendMessage<Schema extends DescMessage>(
    frameType: FrameType,
    channelId: string,
    streamId: string,
    schema: Schema,
    message: MessageShape<Schema>,
    keyId?: bigint,
  ) {
    await this.sendEncrypted(frameType, channelId, streamId, toBinary(schema, message), keyId)
  }

  async forceRekey() {
    if (!this.isActive) return
    if (this.pendingRekey) {
      this.startRekeyDeadline(this.pendingRekey)
      if (this.pendingRekey.initiated) {
        if (this.pendingRekey.commit) {
          await this.sendPendingRekeyCommit(this.pendingRekey)
        } else if (this.pendingRekey.init) {
          await this.sendMessage(
            FrameType.REKEY_INIT,
            V2_CONTROL_CHANNEL_ID,
            V2_CONTROL_STREAM_ID,
            RekeyInitSchema,
            this.pendingRekey.init,
            this.pendingRekey.oldKeyId,
          )
        }
      }
      return
    }
    const nextKeyId = this.activeKeyId + 1n
    const oldKeyId = this.activeKeyId
    const ephemeral = createX25519Ephemeral()
    const rekeyId = crypto.randomUUID()
    this.pendingRekey = {
      id: rekeyId,
      nextKeyId,
      oldKeyId,
      privateKey: ephemeral.privateKey,
      initiated: true,
    }
    this.startRekeyDeadline(this.pendingRekey)
    const signature = ed25519.sign(
      canonicalRekey('init', this.id, rekeyId, nextKeyId, ephemeral.publicKey),
      this.identity.privateKey,
    )
    const init = create(RekeyInitSchema, {
      linkId: this.id,
      rekeyId,
      nextKeyId,
      ephemeralPublicKey: ephemeral.publicKey,
      identitySignature: signature,
    })
    this.pendingRekey.init = cloneRekeyInit(init)
    try {
      await this.sendMessage(
        FrameType.REKEY_INIT,
        V2_CONTROL_CHANNEL_ID,
        V2_CONTROL_STREAM_ID,
        RekeyInitSchema,
        init,
        oldKeyId,
      )
    } catch (error) {
      // Keep the exact INIT and ephemeral key so a Carrier retry can resend
      // the same rekey_id instead of creating a competing generation.
      throw error
    }
  }

  async handleLinkEnvelope(envelope: LinkEnvelope) {
    if (this.closed || envelope.linkId !== this.id) return false
    if (envelope.body.case === 'linkAccept') {
      if (this.pendingAccept) {
        const pending = this.pendingAccept
        this.pendingAccept = undefined
        clearTimeout(pending.timer)
        pending.resolve(envelope.body.value)
      }
      return true
    }
    if (envelope.body.case !== 'encrypted') return false
    // A resumed Carrier is not an application-delivery boundary until the
    // Device has replayed and we have verified its signed LinkAccept. Frames
    // that raced ahead of that proof are intentionally discarded; durable
    // Event/File/RPC layers replay them from their own cursors afterwards.
    if (!this.carrierReady) return true
    await this.handleEncrypted(envelope.body.value)
    return true
  }

  /** Converts Relay queue feedback into the same Stream-local close signal
   * used by the encrypted protocol. No Link key or sibling Stream is touched. */
  handleCarrierStreamRejected(rejection: CarrierStreamRejected) {
    if (
      this.closed ||
      rejection.linkId !== this.id ||
      !rejection.channelId ||
      !rejection.streamId ||
      rejection.reason === ProtocolErrorCode.UNSPECIFIED
    ) {
      return false
    }
    this.activeStreams.delete(rejection.streamId)
    const close = create(StreamCloseSchema, {
      channelId: rejection.channelId,
      streamId: rejection.streamId,
      reason: rejection.reason,
    })
    const frame: V2LinkFrame = {
      record: create(EncryptedRecordSchema, {
        linkId: this.id,
        channelId: rejection.channelId,
        streamId: rejection.streamId,
        keyId: this.activeKeyId,
        direction: Direction.DEVICE_TO_CLIENT,
        frameType: FrameType.STREAM_CLOSE,
      }),
      plaintext: toBinary(StreamCloseSchema, close),
    }
    for (const listener of this.listeners.get(FrameType.STREAM_CLOSE) ?? []) {
      try {
        listener(frame)
      } catch {
        // A Stream owner cannot escalate transport backpressure to the Link.
      }
    }
    return true
  }

  close(error: Error = new Error(defaultLinkError)) {
    if (this.closed) return
    this.closed = true
    this.active = false
    this.carrierReady = false
    if (this.rekeyTimer) clearTimeout(this.rekeyTimer)
    if (this.rekeyDeadlineTimer) clearTimeout(this.rekeyDeadlineTimer)
    if (this.leaseRenewalTimer) clearTimeout(this.leaseRenewalTimer)
    if (this.leaseExpiryTimer) clearTimeout(this.leaseExpiryTimer)
    this.rekeyDeadlineTimer = undefined
    this.leaseRenewalTimer = undefined
    this.leaseExpiryTimer = undefined
    if (this.pendingAccept) {
      clearTimeout(this.pendingAccept.timer)
      this.pendingAccept.reject(error)
      this.pendingAccept = undefined
    }
    if (this.pendingLeaseRenewal) {
      clearTimeout(this.pendingLeaseRenewal.timer)
      this.pendingLeaseRenewal.reject(error)
      this.pendingLeaseRenewal = undefined
    }
    this.readyReject(error)
    this.channels.fail(error)
    for (const waiter of this.carrierWaiters) {
      clearTimeout(waiter.timer)
      waiter.reject(error)
    }
    this.carrierWaiters.clear()
    for (const root of this.roots.values()) zero(root)
    this.roots.clear()
    this.handshakeGrant = undefined
    zero(this.initialPrivate)
    zero(this.pendingRekey?.privateKey)
    zero(this.pendingRekey?.nextRoot)
    this.pendingRekey = undefined
    this.completedRekeys.clear()
    this.completedRekeyOrder.length = 0
    this.listeners.clear()
    this.activeStreams.clear()
  }

  private async waitAccept(
    send: () => Promise<void>,
    timeoutMs = linkReadyTimeoutMs,
    timeoutMessage = '远程设备握手超时。',
  ) {
    if (this.pendingAccept) throw new Error('remote/v2 Link handshake is already active.')
    const result = new Promise<LinkAccept>((resolve, reject) => {
      const timer = setTimeout(() => {
        this.pendingAccept = undefined
        reject(new Error(timeoutMessage))
      }, timeoutMs)
      this.pendingAccept = { resolve, reject, timer }
    })
    try {
      await send()
      return await result
    } catch (error) {
      this.clearPendingAccept()
      throw error
    }
  }

  private validateAccept(initial: V2HandshakeBinding, accept: LinkAccept): V2HandshakeBinding {
    if (!accept.expiresAt) throw new Error('设备身份握手验证失败。')
    const expiresAt = dateFromTimestamp(accept.expiresAt)
    const binding: V2HandshakeBinding = {
      ...initial,
      grantId: accept.grantId,
      linkId: accept.linkId,
      clientId: accept.clientId,
      deviceId: accept.deviceId,
      relayNodeId: accept.relayNodeId,
      relayCellId: accept.relayCellId,
      targetConnectionEpoch: accept.targetConnectionEpoch,
      clientIdentityVersion: initial.clientIdentityVersion,
      deviceIdentityVersion: accept.deviceIdentityKeyVersion,
      clientEphemeralPublic: accept.clientEphemeralPublicKey.slice(),
      deviceEphemeralPublic: accept.deviceEphemeralPublicKey.slice(),
      clientChallenge: accept.clientChallenge.slice(),
      deviceChallenge: accept.deviceChallenge.slice(),
      expiresAtUnixMilli: BigInt(expiresAt.getTime()),
    }
    if (
      binding.grantId !== initial.grantId ||
      binding.linkId !== initial.linkId ||
      binding.clientId !== initial.clientId ||
      binding.deviceId !== initial.deviceId ||
      binding.relayNodeId !== initial.relayNodeId ||
      binding.relayCellId !== initial.relayCellId ||
      binding.targetConnectionEpoch !== initial.targetConnectionEpoch ||
      binding.clientIdentityVersion !== initial.clientIdentityVersion ||
      binding.deviceIdentityVersion !== BigInt(this.deviceIdentityKeyVersion) ||
      !equalBytes(binding.clientEphemeralPublic, initial.clientEphemeralPublic) ||
      !equalBytes(binding.clientChallenge, initial.clientChallenge) ||
      (initial.deviceEphemeralPublic.length > 0 &&
        !equalBytes(binding.deviceEphemeralPublic, initial.deviceEphemeralPublic)) ||
      (initial.deviceChallenge.length > 0 &&
        !equalBytes(binding.deviceChallenge, initial.deviceChallenge)) ||
      expiresAt.getTime() !== Number(initial.expiresAtUnixMilli) ||
      !verifyLinkAccept(this.devicePublicKey, binding, accept.identitySignature)
    ) {
      throw new Error('设备身份握手验证失败。')
    }
    return binding
  }

  private async handleEncrypted(record: EncryptedRecord) {
    try {
      if (
        !this.binding ||
        record.linkId !== this.id ||
        record.direction !== Direction.DEVICE_TO_CLIENT ||
        record.keyId === 0n ||
        record.streamSequence === 0n ||
        record.ciphertext.length === 0
      ) {
        throw new Error('remote/v2 encrypted record is invalid.')
      }
      const metadata: V2RecordMetadata = {
        linkId: record.linkId,
        channelId: record.channelId,
        streamId: record.streamId,
        keyId: record.keyId,
        direction: record.direction,
        frameType: record.frameType,
        streamSequence: record.streamSequence,
      }
      const root = this.roots.get(record.keyId)
      if (!root) throw new Error('remote/v2 key generation is unavailable.')
      const recordKey = recordKeyFor(root, metadata)
      const plaintext = openRecord(recordKey, record.ciphertext, metadata)
      zero(recordKey)
      if (!this.sequencer.accept(metadata)) {
        // A repeated old record is a Stream-level replay, never a reason to
        // close unrelated Channels or the Carrier.
        return
      }
      // The Client is the deterministic rekey initiator. Count accepted
      // Device-to-Client traffic too, so a long download or Event/AI stream
      // rotates its generation even when the Client is otherwise idle.
      this.recordTrafficForRekey(record.frameType, plaintext.length)
      await this.dispatch(record, plaintext)
    } catch (error) {
      // AEAD failure, metadata confusion or key-generation fork is Link-fatal.
      // No plaintext or old-protocol fallback is attempted.
      this.fail(error instanceof Error ? error : new Error(defaultLinkError))
    }
  }

  private async dispatch(record: EncryptedRecord, plaintext: Uint8Array) {
    try {
      switch (record.frameType) {
        case FrameType.LINK_READY: {
          const ready = fromBinary(LinkReadySchema, plaintext)
          if (ready.linkId !== this.id || ready.activeKeyId !== this.activeKeyId)
            throw new Error('remote/v2 Link ready is invalid.')
          const renewalSeconds = ready.leaseRenewalIntervalSeconds
          const durationSeconds = ready.leaseDurationSeconds
          if (
            (renewalSeconds === 0) !== (durationSeconds === 0) ||
            (renewalSeconds !== 0 &&
              (renewalSeconds * 1000 !== leaseRenewalIntervalMs ||
                durationSeconds * 1000 !== leaseDurationMs))
          ) {
            throw new Error('remote/v2 Link lease policy is invalid.')
          }
          if (renewalSeconds !== 0) {
            this.leaseRenewalInterval = leaseRenewalIntervalMs
            this.leaseDuration = leaseDurationMs
          }
          this.active = true
          this.readyResolve()
          return
        }
        case FrameType.LINK_LEASE_RENEWED:
          this.handleLeaseRenewed(fromBinary(LinkLeaseRenewedSchema, plaintext))
          return
        case FrameType.CHANNEL_ACCEPT:
          this.channels.handleAccept(fromBinary(ChannelAcceptSchema, plaintext))
          return
        case FrameType.CHANNEL_CLOSE:
          {
            const close = fromBinary(ChannelCloseSchema, plaintext)
            this.removeChannelStreams(close.channelId)
            this.channels.handleClose(close.channelId)
          }
          return
        case FrameType.REKEY_ACK:
          await this.handleRekeyAck(fromBinary(RekeyAckSchema, plaintext), record.keyId)
          return
        case FrameType.REKEY_INIT:
          await this.handleRekeyInit(fromBinary(RekeyInitSchema, plaintext), record.keyId)
          return
        case FrameType.REKEY_COMMIT:
          this.handleRekeyCommit(fromBinary(RekeyCommitSchema, plaintext))
          return
        default:
          this.trackInboundStream(record, plaintext)
          for (const listener of this.listeners.get(record.frameType) ?? [])
            listener({ record, plaintext })
      }
    } catch (error) {
      if (controlFrames.has(record.frameType)) {
        if (this.isTransientRekeyError(error)) return
        this.fail(error instanceof Error ? error : new Error(defaultLinkError))
        return
      }
      // A malformed RPC/file/event payload stays confined to its Stream. The
      // owning client receives a close notification and can retry only it.
      for (const listener of this.listeners.get(FrameType.STREAM_CLOSE) ?? []) {
        try {
          listener({
            record: { ...record, frameType: FrameType.STREAM_CLOSE },
            plaintext: toBinary(
              StreamCloseSchema,
              create(StreamCloseSchema, {
                channelId: record.channelId,
                streamId: record.streamId,
                reason: ProtocolErrorCode.FRAME_INVALID,
              }),
            ),
          })
        } catch {
          // A Stream observer cannot escalate its own protocol failure to the Link.
        }
      }
    }
  }

  private async handleRekeyAck(ack: RekeyAck, oldKeyId: bigint) {
    const pending = this.pendingRekey
    const completed = this.completedRekeys.get(ack.rekeyId)
    if (!pending && completed) {
      if (
        !this.validRekeyAck(ack, completed.nextKeyId) ||
        !sameRekeyAckMessage(ack, completed.ack)
      ) {
        throw new Error('remote/v2 rekey acknowledgement is invalid.')
      }
      await this.sendPendingRekeyCommit(completed)
      return
    }
    if (
      !pending ||
      !pending.initiated ||
      ack.linkId !== this.id ||
      ack.rekeyId !== pending.id ||
      ack.nextKeyId !== pending.nextKeyId ||
      !this.validRekeyAck(ack, pending.nextKeyId)
    ) {
      throw new Error('remote/v2 rekey acknowledgement is invalid.')
    }
    if (pending.ack) {
      if (!sameRekeyAckMessage(ack, pending.ack))
        throw new Error('remote/v2 rekey acknowledgement is invalid.')
      await this.sendPendingRekeyCommit(pending)
      return
    }
    if (!pending.privateKey) throw new Error('remote/v2 rekey acknowledgement is invalid.')
    const oldRoot = this.roots.get(oldKeyId)
    if (!oldRoot) throw new Error('remote/v2 rekey root is unavailable.')
    const shared = x25519SharedSecret(pending.privateKey, ack.ephemeralPublicKey)
    pending.nextRoot = deriveRekeyRoot(oldRoot, shared, this.id, pending.id, pending.nextKeyId)
    zero(shared)
    zero(pending.privateKey)
    pending.privateKey = undefined
    pending.ack = cloneRekeyAck(ack)
    pending.commit = create(RekeyCommitSchema, {
      linkId: this.id,
      rekeyId: pending.id,
      nextKeyId: pending.nextKeyId,
      boundaries: this.streamBoundaries(),
    })
    await this.sendPendingRekeyCommit(pending)
    this.activateRekey(pending)
  }

  private async handleRekeyInit(init: RekeyInit, oldKeyId: bigint) {
    const completed = this.completedRekeys.get(init.rekeyId)
    if (completed) {
      if (
        !this.validRekeyInit(init, completed.nextKeyId) ||
        !sameRekeyInitMessage(init, completed.init) ||
        !completed.ack
      ) {
        throw new Error('remote/v2 rekey request is invalid.')
      }
      await this.sendMessage(
        FrameType.REKEY_ACK,
        V2_CONTROL_CHANNEL_ID,
        V2_CONTROL_STREAM_ID,
        RekeyAckSchema,
        completed.ack,
        completed.oldKeyId,
      )
      return
    }
    const pending = this.pendingRekey
    if (pending) {
      if (
        !pending.initiated &&
        pending.id === init.rekeyId &&
        pending.nextKeyId === init.nextKeyId &&
        pending.init &&
        sameRekeyInitMessage(init, pending.init) &&
        pending.ack
      ) {
        await this.sendMessage(
          FrameType.REKEY_ACK,
          V2_CONTROL_CHANNEL_ID,
          V2_CONTROL_STREAM_ID,
          RekeyAckSchema,
          pending.ack,
          pending.oldKeyId,
        )
        return
      }
      throw new Error('remote/v2 rekey request is invalid.')
    }
    if (!this.validRekeyInit(init, this.activeKeyId + 1n)) {
      throw new Error('remote/v2 rekey request is invalid.')
    }
    const ephemeral = createX25519Ephemeral()
    const oldRoot = this.roots.get(oldKeyId)
    if (!oldRoot) throw new Error('remote/v2 rekey root is unavailable.')
    const shared = x25519SharedSecret(ephemeral.privateKey, init.ephemeralPublicKey)
    const nextRoot = deriveRekeyRoot(oldRoot, shared, this.id, init.rekeyId, init.nextKeyId)
    zero(shared)
    const pendingResponder: PendingRekey = {
      id: init.rekeyId,
      nextKeyId: init.nextKeyId,
      oldKeyId,
      nextRoot,
      initiated: false,
      init: cloneRekeyInit(init),
    }
    this.pendingRekey = pendingResponder
    this.startRekeyDeadline(pendingResponder)
    const signature = ed25519.sign(
      canonicalRekey('ack', this.id, init.rekeyId, init.nextKeyId, ephemeral.publicKey),
      this.identity.privateKey,
    )
    pendingResponder.ack = create(RekeyAckSchema, {
      linkId: this.id,
      rekeyId: init.rekeyId,
      nextKeyId: init.nextKeyId,
      ephemeralPublicKey: ephemeral.publicKey,
      identitySignature: signature,
    })
    zero(ephemeral.privateKey)
    await this.sendMessage(
      FrameType.REKEY_ACK,
      V2_CONTROL_CHANNEL_ID,
      V2_CONTROL_STREAM_ID,
      RekeyAckSchema,
      pendingResponder.ack,
      oldKeyId,
    )
  }

  private handleRekeyCommit(commit: RekeyCommit) {
    const pending = this.pendingRekey
    const completed = this.completedRekeys.get(commit.rekeyId)
    if (completed) {
      if (
        !this.validRekeyCommit(commit, completed.nextKeyId) ||
        !sameRekeyCommitMessage(commit, completed.commit)
      ) {
        throw new Error('remote/v2 rekey commit is invalid.')
      }
      return
    }
    if (
      !pending ||
      pending.initiated ||
      !pending.nextRoot ||
      commit.linkId !== this.id ||
      commit.rekeyId !== pending.id ||
      commit.nextKeyId !== pending.nextKeyId ||
      !this.validRekeyCommit(commit, pending.nextKeyId)
    ) {
      throw new Error('remote/v2 rekey commit is invalid.')
    }
    pending.commit = cloneRekeyCommit(commit)
    this.activateRekey(pending)
  }

  private activateRekey(pending: PendingRekey) {
    if (!pending.nextRoot || !pending.init || !pending.commit)
      throw new Error('remote/v2 rekey root is unavailable.')
    this.rememberCompletedRekey({
      id: pending.id,
      nextKeyId: pending.nextKeyId,
      oldKeyId: pending.oldKeyId,
      initiated: pending.initiated,
      init: cloneRekeyInit(pending.init),
      ack: pending.ack ? cloneRekeyAck(pending.ack) : undefined,
      commit: cloneRekeyCommit(pending.commit),
    })
    this.roots.set(pending.nextKeyId, pending.nextRoot)
    this.activeKeyId = pending.nextKeyId
    this.pendingRekey = undefined
    if (this.rekeyDeadlineTimer) clearTimeout(this.rekeyDeadlineTimer)
    this.rekeyDeadlineTimer = undefined
    this.trafficRecordCount = 0
    this.trafficPlaintextBytes = 0
    this.trafficRekeyRequested = false
    for (const [keyId, root] of this.roots) {
      if (keyId + 1n < this.activeKeyId) {
        zero(root)
        this.roots.delete(keyId)
      }
    }
    this.scheduleRekey()
  }

  private rememberCompletedRekey(value: CompletedRekey) {
    if (this.completedRekeys.has(value.id)) {
      const index = this.completedRekeyOrder.indexOf(value.id)
      if (index >= 0) this.completedRekeyOrder.splice(index, 1)
    }
    this.completedRekeys.set(value.id, value)
    this.completedRekeyOrder.push(value.id)
    while (this.completedRekeyOrder.length > 4) {
      const oldest = this.completedRekeyOrder.shift()
      if (oldest) this.completedRekeys.delete(oldest)
    }
  }

  private async sendPendingRekeyCommit(value: PendingRekey | CompletedRekey) {
    const commit = value.commit
    if (!commit) throw new Error('remote/v2 rekey commit is unavailable.')
    await this.sendMessage(
      FrameType.REKEY_COMMIT,
      V2_CONTROL_CHANNEL_ID,
      V2_CONTROL_STREAM_ID,
      RekeyCommitSchema,
      commit,
      value.oldKeyId,
    )
  }

  private async retryPendingRekey() {
    const pending = this.pendingRekey
    if (!pending) {
      const latestId = this.completedRekeyOrder[this.completedRekeyOrder.length - 1]
      const completed = latestId ? this.completedRekeys.get(latestId) : undefined
      if (!completed) return
      if (completed.initiated) {
        await this.sendPendingRekeyCommit(completed)
      } else if (completed.ack) {
        await this.sendMessage(
          FrameType.REKEY_ACK,
          V2_CONTROL_CHANNEL_ID,
          V2_CONTROL_STREAM_ID,
          RekeyAckSchema,
          completed.ack,
          completed.oldKeyId,
        )
      }
      return
    }
    this.startRekeyDeadline(pending)
    if (!pending.initiated) {
      if (pending.ack) {
        await this.sendMessage(
          FrameType.REKEY_ACK,
          V2_CONTROL_CHANNEL_ID,
          V2_CONTROL_STREAM_ID,
          RekeyAckSchema,
          pending.ack,
          pending.oldKeyId,
        )
      }
      return
    }
    if (pending.commit) {
      await this.sendPendingRekeyCommit(pending)
      return
    }
    if (pending.init) {
      await this.sendMessage(
        FrameType.REKEY_INIT,
        V2_CONTROL_CHANNEL_ID,
        V2_CONTROL_STREAM_ID,
        RekeyInitSchema,
        pending.init,
        pending.oldKeyId,
      )
    }
  }

  private validRekeyInit(value: RekeyInit, expectedKeyId: bigint) {
    return (
      value.linkId === this.id &&
      value.nextKeyId === expectedKeyId &&
      value.rekeyId.length > 0 &&
      value.ephemeralPublicKey.length === 32 &&
      value.identitySignature.length === 64 &&
      ed25519.verify(
        value.identitySignature,
        canonicalRekey('init', this.id, value.rekeyId, value.nextKeyId, value.ephemeralPublicKey),
        this.devicePublicKey,
        { zip215: false },
      )
    )
  }

  private validRekeyAck(value: RekeyAck, expectedKeyId: bigint) {
    return (
      value.linkId === this.id &&
      value.nextKeyId === expectedKeyId &&
      value.rekeyId.length > 0 &&
      value.ephemeralPublicKey.length === 32 &&
      value.identitySignature.length === 64 &&
      ed25519.verify(
        value.identitySignature,
        canonicalRekey('ack', this.id, value.rekeyId, value.nextKeyId, value.ephemeralPublicKey),
        this.devicePublicKey,
        { zip215: false },
      )
    )
  }

  private validRekeyCommit(value: RekeyCommit, expectedKeyId: bigint) {
    if (value.linkId !== this.id || value.nextKeyId !== expectedKeyId || !value.rekeyId)
      return false
    let previous = ''
    for (const boundary of [...value.boundaries].sort((left, right) =>
      left.streamId.localeCompare(right.streamId),
    )) {
      if (!boundary.streamId || boundary.streamId === previous || boundary.nextSequence === 0n)
        return false
      previous = boundary.streamId
    }
    return true
  }

  private isTransientRekeyError(error: unknown) {
    return error instanceof V2CarrierBackpressureError || !this.carrier.isOpen
  }

  private async renewLease() {
    const renewalInterval = this.leaseRenewalInterval
    const duration = this.leaseDuration
    if (renewalInterval === undefined || duration === undefined) return
    if (!this.isActive) throw new Error('remote/v2 Link lease is unavailable.')
    if (this.leaseExpiresAt !== undefined && this.leaseExpiresAt <= Date.now())
      throw new Error('remote/v2 Link lease has expired.')
    if (this.pendingLeaseRenewal) {
      await this.pendingLeaseRenewal.promise
      return
    }
    const sequence = this.leaseRenewalSequence + 1n
    let resolve!: (value: LinkLeaseRenewed) => void
    let reject!: (error: Error) => void
    const promise = new Promise<LinkLeaseRenewed>((accept, decline) => {
      resolve = accept
      reject = decline
    })
    const pending: PendingLeaseRenewal = {
      sequence,
      promise,
      resolve,
      reject,
      timer: setTimeout(
        () => reject(new Error('remote/v2 Link lease renewal timed out.')),
        leaseAcknowledgementTimeoutMs,
      ),
    }
    this.pendingLeaseRenewal = pending
    try {
      await this.sendMessage(
        FrameType.LINK_LEASE_RENEW,
        V2_CONTROL_CHANNEL_ID,
        V2_CONTROL_STREAM_ID,
        LinkLeaseRenewSchema,
        create(LinkLeaseRenewSchema, { linkId: this.id, renewalSequence: sequence }),
      )
      await promise
    } finally {
      if (this.pendingLeaseRenewal === pending) {
        clearTimeout(pending.timer)
        this.pendingLeaseRenewal = undefined
      }
    }
  }

  private handleLeaseRenewed(renewed: LinkLeaseRenewed) {
    if (
      renewed.linkId !== this.id ||
      renewed.renewalSequence <= 0n ||
      renewed.leaseRenewalIntervalSeconds * 1000 !== leaseRenewalIntervalMs ||
      renewed.leaseDurationSeconds * 1000 !== leaseDurationMs
    ) {
      throw new Error('remote/v2 Link lease acknowledgement is invalid.')
    }
    if (renewed.renewalSequence === this.leaseRenewalSequence) return
    const pending = this.pendingLeaseRenewal
    if (renewed.renewalSequence !== this.leaseRenewalSequence + 1n) {
      throw new Error('remote/v2 Link lease sequence is invalid.')
    }
    this.leaseRenewalSequence = renewed.renewalSequence
    this.leaseExpiresAt = Date.now() + leaseDurationMs - leaseAcknowledgementTimeoutMs
    this.scheduleLeaseTimers()
    pending?.resolve(renewed)
  }

  private scheduleLeaseTimers() {
    if (this.leaseRenewalTimer) clearTimeout(this.leaseRenewalTimer)
    if (this.leaseExpiryTimer) clearTimeout(this.leaseExpiryTimer)
    if (this.closed || this.leaseExpiresAt === undefined) return
    this.leaseRenewalTimer = setTimeout(
      () => this.runScheduledLeaseRenewal(),
      this.leaseRenewalInterval ?? leaseRenewalIntervalMs,
    )
    this.leaseExpiryTimer = setTimeout(
      () => {
        if (this.closed || (this.leaseExpiresAt ?? 0) > Date.now()) return
        this.fail(new Error('remote/v2 Link lease expired before Device renewal.'))
      },
      Math.max(0, this.leaseExpiresAt - Date.now()),
    )
  }

  private runScheduledLeaseRenewal() {
    if (this.closed) return
    void this.renewLease().catch(() => {
      if (this.closed) return
      const remaining = (this.leaseExpiresAt ?? 0) - Date.now()
      if (remaining <= 0) {
        this.fail(new Error('remote/v2 Link lease expired before Device renewal.'))
        return
      }
      if (this.leaseRenewalTimer) clearTimeout(this.leaseRenewalTimer)
      this.leaseRenewalTimer = setTimeout(
        () => this.runScheduledLeaseRenewal(),
        Math.min(leaseRetryDelayMs, remaining),
      )
    })
  }

  private scheduleRekey() {
    if (this.rekeyTimer) clearTimeout(this.rekeyTimer)
    // The device's policy may request an earlier rekey. This independent soft
    // timer guarantees that a long-lived interactive Link never waits for key
    // expiry before beginning its non-blocking control exchange.
    this.rekeyTimer = setTimeout(
      () => {
        void this.forceRekey().catch((error) => {
          if (this.isTransientRekeyError(error)) return
          this.fail(error instanceof Error ? error : new Error(defaultLinkError))
        })
      },
      rekeyBaseDelayMs + secureRandomOffset(rekeyJitterRangeMs),
    )
  }

  private startRekeyDeadline(pending: PendingRekey) {
    if (this.rekeyDeadlineTimer) clearTimeout(this.rekeyDeadlineTimer)
    this.rekeyDeadlineTimer = setTimeout(() => {
      if (this.closed || this.pendingRekey !== pending) return
      if (!this.carrier.isOpen) {
        // Resume retransmits this exact rekey operation and starts a fresh
        // deadline. A transport outage alone must not discard Link keys.
        this.rekeyDeadlineTimer = undefined
        return
      }
      this.fail(new Error('remote/v2 rekey timed out.'))
    }, rekeyTimeoutMs)
  }

  private fail(error: Error) {
    if (this.closed) return
    this.close(error)
    this.failureHandler?.(error)
  }

  private trackOutboundStream(
    frameType: FrameType,
    channelId: string,
    streamId: string,
    plaintext: Uint8Array,
  ) {
    this.trackStream(frameType, channelId, streamId, plaintext)
  }

  private trackInboundStream(record: EncryptedRecord, plaintext: Uint8Array) {
    this.trackStream(record.frameType, record.channelId, record.streamId, plaintext)
  }

  private trackStream(
    frameType: FrameType,
    channelId: string,
    streamId: string,
    plaintext: Uint8Array,
  ) {
    if (frameType === FrameType.CHANNEL_CLOSE) {
      this.removeChannelStreams(channelId)
      return
    }
    try {
      if (frameType === FrameType.STREAM_OPEN) {
        const open = fromBinary(StreamOpenSchema, plaintext)
        if (open.streamId && open.channelId) this.activeStreams.set(open.streamId, open.channelId)
        return
      }
      if (frameType === FrameType.STREAM_CLOSE) {
        const close = fromBinary(StreamCloseSchema, plaintext)
        this.activeStreams.delete(close.streamId)
        return
      }
    } catch {
      // The receiving RPC/file/event owner will contain a malformed business
      // frame at its Stream boundary. It must not change Link-level rekeying.
      return
    }
    if (
      streamId &&
      streamId !== V2_CONTROL_STREAM_ID &&
      streamId !== V2_CHANNEL_CONTROL_STREAM_ID
    ) {
      this.activeStreams.set(streamId, channelId)
    }
  }

  private removeChannelStreams(channelId: string) {
    for (const [streamId, streamChannelId] of this.activeStreams) {
      if (streamChannelId === channelId) this.activeStreams.delete(streamId)
    }
  }

  private streamBoundaries() {
    return [...this.activeStreams.keys()]
      .sort()
      .map((streamId) => create(StreamKeyBoundarySchema, { streamId, nextSequence: 1n }))
  }

  private recordTrafficForRekey(frameType: FrameType, plaintextBytes: number) {
    if (controlFrames.has(frameType) || this.trafficRekeyRequested) return
    this.trafficRecordCount += 1
    this.trafficPlaintextBytes += plaintextBytes
    if (
      this.trafficRecordCount < V2_REKEY_RECORD_LIMIT &&
      this.trafficPlaintextBytes < V2_REKEY_PLAINTEXT_BYTE_LIMIT
    ) {
      return
    }
    this.trafficRekeyRequested = true
    queueMicrotask(() => {
      void this.forceRekey().catch((error) => {
        if (this.isTransientRekeyError(error)) return
        this.fail(error instanceof Error ? error : new Error(defaultLinkError))
      })
    })
  }

  private clearPendingAccept() {
    const pending: { timer: ReturnType<typeof setTimeout> } | undefined = this.pendingAccept
    if (!pending) return
    clearTimeout(pending.timer)
    this.pendingAccept = undefined
  }
}

const equalBytes = (left: Uint8Array, right: Uint8Array) =>
  left.length === right.length && left.every((value, index) => value === right[index])
