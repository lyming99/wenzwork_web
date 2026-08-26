#Requires -Version 5.1

[CmdletBinding()]
param(
    [ValidateSet('ready', 'live')][string]$Mode = 'ready',
    [ValidateRange(0, 300)][int]$WaitSeconds = 0,
    [string]$BaseUrl = 'http://127.0.0.1:19090',
    [switch]$SkipServiceCheck
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
Import-Module (Join-Path $PSScriptRoot 'lib\RelayCommon.psm1') -Force

if (-not $SkipServiceCheck) {
    $service = Get-Service -Name 'WenzWorkRelay' -ErrorAction Stop
    if ($service.Status -ne [ServiceProcess.ServiceControllerStatus]::Running) {
        throw "WenzWorkRelay service is $($service.Status), not Running."
    }
}
[void](Test-RelayHealth -Mode $Mode -WaitSeconds $WaitSeconds -BaseUrl $BaseUrl)
Write-RelayLog "Relay $Mode health check passed."
