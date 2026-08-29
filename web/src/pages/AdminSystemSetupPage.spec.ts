import { createHead } from '@unhead/vue/client'
import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import {
  getSystemEmailSettings,
  resetSystemEmailSettings,
  testSystemEmailSettings,
  updateSystemEmailSettings,
} from '@/api/systemEmail'
import { applySystemSetup, getSystemSetup } from '@/api/systemSetup'
import { useAuthStore } from '@/stores/auth'

import AdminSystemSetupPage from './AdminSystemSetupPage.vue'

vi.mock('@/api/systemSetup', () => ({
  applySystemSetup: vi.fn(),
  getSystemSetup: vi.fn(),
}))
vi.mock('@/api/systemEmail', () => ({
  getSystemEmailSettings: vi.fn(),
  resetSystemEmailSettings: vi.fn(),
  testSystemEmailSettings: vi.fn(),
  updateSystemEmailSettings: vi.fn(),
}))

const setupSettings = {
  required: true,
  publicBaseUrl: 'http://localhost:8080',
  databaseUrl: 'postgres://database',
  redisUrl: 'redis://redis',
  smtpHost: 'smtp.local.test',
  smtpPort: 1025,
  smtpUser: '',
  smtpConfigured: true,
  smtpPasswordConfigured: false,
  mailFrom: 'noreply@local.test',
  cookieSecure: false,
  adminMfaRequired: false,
  registrationEnabled: true,
  allowedOrigins: ['http://localhost:8080'],
  webGithubRepository: 'acme/web',
  desktopGithubRepository: 'acme/desktop',
  mobileGithubRepository: 'acme/mobile',
}

const localEmailSettings = {
  configured: true,
  source: 'local' as const,
  smtpHost: 'smtp.local.test',
  smtpPort: 1025,
  smtpUser: '',
  smtpPasswordConfigured: false,
  mailFrom: 'noreply@local.test',
  version: 1,
  updatedAt: '2026-08-27T00:00:00Z',
}

describe('AdminSystemSetupPage', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    setActivePinia(createPinia())
    useAuthStore().user = {
      id: 'admin-1',
      email: 'admin@example.test',
      displayName: 'Administrator',
      status: 'active',
      emailVerifiedAt: '2026-08-27T00:00:00Z',
      roles: ['super_admin'],
    }
    vi.mocked(getSystemSetup).mockResolvedValue(setupSettings)
    vi.mocked(getSystemEmailSettings).mockResolvedValue(localEmailSettings)
    vi.mocked(applySystemSetup).mockResolvedValue({
      settings: { ...setupSettings, required: false },
      restartRequired: true,
    })
  })

  it('allows first-time setup to finish with system email disabled', async () => {
    const wrapper = mount(AdminSystemSetupPage, { global: { plugins: [createHead()] } })
    await flushPromises()

    const checkbox = wrapper
      .findAll('label.checkbox-row')
      .find((item) => item.text().includes('初始化时配置系统邮箱'))!
      .get('input')
    await checkbox.setValue(false)
    await wrapper.get('form.setup-form').trigger('submit')
    await flushPromises()

    expect(applySystemSetup).toHaveBeenCalledWith(
      expect.objectContaining({
        smtpHost: '',
        smtpUser: '',
        mailFrom: '',
        clearSmtpPassword: true,
      }),
    )
  })

  it('tests a draft and saves a database override after initialization', async () => {
    vi.mocked(getSystemSetup).mockResolvedValue({ ...setupSettings, required: false })
    vi.mocked(updateSystemEmailSettings).mockResolvedValue({
      ...localEmailSettings,
      source: 'database',
      smtpHost: 'smtp.database.test',
      version: 2,
    })
    const wrapper = mount(AdminSystemSetupPage, { global: { plugins: [createHead()] } })
    await flushPromises()

    const host = wrapper
      .findAll('input')
      .find((input) => input.element.value === 'smtp.local.test')!
    await host.setValue('smtp.database.test')
    const testButton = wrapper
      .findAll('button')
      .find((button) => button.text().includes('测试当前填写配置'))!
    await testButton.trigger('click')
    await flushPromises()
    expect(testSystemEmailSettings).toHaveBeenCalledWith(
      expect.objectContaining({
        smtpHost: 'smtp.database.test',
        recipient: 'admin@example.test',
      }),
    )

    await wrapper.get('form.email-settings-form').trigger('submit')
    await flushPromises()
    expect(updateSystemEmailSettings).toHaveBeenCalledWith(
      expect.objectContaining({ smtpHost: 'smtp.database.test', expectedVersion: 1 }),
    )
    expect(wrapper.text()).toContain('数据库动态配置')
    expect(resetSystemEmailSettings).not.toHaveBeenCalled()
  })
})
