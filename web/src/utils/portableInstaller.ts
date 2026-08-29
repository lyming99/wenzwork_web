import type { Release, ReleaseAsset } from '@/api/catalog'

export type PortablePlatform = 'linux' | 'windows' | 'macos'
export type PortableArchitecture = 'x64' | 'arm64'
export type PortableComponent = 'host' | 'relay' | 'device-agent'

export interface PortableTarget {
  platform: PortablePlatform
  architecture: PortableArchitecture
}

export interface HostInstallerCredentials {
  administratorEmail: string
  administratorPassword: string
}

export interface HostInstallerConfiguration extends HostInstallerCredentials {
  httpAddress?: string
  publicBaseURL?: string
}

export interface RelayInstallerConfiguration {
  accessKey: string
  managementURL: string
}

const portableAssetPattern =
  /^wenzwork-(host|relay|device-agent)-deployment-([A-Za-z0-9._+-]+)-(linux|windows|darwin)-(amd64|arm64)\.tar\.gz$/

const canonicalReleaseVersion = (value: string) => value.trim().replace(/^[vV](?=[0-9])/, '')

export interface PortableAssetDescriptor {
  component: PortableComponent
  version: string
  platform: 'linux' | 'windows' | 'darwin'
  architecture: 'amd64' | 'arm64'
}

export const parsePortableAssetFileName = (
  fileName: string,
): PortableAssetDescriptor | undefined => {
  const match = portableAssetPattern.exec(fileName)
  if (!match) return undefined
  return {
    component: match[1] as PortableComponent,
    version: match[2]!,
    platform: match[3] as PortableAssetDescriptor['platform'],
    architecture: match[4] as PortableAssetDescriptor['architecture'],
  }
}

export const portableAssetComponent = (asset: ReleaseAsset): PortableComponent | undefined => {
  return parsePortableAssetFileName(asset.fileName)?.component
}

export const portableAssetMatchesRelease = (
  asset: Pick<ReleaseAsset, 'fileName' | 'platform' | 'architecture'>,
  releaseVersion: string,
) => {
  const descriptor = parsePortableAssetFileName(asset.fileName)
  if (!descriptor) return false
  const metadataPlatform =
    asset.platform === 'macos'
      ? 'darwin'
      : asset.platform === 'linux' || asset.platform === 'windows'
        ? asset.platform
        : undefined
  const metadataArchitecture =
    asset.architecture === 'x64' ? 'amd64' : asset.architecture === 'arm64' ? 'arm64' : undefined
  return (
    canonicalReleaseVersion(descriptor.version) === canonicalReleaseVersion(releaseVersion) &&
    descriptor.platform === metadataPlatform &&
    descriptor.architecture === metadataArchitecture
  )
}

export const findPortableAsset = (
  release: Release | undefined,
  component: PortableComponent,
  target: PortableTarget,
) => {
  const wantedPlatform = targetPlatformName(target.platform)
  const wantedArchitecture = targetArchitectureName(target.architecture)
  return release?.assets.find((asset) => {
    const fileTarget = parsePortableAssetFileName(asset.fileName)
    return (
      fileTarget?.component === component &&
      canonicalReleaseVersion(fileTarget.version) === canonicalReleaseVersion(release.version) &&
      fileTarget.platform === wantedPlatform &&
      fileTarget.architecture === wantedArchitecture &&
      asset.platform === target.platform &&
      asset.architecture === target.architecture
    )
  })
}

export const detectPortableTarget = (): PortableTarget => {
  const userAgent = navigator.userAgent.toLowerCase()
  const platform: PortablePlatform = userAgent.includes('win')
    ? 'windows'
    : userAgent.includes('mac')
      ? 'macos'
      : 'linux'
  const architecture: PortableArchitecture = /arm64|aarch64/.test(userAgent) ? 'arm64' : 'x64'
  return { platform, architecture }
}

export const randomInstallerSecret = (length = 32) => {
  const alphabet = 'ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789_-'
  const bytes = new Uint8Array(length)
  crypto.getRandomValues(bytes)
  return Array.from(bytes, (value) => alphabet[value % alphabet.length]).join('')
}

