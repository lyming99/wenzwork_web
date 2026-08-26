import { beforeEach, describe, expect, it } from 'vitest'

import {
  agentEventCursorKey,
  clearStoredAgentEventCursors,
  readAgentEventCursor,
  removeAgentEventCursor,
  storeAgentEventCursor,
} from './agentEventCursor'

describe('Agent event cursor storage', () => {
  beforeEach(() => localStorage.clear())

  it('stores only the processed sequence and removes all cursors on account cleanup', () => {
    storeAgentEventCursor('device-1', 'project-1', 42)
    localStorage.setItem('unrelated-key', 'preserved')

    expect(readAgentEventCursor('device-1', 'project-1')).toBe(42)
    expect(localStorage.getItem(agentEventCursorKey('device-1', 'project-1'))).toContain('42')

    clearStoredAgentEventCursors()

    expect(readAgentEventCursor('device-1', 'project-1')).toBeUndefined()
    expect(localStorage.getItem('unrelated-key')).toBe('preserved')
  })

  it('removes an individual revoked-device/project cursor', () => {
    storeAgentEventCursor('device-1', 'project-1', 7)
    storeAgentEventCursor('device-1', 'project-2', 8)

    removeAgentEventCursor('device-1', 'project-1')

    expect(readAgentEventCursor('device-1', 'project-1')).toBeUndefined()
    expect(readAgentEventCursor('device-1', 'project-2')).toBe(8)
  })
})
