import { createRemoteDeviceLink, type RemoteDeviceLink } from '@/api/remote'

import { base64UrlToBytes } from './crypto'

export interface V2DeviceLinkClaims {
  grant_id: string
  client_id: string
  device_id: string
  relay_node_id: string
  relay_cell_id: string
  target_connection_epoch: number
  client_identity_key_version: number
  device_identity_key_version: number
  exp: number
}

export interface IssuedDeviceLink {
  link: RemoteDeviceLink
  claims: V2DeviceLinkClaims
}

const uuidPattern = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/u

const parseJWTClaims = (grant: string): V2DeviceLinkClaims => {
  const parts = grant.split('.')
  if (parts.length !== 3 || parts.some((part) => part.length === 0 || part.length > 8192)) {
    throw new Error('设备连接授权格式无效。')
  }
  let value: unknown
  try {
    value = JSON.parse(new TextDecoder('utf-8', { fatal: true }).decode(base64UrlToBytes(parts[1])))
  } catch {
    throw new Error('设备连接授权格式无效。')
  }
  if (!value || typeof value !== 'object' || Array.isArray(value)) {
    throw new Error('设备连接授权格式无效。')
  }
  const claims = value as Record<string, unknown>
  const string = (key: string) => (typeof claims[key] === 'string' ? claims[key] : '')
  const integer = (key: string) =>
    typeof claims[key] === 'number' && Number.isSafeInteger(claims[key]) && claims[key] > 0
      ? claims[key]
      : 0
  const result: V2DeviceLinkClaims = {
    grant_id: string('grant_id'),
    client_id: string('client_id'),
    device_id: string('device_id'),
    relay_node_id: string('relay_node_id'),
    relay_cell_id: string('relay_cell_id'),
    target_connection_epoch: integer('target_connection_epoch'),
    client_identity_key_version: integer('client_identity_key_version'),
    device_identity_key_version: integer('device_identity_key_version'),
    exp: integer('exp'),
  }
  if (
    !uuidPattern.test(result.grant_id) ||
    !uuidPattern.test(result.client_id) ||
    !uuidPattern.test(result.device_id) ||
    !uuidPattern.test(result.relay_node_id) ||
    !uuidPattern.test(result.relay_cell_id)
  ) {
    throw new Error('设备连接授权绑定无效。')
  }
  return result
}

export const validateIssuedDeviceLink = (
  link: RemoteDeviceLink,
  claims: V2DeviceLinkClaims,
  input: { controllerId: string; targetDeviceId: string; keyVersion: number },
) => {
  let endpoint: URL
  try {
    endpoint = new URL(link.relayUrl)
  } catch {
    throw new Error('Relay 地址无效。')
  }
  if (
    (endpoint.protocol !== 'ws:' && endpoint.protocol !== 'wss:') ||
    endpoint.pathname !== '/v2/connect' ||
    endpoint.search ||
    endpoint.hash ||
    claims.client_id !== input.controllerId ||
    link.grantId !== claims.grant_id ||
    claims.device_id !== input.targetDeviceId ||
    claims.relay_node_id !== link.relayNodeId ||
    claims.relay_cell_id !== link.relayCellId ||
    claims.target_connection_epoch !== link.targetConnectionEpoch ||
    claims.client_identity_key_version !== input.keyVersion ||
    claims.device_identity_key_version !== link.deviceIdentityKeyVersion ||
    link.deviceIdentityAlgorithm !== 'Ed25519' ||
    base64UrlToBytes(link.deviceIdentityPublicKey).length !== 32 ||
    !Number.isSafeInteger(link.targetConnectionEpoch) ||
    link.targetConnectionEpoch < 1 ||
    !Number.isSafeInteger(link.deviceIdentityKeyVersion) ||
    link.deviceIdentityKeyVersion < 1 ||
    Date.parse(link.expiresAt) <= Date.now() + 1000 ||
    claims.exp * 1000 < Date.parse(link.expiresAt) - 1000
  ) {
    throw new Error('设备连接授权绑定无效。')
  }
  if (location.protocol === 'https:' && endpoint.protocol === 'ws:') {
    throw new Error('当前 HTTPS 页面不能连接 ws:// Relay；请将 Relay 配置为 wss://。')
  }
}

/** Requests and validates a proof-bound Grant. Callers may cache it in memory. */
export const issueDeviceLink = async (input: {
  controllerId: string
  targetDeviceId: string
  keyVersion: number
  signal?: AbortSignal
  idempotencyKey?: string
}): Promise<IssuedDeviceLink> => {
  const link = await createRemoteDeviceLink(
    input.controllerId,
    {
      targetDeviceId: input.targetDeviceId,
      clientIdentityKeyVersion: input.keyVersion,
    },
    input.signal,
    input.idempotencyKey,
  )
  const claims = parseJWTClaims(link.deviceConnectionGrant)
  validateIssuedDeviceLink(link, claims, input)
  return { link, claims }
}
