<script setup lang="ts">
import { useHead } from '@unhead/vue'

useHead({
  title: '页面未找到｜WenzWork',
  meta: [{ name: 'robots', content: 'noindex' }],
})
</script>

<template>
  <main class="not-found">
    <p class="section-kicker">404</p>
    <h1>这一页还没有写下内容。</h1>
    <p>地址可能已经变化，或者页面尚未发布。</p>
    <RouterLink class="button" to="/">返回首页</RouterLink>
  </main>
</template>
