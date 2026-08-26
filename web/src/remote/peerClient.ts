import type { Ref } from 'vue'

import type { RemoteScope } from '@/api/remote'
import type { RemoteRPCClient } from '@/remote/rpcTypes'

import { REMOTE_COLLABORATION_EVENT_KINDS } from './eventContract'
import { createRemotePeerClientV2 } from './v2/client'

/**
 * Public remote-workspace contract.
 *
 * This facade intentionally contains no Carrier frame or RPC codec. The only
 * production implementation is remote/v2, which uses generated protobuf
 * messages under `api/remote/v2`.
 */
export type RemotePeerScope = Extract<
  RemoteScope,
  | 'remote.peer.query'
  | 'remote.peer.ai.config'
  | 'remote.peer.ai.chat'
  | 'remote.peer.ai.tools'
  | 'remote.peer.terminal'
  | 'remote.peer.terminal.interactive'
  | 'remote.peer.file.send'
  | 'remote.peer.file.receive'
  | 'remote.peer.task.control'
  | 'remote.peer.events'
>

export interface RemotePeerRpcEvent {
  eventId: string
  eventKind: number
  requestId: string
  sequence: number
  highWatermark: number
  occurredAt?: Date
  payload: unknown
}

const MAX_RPC_JSON_BYTES = 56 * 1024
const RPC_TIMEOUT_MS = 30_000
const STREAM_TIMEOUT_MS = 15 * 60_000

// This is the encrypted Agent capability document version, distinct from the
// Carrier protocol major (which is fixed at remote/v2 = 2).
export const REMOTE_AGENT_CAPABILITY_PROTOCOL_VERSION = 1

const projectScopes = new Set<RemotePeerScope>([
  'remote.peer.ai.chat',
  'remote.peer.ai.tools',
  'remote.peer.terminal',
  'remote.peer.terminal.interactive',
  'remote.peer.file.send',
  'remote.peer.file.receive',
  'remote.peer.task.control',
  'remote.peer.events',
])

const requiresProject = (scope: RemoteScope): scope is RemotePeerScope =>
  projectScopes.has(scope as RemotePeerScope)

export const remotePeerRequestTimeoutFor = (_method: string, streaming: boolean) => {
  if (streaming) return STREAM_TIMEOUT_MS
  return RPC_TIMEOUT_MS
}

export interface RemoteAgentCapabilities {
  agentBuildId?: string
  connectionEpoch?: number
  capabilityVersion?: number
  protocolMinimum: number
  protocolMaximum: number
  featureVersions: Readonly<Record<string, number>>
  features: Readonly<Record<string, boolean>>
  operatingSystem: string
  architecture: string
  shells: readonly string[]
  taskRunners: readonly string[]
  resourceLimits: Readonly<Record<string, number>>
  remoteV2Resources?: Readonly<Record<string, number>>
  eventContractVersion?: number
  acceptedEventKinds?: readonly string[]
}

const capabilityRecord = (value: unknown, label: string): Record<string, unknown> => {
  if (!value || typeof value !== 'object' || Array.isArray(value)) {
    throw new Error(`Agent ${label} is invalid.`)
  }
  return value as Record<string, unknown>
}

const capabilityInteger = (value: unknown, label: string) => {
  if (!Number.isSafeInteger(value) || (value as number) < 0) {
    throw new Error(`Agent ${label} is invalid.`)
  }
  return value as number
}

const capabilityIntegerRecord = (value: unknown, label: string) =>
  Object.fromEntries(
    Object.entries(capabilityRecord(value, label)).map(([key, item]) => [
      key,
      capabilityInteger(item, `${label}.${key}`),
    ]),
  )

const capabilityBooleanRecord = (value: unknown, label: string) => {
  const entries = Object.entries(capabilityRecord(value, label))
  if (entries.some(([, item]) => typeof item !== 'boolean')) {
    throw new Error(`Agent ${label} is invalid.`)
  }
  return Object.fromEntries(entries) as Record<string, boolean>
}

const capabilityStringList = (value: unknown, label: string) => {
  if (
    !Array.isArray(value) ||
    value.some((item) => typeof item !== 'string' || item.length === 0) ||
    new Set(value).size !== value.length
  ) {
    throw new Error(`Agent ${label} is invalid.`)
  }
  return value as string[]
}

