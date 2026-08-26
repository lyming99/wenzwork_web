<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import type { NavigationFailure } from 'vue-router'
import { RouterLink, RouterView, useRoute } from 'vue-router'

import BrandLogo from '@/components/BrandLogo.vue'
import { useAuthStore } from '@/stores/auth'

const menuOpen = ref(false)
const productsMenu = ref<HTMLDetailsElement | null>(null)
const auth = useAuthStore()
const route = useRoute()
const authRouteNames = new Set([
  'login',
  'register',
  'forgot-password',
  'reset-password',
  'verify-email',
  'app-login',
])
const isAuthPage = computed(() => authRouteNames.has(String(route.name ?? '')))

const closeMenu = () => {
  menuOpen.value = false
  if (productsMenu.value) productsMenu.value.open = false
}

type MenuNavigate = (event?: MouseEvent) => Promise<void | NavigationFailure>

const navigateFromMenu = async (event: MouseEvent, navigate: MenuNavigate) => {
  try {
    await navigate(event)
  } finally {
    closeMenu()
  }
}

onMounted(() => {
  void auth.bootstrap().catch(() => undefined)
})
</script>

<template>
  <a class="skip-link" href="#main-content">跳到主要内容</a>
  <header class="site-header">
    <div class="shell nav-wrap">
      <RouterLink v-slot="{ href, navigate, isActive }" custom to="/">
        <a
          :class="['brand', { active: isActive }]"
          :href="href"
          aria-label="WenzWork 首页"
          @click="navigateFromMenu($event, navigate)"
        >
          <BrandLogo />
          <span>WenzWork</span>
        </a>
      </RouterLink>

      <button
        class="menu-button"
        type="button"
        :aria-expanded="menuOpen"
        aria-controls="primary-navigation"
        @click="menuOpen = !menuOpen"
      >
        <span class="sr-only">{{ menuOpen ? '关闭导航菜单' : '打开导航菜单' }}</span>
        <span aria-hidden="true">{{ menuOpen ? '关闭' : '菜单' }}</span>
      </button>

      <nav id="primary-navigation" :class="['primary-nav', { open: menuOpen }]" aria-label="主导航">
        <details ref="productsMenu" class="product-menu">
          <summary>
            产品列表
            <span class="product-menu-caret" aria-hidden="true">
              <svg viewBox="0 0 16 16" focusable="false">
                <path d="m4 6 4 4 4-4" />
              </svg>
            </span>
          </summary>
          <div class="product-menu-panel">
            <a
              href="https://wenzflow.com"
              target="_blank"
              rel="noopener noreferrer"
              @click="closeMenu"
            >
              WenzFlow <span aria-hidden="true">↗</span>
            </a>
            <a
              href="https://wenzmark.cn"
              target="_blank"
              rel="noopener noreferrer"
              @click="closeMenu"
            >
              WenzMark <span aria-hidden="true">↗</span>
            </a>
            <a
              href="https://work.wenzflow.com"
              target="_blank"
              rel="noopener noreferrer"
              @click="closeMenu"
            >
              WenzWork <span aria-hidden="true">↗</span>
            </a>
          </div>
        </details>
        <RouterLink v-slot="{ href, navigate, isActive }" custom to="/help">
          <a :class="{ active: isActive }" :href="href" @click="navigateFromMenu($event, navigate)"
            >使用帮助</a
          >
        </RouterLink>
        <RouterLink v-slot="{ href, navigate, isActive }" custom to="/download">
          <a :class="{ active: isActive }" :href="href" @click="navigateFromMenu($event, navigate)"
            >软件下载</a
          >
        </RouterLink>
        <RouterLink v-slot="{ href, navigate, isActive }" custom to="/pricing">
          <a :class="{ active: isActive }" :href="href" @click="navigateFromMenu($event, navigate)"
            >产品价格</a
          >
        </RouterLink>
        <RouterLink
          v-slot="{ href, navigate }"
          custom
          :to="auth.isAuthenticated ? '/account' : '/login'"
        >
          <a
            class="nav-login"
            :class="{ active: route.path === '/login' || route.path.startsWith('/account') }"
            :href="href"
            @click="navigateFromMenu($event, navigate)"
          >
            {{ auth.isAuthenticated ? '账户中心' : '登录' }}
          </a>
        </RouterLink>
        <RouterLink v-slot="{ href, navigate }" custom to="/download">
          <a :href="href" class="button button-small" @click="navigateFromMenu($event, navigate)"
            ><svg
              class="nav-download-icon"
              viewBox="0 0 16 16"
              fill="none"
              stroke="currentColor"
              stroke-width="1.8"
              stroke-linecap="round"
              stroke-linejoin="round"
              aria-hidden="true"
            >
              <path d="M8 2.5v7m0 0 3-3M8 9.5l-3-3" />
              <path d="M2.5 13h11" />
            </svg>
            免费下载</a
          >
        </RouterLink>
      </nav>
    </div>
  </header>

  <main id="main-content" :class="{ 'auth-page-main': isAuthPage }">
    <RouterView />
  </main>

  <footer v-if="!isAuthPage" class="site-footer">
    <div class="shell footer-grid">
      <div>
        <div class="brand footer-brand"><BrandLogo />WenzWork</div>
        <p>本机与远程一体的 AI 项目工作台。</p>
      </div>
      <nav aria-label="页脚导航">
        <RouterLink to="/help">使用帮助</RouterLink>
        <RouterLink to="/download">软件下载</RouterLink>
        <RouterLink to="/pricing">产品价格</RouterLink>
        <RouterLink to="/privacy">隐私政策</RouterLink>
        <a href="https://github.com/lyming99/wenzwork" target="_blank" rel="noopener noreferrer"
          >GitHub 开源</a
        >
      </nav>
      <address class="footer-contact">
        <strong>联系我们</strong>
        <span>QQ 交流群：1026582431</span>
        <span>微信：lyming555</span>
        <a href="mailto:44185539@qq.com">邮箱：44185539@qq.com</a>
      </address>
      <p class="copyright">© 2026 WenzWork</p>
    </div>
  </footer>
</template>
