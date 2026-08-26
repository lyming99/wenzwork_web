import { describe, expect, it } from 'vitest'

import { routes, scrollBehavior } from './index'

describe('router scroll behavior', () => {
  it('returns to the top for a new route', () => {
    expect(scrollBehavior({ hash: '' } as never, {} as never, null)).toEqual({
      left: 0,
      top: 0,
      behavior: 'instant',
    })
  })

  it('keeps browser history positions and supports hash targets', () => {
    const savedPosition = { left: 0, top: 480 }

    expect(scrollBehavior({ hash: '' } as never, {} as never, savedPosition)).toEqual({
      ...savedPosition,
      behavior: 'instant',
    })
    expect(scrollBehavior({ hash: '#details' } as never, {} as never, null)).toEqual({
      el: '#details',
    })
  })

  it('protects the analytics page with the audit permission', () => {
    const admin = routes.find((route) => route.path === '/admin')
    const analytics = admin?.children?.find((route) => route.path === 'analytics')

    expect(analytics?.meta?.requiresPermission).toBe('admin.audit.read')
  })

  it('protects beta-code management with the membership permission', () => {
    const admin = routes.find((route) => route.path === '/admin')
    const betaCodes = admin?.children?.find((route) => route.path === 'beta-codes')

    expect(betaCodes?.meta?.requiresPermission).toBe('admin.memberships.manage')
  })

  it('keeps the catalog in account management and hosts device workspaces separately', () => {
    const remoteApp = routes.find((route) => route.path === '/remote')
    const account = routes.find((route) => route.path === '/account')
    const accountRemote = account?.children?.find((route) => route.path === 'remote')

    expect(remoteApp?.meta?.requiresAuth).toBe(true)
    expect(remoteApp?.children?.map((route) => route.name)).toEqual([
      'remote-app',
      'remote-app-device',
    ])
    expect(remoteApp?.children?.[0]?.redirect).toEqual({ name: 'account-remote' })
    expect(accountRemote?.component).toBeTypeOf('function')
  })
})
