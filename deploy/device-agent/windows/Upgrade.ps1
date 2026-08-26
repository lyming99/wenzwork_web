#Requires -Version 5.1

[CmdletBinding()]
param(
    [string]$InstallRoot,
    [string]$PackageFile,
    [string]$ArtifactUrl,
    [string]$ChecksumsFile,
    [string]$ChecksumsUrl,
    [string]$ChecksumsSignatureFile,
    [string]$ChecksumsSignatureUrl,
    [string]$SigningKeyFile,
    [string]$VerifierFile,
    [string]$VerifierSha256,
    [switch]$ConfirmUpgrade,
    [ValidateRange(1, 300)][int]$HealthWaitSeconds = 60
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
Import-Module (Join-Path $PSScriptRoot 'lib\DeviceAgentCommon.psm1') -Force

Assert-AgentAdministrator
if (-not (Test-AgentServiceExists)) { throw 'WenzWorkDeviceAgent is not installed.' }
if ([string]::IsNullOrWhiteSpace($InstallRoot)) { $InstallRoot = Get-AgentInstalledRoot }
$InstallRoot = Resolve-AgentManagedRoot -Path $InstallRoot -Label 'Install root'
if (-not $ConfirmUpgrade) {
    $confirmation = Read-Host 'Enter UPGRADE to stop the Agent, back up its complete data set, and continue'
    if ($confirmation -cne 'UPGRADE') { throw 'Device Agent upgrade was cancelled.' }
}

$metadataPath = Join-Path $InstallRoot 'current.json'
if (-not (Test-Path -LiteralPath $metadataPath -PathType Leaf)) { throw 'Current release metadata is missing.' }
$current = [IO.File]::ReadAllText($metadataPath) | ConvertFrom-Json
$previousRelease = Resolve-AgentManagedRoot -Path $current.releasePath -Label 'Current release path'
$releasePrefix = [IO.Path]::GetFullPath((Join-Path $InstallRoot 'releases')).TrimEnd('\') + '\'
if (-not $previousRelease.StartsWith($releasePrefix, [StringComparison]::OrdinalIgnoreCase)) { throw 'Current release is outside the managed releases directory.' }
$previousVersion = Read-AgentVersion -PackageRoot $previousRelease
$dataRoot = Resolve-AgentManagedRoot -Path (Get-AgentDataRoot) -Label 'Data root'
$environmentFile = Join-Path (Get-AgentConfigRoot) 'agent.env'
$expectedState = Join-Path $dataRoot 'state\agent-state.json'
[void](Assert-AgentEnvironmentFile -Path $environmentFile -ExpectedStatePath $expectedState)

$bootstrapVerifier = -not [string]::IsNullOrWhiteSpace($VerifierFile)
if (-not $bootstrapVerifier) { $VerifierFile = Join-Path $previousRelease 'bin\relayctl.exe' }
$VerifierFile = Assert-AgentTrustedVerifier -VerifierFile $VerifierFile -VerifierSha256 $VerifierSha256 -RequireHash:$bootstrapVerifier
if ([string]::IsNullOrWhiteSpace($SigningKeyFile)) { $SigningKeyFile = Join-Path $previousRelease 'release-signing-public-key.pem' }

$workDirectory = New-AgentTempDirectory
$serviceStopped = $false
$switched = $false
$backupPath = $null
try {
    $architecture = Get-AgentHostArchitecture
    $packageRoot = Resolve-AgentPackageSource `
        -PackageFile $PackageFile -ArtifactUrl $ArtifactUrl `
        -ChecksumsFile $ChecksumsFile -ChecksumsUrl $ChecksumsUrl `
        -ChecksumsSignatureFile $ChecksumsSignatureFile -ChecksumsSignatureUrl $ChecksumsSignatureUrl `
        -SigningKeyFile $SigningKeyFile -VerifierFile $VerifierFile -WorkDirectory $workDirectory
    $version = Assert-AgentReleaseTree -PackageRoot $packageRoot -VerifierFile $VerifierFile `
        -HostArchitecture $architecture -SigningKeyFile $SigningKeyFile
    $releasePath = Install-AgentReleaseTree -PackageRoot $packageRoot -InstallRoot $InstallRoot -Version $version `
        -VerifierFile $VerifierFile -HostArchitecture $architecture -SigningKeyFile $SigningKeyFile

    Stop-AgentService -WaitSeconds 45
    $serviceStopped = $true
    $backupPath = New-AgentBackup -DataRoot $dataRoot -EnvironmentFile $environmentFile -SourceVersion $previousVersion
    Set-AgentServiceBinary -BinaryPath (Join-Path $releasePath 'bin\wenzwork-device-agent.exe') -EnvironmentFile $environmentFile
    $switched = $true
    Start-AgentService -WaitSeconds 30
    $serviceStopped = $false
    [void](Test-AgentHealth -ReleasePath $releasePath -WaitSeconds $HealthWaitSeconds)
    Write-AgentCurrentMetadata -InstallRoot $InstallRoot -Version $version -ReleasePath $releasePath
    Remove-AgentOldBackups -Keep 5
    Write-AgentLog "Upgrade to $version completed; pre-upgrade backup retained at $backupPath."
}
catch {
    $upgradeError = $_
    if ($switched -and $null -ne $backupPath) {
        Write-Warning "Upgrade failed; restoring release $previousVersion and its complete data snapshot."
        try {
            try { Stop-AgentService -WaitSeconds 45 } catch { }
            [void](Restore-AgentBackup -BackupPath $backupPath -DataRoot $dataRoot -EnvironmentFile $environmentFile)
            Set-AgentServiceBinary -BinaryPath (Join-Path $previousRelease 'bin\wenzwork-device-agent.exe') -EnvironmentFile $environmentFile
            Start-AgentService -WaitSeconds 30
            [void](Test-AgentHealth -ReleasePath $previousRelease -WaitSeconds $HealthWaitSeconds)
            Write-AgentCurrentMetadata -InstallRoot $InstallRoot -Version $previousVersion -ReleasePath $previousRelease
            throw "Upgrade failed and release $previousVersion plus its data were restored: $($upgradeError.Exception.Message)"
        }
        catch {
            if ($_.Exception.Message.StartsWith('Upgrade failed and release')) { throw }
            throw "Upgrade and rollback both failed. Backup: $backupPath. Upgrade: $($upgradeError.Exception.Message). Rollback: $($_.Exception.Message)"
        }
    }
    if ($serviceStopped) {
        try { Start-AgentService -WaitSeconds 30 } catch { Write-Warning "Could not restart the unchanged release: $($_.Exception.Message)" }
    }
    throw
}
finally {
    Remove-AgentTempDirectory -Path $workDirectory
}
