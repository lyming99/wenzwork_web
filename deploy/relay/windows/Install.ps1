#Requires -Version 5.1

[CmdletBinding()]
param(
    [Alias('WorkDir')][string]$InstallRoot,
    [string]$ManagementUrl,
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
    [switch]$AccessKeyStdin,
    [string]$AccessKeyFile,
    [switch]$NonInteractive,
    [ValidateRange(1, 300)][int]$HealthWaitSeconds = 45,
    [string]$HealthBaseUrl = 'http://127.0.0.1:19090'
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
Import-Module (Join-Path $PSScriptRoot 'lib\RelayCommon.psm1') -Force

Assert-RelayAdministrator
$installRootWasExplicit = $PSBoundParameters.ContainsKey('InstallRoot')
$InstallRoot = Read-RelayInstallRoot -Value $InstallRoot -WasExplicit $installRootWasExplicit -NonInteractive:$NonInteractive
if (Test-RelayServiceExists) { throw 'WenzWorkRelay is already installed; use Upgrade.ps1.' }
if ($NonInteractive -and -not $AccessKeyStdin -and [string]::IsNullOrWhiteSpace($AccessKeyFile)) {
    throw 'Non-interactive installation requires -AccessKeyStdin or -AccessKeyFile.'
}

if ([string]::IsNullOrWhiteSpace($VerifierFile)) {
    $candidate = Join-Path $PSScriptRoot 'relayctl.exe'
    if (Test-Path -LiteralPath $candidate -PathType Leaf) { $VerifierFile = $candidate }
}
if ([string]::IsNullOrWhiteSpace($VerifierFile)) {
    throw '-VerifierFile must point to the bootstrap relayctl.exe obtained over the authenticated management HTTPS endpoint.'
}
$VerifierFile = Assert-RelayTrustedVerifier -VerifierFile $VerifierFile -VerifierSha256 $VerifierSha256 -RequireHash

$sourceValues = @(@($PackageDirectory, $PackageFile, $ArtifactUrl) | Where-Object { -not [string]::IsNullOrWhiteSpace($_) })
if ($sourceValues.Count -eq 0) {
    $candidateRoot = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
    if (Test-Path -LiteralPath (Join-Path $candidateRoot 'release-manifest.json') -PathType Leaf) {
        $PackageDirectory = $candidateRoot
    }
}
if ([string]::IsNullOrWhiteSpace($SigningKeyFile)) {
    $candidateKey = Join-Path (Split-Path -Parent $PSScriptRoot) 'release-signing-public-key.pem'
    if (Test-Path -LiteralPath $candidateKey -PathType Leaf) { $SigningKeyFile = $candidateKey }
}

$environmentFile = Join-Path $InstallRoot 'config\relay.env'
if (Test-Path -LiteralPath $environmentFile) {
    throw "A Relay environment already exists at $environmentFile. Purge the previous installation before installing again."
}

$workDirectory = New-RelayTempDirectory
$serviceCreated = $false
$environmentWritten = $false
$accessKey = ''
try {
    $architecture = Get-RelayHostArchitecture
    $packageRoot = Resolve-RelayPackageSource `
        -PackageDirectory $PackageDirectory -PackageFile $PackageFile -ArtifactUrl $ArtifactUrl `
        -ChecksumsFile $ChecksumsFile -ChecksumsUrl $ChecksumsUrl `
        -ChecksumsSignatureFile $ChecksumsSignatureFile -ChecksumsSignatureUrl $ChecksumsSignatureUrl `
        -SigningKeyFile $SigningKeyFile -VerifierFile $VerifierFile -WorkDirectory $workDirectory
    $version = Assert-RelayReleaseTree -PackageRoot $packageRoot -VerifierFile $VerifierFile -HostArchitecture $architecture
    $releasePath = Install-RelayReleaseTree -PackageRoot $packageRoot -InstallRoot $InstallRoot -Version $version -VerifierFile $VerifierFile -HostArchitecture $architecture

    if ([string]::IsNullOrWhiteSpace($ManagementUrl)) {
        if ($NonInteractive) {
            $ManagementUrl = 'https://wenzwork.com'
            Write-RelayLog "No -ManagementUrl was provided; using the default management URL: $ManagementUrl"
        }
        else {
            $ManagementUrl = Read-Host 'Management URL [https://wenzwork.com]'
            if ([string]::IsNullOrWhiteSpace($ManagementUrl)) { $ManagementUrl = 'https://wenzwork.com' }
        }
    }
    $ManagementUrl = Assert-RelayNetworkUrl -Url $ManagementUrl
    $accessKey = Read-RelayAccessKey -FromStdin:$AccessKeyStdin -File $AccessKeyFile

    [void](New-Item -ItemType Directory -Path (Join-Path $InstallRoot 'config') -Force)
    $serverBinary = Join-Path $releasePath 'bin\wenzwork-relay-server.exe'
    New-RelayServiceRegistration -BinaryPath $serverBinary
    $serviceCreated = $true
    Set-RelayInstallAcl -InstallRoot $InstallRoot

    Write-RelayEnvironment -Path $environmentFile -AccessKey $accessKey -ManagementUrl $ManagementUrl -Version $version
    $environmentWritten = $true
    Set-RelayServiceEnvironment -EnvironmentFile $environmentFile
    Write-RelayCurrentMetadata -InstallRoot $InstallRoot -Version $version -ReleasePath $releasePath

    Start-RelayService -WaitSeconds 30
    [void](Test-RelayHealth -Mode live -WaitSeconds $HealthWaitSeconds -BaseUrl $HealthBaseUrl)
    Write-RelayLog "Installation completed in $InstallRoot for windows/$architecture ($version)."
}
catch {
    if ($serviceCreated) {
        try { Remove-RelayServiceRegistration } catch { Write-Warning "Could not remove failed Relay service registration: $($_.Exception.Message)" }
    }
    if ($environmentWritten -and (Test-Path -LiteralPath $environmentFile -PathType Leaf)) {
        Remove-Item -LiteralPath $environmentFile -Force
    }
    $metadata = Join-Path $InstallRoot 'current.json'
    if (Test-Path -LiteralPath $metadata -PathType Leaf) { Remove-Item -LiteralPath $metadata -Force }
    throw
}
finally {
    $accessKey = ''
    Remove-RelayTempDirectory -Path $workDirectory
}
