import { create, fromBinary, toBinary } from '@bufbuild/protobuf'
import { sha256 } from '@noble/hashes/sha2.js'

import { StreamCloseSchema, StreamOpenSchema } from '@/generated/remote/v2/channel_pb'
import { FrameType, ProtocolErrorCode, StreamKind } from '@/generated/remote/v2/common_pb'
import {
  FileAckSchema,
  FileChunkSchema,
  FileCommitSchema,
  FileManifestSchema,
  type FileAck,
} from '@/generated/remote/v2/message_pb'
import type { RemoteFileEntry, RemoteRPCContext } from '@/remote/rpcTypes'

import { V2CarrierBackpressureError } from './carrier'
import { V2_CHANNEL_CONTROL_STREAM_ID, base64UrlToBytes, bytesToBase64Url } from './crypto'
import { V2Link, type V2LinkFrame } from './link'
import { V2RPCClient } from './rpcClient'

export const V2_FILE_CHUNK_BYTES = 32 << 10
// Bound the ACK-driven upload pipeline so latency is hidden without allowing
// one file to monopolize the Carrier's bulk queue.
export const V2_FILE_IN_FLIGHT_WINDOW = 8
const chunkBytes = V2_FILE_CHUNK_BYTES
const fileRecoveryBudgetMs = 5 * 60_000
const maximumChunkReordering = 64
const cooperativeHashChunkBytes = 1 << 20

export interface PreparedDownloadSource {
  prepareMethod: string
  prepareInput: Record<string, unknown>
  scope: 'remote.peer.file.receive' | 'remote.peer.task.control'
  expectedRevision?: number
  maximumBytes?: number
  requireFileName?: boolean
  expectedPreparedValues?: Readonly<Record<string, string | number | boolean>>
}

export interface PreparedDownloadResult {
  blob: Blob
  fileName?: string
}

class V2FileCarrierInterruptedError extends Error {
  constructor() {
    super('文件传输的物理 Carrier 已中断。')
    this.name = 'V2FileCarrierInterruptedError'
  }
}

const yieldToBrowser = () => new Promise<void>((resolve) => setTimeout(resolve, 0))

const hashBlobCooperatively = async (value: Blob) => {
  const hasher = sha256.create()
  for (let offset = 0; offset < value.size; offset += cooperativeHashChunkBytes) {
    const end = Math.min(value.size, offset + cooperativeHashChunkBytes)
    hasher.update(new Uint8Array(await value.slice(offset, end).arrayBuffer()))
    await yieldToBrowser()
  }
  return hasher.digest()
}

const hashChunksCooperatively = async (
  chunks: Map<number, Uint8Array<ArrayBuffer>>,
  totalChunks: number,
) => {
  const hasher = sha256.create()
  let bytesSinceYield = 0
  for (let index = 0; index < totalChunks; index += 1) {
    const chunk = chunks.get(index)
    if (!chunk) throw new Error('下载文件缺少已确认的数据块。')
    hasher.update(chunk)
    bytesSinceYield += chunk.length
    if (bytesSinceYield >= cooperativeHashChunkBytes) {
      bytesSinceYield = 0
      await yieldToBrowser()
    }
  }
  return hasher.digest()
}

const advanceCursor = (through: number, sparse: Set<number>, index: number) => {
  if (index < through) return through
  sparse.add(index)
  while (sparse.delete(through)) through += 1
  return through
}

const equalBytes = (left: Uint8Array, right: Uint8Array) => {
  if (left.length !== right.length) return false
  for (let index = 0; index < left.length; index += 1) {
    if (left[index] !== right[index]) return false
  }
  return true
}

interface PendingAck {
  resolve: (ack: FileAck) => void
  reject: (error: Error) => void
  timer: ReturnType<typeof setTimeout>
  expectedIndex?: bigint
}

interface AckWaiter {
  pending: PendingAck
  promise: Promise<FileAck>
}

