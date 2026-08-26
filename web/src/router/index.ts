import type { RouteRecordRaw, RouterScrollBehavior } from 'vue-router'

import AccountLayout from '@/layouts/AccountLayout.vue'
import AdminLayout from '@/layouts/AdminLayout.vue'
import PublicLayout from '@/layouts/PublicLayout.vue'
import RemoteAppLayout from '@/layouts/RemoteAppLayout.vue'
import HomePage from '@/pages/HomePage.vue'

export const scrollBehavior: RouterScrollBehavior = (to, _from, savedPosition) => {
  if (savedPosition) return { ...savedPosition, behavior: 'instant' }
  if (to.hash) return { el: to.hash }
  return { left: 0, top: 0, behavior: 'instant' }
}

export const routes: RouteRecordRaw[] = [
  {
    path: '/',
    component: PublicLayout,
    children: [
      { path: '', name: 'home', component: HomePage },
      {
        path: 'help',
        name: 'help',
        component: () => import('@/pages/HelpPage.vue'),
      },
      {
        path: 'help/:slug',
        name: 'help-article',
        component: () => import('@/pages/HelpArticlePage.vue'),
      },
      {
        path: 'download',
        name: 'download',
        component: () => import('@/pages/DownloadPage.vue'),
      },
      {
        path: 'pricing',
        name: 'pricing',
        component: () => import('@/pages/PricingPage.vue'),
      },
      {
        path: 'privacy',
        name: 'privacy',
        component: () => import('@/pages/PrivacyPage.vue'),
      },
      {
        path: 'login',
        name: 'login',
        component: () => import('@/pages/LoginPage.vue'),
        meta: { guestOnly: true },
      },
      {
        path: 'app-login',
        name: 'app-login',
        component: () => import('@/pages/AppLoginPage.vue'),
        meta: { requiresAuth: true },
      },
      {
        path: 'register',
        name: 'register',
        component: () => import('@/pages/RegisterPage.vue'),
        meta: { guestOnly: true },
      },
      {
        path: 'verify-email',
        name: 'verify-email',
        component: () => import('@/pages/VerifyEmailPage.vue'),
      },
      {
        path: 'forgot-password',
        name: 'forgot-password',
        component: () => import('@/pages/ForgotPasswordPage.vue'),
        meta: { guestOnly: true },
      },
      {
        path: 'reset-password',
        name: 'reset-password',
        component: () => import('@/pages/ResetPasswordPage.vue'),
      },
    ],
  },
  {
    path: '/remote',
    component: RemoteAppLayout,
    meta: { requiresAuth: true },
    children: [
      {
        path: '',
        name: 'remote-app',
        redirect: { name: 'account-remote' },
      },
      {
        path: ':deviceId',
        name: 'remote-app-device',
        component: () => import('@/pages/RemoteDevicePage.vue'),
      },
    ],
  },
  {
    path: '/account',
    component: AccountLayout,
    meta: { requiresAuth: true },
    children: [
      {
        path: '',
        name: 'account',
        component: () => import('@/pages/AccountOverviewPage.vue'),
      },
      {
        path: 'security',
        name: 'account-security',
        component: () => import('@/pages/AccountSecurityPage.vue'),
      },
      {
        path: 'sessions',
        name: 'account-sessions',
        component: () => import('@/pages/AccountSessionsPage.vue'),
      },
      {
        path: 'remote',
        name: 'account-remote',
        component: () => import('@/pages/RemoteDevicesPage.vue'),
      },
      {
        path: 'remote/:deviceId',
        name: 'account-remote-device',
        redirect: (to) => ({
          name: 'remote-app-device',
          params: { deviceId: to.params.deviceId },
        }),
      },
      {
        path: 'membership',
        name: 'membership',
        component: () => import('@/pages/MembershipPage.vue'),
      },
      {
        path: 'feedback',
        name: 'account-feedback',
        component: () => import('@/pages/AccountFeedbackPage.vue'),
      },
    ],
  },
  {
    path: '/admin',
    component: AdminLayout,
    meta: { requiresAuth: true, requiresAdmin: true, requiresMfa: true },
    children: [
      {
        path: '',
        name: 'admin',
        component: () => import('@/pages/AdminDashboardPage.vue'),
      },
      {
        path: 'setup',
        name: 'admin-setup',
        component: () => import('@/pages/AdminSystemSetupPage.vue'),
        meta: { requiresPermission: 'admin.super' },
      },
      {
        path: 'users',
        name: 'admin-users',
        component: () => import('@/pages/AdminUsersPage.vue'),
        meta: { requiresPermission: 'admin.users.read' },
      },
      {
        path: 'analytics',
        name: 'admin-analytics',
        component: () => import('@/pages/AdminAnalyticsPage.vue'),
        meta: { requiresPermission: 'admin.audit.read' },
      },
      {
        path: 'releases',
        name: 'admin-releases',
        component: () => import('@/pages/AdminReleasesPage.vue'),
        meta: { requiresPermission: 'admin.releases.manage' },
      },
      {
        path: 'relay',
        name: 'admin-relay',
        component: () => import('@/pages/AdminRelayPage.vue'),
        meta: { requiresPermission: 'admin.relay.manage' },
      },
      {
        path: 'pricing',
        name: 'admin-pricing',
        component: () => import('@/pages/AdminPricingPage.vue'),
        meta: { requiresPermission: 'admin.pricing.manage' },
      },
      {
        path: 'redemption-codes',
        name: 'admin-redemption-codes',
        component: () => import('@/pages/AdminRedemptionCodesPage.vue'),
        meta: { requiresPermission: 'admin.memberships.manage' },
      },
      {
        path: 'beta-codes',
        name: 'admin-beta-codes',
        component: () => import('@/pages/AdminBetaCodesPage.vue'),
        meta: { requiresPermission: 'admin.memberships.manage' },
      },
      {
        path: 'help-documents',
        name: 'admin-help-documents',
        component: () => import('@/pages/AdminHelpDocumentsPage.vue'),
        meta: { requiresPermission: 'admin.help.manage' },
      },
      {
        path: 'feedback',
        name: 'admin-feedback',
        component: () => import('@/pages/AdminFeedbackPage.vue'),
        meta: { requiresPermission: 'admin.feedback.manage' },
      },
    ],
  },
  {
    path: '/:pathMatch(.*)*',
    component: () => import('@/pages/NotFoundPage.vue'),
  },
]