const shellQuote = (value: string) => "'" + value.replaceAll("'", "'\"'\"'") + "'"
const powerShellQuote = (value: string) => "'" + value.replaceAll("'", "''") + "'"
const dotenvQuote = (value: string) =>
  '"' + value.replaceAll('\\', '\\\\').replaceAll('"', '\\"') + '"'
const targetPlatformName = (platform: PortablePlatform) =>
  platform === 'macos' ? 'darwin' : platform
const targetArchitectureName = (architecture: PortableArchitecture) =>
  architecture === 'x64' ? 'amd64' : 'arm64'

const shellDownloadAndExtract = (
  asset: ReleaseAsset,
  assetURL: string,
  installFolder: string,
  allowRemoteHTTP = false,
) => [
  'ASSET_URL=' + shellQuote(assetURL),
  'ASSET_NAME=' + shellQuote(asset.fileName),
  'ASSET_SHA256=' + shellQuote(asset.sha256.toLowerCase()),
  'INSTALL_FOLDER=' + shellQuote(installFolder),
  'SCRIPT_DIR="$(CDPATH=\'\' cd -- "$(dirname -- "$0")" && pwd -P)"',
  'if [[ -n "$WENZWORK_INSTALL_DIR" ]]; then INSTALL_ROOT="$WENZWORK_INSTALL_DIR"; else INSTALL_ROOT="$SCRIPT_DIR/$INSTALL_FOLDER"; fi',
  '[[ "$INSTALL_ROOT" == /* ]] || INSTALL_ROOT="$SCRIPT_DIR/$INSTALL_ROOT"',
  'if [[ -e "$INSTALL_ROOT" ]]; then echo "ERROR: install path already exists: $INSTALL_ROOT" >&2; exit 1; fi',
  'for command in curl tar mktemp; do command -v "$command" >/dev/null || { echo "ERROR: missing $command" >&2; exit 1; }; done',
  'case "$ASSET_URL" in',
  ...(allowRemoteHTTP
    ? [
        '  https://*) CURL_PROTOCOL_ARGS=(--proto "=https" --proto-redir "=https" --tlsv1.2) ;;',
        '  http://*) CURL_PROTOCOL_ARGS=(--proto "=http,https" --proto-redir "=http,https" --tlsv1.2) ;;',
        '  *) echo "ERROR: package URL must use HTTP or HTTPS" >&2; exit 1 ;;',
      ]
    : [
        '  https://*) CURL_PROTOCOL_ARGS=(--proto "=https" --proto-redir "=https" --tlsv1.2) ;;',
        '  http://localhost:*|http://127.0.0.1:*|http://\\[::1\\]:*) CURL_PROTOCOL_ARGS=(--proto "=http" --proto-redir "=http") ;;',
        '  *) echo "ERROR: package URL must use HTTPS (loopback HTTP is allowed)" >&2; exit 1 ;;',
      ]),
  'esac',
  'TEMP_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/wenzwork-install.XXXXXX")"',
  'trap \'rm -rf -- "$TEMP_ROOT"\' EXIT HUP INT TERM',
  'ARCHIVE="$TEMP_ROOT/$ASSET_NAME"',
  'curl --fail --silent --show-error --location "${CURL_PROTOCOL_ARGS[@]}" --connect-timeout 10 --max-time 1800 --output "$ARCHIVE" "$ASSET_URL"',
  'if command -v sha256sum >/dev/null; then ACTUAL_SHA256="$(sha256sum "$ARCHIVE" | awk \'{print $1}\')"; else ACTUAL_SHA256="$(shasum -a 256 "$ARCHIVE" | awk \'{print $1}\')"; fi',
  'ACTUAL_SHA256="$(printf "%s" "$ACTUAL_SHA256" | tr "[:upper:]" "[:lower:]")"',
  '[[ "$ACTUAL_SHA256" == "$ASSET_SHA256" ]] || { echo "ERROR: package SHA-256 mismatch" >&2; exit 1; }',
  'while IFS= read -r entry; do',
  '  case "$entry" in /*|../*|*/../*|*/..|*\\\\*) echo "ERROR: unsafe archive path: $entry" >&2; exit 1 ;; esac',
  'done < <(tar -tzf "$ARCHIVE")',
  'tar -tvzf "$ARCHIVE" | awk \'{type=substr($1,1,1); if (type != "-" && type != "d") exit 1}\' || { echo "ERROR: archive contains links or special files" >&2; exit 1; }',
  'mkdir -p "$TEMP_ROOT/package"',
  'tar -xzf "$ARCHIVE" -C "$TEMP_ROOT/package"',
  '[[ -f "$TEMP_ROOT/package/PACKAGE-MANIFEST.json" ]] || { echo "ERROR: package manifest is missing" >&2; exit 1; }',
  'mkdir -p "$(dirname -- "$INSTALL_ROOT")"',
  'mv -- "$TEMP_ROOT/package" "$INSTALL_ROOT"',
]