interface ActiveDownload {
  transferId: string
  channelId: string
  streamId: string
  totalLength: number
  chunkSize: number
  totalChunks: number
  expectedDigest: Uint8Array
  chunks: Map<number, Uint8Array<ArrayBuffer>>
  receivedThrough: number
  receivedSparse: Set<number>
  acknowledgedThrough: number
  acknowledgedSparse: Set<number>
  acking: Set<number>
  receivedBytes: number
  onProgress?: (received: number, total: number) => void
  resolve: () => void
  reject: (error: Error) => void
  completed: Promise<void>
  carrierInterrupted: boolean
  paused: boolean
  stateWaiters: Set<() => void>
  interrupt?: (error: Error) => void
}

/** Dedicated bulk Stream uploader. The opaque handle is a prepared transfer ID. */
export class V2FileTransferClient {
  private readonly waiters = new Map<string, PendingAck[]>()
  private readonly downloads = new Map<string, ActiveDownload>()
  private readonly detachAck: () => void
  private readonly detachChunk: () => void
  private readonly detachClose: () => void

  constructor(
    private readonly link: V2Link,
    private readonly rpc: V2RPCClient,
  ) {
    this.detachAck = link.on(FrameType.FILE_ACK, (frame) => {
      const ack = fromBinary(FileAckSchema, frame.plaintext)
      const queue = this.waiters.get(`${frame.record.streamId}:${ack.transferId}`)
      const pendingIndex = queue?.findIndex(
        (candidate) =>
          candidate.expectedIndex === undefined ||
          ack.confirmedIndexes.includes(candidate.expectedIndex),
      )
      const pending =
        queue && pendingIndex !== undefined && pendingIndex >= 0
          ? queue.splice(pendingIndex, 1)[0]
          : undefined
      if (pending) {
        if (queue?.length === 0) this.waiters.delete(`${frame.record.streamId}:${ack.transferId}`)
        clearTimeout(pending.timer)
        pending.resolve(ack)
      }
    })
    this.detachChunk = link.on(FrameType.FILE_CHUNK, (frame) => this.handleDownloadChunk(frame))
    this.detachClose = link.on(FrameType.STREAM_CLOSE, (frame) => {
      for (const [key, queue] of this.waiters) {
        if (!key.startsWith(`${frame.record.streamId}:`)) continue
        this.waiters.delete(key)
        for (const pending of queue) {
          clearTimeout(pending.timer)
          pending.reject(new Error('文件传输 Stream 已关闭。'))
        }
      }
      for (const [key, download] of this.downloads) {
        if (download.streamId !== frame.record.streamId) continue
        this.downloads.delete(key)
        download.reject(new Error('文件下载 Stream 已关闭。'))
      }
    })
  }

  dispose() {
    this.detachAck()
    this.detachChunk()
    this.detachClose()
    for (const queue of this.waiters.values()) {
      for (const pending of queue) {
        clearTimeout(pending.timer)
        pending.reject(new Error('文件传输已关闭。'))
      }
    }
    this.waiters.clear()
    for (const download of this.downloads.values()) {
      download.reject(new Error('文件下载已关闭。'))
    }
    this.downloads.clear()
  }

  /** Wake ACK/download waits immediately while preserving resumable state. */
  handleCarrierInterrupted() {
    const failure = new V2FileCarrierInterruptedError()
    for (const queue of this.waiters.values()) {
      for (const pending of queue) {
        clearTimeout(pending.timer)
        pending.reject(failure)
      }
    }
    this.waiters.clear()
    for (const download of this.downloads.values()) {
      download.carrierInterrupted = true
      download.interrupt?.(failure)
    }
  }

  pauseDownloads() {
    for (const download of this.downloads.values()) {
      download.paused = true
      this.notifyDownloadState(download)
    }
  }

  resumeDownloads() {
    for (const download of this.downloads.values()) {
      download.paused = false
      this.notifyDownloadState(download)
      const upperBound = Math.min(
        download.totalChunks,
        download.acknowledgedThrough + maximumChunkReordering,
      )
      for (let index = download.acknowledgedThrough; index < upperBound; index += 1) {
        this.scheduleDownloadAck(download, index)
      }
    }
  }

