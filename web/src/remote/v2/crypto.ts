import { xchacha20poly1305 } from '@noble/ciphers/chacha.js'
import { ed25519, x25519 } from '@noble/curves/ed25519.js'
import { hkdf } from '@noble/hashes/hkdf.js'
import { hmac } from '@noble/hashes/hmac.js'
import { sha256 } from '@noble/hashes/sha2.js'

import { Direction, FrameType } from '@/generated/remote/v2/common_pb'

export const V2_PROTOCOL_MAJOR = 2
export const V2_KEY_BYTES = 32
export const V2_X25519_BYTES = 32
export const V2_MAX_PLAINTEXT_BYTES = 1 << 20
export const V2_CONTROL_CHANNEL_ID = 'v2-control'
export const V2_CONTROL_STREAM_ID = 'v2-control'
export const V2_CHANNEL_CONTROL_STREAM_ID = 'v2-channel-control'

const encoder = new TextEncoder()
const decoder = new TextDecoder('utf-8', { fatal: true })

export const utf8 = (value: string) => encoder.encode(value)
export const decodeUtf8 = (value: Uint8Array) => decoder.decode(value)

export const concatBytes = (...values: readonly Uint8Array[]) => {
  const result = new Uint8Array(values.reduce((size, value) => size + value.length, 0))
  let offset = 0
  for (const value of values) {
    result.set(value, offset)
    offset += value.length
  }
  return result
}

const uint32 = (value: number) => {
  if (!Number.isSafeInteger(value) || value < 0 || value > 0xffffffff) {
    throw new Error('remote/v2 field length is invalid.')
  }
  const bytes = new Uint8Array(4)
  new DataView(bytes.buffer).setUint32(0, value, false)
  return bytes
}

const uint64 = (value: bigint) => {
  if (value < 0n || value > 0xffffffffffffffffn) throw new Error('remote/v2 integer is invalid.')
  const bytes = new Uint8Array(8)
  new DataView(bytes.buffer).setBigUint64(0, value, false)
  return bytes
}

const field = (value: string | Uint8Array) => {
  const bytes = typeof value === 'string' ? utf8(value) : value
  return concatBytes(uint32(bytes.length), bytes)
}

const fieldList = (...values: readonly string[]) => concatBytes(...values.map(field))

const validField = (value: string) =>
  value.trim() === value && value.length > 0 && value.length <= 256 && !value.includes('\0')

export const randomBytes = (size: number) => {
  if (
    !Number.isSafeInteger(size) ||
    size < 1 ||
    size > 1 << 20 ||
    !globalThis.crypto?.getRandomValues
  ) {
    throw new Error('Secure browser randomness is unavailable.')
  }
  return crypto.getRandomValues(new Uint8Array(size))
}

export const base64UrlToBytes = (value: string) => {
  if (!/^[A-Za-z0-9_-]*$/u.test(value) || value.length % 4 === 1) {
    throw new Error('remote/v2 base64url value is invalid.')
  }
  const padded =
    value.replace(/-/gu, '+').replace(/_/gu, '/') + '='.repeat((4 - (value.length % 4)) % 4)
  const decoded = atob(padded)
  const bytes = Uint8Array.from(decoded, (item) => item.charCodeAt(0))
  if (bytesToBase64Url(bytes) !== value) throw new Error('remote/v2 base64url value is invalid.')
  return bytes
}

