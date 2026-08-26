<script setup lang="ts">
import { useHead } from '@unhead/vue'
import { onMounted, reactive, ref } from 'vue'

import { problemMessage } from '@/api/auth'
import { applySystemSetup, getSystemSetup, type ApplySystemSetupRequest } from '@/api/systemSetup'

useHead({
  title: '首次系统初始化｜WenzWork',
  meta: [{ name: 'robots', content: 'noindex, nofollow' }],
})

const loading = ref(true)
const pending = ref(false)
const completed = ref(false)
const errorMessage = ref('')
const allowedOriginsText = ref('')
const smtpPassword = ref('')
const smtpPasswordConfigured = ref(false)

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
    completed.value = !settings.required
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
  if (smtpPassword.value) request.smtpPassword = smtpPassword.value
  try {
    await applySystemSetup(request)
    completed.value = true
    smtpPassword.value = ''
  } catch (error) {
    errorMessage.value = problemMessage(error, '系统初始化失败，请检查配置后重试。')
  } finally {
    pending.value = false
  }
}

onMounted(load)
</script>

<template>
  <section class="dashboard-page admin-setup-page">
    <p class="section-kicker">Host 首次登录</p>
    <h1>完成系统初始化</h1>
    <p class="dashboard-lead">
      先确认站点、PostgreSQL、Redis
      与系统邮箱。保存时会先确认数据库连接，并向当前默认管理员邮箱发送测试邮件；随后迁移目标数据库并创建同一管理员。全部成功后才会更新安装目录的
      .env。
    </p>

    <div v-if="completed" class="dashboard-card setup-complete" role="status">
      <strong>配置已经安全写入 .env</strong>
      <p>
        请在安装目录重新运行 <code>start.sh</code>（Windows 为
        <code>Start.ps1 -Background</code>）。Host 重启后会使用新配置，默认管理员明文密码已从 .env
        清除。
      </p>
    </div>
    <p v-if="errorMessage" class="form-message form-error" role="alert">{{ errorMessage }}</p>
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
          完成初始化前会向当前默认管理员邮箱实投一封测试邮件；SMTP 服务器接受投递后才能保存配置。
        </p>
        <div class="form-grid">
          <label>
            <span>SMTP 主机</span>
            <input v-model.trim="form.smtpHost" required />
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
        <label for="setup-mail-from">系统发件人</label>
        <input id="setup-mail-from" v-model.trim="form.mailFrom" required />
        <label v-if="smtpPasswordConfigured" class="checkbox-row">
          <input v-model="form.clearSmtpPassword" type="checkbox" />
          <span>清除已有 SMTP 密码</span>
        </label>
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
</style>
