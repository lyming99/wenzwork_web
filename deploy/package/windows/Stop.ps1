#Requires -Version 5.1

[CmdletBinding()]
param([ValidateRange(1, 300)][int]$WaitSeconds = 30)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$root = [IO.Path]::GetFullPath($PSScriptRoot)
Import-Module (Join-Path $root 'runtime\lib\PackageCommon.psm1') -Force

$pidFile = Join-Path $root 'runtime\pids\wenzwork.pid'
if (-not (Test-Path -LiteralPath $pidFile -PathType Leaf)) {
    Write-PackageLog -Message 'No package-managed process is running.'
    return
}
$pidValue = 0
if (-not [int]::TryParse(([IO.File]::ReadAllText($pidFile).Trim()), [ref]$pidValue) -or $pidValue -le 0) {
    throw "Invalid PID file: $pidFile"
}
$process = Get-Process -Id $pidValue -ErrorAction SilentlyContinue
if ($null -eq $process) {
    Remove-Item -LiteralPath $pidFile -Force
    Write-PackageLog -Message 'Removed stale PID file.'
    return
}
$expectedPrefix = ([IO.Path]::GetFullPath((Join-Path $root 'bin'))).TrimEnd('\') + '\'
if ([string]::IsNullOrWhiteSpace($process.Path) -or -not $process.Path.StartsWith($expectedPrefix, [StringComparison]::OrdinalIgnoreCase)) {
    throw "PID $pidValue does not belong to this package."
}
Stop-Process -Id $pidValue
try { Wait-Process -Id $pidValue -Timeout $WaitSeconds -ErrorAction Stop }
catch { throw "PID $pidValue did not stop within $WaitSeconds seconds." }
Remove-Item -LiteralPath $pidFile -Force
Write-PackageLog -Message 'Package-managed process stopped.'