const powerShellDownloadAndExtract = (
  asset: ReleaseAsset,
  assetURL: string,
  installFolder: string,
) => [
  "$ProgressPreference = 'SilentlyContinue'",
  '$AssetUrl = ' + powerShellQuote(assetURL),
  '$AssetName = ' + powerShellQuote(asset.fileName),
  '$AssetSha256 = ' + powerShellQuote(asset.sha256.toLowerCase()),
  '$InstallFolder = ' + powerShellQuote(installFolder),
  'if ([string]::IsNullOrWhiteSpace($InstallRoot)) { $InstallRoot = Join-Path $PSScriptRoot $InstallFolder }',
  '$InstallRoot = [IO.Path]::GetFullPath($InstallRoot)',
  'if (Test-Path -LiteralPath $InstallRoot) { throw "Install path already exists: $InstallRoot" }',
  '$installParent = Split-Path -Parent $InstallRoot',
  '[void](New-Item -ItemType Directory -Path $installParent -Force)',
  "$temporaryRoot = Join-Path $installParent ('.wenzwork-install-' + [guid]::NewGuid().ToString('N'))",
  '[void](New-Item -ItemType Directory -Path $temporaryRoot)',
  'try {',
  '  $archive = Join-Path $temporaryRoot $AssetName',
  '  Invoke-WebRequest -UseBasicParsing -Uri $AssetUrl -OutFile $archive',
  '  $actualSha256 = (Get-FileHash -Algorithm SHA256 -LiteralPath $archive).Hash.ToLowerInvariant()',
  "  if ($actualSha256 -cne $AssetSha256) { throw 'Package SHA-256 mismatch.' }",
  '  $entries = @(& tar.exe -tzf $archive)',
  "  if ($LASTEXITCODE -ne 0 -or $entries.Count -eq 0) { throw 'Could not list package archive.' }",
  "  foreach ($entry in $entries) { if ($entry.StartsWith('/') -or $entry -match '(^|/)\\.\\.(/|$)' -or $entry.Contains('\\')) { throw \"Unsafe archive path: $entry\" } }",
  '  $metadata = @(& tar.exe -tvzf $archive)',
  "  if ($LASTEXITCODE -ne 0 -or @($metadata | Where-Object { $_ -and $_[0] -notin @('-', 'd') }).Count -ne 0) { throw 'Archive contains links or special files.' }",
  "  $stage = Join-Path $temporaryRoot 'package'",
  '  [void](New-Item -ItemType Directory -Path $stage)',
  '  & tar.exe -xzf $archive -C $stage',
  "  if ($LASTEXITCODE -ne 0 -or -not (Test-Path -LiteralPath (Join-Path $stage 'PACKAGE-MANIFEST.json') -PathType Leaf)) { throw 'Package extraction or manifest validation failed.' }",
  '  [IO.Directory]::Move($stage, $InstallRoot)',
]

