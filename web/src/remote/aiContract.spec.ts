import { describe, expect, it } from 'vitest'

import type { RemoteAIEvent, RemoteChatMessage } from './rpcTypes'
import { isRemoteAIEvent } from './aiContract'

const message: RemoteChatMessage = {
  id: 'message-1',
  revision: 2,
  sequence: 2,
  role: 'assistant',
  content: 'done',
  status: 'complete',
  errorCode: '',
  attachments: [],
  reasoning: '',
  toolRuns: [],
  usage: {
    inputTokens: 1,
    outputTokens: 1,
    reasoningTokens: 0,
    cachedInputTokens: 0,
    totalTokens: 2,
  },
  providerRun: {
    provider: 'openai',
    model: 'model',
    providerRequestId: '',
    finishReason: 'stop',
    attemptCount: 1,
  },
  createdAt: '2026-08-25T00:00:00Z',
  generationId: 'generation-1',
}

const event = (overrides: Partial<RemoteAIEvent> = {}): RemoteAIEvent => ({
  eventId: 'event-1',
  conversationId: 'conversation-1',
  generationId: 'generation-1',
  messageId: 'message-1',
  kind: 'chat.text.delta',
  sequence: 1,
  payload: { delta: 'hello' },
  occurredAt: '2026-08-25T00:00:00Z',
  ...overrides,
})

describe('AI conversation runtime contract', () => {
  it('accepts valid deltas and terminal message references', () => {
    expect(isRemoteAIEvent(event())).toBe(true)
    expect(
      isRemoteAIEvent(
        event({
          kind: 'chat.plan_mode.changed',
          generationId: '',
          messageId: '',
          payload: { active: true },
        }),
      ),
    ).toBe(true)
    expect(
      isRemoteAIEvent(event({ kind: 'chat.completed', sequence: 2, payload: { message } })),
    ).toBe(true)
    expect(
      isRemoteAIEvent(
        event({
          kind: 'chat.failed',
          sequence: 2,
          payload: {
            messageRef: {
              id: 'message-1',
              revision: 2,
              sequence: 2,
              status: 'failed',
              generationId: 'generation-1',
            },
          },
        }),
      ),
    ).toBe(true)
  })

  it.each([
    null,
    event({ sequence: 0 }),
    event({ payload: null as never }),
    event({ kind: 'chat.text.delta', payload: { delta: 42 as never } }),
    event({ kind: 'chat.usage', payload: { usage: { totalTokens: Number.NaN } as never } }),
    event({ kind: 'chat.completed', payload: {} }),
    event({ generationId: '' }),
    event({ kind: 'unknown' as never }),
    event({ occurredAt: 'not-a-time' }),
  ])('rejects malformed envelopes without applying a partial shape', (candidate) => {
    expect(isRemoteAIEvent(candidate)).toBe(false)
  })
})
