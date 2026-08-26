import { create, fromBinary, toBinary } from '@bufbuild/protobuf'
import { sha256 } from '@noble/hashes/sha2.js'
import { describe, expect, it, vi } from 'vitest'

import { FrameType, type EncryptedRecord } from '@/generated/remote/v2/common_pb'
import {
  FileAckSchema,
  FileChunkSchema,
  FileCommitSchema,
  FileManifestSchema,
} from '@/generated/remote/v2/message_pb'

import { V2FileTransferClient, V2_FILE_CHUNK_BYTES } from './fileTransfer'
import type { V2Link, V2LinkFrame } from './link'
import type { V2RPCClient } from './rpcClient'

interface TestDownload {
  completed: Promise<void>
  chunks: Map<number, Uint8Array<ArrayBuffer>>
  receivedThrough: number
  acknowledgedThrough: number
  stateWaiters: Set<() => void>
  reject(error: Error): void
}

interface FileTransferInternals {
  downloads: Map<string, TestDownload>
  newDownload(input: {
    transferId: string
    channelId: string
    streamId: string
    totalLength: number
    expectedDigest: Uint8Array
    onProgress?: (received: number, total: number) => void
  }): TestDownload
  waitForDownloadCompletion(download: TestDownload): Promise<void>
}

describe('V2FileTransferClient prepared downloads', () => {
  it('pauses at the ACK boundary and resumes every verified unacknowledged chunk', async () => {
    const handlers = new Map<FrameType, (frame: V2LinkFrame) => void>()
    const sendEncrypted = vi.fn(async () => undefined)
    const link = {
      on: (type: FrameType, handler: (frame: V2LinkFrame) => void) => {
        handlers.set(type, handler)
        return () => handlers.delete(type)
      },
      sendEncrypted,
      waitForCarrier: vi.fn(async () => undefined),
    } as unknown as V2Link
    const client = new V2FileTransferClient(link, {} as V2RPCClient)
    const internals = client as unknown as FileTransferInternals
    const payload = new Uint8Array(V2_FILE_CHUNK_BYTES).fill(0x5a)
    const transferId = '11111111-1111-4111-8111-111111111111'
    const channelId = '22222222-2222-4222-8222-222222222222'
    const streamId = '33333333-3333-4333-8333-333333333333'
    const progress = vi.fn()
    const download = internals.newDownload({
      transferId,
      channelId,
      streamId,
      totalLength: payload.length,
      expectedDigest: sha256(payload),
      onProgress: progress,
    })
    internals.downloads.set(`${streamId}:${transferId}`, download)
    client.pauseDownloads()

    handlers.get(FrameType.FILE_CHUNK)?.({
      record: { channelId, streamId } as EncryptedRecord,
      plaintext: toBinary(
        FileChunkSchema,
        create(FileChunkSchema, {
          transferId,
          index: 0n,
          payload,
          chunkHash: sha256(payload),
        }),
      ),
    })
    await Promise.resolve()

    expect(progress).toHaveBeenCalledOnce()
    expect(download.chunks.get(0)).toEqual(payload)
    expect(download.receivedThrough).toBe(1)
    expect(download.acknowledgedThrough).toBe(0)
    expect(sendEncrypted).not.toHaveBeenCalled()

    client.resumeDownloads()
    await download.completed

    expect(download.acknowledgedThrough).toBe(1)
    expect(sendEncrypted).toHaveBeenCalledOnce()
    expect(sendEncrypted.mock.calls[0]?.slice(0, 3)).toEqual([
      FrameType.FILE_ACK,
      channelId,
      streamId,
    ])
    client.dispose()
  })

  it('suspends the inactivity timeout for the entire user-paused interval', async () => {
    vi.useFakeTimers()
    try {
      const handlers = new Map<FrameType, (frame: V2LinkFrame) => void>()
      const link = {
        on: (type: FrameType, handler: (frame: V2LinkFrame) => void) => {
          handlers.set(type, handler)
          return () => handlers.delete(type)
        },
      } as unknown as V2Link
      const client = new V2FileTransferClient(link, {} as V2RPCClient)
      const internals = client as unknown as FileTransferInternals
      const download = internals.newDownload({
        transferId: '44444444-4444-4444-8444-444444444444',
        channelId: '55555555-5555-4555-8555-555555555555',
        streamId: '66666666-6666-4666-8666-666666666666',
        totalLength: V2_FILE_CHUNK_BYTES,
        expectedDigest: new Uint8Array(32),
      })
      internals.downloads.set(
        '66666666-6666-4666-8666-666666666666:44444444-4444-4444-8444-444444444444',
        download,
      )
      client.pauseDownloads()
      const settled = vi.fn()
      const waiting = internals.waitForDownloadCompletion(download)
      void waiting.then(settled, settled)

      await vi.advanceTimersByTimeAsync(2 * 60_000)
      expect(settled).not.toHaveBeenCalled()

      download.reject(new Error('test cleanup'))
      await expect(waiting).rejects.toThrow('test cleanup')
      expect(download.stateWaiters.size).toBe(0)
      client.dispose()
    } finally {
      vi.useRealTimers()
    }
  })

  it('tracks logical files larger than 1 GiB without allocating their full length', () => {
    const link = {
      on: () => () => undefined,
    } as unknown as V2Link
    const client = new V2FileTransferClient(link, {} as V2RPCClient)
    const internals = client as unknown as FileTransferInternals
    const logicalLength = (1 << 30) + V2_FILE_CHUNK_BYTES

    const download = internals.newDownload({
      transferId: '77777777-7777-4777-8777-777777777777',
      channelId: '88888888-8888-4888-8888-888888888888',
      streamId: '99999999-9999-4999-8999-999999999999',
      totalLength: logicalLength,
      expectedDigest: new Uint8Array(32),
    })

    expect(download.chunks.size).toBe(0)
    expect(download.receivedThrough).toBe(0)
  })

  it('continues an upload from the Device-confirmed non-zero chunk offset', async () => {
    const handlers = new Map<FrameType, (frame: V2LinkFrame) => void>()
    const sentChunkIndexes: bigint[] = []
    const progress = vi.fn()
    const rpc = {
      call: vi.fn(async (method: string, input: Record<string, unknown>) => {
        if (method === 'file.upload.prepare') {
          return {
            transferId: input.transferId,
            chunkSize: V2_FILE_CHUNK_BYTES,
            acceptedOffset: V2_FILE_CHUNK_BYTES + 123,
          }
        }
        if (method === 'file.stat') return { revision: 2, entry: {} }
        throw new Error(`unexpected RPC method: ${method}`)
      }),
    } as unknown as V2RPCClient
    const link = {
      channels: {
        openProject: vi.fn(async () => ({ id: 'upload-channel' })),
        retain: vi.fn(),
        release: vi.fn(),
      },
      on: (type: FrameType, handler: (frame: V2LinkFrame) => void) => {
        handlers.set(type, handler)
        return () => handlers.delete(type)
      },
      waitForCarrier: vi.fn(async () => undefined),
      sendEncrypted: vi.fn(
        async (type: FrameType, channelId: string, streamId: string, plaintext: Uint8Array) => {
          if (type === FrameType.FILE_MANIFEST) {
            const manifest = fromBinary(FileManifestSchema, plaintext)
            handlers.get(FrameType.FILE_ACK)?.({
              record: { channelId, streamId } as EncryptedRecord,
              plaintext: toBinary(
                FileAckSchema,
                create(FileAckSchema, { transferId: manifest.transferId }),
              ),
            })
          }
          if (type === FrameType.FILE_CHUNK) {
            const chunk = fromBinary(FileChunkSchema, plaintext)
            sentChunkIndexes.push(chunk.index)
            handlers.get(FrameType.FILE_ACK)?.({
              record: { channelId, streamId } as EncryptedRecord,
              plaintext: toBinary(
                FileAckSchema,
                create(FileAckSchema, {
                  transferId: chunk.transferId,
                  confirmedIndexes: [chunk.index],
                }),
              ),
            })
          }
          if (type === FrameType.FILE_COMMIT) {
            const commit = fromBinary(FileCommitSchema, plaintext)
            handlers.get(FrameType.FILE_ACK)?.({
              record: { channelId, streamId } as EncryptedRecord,
              plaintext: toBinary(
                FileAckSchema,
                create(FileAckSchema, {
                  transferId: commit.transferId,
                }),
              ),
            })
          }
        },
      ),
    } as unknown as V2Link
    const payload = new Uint8Array(V2_FILE_CHUNK_BYTES * 2).fill(0x42)
    const file = new File([payload], 'resume.bin')
    const client = new V2FileTransferClient(link, rpc)

    await client.upload('resume.bin', file, { projectId: 'project-id' }, undefined, progress)

    expect(sentChunkIndexes).toEqual([1n])
    expect(progress).toHaveBeenNthCalledWith(1, V2_FILE_CHUNK_BYTES + 123, payload.length)
    expect(progress).toHaveBeenLastCalledWith(payload.length, payload.length)
    client.dispose()
  })
})
