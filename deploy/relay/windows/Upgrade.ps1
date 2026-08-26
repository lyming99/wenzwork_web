#Requires -Version 5.1

[CmdletBinding()]
param(
    [Alias('WorkDir')][string]$InstallRoot,
    [string]$PackageDirectory,
    [string]$PackageFile,
    [string]$ArtifactUrl,
    [string]$ChecksumsFile,
    [string]$ChecksumsUrl,
    [string]$ChecksumsSignatureFile,
    [string]$ChecksumsSignatureUrl,
    [string]$SigningKeyFile,
    [string]$VerifierFile,
    [string]$VerifierSha256,
    [switch]$ConfirmDrained,
    [ValidateRange(1, 300)][int]$HealthWaitSeconds = 60,
    [string]$HealthBaseUrl = 'http://127.0.0.1:19090'
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
Import-Module (Join-Path $PSScriptRoot 'lib\RelayCommon.psm1') -Force

Assert-RelayAdministrator
if ([string]::IsNullOrWhiteSpace($InstallRoot)) { $InstallRoot = Get-RelayDefaultInstallRoot }
$InstallRoot = Resolve-RelayInstallRoot -Path $InstallRoot
if (-not (Test-RelayServiceExists)) { throw 'WenzWorkRelay is not installed.' }

if (-not $ConfirmDrained) {
    $confirmation = Read-Host 'Drain this node and remove it from the external load balancer, then enter UPGRADE'
    if ($confirmation -cne 'UPGRADE') { throw 'Relay upgrade was cancelled.' }
}

$previousBinary = [IO.Path]::GetFullPath((Get-RelayServiceImagePath))
$releasesPrefix = [IO.Path]::GetFullPath((Join-Path $InstallRoot 'releases')).TrimEnd('\') + '\'
if (-not $previousBinary.StartsWith($releasesPrefix, [StringComparison]::OrdinalIgnoreCase)) {
    throw 'The current WenzWorkRelay ImagePath is outside the managed releases directory.'
}

$bootstrapVerifier = -not [string]::IsNullOrWhiteSpace($VerifierFile)
if (-not $bootstrapVerifier) {
    $VerifierFile = Join-Path ([IO.Path]::GetDirectoryName($previousBinary)) 'relayctl.exe'
}
$VerifierFile = Assert-RelayTrustedVerifier -VerifierFile $VerifierFile -VerifierSha256 $VerifierSha256 -RequireHash:$bootstrapVerifier

$sourceValues = @(@($PackageDirectory, $PackageFile, $ArtifactUrl) | Where-Object { -not [string]::IsNullOrWhiteSpace($_) })
if ($sourceValues.Count -eq 0) {
    $candidateRoot = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
    if (Test-Path -LiteralPath (Join-Path $candidateRoot 'release-manifest.json') -PathType Leaf) { $PackageDirectory = $candidateRoot }
}
if ([string]::IsNullOrWhiteSpace($SigningKeyFile)) {
    $candidateKeys = @(
        (Join-Path (Split-Path -Parent $PSScriptRoot) 'release-signing-public-key.pem'),
        (Join-Path (Split-Path -Parent (Split-Path -Parent $PSScriptRoot)) 'release-signing-public-key.pem')
    )
    foreach ($candidate in $candidateKeys) {
        if (Test-Path -LiteralPath $candidate -PathType Leaf) { $SigningKeyFile = $candidate; break }
    }
}

$environmentFile = [IO.Path]::GetFullPath((Get-RelayServiceEnvironmentFile))
$expectedEnvironmentFile = [IO.Path]::GetFullPath((Join-Path $InstallRoot 'config\relay.env'))
if (-not $environmentFile.Equals($expectedEnvironmentFile, [StringComparison]::OrdinalIgnoreCase)) {
    throw 'The WenzWorkRelay environment file is outside the managed config directory.'
}
if (-not (Test-Path -LiteralPath $environmentFile -PathType Leaf)) { throw 'The WenzWorkRelay environment file is missing.' }

$workDirectory = New-RelayTempDirectory
$previousEnvironment = $null
$serviceStopped = $false
$switched = $false
try {
    $architecture = Get-RelayHostArchitecture
    $packageRoot = Resolve-RelayPackageSource `
        -PackageDirectory $PackageDirectory -PackageFile $PackageFile -ArtifactUrl $ArtifactUrl `
        -ChecksumsFile $ChecksumsFile -ChecksumsUrl $ChecksumsUrl `
        -ChecksumsSignatureFile $ChecksumsSignatureFile -ChecksumsSignatureUrl $ChecksumsSignatureUrl `
        -SigningKeyFile $SigningKeyFile -VerifierFile $VerifierFile -WorkDirectory $workDirectory
    $version = Assert-RelayReleaseTree -PackageRoot $packageRoot -VerifierFile $VerifierFile -HostArchitecture $architecture
    $releasePath = Install-RelayReleaseTree -PackageRoot $packageRoot -InstallRoot $InstallRoot -Version $version -VerifierFile $VerifierFile -HostArchitecture $architecture
    $newBinary = Join-Path $releasePath 'bin\wenzwork-relay-server.exe'

    $previousEnvironment = [IO.File]::ReadAllBytes($environmentFile)
    Stop-RelayService -WaitSeconds 30
    $serviceStopped = $true

    # SCM stores ImagePath as one registry value. Updating it switches the complete,
    # already-verified version directory in one native service configuration operation.
    Set-RelayServiceBinary -BinaryPath $newBinary
    $switched = $true
    Update-RelayEnvironmentVersion -Path $environmentFile -Version $version
    Start-RelayService -WaitSeconds 30
    $serviceStopped = $false
    [void](Test-RelayHealth -Mode ready -WaitSeconds $HealthWaitSeconds -BaseUrl $HealthBaseUrl)
    Write-RelayCurrentMetadata -InstallRoot $InstallRoot -Version $version -ReleasePath $releasePath
    Write-RelayLog "Upgrade to $version completed; configuration and Access Key were preserved."
}
catch {
    $upgradeError = $_
    if ($switched) {
        Write-Warning 'Relay upgrade failed; restoring the previous SCM ImagePath and environment atomically.'
        try {
            Stop-RelayService -WaitSeconds 30
            Set-RelayServiceBinary -BinaryPath $previousBinary
            if ($null -ne $previousEnvironment) { Write-RelayAtomicBytes -Path $environmentFile -Bytes $previousEnvironment }
            Start-RelayService -WaitSeconds 30
            [void](Test-RelayHealth -Mode ready -WaitSeconds $HealthWaitSeconds -BaseUrl $HealthBaseUrl)
            throw "Upgrade failed and the previous release was restored: $($upgradeError.Exception.Message)"
        }
        catch {
            if ($_.Exception.Message.StartsWith('Upgrade failed and the previous release was restored:')) { throw }
            throw "Upgrade failed and rollback also failed. Keep the host out of the load balancer. Upgrade error: $($upgradeError.Exception.Message). Rollback error: $($_.Exception.Message)"
        }
    }
    elseif (Test-RelayServiceExists) {
        try { Start-RelayService -WaitSeconds 30 } catch { Write-Warning "Could not restart the unchanged Relay release: $($_.Exception.Message)" }
    }
    throw
}
finally {
    if ($null -ne $previousEnvironment) { [Array]::Clear($previousEnvironment, 0, $previousEnvironment.Length) }
    Remove-RelayTempDirectory -Path $workDirectory
}
