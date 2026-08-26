import { describe, expect, it } from 'vitest'

import {
  REMOTE_COLLABORATION_EVENT_CONTRACT_VERSION,
  REMOTE_COLLABORATION_EVENT_KINDS,
  withRemoteEventContract,
} from './eventContract'

describe('remote event contract negotiation', () => {
  it.each([
    'event.subscribe',
    'conversation.events',
    'conversation.generation.attach',
    'conversation.send',
    'conversation.chat.send',
    'chat.send',
  ])('adds the collaboration contract to %s', (method) => {
    const input = { conversationId: 'conversation-1' }

    expect(withRemoteEventContract(method, input)).toEqual({
      conversationId: 'conversation-1',
      eventContractVersion: REMOTE_COLLABORATION_EVENT_CONTRACT_VERSION,
      acceptedEventKinds: [...REMOTE_COLLABORATION_EVENT_KINDS],
    })
    expect(input).toEqual({ conversationId: 'conversation-1' })
  })

  it('leaves unrelated RPC payloads unchanged', () => {
    const input = { conversationId: 'conversation-1' }
    expect(withRemoteEventContract('conversation.get', input)).toBe(input)
  })
})
