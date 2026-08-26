<script setup lang="ts">
import { useHead } from '@unhead/vue'
import { computed, onMounted, ref } from 'vue'

import {
  getAdminAnalyticsOverview,
  listAdminLoginEvents,
  type AdminAnalyticsOverview,
  type AdminLoginEventList,
  type AnalyticsGranularity,
  type AnalyticsRangeParams,
} from '@/api/adminAnalytics'
import { problemMessage } from '@/api/auth'

useHead({ title: '访问统计｜WenzWork', meta: [{ name: 'robots', content: 'noindex, nofollow' }] })

const today = new Intl.DateTimeFormat('en-CA', { timeZone: 'Asia/Shanghai' }).format(new Date())
const shiftDate = (value: string, days: number) => {
  const date = new Date(`${value}T00:00:00Z`)
  date.setUTCDate(date.getUTCDate() + days)
  return date.toISOString().slice(0, 10)
}

const fromDate = ref(shiftDate(today, -6))
const toDate = ref(today)
const rangePreset = ref<'today' | '7d' | '30d' | 'custom'>('7d')
const rangePresetOptions = [
  { value: 'today', label: '今日' },
  { value: '7d', label: '近 7 日' },
  { value: '30d', label: '近 30 日' },
] as const
const granularity = ref<AnalyticsGranularity>('day')
const overview = ref<AdminAnalyticsOverview | null>(null)
const loginEvents = ref<AdminLoginEventList | null>(null)
const loginQuery = ref('')
const loginOffset = ref(0)
const pageSize = 50
const loading = ref(true)
const loginLoading = ref(false)
const errorMessage = ref('')

const rangeParams = (): AnalyticsRangeParams => ({
  from: new Date(`${fromDate.value}T00:00:00+08:00`).toISOString(),
  to: new Date(`${shiftDate(toDate.value, 1)}T00:00:00+08:00`).toISOString(),
})

const rangeDurationDays = () => {
  const from = Date.parse(`${fromDate.value}T00:00:00Z`)
  const to = Date.parse(`${shiftDate(toDate.value, 1)}T00:00:00Z`)
  return (to - from) / 86_400_000
}

const validateRange = () => {
  const from = Date.parse(`${fromDate.value}T00:00:00Z`)
  const to = Date.parse(`${toDate.value}T00:00:00Z`)
  return (
    Number.isFinite(from) &&
    Number.isFinite(to) &&
    to >= from &&
    rangeDurationDays() <= 366 &&
    (granularity.value !== 'hour' || rangeDurationDays() <= 31)
  )
}

const loadLogins = async () => {
  if (!validateRange()) return
  loginLoading.value = true
  try {
    loginEvents.value = await listAdminLoginEvents({
      ...rangeParams(),
      q: loginQuery.value.trim() || undefined,
      limit: pageSize,
      offset: loginOffset.value,
    })
  } catch (error) {
    errorMessage.value = problemMessage(error, '暂时无法读取登录 IP 记录。')
  } finally {
    loginLoading.value = false
  }
}

const load = async () => {
  if (!validateRange()) {
    errorMessage.value = '日期范围最多为 366 天；选择按小时统计时最多查询 31 天。'
    return
  }
  loading.value = true
  errorMessage.value = ''
  loginOffset.value = 0
  try {
    const params = rangeParams()
    const [overviewResult, loginResult] = await Promise.all([
      getAdminAnalyticsOverview({ ...params, granularity: granularity.value }),
      listAdminLoginEvents({ ...params, limit: pageSize, offset: 0 }),
    ])
    overview.value = overviewResult
    loginEvents.value = loginResult
  } catch (error) {
    errorMessage.value = problemMessage(error, '暂时无法读取访问统计。')
  } finally {
    loading.value = false
  }
}

const selectRangePreset = async (preset: 'today' | '7d' | '30d') => {
  rangePreset.value = preset
  toDate.value = today
  if (preset === 'today') {
    fromDate.value = today
    granularity.value = 'hour'
  } else {
    fromDate.value = shiftDate(today, preset === '7d' ? -6 : -29)
    granularity.value = 'day'
  }
  await load()
}

const markCustomRange = () => {
  rangePreset.value = 'custom'
}

const searchLogins = async () => {
  errorMessage.value = ''
  loginOffset.value = 0
  await loadLogins()
}

