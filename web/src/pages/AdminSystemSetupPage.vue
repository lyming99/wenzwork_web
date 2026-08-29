<script setup lang="ts">
import { useHead } from '@unhead/vue'
import { computed, onMounted, reactive, ref } from 'vue'

import { problemMessage } from '@/api/auth'
import {
  getSystemEmailSettings,
  resetSystemEmailSettings,
  testSystemEmailSettings,
  updateSystemEmailSettings,
  type SystemEmailSettings,
  type UpdateSystemEmailSettingsRequest,
} from '@/api/systemEmail'
import { applySystemSetup, getSystemSetup, type ApplySystemSetupRequest } from '@/api/systemSetup'
import { useAuthStore } from '@/stores/auth'

useHead({
  title: '系统设置｜WenzWork',
  meta: [{ name: 'robots', content: 'noindex, nofollow' }],
})

const loading = ref(true)
const pending = ref(false)
const completed = ref(false)
const errorMessage = ref('')
const successMessage = ref('')
const allowedOriginsText = ref('')
const smtpPassword = ref('')
const smtpPasswordConfigured = ref(false)
const setupEmailEnabled = ref(true)
const emailSettings = ref<SystemEmailSettings | null>(null)
const emailPassword = ref('')
const emailPending = ref(false)
const testPending = ref(false)
const testRecipient = ref('')
const auth = useAuthStore()

const pageTitle = computed(() => (completed.value ? '系统设置' : '完成系统初始化'))

const form = reactive<ApplySystemSetupRequest>({
  publicBaseUrl: 'http://localhost:8080',
  databaseUrl: '',
  redisUrl: '',
  smtpHost: 'localhost',
  smtpPort: 1025,
  smtpUser: '',
  clearSmtpPassword: false,
  mailFrom: 'WenzWork <noreply@local.wenzwork.test>',
  cookieSecure: false,
  adminMfaRequired: false,
  registrationEnabled: true,
  allowedOrigins: [],
  webGithubRepository: 'lyming99/wenzwork_web',
  desktopGithubRepository: 'lyming99/wenzwork',
  mobileGithubRepository: 'lyming99/wenzwork_mobile',
})

const emailForm = reactive<UpdateSystemEmailSettingsRequest>({
  smtpHost: '',
  smtpPort: 1025,
  smtpUser: '',
  clearSmtpPassword: false,
  mailFrom: '',
  expectedVersion: 1,
})

const applyEmailSettings = (settings: SystemEmailSettings) => {
  emailSettings.value = settings
  Object.assign(emailForm, {
    smtpHost: settings.smtpHost,
    smtpPort: settings.smtpPort || 1025,
    smtpUser: settings.smtpUser,
    clearSmtpPassword: false,
    mailFrom: settings.mailFrom,
    expectedVersion: settings.version,
  })
  emailPassword.value = ''
}

const load = async () => {
  loading.value = true
  errorMessage.value = ''
  try {
    const settings = await getSystemSetup()
    Object.assign(form, {
      publicBaseUrl: settings.publicBaseUrl,
      databaseUrl: settings.databaseUrl,
      redisUrl: settings.redisUrl,
      smtpHost: settings.smtpHost,
      smtpPort: settings.smtpPort,
      smtpUser: settings.smtpUser,
      mailFrom: settings.mailFrom,
      cookieSecure: settings.cookieSecure,
      adminMfaRequired: settings.adminMfaRequired,
      registrationEnabled: settings.registrationEnabled,
      allowedOrigins: settings.allowedOrigins,
      webGithubRepository: settings.webGithubRepository,
      desktopGithubRepository: settings.desktopGithubRepository,
      mobileGithubRepository: settings.mobileGithubRepository,
      clearSmtpPassword: false,
    })
    allowedOriginsText.value = settings.allowedOrigins.join('\n')
    smtpPasswordConfigured.value = settings.smtpPasswordConfigured
    setupEmailEnabled.value = settings.smtpConfigured
    completed.value = !settings.required
    testRecipient.value ||= auth.user?.email ?? ''
    try {
      applyEmailSettings(await getSystemEmailSettings())
    } catch (error) {
      errorMessage.value = problemMessage(error, '暂时无法读取系统邮箱配置。')
    }
  } catch (error) {
    errorMessage.value = problemMessage(error, '暂时无法读取系统初始化配置。')
  } finally {
    loading.value = false
  }
}

