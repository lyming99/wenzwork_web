#Requires -Version 5.1

[CmdletBinding()]
param(
    [string]$Version,
    [string]$PackageFile,
    [string]$ChecksumsFile
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$root = [IO.Path]::GetFullPath($PSScriptRoot)
Import-Module (Join-Path $root 'runtime\lib\PackageCommon.psm1') -Force

$metadata = Assert-PackageTree -Root $root
if ($metadata.WENZWORK_PACKAGE_PLATFORM -ne 'windows') { throw 'Upgrade.ps1 requires a Windows package.' }
Initialize-PackageRuntimeDirectories -Root $root
$currentVersion = [string]$metadata.WENZWORK_PACKAGE_VERSION
$environment = Read-PackageEnvironment -Path (Join-Path $root '.env')

$temporary = Join-Path $root "runtime\.upgrade.$([Guid]::NewGuid().ToString('N'))"
[void](New-Item -ItemType Directory -Path $temporary)
try {
    if ([string]::IsNullOrWhiteSpace($PackageFile)) {
        $download = Resolve-PackageUpgradeDownload -Metadata $metadata -Environment $environment `
            -Version $Version -TemporaryDirectory $temporary
        $PackageFile = [string]$download.PackageFile
        $ChecksumsFile = [string]$download.ChecksumsFile
    }
    else {
        if ([string]::IsNullOrWhiteSpace($ChecksumsFile)) {
            throw 'A local PackageFile requires ChecksumsFile.'
        }
        $PackageFile = [IO.Path]::GetFullPath($PackageFile)
        $ChecksumsFile = [IO.Path]::GetFullPath($ChecksumsFile)
    }
    if (-not (Test-Path -LiteralPath $PackageFile -PathType Leaf)) { throw "Package file is missing: $PackageFile" }
    if (-not (Test-Path -LiteralPath $ChecksumsFile -PathType Leaf)) { throw "Checksum file is missing: $ChecksumsFile" }

    $archiveName = [IO.Path]::GetFileName($PackageFile)
    $checksumMatches = [Collections.Generic.List[string]]::new()
    foreach ($line in [IO.File]::ReadAllLines($ChecksumsFile)) {
        if ($line -match '^([0-9A-Fa-f]{64})\s+\*?(.+)$') {
            $candidate = $Matches[2] -replace '^[.][\\/]', ''
            if ($candidate -ceq $archiveName) { $checksumMatches.Add($Matches[1].ToLowerInvariant()) }
        }
    }
    if ($checksumMatches.Count -ne 1) { throw "SHA256SUMS must contain exactly one entry for $archiveName." }
    $actualHash = Get-PackageFileSha256 -Path $PackageFile
    if ($actualHash -cne $checksumMatches[0]) { throw "SHA-256 mismatch for $archiveName." }
    Write-PackageLog -Message "Verified SHA-256 for $archiveName."

    $entries = @(& tar.exe -tzf $PackageFile)
    if ($LASTEXITCODE -ne 0 -or $entries.Count -eq 0) { throw 'Could not list deployment archive.' }
    foreach ($entryValue in $entries) {
        $entry = ([string]$entryValue) -replace '^[.][\\/]', ''
        if ($entry.StartsWith('/') -or $entry.StartsWith('\') -or $entry -match '(^|[\\/])[.][.]([\\/]|$)') {
            throw "Unsafe archive entry: $entry"
        }
    }
    $verboseEntries = @(& tar.exe -tvzf $PackageFile)
    if ($LASTEXITCODE -ne 0) { throw 'Could not inspect deployment archive types.' }
    foreach ($line in $verboseEntries) {
        if ([string]::IsNullOrEmpty($line)) { continue }
        if ($line.Substring(0, 1) -notin @('-', 'd')) { throw 'Archive contains a link or special file.' }
    }

    $stage = Join-Path $temporary 'stage'
    [void](New-Item -ItemType Directory -Path $stage)
    & tar.exe -xzf $PackageFile -C $stage
    if ($LASTEXITCODE -ne 0) { throw 'Could not extract deployment archive.' }
    $next = Assert-PackageTree -Root $stage
    if ($next.WENZWORK_PACKAGE_COMPONENT -cne $metadata.WENZWORK_PACKAGE_COMPONENT) { throw 'Package component mismatch.' }
    if ($next.WENZWORK_PACKAGE_PLATFORM -cne 'windows') { throw 'Package platform mismatch.' }
    if ($next.WENZWORK_PACKAGE_ARCHITECTURE -cne $metadata.WENZWORK_PACKAGE_ARCHITECTURE) { throw 'Package architecture mismatch.' }

    $timestamp = [DateTime]::UtcNow.ToString('yyyyMMddTHHmmssZ')
    $backup = Join-Path $root "cache\backups\$($currentVersion)_$timestamp"
    [void](New-Item -ItemType Directory -Path $backup)
    $managed = @('bin', 'config', 'runtime\lib', 'web', 'migrations', '.env', 'start.sh', 'stop.sh', 'init.sh', 'upgrade.sh', 'backup.sh', 'start.cmd', 'Start.ps1', 'Stop.ps1', 'Init.ps1', 'Upgrade.ps1', 'Backup.ps1', 'VERSION', 'PACKAGE-MANIFEST.json')
    foreach ($name in $managed) {
        $source = Join-Path $root $name
        if (-not (Test-Path -LiteralPath $source)) { continue }
        $destination = Join-Path $backup $name
        [void](New-Item -ItemType Directory -Path (Split-Path -Parent $destination) -Force)
        Copy-Item -LiteralPath $source -Destination $destination -Recurse -Force
    }
    Write-PackageLog -Message "Backed up $currentVersion package files to $backup."

    $wasRunning = $false
    $pidFile = Join-Path $root 'runtime\pids\wenzwork.pid'
    if (Test-Path -LiteralPath $pidFile -PathType Leaf) {
        $pidValue = 0
        [void][int]::TryParse(([IO.File]::ReadAllText($pidFile).Trim()), [ref]$pidValue)
        if ($pidValue -gt 0 -and (Get-Process -Id $pidValue -ErrorAction SilentlyContinue)) {
            $wasRunning = $true
            & (Join-Path $root 'Stop.ps1')
        }
    }

    function Restore-PackageBackup {
        foreach ($name in $managed) {
            Remove-Item -LiteralPath (Join-Path $root $name) -Recurse -Force -ErrorAction SilentlyContinue
        }
        foreach ($name in $managed) {
            $source = Join-Path $backup $name
            if (-not (Test-Path -LiteralPath $source)) { continue }
            $destination = Join-Path $root $name
            [void](New-Item -ItemType Directory -Path (Split-Path -Parent $destination) -Force)
            Copy-Item -LiteralPath $source -Destination $destination -Recurse -Force
        }
        Write-PackageLog -Message "Restored package files from $backup."
    }

    try {
        foreach ($name in @('bin', 'runtime\lib', 'web', 'migrations')) {
            Remove-Item -LiteralPath (Join-Path $root $name) -Recurse -Force -ErrorAction SilentlyContinue
            $source = Join-Path $stage $name
            if (Test-Path -LiteralPath $source) {
                $destination = Join-Path $root $name
                [void](New-Item -ItemType Directory -Path (Split-Path -Parent $destination) -Force)
                Copy-Item -LiteralPath $source -Destination $destination -Recurse -Force
            }
        }
        Copy-Item -Path (Join-Path $stage 'config\*') -Destination (Join-Path $root 'config') -Recurse -Force
        $lifecycleFiles = @('start.sh', 'stop.sh', 'init.sh', 'upgrade.sh', 'backup.sh', 'start.cmd', 'Start.ps1', 'Stop.ps1', 'Init.ps1', 'Upgrade.ps1', 'Backup.ps1', 'VERSION', 'PACKAGE-MANIFEST.json')
        foreach ($name in $lifecycleFiles) {
            Remove-Item -LiteralPath (Join-Path $root $name) -Force -ErrorAction SilentlyContinue
        }
        foreach ($name in $lifecycleFiles) {
            $source = Join-Path $stage $name
            if (Test-Path -LiteralPath $source) {
                Copy-Item -LiteralPath $source -Destination (Join-Path $root $name) -Force
            }
        }
        [void](Assert-PackageTree -Root $root)
        & (Join-Path $root 'Init.ps1') -Upgrade
        if ($wasRunning) {
            & (Join-Path $root 'Start.ps1') -Background
        }
    }
    catch {
        $upgradeError = $_
        Restore-PackageBackup
        if ($wasRunning) {
            try { & (Join-Path $root 'Start.ps1') -Background }
            catch { Write-Warning "The previous package was restored but could not be restarted: $($_.Exception.Message)" }
        }
        throw "Upgrade failed and $currentVersion was restored: $($upgradeError.Exception.Message)"
    }
    Write-PackageLog -Message "Upgrade completed: $currentVersion -> $($next.WENZWORK_PACKAGE_VERSION)"
}
finally {
    Remove-Item -LiteralPath $temporary -Recurse -Force -ErrorAction SilentlyContinue
}
