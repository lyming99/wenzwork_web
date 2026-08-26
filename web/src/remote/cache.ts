export interface RevisionedRecord {
  id: string
  revision: number
}

export interface RemoteChange<T extends RevisionedRecord> {
  sequence: number
  deleted?: boolean
  value: T
}

export interface SyncState<T extends RevisionedRecord> {
  records: T[]
  highWatermark: number
}

export interface SyncDelta<T extends RevisionedRecord> {
  changes: RemoteChange<T>[]
  highWatermark: number
  resetRequired?: boolean
}

export class RemoteDeltaGapError extends Error {
  readonly code = 'remote_delta_gap'
  readonly expectedSequence: number
  readonly receivedSequence: number

  constructor(expectedSequence: number, receivedSequence: number) {
    super(
      `Remote change journal is not contiguous: expected ${expectedSequence}, received ${receivedSequence}.`,
    )
    this.name = 'RemoteDeltaGapError'
    this.expectedSequence = expectedSequence
    this.receivedSequence = receivedSequence
  }
}

/**
 * Applies an ordered device change journal without replacing newer cached
 * revisions. Duplicate events are harmless and a retained-journal gap forces
 * a partition reset instead of silently returning an incomplete view.
 */
export const mergeRemoteDelta = <T extends RevisionedRecord>(
  current: SyncState<T>,
  delta: SyncDelta<T>,
): SyncState<T> => {
  const records = new Map<string, T>(
    (delta.resetRequired ? [] : current.records).map((record) => [record.id, record]),
  )
  let watermark = delta.resetRequired ? 0 : current.highWatermark

  const changes = [...delta.changes].sort(
    (left, right) => left.sequence - right.sequence || left.value.id.localeCompare(right.value.id),
  )
  for (const change of changes) {
    if (!Number.isSafeInteger(change.sequence) || change.sequence <= watermark) continue
    if (!delta.resetRequired && change.sequence !== watermark + 1) {
      throw new RemoteDeltaGapError(watermark + 1, change.sequence)
    }
    const previous = records.get(change.value.id)
    if (!previous || change.value.revision >= previous.revision) {
      if (change.deleted) records.delete(change.value.id)
      else records.set(change.value.id, change.value)
    }
    watermark = change.sequence
  }

  if (!delta.resetRequired && delta.highWatermark > watermark) {
    throw new RemoteDeltaGapError(watermark + 1, delta.highWatermark)
  }

  return {
    records: [...records.values()],
    highWatermark: Math.max(watermark, delta.highWatermark),
  }
}

interface StoredRecord<T extends RevisionedRecord> {
  key: string
  partition: string
  value: T
}

interface StoredPartition {
  partition: string
  highWatermark: number
  updatedAt: string
}

const databaseName = 'wenzwork-remote-cache-v1'
const recordStore = 'records'
const partitionStore = 'partitions'

const openDatabase = () =>
  new Promise<IDBDatabase>((resolve, reject) => {
    const request = indexedDB.open(databaseName, 1)
    request.onerror = () => reject(request.error ?? new Error('无法打开远程缓存。'))
    request.onupgradeneeded = () => {
      const database = request.result
      if (!database.objectStoreNames.contains(recordStore)) {
        const records = database.createObjectStore(recordStore, { keyPath: 'key' })
        records.createIndex('partition', 'partition', { unique: false })
      }
      if (!database.objectStoreNames.contains(partitionStore)) {
        database.createObjectStore(partitionStore, { keyPath: 'partition' })
      }
    }
    request.onsuccess = () => resolve(request.result)
  })

const requestValue = <T>(request: IDBRequest<T>) =>
  new Promise<T>((resolve, reject) => {
    request.onerror = () => reject(request.error ?? new Error('远程缓存请求失败。'))
    request.onsuccess = () => resolve(request.result)
  })

const transactionDone = (transaction: IDBTransaction) =>
  new Promise<void>((resolve, reject) => {
    transaction.onerror = () => reject(transaction.error ?? new Error('远程缓存事务失败。'))
    transaction.onabort = () => reject(transaction.error ?? new Error('远程缓存事务已取消。'))
    transaction.oncomplete = () => resolve()
  })

