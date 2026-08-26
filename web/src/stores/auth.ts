import { defineStore } from 'pinia'
import { computed, ref } from 'vue'

import {
  getCurrentAccount,
  isAuthenticationError,
  LoginSessionUnavailableError,
  loginAccount,
  logoutAccount,
  type AuthUser,
  type LoginRequest,
} from '@/api/auth'
import { clearStoredAgentEventCursors } from '@/remote/agentEventCursor'

type BootstrapStatus = 'idle' | 'loading' | 'ready'

export const useAuthStore = defineStore('auth', () => {
  const user = ref<AuthUser | null>(null)
  const permissions = ref<string[]>([])
  const mfaEnforced = ref(false)
  const assuranceLevel = ref<1 | 2>(1)
  const absoluteExpiresAt = ref<string | null>(null)
  const systemSetupRequired = ref(false)
  const bootstrapStatus = ref<BootstrapStatus>('idle')
  let bootstrapRequest: Promise<void> | null = null
  const isAuthenticated = computed(() => user.value !== null)
  const isAdministrator = computed(() =>
    permissions.value.some((item) => item.startsWith('admin.')),
  )

  const clear = () => {
    clearStoredAgentEventCursors()
    user.value = null
    permissions.value = []
    mfaEnforced.value = false
    assuranceLevel.value = 1
    absoluteExpiresAt.value = null
    systemSetupRequired.value = false
  }

  const applyAccount = (account: {
    user: AuthUser
    permissions: string[]
    mfaEnforced: boolean
    assuranceLevel: number
    absoluteExpiresAt: string
    systemSetupRequired?: boolean
  }) => {
    user.value = account.user
    permissions.value = account.permissions
    mfaEnforced.value = account.mfaEnforced
    assuranceLevel.value = account.assuranceLevel === 2 ? 2 : 1
    absoluteExpiresAt.value = account.absoluteExpiresAt
    systemSetupRequired.value = account.systemSetupRequired === true
  }

  const bootstrap = async (force = false) => {
    if (typeof window === 'undefined') return
    if (!force && bootstrapStatus.value === 'ready') return
    if (bootstrapRequest) return bootstrapRequest

    bootstrapStatus.value = 'loading'
    bootstrapRequest = getCurrentAccount()
      .then(applyAccount)
      .catch((error: unknown) => {
        clear()
        if (!isAuthenticationError(error)) throw error
      })
      .finally(() => {
        bootstrapStatus.value = 'ready'
        bootstrapRequest = null
      })
    return bootstrapRequest
  }

  const login = async (request: LoginRequest) => {
    const account = await loginAccount(request)
    try {
      // A successful login response only proves that the credentials were
      // accepted. Confirm the browser actually retained the HttpOnly session
      // cookie before exposing authenticated UI state.
      const confirmedAccount = await getCurrentAccount()
      applyAccount(confirmedAccount)
      bootstrapStatus.value = 'ready'
      return account
    } catch (error) {
      clear()
      bootstrapStatus.value = 'ready'
      if (isAuthenticationError(error)) {
        throw new LoginSessionUnavailableError({ cause: error })
      }
      throw error
    }
  }

  const logout = async () => {
    try {
      await logoutAccount()
    } finally {
      clear()
      bootstrapStatus.value = 'ready'
    }
  }

  const hasPermission = (permission: string) => permissions.value.includes(permission)

  return {
    user,
    permissions,
    mfaEnforced,
    assuranceLevel,
    absoluteExpiresAt,
    systemSetupRequired,
    bootstrapStatus,
    isAuthenticated,
    isAdministrator,
    bootstrap,
    login,
    logout,
    clear,
    applyAccount,
    hasPermission,
  }
})
