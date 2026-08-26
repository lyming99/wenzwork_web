#Requires -Version 5.1

[CmdletBinding()]
param(
    [ValidateRange(1, 300)][int]$WaitSeconds = 30,
    [ValidateRange(0, 300)][int]$HealthWaitSeconds = 45,
    [ValidateSet('ready', 'live')][string]$HealthMode = 'live',
    [string]$HealthBaseUrl = 'http://127.0.0.1:19090'
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
Import-Module (Join-Path $PSScriptRoot 'lib\RelayCommon.psm1') -Force

Assert-RelayAdministrator
if (-not (Test-RelayServiceExists)) { throw 'WenzWorkRelay is not installed.' }
Start-RelayService -WaitSeconds $WaitSeconds
[void](Test-RelayHealth -Mode $HealthMode -WaitSeconds $HealthWaitSeconds -BaseUrl $HealthBaseUrl)
Write-RelayLog "WenzWorkRelay is running and $HealthMode."
