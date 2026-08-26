import { flushPromises, mount } from '@vue/test-utils'
import { createPinia } from 'pinia'
import { nextTick, ref, type Ref } from 'vue'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import {
  mergeRemoteDelta,
  RemoteIndexedCache,
  type RevisionedRecord,
  type SyncDelta,
  type SyncState,
} from '@/remote/cache'
import {
  agentEventConnectionKey,
  type AgentEventConnection,
  type AgentStateEvent,
} from '@/remote/agentEvents'
import {
  remoteRPCKey,
  type RemoteAIEvent,
  type RemoteChatMessage,
  type RemoteConversation,
  type RemoteRPCClient,
} from '@/remote/rpcTypes'
import type { RemoteAgentCapabilities } from '@/remote/peerClient'
import { useAuthStore } from '@/stores/auth'

import RemoteChatPanel from './RemoteChatPanel.vue'

const projectPartition = (resource: string) =>
  `u=user-1|d=device-1|p=11111111-1111-4111-8111-111111111111|rpc=1|cap=test-capability-v2|r=${encodeURIComponent(resource)}`

const cacheStates = new Map<string, SyncState<RevisionedRecord>>()

const cloneState = <T extends RevisionedRecord>(state: SyncState<T>): SyncState<T> => ({
  records: [...state.records],
  highWatermark: state.highWatermark,
})

const conversation = (
  revision: number,
  messageCount: number,
  overrides: Partial<RemoteConversation> = {},
): RemoteConversation => ({
  id: 'conversation-1',
  projectId: '11111111-1111-4111-8111-111111111111',
  revision,
  title: '测试对话',
  configId: 'default',
  modelBinding: {
    configId: 'default',
    configRevision: 1,
    provider: 'openai-compatible',
    model: 'model-a',
  },
  workspaceMode: 'readOnly',
  lastMessageSequence: messageCount,
  createdAt: '2026-08-08T00:00:00Z',
  updatedAt: `2026-08-08T00:00:${String(revision).padStart(2, '0')}Z`,
  messageCount,
  state: 'idle',
  ...overrides,
})

const message = (sequence: number, revision = sequence): RemoteChatMessage => ({
  id: `message-${sequence}`,
  revision,
  sequence,
  role: sequence % 2 === 0 ? 'assistant' : 'user',
  content: `message-${sequence}`,
  status: 'complete',
  errorCode: '',
  attachments: [],
  reasoning: '',
  toolRuns: [],
  usage: {
    inputTokens: 0,
    outputTokens: 0,
    reasoningTokens: 0,
    cachedInputTokens: 0,
    totalTokens: 0,
  },
  providerRun: {
    provider: '',
    model: '',
    providerRequestId: '',
    finishReason: '',
    attemptCount: 0,
  },
  createdAt: `2026-08-08T00:${String(Math.floor(sequence / 60)).padStart(2, '0')}:${String(sequence % 60).padStart(2, '0')}Z`,
})

const event = (
  kind: RemoteAIEvent['kind'],
  sequence: number,
  payload: RemoteAIEvent['payload'],
): RemoteAIEvent => ({
  eventId: `event-${sequence}`,
  conversationId: 'conversation-1',
  generationId: 'generation-1',
  messageId: 'message-2',
  kind,
  sequence,
  payload,
  occurredAt: '2026-08-08T00:00:02Z',
})

const idleStartStream: RemoteRPCClient['startStream'] = <TDelta, TResult = unknown>(
  method: string,
  input: Record<string, unknown>,
  onDelta: (delta: TDelta) => void,
) => {
  void method
  void input
  void onDelta
  return {
    result: Promise.resolve(undefined as TResult),
    detach: async () => undefined,
  }
}

const installMemoryCache = () => {
  vi.spyOn(RemoteIndexedCache, 'supported').mockReturnValue(true)
  vi.spyOn(RemoteIndexedCache.prototype, 'read').mockImplementation(
    async <T extends RevisionedRecord>(partition: string): Promise<SyncState<T>> =>
      cloneState(
        (cacheStates.get(partition) ?? {
          records: [],
          highWatermark: 0,
        }) as SyncState<T>,
      ),
  )
  vi.spyOn(RemoteIndexedCache.prototype, 'replace').mockImplementation(
    async <T extends RevisionedRecord>(partition: string, state: SyncState<T>) => {
      cacheStates.set(partition, cloneState(state) as SyncState<RevisionedRecord>)
    },
  )
  const merge = vi
    .spyOn(RemoteIndexedCache.prototype, 'merge')
    .mockImplementation(
      async <T extends RevisionedRecord>(
        partition: string,
        delta: SyncDelta<T>,
      ): Promise<SyncState<T>> => {
        const current = cloneState(
          (cacheStates.get(partition) ?? {
            records: [],
            highWatermark: 0,
          }) as SyncState<T>,
        )
        const next = mergeRemoteDelta(current, delta)
        cacheStates.set(partition, cloneState(next) as SyncState<RevisionedRecord>)
        return cloneState(next)
      },
    )
  vi.spyOn(RemoteIndexedCache.prototype, 'clearPartition').mockImplementation(
    async (partition: string) => {
      cacheStates.set(partition, { records: [], highWatermark: 0 })
    },
  )
  return merge
}

