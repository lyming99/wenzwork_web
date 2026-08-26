#Requires -Version 5.1

[CmdletBinding()]
param(
    [Alias('WorkDir')][string]$InstallRoot,
    [switch]$ConfirmUninstall,
    [switch]$Purge,
    [string]$ConfirmPurge
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
Import-Module (Join-Path $PSScriptRoot 'lib\RelayCommon.psm1') -Force

Assert-RelayAdministrator
if ([string]::IsNullOrWhiteSpace($InstallRoot)) { $InstallRoot = Get-RelayDefaultInstallRoot }
$InstallRoot = Resolve-RelayInstallRoot -Path $InstallRoot

if (-not $ConfirmUninstall) {
    $confirmation = Read-Host 'Enter UNINSTALL to remove the WenzWorkRelay service and installed releases'
    if ($confirmation -cne 'UNINSTALL') { throw 'Relay uninstall was cancelled.' }
}
if ($Purge -and $ConfirmPurge -cne 'DELETE_RELAY_DATA') {
    throw 'Purging the Access Key and configuration requires -ConfirmPurge DELETE_RELAY_DATA.'
}

Remove-RelayServiceRegistration
Remove-RelayInstalledFiles -InstallRoot $InstallRoot -Purge:$Purge
if ($Purge) {
    Write-RelayLog "WenzWorkRelay and all managed data under $InstallRoot were removed permanently."
}
else {
    Write-RelayLog "WenzWorkRelay and installed releases were removed; $InstallRoot\config was preserved."
}
