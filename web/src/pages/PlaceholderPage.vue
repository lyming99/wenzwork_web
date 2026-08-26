<script setup lang="ts">
import { useHead } from '@unhead/vue'

const props = defineProps<{
  eyebrow: string
  title: string
  description: string
}>()

useHead({
  title: () => `${props.title}｜WenzWork`,
  meta: [{ name: 'description', content: () => props.description }],
})
</script>

<template>
  <section class="page-hero">
    <div class="shell narrow-shell">
      <p class="section-kicker">{{ eyebrow }}</p>
      <h1>{{ title }}</h1>
      <p>{{ description }}</p>
      <RouterLink class="text-link" to="/">返回首页 <span aria-hidden="true">→</span></RouterLink>
    </div>
  </section>
</template>