const hostEnvironment = (configuration: HostInstallerConfiguration) => {
  const publicBaseURL = configuration.publicBaseURL ?? 'http://localhost:8080'
  const httpAddress = configuration.httpAddress ?? ':8080'
  return [
    '# Initial WenzWork Host configuration. Complete setup after the first sign-in.',
    'APP_ENV=development',
    'PUBLIC_BASE_URL=' + dotenvQuote(publicBaseURL),
    'HTTP_ADDR=' + dotenvQuote(httpAddress),
    'WEB_ROOT=web',
    'COOKIE_SECURE=false',
    'ADMIN_MFA_REQUIRED=false',
    'ALLOWED_ORIGINS=' + dotenvQuote(publicBaseURL),
    'SYSTEM_ADMIN_EMAIL=' + dotenvQuote(configuration.administratorEmail),
    'SYSTEM_ADMIN_PASSWORD=' + dotenvQuote(configuration.administratorPassword),
    'SYSTEM_ADMIN_DISPLAY_NAME=WenzWork Administrator',
    'SYSTEM_SETUP_COMPLETED=false',
    'GITHUB_RELEASE_REPOSITORY=lyming99/wenzwork_web',
    '# Optional; public GitHub Releases do not need an Access Token.',
    'GITHUB_ACCESS_TOKEN=',
  ]
}

export const createHostInstaller = (
  asset: ReleaseAsset,
  target: PortableTarget,
  assetURL: string,
  configuration: HostInstallerConfiguration,
) => {
  const publicBaseURL = configuration.publicBaseURL ?? 'http://localhost:8080'
  if (target.platform === 'windows') {
    const environmentLines = hostEnvironment(configuration)
      .map((line) => '    ' + powerShellQuote(line))
      .join(',\n')
    return [
      '#Requires -Version 5.1',
      '[CmdletBinding()]',
      'param([string]$InstallRoot)',
      'Set-StrictMode -Version Latest',
      "$ErrorActionPreference = 'Stop'",
      ...powerShellDownloadAndExtract(asset, assetURL, 'wenzwork-host'),
      "  [IO.File]::WriteAllLines((Join-Path $InstallRoot '.env'), @(\n" +
        environmentLines +
        '\n  ), [Text.UTF8Encoding]::new($false))',
      "  & (Join-Path $InstallRoot 'Init.ps1')",
      "  if ($LASTEXITCODE -ne 0) { throw 'Host initialization failed.' }",
      "  & (Join-Path $InstallRoot 'Start.ps1') -Background",
      "  if ($LASTEXITCODE -ne 0) { throw 'Host startup failed.' }",
      '  Write-Host ' + powerShellQuote('WenzWork Host is ready at ' + publicBaseURL),
      '  Write-Host ' + powerShellQuote('Administrator: ' + configuration.administratorEmail),
      '  Write-Host ' + powerShellQuote('Password: ' + configuration.administratorPassword),
      '} finally {',
      '  if (Test-Path -LiteralPath $temporaryRoot) { Remove-Item -LiteralPath $temporaryRoot -Recurse -Force }',
      '}',
      '',
    ].join('\r\n')
  }

  return [
    '#!/usr/bin/env bash',
    'set -Eeo pipefail',
    'umask 077',
    'WENZWORK_INSTALL_DIR="${WENZWORK_INSTALL_DIR-}"',
    ...shellDownloadAndExtract(asset, assetURL, 'wenzwork-host'),
    'cat > "$INSTALL_ROOT/.env" <<\'WENZWORK_HOST_ENV\'',
    ...hostEnvironment(configuration),
    'WENZWORK_HOST_ENV',
    'chmod 0600 "$INSTALL_ROOT/.env"',
    '(cd "$INSTALL_ROOT" && ./start.sh)',
    'printf "\\nWenzWork Host is ready at %s\\nAdministrator: %s\\nPassword: %s\\n" ' +
      shellQuote(publicBaseURL) +
      ' ' +
      shellQuote(configuration.administratorEmail) +
      ' ' +
      shellQuote(configuration.administratorPassword),
    '',
  ].join('\n')
}

const relayEnvironment = (asset: ReleaseAsset, configuration: RelayInstallerConfiguration) => {
  const version = parsePortableAssetFileName(asset.fileName)?.version ?? '0.0.0'
  return [
    '# WenzWork Relay one-click configuration.',
    'RELAY_ACCESS_KEY=' + configuration.accessKey,
    'RELAY_MANAGEMENT_URL=' + dotenvQuote(configuration.managementURL),
    'RELAY_VERSION=' + version,
    'GITHUB_RELEASE_REPOSITORY=lyming99/wenzwork_web',
    '# Optional; public GitHub Releases do not need an Access Token.',
    'GITHUB_ACCESS_TOKEN=',
  ]
}

