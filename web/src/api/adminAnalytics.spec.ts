import { beforeEach, describe, expect, it, vi } from 'vitest'

import { getAdminAnalyticsOverview, listAdminLoginEvents } from './adminAnalytics'
import { apiClient } from './client'

vi.mock('./client', () => ({ apiClient: { get: vi.fn() } }))

describe('admin analytics API', () => {
  beforeEach(() => vi.clearAllMocks())

  it('sends the selected range and paginated login search', async () => {
    const range = { from: '2026-07-01T16:00:00.000Z', to: '2026-08-01T16:00:00.000Z' }
    const overview = { ...range, granularity: 'day' as const }
    vi.mocked(apiClient.get)
      .mockResolvedValueOnce({ data: { summary: { pageViews: 12 } } })
      .mockResolvedValueOnce({ data: { items: [], total: 0, limit: 50, offset: 0 } })

    await getAdminAnalyticsOverview(overview)
    await listAdminLoginEvents({ ...range, q: 'member@example.test', limit: 50, offset: 0 })

    expect(apiClient.get).toHaveBeenNthCalledWith(1, '/admin/analytics/overview', {
      params: overview,
    })
    expect(apiClient.get).toHaveBeenNthCalledWith(2, '/admin/analytics/login-events', {
      params: { ...range, q: 'member@example.test', limit: 50, offset: 0 },
    })
  })
})
