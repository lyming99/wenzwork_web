import { flushPromises, mount } from '@vue/test-utils'
import { createHead } from '@unhead/vue/client'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import {
  createAdminRelease,
  deleteAdminRelease,
  getLatestGitHubRelease,
  getReleaseAccessKeySettings,
  getReleaseDeliverySettings,
  getReleaseSourceSettings,
  importLatestMirrorRelease,
  importReleaseAsset,
  listAdminReleases,
  updateReleaseDeliverySettings,
  updateReleaseAccessKeySettings,
  updateReleaseSourceSettings,
  updateAdminRelease,
} from '@/api/admin'

import AdminReleasesPage from './AdminReleasesPage.vue'

vi.mock('@/api/admin', () => ({
  createAdminRelease: vi.fn(),
  deleteAdminRelease: vi.fn(),
  getLatestGitHubRelease: vi.fn(),
  getReleaseAccessKeySettings: vi.fn(),
  getReleaseDeliverySettings: vi.fn(),
  getReleaseSourceSettings: vi.fn(),
  importLatestMirrorRelease: vi.fn(),
  importReleaseAsset: vi.fn(),
  listAdminReleases: vi.fn(),
  publishAdminRelease: vi.fn(),
  updateAdminRelease: vi.fn(),
  updateReleaseAccessKeySettings: vi.fn(),
  updateReleaseDeliverySettings: vi.fn(),
  updateReleaseSourceSettings: vi.fn(),
  uploadReleaseAsset: vi.fn(),
  withdrawAdminRelease: vi.fn(),
}))

const storedAsset = {
  objectKey: 'releases/1.2.3/windows/x64/id/WenzWork.exe',
  downloadUrl: 'https://objects.example.test/releases/1.2.3/windows/x64/id/WenzWork.exe',
  fileName: 'WenzWork.exe',
  fileSizeBytes: 4096,
  sha256: 'a'.repeat(64),
  contentType: 'application/octet-stream',
  platform: 'windows' as const,
  architecture: 'x64' as const,
}