const submit = async () => {
  if (pending.value || completed.value) return
  pending.value = true
  errorMessage.value = ''
  form.allowedOrigins = allowedOriginsText.value
    .split(/[\n,]/)
    .map((value) => value.trim())
    .filter(Boolean)
  const request: ApplySystemSetupRequest = { ...form }
  if (setupEmailEnabled.value) {
    if (smtpPassword.value) request.smtpPassword = smtpPassword.value
  } else {
    request.smtpHost = ''
    request.smtpUser = ''
    request.mailFrom = ''
    request.clearSmtpPassword = true
    delete request.smtpPassword
  }
  try {
    await applySystemSetup(request)
    completed.value = true
    smtpPassword.value = ''
    successMessage.value = '初始化配置已保存；系统邮箱不会阻止初始化完成。'
    getSystemEmailSettings()
      .then(applyEmailSettings)
      .catch(() => undefined)
  } catch (error) {
    errorMessage.value = problemMessage(error, '系统初始化失败，请检查配置后重试。')
  } finally {
    pending.value = false
  }
}

const emailDraft = (setup: boolean) => {
  const request = {
    smtpHost: setup ? (form.smtpHost ?? '') : emailForm.smtpHost,
    smtpPort: setup ? form.smtpPort : emailForm.smtpPort,
    smtpUser: setup ? form.smtpUser : emailForm.smtpUser,
    clearSmtpPassword: setup ? form.clearSmtpPassword : emailForm.clearSmtpPassword,
    mailFrom: setup ? (form.mailFrom ?? '') : emailForm.mailFrom,
    recipient: testRecipient.value.trim(),
  }
  const password = setup ? smtpPassword.value : emailPassword.value
  return password ? { ...request, smtpPassword: password } : request
}

const sendTest = async (setup: boolean) => {
  if (testPending.value) return
  testPending.value = true
  errorMessage.value = ''
  successMessage.value = ''
  try {
    await testSystemEmailSettings(emailDraft(setup))
    successMessage.value = `测试邮件已发送至 ${testRecipient.value.trim()}。`
  } catch (error) {
    errorMessage.value = problemMessage(error, '测试邮件发送失败，请检查当前填写的配置。')
  } finally {
    testPending.value = false
  }
}

const saveEmail = async () => {
  if (emailPending.value) return
  emailPending.value = true
  errorMessage.value = ''
  successMessage.value = ''
  const request: UpdateSystemEmailSettingsRequest = { ...emailForm }
  if (emailPassword.value) request.smtpPassword = emailPassword.value
  try {
    applyEmailSettings(await updateSystemEmailSettings(request))
    successMessage.value = '系统邮箱已保存到数据库并立即生效。'
  } catch (error) {
    errorMessage.value = problemMessage(error, '无法保存系统邮箱配置。')
  } finally {
    emailPending.value = false
  }
}

const resetEmail = async () => {
  if (
    emailPending.value ||
    !emailSettings.value ||
    !window.confirm('恢复本地保底配置？数据库中的系统邮箱配置和加密密码将被清除。')
  )
    return
  emailPending.value = true
  errorMessage.value = ''
  successMessage.value = ''
  try {
    applyEmailSettings(await resetSystemEmailSettings(emailSettings.value.version))
    successMessage.value = '已恢复使用本地保底配置。'
  } catch (error) {
    errorMessage.value = problemMessage(error, '无法恢复本地系统邮箱配置。')
  } finally {
    emailPending.value = false
  }
}

onMounted(load)
</script>

