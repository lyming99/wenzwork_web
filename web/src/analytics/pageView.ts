import type { Router } from 'vue-router'

export const recordPageView = async (pagePath: string, referrer = document.referrer) => {
  const response = await fetch('/api/v1/analytics/page-view', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', Accept: 'application/json' },
    body: JSON.stringify({ path: pagePath, referrer }),
    credentials: 'omit',
    keepalive: true,
  })
  if (!response.ok) throw new Error(`page view request failed with ${response.status}`)
}

export const installPageViewTracking = (router: Router) => {
  router.afterEach((to, _from, failure) => {
    if (failure || to.path === '/admin' || to.path.startsWith('/admin/')) return
    void recordPageView(to.path).catch(() => undefined)
  })
}
