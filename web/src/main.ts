import { createPinia } from 'pinia'
import { ViteSSG } from 'vite-ssg'

import App from './App.vue'
import { installPageViewTracking } from './analytics/pageView'
import { authenticationFailureEvent } from './api/client'
import { helpArticlePaths } from './content/help'
import { routes, scrollBehavior } from './router'
import { useAuthStore } from './stores/auth'
import './styles/main.css'

export const createApp = ViteSSG(App, { routes, scrollBehavior }, ({ app, router }) => {
  const pinia = createPinia()
  app.use(pinia)

  if (!import.meta.env.SSR) {
    installPageViewTracking(router)
    const auth = useAuthStore(pinia)
    let authenticationRecoveryPending = false
    window.addEventListener(authenticationFailureEvent, () => {
      const currentRoute = router.currentRoute.value
      if (!currentRoute.meta.requiresAuth || currentRoute.name === 'login') {
        auth.clear()
        return
      }
      if (authenticationRecoveryPending) return

      // A response from a request started before a successful re-login can
      // arrive late. Re-read /me before clearing the new session so that a
      // stale 401 cannot log the user out again.
      authenticationRecoveryPending = true
      void auth
        .bootstrap(true)
        .then(() => {
          if (auth.isAuthenticated) return
          return router.replace({ name: 'login', query: { redirect: currentRoute.fullPath } })
        })
        .catch(() =>
          router.replace({
            name: 'login',
            query: { redirect: currentRoute.fullPath, unavailable: '1' },
          }),
        )
        .finally(() => (authenticationRecoveryPending = false))
    })
    router.beforeEach(async (to) => {
      if (to.meta.requiresAuth || to.meta.guestOnly) {
        try {
          await auth.bootstrap()
        } catch {
          if (to.meta.requiresAuth) return { name: 'login', query: { unavailable: '1' } }
        }
      }

      if (to.meta.guestOnly && auth.isAuthenticated) return { name: 'account' }
      if (to.meta.requiresAuth && !auth.isAuthenticated) {
        return { name: 'login', query: { redirect: to.fullPath } }
      }
      if (
        auth.systemSetupRequired &&
        auth.hasPermission('admin.super') &&
        to.name !== 'account-security' &&
        to.name !== 'admin-setup'
      ) {
        if (auth.mfaEnforced && auth.assuranceLevel < 2) {
          return { name: 'account-security', query: { mfa: 'required' } }
        }
        return { name: 'admin-setup' }
      }
      if (to.meta.requiresAdmin && !auth.isAdministrator) return { name: 'account' }
      if (to.meta.requiresMfa && auth.mfaEnforced && auth.assuranceLevel < 2) {
        return { name: 'account-security', query: { mfa: 'required' } }
      }
      if (to.meta.requiresPermission && !auth.hasPermission(to.meta.requiresPermission)) {
        return { name: 'admin' }
      }
      return true
    })
  }
})

export const includedRoutes = () => [
  '/',
  '/help',
  ...helpArticlePaths,
  '/download',
  '/pricing',
  '/privacy',
  '/404',
]
