import { beforeEach, describe, expect, it, vi } from 'vitest'

import { apiClient } from './client'
import { getLatestRelease, isReleaseNotFound, listPricingPlans, listReleases } from './catalog'

vi.mock('./client', () => ({
  apiClient: { get: vi.fn() },
}))

const mockedGet = vi.mocked(apiClient.get)

describe('public catalog client', () => {
  beforeEach(() => mockedGet.mockReset())

  it('unwraps pricing and release list responses', async () => {
    mockedGet
      .mockResolvedValueOnce({ data: { items: [{ code: 'free' }] } })
      .mockResolvedValueOnce({ data: { items: [{ version: '1.0.0' }] } })

    await expect(listPricingPlans()).resolves.toEqual([{ code: 'free' }])
    await expect(listReleases('mobile', 5)).resolves.toEqual([{ version: '1.0.0' }])
    expect(mockedGet).toHaveBeenLastCalledWith('/releases', {
      params: { project: 'mobile', channel: 'stable', limit: 5 },
      headers: { 'Cache-Control': 'no-cache', Pragma: 'no-cache' },
    })
  })

  it('requests the stable latest release', async () => {
    mockedGet.mockResolvedValueOnce({ data: { version: '1.0.0' } })
    await expect(getLatestRelease('web')).resolves.toEqual({ version: '1.0.0' })
    expect(mockedGet).toHaveBeenCalledWith('/releases/latest', {
      params: { project: 'web', channel: 'stable' },
      headers: { 'Cache-Control': 'no-cache', Pragma: 'no-cache' },
    })
  })

  it('recognizes only the stable release-not-found problem', () => {
    expect(
      isReleaseNotFound({
        isAxiosError: true,
        response: { status: 404, data: { code: 'release_not_found' } },
      }),
    ).toBe(true)
    expect(
      isReleaseNotFound({
        isAxiosError: true,
        response: { status: 503, data: { code: 'catalog_unavailable' } },
      }),
    ).toBe(false)
  })
})