const mountPanel = (
  call: RemoteRPCClient['call'],
  stream: RemoteRPCClient['stream'] = vi.fn(async () => undefined),
  connected: Ref<boolean> = ref(true),
  startStream: RemoteRPCClient['startStream'] = idleStartStream,
  agentEvents?: AgentEventConnection,
  agentCapabilities: RemoteAgentCapabilities = {
    protocolMinimum: 1,
    protocolMaximum: 1,
    featureVersions: { projects: 2, ai: 3 },
    features: { 'project.v2': true, 'ai.v2': true, 'ai.generationRecovery': true },
    operatingSystem: 'windows',
    architecture: 'amd64',
    shells: ['powershell'],
    taskRunners: ['script'],
    resourceLimits: {},
  },
  componentProps: { toolsAuthorized?: boolean } = {},
) => {
  const pinia = createPinia()
  useAuthStore(pinia).applyAccount({
    user: {
      id: 'user-1',
      email: 'user@example.test',
      displayName: 'User',
      status: 'active',
      emailVerifiedAt: '2026-08-08T00:00:00Z',
      roles: [],
    },
    permissions: [],
    mfaEnforced: false,
    assuranceLevel: 2,
    absoluteExpiresAt: '2026-08-09T00:00:00Z',
  })
  const rpc = {
    connected,
    reconnecting: ref(false),
    error: ref(''),
    connect: vi.fn(async () => undefined),
    close: vi.fn(async () => undefined),
    getCapabilities: vi.fn(async () => agentCapabilities),
    call,
    stream,
    startStream,
    subscribeAgentEvents: vi.fn(async () => undefined),
    cancelAgentEventSubscriptions: vi.fn(async () => undefined),
    downloadFile: vi.fn(),
    downloadTaskLog: vi.fn(),
    uploadFile: vi.fn(),
  } as RemoteRPCClient
  return {
    connected,
    rpc,
    wrapper: mount(RemoteChatPanel, {
      props: {
        deviceId: 'device-1',
        projectId: '11111111-1111-4111-8111-111111111111',
        protocolVersion: 1,
        capabilityVersion: 'test-capability-v2',
        ...componentProps,
      },
      global: {
        plugins: [pinia],
        provide: {
          [remoteRPCKey as symbol]: rpc,
          ...(agentEvents ? { [agentEventConnectionKey as symbol]: agentEvents } : {}),
        },
      },
    }),
  }
}

const buttonWithText = (wrapper: ReturnType<typeof mountPanel>['wrapper'], label: string) => {
  const button = wrapper.findAll('button').find((candidate) => candidate.text().includes(label))
  if (!button) throw new Error(`missing button: ${label}`)
  return button
}