export const createRelayInstaller = (
  asset: ReleaseAsset,
  target: PortableTarget,
  assetURL: string,
  configuration: RelayInstallerConfiguration,
) => {
  if (target.platform === 'windows') {
    const environmentLines = relayEnvironment(asset, configuration)
      .map((line) => '    ' + powerShellQuote(line))
      .join(',\n')
    return [
      '#Requires -Version 5.1',
      '[CmdletBinding()]',
      'param([string]$InstallRoot)',
      'Set-StrictMode -Version Latest',
      "$ErrorActionPreference = 'Stop'",
      ...powerShellDownloadAndExtract(asset, assetURL, 'wenzwork-relay'),
      "  [IO.File]::WriteAllLines((Join-Path $InstallRoot '.env'), @(\n" +
        environmentLines +
        '\n  ), [Text.UTF8Encoding]::new($false))',
      "  & (Join-Path $InstallRoot 'Init.ps1')",
      "  if ($LASTEXITCODE -ne 0) { throw 'Relay initialization failed.' }",
      "  & (Join-Path $InstallRoot 'Start.ps1') -Background",
      "  if ($LASTEXITCODE -ne 0) { throw 'Relay startup failed.' }",
      "  Write-Host 'WenzWork Relay installed and started.'",
      '} finally {',
      '  if (Test-Path -LiteralPath $temporaryRoot) { Remove-Item -LiteralPath $temporaryRoot -Recurse -Force }',
      '}',
      '',
    ].join('\r\n')
  }

  return [
    '#!/usr/bin/env bash',
    'set -Eeo pipefail',
    'umask 077',
    'WENZWORK_INSTALL_DIR="${WENZWORK_INSTALL_DIR-}"',
    ...shellDownloadAndExtract(asset, assetURL, 'wenzwork-relay', true),
    'cat > "$INSTALL_ROOT/.env" <<\'WENZWORK_RELAY_ENV\'',
    ...relayEnvironment(asset, configuration),
    'WENZWORK_RELAY_ENV',
    'chmod 0600 "$INSTALL_ROOT/.env"',
    '(cd "$INSTALL_ROOT" && ./start.sh)',
    'printf "\\nWenzWork Relay installed and started.\\n"',
    '',
  ].join('\n')
}
const deviceEnvironment = (controlURL: string, accessKey: string) => [
  '# WenzWork Device Agent one-click configuration.',
  'WENZWORK_CONTROL_URL=' + controlURL,
  'WENZWORK_DEVICE_ACCESS_KEY=' + accessKey,
  'WENZWORK_DEVICE_STATE_FILE=./runtime/state/agent-state.json',
  'WENZWORK_DEVICE_WORKSPACE=./workspace',
  'WENZWORK_AGENT_SECRET_STORE=file',
  '# Direct mode binds and advertises this exact IP and port. Replace the IP before enabling.',
  'WENZWORK_DEVICE_DIRECT_ENABLED=false',
  'WENZWORK_DEVICE_DIRECT_IP=127.0.0.1',
  'WENZWORK_DEVICE_DIRECT_PORT=9443',
  '# WENZWORK_DEVICE_DIRECT_ACCESS_KEY=device_replace_with_a_43_character_urlsafe_access_key',
  '# WENZWORK_DEVICE_DIRECT_TLS_CERT_FILE=./config/direct-cert.pem',
  '# WENZWORK_DEVICE_DIRECT_TLS_KEY_FILE=./config/direct-key.pem',
  'GITHUB_RELEASE_REPOSITORY=lyming99/wenzwork_web',
  '# Optional; public GitHub Releases do not need an Access Token.',
  'GITHUB_ACCESS_TOKEN=',
]