export const parseAgentCapabilities = (value: unknown): RemoteAgentCapabilities => {
  const root = capabilityRecord(value, 'capability response')
  const protocol = capabilityRecord(root.protocol, 'protocol range')
  const protocolMinimum = capabilityInteger(protocol.minimum, 'protocol minimum')
  const protocolMaximum = capabilityInteger(protocol.maximum, 'protocol maximum')
  if (protocolMinimum === 0 || protocolMaximum < protocolMinimum) {
    throw new Error('Agent protocol range is invalid.')
  }
  const platform = capabilityRecord(root.platform, 'platform')
  const eventContract = root.eventContract
    ? capabilityRecord(root.eventContract, 'event contract')
    : undefined
  if (
    typeof platform.os !== 'string' ||
    platform.os.length === 0 ||
    typeof platform.arch !== 'string' ||
    platform.arch.length === 0
  ) {
    throw new Error('Agent platform description is invalid.')
  }
  return {
    agentBuildId:
      typeof root.agentBuildId === 'string' && root.agentBuildId.length > 0
        ? root.agentBuildId
        : '',
    connectionEpoch:
      root.connectionEpoch === undefined
        ? 0
        : capabilityInteger(root.connectionEpoch, 'connection epoch'),
    capabilityVersion:
      root.capabilityVersion === undefined
        ? 0
        : capabilityInteger(root.capabilityVersion, 'capability version'),
    protocolMinimum,
    protocolMaximum,
    featureVersions: capabilityIntegerRecord(root.featureVersions, 'feature versions'),
    features: capabilityBooleanRecord(root.features, 'feature flags'),
    operatingSystem: platform.os,
    architecture: platform.arch,
    shells: capabilityStringList(root.shells, 'shells'),
    taskRunners: capabilityStringList(root.taskRunners, 'task runners'),
    resourceLimits: capabilityIntegerRecord(root.resourceLimits, 'resource limits'),
    remoteV2Resources:
      root.remoteV2Resources === undefined
        ? {}
        : capabilityIntegerRecord(root.remoteV2Resources, 'remote/v2 resources'),
    eventContractVersion: eventContract
      ? capabilityInteger(eventContract.version, 'event contract version')
      : 0,
    acceptedEventKinds: eventContract
      ? capabilityStringList(eventContract.kinds, 'event contract kinds')
      : [],
  }
}

export const agentCapabilityCacheVersion = (capabilities: RemoteAgentCapabilities) => {
  const versions = Object.entries(capabilities.featureVersions).sort(([left], [right]) =>
    left.localeCompare(right),
  )
  const flags = Object.entries(capabilities.features).sort(([left], [right]) =>
    left.localeCompare(right),
  )
  return [
    `p${capabilities.protocolMinimum}-${capabilities.protocolMaximum}`,
    `c:${capabilities.capabilityVersion ?? 0}`,
    `a:${capabilities.agentBuildId ?? ''}:${capabilities.connectionEpoch ?? 0}`,
    `v:${versions.map(([key, value]) => `${key}=${value}`).join(',')}`,
    `f:${flags.map(([key, value]) => `${key}=${value ? 1 : 0}`).join(',')}`,
    `s:${[...capabilities.shells].sort().join(',')}`,
    `e:${capabilities.eventContractVersion ?? 0}:${[...(capabilities.acceptedEventKinds ?? [])].sort().join(',')}`,
    `r:${[...capabilities.taskRunners].sort().join(',')}`,
  ].join('|')
}

/** Maps one business operation to its least-privileged v2 Channel scope. */
export const peerScopeForMethod = (
  method: string,
  input?: Readonly<Record<string, unknown>>,
): RemotePeerScope => {
  if (method.startsWith('ai.config.') || method.startsWith('agent.environment.')) {
    return 'remote.peer.ai.config'
  }
  if (method === 'event.subscribe') return 'remote.peer.events'
  if (method === 'conversation.approval.respond' || method === 'conversation.question.answer') {
    return 'remote.peer.ai.tools'
  }
  // Starting or queuing a turn with explicit workspace-tool intent must use
  // the tools Channel. The Device rejects the same flag on a chat-only
  // Channel, preventing a valid JSON field from being silently ignored.
  if (
    input?.enableWorkspaceTools === true &&
    (method === 'conversation.send' ||
      method === 'conversation.regenerate' ||
      method.startsWith('conversation.goal.') ||
      method === 'conversation.subagent.message')
  ) {
    return 'remote.peer.ai.tools'
  }
  if (method.startsWith('conversation.')) return 'remote.peer.ai.chat'
  if (
    method === 'project.create' ||
    method === 'project.directory.list' ||
    method === 'project.remove'
  ) {
    return 'remote.peer.query'
  }
  if (method === 'terminal.execute') return 'remote.peer.terminal'
  if (method.startsWith('terminal.')) return 'remote.peer.terminal.interactive'
  if (method.startsWith('task.') || method.startsWith('workflow.')) {
    return 'remote.peer.task.control'
  }
  if (
    method.startsWith('file.upload.') ||
    [
      'file.write-text',
      'file.create-text',
      'file.mkdir',
      'file.rename',
      'file.move',
      'file.delete.prepare',
      'file.delete',
    ].includes(method)
  ) {
    return 'remote.peer.file.send'
  }
  if (method.startsWith('file.')) return 'remote.peer.file.receive'
  return 'remote.peer.query'
}

