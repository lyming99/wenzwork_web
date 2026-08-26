import axios, { type AxiosInstance } from 'axios'

const csrfCookieNames = ['__Host-wenzwork_csrf', 'wenzwork_csrf']
const authenticationFailureCodes = new Set(['authentication_required', 'session_expired'])

export const authenticationFailureEvent = 'wenzwork:authentication-failure'

interface ProblemResponse {
  code?: string
}

const readCookie = (name: string) => {
  if (typeof document === 'undefined') return undefined

  const prefix = `${encodeURIComponent(name)}=`
  const item = document.cookie.split('; ').find((cookie) => cookie.startsWith(prefix))
  if (!item) return undefined

  try {
    return decodeURIComponent(item.slice(prefix.length))
  } catch {
    return undefined
  }
}

const configureAPIClient = (baseURL: string): AxiosInstance => {
  const client = axios.create({
    baseURL,
    timeout: 10_000,
    withCredentials: true,
    headers: {
      Accept: 'application/json',
    },
  })
  client.interceptors.request.use((config) => {
    const method = config.method?.toLowerCase()
    if (!method || !['post', 'put', 'patch', 'delete'].includes(method)) return config

    const csrfToken = csrfCookieNames.map(readCookie).find(Boolean)
    if (csrfToken) config.headers.set('X-CSRF-Token', csrfToken)
    return config
  })
  client.interceptors.response.use(undefined, (error: unknown) => {
    if (
      typeof window !== 'undefined' &&
      axios.isAxiosError<ProblemResponse>(error) &&
      error.response?.status === 401 &&
      authenticationFailureCodes.has(error.response.data?.code ?? '')
    ) {
      window.dispatchEvent(new Event(authenticationFailureEvent))
    }
    return Promise.reject(error)
  })
  return client
}

export const apiClient = configureAPIClient('/api/v1')

// remote/v2 intentionally has a separate base path.  Reusing the v1 client
// would silently turn `/remote/...` into `/api/v1/remote/...` and make a
// protocol fallback look like a successful request.
export const apiV2Client = configureAPIClient('/api/v2')
