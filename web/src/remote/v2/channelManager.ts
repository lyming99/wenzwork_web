import { create, toBinary } from '@bufbuild/protobuf'

import {
  ChannelCloseSchema,
  ChannelOpenSchema,
  type ChannelAccept,
} from '@/generated/remote/v2/channel_pb'
import { ChannelKind, FrameType, ProtocolErrorCode } from '@/generated/remote/v2/common_pb'

import { V2_CHANNEL_CONTROL_STREAM_ID } from './crypto'

export interface V2Channel {
  id: string
  kind: ChannelKind
  projectId?: string
  scopes: ReadonlySet<string>
  capabilityRevision: Uint8Array
}

export const V2_CHANNEL_MAXIMUM = 24
export const V2_CHANNEL_RESERVED_CAPACITY = 8
export const V2_CHANNEL_IDLE_TTL_MS = 60_000

interface ChannelState {
  references: number
  lastUsedAt: number
  pinned: boolean
  idleTimer?: ReturnType<typeof setTimeout>
}

interface PendingChannel {
  channel: V2Channel
  resolve: (channel: V2Channel) => void
  reject: (error: Error) => void
  timer: ReturnType<typeof setTimeout>
}

export interface V2ChannelSender {
  sendEncrypted(
    frameType: FrameType,
    channelId: string,
    streamId: string,
    plaintext: Uint8Array,
  ): Promise<void>
}

const projectPattern = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/u

const sortedUniqueScopes = (scopes: readonly string[]) => {
  const values = [...new Set(scopes.map((scope) => scope.trim()))].sort()
  if (
    values.length === 0 ||
    values.some(
      (scope) =>
        !scope.startsWith('remote.peer.') ||
        scope.length > 80 ||
        scope.includes('\0') ||
        scope.includes('\r') ||
        scope.includes('\n'),
    )
  ) {
    throw new Error('remote/v2 Channel scopes are invalid.')
  }
  return values
}

/** Manages encrypted Device-query and project Channels without Carrier state. */
export class V2ChannelManager {
  private readonly channels = new Map<string, V2Channel>()
  private readonly pending = new Map<string, PendingChannel>()
  private readonly reusable = new Map<string, Promise<V2Channel>>()
  private readonly reusableKeyByChannel = new Map<string, string>()
  private readonly states = new Map<string, ChannelState>()

  constructor(
    private readonly sender: V2ChannelSender,
    private readonly options: {
      idleTtlMs?: number
      maximumChannels?: number
      reservedCapacity?: number
    } = {},
  ) {}

  /**
   * Opens a least-privilege device-level Channel. Capabilities must use the
   * query scope; the only other device-level operation today is AI
   * configuration. Every project-bound capability goes through openProject.
   */
  openDeviceScope(scope = 'remote.peer.query') {
    if (scope !== 'remote.peer.query' && scope !== 'remote.peer.ai.config') {
      throw new Error('该远程能力必须在项目 Channel 中执行。')
    }
    return this.open(`device:${scope}`, ChannelKind.DEVICE_QUERY, undefined, [scope])
  }

  openDeviceQuery() {
    return this.openDeviceScope('remote.peer.query')
  }

  openProject(projectId: string, scopes: readonly string[]) {
    if (!projectPattern.test(projectId)) throw new Error('请先选择一个可用项目，再使用该远程能力。')
    const normalized = sortedUniqueScopes(scopes)
    return this.open(
      `project:${projectId}:${normalized.join(',')}`,
      ChannelKind.PROJECT,
      projectId,
      normalized,
    )
  }

  get(channelId: string) {
    return this.channels.get(channelId)
  }

  get activeCount() {
    return this.channels.size
  }

  get channelStats() {
    let referenced = 0
    let pinned = 0
    for (const state of this.states.values()) {
      if (state.references > 0) referenced += 1
      if (state.pinned) pinned += 1
    }
    return { active: this.channels.size, referenced, pinned }
  }

  acquire(channelId: string): V2Channel | undefined
  acquire(projectId: string, scopes: readonly string[]): Promise<V2Channel>
  acquire(first: string, scopes?: readonly string[]) {
    if (scopes !== undefined) {
      return this.openProject(first, scopes).then((channel) => {
        this.retain(channel.id)
        return channel
      })
    }
    const channel = this.channels.get(first)
    if (!channel) return undefined
    this.retain(first)
    return channel
  }

  retain(channelId: string) {
    const state = this.states.get(channelId)
    if (!state) return false
    state.references += 1
    state.lastUsedAt = Date.now()
    if (state.idleTimer) {
      clearTimeout(state.idleTimer)
      state.idleTimer = undefined
    }
    return true
  }

  release(channelId: string) {
    const state = this.states.get(channelId)
    if (!state) return
    state.references = Math.max(0, state.references - 1)
    state.lastUsedAt = Date.now()
    this.scheduleIdleClose(channelId, state)
  }

  setPinned(channelId: string, pinned: boolean) {
    const state = this.states.get(channelId)
    if (!state) return
    state.pinned = pinned
    state.lastUsedAt = Date.now()
    if (!pinned) this.scheduleIdleClose(channelId, state)
  }

  async close(channelId: string, reason = ProtocolErrorCode.UNSPECIFIED) {
    const channel = this.channels.get(channelId)
    if (!channel) return
    const state = this.states.get(channelId)
    if (state?.idleTimer) clearTimeout(state.idleTimer)
    this.states.delete(channelId)
    this.channels.delete(channelId)
    const reusableKey = this.reusableKeyByChannel.get(channelId)
    if (reusableKey) this.reusable.delete(reusableKey)
    this.reusableKeyByChannel.delete(channelId)
    await this.sender.sendEncrypted(
      FrameType.CHANNEL_CLOSE,
      channelId,
      V2_CHANNEL_CONTROL_STREAM_ID,
      toBinary(ChannelCloseSchema, create(ChannelCloseSchema, { channelId, reason })),
    )
  }

