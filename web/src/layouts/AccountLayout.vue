<script setup lang="ts">
import { RouterLink, RouterView } from 'vue-router'
import { useRouter } from 'vue-router'

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
  <div class="app-shell">
    <aside class="app-sidebar">
      <RouterLink class="brand app-sidebar-brand" to="/"><BrandLogo />WenzWork</RouterLink>
      <nav class="side-nav" aria-label="账户导航">
        <RouterLink to="/account"
          ><span class="side-icon" aria-hidden="true">●</span>账户资料</RouterLink
        >
        <RouterLink to="/account/security"
          ><span class="side-icon" aria-hidden="true">◆</span>安全设置</RouterLink
        >
        <RouterLink to="/account/sessions"
          ><span class="side-icon" aria-hidden="true">▣</span>登录会话</RouterLink
        >
        <RouterLink to="/account/remote"
          ><span class="side-icon" aria-hidden="true">⌁</span>远程设备</RouterLink
        >
        <RouterLink to="/account/membership"
          ><span class="side-icon" aria-hidden="true">★</span>会员中心</RouterLink
        >
        <RouterLink to="/account/feedback"
          ><span class="side-icon" aria-hidden="true">▤</span>意见反馈</RouterLink
        >
        <RouterLink v-if="auth.isAdministrator" to="/admin"
          ><span class="side-icon" aria-hidden="true">⚙</span>管理后台</RouterLink
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