<template>
  <section class="dashboard-page admin-setup-page">
    <p class="section-kicker">{{ completed ? 'Host 管理' : 'Host 首次登录' }}</p>
    <h1>{{ pageTitle }}</h1>
    <p v-if="!completed" class="dashboard-lead">
      先确认站点、PostgreSQL、Redis
      与其它运行参数。系统邮箱可以暂不配置，也不会阻止初始化；需要时可先单独测试，初始化完成后仍可随时修改。保存时会确认数据服务、迁移目标数据库并创建同一管理员，成功后更新安装目录的
      .env，同时把完整邮箱配置加密保存到数据库。
    </p>
    <p v-else class="dashboard-lead">
      管理系统邮件投递配置。数据库动态配置优先且保存后立即生效；恢复本地配置后，Host 会使用 .env
      作为保底。
    </p>

    <div v-if="completed" class="dashboard-card setup-complete" role="status">
      <strong>首次初始化已经完成</strong>
      <p>
        如果刚刚完成初始化，请在安装目录重新运行 <code>start.sh</code>（Windows 为
        <code>Start.ps1 -Background</code>）。Host
        重启后会使用其它新配置；数据库邮箱配置无需重启即可生效。
      </p>
    </div>
    <p v-if="errorMessage" class="form-message form-error" role="alert">{{ errorMessage }}</p>
    <p v-if="successMessage" class="form-message" role="status">{{ successMessage }}</p>
    <p v-if="loading" class="inline-status" role="status">正在读取当前配置…</p>

    <form
      v-else-if="!completed"
      class="dashboard-card settings-form setup-form"
      @submit.prevent="submit"
    >
      <fieldset>
        <legend>站点与数据服务</legend>
        <label for="setup-public-url">站点公开地址</label>
        <input id="setup-public-url" v-model.trim="form.publicBaseUrl" type="url" required />
        <small>生产环境必须使用 HTTPS；仅 localhost / 127.0.0.1 / ::1 可使用 HTTP。</small>

        <label for="setup-database-url">PostgreSQL 连接地址</label>
        <input
          id="setup-database-url"
          v-model.trim="form.databaseUrl"
          required
          spellcheck="false"
        />

        <label for="setup-redis-url">Redis 连接地址</label>
        <input id="setup-redis-url" v-model.trim="form.redisUrl" required spellcheck="false" />

        <label for="setup-origins">允许的浏览器来源（每行一个）</label>
        <textarea id="setup-origins" v-model="allowedOriginsText" rows="3" spellcheck="false" />
        <small>留空时自动使用站点公开地址。</small>
      </fieldset>

      <fieldset>
        <legend>系统邮箱</legend>
        <p class="setup-note">
          可选配置，不影响初始化。启用后会加密保存到数据库，同时写入本地配置作为保底。
        </p>
        <label class="checkbox-row">
          <input v-model="setupEmailEnabled" type="checkbox" />
          <span>初始化时配置系统邮箱</span>
        </label>
        <div v-if="setupEmailEnabled" class="form-grid">
          <label>
            <span>SMTP 主机</span>
            <input v-model.trim="form.smtpHost" />
          </label>
          <label>
            <span>SMTP 端口</span>
            <input v-model.number="form.smtpPort" type="number" min="1" max="65535" required />
          </label>
          <label>
            <span>SMTP 用户名</span>
            <input v-model.trim="form.smtpUser" autocomplete="username" />
          </label>
          <label>
            <span>SMTP 密码</span>
            <input
              v-model="smtpPassword"
              type="password"
              autocomplete="new-password"
              :placeholder="smtpPasswordConfigured ? '留空保留当前密码' : '未配置'"
            />
          </label>
        </div>
        <label v-if="setupEmailEnabled" for="setup-mail-from">系统发件人</label>
        <input v-if="setupEmailEnabled" id="setup-mail-from" v-model.trim="form.mailFrom" />
        <label v-if="setupEmailEnabled && smtpPasswordConfigured" class="checkbox-row">
          <input v-model="form.clearSmtpPassword" type="checkbox" />
          <span>清除已有 SMTP 密码</span>
        </label>
        <div v-if="setupEmailEnabled" class="email-test-row">
          <label>
            <span>测试收件地址</span>
            <input v-model.trim="testRecipient" type="email" />
          </label>
          <button
            class="button button-secondary"
            type="button"
            :disabled="testPending || !testRecipient"
            @click="sendTest(true)"
          >
            {{ testPending ? '正在发送…' : '测试发送' }}
          </button>
        </div>
      </fieldset>

      <fieldset>
        <legend>发布来源与其它参数</legend>
        <label>
          <span>Web / 服务端 GitHub 仓库</span>
          <input
            v-model.trim="form.webGithubRepository"
            required
            pattern="[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+"
          />
        </label>
        <label>
          <span>桌面端 GitHub 仓库</span>
          <input
            v-model.trim="form.desktopGithubRepository"
            required
            pattern="[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+"
          />
        </label>
        <label>
          <span>手机端 GitHub 仓库</span>
          <input
            v-model.trim="form.mobileGithubRepository"
            required
            pattern="[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+"
          />
        </label>
        <p class="setup-note">
          GitHub Access Token
          始终可选；公开仓库无需填写。私有仓库可在“软件版本”页分别配置三套加密凭据。
        </p>
        <label class="checkbox-row">
          <input v-model="form.registrationEnabled" type="checkbox" />
          <span>允许新用户注册</span>
        </label>
        <label class="checkbox-row">
          <input v-model="form.cookieSecure" type="checkbox" />
          <span>为会话 Cookie 启用 Secure 属性（仅 HTTPS）</span>
        </label>
        <small>
          生产模式不会自动勾选；启用后必须始终从 HTTPS 地址登录，否则浏览器无法建立会话。
        </small>
        <label class="checkbox-row">
          <input v-model="form.adminMfaRequired" type="checkbox" />
          <span>强制管理员完成 TOTP 二次验证</span>
        </label>
        <small>生产模式默认不强制；仅在管理员明确开启后保护后台路由。</small>
      </fieldset>

      <button class="button" type="submit" :disabled="pending">
        {{ pending ? '正在校验并初始化…' : '保存配置并完成初始化' }}
      </button>
    </form>

    <form
      v-if="completed && emailSettings"
      class="dashboard-card settings-form setup-form email-settings-form"
      @submit.prevent="saveEmail"
    >
      <header class="email-settings-header">
        <div>
          <h2>系统邮箱</h2>
          <p>
            当前来源：
            <strong>{{
              emailSettings.source === 'database'
                ? '数据库动态配置'
                : emailSettings.source === 'local'
                  ? '本地保底配置'
                  : '未配置'
            }}</strong>
          </p>
        </div>
        <span :class="['tag', { 'tag-muted': !emailSettings.configured }]">
          {{ emailSettings.configured ? '已配置' : '未配置' }}
        </span>
      </header>

      <div class="form-grid">
        <label>
          <span>SMTP 主机</span>
          <input v-model.trim="emailForm.smtpHost" required />
        </label>
        <label>
          <span>SMTP 端口</span>
          <input v-model.number="emailForm.smtpPort" type="number" min="1" max="65535" required />
        </label>
        <label>
          <span>SMTP 用户名</span>
          <input v-model.trim="emailForm.smtpUser" autocomplete="username" />
        </label>
        <label>
          <span>SMTP 密码</span>
          <input
            v-model="emailPassword"
            type="password"
            autocomplete="new-password"
            :placeholder="emailSettings.smtpPasswordConfigured ? '留空保留当前密码' : '未配置'"
          />
        </label>
      </div>
      <label>
        <span>系统发件人</span>
        <input v-model.trim="emailForm.mailFrom" required />
      </label>
      <label v-if="emailSettings.smtpPasswordConfigured" class="checkbox-row">
        <input v-model="emailForm.clearSmtpPassword" type="checkbox" />
        <span>保存时清除已有 SMTP 密码</span>
      </label>

      <div class="email-test-row">
        <label>
          <span>测试收件地址</span>
          <input v-model.trim="testRecipient" type="email" required />
        </label>
        <button
          class="button button-secondary"
          type="button"
          :disabled="testPending || !testRecipient"
          @click="sendTest(false)"
        >
          {{ testPending ? '正在发送…' : '测试当前填写配置' }}
        </button>
      </div>

      <div class="email-actions">
        <button class="button" type="submit" :disabled="emailPending">
          {{ emailPending ? '正在保存…' : '保存到数据库并立即启用' }}
        </button>
        <button
          v-if="emailSettings.source === 'database'"
          class="button button-secondary"
          type="button"
          :disabled="emailPending"
          @click="resetEmail"
        >
          恢复本地保底配置
        </button>
      </div>
    </form>
  </section>
