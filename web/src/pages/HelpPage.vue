<script setup lang="ts">
import HelpDocumentTree from '@/components/HelpDocumentTree.vue'
import { usePageHead } from '@/composables/usePageHead'
import { helpArticles } from '@/content/help'

usePageHead({
  title: '使用帮助',
  description:
    '通过分组文档目录查找 WenzWork 一键部署、远程设备接入、项目工作区、安全校验和会员兑换说明。',
  path: '/help',
})

const featuredArticles = helpArticles.slice(0, 3)
</script>

<template>
  <section class="help-docs-page">
    <div class="shell help-docs-layout">
      <HelpDocumentTree />

      <article class="help-doc-content help-overview">
        <nav class="help-breadcrumbs" aria-label="面包屑">
          <RouterLink to="/">首页</RouterLink><span aria-hidden="true">/</span><span>使用帮助</span>
        </nav>
        <p class="section-kicker">WenzWork 文档</p>
        <h1>从目录找到问题，<br />在这里读完答案。</h1>
        <p class="help-doc-lead">
          左侧文档树按主题组织 Host/Relay
          一键部署、远程设备、项目工作区、数据安全与会员说明。选择文章后，目录会保持在原位，正文在当前区域打开。
        </p>

        <aside class="help-tip-card">
          <strong>离线也能读</strong>
          <p>核心帮助随网站构建发布；服务端暂不可用时，内置 Markdown 文档和目录仍然可读。</p>
        </aside>

        <section class="help-start-section" aria-labelledby="help-start-title">
          <p class="section-kicker">建议从这里开始</p>
          <h2 id="help-start-title">三篇文章，从部署到接入设备。</h2>
          <div class="help-start-grid">
            <RouterLink
              v-for="article in featuredArticles"
              :key="article.slug"
              :to="`/help/${article.slug}`"
            >
              <span>{{ article.category }}</span>
              <strong>{{ article.title }}</strong>
              <p>{{ article.description }}</p>
            </RouterLink>
          </div>
        </section>

        <aside class="help-contact-card">
          <div>
            <p class="section-kicker">仍然没有答案？</p>
            <h2>直接联系我们。</h2>
            <p>QQ 交流群：<strong>1026582431</strong></p>
            <p>微信：<strong>lyming555</strong></p>
            <p>邮箱：<a href="mailto:44185539@qq.com">44185539@qq.com</a></p>
          </div>
          <RouterLink class="button button-secondary" to="/account/feedback"
            >提交产品反馈</RouterLink
          >
        </aside>
      </article>
    </div>
  </section>
</template>
