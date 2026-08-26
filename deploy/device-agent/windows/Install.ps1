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
    [Parameter(Mandatory = $true)][string]$SigningKeyFile,
    [Parameter(Mandatory = $true)][string]$VerifierFile,
    [Parameter(Mandatory = $true)][string]$VerifierSha256,
    [string]$AgentEnvironmentFile,
    [ValidateRange(1, 300)][int]$HealthWaitSeconds = 60
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
Import-Module (Join-Path $PSScriptRoot 'lib\DeviceAgentCommon.psm1') -Force

Assert-AgentAdministrator
if (Test-AgentServiceExists) { throw 'WenzWorkDeviceAgent is already installed; use Upgrade.ps1.' }
if ([string]::IsNullOrWhiteSpace($InstallRoot)) { $InstallRoot = Get-AgentDefaultInstallRoot }
$InstallRoot = Resolve-AgentManagedRoot -Path $InstallRoot -Label 'Install root'
$dataRoot = Resolve-AgentManagedRoot -Path (Get-AgentDataRoot) -Label 'Data root'
$configRoot = Resolve-AgentManagedRoot -Path (Get-AgentConfigRoot) -Label 'Config root'
$environmentFile = Join-Path $configRoot 'agent.env'
$expectedState = Join-Path $dataRoot 'state\agent-state.json'
$VerifierFile = Assert-AgentTrustedVerifier -VerifierFile $VerifierFile -VerifierSha256 $VerifierSha256 -RequireHash
if (-not (Test-Path -LiteralPath $SigningKeyFile -PathType Leaf) -or ((Get-Item -LiteralPath $SigningKeyFile -Force).Attributes -band [IO.FileAttributes]::ReparsePoint)) {
    throw 'Trusted Release signing public key is missing or unsafe.'
}

$workDirectory = New-AgentTempDirectory
$serviceCreated = $false
try {
    $architecture = Get-AgentHostArchitecture
    $packageRoot = Resolve-AgentPackageSource `
        -PackageFile $PackageFile -ArtifactUrl $ArtifactUrl `
        -ChecksumsFile $ChecksumsFile -ChecksumsUrl $ChecksumsUrl `
        -ChecksumsSignatureFile $ChecksumsSignatureFile -ChecksumsSignatureUrl $ChecksumsSignatureUrl `
        -SigningKeyFile $SigningKeyFile -VerifierFile $VerifierFile -WorkDirectory $workDirectory
    $version = Assert-AgentReleaseTree -PackageRoot $packageRoot -VerifierFile $VerifierFile `
        -HostArchitecture $architecture -SigningKeyFile $SigningKeyFile

    foreach ($directory in @($InstallRoot, (Join-Path $InstallRoot 'releases'), $dataRoot, (Join-Path $dataRoot 'state'), (Join-Path $dataRoot 'workspace'), $configRoot)) {
        if (-not (Test-Path -LiteralPath $directory -PathType Container)) { [void](New-Item -ItemType Directory -Path $directory -Force) }
    }
    Protect-AgentPath -Path $dataRoot -Directory
    Protect-AgentPath -Path $configRoot -Directory
    Write-AgentInstallMetadata -InstallRoot $InstallRoot

    if (Test-Path -LiteralPath $environmentFile -PathType Leaf) {
        [void](Assert-AgentEnvironmentFile -Path $environmentFile -ExpectedStatePath $expectedState)
        Write-AgentLog 'Existing Device Access Key and Control Plane configuration preserved.'
    }
    else {
        if ([string]::IsNullOrWhiteSpace($AgentEnvironmentFile)) { throw 'First installation requires -AgentEnvironmentFile.' }
        Assert-RelaySecretFileAcl -Path $AgentEnvironmentFile
        Install-AgentEnvironmentFile -Source $AgentEnvironmentFile -Destination $environmentFile -ExpectedStatePath $expectedState
    }

    $releasePath = Install-AgentReleaseTree -PackageRoot $packageRoot -InstallRoot $InstallRoot -Version $version `
        -VerifierFile $VerifierFile -HostArchitecture $architecture -SigningKeyFile $SigningKeyFile
    $binary = Join-Path $releasePath 'bin\wenzwork-device-agent.exe'
    Install-AgentService -BinaryPath $binary -EnvironmentFile $environmentFile
    $serviceCreated = $true
    Start-AgentService -WaitSeconds 30
    [void](Test-AgentHealth -ReleasePath $releasePath -WaitSeconds $HealthWaitSeconds)
    Write-AgentCurrentMetadata -InstallRoot $InstallRoot -Version $version -ReleasePath $releasePath
    Write-AgentLog "Installation completed for windows/$architecture ($version). Business data is under $dataRoot."
}
catch {
    if ($serviceCreated) {
        try { Remove-AgentService } catch { Write-Warning "Could not remove failed service registration: $($_.Exception.Message)" }
    }
    throw
}
finally {
    Remove-AgentTempDirectory -Path $workDirectory
}
