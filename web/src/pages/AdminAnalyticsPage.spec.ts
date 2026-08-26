import { createHead } from '@unhead/vue/client'
import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import { getAdminAnalyticsOverview, listAdminLoginEvents } from '@/api/adminAnalytics'

import AdminAnalyticsPage from './AdminAnalyticsPage.vue'

vi.mock('@/api/adminAnalytics', () => ({
  getAdminAnalyticsOverview: vi.fn(),
  listAdminLoginEvents: vi.fn(),
}))

describe('AdminAnalyticsPage', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    vi.mocked(getAdminAnalyticsOverview).mockResolvedValue({
      range: {
        from: '2026-07-01T16:00:00Z',
        to: '2026-07-31T16:00:00Z',
        timezone: 'Asia/Shanghai',
        granularity: 'day',
      },
      summary: {
        pageViews: 128,
        uniqueIps: 42,
        downloadEvents: 37,
        loginEvents: 7,
        uniqueLoginIps: 5,
        downloadedVisitorIps: 12,
        registeredVisitorIps: 4,
        visitorDownloadRate: 12 / 42,
        visitorRegistrationRate: 4 / 42,
      },
      daily: [
        { date: '2026-07-22', pageViews: 12, uniqueIps: 8, downloadEvents: 5, loginEvents: 2 },
      ],
      timeline: [
        {
          bucketStart: '2026-07-21T16:00:00Z',
          pageViews: 12,
          uniqueIps: 8,
          downloadEvents: 5,
          loginEvents: 2,
          downloadedVisitorIps: 3,
          registeredVisitorIps: 1,
          visitorDownloadRate: 3 / 8,
          visitorRegistrationRate: 1 / 8,
        },
      ],
      regions: [
        {
          countryCode: 'CN',
          countryName: '中国',
          regionName: '北京市',
          cityName: '北京',
          pageViews: 80,
          uniqueIps: 25,
        },
      ],
      ips: [
        {
          ip: '203.0.113.8',
          countryCode: 'CN',
          countryName: '中国',
          regionName: '北京市',
          cityName: '北京',
          pageViews: 9,
          lastSeenAt: '2026-07-22T03:00:00Z',
        },
      ],
      recentNewIps: [
        {
          ip: '203.0.113.9',
          countryCode: 'CN',
          countryName: '中国',
          regionName: '浙江省',
          cityName: '杭州',
          pageViews: 3,
          firstSeenAt: '2026-07-22T01:00:00Z',
          lastSeenAt: '2026-07-22T03:00:00Z',
          downloadedSameDay: true,
          registeredSameDay: false,
        },
      ],
      sources: [
        { referrerHost: 'search.example.test', pageViews: 80, uniqueIps: 25 },
        { referrerHost: '', pageViews: 48, uniqueIps: 17 },
      ],
      paths: [{ path: '/pricing', pageViews: 50, uniqueIps: 30 }],
    })
    vi.mocked(listAdminLoginEvents).mockResolvedValue({
      items: [
        {
          id: 1,
          userId: '11111111-1111-4111-8111-111111111111',
          email: 'member@example.test',
          displayName: 'Member',
          ip: '198.51.100.20',
          countryCode: 'CN',
          countryName: '中国',
          regionName: '上海市',
          cityName: '上海',
          userAgentSummary: 'Test Browser',
          loginMethod: 'password',
          loggedInAt: '2026-07-22T02:00:00Z',
        },
      ],
      total: 1,
      limit: 50,
      offset: 0,
    })
  })

  it('shows access, region, IP, path and account login statistics', async () => {
    const wrapper = mount(AdminAnalyticsPage, { global: { plugins: [createHead()] } })
    await flushPromises()

    expect(getAdminAnalyticsOverview).toHaveBeenCalledOnce()
    expect(listAdminLoginEvents).toHaveBeenCalledOnce()
    expect(wrapper.text()).toContain('128')
    expect(wrapper.text()).toContain('下载次数')
    expect(wrapper.text()).toContain('37')
    expect(wrapper.text()).toContain('28.6%')
    expect(wrapper.text()).toContain('9.5%')
    expect(wrapper.text()).toContain('北京市')
    expect(wrapper.text()).toContain('search.example.test')
    expect(wrapper.text()).toContain('直接访问')
    expect(wrapper.text()).toContain('62.5%')
    expect(wrapper.text()).toContain('37.5%')
    expect(wrapper.text()).toContain('203.0.113.8')
    expect(wrapper.text()).toContain('203.0.113.9')
    expect(wrapper.text()).toContain('已下载')
    expect(wrapper.text()).toContain('/pricing')
    expect(wrapper.text()).toContain('member@example.test')
    expect(wrapper.text()).toContain('198.51.100.20')
    expect(wrapper.findAll('.analytics-breakdown-scroll')).toHaveLength(3)
    expect(wrapper.findAll('.analytics-breakdown-panel')).toHaveLength(3)
  })

  it('loads the most recent seven days by default', async () => {
    const wrapper = mount(AdminAnalyticsPage, { global: { plugins: [createHead()] } })
    await flushPromises()

    expect(wrapper.get('.analytics-preset-button.active').text()).toBe('近 7 日')
    expect(getAdminAnalyticsOverview).toHaveBeenCalledOnce()
    const params = vi.mocked(getAdminAnalyticsOverview).mock.calls[0]![0]
    expect(params.granularity).toBe('day')
    expect(Date.parse(params.to) - Date.parse(params.from)).toBe(7 * 86_400_000)
  })

  it('loads the current day as an hourly 24-hour range', async () => {
    const wrapper = mount(AdminAnalyticsPage, { global: { plugins: [createHead()] } })
    await flushPromises()
    vi.mocked(getAdminAnalyticsOverview).mockClear()
    vi.mocked(listAdminLoginEvents).mockClear()

    const todayButton = wrapper.findAll('button').find((button) => button.text().trim() === '今日')
    expect(todayButton).toBeDefined()
    await todayButton!.trigger('click')
    await flushPromises()

    expect(getAdminAnalyticsOverview).toHaveBeenCalledOnce()
    const params = vi.mocked(getAdminAnalyticsOverview).mock.calls[0]![0]
    expect(params.granularity).toBe('hour')
    expect(Date.parse(params.to) - Date.parse(params.from)).toBe(86_400_000)
    expect(listAdminLoginEvents).toHaveBeenCalledOnce()
  })
})
