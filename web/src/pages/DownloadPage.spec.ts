import { flushPromises, mount } from '@vue/test-utils'
import { createHead } from '@unhead/vue/client'
import { createMemoryHistory, createRouter } from 'vue-router'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { execFileSync } from 'node:child_process'
import { existsSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

import { getLatestRelease, listReleases } from '@/api/catalog'

import DownloadPage from './DownloadPage.vue'

vi.mock('@/api/catalog', () => ({
  getLatestRelease: vi.fn(),
  listReleases: vi.fn(),
  isReleaseNotFound: () => false,
}))

const latestMock = vi.mocked(getLatestRelease)
const listMock = vi.mocked(listReleases)

describe('DownloadPage', () => {
  beforeEach(() => {
    vi.restoreAllMocks()
    latestMock.mockReset().mockRejectedValue(new Error('offline'))
    listMock.mockReset().mockRejectedValue(new Error('offline'))
  })

  it('keeps every platform and safety guidance visible when the API fails', async () => {
    const router = createRouter({
      history: createMemoryHistory(),
      routes: [{ path: '/:pathMatch(.*)*', component: { template: '<div />' } }],
    })
    const wrapper = mount(DownloadPage, {
      global: { plugins: [router, createHead()] },
    })

    await flushPromises()

    expect(wrapper.text()).toContain('版本服务暂时不可用')
    expect(wrapper.text()).toContain('Windows')
    expect(wrapper.text()).toContain('macOS')
    expect(wrapper.text()).toContain('Linux')
    expect(wrapper.text()).toContain('SHA-256')
    expect(wrapper.findAll('.platform-card')).toHaveLength(3)
  })

  it('orders the four download groups and exposes Device Agent assets separately', async () => {
    const release = {
      id: 'release-web-device-1',
      project: 'web' as const,
      version: '2.1.0',
      channel: 'stable' as const,
      title: 'Web deployment release',
      summary: 'Host、Relay 与 Device Agent 部署包。',
      releaseNotes: '新增受控端安装包',
      publishedAt: '2026-08-23T03:00:00Z',
      assets: [
        {
          id: 'asset-host-linux',
          platform: 'linux' as const,
          architecture: 'x64' as const,
          fileName: 'wenzwork-host-deployment-2.1.0-linux-amd64.tar.gz',
          fileSizeBytes: 20_000,
          sha256: 'a'.repeat(64),
          signatureStatus: 'valid' as const,
          downloadUrl: '/api/v1/release-assets/host/download',
        },
        {
          id: 'asset-device-linux',
          platform: 'linux' as const,
          architecture: 'x64' as const,
          fileName: 'wenzwork-device-agent-deployment-2.1.0-linux-amd64.tar.gz',
          fileSizeBytes: 15_000,
          sha256: 'b'.repeat(64),
          signatureStatus: 'valid' as const,
          downloadUrl: '/api/v1/release-assets/device/download',
        },
      ],
    }
    listMock.mockImplementation(async (project) => (project === 'web' ? [release] : []))
    const router = createRouter({
      history: createMemoryHistory(),
      routes: [{ path: '/:pathMatch(.*)*', component: { template: '<div />' } }],
    })
    const wrapper = mount(DownloadPage, {
      global: { plugins: [router, createHead()] },
    })
    await flushPromises()

    expect(
      wrapper.findAll('.download-project-option').map((button) => button.get('strong').text()),
    ).toEqual(['桌面端', '手机端', 'Web / 服务端', 'Device / 受控端'])
    expect(listMock.mock.calls.map(([project]) => project)).toEqual(['desktop', 'mobile', 'web'])

    await wrapper
      .findAll('.download-project-option')
      .find((button) => button.text().includes('Device / 受控端'))!
      .trigger('click')

    expect(wrapper.text()).toContain('wenzwork-device-agent-deployment-2.1.0-linux-amd64.tar.gz')
    expect(wrapper.text()).not.toContain('wenzwork-host-deployment-2.1.0-linux-amd64.tar.gz')
    expect(wrapper.findAll('.platform-card')).toHaveLength(3)
  })

  it('shows published platform states plus release introduction and update details', async () => {
    const release = {
      id: '9b8d28d3-b51d-49e1-8752-30ea43d824d2',
      project: 'desktop' as const,
      version: '1.2.3',
      channel: 'stable' as const,
      title: '桌面版更新',
      summary: '更快的项目打开速度与更稳定的编辑体验。',
      releaseNotes: '新增：macOS 安装包\n修复：大文档滚动卡顿',
      publishedAt: '2026-07-25T08:00:00Z',
      assets: [
        {
          id: '351e161a-cf53-47f7-8425-e4407615396f',
          platform: 'windows' as const,
          architecture: 'x64' as const,
          fileName: 'WenzWork-1.2.3.exe',
          fileSizeBytes: 80 * 1024 * 1024,
          sha256: 'a'.repeat(64),
          signatureStatus: 'valid' as const,
          downloadUrl: '/api/v1/releases/assets/windows',
        },
        {
          id: 'd3d131d6-a0b8-49c2-9367-12da1d16a063',
          platform: 'macos' as const,
          architecture: 'universal' as const,
          fileName: 'WenzWork-1.2.3.dmg',
          fileSizeBytes: 90 * 1024 * 1024,
          sha256: 'b'.repeat(64),
          signatureStatus: 'valid' as const,
          downloadUrl: '/api/v1/releases/assets/macos',
        },
        {
          id: '0a2fb5fb-71c2-4387-8f7c-2430c7104f47',
          platform: 'linux' as const,
          architecture: 'x64' as const,
          fileName: 'WenzWork-1.2.3.AppImage',
          fileSizeBytes: 85 * 1024 * 1024,
          sha256: 'c'.repeat(64),
          signatureStatus: 'valid' as const,
          downloadUrl: '/api/v1/releases/assets/linux',
        },
      ],
    }
    latestMock.mockResolvedValue(release)
    listMock.mockResolvedValue([release])
    const router = createRouter({
      history: createMemoryHistory(),
      routes: [{ path: '/:pathMatch(.*)*', component: { template: '<div />' } }],
    })
    const wrapper = mount(DownloadPage, {
      global: { plugins: [router, createHead()] },
    })

    await flushPromises()

    expect(wrapper.text()).toContain('已获取桌面端最新版本')
    expect(wrapper.get('.release-summary').text()).toContain('版本简介')
    expect(wrapper.get('.release-summary').text()).toContain('更快的项目打开速度')
    expect(wrapper.get('.release-notes-copy').text()).toContain('新增：macOS 安装包')

    const cards = wrapper.findAll('.platform-card')
    const windowsCard = cards.find((card) => card.get('h2').text() === 'Windows')!
    const macOSCard = cards.find((card) => card.get('h2').text() === 'macOS')!
    const linuxCard = cards.find((card) => card.get('h2').text() === 'Linux')!
    expect(windowsCard.text()).toContain('已发布')
    expect(windowsCard.find('a.button').exists()).toBe(true)
    expect(macOSCard.text()).toContain('已发布')
    expect(macOSCard.find('a.button').exists()).toBe(true)
    expect(linuxCard.text()).toContain('已发布')
    expect(linuxCard.find('a.button').exists()).toBe(true)
  })

  it('groups Host and Relay downloads and builds configured scripts for different platforms', async () => {
    Object.defineProperty(navigator, 'userAgent', {
      configurable: true,
      value: 'Mozilla/5.0 (X11; Linux x86_64)',
    })
    const release = {
      id: '69b68e75-af45-4c44-a6e2-8a6a6db5f4aa',
      project: 'web' as const,
      version: '2.0.0',
      channel: 'stable' as const,
      title: 'Server release',
      summary: 'Host',
      releaseNotes: '',
      publishedAt: '2026-08-21T08:00:00Z',
      assets: [
        {
          id: '0e4e53f8-a510-4d27-9359-9a70bdd5faf3',
          platform: 'linux' as const,
          architecture: 'x64' as const,
          fileName: 'wenzwork-host-deployment-2.0.0-linux-amd64.tar.gz',
          fileSizeBytes: 10_000,
          sha256: 'a'.repeat(64),
          signatureStatus: 'valid' as const,
          downloadUrl: '/api/v1/release-assets/host/download',
        },
        {
          id: '1e4e53f8-a510-4d27-9359-9a70bdd5faf3',
          platform: 'windows' as const,
          architecture: 'x64' as const,
          fileName: 'wenzwork-host-deployment-2.0.0-windows-amd64.tar.gz',
          fileSizeBytes: 10_000,
          sha256: 'b'.repeat(64),
          signatureStatus: 'valid' as const,
          downloadUrl: '/api/v1/release-assets/host-windows/download',
        },
        {
          id: '2e4e53f8-a510-4d27-9359-9a70bdd5faf3',
          platform: 'linux' as const,
          architecture: 'x64' as const,
          fileName: 'wenzwork-relay-deployment-2.0.0-linux-amd64.tar.gz',
          fileSizeBytes: 9_000,
          sha256: 'c'.repeat(64),
          signatureStatus: 'valid' as const,
          downloadUrl: '/api/v1/release-assets/relay/download',
        },
        {
          id: '3e4e53f8-a510-4d27-9359-9a70bdd5faf3',
          platform: 'windows' as const,
          architecture: 'x64' as const,
          fileName: 'wenzwork-relay-deployment-2.0.0-windows-amd64.tar.gz',
          fileSizeBytes: 9_000,
          sha256: 'd'.repeat(64),
          signatureStatus: 'valid' as const,
          downloadUrl: '/api/v1/release-assets/relay-windows/download',
        },
      ],
    }
    latestMock.mockImplementation(async (project) => {
      if (project === 'web') return release
      throw new Error('not published')
    })
    listMock.mockImplementation(async (project) => (project === 'web' ? [release] : []))
    const generatedBlobs: Blob[] = []
    const downloadedNames: string[] = []
    Object.defineProperty(URL, 'createObjectURL', {
      configurable: true,
      value: vi.fn((blob: Blob) => {
        generatedBlobs.push(blob)
        return 'blob:wenzwork-deployment'
      }),
    })
    Object.defineProperty(URL, 'revokeObjectURL', { configurable: true, value: vi.fn() })
    vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(function (
      this: HTMLAnchorElement,
    ) {
      downloadedNames.push(this.download)
    })
    const router = createRouter({
      history: createMemoryHistory(),
      routes: [{ path: '/:pathMatch(.*)*', component: { template: '<div />' } }],
    })
    const wrapper = mount(DownloadPage, {
      global: { plugins: [router, createHead()] },
    })
    await flushPromises()
    await wrapper
      .findAll('.download-project-option')
      .find((button) => button.text().includes('Web / 服务端'))!
      .trigger('click')

    expect(wrapper.findAll('.server-program-details')).toHaveLength(2)
    expect(wrapper.text()).toContain('Host 服务端')
    expect(wrapper.text()).toContain('Relay 中继服务')
    expect(wrapper.find('#deploy-platform').exists()).toBe(true)

    await wrapper.get('#host-port').setValue('9080')
    await wrapper.get('#host-public-url').setValue('http://localhost:9080')
    await wrapper.get('#host-admin-email').setValue('owner@example.test')
    await wrapper.get('#host-admin-password').setValue('Configured-password-123')
    expect(wrapper.get<HTMLButtonElement>('.installer-download-button').element.disabled).toBe(
      false,
    )
    await wrapper.get('.installer-download-button').trigger('click')
    await flushPromises()

    expect(generatedBlobs).toHaveLength(1)
    const hostScript = await generatedBlobs[0]!.text()
    expect(hostScript).toContain('(cd "$INSTALL_ROOT" && ./start.sh)')
    expect(hostScript).not.toContain('init.sh')
    expect(hostScript).toContain('SYSTEM_SETUP_COMPLETED=false')
    expect(hostScript).toContain('HTTP_ADDR=":9080"')
    expect(hostScript).toContain('PUBLIC_BASE_URL="http://localhost:9080"')
    expect(hostScript).toContain('SYSTEM_ADMIN_EMAIL="owner@example.test"')
    expect(hostScript).toContain('wenzwork-host-deployment-2.0.0-linux-amd64.tar.gz')
    expect(hostScript).not.toContain('wenzwork-relay-deployment')
    expect(downloadedNames[0]).toBe('wenzwork-host-install-v2.0.0-linux-amd64.sh')

    const bash = process.platform === 'win32' ? 'C:\\Program Files\\Git\\bin\\bash.exe' : 'bash'
    if (process.platform !== 'win32' || existsSync(bash)) {
      const directory = mkdtempSync(join(tmpdir(), 'wenzwork-one-click-'))
      const scriptPath = join(directory, 'deploy.sh')
      try {
        writeFileSync(scriptPath, hostScript, 'utf8')
        execFileSync(bash, ['-n', scriptPath])
      } finally {
        rmSync(directory, { recursive: true, force: true })
      }
    }

    await wrapper.get('#deploy-component').setValue('relay')
    await wrapper.get('#deploy-platform').setValue('windows')
    await wrapper.get('#relay-management-url').setValue('http://host.example.test:8080')
    await wrapper.get('#relay-access-key').setValue('relay_' + 'R'.repeat(43))
    expect(wrapper.get<HTMLButtonElement>('.installer-download-button').element.disabled).toBe(
      false,
    )
    await wrapper.get('.installer-download-button').trigger('click')
    await flushPromises()

    expect(generatedBlobs).toHaveLength(2)
    const relayScript = await generatedBlobs[1]!.text()
    expect(relayScript).toContain('#Requires -Version 5.1')
    expect(relayScript).toContain('RELAY_ACCESS_KEY=relay_')
    expect(relayScript).toContain('RELAY_MANAGEMENT_URL="http://host.example.test:8080"')
    expect(relayScript).toContain('wenzwork-relay-deployment-2.0.0-windows-amd64.tar.gz')
    expect(relayScript).not.toContain('SYSTEM_ADMIN_PASSWORD')
    expect(downloadedNames[1]).toBe('wenzwork-relay-install-v2.0.0-windows-amd64.ps1')
  })
})