const changeLoginPage = async (direction: -1 | 1) => {
  const next = loginOffset.value + direction * pageSize
  if (next < 0 || (loginEvents.value && next >= loginEvents.value.total)) return
  loginOffset.value = next
  errorMessage.value = ''
  await loadLogins()
}

const maximumTimelineIPs = computed(() =>
  Math.max(1, ...(overview.value?.timeline.map((item) => item.uniqueIps) ?? [1])),
)
const timelineBarHeight = (uniqueIps: number) =>
  uniqueIps === 0
    ? '0%'
    : `${Math.max(6, Math.round((uniqueIps / maximumTimelineIPs.value) * 100))}%`
const canShowNextLogins = computed(() =>
  Boolean(loginEvents.value && loginOffset.value + pageSize < loginEvents.value.total),
)

const formatCount = (value: number) => new Intl.NumberFormat('zh-CN').format(value)
const formatPercent = (value: number) =>
  new Intl.NumberFormat('zh-CN', { style: 'percent', maximumFractionDigits: 1 }).format(value)
const formatDateTime = (value: string) =>
  new Intl.DateTimeFormat('zh-CN', {
    dateStyle: 'medium',
    timeStyle: 'short',
    timeZone: 'Asia/Shanghai',
  }).format(new Date(value))
const formatBucket = (value: string) =>
  new Intl.DateTimeFormat('zh-CN', {
    month: '2-digit',
    day: '2-digit',
    ...(granularity.value === 'hour' ? { hour: '2-digit', hour12: false } : {}),
    timeZone: 'Asia/Shanghai',
  }).format(new Date(value))
const loginMethodLabel = (value: 'password' | 'app_device') =>
  value === 'app_device' ? '桌面客户端' : '网页登录'
const locationLabel = (country: string, region: string, city: string) =>
  [country || '未知', region, city]
    .filter((item, index, values) => item && values.indexOf(item) === index)
    .join(' · ')
const sourceShare = (pageViews: number) =>
  formatPercent(pageViews / Math.max(1, overview.value?.summary.pageViews ?? 0))

onMounted(load)
</script>