  async upload(
    relativePath: string,
    file: File,
    context: RemoteRPCContext,
    expectedRevision?: number,
    onProgress?: (sent: number, total: number) => void,
  ): Promise<{ revision: number; entry: RemoteFileEntry }> {
    if (!context.projectId || !Number.isSafeInteger(file.size) || file.size < 0) {
      throw new Error('文件传输参数无效。')
    }
    const transferId = crypto.randomUUID()
    // Keep hashing off long, synchronous UI turns and never mirror the whole
    // File into a second in-memory buffer.
    const digest = await hashBlobCooperatively(file)
    const digestText = bytesToBase64Url(digest)
    const prepared = await this.recoverOperation(() =>
      this.rpc.call<Record<string, unknown>>(
        'file.upload.prepare',
        {
          transferId,
          path: relativePath,
          size: file.size,
          sha256: digestText,
          ...(expectedRevision === undefined ? {} : { expectedRevision }),
        },
        context,
        transferId,
      ),
    )
    const acceptedOffset = prepared.acceptedOffset ?? 0
    if (
      prepared.transferId !== transferId ||
      !Number.isSafeInteger(prepared.chunkSize) ||
      prepared.chunkSize !== chunkBytes ||
      !Number.isSafeInteger(acceptedOffset) ||
      (acceptedOffset as number) < 0 ||
      (acceptedOffset as number) > file.size
    ) {
      throw new Error('设备返回的文件传输清单无效。')
    }
    const channel = await this.link.channels.openProject(context.projectId, [
      'remote.peer.file.send',
    ])
    this.link.channels.retain(channel.id)
    const streamId = crypto.randomUUID()
    const streamOpen = toBinary(
      StreamOpenSchema,
      create(StreamOpenSchema, {
        channelId: channel.id,
        streamId,
        kind: StreamKind.FILE,
        operationId: transferId,
      }),
    )
    const reopenStream = () =>
      this.sendWithRecovery(
        FrameType.STREAM_OPEN,
        channel.id,
        V2_CHANNEL_CONTROL_STREAM_ID,
        streamOpen,
      )
    try {
      await reopenStream()
      await this.sendAndWait(
        streamId,
        transferId,
        FrameType.FILE_MANIFEST,
        toBinary(
          FileManifestSchema,
          create(FileManifestSchema, {
            transferId,
            totalLength: BigInt(file.size),
            chunkSize: chunkBytes,
            sha256: digest,
            relativePathHandle: transferId,
            expectedRevision: BigInt(expectedRevision ?? 0),
          }),
        ),
        channel.id,
        reopenStream,
      )
      let offset =
        acceptedOffset === file.size
          ? (acceptedOffset as number)
          : Math.floor((acceptedOffset as number) / chunkBytes) * chunkBytes
      let index = BigInt(Math.floor(offset / chunkBytes))
      let acknowledgedBytes = acceptedOffset as number
      if (acknowledgedBytes > 0) onProgress?.(acknowledgedBytes, file.size)
      while (offset < file.size) {
        const batch: Array<Promise<void>> = []
        for (let slot = 0; slot < V2_FILE_IN_FLIGHT_WINDOW && offset < file.size; slot += 1) {
          const chunkOffset = offset
          const chunkEnd = Math.min(file.size, chunkOffset + chunkBytes)
          const chunkIndex = index
          const payload = new Uint8Array(await file.slice(chunkOffset, chunkEnd).arrayBuffer())
          batch.push(
            this.sendAndWait(
              streamId,
              transferId,
              FrameType.FILE_CHUNK,
              toBinary(
                FileChunkSchema,
                create(FileChunkSchema, {
                  transferId,
                  index: chunkIndex,
                  chunkHash: sha256(payload),
                  payload,
                }),
              ),
              channel.id,
              reopenStream,
              chunkIndex,
            ).then((ack) => {
              if (!ack.confirmedIndexes.includes(chunkIndex)) {
                throw new Error('设备没有确认文件块。')
              }
              acknowledgedBytes = Math.max(acknowledgedBytes, chunkEnd)
              onProgress?.(acknowledgedBytes, file.size)
            }),
          )
          offset = chunkEnd
          index += 1n
        }
        const results = await Promise.allSettled(batch)
        const failed = results.find(
          (result): result is PromiseRejectedResult => result.status === 'rejected',
        )
        if (failed) throw failed.reason
      }
      await this.sendAndWait(
        streamId,
        transferId,
        FrameType.FILE_COMMIT,
        toBinary(
          FileCommitSchema,
          create(FileCommitSchema, {
            transferId,
            sha256: digest,
            expectedRevision: BigInt(expectedRevision ?? 0),
          }),
        ),
        channel.id,
        reopenStream,
      )
      const stat = await this.rpc.call<Record<string, unknown>>(
        'file.stat',
        { path: relativePath },
        context,
      )
      if (!Number.isSafeInteger(stat.revision) || !stat.entry || typeof stat.entry !== 'object') {
        throw new Error('设备未返回已提交文件。')
      }
      return { revision: stat.revision as number, entry: stat.entry as RemoteFileEntry }
    } finally {
      await this.link
        .sendEncrypted(
          FrameType.STREAM_CLOSE,
          channel.id,
          V2_CHANNEL_CONTROL_STREAM_ID,
          toBinary(
            StreamCloseSchema,
            create(StreamCloseSchema, {
              channelId: channel.id,
              streamId,
              reason: ProtocolErrorCode.STREAM_CANCELLED,
            }),
          ),
        )
        .catch(() => undefined)
      this.link.channels.release(channel.id)
    }
  }

