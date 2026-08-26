import { describe, expect, it } from 'vitest'

import { mergeRemoteDelta, remoteCachePartition, RemoteDeltaGapError } from './cache'

describe('remote incremental cache', () => {
  it('isolates every partition by user, device, project, protocol and capability surface', () => {
    const identity = {
      userId: 'user/a',
      deviceId: 'device:b',
      projectId: 'project c',
      protocolVersion: 1,
      capabilityVersion: 'ai=2|files=2',
    }
    const baseline = remoteCachePartition(identity, 'conversations')
    const variants = [
      remoteCachePartition({ ...identity, userId: 'other-user' }, 'conversations'),
      remoteCachePartition({ ...identity, deviceId: 'other-device' }, 'conversations'),
      remoteCachePartition({ ...identity, projectId: 'other-project' }, 'conversations'),
      remoteCachePartition({ ...identity, protocolVersion: 2 }, 'conversations'),
      remoteCachePartition({ ...identity, capabilityVersion: 'ai=3|files=2' }, 'conversations'),
    ]

    expect(new Set([baseline, ...variants])).toHaveLength(6)
    expect(baseline).toContain('u=user%2Fa')
    expect(baseline).toContain('d=device%3Ab')
    expect(baseline).toContain('p=project%20c')
    expect(baseline).toContain('rpc=1')
  })

  it('fails closed when a cache identity is incomplete or has an invalid protocol', () => {
    const identity = {
      userId: 'user',
      deviceId: 'device',
      projectId: 'project',
      protocolVersion: 1,
      capabilityVersion: 'capability-v1',
    }

    expect(() => remoteCachePartition({ ...identity, projectId: ' ' }, 'files')).toThrow(
      'project ID is required',
    )
    expect(() =>
      remoteCachePartition({ ...identity, capabilityVersion: '' }, 'files'),
    ).toThrow('capability version is required')
    expect(() => remoteCachePartition({ ...identity, protocolVersion: 0 }, 'files')).toThrow(
      'protocol version is invalid',
    )
  })

  it('merges ordered changes, ignores replay and applies tombstones', () => {
    const state = mergeRemoteDelta(
      {
        records: [
          { id: 'alpha', revision: 2, title: 'newer local copy' },
          { id: 'removed', revision: 1, title: 'remove me' },
        ],
        highWatermark: 5,
      },
      {
        highWatermark: 9,
        changes: [
          { sequence: 7, value: { id: 'alpha', revision: 1, title: 'stale replay' } },
          { sequence: 6, value: { id: 'beta', revision: 1, title: 'created' } },
          {
            sequence: 8,
            deleted: true,
            value: { id: 'removed', revision: 2, title: '' },
          },
          { sequence: 9, value: { id: 'alpha', revision: 3, title: 'latest' } },
        ],
      },
    )

    expect(state.highWatermark).toBe(9)
    expect(state.records).toEqual(
      expect.arrayContaining([
        { id: 'alpha', revision: 3, title: 'latest' },
        { id: 'beta', revision: 1, title: 'created' },
      ]),
    )
    expect(state.records.find((record) => record.id === 'removed')).toBeUndefined()
  })

  it('drops a stale partition when the device journal requests a reset', () => {
    const state = mergeRemoteDelta(
      { records: [{ id: 'stale', revision: 99 }], highWatermark: 900 },
      {
        resetRequired: true,
        highWatermark: 2,
        changes: [{ sequence: 2, value: { id: 'fresh', revision: 1 } }],
      },
    )

    expect(state).toEqual({ records: [{ id: 'fresh', revision: 1 }], highWatermark: 2 })
  })

  it('refuses to advance the watermark across a missing journal sequence', () => {
    expect(() =>
      mergeRemoteDelta(
        { records: [{ id: 'cached', revision: 1 }], highWatermark: 5 },
        {
          highWatermark: 8,
          changes: [
            { sequence: 6, value: { id: 'six', revision: 1 } },
            { sequence: 8, value: { id: 'eight', revision: 1 } },
          ],
        },
      ),
    ).toThrowError(new RemoteDeltaGapError(7, 8))

    expect(() =>
      mergeRemoteDelta(
        { records: [{ id: 'cached', revision: 1 }], highWatermark: 5 },
        { highWatermark: 7, changes: [] },
      ),
    ).toThrowError(new RemoteDeltaGapError(6, 7))
  })
})