/** A ready-to-use Device Agent dotenv file containing the one-time Access Key. */
export const createDeviceEnvironmentFile = (controlURL: string, accessKey: string) =>
  [...deviceEnvironment(controlURL, accessKey), ''].join('\n')

export const createDeviceInstaller = (
  asset: ReleaseAsset,
  target: PortableTarget,
  assetURL: string,
  controlURL: string,
  accessKey: string,
) => {
  if (target.platform === 'windows') {
    const environmentLines = deviceEnvironment(controlURL, accessKey)
      .map((line) => '    ' + powerShellQuote(line))
      .join(',\n')
    return [
      '#Requires -Version 5.1',
      '[CmdletBinding()]',
      'param([string]$InstallRoot)',
      'Set-StrictMode -Version Latest',
      "$ErrorActionPreference = 'Stop'",
      ...powerShellDownloadAndExtract(asset, assetURL, 'wenzwork-device-agent'),
      "  [IO.File]::WriteAllLines((Join-Path $InstallRoot '.env'), @(\n" +
        environmentLines +
        '\n  ), [Text.UTF8Encoding]::new($false))',
      "  & (Join-Path $InstallRoot 'Init.ps1')",
      "  if ($LASTEXITCODE -ne 0) { throw 'Device Agent initialization failed.' }",
      "  & (Join-Path $InstallRoot 'Start.ps1') -Background",
      "  if ($LASTEXITCODE -ne 0) { throw 'Device Agent startup failed.' }",
      "  Write-Host 'WenzWork Device Agent installed and started.'",
      '} finally {',
      '  if (Test-Path -LiteralPath $temporaryRoot) { Remove-Item -LiteralPath $temporaryRoot -Recurse -Force }',
      '}',
      '',
    ].join('\r\n')
  }
  return [
    '#!/usr/bin/env bash',
    'set -Eeo pipefail',
    'umask 077',
    'WENZWORK_INSTALL_DIR="${WENZWORK_INSTALL_DIR-}"',
    ...shellDownloadAndExtract(asset, assetURL, 'wenzwork-device-agent'),
    'cat > "$INSTALL_ROOT/.env" <<\'WENZWORK_DEVICE_ENV\'',
    ...deviceEnvironment(controlURL, accessKey),
    'WENZWORK_DEVICE_ENV',
    'chmod 0600 "$INSTALL_ROOT/.env"',
    '(cd "$INSTALL_ROOT" && ./start.sh)',
    'printf "\\nWenzWork Device Agent installed and started.\\n"',
    '',
  ].join('\n')
}

export const installerFileName = (
  component: PortableComponent,
  target: PortableTarget,
  releaseVersion: string,
) => {
  const version = canonicalReleaseVersion(releaseVersion).replace(/[^A-Za-z0-9._+-]/g, '-')
  if (!version) throw new Error('Installer release version is invalid')
  const extension = target.platform === 'windows' ? 'ps1' : 'sh'
  return (
    'wenzwork-' +
    component +
    '-install-v' +
    version +
    '-' +
    targetPlatformName(target.platform) +
    '-' +
    targetArchitectureName(target.architecture) +
    '.' +
    extension
  )
}

export const deviceEnvironmentFileName = () => '.env'

export const createRelayBootstrapInstaller = (installCommand: string, target: PortableTarget) => {
  const command = installCommand.trim()
  if (!command || command.includes('\0')) throw new Error('Relay install command is invalid')
  if (target.platform === 'windows') {
    return [
      '#Requires -Version 5.1',
      '[CmdletBinding()]',
      'param()',
      'Set-StrictMode -Version Latest',
      "$ErrorActionPreference = 'Stop'",
      "$ProgressPreference = 'SilentlyContinue'",
      command,
      '',
    ].join('\r\n')
  }
  return ['#!/usr/bin/env bash', 'set -Eeo pipefail', 'umask 077', command, ''].join('\n')
}

export const downloadInstallerFile = (contents: string, fileName: string) => {
  const blob = new Blob([contents], { type: 'text/plain;charset=utf-8' })
  const link = document.createElement('a')
  link.href = URL.createObjectURL(blob)
  link.download = fileName
  link.click()
  URL.revokeObjectURL(link.href)
}
