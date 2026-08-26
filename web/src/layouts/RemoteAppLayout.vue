<script setup lang="ts">
import { useHead } from '@unhead/vue'
import { onMounted } from 'vue'
import { RouterView } from 'vue-router'

import { claimRemoteManagerWindowName } from '@/utils/remoteManagerWindow'

useHead({
  titleTemplate: (title) => (title ? `${title} · 设备工作台` : '设备工作台｜WenzWork'),
  meta: [
    { name: 'robots', content: 'noindex, nofollow' },
    { name: 'theme-color', content: '#f4f6f5' },
  ],
})

onMounted(claimRemoteManagerWindowName)
</script>

<template>
  <div class="remote-app-shell"><RouterView /></div>
</template>

<style scoped>
.remote-app-shell {
  min-width: 320px;
  min-height: 100vh;
  color: var(--ink);
  background: #f8faf9;
}
</style>