<template>
  <section class="dashboard-page admin-wide-page analytics-page">
    <p class="section-kicker">流量与安全</p>
    <div class="analytics-heading-row">
      <div>
        <h1>访问统计</h1>
        <p class="dashboard-lead">
          查看独立 IP 趋势、访问来源、新访问 IP、下载与注册转化及账户登录记录。
        </p>
      </div>
      <span class="tag">Asia/Shanghai</span>
    </div>

    <form class="dashboard-card analytics-range-form" @submit.prevent="load">
      <div class="field-group analytics-preset-group">
        <label>快捷范围</label>
        <div class="analytics-range-presets" aria-label="快捷统计范围">
          <button
            v-for="item in rangePresetOptions"
            :key="item.value"
            class="analytics-preset-button"
            :class="{ active: rangePreset === item.value }"
            type="button"
            :disabled="loading"
            @click="selectRangePreset(item.value)"
          >
            {{ item.label }}
          </button>
        </div>
      </div>
      <div class="field-group">
        <label for="analytics-from">开始日期</label>
        <input
          id="analytics-from"
          v-model="fromDate"
          type="date"
          required
          @change="markCustomRange"
        />
      </div>
      <div class="field-group">
        <label for="analytics-to">结束日期</label>
        <input id="analytics-to" v-model="toDate" type="date" required @change="markCustomRange" />
      </div>
      <div class="field-group">
        <label for="analytics-granularity">统计粒度</label>
        <select id="analytics-granularity" v-model="granularity">
          <option value="hour">按小时</option>
          <option value="day">按日</option>
        </select>
      </div>
      <button class="button" type="submit" :disabled="loading">
        {{ loading ? '读取中…' : '更新统计' }}
      </button>
    </form>

    <p v-if="errorMessage" class="form-message form-error" role="alert">{{ errorMessage }}</p>
    <p v-if="loading && !overview" class="inline-status" role="status">正在汇总访问数据…</p>

    <template v-if="overview">
      <div class="analytics-summary-grid" aria-label="访问统计摘要">
        <article class="dashboard-card analytics-summary-card">
          <span>页面访问</span><strong>{{ formatCount(overview.summary.pageViews) }}</strong
          ><small>PV</small>
        </article>
        <article class="dashboard-card analytics-summary-card">
          <span>独立 IP</span><strong>{{ formatCount(overview.summary.uniqueIps) }}</strong
          ><small>访问来源</small>
        </article>
        <article class="dashboard-card analytics-summary-card">
          <span>下载次数</span><strong>{{ formatCount(overview.summary.downloadEvents) }}</strong
          ><small>成功下载请求</small>
        </article>
        <article class="dashboard-card analytics-summary-card">
          <span>访问下载率</span
          ><strong>{{ formatPercent(overview.summary.visitorDownloadRate) }}</strong
          ><small
            >{{ formatCount(overview.summary.downloadedVisitorIps) }} 个访问 IP 当天下载</small
          >
        </article>
        <article class="dashboard-card analytics-summary-card">
          <span>访问注册率</span
          ><strong>{{ formatPercent(overview.summary.visitorRegistrationRate) }}</strong
          ><small
            >{{ formatCount(overview.summary.registeredVisitorIps) }} 个访问 IP 当天注册</small
          >
        </article>
        <article class="dashboard-card analytics-summary-card">
          <span>账户登录</span><strong>{{ formatCount(overview.summary.loginEvents) }}</strong
          ><small>成功登录</small>
        </article>
        <article class="dashboard-card analytics-summary-card">
          <span>登录 IP</span><strong>{{ formatCount(overview.summary.uniqueLoginIps) }}</strong
          ><small>独立来源</small>
        </article>
      </div>
      <p class="analytics-definition-note">
        下载次数按成功获取下载地址或开始代理传输的请求计数，重复下载会分别累计。下载率和注册率以独立访问
        IP 为分母，并判断该 IP 是否在同一个 Asia/Shanghai
        自然日完成下载或注册；历史转化数据从本统计功能启用后开始累积。
      </p>

      <section
        class="dashboard-card analytics-panel analytics-trend-panel"
        aria-labelledby="analytics-trend-title"
      >
        <div class="analytics-panel-heading">
          <div>
            <p class="card-label">
              {{ overview.range.granularity === 'hour' ? '按小时' : '按日' }}趋势
            </p>
            <h2 id="analytics-trend-title">IP 访问、下载与转化</h2>
          </div>
          <span>{{ overview.timeline.length }} 个时段</span>
        </div>
        <div v-if="overview.timeline.length" class="analytics-chart-scroll">
          <div class="analytics-bar-chart" role="img" aria-label="独立 IP 访问、下载与转化趋势图">
            <div
              v-for="item in overview.timeline"
              :key="item.bucketStart"
              class="analytics-bar-column"
            >
              <div class="analytics-bar-track">
                <span
                  class="analytics-bar"
                  :style="{ height: timelineBarHeight(item.uniqueIps) }"
                  :title="`${formatBucket(item.bucketStart)}：${item.uniqueIps} 个访问 IP，${item.pageViews} PV，${item.downloadEvents} 次下载；下载率 ${formatPercent(item.visitorDownloadRate)}，注册率 ${formatPercent(item.visitorRegistrationRate)}，${item.loginEvents} 次登录`"
                ></span>
              </div>
              <strong>{{ formatCount(item.uniqueIps) }} IP</strong>
              <small>{{ formatBucket(item.bucketStart) }}</small>
              <span class="analytics-bar-conversion">
                下载 {{ formatCount(item.downloadEvents) }} 次<br />转
                {{ formatPercent(item.visitorDownloadRate) }} · 注
                {{ formatPercent(item.visitorRegistrationRate) }}
              </span>
            </div>
          </div>
        </div>
        <p v-else class="inline-status">所选时间内没有访问记录。</p>
      </section>

      <div class="analytics-breakdown-grid">
        <section
          class="dashboard-card analytics-panel analytics-breakdown-panel"
          aria-labelledby="analytics-region-title"
        >
          <div class="analytics-panel-heading">
            <div>
              <p class="card-label">地区统计</p>
              <h2 id="analytics-region-title">访问地区</h2>
            </div>
          </div>
          <div
            v-if="overview.regions.length"
            class="analytics-table-scroll analytics-breakdown-scroll"
          >
            <table class="analytics-table">
              <thead>
                <tr>
                  <th>地区</th>
                  <th>访问</th>
                  <th>IP</th>
                </tr>
              </thead>
              <tbody>
                <tr
                  v-for="item in overview.regions"
                  :key="`${item.countryCode}-${item.regionName}-${item.cityName}`"
                >
                  <td>{{ locationLabel(item.countryName, item.regionName, item.cityName) }}</td>
                  <td>{{ formatCount(item.pageViews) }}</td>
                  <td>{{ formatCount(item.uniqueIps) }}</td>
                </tr>
              </tbody>
            </table>
          </div>
          <p v-else class="inline-status">
            暂无地区数据；本地数据库与两个查询服务均未返回结果时，公网地区显示为未知。
          </p>
        </section>

        <section
          class="dashboard-card analytics-panel analytics-breakdown-panel"
          aria-labelledby="analytics-source-title"
        >
          <div class="analytics-panel-heading">
            <div>
              <p class="card-label">流量来源</p>
              <h2 id="analytics-source-title">访问来源</h2>
            </div>
          </div>
          <div
            v-if="overview.sources.length"
            class="analytics-table-scroll analytics-breakdown-scroll"
          >
            <table class="analytics-table">
              <thead>
                <tr>
                  <th>来源</th>
                  <th>访问</th>
                  <th>IP</th>
                  <th>占比</th>
                </tr>
              </thead>
              <tbody>
                <tr
                  v-for="item in overview.sources"
                  :key="item.referrerHost ? `host:${item.referrerHost}` : 'direct'"
                >
                  <td>
                    <code v-if="item.referrerHost">{{ item.referrerHost }}</code>
                    <span v-else>直接访问</span>
                  </td>
                  <td>{{ formatCount(item.pageViews) }}</td>
                  <td>{{ formatCount(item.uniqueIps) }}</td>
                  <td>{{ sourceShare(item.pageViews) }}</td>
                </tr>
              </tbody>
            </table>
          </div>
          <p v-else class="inline-status">所选时间内没有访问来源记录。</p>
        </section>

        <section
          class="dashboard-card analytics-panel analytics-breakdown-panel"
          aria-labelledby="analytics-path-title"
        >
          <div class="analytics-panel-heading">
            <div>
              <p class="card-label">页面排行</p>
              <h2 id="analytics-path-title">热门路径</h2>
            </div>
          </div>
          <div
            v-if="overview.paths.length"
            class="analytics-table-scroll analytics-breakdown-scroll"
          >
            <table class="analytics-table">
              <thead>
                <tr>
                  <th>路径</th>
                  <th>访问</th>
                  <th>IP</th>
                </tr>
              </thead>
              <tbody>
                <tr v-for="item in overview.paths" :key="item.path">
                  <td>
                    <code>{{ item.path }}</code>
                  </td>
                  <td>{{ formatCount(item.pageViews) }}</td>
                  <td>{{ formatCount(item.uniqueIps) }}</td>
                </tr>
              </tbody>
            </table>
          </div>
          <p v-else class="inline-status">所选时间内没有页面访问。</p>
        </section>
      </div>

      <section class="dashboard-card analytics-panel" aria-labelledby="analytics-new-ip-title">
        <div class="analytics-panel-heading">
          <div>
            <p class="card-label">首次访问</p>
            <h2 id="analytics-new-ip-title">最近新增访问 IP</h2>
          </div>
          <span>所选范围内最近 20 条</span>
        </div>
        <div v-if="overview.recentNewIps.length" class="analytics-table-scroll">
          <table class="analytics-table analytics-new-ip-table">
            <thead>
              <tr>
                <th>IP</th>
                <th>地区</th>
                <th>首次访问</th>
                <th>最后访问</th>
                <th>累计访问</th>
                <th>首日下载</th>
                <th>首日注册</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="item in overview.recentNewIps" :key="item.ip">
                <td>
                  <code>{{ item.ip }}</code>
                </td>
                <td>{{ locationLabel(item.countryName, item.regionName, item.cityName) }}</td>
                <td>{{ formatDateTime(item.firstSeenAt) }}</td>
                <td>{{ formatDateTime(item.lastSeenAt) }}</td>
                <td>{{ formatCount(item.pageViews) }}</td>
                <td>
                  <span
                    class="analytics-conversion-status"
                    :class="{ converted: item.downloadedSameDay }"
                  >
                    {{ item.downloadedSameDay ? '已下载' : '未下载' }}
                  </span>
                </td>
                <td>
                  <span
                    class="analytics-conversion-status"
                    :class="{ converted: item.registeredSameDay }"
                  >
                    {{ item.registeredSameDay ? '已注册' : '未注册' }}
                  </span>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
        <p v-else class="inline-status">所选范围内没有首次出现的访问 IP。</p>
      </section>

      <section class="dashboard-card analytics-panel" aria-labelledby="analytics-ip-title">
        <div class="analytics-panel-heading">
          <div>
            <p class="card-label">IP 统计</p>
            <h2 id="analytics-ip-title">访问 IP 排行</h2>
          </div>
          <span>最多显示 50 项</span>
        </div>
        <div v-if="overview.ips.length" class="analytics-table-scroll">
          <table class="analytics-table analytics-ip-table">
            <thead>
              <tr>
                <th>IP</th>
                <th>地区</th>
                <th>访问</th>
                <th>最后访问</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="item in overview.ips" :key="item.ip">
                <td>
                  <code>{{ item.ip }}</code>
                </td>
                <td>{{ locationLabel(item.countryName, item.regionName, item.cityName) }}</td>
                <td>{{ formatCount(item.pageViews) }}</td>
                <td>{{ formatDateTime(item.lastSeenAt) }}</td>
              </tr>
            </tbody>
          </table>
        </div>
        <p v-else class="inline-status">所选时间内没有 IP 访问记录。</p>
      </section>
    </template>

    <section
      class="dashboard-card analytics-panel analytics-login-panel"
      aria-labelledby="analytics-login-title"
    >
      <div class="analytics-panel-heading analytics-login-heading">
        <div>
          <p class="card-label">账户安全</p>
          <h2 id="analytics-login-title">登录 IP 记录</h2>
        </div>
        <form class="analytics-login-search" @submit.prevent="searchLogins">
          <input
            v-model.trim="loginQuery"
            type="search"
            maxlength="100"
            placeholder="邮箱、名称或 IP"
            aria-label="搜索登录记录"
          />
          <button class="button button-secondary" type="submit" :disabled="loginLoading">
            搜索
          </button>
        </form>
      </div>
      <p class="analytics-sensitive-note">
        完整 IP 属于敏感安全数据；本页面仅向具备审计权限的管理员开放，管理员 MFA
        门禁开启时还必须完成二次验证。
      </p>
      <p v-if="loginLoading" class="inline-status" role="status">正在读取登录记录…</p>
      <div v-else-if="loginEvents?.items.length" class="analytics-table-scroll">
        <table class="analytics-table analytics-login-table">
          <thead>
            <tr>
              <th>账户</th>
              <th>方式</th>
              <th>登录 IP</th>
              <th>地区</th>
              <th>设备</th>
              <th>时间</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="event in loginEvents.items" :key="event.id">
              <td>
                <strong>{{ event.displayName }}</strong
                ><small>{{ event.email }}</small>
              </td>
              <td>{{ loginMethodLabel(event.loginMethod) }}</td>
              <td>
                <code>{{ event.ip }}</code>
              </td>
              <td>{{ locationLabel(event.countryName, event.regionName, event.cityName) }}</td>
              <td>{{ event.userAgentSummary || '未知设备' }}</td>
              <td>{{ formatDateTime(event.loggedInAt) }}</td>
            </tr>
          </tbody>
        </table>
      </div>
      <p v-else class="inline-status">所选条件下没有登录记录。</p>
      <div v-if="loginEvents && loginEvents.total > pageSize" class="analytics-pagination">
        <span
          >第 {{ loginOffset + 1 }}–{{ Math.min(loginOffset + pageSize, loginEvents.total) }} 条，共
          {{ formatCount(loginEvents.total) }} 条</span
        >
        <div>
          <button
            class="button button-secondary"
            type="button"
            :disabled="loginOffset === 0 || loginLoading"
            @click="changeLoginPage(-1)"
          >
            上一页
          </button>
          <button
            class="button button-secondary"
            type="button"
            :disabled="!canShowNextLogins || loginLoading"
            @click="changeLoginPage(1)"
          >
            下一页
          </button>
        </div>
      </div>
    </section>
  </section>
</template>
