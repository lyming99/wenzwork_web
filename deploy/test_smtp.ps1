#requires -Version 5.1

<#
.SYNOPSIS
Tests SMTP delivery through WenzWork's production Go mail sender.

.DESCRIPTION
Runs the wenzwork-admin SMTP test command against an explicitly selected .env
file. From a source checkout it uses `go run`; a native wenzwork-admin binary
can instead be supplied with -AdminPath. The SMTP password is never printed.

.PARAMETER EnvFile
Path to the dotenv file. When omitted, the script looks for .env beside this
script and then in its parent directory.

.PARAMETER AdminPath
Optional path to a native wenzwork-admin executable.

.PARAMETER GoPath
Go executable name or path used when AdminPath is omitted. Defaults to go.

.EXAMPLE
.\deploy\test_smtp.ps1 -EnvFile .\.env
#>

[CmdletBinding()]
param(
    [Parameter(Position = 0)]
    [string]$EnvFile,

    [string]$AdminPath,

    [string]$GoPath = 'go'
)

Set-StrictMode -Version 3.0
$ErrorActionPreference = 'Stop'

function Write-TestLog {
    param([Parameter(Mandatory = $true)][string]$Message)
    [Console]::Out.WriteLine("[wenzwork-smtp-test] $Message")
}

function Stop-SmtpTest {
    param([Parameter(Mandatory = $true)][string]$Message)
    throw [System.InvalidOperationException]::new($Message)
}

function Resolve-EnvFile {
    param([string]$RequestedPath)

    if (-not [string]::IsNullOrWhiteSpace($RequestedPath)) {
        if (-not (Test-Path -LiteralPath $RequestedPath -PathType Leaf)) {
            Stop-SmtpTest "Environment file does not exist: $RequestedPath"
        }
        return (Resolve-Path -LiteralPath $RequestedPath).Path
    }

    $candidates = @(
        (Join-Path -Path $PSScriptRoot -ChildPath '.env'),
        (Join-Path -Path (Split-Path -Parent $PSScriptRoot) -ChildPath '.env')
    )
    foreach ($candidate in $candidates) {
        if (Test-Path -LiteralPath $candidate -PathType Leaf) {
            return (Resolve-Path -LiteralPath $candidate).Path
        }
    }

    Stop-SmtpTest 'No .env file was found. Pass its path with -EnvFile.'
}

function Resolve-Application {
    param(
        [Parameter(Mandatory = $true)][string]$NameOrPath,
        [Parameter(Mandatory = $true)][string]$Description
    )

    if (Test-Path -LiteralPath $NameOrPath -PathType Leaf) {
        return (Resolve-Path -LiteralPath $NameOrPath).Path
    }
    $command = Get-Command -Name $NameOrPath -CommandType Application -ErrorAction SilentlyContinue
    if ($null -eq $command) {
        Stop-SmtpTest "$Description was not found: $NameOrPath"
    }
    return $command.Source
}

function Invoke-SmtpTest {
    $resolvedEnvFile = Resolve-EnvFile $EnvFile
    Write-TestLog "Using environment file: $resolvedEnvFile"
    Write-TestLog 'Running the same Go SMTP sender used by the WenzWork API...'

    if (-not [string]::IsNullOrWhiteSpace($AdminPath)) {
        $adminCommand = Resolve-Application $AdminPath 'wenzwork-admin executable'
        & $adminCommand smtp test --env-file $resolvedEnvFile
    }
    else {
        $repositoryRoot = Split-Path -Parent $PSScriptRoot
        $serverDirectory = Join-Path -Path $repositoryRoot -ChildPath 'server'
        if (-not (Test-Path -LiteralPath (Join-Path $serverDirectory 'go.mod') -PathType Leaf)) {
            Stop-SmtpTest 'The server Go module was not found. Run this script from the repository or pass -AdminPath.'
        }
        $goCommand = Resolve-Application $GoPath 'Go executable'
        & $goCommand -C $serverDirectory run ./cmd/admin smtp test --env-file $resolvedEnvFile
    }

    $exitCode = $LASTEXITCODE
    if ($exitCode -ne 0) {
        Stop-SmtpTest "WenzWork Go SMTP test failed with exit code $exitCode."
    }
    Write-TestLog 'SMTP check succeeded through the WenzWork Go mail sender.'
}

try {
    Invoke-SmtpTest
}
catch {
    [Console]::Error.WriteLine("[wenzwork-smtp-test] ERROR: $($_.Exception.Message)")
    exit 1
}