</template>

<style scoped>
.admin-setup-page {
  max-width: 920px;
  margin-inline: auto;
}
.setup-form {
  display: grid;
  gap: 26px;
}
.setup-form fieldset {
  display: grid;
  gap: 10px;
  margin: 0;
  border: 0;
  padding: 0;
}
.setup-form legend {
  margin-bottom: 8px;
  font-size: 1.1rem;
  font-weight: 750;
}
.setup-form input,
.setup-form textarea {
  width: 100%;
}
.setup-form small,
.setup-note,
.setup-complete p {
  margin: 0;
  color: var(--ink-soft);
}
.setup-complete {
  display: grid;
  gap: 8px;
  border-color: var(--mint);
  background: var(--brand-tint);
}
.setup-complete code {
  color: var(--teal-dark);
}
.email-settings-header,
.email-actions,
.email-test-row {
  display: flex;
  align-items: end;
  justify-content: space-between;
  gap: 14px;
}
.email-settings-header h2,
.email-settings-header p {
  margin: 0;
}
.email-settings-header p {
  margin-top: 6px;
  color: var(--ink-soft);
}
.email-test-row label {
  flex: 1;
}
@media (max-width: 680px) {
  .email-actions,
  .email-test-row {
    align-items: stretch;
    flex-direction: column;
  }
}
</style>
