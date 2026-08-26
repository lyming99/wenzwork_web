import { afterEach, describe, expect, it, vi } from 'vitest'

import {
  claimRemoteManagerWindowName,
  openRemoteManagerWindow,
  REMOTE_MANAGER_WINDOW_NAME,
} from './remoteManagerWindow'

describe('remote manager window', () => {
  afterEach(() => {
    vi.restoreAllMocks()
    window.name = ''
  })

  it('opens and focuses a stable named Chrome popup', () => {
    const focus = vi.fn()
    const open = vi.spyOn(window, 'open').mockReturnValue({ focus } as unknown as Window)

    expect(openRemoteManagerWindow('/remote')).not.toBeNull()
    expect(open).toHaveBeenCalledWith(
      '/remote',
      REMOTE_MANAGER_WINDOW_NAME,
      expect.stringMatching(/popup=yes.*width=\d+.*height=\d+.*resizable=yes.*scrollbars=yes/),
    )
    expect(focus).toHaveBeenCalledOnce()
  })

  it('claims the same browsing-context name inside the standalone app', () => {
    claimRemoteManagerWindowName()
    expect(window.name).toBe(REMOTE_MANAGER_WINDOW_NAME)
  })
})
