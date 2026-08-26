import { execFileSync, spawnSync } from 'node:child_process'
import { existsSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

import { describe, expect, it } from 'vitest'

import type { ReleaseAsset } from '@/api/catalog'

import {
  createDeviceEnvironmentFile,
  createDeviceInstaller,
  createHostInstaller,
  createRelayBootstrapInstaller,
  createRelayInstaller,
  findPortableAsset,
  installerFileName,
  portableAssetMatchesRelease,
} from './portableInstaller'

const asset = (
  component: 'host' | 'relay' | 'device-agent',
  platform: 'linux' | 'windows',
): ReleaseAsset => ({
  id: '0e4e53f8-a510-4d27-9359-9a70bdd5faf3',
  platform,
  architecture: 'x64',
  fileName: `wenzwork-${component}-deployment-2.0.0-${platform}-amd64.tar.gz`,
  fileSizeBytes: 10_000,
  sha256: 'a'.repeat(64),
  signatureStatus: 'valid',
  downloadUrl: '/download',
})

const credentials = {
  administratorEmail: 'admin@wenzwork.local',
  administratorPassword: 'Admin_password_234567890123456',
}

describe('portable installers', () => {
  it('rejects release metadata whose deployment file target does not match', () => {
    const mismatched = asset('host', 'linux')
    mismatched.fileName = 'wenzwork-host-deployment-2.0.0-windows-amd64.tar.gz'
    expect(
      findPortableAsset(
        {
          id: 'release',
          project: 'web',
          version: '2.0.0',
          channel: 'stable',
          title: 'Host',
          summary: '',
          releaseNotes: '',
          publishedAt: '2026-08-21T00:00:00Z',
          assets: [mismatched],
        },
        'host',
        { platform: 'linux', architecture: 'x64' },
      ),
    ).toBeUndefined()

    mismatched.fileName = 'wenzwork-host-deployment-v1.9.9-linux-amd64.tar.gz'
    expect(
      findPortableAsset(
        {
          id: 'release',
          project: 'web',
          version: '2.0.0',
          channel: 'stable',
          title: 'Host',
          summary: '',
          releaseNotes: '',
          publishedAt: '2026-08-21T00:00:00Z',
          assets: [mismatched],
        },
        'host',
        { platform: 'linux', architecture: 'x64' },
      ),
    ).toBeUndefined()
    expect(portableAssetMatchesRelease(mismatched, '2.0.0')).toBe(false)
  })

  it('generates syntax-valid Bash Host, Relay and Device installers', () => {
    const host = createHostInstaller(
      asset('host', 'linux'),
      { platform: 'linux', architecture: 'x64' },
      'https://control.example.test/host.tar.gz',
      {
        ...credentials,
        httpAddress: ':9080',
        publicBaseURL: 'http://localhost:9080',
      },
    )
    const relay = createRelayInstaller(
      asset('relay', 'linux'),
      { platform: 'linux', architecture: 'x64' },
      'http://downloads.example.test/relay.tar.gz',
      {
        managementURL: 'http://control.example.test:8080',
        accessKey: 'relay_' + 'R'.repeat(43),
      },
    )
    const device = createDeviceInstaller(
      asset('device-agent', 'linux'),
      { platform: 'linux', architecture: 'x64' },
      'https://control.example.test/device.tar.gz',
      'https://control.example.test',
      'device_' + 'A'.repeat(43),
    )
    const deviceEnvironment = createDeviceEnvironmentFile(
      'https://control.example.test',
      'device_' + 'A'.repeat(43),
    )
    expect(host).toContain('SYSTEM_SETUP_COMPLETED=false')
    expect(host).toContain('ADMIN_MFA_REQUIRED=false')
    expect(host).toContain('HTTP_ADDR=":9080"')
    expect(host).toContain('PUBLIC_BASE_URL="http://localhost:9080"')
    expect(relay).toContain('RELAY_ACCESS_KEY=relay_')
    expect(relay).toContain('RELAY_MANAGEMENT_URL="http://control.example.test:8080"')
    expect(relay).toContain('RELAY_VERSION=2.0.0')
    expect(relay).toContain('http://*) CURL_PROTOCOL_ARGS=(--proto "=http,https"')
    expect(relay).not.toContain('package URL must use HTTPS')
    expect(host).toContain('package URL must use HTTPS')
    expect(device).toContain('WENZWORK_DEVICE_ACCESS_KEY=device_')
    expect(deviceEnvironment).toContain('WENZWORK_CONTROL_URL=https://control.example.test')
    expect(deviceEnvironment).toContain('WENZWORK_DEVICE_ACCESS_KEY=device_')
    expect(deviceEnvironment.endsWith('\n')).toBe(true)
    expect(host).toContain('GITHUB_ACCESS_TOKEN=')
    expect(relay).toContain('GITHUB_ACCESS_TOKEN=')
    expect(device).toContain('GITHUB_ACCESS_TOKEN=')
    expect(installerFileName('relay', { platform: 'linux', architecture: 'x64' }, 'v2.0.0')).toBe(
      'wenzwork-relay-install-v2.0.0-linux-amd64.sh',
    )

    const bash = process.platform === 'win32' ? 'C:\\Program Files\\Git\\bin\\bash.exe' : 'bash'
    if (process.platform === 'win32' && !existsSync(bash)) return
    const directory = mkdtempSync(join(tmpdir(), 'wenzwork-installer-bash-'))
    try {
      for (const [name, script] of [
        ['host.sh', host],
        ['relay.sh', relay],
        ['device.sh', device],
        [
          'relay-bootstrap.sh',
          createRelayBootstrapInstaller('printf "relay bootstrap\\n"', {
            platform: 'linux',
            architecture: 'x64',
          }),
        ],
      ] as const) {
        const path = join(directory, name)
        writeFileSync(path, script, 'utf8')
        execFileSync(bash, ['-n', path])
      }
    } finally {
      rmSync(directory, { recursive: true, force: true })
    }
  })

  it('generates parseable PowerShell Host, Relay and Device installers', () => {
    const pwsh = process.platform === 'win32' ? 'pwsh.exe' : 'pwsh'
    if (
      spawnSync(pwsh, ['-NoLogo', '-NoProfile', '-Command', '$PSVersionTable.PSVersion.ToString()'])
        .status !== 0
    )
      return
    const scripts = [
      createHostInstaller(
        asset('host', 'windows'),
        { platform: 'windows', architecture: 'x64' },
        'https://control.example.test/host.tar.gz',
        credentials,
      ),
      createRelayInstaller(
        asset('relay', 'windows'),
        { platform: 'windows', architecture: 'x64' },
        'https://control.example.test/relay.tar.gz',
        {
          managementURL: 'https://control.example.test',
          accessKey: 'relay_' + 'R'.repeat(43),
        },
      ),
      createDeviceInstaller(
        asset('device-agent', 'windows'),
        { platform: 'windows', architecture: 'x64' },
        'https://control.example.test/device.tar.gz',
        'https://control.example.test',
        'device_' + 'B'.repeat(43),
      ),
      createRelayBootstrapInstaller("Write-Host 'Relay bootstrap'", {
        platform: 'windows',
        architecture: 'x64',
      }),
    ]
    scripts.slice(0, 3).forEach((script) => {
      expect(script).toContain("$ProgressPreference = 'SilentlyContinue'")
      expect(script).toContain('$installParent = Split-Path -Parent $InstallRoot')
      expect(script).toContain(
        "$temporaryRoot = Join-Path $installParent ('.wenzwork-install-' + [guid]::NewGuid().ToString('N'))",
      )
      expect(script).not.toContain('[IO.Path]::GetTempPath()')
    })
    const directory = mkdtempSync(join(tmpdir(), 'wenzwork-installer-powershell-'))
    try {
      scripts.forEach((script, index) => {
        const path = join(directory, `installer-${index}.ps1`)
        writeFileSync(path, script, 'utf8')
        const escaped = path.replaceAll("'", "''")
        execFileSync(pwsh, [
          '-NoLogo',
          '-NoProfile',
          '-Command',
          "[void][scriptblock]::Create([IO.File]::ReadAllText('" + escaped + "'))",
        ])
      })
    } finally {
      rmSync(directory, { recursive: true, force: true })
    }
  }, 20_000)
})