describe('RemoteChatPanel', () => {
  beforeEach(() => {
    vi.restoreAllMocks()
    cacheStates.clear()
  })

  it('opens an active conversation from one snapshot, attaches after its cursor, and detaches on leave', async () => {
    installMemoryCache()
    const activeConversation = conversation(4, 2, {
      state: 'generating',
      generationId: 'generation-1',
    })
    const snapshotMessage: RemoteChatMessage = {
      ...message(2),
      status: 'streaming',
      content: 'already persisted',
      generationId: 'generation-1',
    }
    const calls: Array<{ method: string; input: Record<string, unknown> }> = []
    const call = vi.fn(async (method: string, input: Record<string, unknown> = {}) => {
      calls.push({ method, input })
      if (method === 'conversation.list') {
        return {
          items: [activeConversation],
          changes: [],
          nextCursor: null,
          highWatermark: 4,
          resetRequired: true,
        }
      }
      if (method === 'conversation.get') {
        return {
          conversation: activeConversation,
          messages: [snapshotMessage],
          nextCursor: null,
          highWatermark: 2,
          resetRequired: false,
          snapshotEventHighWatermark: 41,
          earliestAvailableEventSequence: 1,
        }
      }
      throw new Error(`unexpected ${method}`)
    }) as RemoteRPCClient['call']
    const detach = vi.fn(async () => undefined)
    const startStream = vi.fn(
      (
        _method: string,
        _input: Record<string, unknown>,
        onDelta: (delta: RemoteAIEvent) => void,
      ) => {
        onDelta(event('chat.text.delta', 42, { delta: ' plus live' }))
        return {
          result: new Promise<unknown>(() => undefined),
          detach,
        }
      },
    ) as unknown as RemoteRPCClient['startStream']

    const mounted = mountPanel(call, undefined, ref(true), startStream)
    await flushPromises()

    expect(startStream).toHaveBeenCalledWith(
      'conversation.generation.attach',
      {
        conversationId: 'conversation-1',
        generationId: 'generation-1',
        afterSequence: 41,
      },
      expect.any(Function),
      { projectId: '11111111-1111-4111-8111-111111111111' },
    )
    expect(mounted.wrapper.findAll('.message-list article')).toHaveLength(1)
    expect(mounted.wrapper.find('.message-list article p').text()).toBe(
      'already persisted plus live',
    )

    mounted.wrapper.unmount()
    await flushPromises()

    expect(detach).toHaveBeenCalledTimes(1)
    expect(calls.some(({ method }) => method === 'conversation.cancel')).toBe(false)
  })

  it.each([
    ['a first-event gap', () => event('chat.text.delta', 2, { delta: 'must-not-apply' })],
    [
      'an invalid runtime payload',
      () =>
        ({ ...event('chat.text.delta', 1, { delta: 'must-not-apply' }), payload: null }) as never,
    ],
  ])(
    'recovers %s from the authoritative snapshot before mutating messages',
    async (_label, nextEvent) => {
      installMemoryCache()
      const generating = conversation(2, 2, {
        state: 'generating',
        generationId: 'generation-1',
      })
      const completed = conversation(3, 2, { state: 'idle', generationId: undefined })
      const baseline = { ...message(2), status: 'streaming' as const, content: 'baseline' }
      const authoritative = {
        ...message(2, 3),
        content: 'authoritative',
        generationId: 'generation-1',
      }
      let snapshots = 0
      const call = vi.fn(async (method: string) => {
        if (method === 'conversation.list') {
          return {
            items: [generating],
            changes: [],
            nextCursor: null,
            highWatermark: 2,
            resetRequired: true,
          }
        }
        if (method === 'conversation.get') {
          snapshots += 1
          return snapshots === 1
            ? {
                conversation: generating,
                messages: [baseline],
                nextCursor: null,
                highWatermark: 2,
                resetRequired: false,
                snapshotEventHighWatermark: 0,
              }
            : {
                conversation: completed,
                messages: [authoritative],
                nextCursor: null,
                highWatermark: 3,
                resetRequired: false,
                snapshotEventHighWatermark: 2,
              }
        }
        throw new Error(`unexpected ${method}`)
      }) as RemoteRPCClient['call']
      const detach = vi.fn(async () => undefined)
      const startStream = vi.fn(
        (
          _method: string,
          _input: Record<string, unknown>,
          onDelta: (delta: RemoteAIEvent) => void,
        ) => {
          onDelta(nextEvent())
          return { result: new Promise<unknown>(() => undefined), detach }
        },
      ) as unknown as RemoteRPCClient['startStream']

      const mounted = mountPanel(call, undefined, ref(true), startStream)
      await vi.waitFor(() => expect(snapshots).toBeGreaterThanOrEqual(2))
      await flushPromises()

      expect(mounted.wrapper.find('.message-list article p').text()).toBe('authoritative')
      expect(mounted.wrapper.text()).not.toContain('must-not-apply')
      expect(detach).toHaveBeenCalledTimes(1)
    },
  )

  it('renders reasoning, dialogue slices, and tool calls in durable time order', async () => {
    installMemoryCache()
    const activeConversation = conversation(3, 1)
    const timelineMessage: RemoteChatMessage = {
      ...message(2),
      content: '先说明。工具后继续。',
      reasoning: '先分析问题。',
      toolRuns: [
        {
          id: 'tool-later',
          name: 'read_file',
          description: '读取文件',
          status: 'succeeded',
          arguments: { path: 'README.md' },
          result: {},
          output: '文件内容',
          errorCode: '',
          contentOffset: 4,
          startedAt: '2026-08-08T00:00:02Z',
          finishedAt: '2026-08-08T00:00:03Z',
        },
      ],
    }
    const call = vi.fn(async (method: string) => {
      if (method === 'conversation.list') {
        return {
          items: [activeConversation],
          changes: [],
          nextCursor: null,
          highWatermark: 3,
          resetRequired: true,
        }
      }
      if (method === 'conversation.get') {
        return {
          conversation: activeConversation,
          messages: [timelineMessage],
          nextCursor: null,
          highWatermark: 2,
          resetRequired: false,
          snapshotEventHighWatermark: 0,
          earliestAvailableEventSequence: 0,
        }
      }
      throw new Error(`unexpected ${method}`)
    }) as RemoteRPCClient['call']

    const mounted = mountPanel(call)
    await flushPromises()

    const children = mounted.wrapper.find('.message-timeline').element.children
    expect([...children].map((node) => node.className)).toEqual([
      'message-reasoning',
      'message-content',
      'message-tool',
      'message-content',
    ])
    expect(children[0]?.hasAttribute('open')).toBe(false)
    expect(children[1]?.textContent?.trim()).toBe('先说明。')
    expect(children[2]?.textContent).toContain('读取文件 · 已完成')
    expect(children[2]?.hasAttribute('open')).toBe(false)
    expect(children[3]?.textContent?.trim()).toBe('工具后继续。')

    mounted.wrapper.unmount()
  })

  it('does not reload content for an availability hint while attach is healthy', async () => {
    installMemoryCache()
    const activeConversation = conversation(4, 2, {
      state: 'generating',
      generationId: 'generation-1',
    })
    let callCount = 0
    const call = vi.fn(async (method: string) => {
      callCount += 1
      if (method === 'conversation.list') {
        return {
          items: [activeConversation],
          changes: [],
          nextCursor: null,
          highWatermark: 4,
          resetRequired: true,
        }
      }
      if (method === 'conversation.get') {
        return {
          conversation: activeConversation,
          messages: [],
          nextCursor: null,
          highWatermark: 0,
          resetRequired: false,
          snapshotEventHighWatermark: 41,
        }
      }
      throw new Error(`unexpected ${method}`)
    }) as RemoteRPCClient['call']
    const startStream = vi.fn(() => ({
      result: new Promise<unknown>(() => undefined),
      detach: async () => undefined,
    })) as unknown as RemoteRPCClient['startStream']
    let emit: ((event: AgentStateEvent) => void | Promise<void>) | undefined
    const agentEvents: AgentEventConnection = {
      state: ref('ready'),
      error: ref(''),
      ready: ref(true),
      start: async () => undefined,
      close: async () => undefined,
      onEvent: (handler) => {
        emit = handler
        return () => undefined
      },
      onReset: () => () => undefined,
    }

    const mounted = mountPanel(call, undefined, ref(true), startStream, agentEvents)
    await flushPromises()
    const callsBeforeHint = callCount

    await emit?.({
      eventId: 'hint-1',
      sequence: 1,
      highWatermark: 1,
      schemaVersion: 1,
      projectId: '11111111-1111-4111-8111-111111111111',
      topic: 'message',
      type: 'conversation.events.available',
      aggregateType: 'conversation',
      aggregateId: 'conversation-1',
      operation: 'invalidate',
      revision: 4,
      cursor: { kind: 'agentEvent', value: 1 },
      data: { generationId: 'generation-1' },
    })
    await flushPromises()

    expect(callCount).toBe(callsBeforeHint)
    expect(startStream).toHaveBeenCalledTimes(1)
    mounted.wrapper.unmount()
  })

  it('bounds legacy event replay and falls back to an authoritative snapshot', async () => {
    installMemoryCache()
    const generating = conversation(4, 1, {
      state: 'generating',
      generationId: 'generation-1',
    })
    let emit: ((event: AgentStateEvent) => void | Promise<void>) | undefined
    const agentEvents: AgentEventConnection = {
      state: ref('ready'),
      error: ref(''),
      ready: ref(true),
      start: async () => undefined,
      close: async () => undefined,
      onEvent: (handler) => {
        emit = handler
        return () => undefined
      },
      onReset: () => () => undefined,
    }
    let snapshots = 0
    let eventPages = 0
    const call = vi.fn(async (method: string, input: Record<string, unknown> = {}) => {
      if (method === 'conversation.list') {
        return {
          items: [generating],
          changes: [],
          nextCursor: null,
          highWatermark: 4,
          resetRequired: true,
        }
      }
      if (method === 'conversation.get') {
        snapshots += 1
        return {
          conversation: generating,
          messages: [message(1)],
          nextCursor: null,
          highWatermark: 1,
          resetRequired: false,
          snapshotEventHighWatermark: 0,
        }
      }
      if (method === 'conversation.events') {
        eventPages += 1
        return {
          items: [],
          nextSequence: Number(input.afterSequence) + 1,
          highWatermark: 100,
          hasMore: true,
          resetRequired: false,
        }
      }
      throw new Error(`unexpected ${method}`)
    }) as RemoteRPCClient['call']
    const legacyCapabilities: RemoteAgentCapabilities = {
      protocolMinimum: 1,
      protocolMaximum: 1,
      featureVersions: { projects: 2, ai: 2 },
      features: { 'project.v2': true, 'ai.v2': true },
      operatingSystem: 'windows',
      architecture: 'amd64',
      shells: ['powershell'],
      taskRunners: ['script'],
      resourceLimits: {},
    }

    const mounted = mountPanel(
      call,
      undefined,
      ref(true),
      idleStartStream,
      agentEvents,
      legacyCapabilities,
    )
    await flushPromises()
    expect(snapshots).toBe(1)

    await emit?.({
      eventId: 'legacy-hint',
      sequence: 1,
      highWatermark: 1,
      schemaVersion: 1,
      projectId: '11111111-1111-4111-8111-111111111111',
      topic: 'message',
      type: 'conversation.events.available',
      aggregateType: 'conversation',
      aggregateId: 'conversation-1',
      operation: 'invalidate',
      revision: 4,
      cursor: { kind: 'agentEvent', value: 1 },
      data: { generationId: 'generation-1' },
    })
    await flushPromises()

    expect(eventPages).toBe(16)
    expect(snapshots).toBe(2)
    expect(mounted.wrapper.get('[role="alert"]').text()).toContain(
      '旧版对话事件回放页数超过客户端安全上限',
    )
    mounted.wrapper.unmount()
  })

  it('stops legacy event replay as soon as its sequence cursor stalls', async () => {
    installMemoryCache()
    const generating = conversation(4, 1, {
      state: 'generating',
      generationId: 'generation-1',
    })
    let emit: ((event: AgentStateEvent) => void | Promise<void>) | undefined
    const agentEvents: AgentEventConnection = {
      state: ref('ready'),
      error: ref(''),
      ready: ref(true),
      start: async () => undefined,
      close: async () => undefined,
      onEvent: (handler) => {
        emit = handler
        return () => undefined
      },
      onReset: () => () => undefined,
    }
    let snapshots = 0
    let eventPages = 0
    const call = vi.fn(async (method: string, input: Record<string, unknown> = {}) => {
      if (method === 'conversation.list') {
        return {
          items: [generating],
          changes: [],
          nextCursor: null,
          highWatermark: 4,
          resetRequired: true,
        }
      }
      if (method === 'conversation.get') {
        snapshots += 1
        return {
          conversation: generating,
          messages: [message(1)],
          nextCursor: null,
          highWatermark: 1,
          resetRequired: false,
          snapshotEventHighWatermark: 0,
        }
      }
      if (method === 'conversation.events') {
        eventPages += 1
        return {
          items: [],
          nextSequence: input.afterSequence,
          highWatermark: 100,
          hasMore: true,
          resetRequired: false,
        }
      }
      throw new Error(`unexpected ${method}`)
    }) as RemoteRPCClient['call']
    const legacyCapabilities: RemoteAgentCapabilities = {
      protocolMinimum: 1,
      protocolMaximum: 1,
      featureVersions: { projects: 2, ai: 2 },
      features: { 'project.v2': true, 'ai.v2': true },
      operatingSystem: 'windows',
      architecture: 'amd64',
      shells: ['powershell'],
      taskRunners: ['script'],
      resourceLimits: {},
    }
    const mounted = mountPanel(
      call,
      undefined,
      ref(true),
      idleStartStream,
      agentEvents,
      legacyCapabilities,
    )
    await flushPromises()

    await emit?.({
      eventId: 'stalled-legacy-hint',
      sequence: 1,
      highWatermark: 1,
      schemaVersion: 1,
      projectId: '11111111-1111-4111-8111-111111111111',
      topic: 'message',
      type: 'conversation.events.available',
      aggregateType: 'conversation',
      aggregateId: 'conversation-1',
      operation: 'invalidate',
      revision: 4,
      cursor: { kind: 'agentEvent', value: 1 },
      data: { generationId: 'generation-1' },
    })
    await flushPromises()

    expect(eventPages).toBe(1)
    expect(snapshots).toBe(2)
    mounted.wrapper.unmount()
  })

  it('pages 100+ messages with a stable keyset and catches a concurrent message on reconnect', async () => {
    const merge = installMemoryCache()
    const connected = ref(true)
    let concurrentMessageCreated = false
    const calls: Array<{ method: string; input: Record<string, unknown> }> = []
    const call = vi.fn(async (method: string, input: Record<string, unknown> = {}) => {
      calls.push({ method, input })
      if (method === 'conversation.list') {
        if (input.afterSequence === 1) {
          return {
            items: [],
            changes: [{ sequence: 2, value: conversation(2, 121) }],
            nextCursor: null,
            highWatermark: 2,
            resetRequired: false,
          }
        }
        return {
          items: [conversation(1, 120)],
          changes: [],
          nextCursor: null,
          highWatermark: 1,
          resetRequired: true,
        }
      }
      if (method !== 'conversation.messages.before') throw new Error(`unexpected ${method}`)

      if (input.cursor === 'before:71') {
        concurrentMessageCreated = true
        return {
          items: Array.from({ length: 50 }, (_, index) => message(index + 21)),
          nextCursor: 'before:21',
          highWatermark: 121,
          resetRequired: false,
        }
      }
      if (input.cursor === 'before:21') {
        return {
          items: Array.from({ length: 20 }, (_, index) => message(index + 1)),
          nextCursor: null,
          highWatermark: 121,
          resetRequired: false,
        }
      }
      if (input.afterSequence === 120 && concurrentMessageCreated) {
        return {
          items: [],
          changes: [{ sequence: 121, value: message(121) }],
          nextCursor: null,
          highWatermark: 121,
          resetRequired: false,
        }
      }
      return {
        items: Array.from({ length: 50 }, (_, index) => message(index + 71)),
        changes: [],
        nextCursor: 'before:71',
        highWatermark: 120,
        resetRequired: true,
      }
    }) as RemoteRPCClient['call']

    const mounted = mountPanel(call, undefined, connected)
    await flushPromises()

    expect(mounted.wrapper.findAll('.message-list article')).toHaveLength(50)
    await buttonWithText(mounted.wrapper, '加载更早消息').trigger('click')
    await flushPromises()
    await buttonWithText(mounted.wrapper, '加载更早消息').trigger('click')
    await flushPromises()

    expect(mounted.wrapper.findAll('.message-list article')).toHaveLength(120)
    expect(
      calls
        .filter(({ method }) => method === 'conversation.messages.before')
        .map(({ input }) => input.cursor),
    ).toEqual([undefined, 'before:71', 'before:21'])

    connected.value = false
    await nextTick()
    connected.value = true
    await flushPromises()

    const rendered = mounted.wrapper.findAll('.message-list article p').map((node) => node.text())
    expect(rendered).toHaveLength(121)
    expect(new Set(rendered).size).toBe(121)
    expect(rendered).toEqual(Array.from({ length: 121 }, (_, index) => `message-${index + 1}`))
    expect(mounted.wrapper.text()).toContain('121 条 · model-a')
    expect(
      calls.some(
        ({ method, input }) =>
          method === 'conversation.messages.before' && input.afterSequence === 120,
      ),
    ).toBe(true)
    expect(merge).toHaveBeenCalledWith(
      projectPartition('messages:conversation-1'),
      expect.objectContaining({
        highWatermark: 121,
        changes: [expect.objectContaining({ sequence: 121 })],
      }),
    )
  })

  it('applies tombstones, replaces stale messages on reset, and retries a reconnect gap from zero', async () => {
    installMemoryCache()
    cacheStates.set(projectPartition('conversations'), {
      records: [
        conversation(5, 1, { id: 'removed', title: '应删除' }),
        conversation(5, 1, { id: 'conversation-1', title: '保留' }),
      ],
      highWatermark: 5,
    })
    cacheStates.set(projectPartition('messages:conversation-1'), {
      records: [message(1, 10)],
      highWatermark: 10,
    })

    const connected = ref(true)
    let reconnecting = false
    const conversationSequences: number[] = []
    const call = vi.fn(async (method: string, input: Record<string, unknown> = {}) => {
      if (method === 'conversation.list') {
        conversationSequences.push(input.afterSequence as number)
        if (!reconnecting) {
          return {
            items: [],
            changes: [
              {
                sequence: 6,
                deleted: true,
                id: 'removed',
                revision: 6,
              },
            ],
            nextCursor: null,
            highWatermark: 6,
            resetRequired: false,
          }
        }
        if (input.afterSequence === 6) {
          return {
            items: [],
            changes: [{ sequence: 8, value: conversation(8, 1, { title: '不应直接合并' }) }],
            nextCursor: null,
            highWatermark: 8,
            resetRequired: false,
          }
        }
        return {
          items: [conversation(8, 1, { title: '重连后快照' })],
          changes: [],
          nextCursor: null,
          highWatermark: 8,
          resetRequired: true,
        }
      }
      if (method === 'conversation.messages.before') {
        if (input.afterSequence === 10) {
          return {
            items: [message(11, 11)],
            changes: [],
            nextCursor: null,
            highWatermark: 11,
            resetRequired: true,
          }
        }
        return {
          items: [],
          changes: [],
          nextCursor: null,
          highWatermark: 11,
          resetRequired: false,
        }
      }
      throw new Error(`unexpected ${method}`)
    }) as RemoteRPCClient['call']

    const mounted = mountPanel(call, undefined, connected)
    await flushPromises()

    expect(mounted.wrapper.text()).not.toContain('应删除')
    expect(mounted.wrapper.text()).toContain('保留')
    expect(mounted.wrapper.findAll('.message-list article p').map((node) => node.text())).toEqual([
      'message-11',
    ])
    expect(
      cacheStates
        .get(projectPartition('conversations'))
        ?.records.some(({ id }) => id === 'removed'),
    ).toBe(false)

    reconnecting = true
    connected.value = false
    await nextTick()
    connected.value = true
    await flushPromises()

    expect(conversationSequences).toEqual([5, 6, 0])
    expect(mounted.wrapper.text()).toContain('重连后快照')
    expect(mounted.wrapper.text()).not.toContain('不应直接合并')
  })

  it('reconciles streamed messages and refreshes the conversation summary after send', async () => {
    installMemoryCache()
    let sent = false
    const call = vi.fn(async (method: string, input: Record<string, unknown> = {}) => {
      if (method === 'conversation.list') {
        if (sent) {
          return {
            items: [],
            changes: [{ sequence: 2, value: conversation(2, 2) }],
            nextCursor: null,
            highWatermark: 2,
            resetRequired: false,
          }
        }
        return {
          items: [conversation(1, 0)],
          changes: [],
          nextCursor: null,
          highWatermark: 1,
          resetRequired: true,
        }
      }
      if (method === 'conversation.messages.before') {
        if (sent) {
          return {
            items: [],
            changes: [
              { sequence: 1, value: message(1) },
              { sequence: 2, value: message(2) },
            ],
            nextCursor: null,
            highWatermark: 2,
            resetRequired: false,
          }
        }
        return {
          items: [],
          changes: [],
          nextCursor: null,
          highWatermark: 0,
          resetRequired: true,
        }
      }
      throw new Error(`unexpected ${method}: ${JSON.stringify(input)}`)
    }) as RemoteRPCClient['call']
    const stream = vi.fn(
      async (
        _method: string,
        _input: Record<string, unknown>,
        onDelta: (delta: RemoteAIEvent) => void,
      ) => {
        onDelta(event('chat.text.delta', 1, { delta: 'message-2' }))
        onDelta(event('chat.completed', 2, { message: message(2) }))
        sent = true
      },
    ) as RemoteRPCClient['stream']

    const mounted = mountPanel(call, stream)
    await flushPromises()
    await mounted.wrapper.get('textarea').setValue('hello')
    await mounted.wrapper.get('form').trigger('submit')
    await flushPromises()

    expect(stream).toHaveBeenCalledWith(
      'conversation.send',
      expect.objectContaining({
        conversationId: 'conversation-1',
        content: 'hello',
        workspaceMode: 'readOnly',
        enableWorkspaceTools: false,
        messageId: expect.any(String),
      }),
      expect.any(Function),
      { projectId: '11111111-1111-4111-8111-111111111111' },
    )
    expect(mounted.wrapper.findAll('.message-list article')).toHaveLength(2)
    expect(mounted.wrapper.text()).toContain('2 条 · model-a')
    expect(call).toHaveBeenCalledWith(
      'conversation.list',
      expect.objectContaining({ afterSequence: 1, afterRevision: 1 }),
      { projectId: '11111111-1111-4111-8111-111111111111' },
    )
  })

  it('does not restore a draft when the authoritative snapshot proves an unknown send committed', async () => {
    installMemoryCache()
    let committedMessageId = ''
    const committedMessage = (): RemoteChatMessage => ({
      ...message(1),
      id: committedMessageId,
      role: 'user',
      content: 'hello across disconnect',
    })
    const call = vi.fn(async (method: string) => {
      const committed = committedMessageId.length > 0
      if (method === 'conversation.list') {
        return {
          items: [conversation(committed ? 2 : 1, committed ? 1 : 0)],
          changes: [],
          nextCursor: null,
          highWatermark: committed ? 2 : 1,
          resetRequired: true,
        }
      }
      if (method === 'conversation.get') {
        return {
          conversation: conversation(committed ? 2 : 1, committed ? 1 : 0),
          messages: committed ? [committedMessage()] : [],
          nextCursor: null,
          highWatermark: committed ? 1 : 0,
          resetRequired: false,
          snapshotEventHighWatermark: 0,
        }
      }
      throw new Error(`unexpected ${method}`)
    }) as RemoteRPCClient['call']
    const stream = vi.fn(async (_method: string, input: Record<string, unknown>) => {
      committedMessageId = String(input.messageId)
      throw new Error('Carrier closed before the first event')
    }) as RemoteRPCClient['stream']
    const mounted = mountPanel(call, stream)
    await flushPromises()

    await mounted.wrapper.get('textarea').setValue('hello across disconnect')
    await mounted.wrapper.get('form').trigger('submit')
    await flushPromises()

    expect(committedMessageId).toMatch(/^[0-9a-f-]{36}$/u)
    expect(mounted.wrapper.get('textarea').element.value).toBe('')
    expect(mounted.wrapper.findAll('.message-list article')).toHaveLength(1)
    expect(mounted.wrapper.text()).toContain('hello across disconnect')
  })

  it('releases the composer on agent_tool_limit before the RPC response settles', async () => {
    installMemoryCache()
    let terminal = false
    const failedMessage: RemoteChatMessage = {
      ...message(2),
      role: 'assistant',
      status: 'failed',
      errorCode: 'agent_tool_limit',
      generationId: 'generation-1',
    }
    const activeConversation = () =>
      conversation(terminal ? 2 : 1, terminal ? 2 : 0, {
        state: terminal ? 'failed' : 'idle',
      })
    const call = vi.fn(async (method: string) => {
      if (method === 'conversation.list') {
        return {
          items: [activeConversation()],
          changes: [],
          nextCursor: null,
          highWatermark: terminal ? 2 : 1,
          resetRequired: true,
        }
      }
      if (method === 'conversation.get') {
        return {
          conversation: activeConversation(),
          messages: terminal ? [message(1), failedMessage] : [],
          nextCursor: null,
          highWatermark: terminal ? 2 : 0,
          resetRequired: false,
          snapshotEventHighWatermark: terminal ? 2 : 0,
        }
      }
      throw new Error(`unexpected ${method}`)
    }) as RemoteRPCClient['call']
    let emit: ((value: RemoteAIEvent) => void) | undefined
    let settleStream: ((error?: Error) => void) | undefined
    const stream = vi.fn(
      async (
        _method: string,
        _input: Record<string, unknown>,
        onDelta: (value: RemoteAIEvent) => void,
      ) => {
        emit = onDelta
        await new Promise<void>((resolve, reject) => {
          settleStream = (error?: Error) => (error ? reject(error) : resolve())
        })
      },
    ) as RemoteRPCClient['stream']
    const mounted = mountPanel(call, stream, ref(true), idleStartStream, undefined, undefined, {
      toolsAuthorized: true,
    })
    await flushPromises()
    await mounted.wrapper.get('textarea').setValue('hello')
    void mounted.wrapper.get('.chat-composer').trigger('submit')
    await flushPromises()

    const approval = {
      id: 'approval-1',
      conversationId: 'conversation-1',
      generationId: 'generation-1',
      messageId: 'message-2',
      toolCallId: 'tool-call-1',
      toolName: 'terminal_execute',
      preview: { title: '运行命令？', description: '测试审批' },
      expiresAt: '2026-08-08T00:05:00Z',
      allowForSession: true,
    }
    emit?.(event('chat.approval.requested', 1, { approval }))
    await nextTick()
    expect(mounted.wrapper.find('.agent-approval').exists()).toBe(true)
    expect(mounted.wrapper.find('textarea').exists()).toBe(false)

    terminal = true
    emit?.(
      event('chat.failed', 2, {
        message: failedMessage,
        errorCode: 'agent_tool_limit',
      }),
    )
    await flushPromises()

    expect(mounted.wrapper.find('.agent-approval').exists()).toBe(false)
    expect(mounted.wrapper.get('textarea').isVisible()).toBe(true)
    expect(mounted.wrapper.get('textarea').element.value).toBe('')

    settleStream?.(new Error('agent_tool_limit'))
    await flushPromises()
    expect(mounted.wrapper.get('textarea').element.value).toBe('')
    await mounted.wrapper.get('textarea').setValue('retry')
    expect(buttonWithText(mounted.wrapper, '发送').attributes('disabled')).toBeUndefined()
  })

  it('projects durable Plan, Todo, Goal, and child-Agent state from a v8 device', async () => {
    installMemoryCache()
    const goal = {
      id: 'goal-1',
      revision: 3,
      objective: '完成远程 Agent 协作界面',
      phase: 'active' as const,
      roundsStarted: 2,
      maxGoalRounds: 12,
      createdAt: '2026-08-08T00:00:00Z',
      updatedAt: '2026-08-08T00:00:03Z',
    }
    const parent = conversation(4, 1, {
      planModeActive: true,
      todos: [
        { content: '梳理协议', status: 'completed' as const },
        { content: '渲染协作状态', status: 'in_progress' as const },
      ],
      goal,
      goalArmed: true,
    })
    const child = conversation(2, 1, {
      id: 'child-1',
      title: '渲染协作状态',
      subagent: {
        parentConversationId: parent.id,
        label: '界面子任务',
        depth: 1,
        status: 'running',
        background: true,
        createdAt: '2026-08-08T00:00:01Z',
        updatedAt: '2026-08-08T00:00:02Z',
        summary: '正在整理展示状态。',
      },
    })
    const calls: Array<{ method: string; input: Record<string, unknown> }> = []
    const call = vi.fn(async (method: string, input: Record<string, unknown> = {}) => {
      calls.push({ method, input })
      if (method === 'conversation.list') {
        return {
          items: [parent],
          changes: [],
          nextCursor: null,
          highWatermark: 4,
          resetRequired: true,
        }
      }
      if (method === 'conversation.get') {
        return {
          conversation: parent,
          messages: [message(1)],
          nextCursor: null,
          highWatermark: 1,
          resetRequired: false,
          snapshotEventHighWatermark: 1,
        }
      }
      if (method === 'conversation.subagents.list') {
        return { items: [child], nextCursor: null, highWatermark: 4 }
      }
      if (method === 'conversation.plan.set') {
        return { ...parent, planModeActive: input.active === true }
      }
      throw new Error(`unexpected ${method}`)
    }) as RemoteRPCClient['call']
    const capabilities: RemoteAgentCapabilities = {
      protocolMinimum: 1,
      protocolMaximum: 1,
      featureVersions: { projects: 2, ai: 8 },
      features: {
        'project.v2': true,
        'ai.v2': true,
        'ai.planMode': true,
        'ai.todo': true,
        'ai.subagents': true,
        'ai.goal': true,
      },
      operatingSystem: 'windows',
      architecture: 'amd64',
      shells: ['powershell'],
      taskRunners: ['script'],
      resourceLimits: {},
    }

    const mounted = mountPanel(call, undefined, ref(true), idleStartStream, undefined, capabilities)
    await flushPromises()

    expect(mounted.wrapper.text()).toContain('Plan 模式')
    expect(mounted.wrapper.text()).toContain('梳理协议')
    expect(mounted.wrapper.text()).toContain('完成远程 Agent 协作界面')
    expect(mounted.wrapper.text()).toContain('界面子任务 · 运行中')
    await buttonWithText(mounted.wrapper, '退出 Plan').trigger('click')
    await flushPromises()

    expect(calls).toContainEqual({
      method: 'conversation.plan.set',
      input: { conversationId: parent.id, active: false },
    })
  })
})
