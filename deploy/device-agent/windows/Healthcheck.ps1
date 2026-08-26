#Requires -Version 5.1

[CmdletBinding()]
param(
    [string]$InstallRoot,
    [ValidateRange(0, 300)][int]$WaitSeconds = 0
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
Import-Module (Join-Path $PSScriptRoot 'lib\DeviceAgentCommon.psm1') -Force

if ([string]::IsNullOrWhiteSpace($InstallRoot)) { $InstallRoot = Get-AgentInstalledRoot }
$InstallRoot = Resolve-AgentManagedRoot -Path $InstallRoot -Label 'Install root'
$metadata = [IO.File]::ReadAllText((Join-Path $InstallRoot 'current.json')) | ConvertFrom-Json
$releasePath = Resolve-AgentManagedRoot -Path $metadata.releasePath -Label 'Current release path'
[void](Test-AgentHealth -ReleasePath $releasePath -WaitSeconds $WaitSeconds)
Write-AgentLog "Service is running release $($metadata.version)."
