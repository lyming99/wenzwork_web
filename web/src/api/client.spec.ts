import type { AxiosResponse } from 'axios'
import { afterEach, describe, expect, it, vi } from 'vitest'

import { apiClient, authenticationFailureEvent } from './client'

const inspectRequest = async (method: 'get' | 'post') => {
  const response = await apiClient.request({
    url: '/request-inspection',
    method,
    data: method === 'post' ? {} : undefined,
    adapter: async (config): Promise<AxiosResponse> => ({
      data: null,
      status: 200,
      statusText: 'OK',
      headers: {},
      config,
    }),
  })
  return response.config
}

describe('API client security headers', () => {
  afterEach(() => {
    document.cookie = 'wenzwork_csrf=; Max-Age=0; Path=/'
  })

  it('copies the readable CSRF cookie only to mutating requests', async () => {
    document.cookie = 'wenzwork_csrf=session-bound-token; Path=/'

    const post = await inspectRequest('post')
    const get = await inspectRequest('get')

    expect(post.headers.get('X-CSRF-Token')).toBe('session-bound-token')
    expect(get.headers.get('X-CSRF-Token')).toBeUndefined()
  })

  it('announces only session-related 401 responses', async () => {
    const listener = vi.fn()
    window.addEventListener(authenticationFailureEvent, listener)
    try {
      await expect(
        apiClient.get('/protected', {
          adapter: async (config) =>
            Promise.reject({
              isAxiosError: true,
              config,
              response: {
                data: { code: 'authentication_required' },
                status: 401,
                statusText: 'Unauthorized',
                headers: {},
                config,
              },
            }),
        }),
      ).rejects.toMatchObject({ response: { status: 401 } })

      expect(listener).toHaveBeenCalledTimes(1)
    } finally {
      window.removeEventListener(authenticationFailureEvent, listener)
    }
  })

  it('does not announce a rejected login credential', async () => {
    const listener = vi.fn()
    window.addEventListener(authenticationFailureEvent, listener)
    try {
      await expect(
        apiClient.post(
          '/auth/login',
          {},
          {
            adapter: async (config) =>
              Promise.reject({
                isAxiosError: true,
                config,
                response: {
                  data: { code: 'invalid_credentials' },
                  status: 401,
                  statusText: 'Unauthorized',
                  headers: {},
                  config,
                },
              }),
          },
        ),
      ).rejects.toMatchObject({ response: { status: 401 } })

      expect(listener).not.toHaveBeenCalled()
    } finally {
      window.removeEventListener(authenticationFailureEvent, listener)
    }
  })
})
