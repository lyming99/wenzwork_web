import type { Router } from 'vue-router'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import { installPageViewTracking, recordPageView } from './pageView'

describe('page view tracking', () => {
  beforeEach(() => {
    vi.restoreAllMocks()
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue({ ok: true, status: 204 }))
  })

  it('sends only the route path and lets the server reduce the referrer', async () => {
    await recordPageView('/pricing', 'https://search.example/results?q=secret')

    expect(fetch).toHaveBeenCalledWith(
      '/api/v1/analytics/page-view',
      expect.objectContaining({
        method: 'POST',
        credentials: 'omit',
        keepalive: true,
        body: JSON.stringify({
          path: '/pricing',
          referrer: 'https://search.example/results?q=secret',
        }),
      }),
    )
  })

  it('records public and account routes but excludes administrator traffic', async () => {
    let afterEach: ((to: { path: string }, from: unknown, failure?: unknown) => void) | undefined
    const router = {
      afterEach: vi.fn((handler) => {
        afterEach = handler
      }),
    } as unknown as Router
    installPageViewTracking(router)

    afterEach?.({ path: '/help' }, {})
    afterEach?.({ path: '/admin/analytics' }, {})
    await Promise.resolve()

    expect(fetch).toHaveBeenCalledTimes(1)
    expect(fetch).toHaveBeenCalledWith(
      '/api/v1/analytics/page-view',
      expect.objectContaining({ body: expect.stringContaining('"/help"') }),
    )
  })
})
