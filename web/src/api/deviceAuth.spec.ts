import { beforeEach, describe, expect, it, vi } from 'vitest'

import { apiClient } from './client'
import {
  approveDeviceAuthorization,
  denyDeviceAuthorization,
  getDeviceAuthorizationForBrowser,
} from './deviceAuth'

vi.mock('./client', () => ({
  apiClient: { get: vi.fn(), post: vi.fn() },
}))

describe('device authorization browser API', () => {
  beforeEach(() => vi.clearAllMocks())

  it('loads and explicitly decides a desktop login request', async () => {
    const authorization = { requestId: 'request-1', status: 'pending' }
    vi.mocked(apiClient.get).mockResolvedValueOnce({ data: authorization })
    vi.mocked(apiClient.post)
      .mockResolvedValueOnce({ data: { ...authorization, status: 'approved' } })
      .mockResolvedValueOnce({ data: null })

    await expect(getDeviceAuthorizationForBrowser('ABCD-EFGH')).resolves.toEqual(authorization)
    expect(apiClient.get).toHaveBeenCalledWith('/oauth/device-authorization', {
      params: { userCode: 'ABCD-EFGH' },
    })

    await approveDeviceAuthorization('ABCD-EFGH')
    expect(apiClient.post).toHaveBeenNthCalledWith(1, '/oauth/device-authorization/approve', {
      userCode: 'ABCD-EFGH',
    })

    await denyDeviceAuthorization('ABCD-EFGH')
    expect(apiClient.post).toHaveBeenNthCalledWith(2, '/oauth/device-authorization/deny', {
      userCode: 'ABCD-EFGH',
    })
  })
})
