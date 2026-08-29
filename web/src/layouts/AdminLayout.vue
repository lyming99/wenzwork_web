<script setup lang="ts">
import { RouterLink, RouterView, useRouter } from 'vue-router'

import BrandLogo from '@/components/BrandLogo.vue'
import { useAuthStore } from '@/stores/auth'

const auth = useAuthStore()
const router = useRouter()

const logout = async () => {
  await auth.logout().catch(() => undefined)
  await router.push('/')
}
</script>

<template>
  <div class="app-shell admin-shell">
    <aside class="app-sidebar admin-sidebar">
      <RouterLink class="brand app-sidebar-brand" to="/"><BrandLogo />WenzWork</RouterLink>
      <p class="sidebar-label">管理后台</p>
      <p class="sidebar-user">
        {{ auth.user?.displayName }} ·
        {{ auth.mfaEnforced ? 'MFA L' + auth.assuranceLevel : 'MFA 门禁未启用' }}
      </p>
      <nav class="side-nav" aria-label="管理导航">
        <RouterLink v-if="auth.hasPermission('admin.super')" to="/admin/setup"
          ><span class="side-icon" aria-hidden="true">⚙</span
          >{{ auth.systemSetupRequired ? '首次系统初始化' : '系统设置' }}</RouterLink
        >
        <RouterLink to="/admin"
          ><span class="side-icon" aria-hidden="true">▦</span>运营总览</RouterLink
        >
        <RouterLink v-if="auth.hasPermission('admin.users.read')" to="/admin/users"
          ><span class="side-icon" aria-hidden="true">●</span>会员管理</RouterLink
        >
        <RouterLink v-if="auth.hasPermission('admin.audit.read')" to="/admin/analytics"
          ><span class="side-icon" aria-hidden="true">↗</span>访问统计</RouterLink
        >
        <RouterLink v-if="auth.hasPermission('admin.releases.manage')" to="/admin/releases"
          ><span class="side-icon" aria-hidden="true">↑</span>软件版本</RouterLink
        >
        <RouterLink v-if="auth.hasPermission('admin.relay.manage')" to="/admin/relay"
          ><span class="side-icon" aria-hidden="true">⇄</span>中继主机</RouterLink
        >
        <RouterLink v-if="auth.hasPermission('admin.pricing.manage')" to="/admin/pricing"
          ><span class="side-icon" aria-hidden="true">◇</span>价格管理</RouterLink
        >
        <RouterLink
          v-if="auth.hasPermission('admin.memberships.manage')"
          to="/admin/redemption-codes"
          ><span class="side-icon" aria-hidden="true">#</span>兑换码管理</RouterLink
        >
        <RouterLink v-if="auth.hasPermission('admin.memberships.manage')" to="/admin/beta-codes"
          ><span class="side-icon" aria-hidden="true">✦</span>内测码与试用码</RouterLink
        >
        <RouterLink v-if="auth.hasPermission('admin.help.manage')" to="/admin/help-documents"
          ><span class="side-icon" aria-hidden="true">▤</span>帮助文档</RouterLink
        >
        <RouterLink v-if="auth.hasPermission('admin.feedback.manage')" to="/admin/feedback"
          ><span class="side-icon" aria-hidden="true">◫</span>反馈管理</RouterLink
        >
        <RouterLink to="/account/security"
          ><span class="side-icon" aria-hidden="true">◆</span>安全设置</RouterLink
        >
        <RouterLink to="/"><span class="side-icon" aria-hidden="true">↩</span>返回官网</RouterLink>
        <button class="sidebar-action" type="button" @click="logout">
          <span class="side-icon" aria-hidden="true">⏻</span>退出登录
        </button>
      </nav>
    </aside>
    <main id="main-content" class="app-content"><RouterView /></main>
  </div>
</template>