  handleAccept(message: ChannelAccept) {
    const pending = this.pending.get(message.channelId)
    if (!pending) return false
    const granted = sortedUniqueScopes(message.grantedScopes)
    for (const scope of granted) {
      if (!pending.channel.scopes.has(scope)) {
        this.reject(message.channelId, new Error('设备返回了未请求的 Channel 权限。'))
        return true
      }
    }
    clearTimeout(pending.timer)
    this.pending.delete(message.channelId)
    const channel: V2Channel = {
      ...pending.channel,
      scopes: new Set(granted),
      capabilityRevision: message.capabilityRevision.slice(),
    }
    this.channels.set(channel.id, channel)
    const state = { references: 0, lastUsedAt: Date.now(), pinned: false } satisfies ChannelState
    this.states.set(channel.id, state)
    this.scheduleIdleClose(channel.id, state)
    pending.resolve(channel)
    return true
  }

  handleClose(channelId: string) {
    const state = this.states.get(channelId)
    if (state?.idleTimer) clearTimeout(state.idleTimer)
    this.states.delete(channelId)
    this.channels.delete(channelId)
    const key = this.reusableKeyByChannel.get(channelId)
    if (key) this.reusable.delete(key)
    this.reusableKeyByChannel.delete(channelId)
    this.reject(channelId, new Error('远程项目 Channel 已关闭。'))
  }

  fail(error: Error) {
    for (const pending of this.pending.values()) {
      clearTimeout(pending.timer)
      pending.reject(error)
    }
    this.pending.clear()
    this.channels.clear()
    this.reusable.clear()
    this.reusableKeyByChannel.clear()
    for (const state of this.states.values()) {
      if (state.idleTimer) clearTimeout(state.idleTimer)
    }
    this.states.clear()
  }

  private open(
    key: string,
    kind: ChannelKind,
    projectId: string | undefined,
    scopes: readonly string[],
  ) {
    const active = this.reusable.get(key)
    if (active) return active
    this.ensureCapacity()
    const promise = new Promise<V2Channel>((resolve, reject) => {
      const channelId = crypto.randomUUID()
      this.reusableKeyByChannel.set(channelId, key)
      const channel: V2Channel = {
        id: channelId,
        kind,
        projectId,
        scopes: new Set(scopes),
        capabilityRevision: new Uint8Array(),
      }
      const timer = setTimeout(
        () => this.reject(channelId, new Error('打开远程项目 Channel 超时。')),
        15_000,
      )
      this.pending.set(channelId, { channel, resolve, reject, timer })
      const plaintext = toBinary(
        ChannelOpenSchema,
        create(ChannelOpenSchema, {
          channelId,
          kind,
          projectId: projectId ?? '',
          scopes: [...scopes],
        }),
      )
      void this.sender
        .sendEncrypted(FrameType.CHANNEL_OPEN, channelId, V2_CHANNEL_CONTROL_STREAM_ID, plaintext)
        .catch((error) =>
          this.reject(
            channelId,
            error instanceof Error ? error : new Error('打开远程项目 Channel 失败。'),
          ),
        )
    })
    this.reusable.set(key, promise)
    void promise.then((value) => {
      if (this.reusable.get(key) === promise) this.reusableKeyByChannel.set(value.id, key)
    })
    void promise.catch(() => {
      if (this.reusable.get(key) === promise) this.reusable.delete(key)
    })
    return promise
  }

  private reject(channelId: string, error: Error) {
    const pending = this.pending.get(channelId)
    if (!pending) return
    clearTimeout(pending.timer)
    this.pending.delete(channelId)
    const key = this.reusableKeyByChannel.get(channelId)
    if (key) this.reusable.delete(key)
    this.reusableKeyByChannel.delete(channelId)
    pending.reject(error)
  }

  private ensureCapacity() {
    const maximum = this.options.maximumChannels ?? V2_CHANNEL_MAXIMUM
    const reserved = this.options.reservedCapacity ?? V2_CHANNEL_RESERVED_CAPACITY
    const target = Math.max(1, maximum - reserved)
    while (this.channels.size + this.pending.size >= target) {
      const candidate = [...this.states.entries()]
        .filter(([, state]) => state.references === 0 && !state.pinned)
        .sort(([, left], [, right]) => left.lastUsedAt - right.lastUsedAt)[0]
      if (!candidate) break
      void this.close(candidate[0], ProtocolErrorCode.UNSPECIFIED).catch(() => undefined)
    }
    if (
      this.channels.size + this.pending.size >= maximum &&
      ![...this.states.values()].some((state) => state.references === 0 && !state.pinned)
    ) {
      throw new Error('远程 Channel 容量已满，请先结束空闲操作。')
    }
  }

  private scheduleIdleClose(channelId: string, state: ChannelState) {
    if (state.references > 0 || state.pinned || state.idleTimer) return
    const ttl = Math.max(0, this.options.idleTtlMs ?? V2_CHANNEL_IDLE_TTL_MS)
    state.idleTimer = setTimeout(() => {
      state.idleTimer = undefined
      const current = this.states.get(channelId)
      if (current !== state || current.references > 0 || current.pinned) return
      void this.close(channelId).catch(() => undefined)
    }, ttl)
  }
}
