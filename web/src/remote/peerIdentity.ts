import { ed25519 } from '@noble/curves/ed25519.js'

import { bytesToBase64Url, utf8 as utf8Bytes } from './v2/crypto'

const DATABASE_NAME = 'wenzwork-remote-security'
const DATABASE_VERSION = 1
const STORE_NAME = 'controller-identities'

interface StoredControllerIdentity {
  userId: string
  controllerId: string
  privateKey: Uint8Array
  keyVersion: number
  connectionEpoch: number
  createdAt: string
}

export interface BrowserControllerIdentity extends StoredControllerIdentity {
  publicKey: Uint8Array
  identityPublicKey: string
}

let identityOperation = Promise.resolve()

const serialized = async <T>(operation: () => Promise<T>) => {
  const previous = identityOperation
  let release!: () => void
  identityOperation = new Promise<void>((resolve) => {
    release = resolve
  })
  await previous
  try {
    return await operation()
  } finally {
    release()
  }
}

const openDatabase = () =>
  new Promise<IDBDatabase>((resolve, reject) => {
    if (typeof indexedDB === 'undefined') {
      reject(new Error('This browser cannot securely persist the remote controller key.'))
      return
    }
    const request = indexedDB.open(DATABASE_NAME, DATABASE_VERSION)
    request.onupgradeneeded = () => {
      const database = request.result
      if (!database.objectStoreNames.contains(STORE_NAME)) {
        database.createObjectStore(STORE_NAME, { keyPath: 'userId' })
      }
    }
    request.onsuccess = () => resolve(request.result)
    request.onerror = () =>
      reject(request.error ?? new Error('Unable to open controller key storage.'))
    request.onblocked = () => reject(new Error('Controller key storage upgrade is blocked.'))
  })

const readRecord = (database: IDBDatabase, userId: string) =>
  new Promise<StoredControllerIdentity | undefined>((resolve, reject) => {
    const transaction = database.transaction(STORE_NAME, 'readonly')
    const request = transaction.objectStore(STORE_NAME).get(userId)
    request.onsuccess = () => resolve(request.result as StoredControllerIdentity | undefined)
    request.onerror = () =>
      reject(request.error ?? new Error('Unable to read controller identity.'))
  })

const writeRecord = (database: IDBDatabase, identity: StoredControllerIdentity) =>
  new Promise<void>((resolve, reject) => {
    const transaction = database.transaction(STORE_NAME, 'readwrite')
    transaction.objectStore(STORE_NAME).put(identity)
    transaction.oncomplete = () => resolve()
    transaction.onerror = () =>
      reject(transaction.error ?? new Error('Unable to save controller identity.'))
    transaction.onabort = () =>
      reject(transaction.error ?? new Error('Saving controller identity was aborted.'))
  })

const deleteRecord = (database: IDBDatabase, userId: string) =>
  new Promise<void>((resolve, reject) => {
    const transaction = database.transaction(STORE_NAME, 'readwrite')
    transaction.objectStore(STORE_NAME).delete(userId)
    transaction.oncomplete = () => resolve()
    transaction.onerror = () =>
      reject(transaction.error ?? new Error('Unable to remove controller identity.'))
    transaction.onabort = () =>
      reject(transaction.error ?? new Error('Removing controller identity was aborted.'))
  })

const validateRecord = (value: StoredControllerIdentity | undefined, userId: string) => {
  if (
    !value ||
    value.userId !== userId ||
    !value.controllerId ||
    !(value.privateKey instanceof Uint8Array) ||
    value.privateKey.length !== 32 ||
    !Number.isSafeInteger(value.keyVersion) ||
    value.keyVersion < 1 ||
    !Number.isSafeInteger(value.connectionEpoch) ||
    value.connectionEpoch < 0
  ) {
    return undefined
  }
  return value
}

export const loadBrowserControllerIdentity = (userId: string) =>
  serialized(async (): Promise<BrowserControllerIdentity> => {
    if (!userId || userId.length > 128 || !globalThis.crypto?.randomUUID) {
      throw new Error('The authenticated user or browser cryptography is unavailable.')
    }
    const database = await openDatabase()
    try {
      let stored = validateRecord(await readRecord(database, userId), userId)
      if (!stored) {
        stored = {
          userId,
          controllerId: crypto.randomUUID(),
          privateKey: ed25519.utils.randomSecretKey(),
          keyVersion: 1,
          connectionEpoch: 0,
          createdAt: new Date().toISOString(),
        }
        await writeRecord(database, stored)
      }
      const publicKey = ed25519.getPublicKey(stored.privateKey)
      return {
        ...stored,
        privateKey: stored.privateKey.slice(),
        publicKey,
        identityPublicKey: bytesToBase64Url(publicKey),
      }
    } finally {
      database.close()
    }
  })

export const nextConnectionEpoch = (identity: BrowserControllerIdentity) =>
  serialized(async () => {
    const database = await openDatabase()
    try {
      const stored = validateRecord(await readRecord(database, identity.userId), identity.userId)
      if (
        !stored ||
        stored.controllerId !== identity.controllerId ||
        stored.keyVersion !== identity.keyVersion ||
        stored.connectionEpoch >= Number.MAX_SAFE_INTEGER
      ) {
        throw new Error('Controller connection epoch is unavailable.')
      }
      stored.connectionEpoch += 1
      await writeRecord(database, stored)
      identity.connectionEpoch = stored.connectionEpoch
      return stored.connectionEpoch
    } finally {
      database.close()
    }
  })

export const resetBrowserControllerIdentity = (userId: string, expectedControllerId: string) =>
  serialized(async () => {
    if (!userId || !expectedControllerId) {
      throw new Error('The controller identity reset request is incomplete.')
    }
    const database = await openDatabase()
    try {
      const stored = validateRecord(await readRecord(database, userId), userId)
      if (!stored || stored.controllerId !== expectedControllerId) {
        throw new Error('The saved controller identity changed; refresh before resetting it.')
      }
      await deleteRecord(database, userId)
    } finally {
      database.close()
    }
  })

export const controllerRegistrationTranscript = (input: {
  userId: string
  controllerId: string
  identityPublicKey: string
  keyVersion: number
}) =>
  utf8Bytes(
    `wenzwork-browser-controller:v2\n${input.userId}\n${input.controllerId}\n${input.identityPublicKey}\n${input.keyVersion}`,
  )

export const signControllerRegistration = (identity: BrowserControllerIdentity) =>
  bytesToBase64Url(
    ed25519.sign(
      controllerRegistrationTranscript({
        userId: identity.userId,
        controllerId: identity.controllerId,
        identityPublicKey: identity.identityPublicKey,
        keyVersion: identity.keyVersion,
      }),
      identity.privateKey,
    ),
  )