export const bytesToBase64Url = (value: Uint8Array) => {
  let binary = ''
  // Avoid Function.apply's argument cap for a permitted 1 MiB RPC payload.
  for (let offset = 0; offset < value.length; offset += 0x8000) {
    binary += String.fromCharCode(...value.subarray(offset, offset + 0x8000))
  }
  return btoa(binary).replace(/\+/gu, '-').replace(/\//gu, '_').replace(/=+$/u, '')
}

export interface V2HandshakeBinding {
  grantId: string
  linkId: string
  clientId: string
  deviceId: string
  relayNodeId: string
  relayCellId: string
  targetConnectionEpoch: bigint
  clientIdentityVersion: bigint
  deviceIdentityVersion: bigint
  clientEphemeralPublic: Uint8Array
  deviceEphemeralPublic: Uint8Array
  clientChallenge: Uint8Array
  deviceChallenge: Uint8Array
  expiresAtUnixMilli: bigint
}

const validBinding = (binding: V2HandshakeBinding, complete: boolean) => {
  if (
    !validField(binding.grantId) ||
    !validField(binding.linkId) ||
    !validField(binding.clientId) ||
    !validField(binding.deviceId) ||
    binding.clientId === binding.deviceId ||
    !validField(binding.relayNodeId) ||
    !validField(binding.relayCellId) ||
    binding.targetConnectionEpoch <= 0n ||
    binding.clientIdentityVersion <= 0n ||
    binding.expiresAtUnixMilli <= 0n ||
    binding.clientEphemeralPublic.length !== V2_X25519_BYTES ||
    binding.clientChallenge.length !== 32
  ) {
    return false
  }
  if (!complete) {
    return (
      binding.deviceIdentityVersion === 0n &&
      binding.deviceEphemeralPublic.length === 0 &&
      binding.deviceChallenge.length === 0
    )
  }
  return (
    binding.deviceIdentityVersion > 0n &&
    binding.deviceEphemeralPublic.length === V2_X25519_BYTES &&
    binding.deviceChallenge.length === 32
  )
}

const canonicalHandshake = (binding: V2HandshakeBinding, complete: boolean) => {
  if (!validBinding(binding, complete)) throw new Error('remote/v2 handshake binding is invalid.')
  return concatBytes(
    fieldList(
      'wenzwork-remote-v2/handshake',
      String(V2_PROTOCOL_MAJOR),
      binding.grantId,
      binding.linkId,
      binding.clientId,
      binding.deviceId,
      binding.relayNodeId,
      binding.relayCellId,
      binding.targetConnectionEpoch.toString(),
      binding.clientIdentityVersion.toString(),
      binding.deviceIdentityVersion.toString(),
    ),
    field(binding.clientEphemeralPublic),
    field(binding.deviceEphemeralPublic),
    field(binding.clientChallenge),
    field(binding.deviceChallenge),
    uint64(binding.expiresAtUnixMilli),
    field(new Uint8Array([complete ? 1 : 0])),
  )
}

export const canonicalLinkInitTranscript = (binding: V2HandshakeBinding) =>
  canonicalHandshake(binding, false)
export const canonicalLinkTranscript = (binding: V2HandshakeBinding) =>
  canonicalHandshake(binding, true)

export const signLinkInit = (privateKey: Uint8Array, binding: V2HandshakeBinding) =>
  ed25519.sign(canonicalLinkInitTranscript(binding), privateKey)

export const verifyLinkAccept = (
  publicKey: Uint8Array,
  binding: V2HandshakeBinding,
  signature: Uint8Array,
) =>
  signature.length === 64 &&
  publicKey.length === 32 &&
  ed25519.verify(signature, canonicalLinkTranscript(binding), publicKey, { zip215: false })

export const deriveRootKey = (sharedSecret: Uint8Array, binding: V2HandshakeBinding) => {
  if (sharedSecret.length !== V2_X25519_BYTES)
    throw new Error('remote/v2 shared secret is invalid.')
  return hkdf(
    sha256,
    sharedSecret,
    sha256(canonicalLinkTranscript(binding)),
    fieldList('wenzwork-remote-v2/root', binding.linkId, '1'),
    V2_KEY_BYTES,
  )
}

export const linkConfirmationMac = (rootKey: Uint8Array, binding: V2HandshakeBinding) => {
  if (rootKey.length !== V2_KEY_BYTES) throw new Error('remote/v2 root key is invalid.')
  return hmac(
    sha256,
    rootKey,
    concatBytes(utf8('wenzwork-remote-v2/link-confirm'), sha256(canonicalLinkTranscript(binding))),
  )
}

export const canonicalCarrierProof = (input: {
  grantId: string
  carrierId: string
  carrierEpoch: bigint
  challenge: Uint8Array
}) => {
  if (
    !validField(input.grantId) ||
    !validField(input.carrierId) ||
    input.carrierEpoch <= 0n ||
    input.challenge.length !== 32
  ) {
    throw new Error('remote/v2 Carrier proof is invalid.')
  }
  return concatBytes(
    fieldList('wenzwork-remote-v2/carrier-proof', input.grantId, input.carrierId),
    uint64(input.carrierEpoch),
    field(input.challenge),
  )
}

export const signCarrierProof = (
  privateKey: Uint8Array,
  input: { grantId: string; carrierId: string; carrierEpoch: bigint; challenge: Uint8Array },
) => ed25519.sign(canonicalCarrierProof(input), privateKey)

export interface V2RecordMetadata {
  linkId: string
  channelId: string
  streamId: string
  keyId: bigint
  direction: Direction
  frameType: FrameType
  streamSequence: bigint
}

const validRecordMetadata = (metadata: V2RecordMetadata) =>
  validField(metadata.linkId) &&
  validField(metadata.channelId) &&
  validField(metadata.streamId) &&
  metadata.keyId > 0n &&
  metadata.streamSequence > 0n &&
  (metadata.direction === Direction.CLIENT_TO_DEVICE ||
    metadata.direction === Direction.DEVICE_TO_CLIENT) &&
  metadata.frameType >= FrameType.CHANNEL_OPEN &&
  metadata.frameType <= FrameType.LINK_LEASE_RENEWED

export const canonicalRecordMetadata = (metadata: V2RecordMetadata) => {
  if (!validRecordMetadata(metadata))
    throw new Error('remote/v2 encrypted record metadata is invalid.')
  return concatBytes(
    fieldList(
      'wenzwork-remote-v2/record',
      metadata.linkId,
      metadata.channelId,
      metadata.streamId,
      metadata.keyId.toString(),
      String(metadata.direction),
      String(metadata.frameType),
    ),
    uint64(metadata.streamSequence),
  )
}

export const deriveControlKey = (
  rootKey: Uint8Array,
  linkId: string,
  keyId: bigint,
  direction: Direction,
) => deriveKey(rootKey, linkId, keyId, direction, 'control', '', '')

export const deriveChannelKey = (
  rootKey: Uint8Array,
  linkId: string,
  keyId: bigint,
  direction: Direction,
  channelId: string,
) => deriveKey(rootKey, linkId, keyId, direction, 'channel', channelId, '')

export const deriveStreamKey = (
  rootKey: Uint8Array,
  linkId: string,
  keyId: bigint,
  direction: Direction,
  channelId: string,
  streamId: string,
) => deriveKey(rootKey, linkId, keyId, direction, 'stream', channelId, streamId)

const deriveKey = (
  rootKey: Uint8Array,
  linkId: string,
  keyId: bigint,
  direction: Direction,
  layer: 'control' | 'channel' | 'stream',
  channelId: string,
  streamId: string,
) => {
  if (
    rootKey.length !== V2_KEY_BYTES ||
    !validField(linkId) ||
    keyId <= 0n ||
    (direction !== Direction.CLIENT_TO_DEVICE && direction !== Direction.DEVICE_TO_CLIENT) ||
    (layer === 'channel' && !validField(channelId)) ||
    (layer === 'stream' && (!validField(channelId) || !validField(streamId)))
  ) {
    throw new Error('remote/v2 key derivation metadata is invalid.')
  }
  return hkdf(
    sha256,
    rootKey,
    undefined,
    fieldList(
      'wenzwork-remote-v2/key',
      layer,
      linkId,
      keyId.toString(),
      String(direction),
      channelId,
      streamId,
    ),
    V2_KEY_BYTES,
  )
}

export const recordNonce = (streamKey: Uint8Array, metadata: V2RecordMetadata) => {
  if (streamKey.length !== V2_KEY_BYTES || !validRecordMetadata(metadata)) {
    throw new Error('remote/v2 encrypted record metadata is invalid.')
  }
  const nonceKey = hkdf(sha256, streamKey, undefined, utf8('wenzwork-remote-v2/nonce-key'), 32)
  const withoutSequence = { ...metadata, streamSequence: 0n }
  // canonicalRecordMetadata requires a non-zero sequence, while Go's nonce
  // input intentionally serializes a zero sequence. Recreate only that final
  // uint64 with the same field-prefix encoding.
  const prefix = concatBytes(
    fieldList(
      'wenzwork-remote-v2/record',
      metadata.linkId,
      metadata.channelId,
      metadata.streamId,
      metadata.keyId.toString(),
      String(metadata.direction),
      String(metadata.frameType),
    ),
    uint64(withoutSequence.streamSequence),
  )
  const digest = hmac(sha256, nonceKey, prefix)
  const nonce = new Uint8Array(24)
  nonce.set(digest.subarray(0, 16), 0)
  nonce.set(uint64(metadata.streamSequence), 16)
  nonceKey.fill(0)
  return nonce
}

export const sealRecord = (
  streamKey: Uint8Array,
  plaintext: Uint8Array,
  metadata: V2RecordMetadata,
) => {
  if (plaintext.length === 0 || plaintext.length > V2_MAX_PLAINTEXT_BYTES) {
    throw new Error('remote/v2 plaintext is invalid.')
  }
  const nonce = recordNonce(streamKey, metadata)
  return xchacha20poly1305(streamKey, nonce, canonicalRecordMetadata(metadata)).encrypt(plaintext)
}

export const openRecord = (
  streamKey: Uint8Array,
  ciphertext: Uint8Array,
  metadata: V2RecordMetadata,
) => {
  if (ciphertext.length <= 16 || ciphertext.length > V2_MAX_PLAINTEXT_BYTES + 16) {
    throw new Error('remote/v2 ciphertext is invalid.')
  }
  const nonce = recordNonce(streamKey, metadata)
  try {
    const plaintext = xchacha20poly1305(
      streamKey,
      nonce,
      canonicalRecordMetadata(metadata),
    ).decrypt(ciphertext)
    if (plaintext.length === 0 || plaintext.length > V2_MAX_PLAINTEXT_BYTES) {
      throw new Error('remote/v2 plaintext is invalid.')
    }
    return plaintext
  } catch {
    throw new Error('remote/v2 encrypted record authentication failed.')
  }
}

export const recordKeyFor = (rootKey: Uint8Array, metadata: V2RecordMetadata) => {
  switch (metadata.frameType) {
    case FrameType.LINK_CONFIRM:
    case FrameType.LINK_READY:
    case FrameType.LINK_LEASE_RENEW:
    case FrameType.LINK_LEASE_RENEWED:
    case FrameType.REKEY_INIT:
    case FrameType.REKEY_ACK:
    case FrameType.REKEY_COMMIT:
      if (
        metadata.channelId !== V2_CONTROL_CHANNEL_ID ||
        metadata.streamId !== V2_CONTROL_STREAM_ID
      ) {
        throw new Error('remote/v2 control record is invalid.')
      }
      return deriveControlKey(rootKey, metadata.linkId, metadata.keyId, metadata.direction)
    case FrameType.CHANNEL_OPEN:
    case FrameType.CHANNEL_ACCEPT:
    case FrameType.CHANNEL_CLOSE:
    case FrameType.STREAM_OPEN:
    case FrameType.STREAM_ACK:
    case FrameType.STREAM_CLOSE:
      if (
        metadata.channelId === V2_CONTROL_CHANNEL_ID ||
        metadata.streamId !== V2_CHANNEL_CONTROL_STREAM_ID
      ) {
        throw new Error('remote/v2 channel-control record is invalid.')
      }
      return deriveChannelKey(
        rootKey,
        metadata.linkId,
        metadata.keyId,
        metadata.direction,
        metadata.channelId,
      )
    default:
      if (
        metadata.channelId === V2_CONTROL_CHANNEL_ID ||
        metadata.streamId === V2_CONTROL_STREAM_ID ||
        metadata.streamId === V2_CHANNEL_CONTROL_STREAM_ID
      ) {
        throw new Error('remote/v2 stream record is invalid.')
      }
      return deriveStreamKey(
        rootKey,
        metadata.linkId,
        metadata.keyId,
        metadata.direction,
        metadata.channelId,
        metadata.streamId,
      )
  }
}

export class V2RecordSequencer {
  private readonly outgoing = new Map<string, bigint>()
  private readonly incoming = new Map<string, { maximum: bigint; seen: Set<bigint> }>()

  next(keyId: bigint, direction: Direction, streamId: string) {
    const key = `${keyId}:${direction}:${streamId}`
    const sequence = (this.outgoing.get(key) ?? 0n) + 1n
    this.outgoing.set(key, sequence)
    return sequence
  }

  accept(metadata: V2RecordMetadata, width = 4096n) {
    const key = `${metadata.keyId}:${metadata.direction}:${metadata.streamId}`
    const state = this.incoming.get(key) ?? { maximum: 0n, seen: new Set<bigint>() }
    if (
      state.seen.has(metadata.streamSequence) ||
      (state.maximum > width && metadata.streamSequence <= state.maximum - width)
    ) {
      return false
    }
    state.seen.add(metadata.streamSequence)
    if (metadata.streamSequence > state.maximum) state.maximum = metadata.streamSequence
    const minimum = state.maximum > width ? state.maximum - width : 0n
    for (const sequence of state.seen) {
      if (sequence <= minimum) state.seen.delete(sequence)
    }
    this.incoming.set(key, state)
    return true
  }
}

export const deriveRekeyRoot = (
  oldRoot: Uint8Array,
  sharedSecret: Uint8Array,
  linkId: string,
  rekeyId: string,
  keyId: bigint,
) => {
  if (
    oldRoot.length !== V2_KEY_BYTES ||
    sharedSecret.length !== V2_X25519_BYTES ||
    !validField(linkId) ||
    !validField(rekeyId) ||
    keyId < 2n
  ) {
    throw new Error('remote/v2 rekey data is invalid.')
  }
  return hkdf(
    sha256,
    sharedSecret,
    sha256(oldRoot),
    fieldList('wenzwork-remote-v2/rekey-root', linkId, rekeyId, keyId.toString()),
    V2_KEY_BYTES,
  )
}

export const canonicalRekey = (
  kind: 'init' | 'ack',
  linkId: string,
  rekeyId: string,
  keyId: bigint,
  ephemeralPublic: Uint8Array,
) => {
  if (
    !validField(linkId) ||
    !validField(rekeyId) ||
    keyId < 2n ||
    ephemeralPublic.length !== V2_X25519_BYTES
  ) {
    throw new Error('remote/v2 rekey data is invalid.')
  }
  return concatBytes(
    fieldList('wenzwork-remote-v2/rekey', kind, linkId, rekeyId, keyId.toString()),
    field(ephemeralPublic),
  )
}

export const createX25519Ephemeral = () => {
  const privateKey = x25519.utils.randomSecretKey()
  return { privateKey, publicKey: x25519.getPublicKey(privateKey) }
}

export const x25519SharedSecret = (privateKey: Uint8Array, peerPublicKey: Uint8Array) =>
  x25519.getSharedSecret(privateKey, peerPublicKey)

export const zero = (value: Uint8Array | undefined) => value?.fill(0)
