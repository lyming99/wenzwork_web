<script setup lang="ts">
import { useHead } from '@unhead/vue'
import { computed } from 'vue'
import { RouterLink } from 'vue-router'

import { useAuthStore } from '@/stores/auth'

useHead({ title: '管理后台｜WenzWork', meta: [{ name: 'robots', content: 'noindex, nofollow' }] })

const auth = useAuthStore()
const modules = computed(() =>
  [
    {
      permission: 'admin.memberships.manage',
      title: '内测码管理',
      description: '调整官网内测码剩余名额，并查看邮件发送与兑换状态。',
      to: '/admin/beta-codes',
      ready: true,
    },
    {
      permission: 'admin.memberships.manage',
      title: '兑换码管理',
      description: '创建兑换码、查看逐项状态，并删除尚未使用的码。',
      to: '/admin/redemption-codes',
      ready: true,
    },
    {
      permission: 'admin.releases.manage',
      title: '软件发布',
      description: '维护软件版本、安装文件列表与面向用户的更新公告。',
      to: '/admin/releases',
      ready: true,
    },
    {
      permission: 'admin.relay.manage',
      title: 'Relay 节点',
      description: '登记宿主机、生成一次性注册凭据、核对身份与心跳，并管理节点生命周期。',
      to: '/admin/relay',
      ready: true,
    },
    {
      permission: 'admin.pricing.manage',
      title: '价格与内容',
      description: '维护官网价格套餐、展示顺序、发布版本和上下架状态。',
      to: '/admin/pricing',
      ready: true,
    },
    {
      permission: 'admin.users.read',
      title: '会员管理',
      description: '创建和禁用账户，并设置或取消 Pro 会员权限。',
      to: '/admin/users',
      ready: true,
    },
    {
      permission: 'admin.audit.read',
      title: '访问统计',
      description: '查看独立 IP 趋势、访问来源、新访问 IP、下载与注册转化及账户登录记录。',
      to: '/admin/analytics',
      ready: true,
    },
  ].filter((item) => auth.hasPermission(item.permission)),
)
</script>

<template>
  <section class="dashboard-page">
    <p class="section-kicker">管理后台</p>
    <h1>运营总览</h1>
    <p class="dashboard-lead">
      {{
        auth.mfaEnforced
          ? '当前会话已完成管理员 MFA。'
          : '当前配置未启用管理员 MFA 门禁。'
      }}
      后台仅展示该角色获授的模块，所有写操作仍由 Go API 再次执行 RBAC 校验。
    </p>
    <div class="admin-module-grid">
      <article v-for="item in modules" :key="item.to" class="dashboard-card admin-module-card">
        <span :class="['tag', { 'tag-muted': !item.ready }]">{{
          item.ready ? '可用' : '计划中'
        }}</span>
        <h2>{{ item.title }}</h2>
        <p>{{ item.description }}</p>
        <RouterLink v-if="item.ready" class="text-link" :to="item.to">进入模块 →</RouterLink>
      </article>
      <p v-if="modules.length === 0" class="inline-status">当前角色没有可见的管理模块。</p>
    </div>
  </section>
</template>
