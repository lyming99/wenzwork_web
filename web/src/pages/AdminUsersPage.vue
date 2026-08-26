<script setup lang="ts">
import { useHead } from '@unhead/vue'
import { computed, onMounted, ref } from 'vue'

import {
  cancelAdminUserMembership,
  createAdminUser,
  getRemoteAccessPolicy,
  listAdminUsers,
  setAdminUserMembership,
  updateRemoteAccessPolicy,
  updateAdminUserStatus,
  type AdminUser,
  type RemoteAccessPolicySettings,
} from '@/api/admin'
import { problemMessage } from '@/api/auth'
import { useAuthStore } from '@/stores/auth'

useHead({ title: '会员管理｜WenzWork', meta: [{ name: 'robots', content: 'noindex, nofollow' }] })

const auth = useAuthStore()
const users = ref<AdminUser[]>([])
const total = ref(0)
const loading = ref(true)
const pending = ref(false)
const errorMessage = ref('')
const message = ref('')
const query = ref('')
const statusFilter = ref<'' | 'pending' | 'active' | 'disabled'>('')

const createEmail = ref('')
const createDisplayName = ref('')
const createPassword = ref('')

const membershipTarget = ref<AdminUser | null>(null)
const membershipLifetime = ref(false)
const membershipExpiresAt = ref('')
const membershipReason = ref('')
const accessPolicy = ref<RemoteAccessPolicySettings | null>(null)
const policyDeviceLimit = ref(10)
const policyPending = ref(false)

const canManageUsers = computed(() => auth.hasPermission('admin.users.manage'))
const canManageMemberships = computed(() => auth.hasPermission('admin.memberships.manage'))
const canCreate = computed(
  () =>
    createEmail.value.trim().length > 3 &&
    createDisplayName.value.trim().length > 0 &&
    createPassword.value.length >= 8,
)
const canSaveMembership = computed(
  () => membershipLifetime.value || Boolean(membershipExpiresAt.value),
)
const canSaveAccessPolicy = computed(
  () =>
    Boolean(accessPolicy.value) &&
    Number.isInteger(policyDeviceLimit.value) &&
    policyDeviceLimit.value >= 1 &&
    policyDeviceLimit.value <= 100000,
)

const formatDate = (value: string) =>
  new Intl.DateTimeFormat('zh-CN', { dateStyle: 'medium', timeStyle: 'short' }).format(
    new Date(value),
  )

const statusLabel = (status: AdminUser['status']) =>
  ({ active: '正常', disabled: '已禁用', pending: '待验证' })[status]

const load = async () => {
  loading.value = true
  errorMessage.value = ''
  try {
    const result = await listAdminUsers({
      q: query.value.trim() || undefined,
      status: statusFilter.value || undefined,
      limit: 100,
    })
    users.value = result.items
    total.value = result.total
  } catch (error) {
    errorMessage.value = problemMessage(error, '暂时无法读取账户列表。')
  } finally {
    loading.value = false
  }
}

const loadAccessPolicy = async () => {
  try {
    accessPolicy.value = await getRemoteAccessPolicy()
    policyDeviceLimit.value = accessPolicy.value.deviceLimit
  } catch (error) {
    errorMessage.value = problemMessage(error, '暂时无法读取设备接入策略。')
  }
}

const saveAccessPolicy = async () => {
  if (!accessPolicy.value || !canSaveAccessPolicy.value) return
  policyPending.value = true
  errorMessage.value = ''
  message.value = ''
  try {
    accessPolicy.value = await updateRemoteAccessPolicy({
      deviceLimit: policyDeviceLimit.value,
      expectedVersion: accessPolicy.value.version,
    })
    policyDeviceLimit.value = accessPolicy.value.deviceLimit
    message.value = `设备接入上限已调整为每个账号 ${accessPolicy.value.deviceLimit} 台，并已立即生效。`
  } catch (error) {
    errorMessage.value = problemMessage(error, '暂时无法更新设备接入策略。')
    await loadAccessPolicy()
  } finally {
    policyPending.value = false
  }
}

const create = async () => {
  if (!canCreate.value) return
  pending.value = true
  errorMessage.value = ''
  message.value = ''
  try {
    const created = await createAdminUser({
      email: createEmail.value.trim(),
      displayName: createDisplayName.value.trim(),
      password: createPassword.value,
    })
    createEmail.value = ''
    createDisplayName.value = ''
    createPassword.value = ''
    message.value = `账户 ${created.email} 已创建并完成邮箱验证。`
    await load()
  } catch (error) {
    errorMessage.value = problemMessage(error, '暂时无法创建账户。')
  } finally {
    pending.value = false
  }
}

