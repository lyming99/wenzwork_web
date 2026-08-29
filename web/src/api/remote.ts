import { apiClient, apiV2Client } from './client'

export type RemoteScope =
  | 'remote.connect'
  | 'remote.project.read'
  | 'remote.project.sync'
  | 'remote.task.read'
  | 'remote.task.write'
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

export interface RemoteDevice {
  id: string
  installationDeviceId: string
  deviceName: string
  platform: 'windows' | 'macos' | 'linux'
  agentVersion: string
  status: 'pending_approval' | 'active' | 'disabled' | 'revoked' | 'quarantined'
  presence: 'online' | 'offline' | 'degraded'
  capabilities: string[]
  scopes: RemoteScope[]
  grantVersion: number
  keyVersion?: number
  identityPublicKey?: string
  publicKeyThumbprint?: string
  lastSeenAt: string | null
  lastSyncAt: string | null
  remoteEnabledAt: string | null
  connectionMode: 'relay' | 'direct'
  directModeEnabled: boolean
  directAvailable: boolean
  directTlsEnabled: boolean
  directIp: string | null
  directPort: number | null
}

export interface CursorPage<T> {
  items: T[]
  nextCursor: string | null
  highWatermark?: number
  resetRequired?: boolean
}

export interface RemoteDevicePage extends CursorPage<RemoteDevice> {
  observedAt: string
}

export interface RemoteProject {
  id: string
  displayName: string
  revision: number
  capabilities: string[]
  observedAt: string
  state: 'available' | 'removed' | 'unavailable'
}

export interface RemoteProjectPage extends CursorPage<RemoteProject> {
  observedAt: string
  deviceOnline: boolean
  stale: boolean
  highWatermark?: number
  resetRequired?: boolean
}

export type RemoteTaskStatus =
  | 'queued'
  | 'dispatched'
  | 'accepted'
  | 'running'
  | 'cancel_requested'
  | 'cancelled'
  | 'succeeded'
  | 'failed'
  | 'rejected'
  | 'expired'
  | 'timed_out'

export interface RemoteTask {
  id: string
  deviceId: string
  projectId: string | null
  taskType: string
  title: string
  status: RemoteTaskStatus
  revision: number
  createdAt: string
  startedAt: string | null
  finishedAt: string | null
  resultCode: string | null
}

export interface RemoteOperation {
  id: string
  status: 'pending' | 'running' | 'succeeded' | 'failed' | 'cancelled'
  operationType?: string
  errorCode?: string | null
  errorMessage?: string | null
}

export interface RemoteAccessResult {
  deviceId: string
  status: string
  scopes: RemoteScope[]
  grantVersion: number
  replayed: boolean
}

export interface DeviceAccessKey {
  id: string
  label: string
  key?: string
  keyPrefix: string
  scopes: RemoteScope[]
  status: 'active' | 'revoked' | 'expired'
  expiresAt: string | null
  lastUsedAt: string | null
  createdAt: string
}

export interface BrowserController {
  id: string
  identityAlgorithm: 'Ed25519'
  identityPublicKey: string
  publicKeyThumbprint: string
  keyVersion: number
  grantVersion: number
  scopes: RemoteScope[]
  status: 'active' | 'revoked'
  lastUsedAt: string | null
  createdAt: string
  updatedAt: string
}

/** A reusable, proof-bound credential for a device-scoped remote/v2 Link. */
export interface RemoteDeviceLink {
  /** Non-bearer handle used to revoke the credential. */
  grantId: string
  deviceConnectionGrant: string
  expiresAt: string
  maximumLifetimeSeconds: number
  connectionMode: 'relay' | 'direct'
  connectionUrl: string
  /** @deprecated Use connectionUrl. */
  relayUrl: string
  relayNodeId: string
  relayCellId: string
  targetConnectionEpoch: number
  deviceIdentityAlgorithm: 'Ed25519'
  deviceIdentityPublicKey: string
  deviceKeyThumbprint: string
  deviceIdentityKeyVersion: number
}

let idempotencySequence = 0
export const createRemoteIdempotencyKey = (): string =>
  globalThis.crypto?.randomUUID?.() ?? `remote-web-${Date.now()}-${++idempotencySequence}`

const idempotencyHeaders = (idempotencyKey = createRemoteIdempotencyKey()) => ({
  'Idempotency-Key': idempotencyKey,
})

export const listRemoteDevices = async (cursor?: string, limit = 30) =>
  (
    await apiClient.get<RemoteDevicePage>('/remote/devices', {
      params: { cursor: cursor || undefined, limit },
    })
  ).data

export const getRemoteDevice = async (deviceId: string) =>
  (await apiClient.get<{ device: RemoteDevice }>(`/remote/devices/${encodeURIComponent(deviceId)}`))
    .data.device

export const updateRemoteDevice = async (
  deviceId: string,
  deviceName: string,
  directModeEnabled?: boolean,
) =>
  (
    await apiClient.patch<{ device: RemoteDevice }>(
      `/remote/devices/${encodeURIComponent(deviceId)}`,
      { deviceName, directModeEnabled },
    )
  ).data.device

export const deleteRemoteDevice = async (deviceId: string) => {
  await apiClient.delete(`/remote/devices/${encodeURIComponent(deviceId)}`)
}