  /** Dedicated bulk Stream downloader; large content never uses JSON RPC chunks. */
  async download(
    relativePath: string,
    context: RemoteRPCContext,
    expectedRevision?: number,
    onProgress?: (received: number, total: number) => void,
  ): Promise<Blob> {
    return (
      await this.downloadPreparedSource(
        {
          prepareMethod: 'file.download.prepare',
          prepareInput: {
            path: relativePath,
            ...(expectedRevision === undefined ? {} : { expectedRevision }),
          },
          scope: 'remote.peer.file.receive',
          expectedRevision,
        },
        context,
        onProgress,
      )
    ).blob
  }

  async downloadPreparedSource(
    source: PreparedDownloadSource,
    context: RemoteRPCContext,
    onProgress?: (received: number, total: number) => void,
  ): Promise<PreparedDownloadResult> {
    if (!context.projectId) throw new Error('请先选择一个可用项目，再下载文件。')
    const transferId = crypto.randomUUID()
    const prepared = await this.recoverOperation(() =>
      this.rpc.call<Record<string, unknown>>(
        source.prepareMethod,
        {
          transferId,
          ...source.prepareInput,
        },
        context,
        transferId,
      ),
    )
    if (
      prepared.transferId !== transferId ||
      !Number.isSafeInteger(prepared.size) ||
      (prepared.size as number) < 0 ||
      (source.maximumBytes !== undefined &&
        (!Number.isSafeInteger(source.maximumBytes) ||
          source.maximumBytes < 0 ||
          (prepared.size as number) > source.maximumBytes)) ||
      prepared.chunkSize !== chunkBytes ||
      typeof prepared.sha256 !== 'string' ||
      (source.requireFileName &&
        (typeof prepared.fileName !== 'string' ||
          prepared.fileName.length < 1 ||
          prepared.fileName.length > 255 ||
          prepared.fileName.includes('/') ||
          prepared.fileName.includes('\\') ||
          !prepared.fileName.endsWith('.log'))) ||
      Object.entries(source.expectedPreparedValues ?? {}).some(
        ([key, expected]) => prepared[key] !== expected,
      )
    ) {
      throw new Error('设备返回的文件下载清单无效。')
    }
    let expectedDigest: Uint8Array
    try {
      expectedDigest = base64UrlToBytes(prepared.sha256)
    } catch {
      throw new Error('设备返回的文件下载摘要无效。')
    }
    if (expectedDigest.length !== 32) throw new Error('设备返回的文件下载摘要无效。')
    const totalLength = prepared.size as number
    const channel = await this.link.channels.openProject(context.projectId, [source.scope])
    this.link.channels.retain(channel.id)
    const streamId = crypto.randomUUID()
    const streamOpen = toBinary(
      StreamOpenSchema,
      create(StreamOpenSchema, {
        channelId: channel.id,
        streamId,
        kind: StreamKind.FILE,
        operationId: transferId,
      }),
    )
    const manifest = toBinary(
      FileManifestSchema,
      create(FileManifestSchema, {
        transferId,
        totalLength: BigInt(totalLength),
        chunkSize: chunkBytes,
        sha256: expectedDigest,
        relativePathHandle: transferId,
        expectedRevision: BigInt(source.expectedRevision ?? 0),
      }),
    )
    const reopenStream = () =>
      this.sendWithRecovery(
        FrameType.STREAM_OPEN,
        channel.id,
        V2_CHANNEL_CONTROL_STREAM_ID,
        streamOpen,
      )
    const download = this.newDownload({
      transferId,
      channelId: channel.id,
      streamId,
      totalLength,
      expectedDigest,
      onProgress,
    })
    const key = this.downloadKey(streamId, transferId)
    this.downloads.set(key, download)
    try {
      await reopenStream()
      await this.sendWithRecovery(FrameType.FILE_MANIFEST, channel.id, streamId, manifest)
      await this.waitForDownload(download, reopenStream, manifest)
      if (
        bytesToBase64Url(await hashChunksCooperatively(download.chunks, download.totalChunks)) !==
        bytesToBase64Url(expectedDigest)
      ) {
        throw new Error('下载文件校验失败。')
      }
      await this.sendAndWait(
        streamId,
        transferId,
        FrameType.FILE_COMMIT,
        toBinary(
          FileCommitSchema,
          create(FileCommitSchema, {
            transferId,
            sha256: expectedDigest,
            expectedRevision: BigInt(source.expectedRevision ?? 0),
          }),
        ),
        channel.id,
        reopenStream,
      )
      return {
        blob: new Blob(
          Array.from({ length: download.totalChunks }, (_, index) => {
            const chunk = download.chunks.get(index)
            if (!chunk) throw new Error('下载文件缺少已确认的数据块。')
            return chunk
          }),
        ),
        fileName: typeof prepared.fileName === 'string' ? prepared.fileName : undefined,
      }
    } finally {
      this.downloads.delete(key)
      await this.link
        .sendEncrypted(
          FrameType.STREAM_CLOSE,
          channel.id,
          V2_CHANNEL_CONTROL_STREAM_ID,
          toBinary(
            StreamCloseSchema,
            create(StreamCloseSchema, {
              channelId: channel.id,
              streamId,
              reason: ProtocolErrorCode.STREAM_CANCELLED,
            }),
          ),
        )
        .catch(() => undefined)
      this.link.channels.release(channel.id)
    }
  }