const toggleUserStatus = async (user: AdminUser) => {
  const nextStatus = user.status === 'disabled' ? 'active' : 'disabled'
  const action = nextStatus === 'disabled' ? '禁用' : '启用'
  if (
    !window.confirm(
      `${action}账户“${user.email}”？${nextStatus === 'disabled' ? '该账户的所有登录会话会立即失效。' : ''}`,
    )
  )
    return
  pending.value = true
  errorMessage.value = ''
  message.value = ''
  try {
    await updateAdminUserStatus(user.id, nextStatus)
    message.value = `账户已${action}。`
    await load()
  } catch (error) {
    errorMessage.value = problemMessage(error, `暂时无法${action}账户。`)
  } finally {
    pending.value = false
  }
}

const openMembership = (user: AdminUser) => {
  membershipTarget.value = user
  membershipLifetime.value = user.membership?.lifetime ?? false
  membershipExpiresAt.value = user.membership?.expiresAt
    ? new Date(user.membership.expiresAt).toISOString().slice(0, 16)
    : ''
  if (!user.membership) {
    const defaultExpiry = new Date()
    defaultExpiry.setDate(defaultExpiry.getDate() + 30)
    membershipExpiresAt.value = defaultExpiry.toISOString().slice(0, 16)
  }
  membershipReason.value = ''
}

const saveMembership = async () => {
  if (!membershipTarget.value || !canSaveMembership.value) return
  pending.value = true
  errorMessage.value = ''
  message.value = ''
  try {
    await setAdminUserMembership(membershipTarget.value.id, {
      planCode: 'pro',
      expiresAt: membershipLifetime.value
        ? null
        : new Date(membershipExpiresAt.value).toISOString(),
      reason: membershipReason.value.trim(),
    })
    message.value = `已为 ${membershipTarget.value.email} 设置 Pro 会员。`
    membershipTarget.value = null
    await load()
  } catch (error) {
    errorMessage.value = problemMessage(error, '暂时无法设置会员权限。')
  } finally {
    pending.value = false
  }
}

const cancelMembership = async (user: AdminUser) => {
  if (!window.confirm(`取消“${user.email}”的 Pro 会员权限？账户将立即回到免费方案。`)) return
  pending.value = true
  errorMessage.value = ''
  message.value = ''
  try {
    await cancelAdminUserMembership(user.id)
    message.value = '会员权限已取消。'
    await load()
  } catch (error) {
    errorMessage.value = problemMessage(error, '暂时无法取消会员权限。')
  } finally {
    pending.value = false
  }
}

onMounted(async () => {
  await load()
  if (canManageMemberships.value) await loadAccessPolicy()
})
</script>