export const agentSupportsProjectMethod = (
  capabilities: RemoteAgentCapabilities,
  method: string,
) => {
  if (
    capabilities.protocolMinimum > 1 ||
    capabilities.protocolMaximum < 1 ||
    capabilities.features['project.v2'] !== true ||
    (capabilities.featureVersions.projects ?? 0) < 2
  ) {
    return false
  }
  if (method === 'event.subscribe') {
    return (
      capabilities.features['events.v1'] === true && (capabilities.featureVersions.events ?? 0) >= 1
    )
  }
  if (method.startsWith('file.')) {
    return (
      capabilities.features['file.v2'] === true && (capabilities.featureVersions.files ?? 0) >= 2
    )
  }
  if (method === 'project.create') {
    return (
      capabilities.features['project.remoteCreate'] === true &&
      capabilities.features['file.v2'] === true &&
      (capabilities.featureVersions.files ?? 0) >= 2
    )
  }
  if (method === 'project.directory.list') {
    return (
      capabilities.features['project.remoteRoots'] === true &&
      capabilities.features['project.remoteCreate'] === true &&
      capabilities.features['file.v2'] === true &&
      (capabilities.featureVersions.files ?? 0) >= 2
    )
  }
  if (method === 'project.remove') {
    return (
      capabilities.features['project.remoteRemove'] === true &&
      (capabilities.featureVersions.projects ?? 0) >= 3
    )
  }
  if (method.startsWith('conversation.inbox.')) {
    return (
      capabilities.features['ai.agentLoop'] === true &&
      capabilities.features['ai.durableInbox'] === true &&
      (capabilities.featureVersions.ai ?? 0) >= 6
    )
  }
  if (method.startsWith('conversation.goal.')) {
    return capabilities.features['ai.goal'] === true && (capabilities.featureVersions.ai ?? 0) >= 8
  }
  if (
    method === 'conversation.plan.set' ||
    method === 'conversation.subagents.list' ||
    method === 'conversation.subagent.message' ||
    method === 'conversation.subagent.interrupt'
  ) {
    return (
      capabilities.features['ai.planMode'] === true &&
      capabilities.features['ai.todo'] === true &&
      capabilities.features['ai.subagents'] === true &&
      (capabilities.featureVersions.ai ?? 0) >= 7
    )
  }
  if (method.startsWith('conversation.') || method.startsWith('chat.')) {
    return (capabilities.featureVersions.ai ?? 0) >= 1
  }
  if (method === 'terminal.execute') return (capabilities.featureVersions.terminal ?? 0) >= 1
  if (method.startsWith('terminal.')) {
    return (
      capabilities.features['terminal.interactive'] === true &&
      capabilities.features['terminal.attachLongPoll'] === true &&
      (capabilities.featureVersions.terminal ?? 0) >= 3
    )
  }
  if (method.startsWith('task.')) {
    return (
      capabilities.features['tasks.v2'] === true && (capabilities.featureVersions.tasks ?? 0) >= 2
    )
  }
  if (method.startsWith('workflow.')) {
    return (
      capabilities.features['tasks.v2'] === true &&
      capabilities.features['workflow.v2'] === true &&
      (capabilities.featureVersions.tasks ?? 0) >= 2 &&
      (capabilities.featureVersions.workflows ?? 0) >= 2
    )
  }
  return false
}

export const agentSupportsTerminalDuplexStream = (capabilities: RemoteAgentCapabilities) =>
  capabilities.features['terminal.interactive'] === true &&
  capabilities.features['terminal.duplexStream'] === true &&
  capabilities.features['terminal.duplexKeepAlive'] === true &&
  (capabilities.featureVersions.terminal ?? 0) >= 4

export const agentSupportsCollaborationEvents = (capabilities: RemoteAgentCapabilities) =>
  capabilities.features['events.collaboration.v1'] === true &&
  (capabilities.eventContractVersion ?? 0) >= 1 &&
  REMOTE_COLLABORATION_EVENT_KINDS.every((kind) => capabilities.acceptedEventKinds?.includes(kind))

export const agentSupportsTaskPayloadV2 = (capabilities: RemoteAgentCapabilities) =>
  capabilities.features['taskPayload.v2'] === true &&
  (capabilities.resourceLimits.taskPayloadChunkBytes ?? 0) > 0 &&
  (capabilities.resourceLimits.taskPayloadTotalBytes ?? 0) >
    (capabilities.resourceLimits.rpcJsonBytes ?? MAX_RPC_JSON_BYTES)

export const agentSupportsTaskLogFiles = (capabilities: RemoteAgentCapabilities) =>
  (capabilities.featureVersions.taskLogs ?? 0) >= 1 &&
  capabilities.features['taskLogs.fileSeek'] === true &&
  (capabilities.resourceLimits.taskLogSeekBytes ?? 0) >= 32 * 1024

/** The sole public browser transport factory: no v1 or fallback path exists. */
export const createRemotePeerClient = (targetDeviceId: Readonly<Ref<string>>): RemoteRPCClient =>
  createRemotePeerClientV2(targetDeviceId, {
    scopeForMethod: peerScopeForMethod,
    parseCapabilities: parseAgentCapabilities,
    timeoutFor: remotePeerRequestTimeoutFor,
    projectRequired: requiresProject,
  })
