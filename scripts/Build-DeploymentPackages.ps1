#Requires -Version 7.0

[CmdletBinding()]
param(
    [string]$Version,
    [ValidateSet('host', 'relay', 'device-agent')][string[]]$Components = @('host', 'relay', 'device-agent'),
    [ValidateSet('linux', 'windows', 'darwin')][string[]]$Platforms = @('linux', 'windows', 'darwin'),
    [ValidateSet('amd64', 'arm64')][string[]]$Architectures = @('amd64', 'arm64'),
    [string]$OutputDirectory,
    [switch]$SkipWebBuild,
    [switch]$AllowDirty,
    [switch]$LocalPush,
    [switch]$KeepStaging
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$scriptDirectory = [IO.Path]::GetFullPath($PSScriptRoot)
$repositoryRoot = [IO.Path]::GetFullPath((Join-Path $scriptDirectory '..'))
$serverDirectory = Join-Path $repositoryRoot 'server'
$webDirectory = Join-Path $repositoryRoot 'web'
$deploymentTemplates = Join-Path $repositoryRoot 'deploy\package'
if ([string]::IsNullOrWhiteSpace($OutputDirectory)) {
    $OutputDirectory = Join-Path $repositoryRoot 'dist'
}
$OutputDirectory = [IO.Path]::GetFullPath($OutputDirectory)

function Write-BuildLog {
    param([Parameter(Mandatory = $true)][string]$Message)
    Write-Host "[deployment-build] $Message"
}

function Assert-LastExitCode {
    param([Parameter(Mandatory = $true)][string]$Operation)
    if ($LASTEXITCODE -ne 0) { throw "$Operation failed with exit code $LASTEXITCODE." }
}

function Write-Utf8File {
    param(
        [Parameter(Mandatory = $true)][string]$Path,
        [Parameter(Mandatory = $true)][AllowEmptyString()][string]$Contents
    )
    [IO.File]::WriteAllText($Path, $Contents, [Text.UTF8Encoding]::new($false))
}

function Copy-DirectoryContents {
    param(
        [Parameter(Mandatory = $true)][string]$Source,
        [Parameter(Mandatory = $true)][string]$Destination
    )
    if (-not (Test-Path -LiteralPath $Source -PathType Container)) {
        throw "Source directory is missing: $Source"
    }
    [void](New-Item -ItemType Directory -Path $Destination -Force)
    Get-ChildItem -Force -LiteralPath $Source | ForEach-Object {
        Copy-Item -LiteralPath $_.FullName -Destination (Join-Path $Destination $_.Name) -Recurse -Force
    }
}

function Set-DotEnvValue {
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
    Write-Utf8File -Path $Path -Contents (($lines -join [Environment]::NewLine) + [Environment]::NewLine)
}

function Get-RepositoryName {
    $remote = (& git -C $repositoryRoot remote get-url origin).Trim()
    Assert-LastExitCode -Operation 'Read git origin'
    if ($remote -notmatch 'github\.com[/:]([^/]+)/([^/]+)$') {
        throw "Could not derive GitHub repository from origin: $remote"
    }
    $repositoryName = $Matches[2] -replace '\.git$', ''
    return "$($Matches[1])/$repositoryName"
}

function Invoke-GoBuild {
    param(
        [Parameter(Mandatory = $true)][string]$Command,
        [Parameter(Mandatory = $true)][string]$Platform,
        [Parameter(Mandatory = $true)][string]$Architecture,
        [Parameter(Mandatory = $true)][string]$Destination,
        [switch]$EmbedVersion
    )
    [void](New-Item -ItemType Directory -Path (Split-Path -Parent $Destination) -Force)
    $previous = @{
        CGO_ENABLED = [Environment]::GetEnvironmentVariable('CGO_ENABLED', 'Process')
        GOOS = [Environment]::GetEnvironmentVariable('GOOS', 'Process')
        GOARCH = [Environment]::GetEnvironmentVariable('GOARCH', 'Process')
        GOTOOLCHAIN = [Environment]::GetEnvironmentVariable('GOTOOLCHAIN', 'Process')
    }
    try {
        [Environment]::SetEnvironmentVariable('CGO_ENABLED', '0', 'Process')
        [Environment]::SetEnvironmentVariable('GOOS', $Platform, 'Process')
        [Environment]::SetEnvironmentVariable('GOARCH', $Architecture, 'Process')
        [Environment]::SetEnvironmentVariable('GOTOOLCHAIN', 'auto', 'Process')
        $linkerFlags = '-s -w -buildid='
        if ($EmbedVersion) { $linkerFlags += " -X main.version=$Version" }
        Write-BuildLog -Message "Building $Command for $Platform/$Architecture..."
        & go -C $serverDirectory build -buildvcs=false -trimpath "-ldflags=$linkerFlags" -o $Destination "./cmd/$Command"
        Assert-LastExitCode -Operation "Build $Command for $Platform/$Architecture"
        if (-not (Test-Path -LiteralPath $Destination -PathType Leaf) -or (Get-Item -LiteralPath $Destination).Length -eq 0) {
            throw "Go build did not create $Destination."
        }
    }
    finally {
        foreach ($name in $previous.Keys) {
            [Environment]::SetEnvironmentVariable($name, $previous[$name], 'Process')
        }
    }
}

function Get-BinaryCachePath {
    param([string]$Command, [string]$Platform, [string]$Architecture)
    $extension = if ($Platform -eq 'windows') { '.exe' } else { '' }
    return Join-Path $stagingRoot "binaries\$Platform-$Architecture\$Command$extension"
}

function Copy-BinaryToPackage {
    param(
        [string]$Command,
        [string]$PackageName,
        [string]$Platform,
        [string]$Architecture,
        [string]$PackageRoot
    )
    $extension = if ($Platform -eq 'windows') { '.exe' } else { '' }
    Copy-Item -LiteralPath (Get-BinaryCachePath -Command $Command -Platform $Platform -Architecture $Architecture) -Destination (Join-Path $PackageRoot "bin\$PackageName$extension") -Force
}

function Add-PortableLifecycle {
    param([string]$Component, [string]$Platform, [string]$PackageRoot)
    foreach ($name in @('start.sh', 'stop.sh', 'upgrade.sh')) {
        Copy-Item -LiteralPath (Join-Path $deploymentTemplates "unix\$name") -Destination (Join-Path $PackageRoot $name) -Force
    }
    if ($Component -eq 'host') {
        Copy-Item -LiteralPath (Join-Path $deploymentTemplates 'unix\backup.sh') -Destination (Join-Path $PackageRoot 'backup.sh') -Force
    }
    [void](New-Item -ItemType Directory -Path (Join-Path $PackageRoot 'runtime\lib') -Force)
    Copy-Item -LiteralPath (Join-Path $deploymentTemplates 'unix\lib\common.sh') -Destination (Join-Path $PackageRoot 'runtime\lib\common.sh') -Force
    if ($Platform -eq 'windows') {
        foreach ($name in @('Init.ps1', 'Start.ps1', 'Stop.ps1', 'Upgrade.ps1')) {
            Copy-Item -LiteralPath (Join-Path $deploymentTemplates "windows\$name") -Destination (Join-Path $PackageRoot $name) -Force
        }
        if ($Component -eq 'device-agent') {
            Copy-Item -LiteralPath (Join-Path $deploymentTemplates 'windows\start.cmd') -Destination (Join-Path $PackageRoot 'start.cmd') -Force
        }
        if ($Component -eq 'host') {
            Copy-Item -LiteralPath (Join-Path $deploymentTemplates 'windows\Backup.ps1') -Destination (Join-Path $PackageRoot 'Backup.ps1') -Force
        }
        Copy-Item -LiteralPath (Join-Path $deploymentTemplates 'windows\PackageCommon.psm1') -Destination (Join-Path $PackageRoot 'runtime\lib\PackageCommon.psm1') -Force
    }
}

function Add-ComponentConfiguration {
    param(
        [string]$Component,
        [string]$Platform,
        [string]$Architecture,
        [string]$PackageRoot,
        [string]$Repository
    )
    switch ($Component) {
        'host' {
            Copy-Item -LiteralPath (Join-Path $repositoryRoot '.env.example') -Destination (Join-Path $PackageRoot 'config\host.env.example') -Force
            Copy-DirectoryContents -Source (Join-Path $serverDirectory 'migrations') -Destination (Join-Path $PackageRoot 'migrations')
            Copy-DirectoryContents -Source (Join-Path $webDirectory 'dist') -Destination (Join-Path $PackageRoot 'web')
            Copy-Item -LiteralPath (Join-Path $repositoryRoot 'deploy\Caddyfile') -Destination (Join-Path $PackageRoot 'config\Caddyfile') -Force
            $bootstrapRoot = Join-Path $PackageRoot 'config\relay-bootstrap'
            [void](New-Item -ItemType Directory -Path (Join-Path $bootstrapRoot 'lib') -Force)
            Copy-Item -LiteralPath (Join-Path $repositoryRoot 'deploy\relay\install.sh') -Destination (Join-Path $bootstrapRoot 'install.sh') -Force
            Copy-Item -LiteralPath (Join-Path $repositoryRoot 'deploy\relay\upgrade.sh') -Destination (Join-Path $bootstrapRoot 'upgrade.sh') -Force
            Copy-Item -LiteralPath (Join-Path $repositoryRoot 'deploy\relay\lib\common.sh') -Destination (Join-Path $bootstrapRoot 'lib\common.sh') -Force
            Copy-Item -LiteralPath (Join-Path $repositoryRoot 'deploy\relay\release-signing-public-key.pem') -Destination (Join-Path $bootstrapRoot 'release-signing-public-key.pem') -Force
            Copy-Item -LiteralPath (Join-Path $repositoryRoot 'deploy\relay\TEST_ONLY_SIGNING_KEY') -Destination (Join-Path $bootstrapRoot 'TEST_ONLY_SIGNING_KEY') -Force
            [void](New-Item -ItemType Directory -Path (Join-Path $bootstrapRoot 'windows\lib') -Force)
            Copy-Item -LiteralPath (Join-Path $repositoryRoot 'deploy\relay\windows\Install.ps1') -Destination (Join-Path $bootstrapRoot 'windows\Install.ps1') -Force
            Copy-Item -LiteralPath (Join-Path $repositoryRoot 'deploy\relay\windows\Upgrade.ps1') -Destination (Join-Path $bootstrapRoot 'windows\Upgrade.ps1') -Force
            Copy-Item -LiteralPath (Join-Path $repositoryRoot 'deploy\relay\windows\lib\RelayCommon.psm1') -Destination (Join-Path $bootstrapRoot 'windows\lib\RelayCommon.psm1') -Force
            foreach ($bootstrapArchitecture in @('amd64', 'arm64')) {
                Copy-Item -LiteralPath (Get-BinaryCachePath -Command relayctl -Platform windows -Architecture $bootstrapArchitecture) -Destination (Join-Path $bootstrapRoot "windows\relayctl-$bootstrapArchitecture.exe") -Force
            }
            [void](New-Item -ItemType Directory -Path (Join-Path $bootstrapRoot 'darwin\lib') -Force)
            Copy-Item -LiteralPath (Join-Path $repositoryRoot 'deploy\relay\darwin\install.sh') -Destination (Join-Path $bootstrapRoot 'darwin\install.sh') -Force
            Copy-Item -LiteralPath (Join-Path $repositoryRoot 'deploy\relay\darwin\upgrade.sh') -Destination (Join-Path $bootstrapRoot 'darwin\upgrade.sh') -Force
            Copy-Item -LiteralPath (Join-Path $repositoryRoot 'deploy\relay\darwin\lib\common.sh') -Destination (Join-Path $bootstrapRoot 'darwin\lib\common.sh') -Force
            foreach ($bootstrapArchitecture in @('amd64', 'arm64')) {
                Copy-Item -LiteralPath (Get-BinaryCachePath -Command relayctl -Platform darwin -Architecture $bootstrapArchitecture) -Destination (Join-Path $bootstrapRoot "darwin\relayctl-$bootstrapArchitecture") -Force
            }
        }
        'relay' {
            $environmentTemplate = Join-Path $PackageRoot 'config\relay.env.example'
            Copy-Item -LiteralPath (Join-Path $repositoryRoot 'deploy\relay\relay.env.example') -Destination $environmentTemplate -Force
            Copy-Item -LiteralPath (Join-Path $repositoryRoot 'deploy\relay\config.example.yaml') -Destination (Join-Path $PackageRoot 'config\config.example.yaml') -Force
            Set-DotEnvValue -Path $environmentTemplate -Name 'RELAY_VERSION' -Value $Version
            Set-DotEnvValue -Path $environmentTemplate -Name 'GITHUB_RELEASE_REPOSITORY' -Value $Repository
            Set-DotEnvValue -Path $environmentTemplate -Name 'GITHUB_ACCESS_TOKEN' -Value ''
            if ($Platform -eq 'linux') {
                [void](New-Item -ItemType Directory -Path (Join-Path $PackageRoot 'config\systemd') -Force)
                Copy-Item -LiteralPath (Join-Path $repositoryRoot 'deploy\relay\systemd\wenzwork-relay.service') -Destination (Join-Path $PackageRoot 'config\systemd\wenzwork-relay.service') -Force
            }
            elseif ($Platform -eq 'darwin') {
                [void](New-Item -ItemType Directory -Path (Join-Path $PackageRoot 'config\launchd') -Force)
                Copy-Item -LiteralPath (Join-Path $repositoryRoot 'deploy\relay\darwin\launchd\com.wenzwork.relay.plist') -Destination (Join-Path $PackageRoot 'config\launchd\com.wenzwork.relay.plist') -Force
            }
        }
        'device-agent' {
            $environmentContents = @(
                '# WenzWork Device Agent portable deployment configuration.'
                'WENZWORK_CONTROL_URL=https://wenzwork.com'
                'WENZWORK_DEVICE_ACCESS_KEY=device_replace_with_a_43_character_urlsafe_access_key'
                'WENZWORK_DEVICE_STATE_FILE=./runtime/state/agent-state.json'
                'WENZWORK_DEVICE_WORKSPACE=./workspace'
                'WENZWORK_AGENT_SECRET_STORE=file'
				'# Replace the IP with a reachable device address before enabling direct mode.'
				'WENZWORK_DEVICE_DIRECT_ENABLED=false'
				'WENZWORK_DEVICE_DIRECT_IP=127.0.0.1'
				'WENZWORK_DEVICE_DIRECT_PORT=9443'
				'# WENZWORK_DEVICE_DIRECT_ACCESS_KEY=device_replace_with_a_43_character_urlsafe_access_key'
				'# WENZWORK_DEVICE_DIRECT_TLS_CERT_FILE=./config/direct-cert.pem'
				'# WENZWORK_DEVICE_DIRECT_TLS_KEY_FILE=./config/direct-key.pem'
                '# WENZWORK_AGENT_FEATURE_FLAGS=-terminal.interactive,-tasks.v2,-ai.tools'
                '# WENZWORK_DEVICE_TLS_CA_FILE=./config/control-ca.pem'
                "GITHUB_RELEASE_REPOSITORY=$Repository"
                'GITHUB_ACCESS_TOKEN='
                ''
            ) -join [Environment]::NewLine
            Write-Utf8File -Path (Join-Path $PackageRoot 'config\device-agent.env.example') -Contents $environmentContents
            if ($Platform -eq 'linux') {
                [void](New-Item -ItemType Directory -Path (Join-Path $PackageRoot 'config\systemd') -Force)
                Copy-Item -LiteralPath (Join-Path $repositoryRoot 'deploy\device-agent\systemd\wenzwork-device-agent.service') -Destination (Join-Path $PackageRoot 'config\systemd\wenzwork-device-agent.service') -Force
            }
            elseif ($Platform -eq 'darwin') {
                [void](New-Item -ItemType Directory -Path (Join-Path $PackageRoot 'config\launchd') -Force)
                Copy-Item -LiteralPath (Join-Path $repositoryRoot 'deploy\device-agent\darwin\launchd\com.wenzwork.device-agent.plist') -Destination (Join-Path $PackageRoot 'config\launchd\com.wenzwork.device-agent.plist') -Force
            }
        }
    }
}

function New-PackageManifest {
    param([string]$Component, [string]$Platform, [string]$Architecture, [string]$PackageRoot)
    $files = @(
        Get-ChildItem -LiteralPath $PackageRoot -Recurse -File |
            Where-Object { $_.Name -ne 'PACKAGE-MANIFEST.json' } |
            ForEach-Object {
                $relative = [IO.Path]::GetRelativePath($PackageRoot, $_.FullName).Replace('\', '/')
                [ordered]@{
                    path = $relative
                    sha256 = (Get-FileHash -LiteralPath $_.FullName -Algorithm SHA256).Hash.ToLowerInvariant()
                    size = $_.Length
                }
            } |
            Sort-Object path
    )
    $manifest = [ordered]@{
        schemaVersion = 1
        component = $Component
        version = $Version
        platform = $Platform
        architecture = $Architecture
        commit = $commit
        dirty = $isDirty
        builtAtUtc = $builtAtUtc
        files = $files
    }
    Write-Utf8File -Path (Join-Path $PackageRoot 'PACKAGE-MANIFEST.json') -Contents ($manifest | ConvertTo-Json -Depth 8)
}

if (-not (Test-Path -LiteralPath (Join-Path $serverDirectory 'go.mod') -PathType Leaf)) { throw "Repository root is invalid: $repositoryRoot" }
foreach ($command in @('git', 'go', 'tar')) {
    if (-not (Get-Command $command -ErrorAction SilentlyContinue)) { throw "Required command is missing: $command" }
}
if ($Components -contains 'host' -and -not (Get-Command corepack -ErrorAction SilentlyContinue)) {
    throw 'Required command is missing: corepack'
}
if ([string]::IsNullOrWhiteSpace($Version)) {
    $packageMetadata = Get-Content -Raw -LiteralPath (Join-Path $repositoryRoot 'package.json') | ConvertFrom-Json
    $Version = "v$($packageMetadata.version)"
}
if ($Version -notmatch '^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$') { throw "Invalid release version: $Version" }
$safeVersion = $Version -replace '[^A-Za-z0-9._-]', '-'
if ($LocalPush) {
    # Local push releases are mutable by design and are keyed by the explicit
    # version supplied by the operator. Mark their source revision as
    # unverified so these artifacts cannot accidentally enter the immutable
    # GitHub Release workflow, which rejects dirty manifests.
    $commit = '0000000000000000000000000000000000000000'
    $builtAtUtc = [DateTimeOffset]::UtcNow.UtcDateTime.ToString('o')
    $isDirty = $true
    Write-BuildLog -Message 'Local push mode: skipped git commit and worktree checks.'
}
else {
    $commit = (& git -C $repositoryRoot rev-parse HEAD).Trim()
    Assert-LastExitCode -Operation 'Read git commit'
    $commitTimestamp = (& git -C $repositoryRoot show -s --format=%cI $commit).Trim()
    Assert-LastExitCode -Operation 'Read git commit timestamp'
    $builtAtUtc = [DateTimeOffset]::ParseExact(
        $commitTimestamp,
        'yyyy-MM-ddTHH:mm:ssK',
        [Globalization.CultureInfo]::InvariantCulture
    ).UtcDateTime.ToString('o')
    $trackedChanges = @(& git -C $repositoryRoot status --porcelain --untracked-files=normal)
    Assert-LastExitCode -Operation 'Inspect worktree'
    $isDirty = $trackedChanges.Count -gt 0
    if ($isDirty -and -not $AllowDirty) {
        throw 'Tracked worktree changes would make the packages unreproducible. Commit them or pass -AllowDirty for a non-release build.'
    }
}
$repository = Get-RepositoryName
[void](New-Item -ItemType Directory -Path $OutputDirectory -Force)
foreach ($metadataName in @('DEPLOYMENT-SHA256SUMS', 'DEPLOYMENT-RELEASE-MANIFEST.json')) {
    Remove-Item -LiteralPath (Join-Path $OutputDirectory $metadataName) -Force -ErrorAction SilentlyContinue
}
# Remove artifacts written by the pre-deployment naming scheme. The anchored
# pattern cannot match unrelated files in dist.
$legacyPattern = '^wenzwork-(?:host|relay|device-agent)-' + [regex]::Escape($safeVersion) + '-(?:linux|windows|darwin)-(?:amd64|arm64)\.tar\.gz$'
Get-ChildItem -LiteralPath $OutputDirectory -File | Where-Object { $_.Name -match $legacyPattern } | ForEach-Object {
    Remove-Item -LiteralPath $_.FullName -Force
}
foreach ($legacyMetadata in @('SHA256SUMS', 'RELEASE-MANIFEST.json')) {
    Remove-Item -LiteralPath (Join-Path $OutputDirectory $legacyMetadata) -Force -ErrorAction SilentlyContinue
}
$stagingRoot = Join-Path $OutputDirectory '.deployment-staging'
$outputPrefix = [IO.Path]::GetFullPath($OutputDirectory).TrimEnd('\') + '\'
$resolvedStaging = [IO.Path]::GetFullPath($stagingRoot)
if (-not $resolvedStaging.StartsWith($outputPrefix, [StringComparison]::OrdinalIgnoreCase)) { throw "Unsafe staging directory: $resolvedStaging" }
Remove-Item -LiteralPath $resolvedStaging -Recurse -Force -ErrorAction SilentlyContinue
[void](New-Item -ItemType Directory -Path $resolvedStaging)
$archiveToolDirectory = Join-Path $resolvedStaging 'tools'
[void](New-Item -ItemType Directory -Path $archiveToolDirectory)
$archiveTool = Join-Path $archiveToolDirectory $(if ($IsWindows) { 'deployment-archive.exe' } else { 'deployment-archive' })
& go build -trimpath -o $archiveTool (Join-Path $scriptDirectory 'deployment_archive.go')
Assert-LastExitCode -Operation 'Build deployment archive tool'

try {
    if ($Components -contains 'host' -and -not $SkipWebBuild) {
        Write-BuildLog -Message 'Building Web application...'
        & corepack pnpm --dir $webDirectory build
        Assert-LastExitCode -Operation 'Build Web application'
    }
    if ($Components -contains 'host' -and -not (Test-Path -LiteralPath (Join-Path $webDirectory 'dist') -PathType Container)) {
        throw 'web/dist is missing; run without -SkipWebBuild or build Web first.'
    }

    foreach ($platform in $Platforms) {
        foreach ($architecture in $Architectures) {
            $commands = [Collections.Generic.HashSet[string]]::new([StringComparer]::Ordinal)
            if ($Components -contains 'host') {
                foreach ($command in @('api', 'admin', 'migrate')) { [void]$commands.Add($command) }
            }
            if ($Components -contains 'relay') {
                foreach ($command in @('relay-server', 'relayctl')) { [void]$commands.Add($command) }
            }
            if ($Components -contains 'device-agent') {
                foreach ($command in @('device-agent', 'relayctl')) { [void]$commands.Add($command) }
            }
            foreach ($command in $commands) {
                $embedVersion = $command -in @('relay-server', 'relayctl', 'device-agent')
                $destination = Get-BinaryCachePath -Command $command -Platform $platform -Architecture $architecture
                Invoke-GoBuild -Command $command -Platform $platform -Architecture $architecture -Destination $destination -EmbedVersion:$embedVersion
            }
        }
    }
    if ($Components -contains 'host') {
        # Every Host serves bootstrap verifiers for remote Windows and macOS
        # Relay nodes, regardless of the Host package's own target.
        foreach ($bootstrapPlatform in @('windows', 'darwin')) {
            foreach ($bootstrapArchitecture in @('amd64', 'arm64')) {
                $destination = Get-BinaryCachePath -Command relayctl -Platform $bootstrapPlatform -Architecture $bootstrapArchitecture
                if (-not (Test-Path -LiteralPath $destination -PathType Leaf)) {
                    Invoke-GoBuild -Command relayctl -Platform $bootstrapPlatform -Architecture $bootstrapArchitecture -Destination $destination -EmbedVersion
                }
            }
        }
    }

    $assets = [Collections.Generic.List[object]]::new()
    foreach ($component in $Components) {
        $assetBaseName = "wenzwork-$component-deployment"
        foreach ($platform in $Platforms) {
            foreach ($architecture in $Architectures) {
                $archiveName = "$assetBaseName-$safeVersion-$platform-$architecture.tar.gz"
                $packageRoot = Join-Path $stagingRoot "packages\$component-$platform-$architecture"
                foreach ($directory in @('bin', 'config', 'runtime', 'logs', 'workspace', 'cache')) {
                    [void](New-Item -ItemType Directory -Path (Join-Path $packageRoot $directory) -Force)
                }
                Add-PortableLifecycle -Component $component -Platform $platform -PackageRoot $packageRoot
                Add-ComponentConfiguration -Component $component -Platform $platform -Architecture $architecture -PackageRoot $packageRoot -Repository $repository
                switch ($component) {
                    'host' {
                        Copy-BinaryToPackage -Command api -PackageName wenzwork-api -Platform $platform -Architecture $architecture -PackageRoot $packageRoot
                        Copy-BinaryToPackage -Command admin -PackageName wenzwork-admin -Platform $platform -Architecture $architecture -PackageRoot $packageRoot
                        Copy-BinaryToPackage -Command migrate -PackageName wenzwork-migrate -Platform $platform -Architecture $architecture -PackageRoot $packageRoot
                    }
                    'relay' {
                        Copy-BinaryToPackage -Command relay-server -PackageName wenzwork-relay-server -Platform $platform -Architecture $architecture -PackageRoot $packageRoot
                        Copy-BinaryToPackage -Command relayctl -PackageName relayctl -Platform $platform -Architecture $architecture -PackageRoot $packageRoot
                    }
                    'device-agent' {
                        Copy-BinaryToPackage -Command device-agent -PackageName wenzwork-device-agent -Platform $platform -Architecture $architecture -PackageRoot $packageRoot
                        Copy-BinaryToPackage -Command relayctl -PackageName relayctl -Platform $platform -Architecture $architecture -PackageRoot $packageRoot
                    }
                }
                Write-Utf8File -Path (Join-Path $packageRoot 'VERSION') -Contents "$Version$([Environment]::NewLine)"
                $packageEnvironment = @(
                    "WENZWORK_PACKAGE_COMPONENT=$component"
                    "WENZWORK_PACKAGE_PLATFORM=$platform"
                    "WENZWORK_PACKAGE_ARCHITECTURE=$architecture"
                    "WENZWORK_PACKAGE_VERSION=$Version"
                    "WENZWORK_PACKAGE_ASSET_BASENAME=$assetBaseName"
                    'WENZWORK_PACKAGE_CHECKSUM_ASSET=DEPLOYMENT-SHA256SUMS'
                    "WENZWORK_GITHUB_REPOSITORY=$repository"
                    ''
                ) -join [Environment]::NewLine
                Write-Utf8File -Path (Join-Path $packageRoot 'config\package.env') -Contents $packageEnvironment
                New-PackageManifest -Component $component -Platform $platform -Architecture $architecture -PackageRoot $packageRoot

                $archivePath = Join-Path $OutputDirectory $archiveName
                Remove-Item -LiteralPath $archivePath -Force -ErrorAction SilentlyContinue
                Write-BuildLog -Message "Packing $archiveName..."
                & $archiveTool create $packageRoot $archivePath
                Assert-LastExitCode -Operation "Pack $archiveName with explicit file modes"
                & $archiveTool verify - $archivePath
                Assert-LastExitCode -Operation "Verify file modes in $archiveName"
                $archiveInfo = Get-Item -LiteralPath $archivePath
                $assets.Add([ordered]@{
                    name = $archiveName
                    component = $component
                    platform = $platform
                    architecture = $architecture
                    size = $archiveInfo.Length
                    sha256 = (Get-FileHash -LiteralPath $archivePath -Algorithm SHA256).Hash.ToLowerInvariant()
                })
            }
        }
    }

    $checksumLines = @($assets | Sort-Object name | ForEach-Object { "$($_.sha256)  $($_.name)" })
    Write-Utf8File -Path (Join-Path $OutputDirectory 'DEPLOYMENT-SHA256SUMS') -Contents (($checksumLines -join [Environment]::NewLine) + [Environment]::NewLine)
    $releaseManifest = [ordered]@{
        schemaVersion = 1
        version = $Version
        repository = $repository
        commit = $commit
        dirty = $isDirty
        builtAtUtc = $builtAtUtc
        packageCount = $assets.Count
        packages = @($assets | Sort-Object name)
    }
    Write-Utf8File -Path (Join-Path $OutputDirectory 'DEPLOYMENT-RELEASE-MANIFEST.json') -Contents ($releaseManifest | ConvertTo-Json -Depth 8)

    & (Join-Path $scriptDirectory 'Test-DeploymentPackages.ps1') -OutputDirectory $OutputDirectory -Version $Version
    Assert-LastExitCode -Operation 'Verify deployment packages'
    Write-BuildLog -Message "Created and verified $($assets.Count) deployment packages in $OutputDirectory."
}
finally {
    if (-not $KeepStaging) {
        Remove-Item -LiteralPath $resolvedStaging -Recurse -Force -ErrorAction SilentlyContinue
    }
}