<template>
  <section class="dashboard-page admin-wide-page">
    <p class="section-kicker">账户与权益</p>
    <h1>会员管理</h1>
    <p class="dashboard-lead">创建可登录账户、调整 Pro 会员有效期，并在需要时立即禁用账户。</p>
    <p v-if="errorMessage" class="form-message form-error" role="alert">{{ errorMessage }}</p>
    <p v-if="message" class="form-message form-success" role="status">{{ message }}</p>

    <form
      v-if="canManageMemberships"
      class="dashboard-card admin-create-form"
      @submit.prevent="saveAccessPolicy"
    >
      <div class="batch-form-heading">
        <div>
          <p class="card-label">设备接入策略</p>
          <h2>每账号设备上限</h2>
        </div>
        <span class="tag">仅有效 Pro 会员可接入</span>
      </div>
      <div class="form-grid admin-user-create-grid">
        <div class="field-group">
          <label for="remote-device-limit">最多接入设备数</label>
          <input
            id="remote-device-limit"
            v-model.number="policyDeviceLimit"
            type="number"
            min="1"
            max="100000"
            step="1"
            required
          />
          <small>默认 10 台；保存后无需重启服务，后续注册立即采用新上限。</small>
        </div>
        <div v-if="accessPolicy" class="field-group">
          <span>当前策略版本</span>
          <strong>v{{ accessPolicy.version }}</strong>
          <small>更新于 {{ formatDate(accessPolicy.updatedAt) }}</small>
        </div>
      </div>
      <button class="button" type="submit" :disabled="policyPending || !canSaveAccessPolicy">
        {{ policyPending ? '正在保存…' : '保存设备上限' }}
      </button>
      <p class="form-hint">
        调低上限不会删除现有设备；已达到或超过新上限的账号需先永久删除旧设备，才能继续接入。
      </p>
    </form>

    <form v-if="canManageUsers" class="dashboard-card admin-create-form" @submit.prevent="create">
      <div class="batch-form-heading">
        <div>
          <p class="card-label">创建账户</p>
          <h2>新增会员账户</h2>
        </div>
        <span class="tag">创建后可直接登录</span>
      </div>
      <div class="form-grid admin-user-create-grid">
        <div class="field-group">
          <label for="admin-user-email">邮箱</label>
          <input
            id="admin-user-email"
            v-model.trim="createEmail"
            type="email"
            required
            maxlength="320"
          />
        </div>
        <div class="field-group">
          <label for="admin-user-name">显示名称</label>
          <input id="admin-user-name" v-model.trim="createDisplayName" required maxlength="120" />
        </div>
        <div class="field-group field-wide">
          <label for="admin-user-password">初始密码</label>
          <input
            id="admin-user-password"
            v-model="createPassword"
            type="password"
            required
            minlength="8"
            maxlength="128"
            autocomplete="new-password"
          />
          <small>至少 8 个字符；请通过安全渠道交给账户持有人。</small>
        </div>
      </div>
      <button class="button" type="submit" :disabled="pending || !canCreate">创建账户</button>
    </form>

    <form
      v-if="membershipTarget"
      class="dashboard-card membership-adjustment-card"
      @submit.prevent="saveMembership"
    >
      <div class="batch-form-heading">
        <div>
          <p class="card-label">会员权限</p>
          <h2>设置 {{ membershipTarget.email }}</h2>
        </div>
        <button class="text-button" type="button" @click="membershipTarget = null">关闭</button>
      </div>
      <div class="form-grid">
        <div class="field-group">
          <label for="membership-plan">会员方案</label>
          <select id="membership-plan" disabled>
            <option>Pro</option>
          </select>
        </div>
        <label class="checkbox-row membership-lifetime-toggle">
          <input v-model="membershipLifetime" type="checkbox" />
          设置为长期会员
        </label>
        <div v-if="!membershipLifetime" class="field-group">
          <label for="membership-expires">到期时间</label>
          <input
            id="membership-expires"
            v-model="membershipExpiresAt"
            type="datetime-local"
            required
          />
        </div>
        <div class="field-group" :class="{ 'field-wide': membershipLifetime }">
          <label for="membership-reason">调整原因（可选）</label>
          <input
            id="membership-reason"
            v-model.trim="membershipReason"
            maxlength="2000"
            placeholder="例如：客服补发"
          />
        </div>
      </div>
      <button class="button" type="submit" :disabled="pending || !canSaveMembership">
        保存会员权限
      </button>
    </form>

    <section class="admin-list-section" aria-labelledby="admin-user-list-title">
      <div class="section-heading-row">
        <div>
          <p class="section-kicker">账户列表</p>
          <h2 id="admin-user-list-title">共 {{ total }} 个账户</h2>
        </div>
        <form class="admin-filter-row" @submit.prevent="load">
          <input
            v-model.trim="query"
            type="search"
            placeholder="搜索邮箱或名称"
            aria-label="搜索账户"
          />
          <select v-model="statusFilter" aria-label="账户状态">
            <option value="">全部状态</option>
            <option value="active">正常</option>
            <option value="pending">待验证</option>
            <option value="disabled">已禁用</option>
          </select>
          <button class="button button-secondary" type="submit">查询</button>
        </form>
      </div>
      <p v-if="loading" class="inline-status" role="status">正在读取账户…</p>
      <div v-else-if="users.length" class="admin-record-list">
        <article v-for="user in users" :key="user.id" class="dashboard-card admin-record-row">
          <div class="admin-record-main">
            <div class="batch-title-row">
              <h3>{{ user.displayName }}</h3>
              <span :class="['tag', { 'tag-muted': user.status !== 'active' }]">{{
                statusLabel(user.status)
              }}</span>
              <span v-if="user.membership" class="tag">Pro</span>
            </div>
            <p>{{ user.email }}</p>
            <small>
              {{
                user.membership
                  ? user.membership.lifetime
                    ? '长期会员'
                    : `会员至 ${formatDate(user.membership.expiresAt!)}`
                  : '免费方案'
              }}
              · 创建于 {{ formatDate(user.createdAt) }}
            </small>
          </div>
          <div class="admin-row-actions">
            <button
              v-if="canManageMemberships && user.status === 'active'"
              class="button button-secondary"
              type="button"
              @click="openMembership(user)"
            >
              {{ user.membership ? '修改会员' : '设置会员' }}
            </button>
            <button
              v-if="canManageMemberships && user.membership"
              class="text-button danger-text-button"
              type="button"
              @click="cancelMembership(user)"
            >
              取消会员
            </button>
            <button
              v-if="canManageUsers"
              class="text-button"
              :class="{ 'danger-text-button': user.status !== 'disabled' }"
              type="button"
              :disabled="user.id === auth.user?.id"
              @click="toggleUserStatus(user)"
            >
              {{ user.status === 'disabled' ? '重新启用' : '禁用账户' }}
            </button>
          </div>
        </article>
      </div>
      <p v-else class="inline-status">没有符合条件的账户。</p>
    </section>
  </section>
</template>
