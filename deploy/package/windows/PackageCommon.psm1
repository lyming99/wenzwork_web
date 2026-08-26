Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

function Write-PackageLog {
    param([Parameter(Mandatory = $true)][string]$Message)
    Write-Host "[wenzwork-package] $Message"
}

function ConvertFrom-PackageDoubleQuotedValue {
    param([Parameter(Mandatory = $true)][AllowEmptyString()][string]$Value)
    $decoded = [Text.StringBuilder]::new()
    for ($index = 0; $index -lt $Value.Length; $index++) {
        $character = $Value[$index]
        if ($character -ne [char]92) {
            [void]$decoded.Append($character)
            continue
        }
        $index++
        if ($index -ge $Value.Length) { throw 'Invalid trailing escape in environment value.' }
        $next = $Value[$index]
        if ($next -eq [char]92 -or $next -eq [char]34) {
            [void]$decoded.Append($next)
        }
        else {
            [void]$decoded.Append([char]92)
            [void]$decoded.Append($next)
        }
    }
    return $decoded.ToString()
}

function Read-PackageEnvironment {
    param([Parameter(Mandatory = $true)][string]$Path)
    if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) {
        throw "Environment file is missing: $Path"
    }
    $values = [ordered]@{}
    foreach ($rawLine in [IO.File]::ReadAllLines($Path)) {
        $line = $rawLine.Trim()
        if ($line.Length -eq 0 -or $line.StartsWith('#')) { continue }
        if ($line.StartsWith('export ')) { $line = $line.Substring(7).Trim() }
        if ($line -notmatch '^([A-Za-z_][A-Za-z0-9_]*)\s*=(.*)$') {
            throw "Invalid environment entry in $Path"
        }
        $key = $Matches[1]
        $value = $Matches[2].Trim()
        if ($value.Length -ge 2) {
            $first = $value.Substring(0, 1)
            $last = $value.Substring($value.Length - 1, 1)
            if ($first -eq '"' -and $last -eq '"') {
                $value = $value.Substring(1, $value.Length - 2)
                $value = ConvertFrom-PackageDoubleQuotedValue -Value $value
            }
            elseif ($first -eq "'" -and $last -eq "'") {
                $value = $value.Substring(1, $value.Length - 2)
            }
        }
        $values[$key] = $value
    }
    return $values
}

function Import-PackageEnvironment {
    param([Parameter(Mandatory = $true)][string]$Path)
    $values = Read-PackageEnvironment -Path $Path
    foreach ($entry in $values.GetEnumerator()) {
        [Environment]::SetEnvironmentVariable($entry.Key, [string]$entry.Value, 'Process')
    }
    return $values
}

function Set-PackageEnvironmentValue {
    param(
        [Parameter(Mandatory = $true)][string]$Path,
        [Parameter(Mandatory = $true)][string]$Name,
        [Parameter(Mandatory = $true)][AllowEmptyString()][string]$Value
    )
    $lines = [Collections.Generic.List[string]]::new()
    $found = $false
    foreach ($line in [IO.File]::ReadAllLines($Path)) {
        if ($line.Trim() -match "^$([regex]::Escape($Name))\s*=") {
            if (-not $found) { $lines.Add("$Name=$Value") }
            $found = $true
        }
        else {
            $lines.Add($line)
        }
    }
    if (-not $found) {
        $lines.Add('')
        $lines.Add("$Name=$Value")
    }
    $temporary = "$Path.tmp.$([Guid]::NewGuid().ToString('N'))"
    try {
        [IO.File]::WriteAllLines($temporary, $lines, [Text.UTF8Encoding]::new($false))
        Move-Item -LiteralPath $temporary -Destination $Path -Force
    }
    finally {
        Remove-Item -LiteralPath $temporary -Force -ErrorAction SilentlyContinue
    }
}

function Get-PackageMetadata {
    param([Parameter(Mandatory = $true)][string]$Root)
    $metadata = Read-PackageEnvironment -Path (Join-Path $Root 'config\package.env')
    foreach ($name in @(
        'WENZWORK_PACKAGE_COMPONENT',
        'WENZWORK_PACKAGE_PLATFORM',
        'WENZWORK_PACKAGE_ARCHITECTURE',
        'WENZWORK_PACKAGE_VERSION',
        'WENZWORK_PACKAGE_ASSET_BASENAME',
        'WENZWORK_PACKAGE_CHECKSUM_ASSET',
        'WENZWORK_GITHUB_REPOSITORY'
    )) {
        if (-not $metadata.Contains($name) -or [string]::IsNullOrWhiteSpace($metadata[$name])) {
            throw "Package metadata is missing $name."
        }
    }
    return $metadata
}

