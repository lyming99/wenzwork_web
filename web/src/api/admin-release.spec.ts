import { beforeEach, describe, expect, it, vi } from 'vitest'

import {
  getLatestGitHubRelease,
  getReleaseAccessKeySettings,
  getReleaseDeliverySettings,
  getReleaseSourceSettings,
  importLatestMirrorRelease,
  importReleaseAsset,
  updateReleaseDeliverySettings,
  updateReleaseAccessKeySettings,
  updateReleaseSourceSettings,
  uploadReleaseAsset,
} from './admin'
import { apiClient } from './client'

vi.mock('./client', () => ({
  apiClient: { get: vi.fn(), post: vi.fn(), put: vi.fn() },
}))

describe('admin release storage API', () => {
  beforeEach(() => vi.clearAllMocks())

  it('streams local files through the same-origin API', async () => {
    const file = new File(['data'], 'WenzWork.exe')
    const request = {
      version: '1.2.3',
      platform: 'windows' as const,
      architecture: 'x64' as const,
      fileName: 'WenzWork.exe',
      fileSizeBytes: 4,
      sha256: 'a'.repeat(64),
    }
    const stored = {
      objectKey: 'releases/1.2.3/windows/x64/id/WenzWork.exe',
      downloadUrl: 'https://downloads.example.test/releases/1.2.3/windows/x64/id/WenzWork.exe',
      fileSizeBytes: 4,
      sha256: 'a'.repeat(64),
    }
    vi.mocked(apiClient.post).mockResolvedValueOnce({ data: stored })

    await expect(uploadReleaseAsset(request, file, vi.fn())).resolves.toEqual(stored)
    expect(apiClient.post).toHaveBeenCalledWith(
      '/admin/release-assets/upload',
      file,
      expect.objectContaining({ params: request, timeout: 0 }),
    )
  })

  it('imports external files and exposes GitHub and delivery settings', async () => {
    const imported = { objectKey: 'releases/id/file.exe' }
    vi.mocked(apiClient.post).mockResolvedValueOnce({ data: imported })
    await importReleaseAsset({
      version: '1.2.3',
      platform: 'windows',
      architecture: 'x64',
      downloadUrl: 'https://github.com/acme/wenzwork/file.exe',
    })
    expect(apiClient.post).toHaveBeenCalledWith(
      '/admin/release-assets/import',
      expect.objectContaining({ version: '1.2.3' }),
      { timeout: 0 },
    )

    vi.mocked(apiClient.post).mockResolvedValueOnce({ data: { version: '1.2.3', assets: [] } })
    await importLatestMirrorRelease('desktop')
    expect(apiClient.post).toHaveBeenCalledWith('/admin/mirror-releases/latest/import', null, {
      params: { project: 'desktop' },
      timeout: 0,
    })

    vi.mocked(apiClient.get)
      .mockResolvedValueOnce({ data: { tagName: 'v1.2.3' } })
      .mockResolvedValueOnce({
        data: {
          items: [
            {
              project: 'desktop',
              githubRepository: 'acme/wenzwork',
              githubTokenConfigured: true,
              mirrorBaseUrl: 'https://mirror.example.test',
              version: 1,
            },
          ],
        },
      })
      .mockResolvedValueOnce({ data: { downloadMode: 'proxy_cached', version: 1 } })
      .mockResolvedValueOnce({
        data: {
          accessKeyConfigured: true,
          keyPrefix: 'release_abcdefgh',
          version: 1,
        },
      })
    await getLatestGitHubRelease('desktop')
    await getReleaseSourceSettings()
    await getReleaseDeliverySettings()
    await getReleaseAccessKeySettings()
    expect(apiClient.get).toHaveBeenNthCalledWith(1, '/admin/github-releases/latest', {
      params: { project: 'desktop' },
    })
    expect(apiClient.get).toHaveBeenNthCalledWith(2, '/admin/release-source-settings')
    expect(apiClient.get).toHaveBeenNthCalledWith(3, '/admin/release-delivery-settings')
    expect(apiClient.get).toHaveBeenNthCalledWith(4, '/admin/release-access-key-settings')

    vi.mocked(apiClient.put).mockResolvedValueOnce({
      data: {
        githubRepository: 'acme/wenzwork-next',
        githubTokenConfigured: true,
        mirrorBaseUrl: 'https://mirror-next.example.test',
        version: 2,
      },
    })
    await updateReleaseSourceSettings({
      project: 'desktop',
      githubRepository: 'acme/wenzwork-next',
      githubToken: 'github_pat_replacement',
      clearGithubToken: false,
      mirrorBaseUrl: 'https://mirror-next.example.test',
      expectedVersion: 1,
    })
    expect(apiClient.put).toHaveBeenCalledWith('/admin/release-source-settings', {
      project: 'desktop',
      githubRepository: 'acme/wenzwork-next',
      githubToken: 'github_pat_replacement',
      clearGithubToken: false,
      mirrorBaseUrl: 'https://mirror-next.example.test',
      expectedVersion: 1,
    })

    vi.mocked(apiClient.put).mockResolvedValueOnce({ data: { downloadMode: 's3_redirect' } })
    await updateReleaseDeliverySettings({
      downloadMode: 's3_redirect',
      s3UrlPrefix: 'https://cdn.example.test/files',
      expectedVersion: 1,
    })
    expect(apiClient.put).toHaveBeenCalledWith('/admin/release-delivery-settings', {
      downloadMode: 's3_redirect',
      s3UrlPrefix: 'https://cdn.example.test/files',
      expectedVersion: 1,
    })

    const accessKey = `release_${'k'.repeat(43)}`
    vi.mocked(apiClient.put).mockResolvedValueOnce({
      data: { accessKeyConfigured: true, keyPrefix: accessKey.slice(0, 16), version: 2 },
    })
    await updateReleaseAccessKeySettings({ accessKey, expectedVersion: 1 })
    expect(apiClient.put).toHaveBeenCalledWith('/admin/release-access-key-settings', {
      accessKey,
      expectedVersion: 1,
    })
  })
})