  private newDownload(input: {
    transferId: string
    channelId: string
    streamId: string
    totalLength: number
    expectedDigest: Uint8Array
    onProgress?: (received: number, total: number) => void
  }): ActiveDownload {
    let resolve!: () => void
    let reject!: (error: Error) => void
    const totalChunks = Math.ceil(input.totalLength / chunkBytes)
    const completed = new Promise<void>((resolvePromise, rejectPromise) => {
      resolve = resolvePromise
      reject = rejectPromise
    })
    const state: ActiveDownload = {
      ...input,
      chunkSize: chunkBytes,
      totalChunks,
      chunks: new Map<number, Uint8Array<ArrayBuffer>>(),
      receivedThrough: 0,
      receivedSparse: new Set<number>(),
      acknowledgedThrough: 0,
      acknowledgedSparse: new Set<number>(),
      acking: new Set<number>(),
      receivedBytes: 0,
      resolve,
      reject,
      completed,
      carrierInterrupted: false,
      paused: false,
      stateWaiters: new Set(),
    }
    if (totalChunks === 0) state.resolve()
    return state
  }

  private downloadKey(streamId: string, transferId: string) {
    return `${streamId}:${transferId}`
  }

  private handleDownloadChunk(frame: V2LinkFrame) {
    const chunk = fromBinary(FileChunkSchema, frame.plaintext)
    const download = this.downloads.get(this.downloadKey(frame.record.streamId, chunk.transferId))
    if (!download || frame.record.channelId !== download.channelId) return
    try {
      const index = Number(chunk.index)
      if (!Number.isSafeInteger(index) || index < 0 || index >= download.totalChunks) {
        throw new Error('设备返回了无效的文件下载块序号。')
      }
      if (
        index >= download.receivedThrough &&
        index - download.receivedThrough >= maximumChunkReordering
      ) {
        throw new Error('设备返回的文件下载块超出重排窗口。')
      }
      const offset = index * download.chunkSize
      const expectedLength = Math.min(download.chunkSize, download.totalLength - offset)
      if (
        chunk.payload.length !== expectedLength ||
        chunk.chunkHash.length !== 32 ||
        bytesToBase64Url(sha256(chunk.payload)) !== bytesToBase64Url(chunk.chunkHash)
      ) {
        throw new Error('设备返回了无效的文件下载块。')
      }
      const existing = download.chunks.get(index)
      if (existing && !equalBytes(existing, chunk.payload)) {
        throw new Error('设备为同一序号返回了不同的文件下载块。')
      }
      if (!existing) {
        download.chunks.set(index, Uint8Array.from(chunk.payload))
        download.receivedThrough = advanceCursor(
          download.receivedThrough,
          download.receivedSparse,
          index,
        )
        download.receivedBytes += chunk.payload.length
        download.onProgress?.(download.receivedBytes, download.totalLength)
      }
      this.scheduleDownloadAck(download, index)
    } catch (error) {
      download.reject(error instanceof Error ? error : new Error('设备返回了无效的文件下载块。'))
    }
  }