export class RemoteIndexedCache {
  static supported() {
    return typeof indexedDB !== 'undefined'
  }

  async read<T extends RevisionedRecord>(partition: string): Promise<SyncState<T>> {
    if (!RemoteIndexedCache.supported()) return { records: [], highWatermark: 0 }
    const database = await openDatabase()
    try {
      const transaction = database.transaction([recordStore, partitionStore], 'readonly')
      const recordsRequest = transaction
        .objectStore(recordStore)
        .index('partition')
        .getAll(IDBKeyRange.only(partition))
      const partitionRequest = transaction.objectStore(partitionStore).get(partition)
      const [storedRecords, storedPartition] = await Promise.all([
        requestValue(recordsRequest) as Promise<StoredRecord<T>[]>,
        requestValue(partitionRequest) as Promise<StoredPartition | undefined>,
        transactionDone(transaction),
      ])
      return {
        records: storedRecords.map((record) => record.value),
        highWatermark: storedPartition?.highWatermark ?? 0,
      }
    } finally {
      database.close()
    }
  }

  async replace<T extends RevisionedRecord>(partition: string, state: SyncState<T>): Promise<void> {
    if (!RemoteIndexedCache.supported()) return
    const database = await openDatabase()
    try {
      const transaction = database.transaction([recordStore, partitionStore], 'readwrite')
      const records = transaction.objectStore(recordStore)
      const keys = await requestValue(
        records.index('partition').getAllKeys(IDBKeyRange.only(partition)),
      )
      for (const key of keys) records.delete(key)
      for (const value of state.records) {
        records.put({ key: `${partition}:${value.id}`, partition, value } satisfies StoredRecord<T>)
      }
      transaction.objectStore(partitionStore).put({
        partition,
        highWatermark: state.highWatermark,
        updatedAt: new Date().toISOString(),
      } satisfies StoredPartition)
      await transactionDone(transaction)
    } finally {
      database.close()
    }
  }

  async merge<T extends RevisionedRecord>(
    partition: string,
    delta: SyncDelta<T>,
  ): Promise<SyncState<T>> {
    const next = mergeRemoteDelta(await this.read<T>(partition), delta)
    await this.replace(partition, next)
    return next
  }

  async clearPartition(partition: string): Promise<void> {
    await this.replace(partition, { records: [], highWatermark: 0 })
  }
}

export interface RemoteCacheIdentity {
  userId: string
  deviceId: string
  projectId: string
  protocolVersion: number
  capabilityVersion: string
}

type RemoteCacheResource =
    | 'projects'
    | 'tasks'
    | 'tasks-sequence-v2'
    | 'task-projections-v3'
    | 'ai-configs'
    | 'conversations'
    | 'files'
    | `messages:${string}`
    | `project:${string}:conversations`
    | `project:${string}:messages:${string}`

const partitionSegment = (value: string, label: string) => {
  const normalized = value.trim()
  if (!normalized) throw new Error(`Remote cache ${label} is required.`)
  return encodeURIComponent(normalized)
}

/**
 * Builds a fail-closed cache namespace. Every device-derived record is bound
 * to the signed-in user, device, project (or explicit device projection), RPC
 * protocol and advertised capability surface so an upgrade or context switch
 * cannot hydrate an incompatible view.
 */
export const remoteCachePartition = (
  identity: RemoteCacheIdentity,
  resource: RemoteCacheResource,
) => {
  if (!Number.isSafeInteger(identity.protocolVersion) || identity.protocolVersion <= 0) {
    throw new Error('Remote cache protocol version is invalid.')
  }
  return [
    `u=${partitionSegment(identity.userId, 'user ID')}`,
    `d=${partitionSegment(identity.deviceId, 'device ID')}`,
    `p=${partitionSegment(identity.projectId, 'project ID')}`,
    `rpc=${identity.protocolVersion}`,
    `cap=${partitionSegment(identity.capabilityVersion, 'capability version')}`,
    `r=${partitionSegment(resource, 'resource')}`,
  ].join('|')
}
