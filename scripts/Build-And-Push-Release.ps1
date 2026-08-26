#Requires -Version 7.0

[CmdletBinding()]
param(
    [Parameter(Mandatory = $true, Position = 0)][Alias('v')][ValidateNotNullOrEmpty()][string]$Version,
    [ValidateSet('host', 'relay', 'device-agent')][string[]]$Components = @('host', 'relay', 'device-agent'),
    [ValidateSet('linux', 'windows', 'darwin')][string[]]$Platforms = @('linux', 'windows', 'darwin'),
    [ValidateSet('amd64', 'arm64')][string[]]$Architectures = @('amd64', 'arm64'),
    [ValidateSet('stable', 'beta')][string]$Channel = 'stable',
    [string]$UpdateNotes,
    [string]$ReleaseBaseUrl,
    [string]$AccessKey,
    [string]$OutputDirectory,
    [switch]$Draft,
    [switch]$SkipWebBuild,
    [switch]$AllowDirty,
    [switch]$KeepStaging
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$scriptDirectory = [IO.Path]::GetFullPath($PSScriptRoot)
$repositoryRoot = [IO.Path]::GetFullPath((Join-Path $scriptDirectory '..'))
if ([string]::IsNullOrWhiteSpace($OutputDirectory)) { $OutputDirectory = Join-Path $repositoryRoot 'dist' }
$OutputDirectory = [IO.Path]::GetFullPath($OutputDirectory)
$buildScript = Join-Path $scriptDirectory 'Build-DeploymentPackages.ps1'
$pushScript = Join-Path $scriptDirectory 'Push-Release.ps1'

$buildParameters = @{
    Version = $Version
    Components = $Components
    Platforms = $Platforms
    Architectures = $Architectures
    OutputDirectory = $OutputDirectory
    SkipWebBuild = $SkipWebBuild
    LocalPush = $true
    KeepStaging = $KeepStaging
}
if ($AllowDirty) {
    Write-Verbose '-AllowDirty is retained for compatibility; local pushes no longer inspect the git worktree.'
}
& $buildScript @buildParameters
if ($LASTEXITCODE -ne 0) { throw "Deployment build failed with exit code $LASTEXITCODE." }

$componentNames = @{
    host = 'WenzWork Host'
    relay = 'WenzWork Relay'
    'device-agent' = 'WenzWork Device Agent'
}
$softwareName = (@($Components | ForEach-Object { $componentNames[$_] }) -join '、')
$pushParameters = @{
    Version = $Version
    Project = 'web'
    AssetManifestPath = (Join-Path $OutputDirectory 'DEPLOYMENT-RELEASE-MANIFEST.json')
    Channel = $Channel
    SoftwareName = $softwareName
    Draft = $Draft
}
if (-not [string]::IsNullOrWhiteSpace($UpdateNotes)) { $pushParameters.UpdateNotes = $UpdateNotes }
if (-not [string]::IsNullOrWhiteSpace($ReleaseBaseUrl)) { $pushParameters.ReleaseBaseUrl = $ReleaseBaseUrl }
if (-not [string]::IsNullOrWhiteSpace($AccessKey)) { $pushParameters.AccessKey = $AccessKey }
& $pushScript @pushParameters
if ($LASTEXITCODE -ne 0) { throw "Release push failed with exit code $LASTEXITCODE." }