  private scheduleDownloadAck(download: ActiveDownload, index: number) {
    if (
      download.paused ||
      index < download.acknowledgedThrough ||
      index !== download.acknowledgedThrough ||
      download.acknowledgedSparse.has(index) ||
      download.acking.has(index) ||
      !download.chunks.has(index)
    )
      return
    download.acking.add(index)
    void this.ackDownload(download, index)
      .then(() => {
        download.acknowledgedThrough = advanceCursor(
          download.acknowledgedThrough,
          download.acknowledgedSparse,
          index,
        )
        this.resolveDownloadIfComplete(download)
        this.scheduleDownloadAck(download, download.acknowledgedThrough)
      })
      .catch((error) =>
        download.reject(error instanceof Error ? error : new Error('无法确认文件下载块。')),
      )
      .finally(() => download.acking.delete(index))
  }

  private async ackDownload(download: ActiveDownload, index: number) {
    await this.sendWithRecovery(
      FrameType.FILE_ACK,
      download.channelId,
      download.streamId,
      toBinary(
        FileAckSchema,
        create(FileAckSchema, {
          transferId: download.transferId,
          confirmedIndexes: [BigInt(index)],
        }),
      ),
    )
  }

  private resolveDownloadIfComplete(download: ActiveDownload) {
    if (
      download.receivedThrough === download.totalChunks &&
      download.acknowledgedThrough === download.totalChunks
    ) {
      download.resolve()
    }
  }

  private notifyDownloadState(download: ActiveDownload) {
    for (const resolve of download.stateWaiters) resolve()
    download.stateWaiters.clear()
  }

  private waitForDownloadState(download: ActiveDownload) {
    let wake!: () => void
    const promise = new Promise<void>((resolve) => {
      wake = resolve
      download.stateWaiters.add(wake)
    })
    return {
      promise,
      cancel: () => download.stateWaiters.delete(wake),
    }
  }

  private async waitForDownload(
    download: ActiveDownload,
    reopenStream: () => Promise<void>,
    manifest: Uint8Array,
  ) {
    const deadline = Date.now() + fileRecoveryBudgetMs
    for (;;) {
      try {
        await this.waitForDownloadCompletion(download)
        return
      } catch (error) {
        const normalized = error instanceof Error ? error : new Error('文件下载失败。')
        if (
          normalized.message !== '等待文件下载块超时。' &&
          !(normalized instanceof V2FileCarrierInterruptedError)
        ) {
          throw normalized
        }
        if (Date.now() >= deadline) {
          throw normalized
        }
        download.carrierInterrupted = false
        await this.waitForCarrierUntil(deadline, normalized)
        await reopenStream()
        await this.sendWithRecovery(
          FrameType.FILE_MANIFEST,
          download.channelId,
          download.streamId,
          manifest,
        )
      }
    }
  }