export const enableRemoteDeviceAccess = async (deviceId: string, scopes: RemoteScope[]) =>
  (
    await apiClient.post<{ access: RemoteAccessResult }>(
      `/remote/devices/${encodeURIComponent(deviceId)}/remote-access`,
      { scopes, confirmation: 'enable_remote_access' },
      { headers: idempotencyHeaders() },
    )
  ).data.access

export const revokeRemoteDeviceAccess = async (deviceId: string) =>
  (
    await apiClient.delete<{ access: RemoteAccessResult }>(
      `/remote/devices/${encodeURIComponent(deviceId)}/remote-access`,
      { headers: idempotencyHeaders() },
    )
  ).data.access

export const listRemoteProjects = async (deviceId: string, cursor?: string, limit = 30) =>
  (
    await apiClient.get<RemoteProjectPage>(
      `/remote/devices/${encodeURIComponent(deviceId)}/projects`,
      { params: { cursor: cursor || undefined, limit } },
    )
  ).data

export const syncRemoteProjects = async (deviceId: string) =>
  (
    await apiClient.post<{ operation: RemoteOperation }>(
      `/remote/devices/${encodeURIComponent(deviceId)}/project-syncs`,
      undefined,
      { headers: idempotencyHeaders() },
    )
  ).data.operation

export const listRemoteTasks = async (
  deviceId: string,
  cursor?: string,
  limit = 30,
  afterRevision?: number,
) =>
  (
    await apiClient.get<CursorPage<RemoteTask>>(
      `/remote/devices/${encodeURIComponent(deviceId)}/tasks`,
      { params: { cursor: cursor || undefined, limit, afterRevision } },
    )
  ).data

export const getRemoteTask = async (taskId: string) =>
  (await apiClient.get<{ task: RemoteTask }>(`/remote/tasks/${encodeURIComponent(taskId)}`)).data
    .task

export const cancelRemoteTask = async (taskId: string) =>
  (
    await apiClient.post<{ operation: RemoteOperation }>(
      `/remote/tasks/${encodeURIComponent(taskId)}/cancellations`,
      undefined,
      { headers: idempotencyHeaders() },
    )
  ).data.operation

export const listDeviceAccessKeys = async () =>
  (await apiClient.get<CursorPage<DeviceAccessKey>>('/remote/device-access-keys')).data.items

export const createDeviceAccessKey = async (
  request: {
    label: string
    /** @deprecated Device enrollment always receives the full permission profile. */
    scopes?: RemoteScope[]
    expiresAt?: string | null
  },
  idempotencyKey = createRemoteIdempotencyKey(),
) =>
  (
    await apiClient.post<DeviceAccessKey>('/remote/device-access-keys', request, {
      headers: idempotencyHeaders(idempotencyKey),
    })
  ).data

export const revokeDeviceAccessKey = async (keyId: string) => {
  await apiClient.delete(`/remote/device-access-keys/${encodeURIComponent(keyId)}`)
}

export const deleteDeviceAccessKey = async (keyId: string) => {
  await apiClient.delete(`/remote/device-access-keys/${encodeURIComponent(keyId)}/permanent`)
}

export const rotateDeviceAccessKey = async (
  keyId: string,
  idempotencyKey = createRemoteIdempotencyKey(),
) =>
  (
    await apiClient.post<DeviceAccessKey>(
      `/remote/device-access-keys/${encodeURIComponent(keyId)}/rotation`,
      undefined,
      { headers: idempotencyHeaders(idempotencyKey) },
    )
  ).data

export const registerBrowserController = async (request: {
  controllerId: string
  identityAlgorithm: 'Ed25519'
  identityPublicKey: string
  proof: string
  scopes: RemoteScope[]
}) =>
  (
    await apiClient.post<{ controller: BrowserController }>('/remote/controllers', request, {
      headers: idempotencyHeaders(),
    })
  ).data.controller

export const getBrowserController = async (controllerId: string) =>
  (
    await apiClient.get<{ controller: BrowserController }>(
      `/remote/controllers/${encodeURIComponent(controllerId)}`,
    )
  ).data.controller

export const revokeBrowserController = async (controllerId: string) =>
  (
    await apiClient.delete<{ controller: BrowserController }>(
      `/remote/controllers/${encodeURIComponent(controllerId)}`,
      { headers: idempotencyHeaders() },
    )
  ).data.controller

/**
 * Creates a remote/v2 device Link grant.  This is deliberately separate from
 * the historical per-project Peer-ticket endpoint: neither projectId nor a
 * business scope is accepted here.
 */
export const createRemoteDeviceLink = async (
  controllerId: string,
  request: {
    targetDeviceId: string
    clientIdentityKeyVersion: number
    requestedMaximumLifetimeSeconds?: number
  },
  signal?: AbortSignal,
  idempotencyKey = createRemoteIdempotencyKey(),
) =>
  (
    await apiV2Client.post<{ deviceLink: RemoteDeviceLink }>(
      `/remote/controllers/${encodeURIComponent(controllerId)}/device-links`,
      request,
      { headers: idempotencyHeaders(idempotencyKey), signal },
    )
  ).data.deviceLink

export const revokeRemoteDeviceLink = async (controllerId: string, grantId: string) => {
  await apiV2Client.delete(
    `/remote/controllers/${encodeURIComponent(controllerId)}/device-links/${encodeURIComponent(grantId)}`,
  )
}
