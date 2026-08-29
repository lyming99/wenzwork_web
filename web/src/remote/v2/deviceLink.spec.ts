import { describe, expect, it } from 'vitest'

import type { RemoteDeviceLink } from '@/api/remote'

import { validateIssuedDeviceLink, type V2DeviceLinkClaims } from './deviceLink'

const controllerId = '018f1f2e-7b5f-789a-8abc-0123456789ab'
const deviceId = '018f1f2e-7b5f-789a-8abc-0123456789ac'
const grantId = '018f1f2e-7b5f-789a-8abc-0123456789ad'
const nodeId = '018f1f2e-7b5f-789a-8abc-0123456789ae'
const cellId = '018f1f2e-7b5f-789a-8abc-0123456789af'

const fixture = (): { link: RemoteDeviceLink; claims: V2DeviceLinkClaims } => {
  const expiresAt = new Date(Date.now() + 5 * 60_000).toISOString()
  return {
    link: {
      grantId,
      deviceConnectionGrant: 'header.payload.signature',
      expiresAt,
      maximumLifetimeSeconds: 300,
      connectionMode: 'direct',
      connectionUrl: 'ws://192.0.2.80:9443/v2/connect',
      relayUrl: 'ws://192.0.2.80:9443/v2/connect',
      relayNodeId: nodeId,
      relayCellId: cellId,
      targetConnectionEpoch: 7,
      deviceIdentityAlgorithm: 'Ed25519',
      deviceIdentityPublicKey: 'A'.repeat(43),
      deviceKeyThumbprint: 'B'.repeat(43),
      deviceIdentityKeyVersion: 2,
    },
    claims: {
      grant_id: grantId,
      client_id: controllerId,
      device_id: deviceId,
      relay_node_id: nodeId,
      relay_cell_id: cellId,
      target_connection_epoch: 7,
      client_identity_key_version: 3,
      device_identity_key_version: 2,
      exp: Math.floor(Date.parse(expiresAt) / 1000),
    },
  }
}

describe('remote/v2 connection endpoint validation', () => {
  it('accepts a bounded direct Carrier endpoint', () => {
    const { link, claims } = fixture()
    expect(() =>
      validateIssuedDeviceLink(link, claims, {
        controllerId,
        targetDeviceId: deviceId,
        keyVersion: 3,
      }),
    ).not.toThrow()
  })

  it('rejects persistent direct grants and mismatched compatibility aliases', () => {
    const persistent = fixture()
    persistent.link.maximumLifetimeSeconds = 0
    expect(() =>
      validateIssuedDeviceLink(persistent.link, persistent.claims, {
        controllerId,
        targetDeviceId: deviceId,
        keyVersion: 3,
      }),
    ).toThrow('直连授权有效期')

    const mismatched = fixture()
    mismatched.link.relayUrl = 'ws://192.0.2.81:9443/v2/connect'
    expect(() =>
      validateIssuedDeviceLink(mismatched.link, mismatched.claims, {
        controllerId,
        targetDeviceId: deviceId,
        keyVersion: 3,
      }),
    ).toThrow('授权绑定无效')
  })
})