function Get-PackageEnvironmentTemplatePath {
    param(
        [Parameter(Mandatory = $true)][string]$Root,
        [Parameter(Mandatory = $true)]$Metadata
    )
    $relativePath = switch ([string]$Metadata.WENZWORK_PACKAGE_COMPONENT) {
        'host' { 'config\host.env.example' }
        'relay' { 'config\relay.env.example' }
        'device-agent' { 'config\device-agent.env.example' }
        default { throw "Unknown package component: $($Metadata.WENZWORK_PACKAGE_COMPONENT)" }
    }
    $path = Join-Path $Root $relativePath
    $item = Get-Item -LiteralPath $path -Force -ErrorAction SilentlyContinue
    if ($null -eq $item -or $item.PSIsContainer -or
        ($item.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) {
        throw "Environment template is missing or unsafe: $relativePath"
    }
    return $item.FullName
}

function Initialize-PackageEnvironment {
    param(
        [Parameter(Mandatory = $true)][string]$Root,
        [Parameter(Mandatory = $true)]$Metadata
    )
    $environmentPath = Join-Path $Root '.env'
    $existing = Get-Item -LiteralPath $environmentPath -Force -ErrorAction SilentlyContinue
    if ($null -ne $existing) {
        if ($existing.PSIsContainer -or
            ($existing.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) {
            throw "Environment path must be a regular file and must not be a reparse point: $environmentPath"
        }
        return [pscustomobject]@{ Path = $existing.FullName; Created = $false }
    }

    $templatePath = Get-PackageEnvironmentTemplatePath -Root $Root -Metadata $Metadata
    $temporary = "$environmentPath.init.$([Guid]::NewGuid().ToString('N'))"
    $created = $false
    try {
        [IO.File]::WriteAllBytes($temporary, [IO.File]::ReadAllBytes($templatePath))
        try {
            [IO.File]::Move($temporary, $environmentPath)
            $created = $true
        }
        catch [IO.IOException] {
            $existing = Get-Item -LiteralPath $environmentPath -Force -ErrorAction SilentlyContinue
            if ($null -eq $existing -or $existing.PSIsContainer -or
                ($existing.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) {
                throw
            }
        }
    }
    finally {
        Remove-Item -LiteralPath $temporary -Force -ErrorAction SilentlyContinue
    }
    if ($created) {
        Write-PackageLog -Message "Created $environmentPath from $templatePath."
    }
    $environment = Get-Item -LiteralPath $environmentPath -Force
    return [pscustomobject]@{ Path = $environment.FullName; Created = $created }
}

function Get-PackageHostArchitecture {
    $raw = [Environment]::GetEnvironmentVariable('PROCESSOR_ARCHITEW6432')
    if ([string]::IsNullOrWhiteSpace($raw)) {
        $raw = [Environment]::GetEnvironmentVariable('PROCESSOR_ARCHITECTURE')
    }
    switch ($raw.ToUpperInvariant()) {
        'AMD64' { return 'amd64' }
        'ARM64' { return 'arm64' }
        default { throw "Unsupported Windows architecture: $raw" }
    }
}

function Set-PackageComponentDefaults {
    param(
        [Parameter(Mandatory = $true)][string]$Root,
        [Parameter(Mandatory = $true)]$Metadata
    )
    if ($Metadata.WENZWORK_PACKAGE_COMPONENT -cne 'host') { return }
    $environment = 'production'
    $publicBaseUrl = 'https://wenzwork.com'
    $cookieSecure = 'false'
    $setupCompleted = [Environment]::GetEnvironmentVariable('SYSTEM_SETUP_COMPLETED', 'Process')
    if ($setupCompleted -ne 'true' -or
        (Test-Path -LiteralPath (Join-Path $Root 'config\relay-bootstrap\TEST_ONLY_SIGNING_KEY') -PathType Leaf)) {
        $environment = 'development'
        $publicBaseUrl = 'http://localhost:8080'
        $cookieSecure = 'false'
    }
    $defaults = [ordered]@{
        WENZWORK_ENV_FILE = (Join-Path $Root '.env')
        MIGRATIONS_DIR = (Join-Path $Root 'migrations')
        APP_ENV = $environment
        PUBLIC_BASE_URL = $publicBaseUrl
        HTTP_ADDR = ':8080'
        WEB_ROOT = (Join-Path $Root 'web')
        LOG_LEVEL = 'info'
        REGISTRATION_ENABLED = 'true'
        COOKIE_SECURE = $cookieSecure
        ADMIN_MFA_REQUIRED = 'false'
        ALLOWED_ORIGINS = $publicBaseUrl
        HOST_SECRETS_FILE = (Join-Path $Root 'cache\host-secrets\application.env')
        RELEASE_ASSET_CACHE_DIR = (Join-Path $Root 'cache\releases')
        GITHUB_RELEASE_REPOSITORY = [string]$Metadata.WENZWORK_GITHUB_REPOSITORY
        RELAY_DEVELOPMENT_CA_DIR = (Join-Path $Root 'cache\host-secrets\relay-ca')
        RELAY_BOOTSTRAP_ASSETS_DIR = (Join-Path $Root 'config\relay-bootstrap')
        REMOTE_MVP_ENABLED = 'true'
    }
    foreach ($entry in $defaults.GetEnumerator()) {
        if ([string]::IsNullOrWhiteSpace([Environment]::GetEnvironmentVariable($entry.Key, 'Process'))) {
            [Environment]::SetEnvironmentVariable($entry.Key, [string]$entry.Value, 'Process')
        }
    }
}

function Get-PackageRequiredBinary {
    param([Parameter(Mandatory = $true)][string]$Component)
    switch ($Component) {
        'host' { return 'wenzwork-api.exe' }
        'relay' { return 'wenzwork-relay-server.exe' }
        'device-agent' { return 'wenzwork-device-agent.exe' }
        default { throw "Unknown package component: $Component" }
    }
}

function Assert-PackageTree {
    param([Parameter(Mandatory = $true)][string]$Root)
    $resolved = [IO.Path]::GetFullPath($Root).TrimEnd('\')
    if ([string]::IsNullOrWhiteSpace($resolved) -or $resolved -eq [IO.Path]::GetPathRoot($resolved)) {
        throw "Unsafe package root: $resolved"
    }
    foreach ($directory in @('bin', 'config', 'runtime', 'workspace', 'cache')) {
        if (-not (Test-Path -LiteralPath (Join-Path $resolved $directory) -PathType Container)) {
            throw "Required directory is missing: $directory"
        }
    }
    foreach ($file in @('start.sh', 'upgrade.sh', 'Start.ps1', 'Init.ps1', 'Upgrade.ps1', 'VERSION', 'PACKAGE-MANIFEST.json')) {
        if (-not (Test-Path -LiteralPath (Join-Path $resolved $file) -PathType Leaf)) {
            throw "Required file is missing: $file"
        }
    }
    $metadata = Get-PackageMetadata -Root $resolved
    [void](Get-PackageEnvironmentTemplatePath -Root $resolved -Metadata $metadata)
    $binary = Get-PackageRequiredBinary -Component $metadata.WENZWORK_PACKAGE_COMPONENT
    if (-not (Test-Path -LiteralPath (Join-Path $resolved "bin\$binary") -PathType Leaf)) {
        throw "Required executable is missing: $binary"
    }
    return $metadata
}

function Initialize-PackageRuntimeDirectories {
    param([Parameter(Mandatory = $true)][string]$Root)
    foreach ($relative in @('runtime\logs', 'runtime\pids', 'runtime\state', 'workspace', 'cache\backups')) {
        [void](New-Item -ItemType Directory -Path (Join-Path $Root $relative) -Force)
    }
}

function Get-PackageFileSha256 {
    param([Parameter(Mandatory = $true)][string]$Path)
    return (Get-FileHash -LiteralPath $Path -Algorithm SHA256).Hash.ToLowerInvariant()
}

function Get-PackageSettingValue {
    param(
        [Parameter(Mandatory = $true)][Collections.IDictionary]$Values,
        [Parameter(Mandatory = $true)][string]$Name
    )
    if (-not $Values.Contains($Name)) { return '' }
    return [string]$Values[$Name]
}

function ConvertTo-PackageSafeVersion {
    param([Parameter(Mandatory = $true)][string]$Version)
    return $Version -replace '[^A-Za-z0-9._-]', '-'
}

function ConvertTo-PackageGitHubTag {
    param([Parameter(Mandatory = $true)][string]$Version)
    if ($Version -match '^[0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.-]+)?$') {
        return "v$Version"
    }
    return $Version
}

function Test-PackageRemoteUri {
    param([Parameter(Mandatory = $true)][string]$Uri)
    $parsed = $null
    if (-not [Uri]::TryCreate($Uri, [UriKind]::Absolute, [ref]$parsed)) { return $false }
    if (-not [string]::IsNullOrEmpty($parsed.UserInfo)) { return $false }
    if ($parsed.Scheme -ceq 'https') { return $true }
    return $parsed.Scheme -ceq 'http' -and $parsed.IsLoopback
}

function Invoke-PackageRemoteFile {
    param(
        [Parameter(Mandatory = $true)][string]$Uri,
        [Parameter(Mandatory = $true)][string]$Destination,
        [AllowEmptyString()][string]$Token = '',
        [string]$Accept = 'application/octet-stream'
    )
    if (-not (Test-PackageRemoteUri -Uri $Uri)) { return $false }
    if (-not [string]::IsNullOrWhiteSpace($Token) -and $Token -notmatch '^[A-Za-z0-9._-]+$') {
        return $false
    }
    $headers = @{
        Accept = $Accept
        'X-GitHub-Api-Version' = '2022-11-28'
        'User-Agent' = 'wenzwork-deployment-upgrader'
    }
    if (-not [string]::IsNullOrWhiteSpace($Token)) { $headers.Authorization = "Bearer $Token" }
    Remove-Item -LiteralPath $Destination -Force -ErrorAction SilentlyContinue
    try {
        [void](Invoke-WebRequest -UseBasicParsing -Headers $headers -Uri $Uri -OutFile $Destination)
        $download = Get-Item -LiteralPath $Destination -Force -ErrorAction SilentlyContinue
        if ($null -eq $download -or $download.PSIsContainer -or $download.Length -eq 0) {
            Remove-Item -LiteralPath $Destination -Force -ErrorAction SilentlyContinue
            return $false
        }
        return $true
    }
    catch {
        Remove-Item -LiteralPath $Destination -Force -ErrorAction SilentlyContinue
        return $false
    }
    finally {
        $headers.Remove('Authorization')
    }
}

function Invoke-PackageSourceDownload {
    param(
        [Parameter(Mandatory = $true)][string]$Uri,
        [Parameter(Mandatory = $true)][string]$Destination,
        [AllowEmptyString()][string]$Token = '',
        [string]$Accept = 'application/octet-stream',
        [AllowNull()][scriptblock]$DownloadCommand
    )
    Remove-Item -LiteralPath $Destination -Force -ErrorAction SilentlyContinue
    if ($null -eq $DownloadCommand) {
        $succeeded = Invoke-PackageRemoteFile -Uri $Uri -Destination $Destination -Token $Token -Accept $Accept
    }
    else {
        $result = @(& $DownloadCommand -Uri $Uri -Destination $Destination -Token $Token -Accept $Accept)
        $succeeded = $result.Count -eq 1 -and [bool]$result[0]
    }
    $download = Get-Item -LiteralPath $Destination -Force -ErrorAction SilentlyContinue
    if (-not $succeeded -or $null -eq $download -or $download.PSIsContainer -or $download.Length -eq 0) {
        Remove-Item -LiteralPath $Destination -Force -ErrorAction SilentlyContinue
        return $false
    }
    return $true
}

function Get-PackageCatalogAsset {
    param(
        [Parameter(Mandatory = $true)]$Payload,
        [Parameter(Mandatory = $true)][Collections.IDictionary]$Metadata,
        [AllowEmptyString()][string]$Version = ''
    )
    $itemsProperty = $Payload.PSObject.Properties['items']
    $releases = if ($null -eq $itemsProperty) { @($Payload) } else { @($itemsProperty.Value) }
    $platform = [string]$Metadata.WENZWORK_PACKAGE_PLATFORM
    $catalogPlatform = if ($platform -ceq 'darwin') { 'macos' } else { $platform }
    $architecture = [string]$Metadata.WENZWORK_PACKAGE_ARCHITECTURE
    $catalogArchitecture = if ($architecture -ceq 'amd64') { 'x64' } else { $architecture }
    $basename = [string]$Metadata.WENZWORK_PACKAGE_ASSET_BASENAME
    $suffix = "-$platform-$architecture.tar.gz"
    $wantedNames = [Collections.Generic.HashSet[string]]::new([StringComparer]::Ordinal)
    if (-not [string]::IsNullOrWhiteSpace($Version)) {
        $safeVersion = ConvertTo-PackageSafeVersion -Version $Version
        [void]$wantedNames.Add("$basename-$safeVersion$suffix")
        if ($Version -match '^[0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.-]+)?$') {
            [void]$wantedNames.Add("$basename-v$safeVersion$suffix")
        }
    }

    $assetMatches = [Collections.Generic.List[object]]::new()
    foreach ($release in $releases) {
        if ($null -eq $release) { continue }
        $assetsProperty = $release.PSObject.Properties['assets']
        if ($null -eq $assetsProperty) { continue }
        foreach ($asset in @($assetsProperty.Value)) {
            if ($null -eq $asset) { continue }
            $nameProperty = $asset.PSObject.Properties['fileName']
            $platformProperty = $asset.PSObject.Properties['platform']
            $architectureProperty = $asset.PSObject.Properties['architecture']
            $sha256Property = $asset.PSObject.Properties['sha256']
            $downloadProperty = $asset.PSObject.Properties['downloadUrl']
            if ($null -in @($nameProperty, $platformProperty, $architectureProperty, $sha256Property, $downloadProperty)) {
                continue
            }
            $name = [string]$nameProperty.Value
            if ([string]$platformProperty.Value -cne $catalogPlatform -or
                [string]$architectureProperty.Value -cne $catalogArchitecture) {
                continue
            }
            $nameMatches = if ($wantedNames.Count -gt 0) {
                $wantedNames.Contains($name)
            }
            else {
                $name.StartsWith("$basename-", [StringComparison]::Ordinal) -and
                    $name.EndsWith($suffix, [StringComparison]::Ordinal)
            }
            if (-not $nameMatches -or $name -notmatch '^[A-Za-z0-9][A-Za-z0-9._+-]*\.tar\.gz$') { continue }
            $sha256 = ([string]$sha256Property.Value).ToLowerInvariant()
            $downloadUrl = [string]$downloadProperty.Value
            if ($sha256 -notmatch '^[0-9a-f]{64}$' -or [string]::IsNullOrWhiteSpace($downloadUrl)) { continue }
            $assetMatches.Add([pscustomobject]@{
                Name = $name
                Sha256 = $sha256
                DownloadUrl = $downloadUrl
            })
        }
    }
    if ($assetMatches.Count -ne 1) { return $null }
    return $assetMatches[0]
}

function Get-PackageChecksumArchiveName {
    param(
        [Parameter(Mandatory = $true)][string]$Path,
        [Parameter(Mandatory = $true)][Collections.IDictionary]$Metadata,
        [AllowEmptyString()][string]$ExpectedName = ''
    )
    $prefix = "$($Metadata.WENZWORK_PACKAGE_ASSET_BASENAME)-"
    $suffix = "-$($Metadata.WENZWORK_PACKAGE_PLATFORM)-$($Metadata.WENZWORK_PACKAGE_ARCHITECTURE).tar.gz"
    $archiveMatches = [Collections.Generic.List[string]]::new()
    foreach ($line in [IO.File]::ReadAllLines($Path)) {
        if ($line -notmatch '^[0-9A-Fa-f]{64}\s+\*?(.+)$') { continue }
        $candidate = $Matches[1] -replace '^[.][\\/]', ''
        $wanted = if (-not [string]::IsNullOrWhiteSpace($ExpectedName)) {
            $candidate -ceq $ExpectedName
        }
        else {
            $candidate.StartsWith($prefix, [StringComparison]::Ordinal) -and
                $candidate.EndsWith($suffix, [StringComparison]::Ordinal)
        }
        if ($wanted -and $candidate -match '^[A-Za-z0-9][A-Za-z0-9._+-]*\.tar\.gz$') {
            $archiveMatches.Add($candidate)
        }
    }
    if ($archiveMatches.Count -ne 1) { return '' }
    return $archiveMatches[0]
}

function Get-PackageOfficialReleaseDownload {
    param(
        [Parameter(Mandatory = $true)][string]$BaseUrl,
        [Parameter(Mandatory = $true)][Collections.IDictionary]$Metadata,
        [AllowEmptyString()][string]$Version,
        [Parameter(Mandatory = $true)][string]$TemporaryDirectory,
        [AllowNull()][scriptblock]$DownloadCommand
    )
    $BaseUrl = $BaseUrl.TrimEnd('/')
    if (-not (Test-PackageRemoteUri -Uri $BaseUrl)) { return $null }
    $catalogPlatform = if ($Metadata.WENZWORK_PACKAGE_PLATFORM -ceq 'darwin') { 'macos' } else { [string]$Metadata.WENZWORK_PACKAGE_PLATFORM }
    $catalogArchitecture = if ($Metadata.WENZWORK_PACKAGE_ARCHITECTURE -ceq 'amd64') { 'x64' } else { [string]$Metadata.WENZWORK_PACKAGE_ARCHITECTURE }
    $query = "project=web&channel=stable&platform=$catalogPlatform&architecture=$catalogArchitecture"
    $metadataUrl = if ([string]::IsNullOrWhiteSpace($Version)) {
        "$BaseUrl/api/v1/releases/latest?$query"
    }
    else {
        "$BaseUrl/api/v1/releases?$query&limit=50"
    }
    $metadataFile = Join-Path $TemporaryDirectory 'official-release.json'
    if (-not (Invoke-PackageSourceDownload -Uri $metadataUrl -Destination $metadataFile -Accept 'application/json' -DownloadCommand $DownloadCommand)) {
        return $null
    }
    try {
        $payload = [IO.File]::ReadAllText($metadataFile) | ConvertFrom-Json
        $asset = Get-PackageCatalogAsset -Payload $payload -Metadata $Metadata -Version $Version
        if ($null -eq $asset) { return $null }
        if ([string]$asset.DownloadUrl -match '^/') {
            $downloadUrl = "$BaseUrl$($asset.DownloadUrl)"
        }
        elseif (Test-PackageRemoteUri -Uri ([string]$asset.DownloadUrl)) {
            $downloadUrl = [string]$asset.DownloadUrl
        }
        else {
            return $null
        }
        $packageFile = Join-Path $TemporaryDirectory ([string]$asset.Name)
        if (-not (Invoke-PackageSourceDownload -Uri $downloadUrl -Destination $packageFile -DownloadCommand $DownloadCommand)) {
            return $null
        }
        $checksumsFile = Join-Path $TemporaryDirectory "official-$($Metadata.WENZWORK_PACKAGE_CHECKSUM_ASSET)"
        [IO.File]::WriteAllText(
            $checksumsFile,
            "$($asset.Sha256)  $($asset.Name)`r`n",
            [Text.UTF8Encoding]::new($false)
        )
        Write-PackageLog -Message "Downloaded $($asset.Name) from $BaseUrl."
        return [pscustomobject]@{ PackageFile = $packageFile; ChecksumsFile = $checksumsFile; Source = $BaseUrl }
    }
    catch {
        return $null
    }
}

function Get-PackageGitHubApiReleaseDownload {
    param(
        [Parameter(Mandatory = $true)][string]$Repository,
        [Parameter(Mandatory = $true)][string]$Token,
        [Parameter(Mandatory = $true)][Collections.IDictionary]$Metadata,
        [AllowEmptyString()][string]$Version,
        [Parameter(Mandatory = $true)][string]$TemporaryDirectory,
        [AllowNull()][scriptblock]$DownloadCommand,
        [string]$GitHubApiBaseUrl = 'https://api.github.com'
    )
    $GitHubApiBaseUrl = $GitHubApiBaseUrl.TrimEnd('/')
    if (-not (Test-PackageRemoteUri -Uri $GitHubApiBaseUrl)) { return $null }
    $releasePath = 'latest'
    if (-not [string]::IsNullOrWhiteSpace($Version)) {
        $tag = ConvertTo-PackageGitHubTag -Version $Version
        $releasePath = 'tags/' + [Uri]::EscapeDataString($tag)
    }
    $releaseFile = Join-Path $TemporaryDirectory 'github-release.json'
    $releaseUrl = "$GitHubApiBaseUrl/repos/$Repository/releases/$releasePath"
    if (-not (Invoke-PackageSourceDownload -Uri $releaseUrl -Destination $releaseFile -Token $Token -Accept 'application/vnd.github+json' -DownloadCommand $DownloadCommand)) {
        return $null
    }
    try {
        $release = [IO.File]::ReadAllText($releaseFile) | ConvertFrom-Json
        $tagProperty = $release.PSObject.Properties['tag_name']
        $assetsProperty = $release.PSObject.Properties['assets']
        if ($null -eq $tagProperty -or $null -eq $assetsProperty -or [string]::IsNullOrWhiteSpace([string]$tagProperty.Value)) {
            return $null
        }
        $safeTag = ConvertTo-PackageSafeVersion -Version ([string]$tagProperty.Value)
        $archiveName = "$($Metadata.WENZWORK_PACKAGE_ASSET_BASENAME)-$safeTag-$($Metadata.WENZWORK_PACKAGE_PLATFORM)-$($Metadata.WENZWORK_PACKAGE_ARCHITECTURE).tar.gz"
        $archiveAssets = [Collections.Generic.List[object]]::new()
        $checksumAssets = [Collections.Generic.List[object]]::new()
        $assetUrlPrefix = "$GitHubApiBaseUrl/repos/$Repository/releases/assets/"
        foreach ($asset in @($assetsProperty.Value)) {
            if ($null -eq $asset) { continue }
            $nameProperty = $asset.PSObject.Properties['name']
            $urlProperty = $asset.PSObject.Properties['url']
            if ($null -eq $nameProperty -or $null -eq $urlProperty -or
                -not ([string]$urlProperty.Value).StartsWith($assetUrlPrefix, [StringComparison]::Ordinal) -or
                ([string]$urlProperty.Value).Substring($assetUrlPrefix.Length) -notmatch '^[0-9]+$') {
                continue
            }
            if ([string]$nameProperty.Value -ceq $archiveName) { $archiveAssets.Add($asset) }
            if ([string]$nameProperty.Value -ceq [string]$Metadata.WENZWORK_PACKAGE_CHECKSUM_ASSET) { $checksumAssets.Add($asset) }
        }
        if ($archiveAssets.Count -ne 1 -or $checksumAssets.Count -ne 1) { return $null }
        $packageFile = Join-Path $TemporaryDirectory $archiveName
        $checksumsFile = Join-Path $TemporaryDirectory ([string]$Metadata.WENZWORK_PACKAGE_CHECKSUM_ASSET)
        $checksumUrl = [string]$checksumAssets[0].PSObject.Properties['url'].Value
        $archiveUrl = [string]$archiveAssets[0].PSObject.Properties['url'].Value
        if (-not (Invoke-PackageSourceDownload -Uri $checksumUrl -Destination $checksumsFile -Token $Token -DownloadCommand $DownloadCommand) -or
            -not (Invoke-PackageSourceDownload -Uri $archiveUrl -Destination $packageFile -Token $Token -DownloadCommand $DownloadCommand)) {
            return $null
        }
        Write-PackageLog -Message "Downloaded $archiveName through the authenticated GitHub Release API."
        return [pscustomobject]@{ PackageFile = $packageFile; ChecksumsFile = $checksumsFile; Source = 'github-api' }
    }
    catch {
        return $null
    }
}

function Get-PackagePublicGitHubReleaseDownload {
    param(
        [Parameter(Mandatory = $true)][string]$Repository,
        [Parameter(Mandatory = $true)][Collections.IDictionary]$Metadata,
        [AllowEmptyString()][string]$Version,
        [Parameter(Mandatory = $true)][string]$TemporaryDirectory,
        [AllowNull()][scriptblock]$DownloadCommand,
        [string]$GitHubWebBaseUrl = 'https://github.com'
    )
    $GitHubWebBaseUrl = $GitHubWebBaseUrl.TrimEnd('/')
    if (-not (Test-PackageRemoteUri -Uri $GitHubWebBaseUrl)) { return $null }
    $expectedName = ''
    if ([string]::IsNullOrWhiteSpace($Version)) {
        $releasePath = 'latest/download'
    }
    else {
        $tag = ConvertTo-PackageGitHubTag -Version $Version
        $safeTag = ConvertTo-PackageSafeVersion -Version $tag
        $expectedName = "$($Metadata.WENZWORK_PACKAGE_ASSET_BASENAME)-$safeTag-$($Metadata.WENZWORK_PACKAGE_PLATFORM)-$($Metadata.WENZWORK_PACKAGE_ARCHITECTURE).tar.gz"
        $releasePath = 'download/' + [Uri]::EscapeDataString($tag)
    }
    $checksumsFile = Join-Path $TemporaryDirectory ([string]$Metadata.WENZWORK_PACKAGE_CHECKSUM_ASSET)
    $checksumsUrl = "$GitHubWebBaseUrl/$Repository/releases/$releasePath/$($Metadata.WENZWORK_PACKAGE_CHECKSUM_ASSET)"
    if (-not (Invoke-PackageSourceDownload -Uri $checksumsUrl -Destination $checksumsFile -DownloadCommand $DownloadCommand)) {
        return $null
    }
    try {
        $archiveName = Get-PackageChecksumArchiveName -Path $checksumsFile -Metadata $Metadata -ExpectedName $expectedName
        if ([string]::IsNullOrWhiteSpace($archiveName)) { return $null }
        $packageFile = Join-Path $TemporaryDirectory $archiveName
        $archiveUrl = "$GitHubWebBaseUrl/$Repository/releases/$releasePath/$archiveName"
        if (-not (Invoke-PackageSourceDownload -Uri $archiveUrl -Destination $packageFile -DownloadCommand $DownloadCommand)) {
            return $null
        }
        Write-PackageLog -Message "Downloaded $archiveName from the public GitHub Release page."
        return [pscustomobject]@{ PackageFile = $packageFile; ChecksumsFile = $checksumsFile; Source = 'github-public' }
    }
    catch {
        return $null
    }
}

function Resolve-PackageUpgradeDownload {
    param(
        [Parameter(Mandatory = $true)][Collections.IDictionary]$Metadata,
        [Parameter(Mandatory = $true)][Collections.IDictionary]$Environment,
        [AllowEmptyString()][string]$Version = '',
        [Parameter(Mandatory = $true)][string]$TemporaryDirectory,
        [AllowNull()][scriptblock]$DownloadCommand,
        [string]$GitHubApiBaseUrl = 'https://api.github.com',
        [string]$GitHubWebBaseUrl = 'https://github.com'
    )
    if (-not [string]::IsNullOrWhiteSpace($Version) -and $Version -notmatch '^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$') {
        throw "Invalid release tag: $Version"
    }
    $repository = Get-PackageSettingValue -Values $Environment -Name 'GITHUB_RELEASE_REPOSITORY'
    if ([string]::IsNullOrWhiteSpace($repository)) { $repository = [string]$Metadata.WENZWORK_GITHUB_REPOSITORY }
    if ($repository -notmatch '^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$') {
        throw 'GITHUB_RELEASE_REPOSITORY must use owner/repository format.'
    }
    $token = Get-PackageSettingValue -Values $Environment -Name 'GITHUB_ACCESS_TOKEN'
    $officialOverride = Get-PackageSettingValue -Values $Environment -Name 'WENZWORK_OFFICIAL_RELEASE_BASE_URL'
    if ([string]::IsNullOrWhiteSpace($officialOverride)) {
        $officialSources = @('https://work.wenzflow.com', 'https://wenzwork.com')
        $missingSources = 'work.wenzflow.com, wenzwork.com, or github.com'
    }
    else {
        $officialSources = @($officialOverride.TrimEnd('/'))
        $missingSources = "$($officialSources[0]) or github.com"
    }

    foreach ($officialSource in $officialSources) {
        Write-PackageLog -Message "Trying release source $officialSource."
        $download = Get-PackageOfficialReleaseDownload -BaseUrl $officialSource -Metadata $Metadata -Version $Version `
            -TemporaryDirectory $TemporaryDirectory -DownloadCommand $DownloadCommand
        if ($null -ne $download) { return $download }
        Write-PackageLog -Message "No matching upgrade package is available from $officialSource; trying the next source."
    }

    Write-PackageLog -Message 'Trying release source github.com.'
    if (-not [string]::IsNullOrWhiteSpace($token)) {
        $download = Get-PackageGitHubApiReleaseDownload -Repository $repository -Token $token -Metadata $Metadata `
            -Version $Version -TemporaryDirectory $TemporaryDirectory -DownloadCommand $DownloadCommand `
            -GitHubApiBaseUrl $GitHubApiBaseUrl
        if ($null -ne $download) { return $download }
        Write-PackageLog -Message 'The authenticated GitHub Release API did not provide a matching package; trying the public Release page.'
    }
    $download = Get-PackagePublicGitHubReleaseDownload -Repository $repository -Metadata $Metadata -Version $Version `
        -TemporaryDirectory $TemporaryDirectory -DownloadCommand $DownloadCommand -GitHubWebBaseUrl $GitHubWebBaseUrl
    if ($null -ne $download) { return $download }

    throw "Upgrade package was not found at $missingSources. Check release availability, network access, and GITHUB_ACCESS_TOKEN for private repositories."
}

Export-ModuleMember -Function *