  private async waitForDownloadCompletion(download: ActiveDownload) {
    for (;;) {
      if (download.carrierInterrupted) throw new V2FileCarrierInterruptedError()
      let timer: ReturnType<typeof setTimeout> | undefined
      let interrupt: ((error: Error) => void) | undefined
      const stateWaiter = this.waitForDownloadState(download)
      try {
        const outcome = await Promise.race([
          download.completed.then(() => 'complete' as const),
          stateWaiter.promise.then(() => 'stateChanged' as const),
          ...(download.paused
            ? []
            : [
                new Promise<never>((_, reject) => {
                  timer = setTimeout(() => reject(new Error('等待文件下载块超时。')), 30_000)
                }),
              ]),
          new Promise<never>((_, reject) => {
            interrupt = reject
            download.interrupt = reject
            if (download.carrierInterrupted) reject(new V2FileCarrierInterruptedError())
          }),
        ])
        if (outcome === 'complete') return
      } finally {
        stateWaiter.cancel()
        if (timer) clearTimeout(timer)
        if (download.interrupt === interrupt) download.interrupt = undefined
      }
    }
  }

  private async sendAndWait(
    streamId: string,
    transferId: string,
    frameType: FrameType,
    plaintext: Uint8Array,
    channelId: string,
    reopenStream: () => Promise<void>,
    expectedIndex?: bigint,
  ) {
    const deadline = Date.now() + fileRecoveryBudgetMs
    for (;;) {
      const waiter = this.waitForAck(streamId, transferId, expectedIndex)
      try {
        await this.link.sendEncrypted(frameType, channelId, streamId, plaintext)
        return await waiter.promise
      } catch (error) {
        this.removeWaiter(streamId, transferId, waiter.pending)
        const normalized = error instanceof Error ? error : new Error('文件传输失败。')
        if (!this.canRecover(normalized) || Date.now() >= deadline) throw normalized
        await this.waitForCarrierUntil(deadline, normalized)
        // The original STREAM_OPEN may have been buffered locally when the
        // Carrier failed. Reasserting the same IDs is idempotent at Device and
        // makes a resumed manifest/chunk valid whether it arrived or not.
        await reopenStream()
      }
    }
  }

  private waitForAck(streamId: string, transferId: string, expectedIndex?: bigint) {
    const key = `${streamId}:${transferId}`
    let pending!: PendingAck
    const promise = new Promise<FileAck>((resolve, reject) => {
      const timer = setTimeout(() => {
        this.removeWaiter(streamId, transferId, pending)
        reject(new Error('等待文件块确认超时。'))
      }, 30_000)
      const queue = this.waiters.get(key) ?? []
      pending = { resolve, reject, timer, expectedIndex }
      queue.push(pending)
      this.waiters.set(key, queue)
    })
    return { pending, promise } satisfies AckWaiter
  }

  private removeWaiter(streamId: string, transferId: string, pending: PendingAck) {
    const key = `${streamId}:${transferId}`
    const queue = this.waiters.get(key)
    if (!queue) return
    const index = queue.indexOf(pending)
    if (index < 0) return
    queue.splice(index, 1)
    clearTimeout(pending.timer)
    if (queue.length === 0) this.waiters.delete(key)
  }

  private canRecover(error: Error) {
    if (error instanceof V2CarrierBackpressureError) return false
    // A device-side Stream close is an authorization/protocol result, not a
    // Carrier interruption. Retrying it would turn one bad Stream into work
    // on a healthy Link.
    return (
      !error.message.includes('Stream 已关闭') &&
      !error.message.includes('文件传输 Stream') &&
      !error.message.includes('设备拒绝了远程操作')
    )
  }

  private async sendWithRecovery(
    frameType: FrameType,
    channelId: string,
    streamId: string,
    plaintext: Uint8Array,
  ) {
    await this.recoverOperation(() =>
      this.link.sendEncrypted(frameType, channelId, streamId, plaintext),
    )
  }

  private async recoverOperation<T>(operation: () => Promise<T>) {
    const deadline = Date.now() + fileRecoveryBudgetMs
    for (;;) {
      try {
        return await operation()
      } catch (error) {
        const normalized = error instanceof Error ? error : new Error('文件传输失败。')
        if (!this.canRecover(normalized) || Date.now() >= deadline) throw normalized
        await this.waitForCarrierUntil(deadline, normalized)
      }
    }
  }

  private async waitForCarrierUntil(deadline: number, fallback: Error) {
    const remaining = deadline - Date.now()
    if (remaining <= 0) throw fallback
    try {
      await this.link.waitForCarrier(remaining)
    } catch (error) {
      if (Date.now() >= deadline) throw fallback
      throw error instanceof Error ? error : fallback
    }
  }
}
