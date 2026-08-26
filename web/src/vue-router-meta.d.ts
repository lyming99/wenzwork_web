import 'vue-router'

export {}

declare module 'vue-router' {
  interface RouteMeta {
    guestOnly?: boolean
    requiresAuth?: boolean
    requiresAdmin?: boolean
    requiresMfa?: boolean
    requiresPermission?: string
  }
}
