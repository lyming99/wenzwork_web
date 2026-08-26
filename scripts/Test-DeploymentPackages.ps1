#Requires -Version 7.0

[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$OutputDirectory,
    [string]$Version
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$scriptDirectory = [IO.Path]::GetFullPath($PSScriptRoot)
$OutputDirectory = [IO.Path]::GetFullPath($OutputDirectory)

function Write-VerifyLog {
    param([string]$Message)
    Write-Host "[deployment-verify] $Message"
}

function Read-PackageMetadata {
    param([string]$Path)
    $values = @{}
    foreach ($lineValue in [IO.File]::ReadAllLines($Path)) {
        $line = $lineValue.Trim()
        if ($line.Length -eq 0 -or $line.StartsWith('#')) { continue }
        if ($line -notmatch '^([A-Za-z_][A-Za-z0-9_]*)=(.*)$') { throw "Invalid package metadata in $Path." }
        $values[$Matches[1]] = $Matches[2].Trim()
    }
    return $values
}

function Assert-PowerShellSyntax {
    param([string]$Path)
    $tokens = $null
    $errors = $null
    [void][Management.Automation.Language.Parser]::ParseFile($Path, [ref]$tokens, [ref]$errors)
    if ($errors.Count -gt 0) {
        throw ('PowerShell syntax error in {0}: {1}' -f $Path, $errors[0].Message)
    }
}

$releaseManifestPath = Join-Path $OutputDirectory 'DEPLOYMENT-RELEASE-MANIFEST.json'
$checksumsPath = Join-Path $OutputDirectory 'DEPLOYMENT-SHA256SUMS'
if (-not (Test-Path -LiteralPath $releaseManifestPath -PathType Leaf)) { throw 'DEPLOYMENT-RELEASE-MANIFEST.json is missing.' }
if (-not (Test-Path -LiteralPath $checksumsPath -PathType Leaf)) { throw 'DEPLOYMENT-SHA256SUMS is missing.' }
$release = Get-Content -Raw -LiteralPath $releaseManifestPath | ConvertFrom-Json
if ($release.schemaVersion -ne 1) { throw 'Unsupported release manifest schema.' }
if (-not [string]::IsNullOrWhiteSpace($Version) -and $release.version -cne $Version) {
    throw "Release manifest version is $($release.version), expected $Version."
}
$packages = @($release.packages)
if ($packages.Count -eq 0 -or $release.packageCount -ne $packages.Count) { throw 'Release package count is invalid.' }
if (@($packages.name | Sort-Object -Unique).Count -ne $packages.Count) { throw 'Release manifest has duplicate package names.' }

$checksumEntries = @{}
foreach ($line in [IO.File]::ReadAllLines($checksumsPath)) {
    if ($line -notmatch '^([0-9a-f]{64})  ([^\\/]+\.tar\.gz)$') { throw "Invalid SHA256SUMS line: $line" }
    if ($checksumEntries.ContainsKey($Matches[2])) { throw "Duplicate SHA256SUMS entry: $($Matches[2])" }
    $checksumEntries[$Matches[2]] = $Matches[1]
}
if ($checksumEntries.Count -ne $packages.Count) { throw 'SHA256SUMS package count does not match the release manifest.' }

$temporary = Join-Path $OutputDirectory ".deployment-verification.$([Guid]::NewGuid().ToString('N'))"
$outputPrefix = $OutputDirectory.TrimEnd('\') + '\'
$resolvedTemporary = [IO.Path]::GetFullPath($temporary)
if (-not $resolvedTemporary.StartsWith($outputPrefix, [StringComparison]::OrdinalIgnoreCase)) {
    throw "Unsafe verification directory: $resolvedTemporary"
}
[void](New-Item -ItemType Directory -Path $resolvedTemporary)
try {
    $archiveTool = Join-Path $resolvedTemporary $(if ($IsWindows) { 'deployment-archive.exe' } else { 'deployment-archive' })
    & go build -trimpath -o $archiveTool (Join-Path $scriptDirectory 'deployment_archive.go')
    if ($LASTEXITCODE -ne 0) { throw 'Could not build the deployment archive verifier.' }
    foreach ($package in $packages) {
        $archivePath = Join-Path $OutputDirectory $package.name
        if (-not (Test-Path -LiteralPath $archivePath -PathType Leaf)) { throw "Package is missing: $($package.name)" }
        $archiveInfo = Get-Item -LiteralPath $archivePath
        $actualHash = (Get-FileHash -LiteralPath $archivePath -Algorithm SHA256).Hash.ToLowerInvariant()
        if ($actualHash -cne $package.sha256 -or $actualHash -cne $checksumEntries[$package.name]) {
            throw "Archive hash mismatch: $($package.name)"
        }
        if ($archiveInfo.Length -ne $package.size -or $archiveInfo.Length -eq 0) {
            throw "Archive size mismatch: $($package.name)"
        }
        & $archiveTool verify - $archivePath
        if ($LASTEXITCODE -ne 0) { throw "Archive file-mode verification failed: $($package.name)" }

        $entries = @(& tar -tzf $archivePath)
        if ($LASTEXITCODE -ne 0 -or $entries.Count -eq 0) { throw "Could not list $($package.name)." }
        $normalizedEntries = @($entries | ForEach-Object { ([string]$_).Replace('\', '/') -replace '^[.]/', '' })
        if (@($normalizedEntries | Sort-Object -Unique).Count -ne $normalizedEntries.Count) {
            throw "Archive contains duplicate paths: $($package.name)"
        }
        foreach ($entry in $normalizedEntries) {
            if ($entry.StartsWith('/') -or $entry -match '(^|/)[.]?\.(/|$)') { throw "Unsafe entry in $($package.name): $entry" }
        }
        $verboseEntries = @(& tar -tvzf $archivePath)
        if ($LASTEXITCODE -ne 0) { throw "Could not inspect archive types in $($package.name)." }
        foreach ($line in $verboseEntries) {
            if (-not [string]::IsNullOrEmpty($line) -and $line.Substring(0, 1) -notin @('-', 'd')) {
                throw "Archive contains a link or special file: $($package.name)"
            }
        }
        foreach ($required in @('bin/', 'config/', 'runtime/', 'logs/', 'workspace/', 'cache/', 'start.sh', 'upgrade.sh', 'VERSION', 'PACKAGE-MANIFEST.json')) {
            if (-not @($normalizedEntries | Where-Object { $_ -ceq $required -or $_.StartsWith($required) }).Count) {
                throw "$($package.name) is missing $required."
            }
        }
        if ($normalizedEntries -ccontains '.env') {
            throw "$($package.name) must not contain a root .env file."
        }
        if ($normalizedEntries -contains 'init.sh') {
            throw "$($package.name) still contains the removed init.sh entrypoint."
        }

        $extractRoot = Join-Path $resolvedTemporary ([IO.Path]::GetFileNameWithoutExtension([IO.Path]::GetFileNameWithoutExtension($package.name)))
        [void](New-Item -ItemType Directory -Path $extractRoot)
        & tar -xzf $archivePath -C $extractRoot
        if ($LASTEXITCODE -ne 0) { throw "Could not extract $($package.name)." }
        $metadata = Read-PackageMetadata -Path (Join-Path $extractRoot 'config\package.env')
        foreach ($mapping in @{
            WENZWORK_PACKAGE_COMPONENT = [string]$package.component
            WENZWORK_PACKAGE_PLATFORM = [string]$package.platform
            WENZWORK_PACKAGE_ARCHITECTURE = [string]$package.architecture
            WENZWORK_PACKAGE_VERSION = [string]$release.version
            WENZWORK_PACKAGE_CHECKSUM_ASSET = 'DEPLOYMENT-SHA256SUMS'
        }.GetEnumerator()) {
            if (-not $metadata.ContainsKey($mapping.Key) -or $metadata[$mapping.Key] -cne $mapping.Value) {
                throw "$($package.name) metadata mismatch for $($mapping.Key)."
            }
        }
        $expectedName = "$($metadata.WENZWORK_PACKAGE_ASSET_BASENAME)-$($release.version -replace '[^A-Za-z0-9._-]', '-')-$($package.platform)-$($package.architecture).tar.gz"
        if ($package.name -cne $expectedName) { throw "Unexpected package name: $($package.name)" }

        $manifestPath = Join-Path $extractRoot 'PACKAGE-MANIFEST.json'
        $manifest = Get-Content -Raw -LiteralPath $manifestPath | ConvertFrom-Json
        if ($manifest.component -cne $package.component -or $manifest.platform -cne $package.platform -or
            $manifest.architecture -cne $package.architecture -or $manifest.version -cne $release.version -or
            $manifest.commit -cne $release.commit) {
            throw "Inner manifest target mismatch: $($package.name)"
        }
        $manifestFiles = @($manifest.files)
        $actualFiles = @(
            Get-ChildItem -LiteralPath $extractRoot -Recurse -File |
                Where-Object { $_.Name -ne 'PACKAGE-MANIFEST.json' } |
                ForEach-Object { [IO.Path]::GetRelativePath($extractRoot, $_.FullName).Replace('\', '/') } |
                Sort-Object
        )
        $manifestPaths = @($manifestFiles.path | Sort-Object)
        if (Compare-Object -ReferenceObject $actualFiles -DifferenceObject $manifestPaths) {
            throw "Inner manifest file set mismatch: $($package.name)"
        }
        foreach ($file in $manifestFiles) {
            $path = Join-Path $extractRoot ([string]$file.path).Replace('/', '\')
            $hash = (Get-FileHash -LiteralPath $path -Algorithm SHA256).Hash.ToLowerInvariant()
            if ($hash -cne $file.sha256 -or (Get-Item -LiteralPath $path).Length -ne $file.size) {
                throw "Inner file digest mismatch in $($package.name): $($file.path)"
            }
        }

        $extension = if ($package.platform -eq 'windows') { '.exe' } else { '' }
        $requiredBinary = switch ($package.component) {
            'host' { "wenzwork-api$extension" }
            'relay' { "wenzwork-relay-server$extension" }
            'device-agent' { "wenzwork-device-agent$extension" }
        }
        $binaryPath = Join-Path $extractRoot "bin\$requiredBinary"
        if (-not (Test-Path -LiteralPath $binaryPath -PathType Leaf)) { throw "Required binary is missing: $requiredBinary" }
        $goMetadata = (& go version -m $binaryPath 2>&1) -join [Environment]::NewLine
        if ($LASTEXITCODE -ne 0 -or $goMetadata -notmatch "GOOS=$([regex]::Escape($package.platform))" -or
            $goMetadata -notmatch "GOARCH=$([regex]::Escape($package.architecture))" -or $goMetadata -notmatch 'CGO_ENABLED=0') {
            throw "Go target metadata mismatch in $($package.name)."
        }
        if ($IsWindows -and $package.platform -eq 'windows' -and $package.architecture -eq 'amd64' -and
            $package.component -in @('relay', 'device-agent')) {
            $versionBinary = Join-Path $extractRoot 'bin\relayctl.exe'
            $binaryVersion = (& $versionBinary version 2>&1) -join ''
            if ($LASTEXITCODE -ne 0 -or $binaryVersion.Trim() -cne $release.version) {
                throw "Release version was not embedded in relayctl.exe for $($package.name)."
            }
            if ($package.component -eq 'device-agent') {
                $binaryVersion = (& $binaryPath version 2>&1) -join ''
                if ($LASTEXITCODE -ne 0 -or $binaryVersion.Trim() -cne $release.version) {
                    throw "Release version was not embedded in $requiredBinary."
                }
            }
        }
        $unixCommon = [IO.File]::ReadAllText((Join-Path $extractRoot 'runtime\lib\common.sh'))
        if (-not $unixCommon.Contains('package_initialize_environment') -or
            -not $unixCommon.Contains('config/host.env.example') -or
            -not $unixCommon.Contains('config/relay.env.example') -or
            -not $unixCommon.Contains('config/device-agent.env.example')) {
            throw "Unix lifecycle does not create component .env files from packaged templates: $($package.name)"
        }
        switch ($package.component) {
            'host' {
                foreach ($requiredPath in @(
                    "bin/wenzwork-admin$extension",
                    "bin/wenzwork-migrate$extension",
                    'migrations/',
                    'web/',
                    'backup.sh',
                    'config/host.env.example',
                    'config/relay-bootstrap/windows/Install.ps1',
                    'config/relay-bootstrap/windows/relayctl-amd64.exe',
                    'config/relay-bootstrap/windows/relayctl-arm64.exe',
                    'config/relay-bootstrap/darwin/install.sh',
                    'config/relay-bootstrap/darwin/relayctl-amd64',
                    'config/relay-bootstrap/darwin/relayctl-arm64',
                    'config/relay-bootstrap/release-signing-public-key.pem',
                    'config/relay-bootstrap/TEST_ONLY_SIGNING_KEY'
                )) {
                    $localPath = Join-Path $extractRoot $requiredPath.Replace('/', '\').TrimEnd('\')
                    if (-not (Test-Path -LiteralPath $localPath)) { throw "Host package is missing $requiredPath." }
                }
                foreach ($bootstrapVerifier in @(
                    @{ Path = 'config\relay-bootstrap\windows\relayctl-amd64.exe'; Platform = 'windows'; Architecture = 'amd64' },
                    @{ Path = 'config\relay-bootstrap\windows\relayctl-arm64.exe'; Platform = 'windows'; Architecture = 'arm64' },
                    @{ Path = 'config\relay-bootstrap\darwin\relayctl-amd64'; Platform = 'darwin'; Architecture = 'amd64' },
                    @{ Path = 'config\relay-bootstrap\darwin\relayctl-arm64'; Platform = 'darwin'; Architecture = 'arm64' }
                )) {
                    $verifierPath = Join-Path $extractRoot $bootstrapVerifier.Path
                    $metadata = (& go version -m $verifierPath 2>&1) -join [Environment]::NewLine
                    if ($LASTEXITCODE -ne 0 -or
                        $metadata -notmatch "GOOS=$([regex]::Escape($bootstrapVerifier.Platform))" -or
                        $metadata -notmatch "GOARCH=$([regex]::Escape($bootstrapVerifier.Architecture))" -or
                        $metadata -notmatch 'CGO_ENABLED=0') {
                        throw "Relay bootstrap verifier target metadata mismatch in $($package.name): $($bootstrapVerifier.Path)"
                    }
                }
                if ($package.platform -eq 'windows' -and -not (Test-Path -LiteralPath (Join-Path $extractRoot 'Backup.ps1') -PathType Leaf)) {
                    throw "Windows Host package is missing Backup.ps1."
                }
                $unixStartup = [IO.File]::ReadAllText((Join-Path $extractRoot 'start.sh'))
                if (-not $unixStartup.Contains('package_read_env_value "$PACKAGE_ROOT/.env" DATABASE_URL') -or
                    -not $unixStartup.Contains('package_initialize_environment "$PACKAGE_ROOT"') -or
                    -not $unixStartup.Contains('validate_host_environment_state') -or
                    -not $unixStartup.Contains('config/host.env.example') -or
                    -not $unixStartup.Contains('append_host_dependency_url') -or
                    -not $unixStartup.Contains('Database and Redis are absent from .env') -or
                    $unixStartup.Contains('package_set_env_value "$PACKAGE_ROOT/.env"') -or
                    $unixStartup.Contains('init.sh')) {
                    throw "Unix Host startup does not preserve template-aware .env initialization: $($package.name)"
                }
                if ($package.platform -eq 'windows') {
                    $windowsInitialization = [IO.File]::ReadAllText((Join-Path $extractRoot 'Init.ps1'))
                    if (-not $windowsInitialization.Contains('Initialize-PackageEnvironment') -or
                        -not $windowsInitialization.Contains('runtime\state') -or
                        -not $windowsInitialization.Contains("'deployed-version'") -or
                        -not $windowsInitialization.Contains('skipping managed PostgreSQL and Redis creation')) {
                        throw "Windows Host initialization does not gate managed dependency creation by deployed version: $($package.name)"
                    }
                }
                $backupEntrypoints = @('backup.sh')
                if ($package.platform -eq 'windows') { $backupEntrypoints += 'Backup.ps1' }
                foreach ($backupEntrypoint in $backupEntrypoints) {
                    $backupContents = [IO.File]::ReadAllText((Join-Path $extractRoot $backupEntrypoint))
                    if (-not $backupContents.Contains('--volumes-from') -or $backupContents -match '(?i)\bpg_(?:dump|restore)\b') {
                        throw "Host backup entrypoint must operate on the PostgreSQL container volume: $backupEntrypoint"
                    }
                }
                $expectedHostSettings = @(
                    'SYSTEM_ADMIN_EMAIL', 'SYSTEM_ADMIN_PASSWORD', 'SYSTEM_SETUP_COMPLETED'
                ) | Sort-Object
                $hostTemplatePath = Join-Path $extractRoot 'config\host.env.example'
                $actualHostSettings = @((Read-PackageMetadata -Path $hostTemplatePath).Keys | Sort-Object)
                if (Compare-Object -ReferenceObject $expectedHostSettings -DifferenceObject $actualHostSettings) {
                    throw "Host first-start template must contain only the default administrator and setup state: $($package.name)"
                }
            }
            'relay' {
                foreach ($requiredPath in @("bin/relayctl$extension", 'config/relay.env.example', 'config/config.example.yaml')) {
                    if (-not (Test-Path -LiteralPath (Join-Path $extractRoot $requiredPath.Replace('/', '\')))) {
                        throw "Relay package is missing $requiredPath."
                    }
                }
                $relayTemplate = Read-PackageMetadata -Path (Join-Path $extractRoot 'config\relay.env.example')
                foreach ($requiredSetting in @('RELAY_ACCESS_KEY', 'RELAY_VERSION', 'GITHUB_RELEASE_REPOSITORY', 'GITHUB_ACCESS_TOKEN')) {
                    if (-not $relayTemplate.ContainsKey($requiredSetting)) {
                        throw "Relay template is missing $requiredSetting in $($package.name)."
                    }
                }
                if ($relayTemplate.RELAY_VERSION -cne $release.version -or
                    $relayTemplate.GITHUB_RELEASE_REPOSITORY -cne $release.repository) {
                    throw "Relay template release metadata is stale in $($package.name)."
                }
            }
            'device-agent' {
                foreach ($requiredPath in @("bin/relayctl$extension", 'config/device-agent.env.example')) {
                    if (-not (Test-Path -LiteralPath (Join-Path $extractRoot $requiredPath.Replace('/', '\')))) {
                        throw "Device Agent package is missing $requiredPath."
                    }
                }
                $agentTemplate = Read-PackageMetadata -Path (Join-Path $extractRoot 'config\device-agent.env.example')
                foreach ($requiredSetting in @(
                    'WENZWORK_CONTROL_URL', 'WENZWORK_DEVICE_ACCESS_KEY', 'WENZWORK_DEVICE_STATE_FILE',
                    'WENZWORK_DEVICE_WORKSPACE', 'WENZWORK_AGENT_SECRET_STORE',
                    'GITHUB_RELEASE_REPOSITORY', 'GITHUB_ACCESS_TOKEN'
                )) {
                    if (-not $agentTemplate.ContainsKey($requiredSetting)) {
                        throw "Device Agent template is missing $requiredSetting in $($package.name)."
                    }
                }
                if ($agentTemplate.GITHUB_RELEASE_REPOSITORY -cne $release.repository) {
                    throw "Device Agent template release repository is stale in $($package.name)."
                }
            }
        }
        if ($package.component -eq 'device-agent' -and
            @((Get-ChildItem -LiteralPath (Join-Path $extractRoot 'bin') -File).Name | Where-Object { $_ -like '*device-agent*' }).Count -ne 1) {
            throw "Device Agent executable naming is ambiguous in $($package.name)."
        }

        foreach ($shellScript in Get-ChildItem -LiteralPath $extractRoot -Recurse -Filter '*.sh' -File) {
            $bytes = [IO.File]::ReadAllBytes($shellScript.FullName)
            if ($bytes.Length -lt 3 -or [Text.Encoding]::UTF8.GetString($bytes, 0, [Math]::Min($bytes.Length, 32)) -notmatch '^#!/usr/bin/env bash') {
                if ($shellScript.FullName -match 'config[\\/]relay-bootstrap') { continue }
                throw "Shell script has no Bash shebang: $($shellScript.FullName)"
            }
            if ($bytes -contains 13) { throw "Shell script contains CRLF: $($shellScript.FullName)" }
        }
        if ($package.platform -eq 'windows') {
            foreach ($powerShellFile in Get-ChildItem -LiteralPath $extractRoot -Recurse -Include '*.ps1', '*.psm1' -File) {
                Assert-PowerShellSyntax -Path $powerShellFile.FullName
            }
        }
        foreach ($textFile in Get-ChildItem -LiteralPath $extractRoot -Recurse -Include '*.env', '*.example', '*.json', '*.yaml', '*.yml', '*.pem', '*.sh', '*.ps1', '*.psm1' -File) {
            if (Select-String -LiteralPath $textFile.FullName -Pattern '-----BEGIN (?:[A-Z0-9]+ )?PRIVATE KEY-----' -Quiet) {
                throw "Private key material found in $($package.name)."
            }
            if (Select-String -LiteralPath $textFile.FullName -Pattern '(?:github_pat_|gh[pousr]_)[A-Za-z0-9_]{20,}' -Quiet) {
                throw "GitHub credential material found in $($package.name)."
            }
        }
        Write-VerifyLog -Message "Verified $($package.name)."
    }
}
finally {
    Remove-Item -LiteralPath $resolvedTemporary -Recurse -Force -ErrorAction SilentlyContinue
}

Write-VerifyLog -Message "All $($packages.Count) deployment packages passed structure, digest, target, and syntax verification."