describe('AdminReleasesPage', () => {
  afterEach(() => {
    vi.unstubAllGlobals()
  })

  beforeEach(() => {
    vi.clearAllMocks()
    vi.mocked(listAdminReleases).mockResolvedValue([])
    vi.mocked(getReleaseDeliverySettings).mockResolvedValue({
      downloadMode: 'proxy_cached',
      s3UrlPrefix: '',
      version: 1,
      updatedAt: new Date().toISOString(),
    })
    vi.mocked(getReleaseAccessKeySettings).mockResolvedValue({
      accessKeyConfigured: true,
      keyPrefix: 'release_abcdefgh',
      version: 1,
      updatedAt: new Date().toISOString(),
    })
    vi.mocked(getReleaseSourceSettings).mockResolvedValue([
      {
        project: 'web',
        githubRepository: 'acme/wenzwork-web',
        githubTokenConfigured: false,
        mirrorBaseUrl: '',
        version: 1,
        updatedAt: new Date().toISOString(),
      },
      {
        project: 'desktop',
        githubRepository: 'acme/wenzwork',
        githubTokenConfigured: true,
        mirrorBaseUrl: 'https://mirror.example.test',
        version: 1,
        updatedAt: new Date().toISOString(),
      },
      {
        project: 'mobile',
        githubRepository: 'acme/wenzwork-mobile',
        githubTokenConfigured: false,
        mirrorBaseUrl: '',
        version: 1,
        updatedAt: new Date().toISOString(),
      },
    ])
    vi.mocked(createAdminRelease).mockResolvedValue({} as never)
    vi.mocked(importReleaseAsset).mockResolvedValue(storedAsset)
  })

  it('keeps save actionable and explains incomplete fields', async () => {
    const wrapper = mount(AdminReleasesPage, { global: { plugins: [createHead()] } })
    await flushPromises()

    const saveButton = wrapper.get('form.release-editor button[type="submit"]')
    expect(saveButton.attributes('disabled')).toBeUndefined()
    expect(wrapper.text()).toContain('请先填写版本号。')
  })

  it('separates settings, publishing, and releases into accessible tabs', async () => {
    const wrapper = mount(AdminReleasesPage, { global: { plugins: [createHead()] } })
    await flushPromises()

    const tabs = wrapper.findAll('[role="tab"]')
    expect(tabs.map((tab) => tab.text())).toEqual(['基础配置', '版本发布', '版本列表'])
    expect(
      wrapper.get('#release-management-panel-settings').attributes('style') ?? '',
    ).not.toContain('display: none')
    expect(wrapper.get('#release-management-panel-publish').attributes('style')).toContain(
      'display: none',
    )
    expect(wrapper.get('#release-management-panel-list').attributes('style')).toContain(
      'display: none',
    )

    await tabs[1]!.trigger('click')
    expect(tabs[1]!.attributes('aria-selected')).toBe('true')
    expect(
      wrapper.get('#release-management-panel-publish').attributes('style') ?? '',
    ).not.toContain('display: none')
    expect(wrapper.get('#release-management-panel-settings').attributes('style')).toContain(
      'display: none',
    )
  })

  it('detects an external link and stores it in S3 without manual file parameters', async () => {
    const wrapper = mount(AdminReleasesPage, { global: { plugins: [createHead()] } })
    await flushPromises()
    await wrapper.get('#release-version').setValue('1.2.3')
    await wrapper.get('#release-title').setValue('Release')
    await wrapper
      .findAll('button')
      .find((button) => button.text() === '添加文件')!
      .trigger('click')
    await wrapper
      .findAll('button')
      .find((button) => button.text().includes('外链检测并转存'))!
      .trigger('click')

    expect(wrapper.text()).toContain('自动下载检测文件名、大小和 SHA-256')
    expect(wrapper.find('#asset-name-0').exists()).toBe(false)
    expect(wrapper.find('#asset-size-0').exists()).toBe(false)
    expect(wrapper.find('#asset-sha-0').exists()).toBe(false)
    await wrapper
      .get('#asset-url-0')
      .setValue('https://github.com/acme/wenzwork/releases/download/v1.2.3/WenzWork.exe')
    await wrapper
      .findAll('button')
      .find((button) => button.text() === '检测并转存到 S3')!
      .trigger('click')
    await flushPromises()

    expect(importReleaseAsset).toHaveBeenCalledWith(
      expect.objectContaining({
        version: '1.2.3',
        downloadUrl: 'https://github.com/acme/wenzwork/releases/download/v1.2.3/WenzWork.exe',
      }),
    )
    expect(wrapper.text()).toContain('S3 已就绪')
    await wrapper.get('form.release-editor').trigger('submit')
    await flushPromises()
    expect(createAdminRelease).toHaveBeenCalledWith(
      expect.objectContaining({
        assets: [
          expect.objectContaining({
            source: 's3',
            objectKey: storedAsset.objectKey,
            sha256: storedAsset.sha256,
          }),
        ],
      }),
    )
  })

  it('queries the latest GitHub Release and keeps authenticated asset references', async () => {
    vi.mocked(getLatestGitHubRelease).mockResolvedValue({
      repository: 'acme/wenzwork',
      tagName: 'v1.2.3',
      version: '1.2.3',
      name: 'WenzWork 1.2.3',
      summary: 'Fast release',
      body: 'Release notes',
      htmlUrl: 'https://github.com/acme/wenzwork/releases/tag/v1.2.3',
      prerelease: false,
      publishedAt: new Date().toISOString(),
      assets: [
        {
          fileName: 'WenzWork-windows-x64.exe',
          fileSizeBytes: 4096,
          sha256: 'a'.repeat(64),
          contentType: 'application/octet-stream',
          downloadUrl:
            'https://github.com/acme/wenzwork/releases/download/v1.2.3/WenzWork-windows-x64.exe',
          source: 'github',
          objectKey: 'github/acme/wenzwork/assets/42/WenzWork-windows-x64.exe',
          platform: 'windows',
          architecture: 'x64',
        },
      ],
    })
    const wrapper = mount(AdminReleasesPage, { global: { plugins: [createHead()] } })
    await flushPromises()
    await wrapper
      .findAll('[role="tab"]')
      .find((tab) => tab.text() === '版本发布')!
      .trigger('click')
    await wrapper
      .findAll('button')
      .find((button) => button.text().includes('最新 Release'))!
      .trigger('click')
    await flushPromises()

    expect(getLatestGitHubRelease).toHaveBeenCalledWith('desktop')
    expect(importReleaseAsset).not.toHaveBeenCalled()
    expect((wrapper.get('#release-version').element as HTMLInputElement).value).toBe('1.2.3')
    expect(wrapper.text()).toContain('已读取 v1.2.3 并关联 1 个 GitHub 安装包')
    expect(wrapper.get('#release-assets-content').attributes('style')).toContain('display: none')
    expect(wrapper.get('.release-assets-collapsed-summary').text()).toContain('已折叠 1 个安装文件')
    expect(wrapper.get('.release-editor .batch-form-heading button[type="submit"]').text()).toBe(
      '发布版本',
    )
    await wrapper.get('form.release-editor').trigger('submit')
    await flushPromises()
    expect(createAdminRelease).toHaveBeenCalledWith(
      expect.objectContaining({
        status: 'published',
        assets: [
          expect.objectContaining({
            source: 'github',
            objectKey: 'github/acme/wenzwork/assets/42/WenzWork-windows-x64.exe',
          }),
        ],
      }),
    )
  })

  it('rotates the database-backed Release Access Key without receiving plaintext', async () => {
    const accessKey = `release_${'k'.repeat(43)}`
    vi.mocked(updateReleaseAccessKeySettings).mockResolvedValue({
      accessKeyConfigured: true,
      keyPrefix: accessKey.slice(0, 16),
      version: 2,
      updatedAt: new Date().toISOString(),
    })
    vi.stubGlobal(
      'confirm',
      vi.fn(() => true),
    )
    const wrapper = mount(AdminReleasesPage, { global: { plugins: [createHead()] } })
    await flushPromises()

    expect(wrapper.text()).toContain('当前密钥前缀：')
    expect(wrapper.text()).not.toContain(accessKey)
    await wrapper.get('#release-access-key').setValue(accessKey)
    await wrapper.get('#release-access-key-confirmation').setValue(accessKey)
    await wrapper.get('form.release-access-key-card').trigger('submit')
    await flushPromises()

    expect(updateReleaseAccessKeySettings).toHaveBeenCalledWith({
      accessKey,
      expectedVersion: 1,
    })
    expect((wrapper.get('#release-access-key').element as HTMLInputElement).value).toBe('')
    expect(wrapper.text()).toContain('旧密钥已失效')
  })

  it('blocks a Web deployment asset from an older Release version', async () => {
    vi.mocked(getLatestGitHubRelease).mockResolvedValue({
      repository: 'acme/wenzwork-web',
      tagName: 'v0.2.9',
      version: '0.2.9',
      name: 'WenzWork v0.2.9',
      summary: '',
      body: '',
      htmlUrl: 'https://github.com/acme/wenzwork-web/releases/tag/v0.2.9',
      prerelease: false,
      publishedAt: new Date().toISOString(),
      assets: [
        {
          fileName: 'wenzwork-relay-deployment-v0.2.8-linux-amd64.tar.gz',
          fileSizeBytes: 4096,
          sha256: 'a'.repeat(64),
          contentType: 'application/gzip',
          downloadUrl:
            'https://github.com/acme/wenzwork-web/releases/download/v0.2.9/wenzwork-relay-deployment-v0.2.8-linux-amd64.tar.gz',
          source: 'github',
          objectKey:
            'github/acme/wenzwork-web/assets/42/wenzwork-relay-deployment-v0.2.8-linux-amd64.tar.gz',
          platform: 'linux',
          architecture: 'x64',
        },
      ],
    })
    const wrapper = mount(AdminReleasesPage, { global: { plugins: [createHead()] } })
    await flushPromises()
    await wrapper.get('#release-project').setValue('web')
    await wrapper
      .findAll('button')
      .find((button) => button.text().includes('最新 Release'))!
      .trigger('click')
    await flushPromises()

    expect(wrapper.text()).toContain('部署包版本、平台或架构与当前 Web 发布记录不一致')
    await wrapper.get('form.release-editor').trigger('submit')
    await flushPromises()
    expect(createAdminRelease).not.toHaveBeenCalled()
  })

  it('queries another WenzWork server and saves links for direct host caching', async () => {
    vi.mocked(importLatestMirrorRelease).mockResolvedValue({
      mirrorBaseUrl: 'https://mirror.example.test',
      project: 'desktop',
      version: '1.3.0',
      channel: 'stable',
      title: 'WenzWork 1.3.0',
      summary: 'Mirror release',
      releaseNotes: 'Release notes from mirror',
      publishedAt: new Date().toISOString(),
      assets: [
        {
          fileName: 'WenzWork-windows-x64.exe',
          fileSizeBytes: 8192,
          sha256: 'c'.repeat(64),
          contentType: 'application/octet-stream',
          downloadUrl:
            'https://mirror.example.test/api/v1/release-assets/00000000-0000-0000-0000-000000000001/download',
          source: 'mirror',
          objectKey: `mirror/${'d'.repeat(64)}/WenzWork-windows-x64.exe`,
          platform: 'windows',
          architecture: 'x64',
          signatureStatus: 'valid',
        },
      ],
    })
    const wrapper = mount(AdminReleasesPage, { global: { plugins: [createHead()] } })
    await flushPromises()
    await wrapper
      .findAll('button')
      .find((button) => button.text().includes('从镜像拉取'))!
      .trigger('click')
    await flushPromises()

    expect(importLatestMirrorRelease).toHaveBeenCalledWith('desktop')
    expect(importReleaseAsset).not.toHaveBeenCalled()
    expect((wrapper.get('#release-version').element as HTMLInputElement).value).toBe('1.3.0')
    expect(wrapper.text()).toContain('首次下载会按大小和 SHA-256 校验后直接写入本机缓存')
    expect(wrapper.text()).toContain('镜像站链接')
    const mirrorAssetEditor = wrapper.get('.release-asset-editor')
    expect(mirrorAssetEditor.text()).toContain('镜像引用已关联')
    expect(mirrorAssetEditor.text()).toContain('镜像站下载链接')
    expect(mirrorAssetEditor.text()).not.toContain('S3 已就绪')
    await wrapper.get('form.release-editor').trigger('submit')
    await flushPromises()
    expect(createAdminRelease).toHaveBeenCalledWith(
      expect.objectContaining({
        assets: [
          expect.objectContaining({
            source: 'mirror',
            objectKey: `mirror/${'d'.repeat(64)}/WenzWork-windows-x64.exe`,
            signatureStatus: 'valid',
          }),
        ],
      }),
    )
  })

  it('saves the GitHub repository and encrypted token in release settings', async () => {
    vi.mocked(updateReleaseSourceSettings).mockResolvedValue({
      project: 'desktop',
      githubRepository: 'acme/wenzwork-next',
      githubTokenConfigured: true,
      mirrorBaseUrl: 'https://mirror-next.example.test',
      version: 2,
      updatedAt: new Date().toISOString(),
    })
    const wrapper = mount(AdminReleasesPage, { global: { plugins: [createHead()] } })
    await flushPromises()

    expect(
      (wrapper.get('#release-github-repository-desktop').element as HTMLInputElement).value,
    ).toBe('acme/wenzwork')
    await wrapper.get('#release-github-repository-desktop').setValue('acme/wenzwork-next')
    await wrapper.get('#release-github-token-desktop').setValue('github_pat_replacement')
    await wrapper.get('#release-mirror-url-desktop').setValue('https://mirror-next.example.test/')
    const importButton = wrapper
      .findAll('button')
      .find((button) => button.text().includes('最新 Release'))!
    expect(importButton.attributes('disabled')).toBeDefined()
    await wrapper.get('.release-source-project:nth-child(2)').trigger('submit')
    await flushPromises()

    expect(updateReleaseSourceSettings).toHaveBeenCalledWith({
      project: 'desktop',
      githubRepository: 'acme/wenzwork-next',
      githubToken: 'github_pat_replacement',
      clearGithubToken: false,
      mirrorBaseUrl: 'https://mirror-next.example.test',
      expectedVersion: 1,
    })
    expect((wrapper.get('#release-github-token-desktop').element as HTMLInputElement).value).toBe(
      '',
    )
    expect(wrapper.text()).toContain('Token 已加密保存')
    expect(importButton.attributes('disabled')).toBeUndefined()
  })

  it('clears a saved GitHub token without returning its plaintext', async () => {
    vi.mocked(updateReleaseSourceSettings).mockResolvedValue({
      project: 'desktop',
      githubRepository: 'acme/wenzwork',
      githubTokenConfigured: false,
      mirrorBaseUrl: 'https://mirror.example.test',
      version: 2,
      updatedAt: new Date().toISOString(),
    })
    const wrapper = mount(AdminReleasesPage, { global: { plugins: [createHead()] } })
    await flushPromises()

    expect(wrapper.text()).toContain('Token 已配置')
    expect((wrapper.get('#release-github-token-desktop').element as HTMLInputElement).value).toBe(
      '',
    )
    await wrapper
      .get('.release-source-project:nth-child(2) .release-token-clear input[type="checkbox"]')
      .setValue(true)
    await wrapper.get('.release-source-project:nth-child(2)').trigger('submit')
    await flushPromises()

    expect(updateReleaseSourceSettings).toHaveBeenCalledWith({
      project: 'desktop',
      githubRepository: 'acme/wenzwork',
      clearGithubToken: true,
      mirrorBaseUrl: 'https://mirror.example.test',
      expectedVersion: 1,
    })
    expect(wrapper.text()).toContain('Token 未配置')
  })

  it('saves S3 redirect settings from the admin page', async () => {
    vi.mocked(updateReleaseDeliverySettings).mockResolvedValue({
      downloadMode: 's3_redirect',
      s3UrlPrefix: 'https://cdn.example.test/files',
      version: 2,
      updatedAt: new Date().toISOString(),
    })
    const wrapper = mount(AdminReleasesPage, { global: { plugins: [createHead()] } })
    await flushPromises()
    await wrapper
      .findAll('button')
      .find((button) => button.text().includes('S3 链接'))!
      .trigger('click')
    await wrapper.get('#release-s3-url-prefix').setValue('https://cdn.example.test/files')
    await wrapper.get('form.release-delivery-card').trigger('submit')
    await flushPromises()

    expect(updateReleaseDeliverySettings).toHaveBeenCalledWith({
      downloadMode: 's3_redirect',
      s3UrlPrefix: 'https://cdn.example.test/files',
      expectedVersion: 1,
    })
  })

  it('saves GitHub link delivery mode without an S3 prefix', async () => {
    vi.mocked(updateReleaseDeliverySettings).mockResolvedValue({
      downloadMode: 'github_redirect',
      s3UrlPrefix: '',
      version: 2,
      updatedAt: new Date().toISOString(),
    })
    const wrapper = mount(AdminReleasesPage, { global: { plugins: [createHead()] } })
    await flushPromises()
    await wrapper
      .findAll('button')
      .find((button) => button.text().includes('GitHub 链接'))!
      .trigger('click')
    await wrapper.get('form.release-delivery-card').trigger('submit')
    await flushPromises()

    expect(updateReleaseDeliverySettings).toHaveBeenCalledWith({
      downloadMode: 'github_redirect',
      s3UrlPrefix: '',
      expectedVersion: 1,
    })
    expect(wrapper.text()).toContain('不向浏览器暴露 Token')
  })

  it('keeps Release Access Key pushed assets local when editing in the admin page', async () => {
    const digest = 'd'.repeat(64)
    vi.mocked(listAdminReleases).mockResolvedValue([
      {
        id: '80a949b3-f057-42cc-8aa2-621d6ca2e33a',
        project: 'desktop',
        version: '1.2.3',
        channel: 'stable',
        title: 'WenzWork 桌面端 1.2.3更新啦~',
        summary: 'WenzWork 桌面端 1.2.3更新啦~',
        releaseNotes: 'WenzWork 桌面端 1.2.3更新啦~',
        status: 'published',
        publishedAt: new Date().toISOString(),
        createdAt: new Date().toISOString(),
        updatedAt: new Date().toISOString(),
        assets: [
          {
            id: 'd42fedb7-4793-4ce4-b2f2-09a325394039',
            platform: 'windows',
            architecture: 'x64',
            fileName: 'wenzwork-desktop-1.2.3-windows-x64.zip',
            fileSizeBytes: 4096,
            sha256: digest,
            signatureStatus: 'unknown',
            source: 'local',
            objectKey: `local/desktop/1.2.3/windows/x64/${digest}/wenzwork-desktop-1.2.3-windows-x64.zip`,
            downloadUrl: '',
            status: 'published',
          },
        ],
      },
    ])
    vi.mocked(updateAdminRelease).mockResolvedValue({} as never)
    const wrapper = mount(AdminReleasesPage, { global: { plugins: [createHead()] } })
    await flushPromises()
    await wrapper
      .findAll('button')
      .find((button) => button.text() === '编辑')!
      .trigger('click')

    expect(wrapper.text()).toContain('本地构建推送')
    expect(wrapper.text()).toContain('本地推送已就绪')
    expect(wrapper.text()).not.toContain('该版本当前已发布')
    await wrapper.get('form.release-editor').trigger('submit')
    await flushPromises()
    expect(updateAdminRelease).toHaveBeenCalledWith(
      '80a949b3-f057-42cc-8aa2-621d6ca2e33a',
      expect.objectContaining({
        assets: [expect.objectContaining({ source: 'local', downloadUrl: '' })],
      }),
    )
  })

  it('permanently deletes a published release after explicit confirmation', async () => {
    vi.mocked(listAdminReleases)
      .mockResolvedValueOnce([
        {
          id: '59e2dc77-23c3-46dd-9422-f258113826b4',
          project: 'desktop',
          version: '1.2.3',
          channel: 'stable',
          title: 'Published release',
          summary: '',
          releaseNotes: '',
          status: 'published',
          publishedAt: new Date().toISOString(),
          createdAt: new Date().toISOString(),
          updatedAt: new Date().toISOString(),
          assets: [],
        },
      ])
      .mockResolvedValueOnce([])
    vi.mocked(deleteAdminRelease).mockResolvedValue(undefined)
    const confirm = vi.fn(() => true)
    vi.stubGlobal('confirm', confirm)
    const wrapper = mount(AdminReleasesPage, { global: { plugins: [createHead()] } })
    await flushPromises()

    const deleteButton = wrapper.findAll('button').find((button) => button.text() === '删除')!
    await deleteButton.trigger('click')
    await flushPromises()

    expect(confirm).toHaveBeenCalledWith(expect.stringContaining('该版本当前已发布'))
    expect(confirm).toHaveBeenCalledWith(expect.stringContaining('此操作不可恢复'))
    expect(deleteAdminRelease).toHaveBeenCalledWith('59e2dc77-23c3-46dd-9422-f258113826b4')
    expect(listAdminReleases).toHaveBeenCalledTimes(2)
    expect(wrapper.text()).toContain('版本 1.2.3 已永久删除。')
  })
})
