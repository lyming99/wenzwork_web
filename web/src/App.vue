<script setup lang="ts">
import { onErrorCaptured, ref } from 'vue'

const failed = ref(false)

onErrorCaptured(() => {
  failed.value = true
  return false
})

const reload = () => {
  if (typeof window !== 'undefined') window.location.reload()
}
</script>

<template>
  <section v-if="failed" class="global-error" role="alert">
    <div>
      <p class="section-kicker">页面发生错误</p>
      <h1>内容没有正确加载。</h1>
      <p>你可以重新加载页面；如果问题持续出现，请稍后再试。</p>
      <div class="error-actions">
        <button class="button" type="button" @click="reload">重新加载</button>
        <a class="button button-secondary" href="/">返回首页</a>
      </div>
    </div>
  </section>
  <RouterView v-else />
</template>
