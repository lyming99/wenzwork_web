#Requires -Version 5.1

[CmdletBinding()]
param(
    [string]$InstallRoot,
    [switch]$ConfirmUninstall,
    [switch]$Purge,
    [string]$ConfirmPurge
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
Import-Module (Join-Path $PSScriptRoot 'lib\DeviceAgentCommon.psm1') -Force

Assert-AgentAdministrator
if ([string]::IsNullOrWhiteSpace($InstallRoot)) { $InstallRoot = Get-AgentInstalledRoot }
$InstallRoot = Resolve-AgentManagedRoot -Path $InstallRoot -Label 'Install root'
if (-not $ConfirmUninstall) {
    $confirmation = Read-Host 'Enter UNINSTALL to remove the Device Agent service and binaries'
    if ($confirmation -cne 'UNINSTALL') { throw 'Device Agent uninstall was cancelled.' }
}
if ($Purge -and $ConfirmPurge -cne 'DELETE_DEVICE_AGENT_DATA') {
    throw 'Permanent data deletion requires -Purge -ConfirmPurge DELETE_DEVICE_AGENT_DATA.'
}

Remove-AgentService
Remove-AgentInstalledBinaries -InstallRoot $InstallRoot
if ($Purge) {
    Remove-AgentAllData
    Write-AgentLog 'Service, binaries, configuration, identity, secrets, backups, and business data were permanently removed.'
}
else {
    Write-AgentLog "Service and binaries were removed. Configuration, identity, secrets, backups, and all business data remain under $(Get-AgentApplicationRoot)."
}
