import { sha256 } from '@noble/hashes/sha2.js'

export type RemoteProtocolStage =
  | 'hostSession'
  | 'relayHandshake'
  | 'relayEnvelope'
  | 'peerBinding'
  | 'peerCiphertext'
  | 'rpcEnvelope'
  | 'rpcJson'
  | 'rpcModel'

export type RemoteProtocolFaultLevel = 'operation' | 'channel' | 'session' | 'connection'

export type RemoteProtocolDiagnosticContext = {
  code?: string
  epoch?: number
  scope?: string
  payloadBytes?: number
  requestId?: string
  sessionId?: string
}

export type RemoteProtocolDiagnostic = {
  occurredAt: string
  stage: RemoteProtocolStage
  reason: string
  faultLevel: RemoteProtocolFaultLevel
  recoverable: boolean
  epoch: number
  scope: string
  payloadSizeBucket: string
  requestHash?: string
  sessionHash?: string
  rootFailureId: string
}

const MAXIMUM_PROTOCOL_DIAGNOSTICS = 128
const textEncoder = new TextEncoder()
const diagnosticSalt = (() => {
  const salt = new Uint8Array(32)
  if (globalThis.crypto?.getRandomValues) return globalThis.crypto.getRandomValues(salt)
  return sha256(textEncoder.encode('wenzwork-web-protocol-diagnostics-fallback'))
})()
const protocolDiagnostics: RemoteProtocolDiagnostic[] = []

const hex = (value: Uint8Array) =>
  Array.from(value, (byte) => byte.toString(16).padStart(2, '0')).join('')

const diagnosticHash = (kind: string, value: string) => {
  if (!value) return undefined
  const encoded = textEncoder.encode(`${kind}\0${value}`)
  const input = new Uint8Array(diagnosticSalt.length + encoded.length)
  input.set(diagnosticSalt)
  input.set(encoded, diagnosticSalt.length)
  return hex(sha256(input).slice(0, 12))
}

const safeScope = (scope = '') =>
  new Set([
    'remote.peer.query',
    'remote.peer.file.send',
    'remote.peer.file.receive',
    'remote.peer.terminal',
    'remote.peer.terminal.interactive',
    'remote.peer.task.control',
    'remote.peer.ai.config',
    'remote.peer.ai.chat',
    'remote.peer.ai.tools',
    'remote.peer.events',
  ]).has(scope)
    ? scope
    : 'unknown'

const payloadSizeBucket = (bytes = 0) => {
  if (!Number.isSafeInteger(bytes) || bytes <= 0) return 'empty'
  if (bytes <= 48 * 1024) return 'at_or_below_48KiB'
  if (bytes <= 56 * 1024) return '48_to_56KiB'
  if (bytes <= 60 * 1024) return '56_to_60KiB'
  return 'over_60KiB'
}

const protocolCategory = (reason: string) => {
  if (
    reason === 'authorization_required' ||
    reason === 'incompatible_agent' ||
    reason === 'session_unavailable' ||
    reason === 'transport_unavailable'
  )
    return reason
  if (
    reason === 'protocol_binding_invalid' ||
    reason === 'peer_binding_invalid' ||
    reason === 'peer_epoch_stale'
  )
    return 'protocol_binding_invalid'
  if (
    reason === 'rpc_payload_too_large' ||
    reason === 'rpc_json_too_large' ||
    reason === 'rpc_envelope_too_large'
  )
    return 'rpc_payload_too_large'
  if (reason === 'rpc_response_invalid' || reason === 'rpc_model_invalid')
    return 'rpc_response_invalid'
  return 'protocol_invalid'
}

const recordProtocolDiagnostic = (
  failure: RemoteProtocolFailure,
  context: RemoteProtocolDiagnosticContext,
) => {
  const epoch =
    Number.isSafeInteger(context.epoch) && (context.epoch ?? 0) >= 0 ? context.epoch! : 0
  const scope = safeScope(context.scope)
  const requestHash = diagnosticHash('request', context.requestId?.trim() ?? '')
  const sessionHash = diagnosticHash('session', context.sessionId?.trim() ?? '')
  const root = [
    failure.stage,
    failure.reasonCode,
    failure.faultLevel,
    epoch,
    scope,
    requestHash,
    sessionHash,
  ]
    .filter((value) => value !== undefined)
    .join('|')
  protocolDiagnostics.push({
    occurredAt: new Date().toISOString(),
    stage: failure.stage,
    reason: /^[a-z][a-z0-9_]{0,79}$/.test(failure.reasonCode)
      ? failure.reasonCode
      : 'protocol_invalid',
    faultLevel: failure.faultLevel,
    recoverable: failure.recoverable,
    epoch,
    scope,
    payloadSizeBucket: payloadSizeBucket(context.payloadBytes),
    requestHash,
    sessionHash,
    rootFailureId: diagnosticHash('root', root)!,
  })
  if (protocolDiagnostics.length > MAXIMUM_PROTOCOL_DIAGNOSTICS) {
    protocolDiagnostics.splice(0, protocolDiagnostics.length - MAXIMUM_PROTOCOL_DIAGNOSTICS)
  }
}

export const remoteProtocolDiagnosticSnapshot = () =>
  protocolDiagnostics.map((diagnostic) => ({ ...diagnostic }))

export const clearRemoteProtocolDiagnostics = () => {
  protocolDiagnostics.splice(0)
}

export class RemoteProtocolFailure extends Error {
  readonly name = 'RemoteProtocolFailure'
  readonly stage: RemoteProtocolStage
  readonly reasonCode: string
  readonly code: string
  readonly faultLevel: RemoteProtocolFaultLevel
  readonly recoverable: boolean
  readonly rootFailureId: string

  constructor(
    stage: RemoteProtocolStage,
    reasonCode: string,
    faultLevel: RemoteProtocolFaultLevel,
    recoverable: boolean,
    safeMessage: string,
    diagnosticContext: RemoteProtocolDiagnosticContext = {},
  ) {
    super(safeMessage)
    this.stage = stage
    this.reasonCode = reasonCode
    this.code = diagnosticContext.code ?? protocolCategory(reasonCode)
    this.faultLevel = faultLevel
    this.recoverable = recoverable
    this.rootFailureId = diagnosticHash(
      'root',
      [
        stage,
        reasonCode,
        faultLevel,
        diagnosticContext.epoch ?? 0,
        safeScope(diagnosticContext.scope),
      ].join('|'),
    )!
    recordProtocolDiagnostic(this, diagnosticContext)
  }

  get incompatible() {
    return this.code === 'incompatible_agent'
  }
}
