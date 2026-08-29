#Requires -Version 5.1

[CmdletBinding()]
param(
    [switch]$Background,
    [Parameter(ValueFromRemainingArguments = $true)][string[]]$Arguments
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$root = [IO.Path]::GetFullPath($PSScriptRoot)
Import-Module (Join-Path $root 'runtime\lib\PackageCommon.psm1') -Force

$metadata = Assert-PackageTree -Root $root
if ($metadata.WENZWORK_PACKAGE_PLATFORM -ne 'windows') { throw 'Start.ps1 requires a Windows package.' }
if ($metadata.WENZWORK_PACKAGE_ARCHITECTURE -ne (Get-PackageHostArchitecture)) {
    throw 'Package architecture does not match this Windows host.'
}
Initialize-PackageRuntimeDirectories -Root $root
$environment = Initialize-PackageEnvironment -Root $root -Metadata $metadata
if ($environment.Created) {
    throw "Edit $($environment.Path), then run Start.ps1 again."
}
[void](Import-PackageEnvironment -Path $environment.Path)
Set-PackageComponentDefaults -Root $root -Metadata $metadata
foreach ($name in @('GITHUB_ACCESS_TOKEN', 'GH_TOKEN', 'GITHUB_TOKEN')) {
    [Environment]::SetEnvironmentVariable($name, $null, 'Process')
}

if ($metadata.WENZWORK_PACKAGE_COMPONENT -eq 'host') {
    & (Join-Path $root 'Init.ps1')
    if ($LASTEXITCODE -ne 0) { throw 'Host initialization failed.' }
    [void](Import-PackageEnvironment -Path (Join-Path $root '.env'))
    Set-PackageComponentDefaults -Root $root -Metadata $metadata
    foreach ($name in @('GITHUB_ACCESS_TOKEN', 'GH_TOKEN', 'GITHUB_TOKEN')) {
        [Environment]::SetEnvironmentVariable($name, $null, 'Process')
    }
}

[string[]]$serviceArguments = @()
if ($null -ne $Arguments) { $serviceArguments = @($Arguments) }
switch ($metadata.WENZWORK_PACKAGE_COMPONENT) {
    'host' {
        $executable = Join-Path $root 'bin\wenzwork-api.exe'
    }
    'relay' {
        $executable = Join-Path $root 'bin\wenzwork-relay-server.exe'
    }
    'device-agent' {
        $executable = Join-Path $root 'bin\wenzwork-device-agent.exe'
        $serviceArguments = @('serve') + $serviceArguments
    }
    default { throw "Unknown package component: $($metadata.WENZWORK_PACKAGE_COMPONENT)" }
}

if (-not $Background) {
    Push-Location $root
    try {
        & $executable @serviceArguments
        exit $LASTEXITCODE
    }
    finally {
        Pop-Location
    }
}

$pidFile = Join-Path $root 'runtime\pids\wenzwork.pid'
if (Test-Path -LiteralPath $pidFile -PathType Leaf) {
    Write-PackageLog -Message "Stopping the previous $($metadata.WENZWORK_PACKAGE_COMPONENT) instance before startup..."
    & (Join-Path $root 'Stop.ps1')
}
$logFile = Join-Path $root 'runtime\logs\wenzwork.log'
$errorLog = Join-Path $root 'runtime\logs\wenzwork-error.log'
$startParameters = @{
    FilePath = $executable
    WorkingDirectory = $root
    RedirectStandardOutput = $logFile
    RedirectStandardError = $errorLog
    WindowStyle = 'Hidden'
    PassThru = $true
}
if ($serviceArguments.Count -gt 0) { $startParameters.ArgumentList = $serviceArguments }
$process = Start-Process @startParameters
[IO.File]::WriteAllText($pidFile, "$($process.Id)`n", [Text.UTF8Encoding]::new($false))
Start-Sleep -Seconds 1
if ($process.HasExited) { throw "$($metadata.WENZWORK_PACKAGE_COMPONENT) exited during startup; inspect $errorLog." }
Write-PackageLog -Message "$($metadata.WENZWORK_PACKAGE_COMPONENT) started as PID $($process.Id)."
