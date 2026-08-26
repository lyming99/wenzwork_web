#Requires -Version 5.1
[CmdletBinding()] param([ValidateRange(1, 300)][int]$WaitSeconds = 30)
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
Import-Module (Join-Path $PSScriptRoot 'lib\DeviceAgentCommon.psm1') -Force
Assert-AgentAdministrator
Start-AgentService -WaitSeconds $WaitSeconds
