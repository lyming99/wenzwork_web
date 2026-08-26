#Requires -Version 5.1

[CmdletBinding()]
param([ValidateRange(1, 300)][int]$WaitSeconds = 30)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
Import-Module (Join-Path $PSScriptRoot 'lib\RelayCommon.psm1') -Force

Assert-RelayAdministrator
if (-not (Test-RelayServiceExists)) {
    Write-RelayLog 'WenzWorkRelay is not installed.'
    exit 0
}
Stop-RelayService -WaitSeconds $WaitSeconds
Write-RelayLog 'WenzWorkRelay is stopped.'
