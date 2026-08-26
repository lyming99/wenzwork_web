#Requires -Version 5.1

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

function Assert-True {
    param([Parameter(Mandatory = $true)][bool]$Condition, [Parameter(Mandatory = $true)][string]$Message)
    if (-not $Condition) { throw $Message }
}

$repositoryRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..\..\..'))
$testRoot = Join-Path ([IO.Path]::GetTempPath()) ('wenzwork-host-init-test-' + [Guid]::NewGuid().ToString('N'))
$packageRoot = Join-Path $testRoot 'package'
$toolsDirectory = Join-Path $testRoot 'tools'
$migrationCount = Join-Path $testRoot 'migration-count'
$dockerLog = Join-Path $testRoot 'docker.log'
$utf8 = [Text.UTF8Encoding]::new($false)
$startedProcessId = 0

try {
    foreach ($path in @(
        'bin', 'config', 'runtime\lib', 'logs', 'workspace', 'cache', 'migrations'
    )) {
        [void](New-Item -ItemType Directory -Path (Join-Path $packageRoot $path) -Force)
    }
    [void](New-Item -ItemType Directory -Path $toolsDirectory -Force)
    Copy-Item -LiteralPath (Join-Path $repositoryRoot 'deploy\package\windows\Init.ps1') -Destination (Join-Path $packageRoot 'Init.ps1')
    Copy-Item -LiteralPath (Join-Path $repositoryRoot 'deploy\package\windows\PackageCommon.psm1') -Destination (Join-Path $packageRoot 'runtime\lib\PackageCommon.psm1')
    foreach ($name in @('Start.ps1', 'Upgrade.ps1', 'start.sh', 'stop.sh', 'upgrade.sh')) {
        [IO.File]::WriteAllText((Join-Path $packageRoot $name), "exit 0`r`n", $utf8)
    }
    [IO.File]::WriteAllText((Join-Path $packageRoot 'VERSION'), "v1.0.0`r`n", $utf8)
    [IO.File]::WriteAllText((Join-Path $packageRoot 'PACKAGE-MANIFEST.json'), "{}`r`n", $utf8)
    [IO.File]::WriteAllLines((Join-Path $packageRoot 'config\package.env'), @(
        'WENZWORK_PACKAGE_COMPONENT=host'
        'WENZWORK_PACKAGE_PLATFORM=windows'
        'WENZWORK_PACKAGE_ARCHITECTURE=amd64'
        'WENZWORK_PACKAGE_VERSION=v1.0.0'
        'WENZWORK_PACKAGE_ASSET_BASENAME=wenzwork-host-deployment'
        'WENZWORK_PACKAGE_CHECKSUM_ASSET=DEPLOYMENT-SHA256SUMS'
        'WENZWORK_GITHUB_REPOSITORY=example/wenzwork'
    ), $utf8)
    Copy-Item -LiteralPath (Join-Path $repositoryRoot '.env.example') -Destination (Join-Path $packageRoot 'config\host.env.example')

    Import-Module (Join-Path $packageRoot 'runtime\lib\PackageCommon.psm1') -Force
    $policyEnvironmentNames = @(
        'SYSTEM_SETUP_COMPLETED', 'WENZWORK_ENV_FILE', 'MIGRATIONS_DIR', 'APP_ENV',
        'PUBLIC_BASE_URL', 'HTTP_ADDR', 'WEB_ROOT', 'LOG_LEVEL', 'REGISTRATION_ENABLED',
        'COOKIE_SECURE', 'ADMIN_MFA_REQUIRED', 'ALLOWED_ORIGINS', 'HOST_SECRETS_FILE',
        'RELEASE_ASSET_CACHE_DIR', 'GITHUB_RELEASE_REPOSITORY', 'RELAY_DEVELOPMENT_CA_DIR',
        'RELAY_BOOTSTRAP_ASSETS_DIR', 'REMOTE_MVP_ENABLED'
    )
    $savedPolicyEnvironment = @{}
    foreach ($name in $policyEnvironmentNames) {
        $savedPolicyEnvironment[$name] = [Environment]::GetEnvironmentVariable($name, 'Process')
        [Environment]::SetEnvironmentVariable($name, $null, 'Process')
    }
    try {
        [Environment]::SetEnvironmentVariable('SYSTEM_SETUP_COMPLETED', 'true', 'Process')
        $metadata = Get-PackageMetadata -Root $packageRoot
        Set-PackageComponentDefaults -Root $packageRoot -Metadata $metadata
        Assert-True ($env:APP_ENV -ceq 'production') 'Completed Windows Host did not select production mode.'
        Assert-True ($env:COOKIE_SECURE -ceq 'false') 'Completed Windows Host forced Cookie Secure in production.'
        Assert-True ($env:ADMIN_MFA_REQUIRED -ceq 'false') 'Completed Windows Host forced administrator MFA in production.'

        [Environment]::SetEnvironmentVariable('COOKIE_SECURE', 'true', 'Process')
        [Environment]::SetEnvironmentVariable('ADMIN_MFA_REQUIRED', 'true', 'Process')
        Set-PackageComponentDefaults -Root $packageRoot -Metadata $metadata
        Assert-True ($env:COOKIE_SECURE -ceq 'true') 'Windows Host overwrote the explicit Cookie Secure opt-in.'
        Assert-True ($env:ADMIN_MFA_REQUIRED -ceq 'true') 'Windows Host overwrote the explicit administrator MFA opt-in.'
    }
    finally {
        foreach ($name in $policyEnvironmentNames) {
            [Environment]::SetEnvironmentVariable($name, $savedPolicyEnvironment[$name], 'Process')
        }
    }

    $helperSource = @'
package main

import (
    "fmt"
    "os"
    "path/filepath"
    "strconv"
    "strings"
    "time"
)

func main() {
    name := strings.ToLower(filepath.Base(os.Args[0]))
    switch name {
    case "docker.exe":
        file, _ := os.OpenFile(os.Getenv("WENZWORK_TEST_DOCKER_LOG"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
        if file != nil {
            _, _ = fmt.Fprintln(file, strings.Join(os.Args[1:], " "))
            _ = file.Close()
        }
        os.Exit(88)
    case "wenzwork-migrate.exe":
        path := os.Getenv("WENZWORK_TEST_MIGRATION_COUNT")
        count := 0
        if value, err := os.ReadFile(path); err == nil {
            count, _ = strconv.Atoi(strings.TrimSpace(string(value)))
        }
        _ = os.WriteFile(path, []byte(strconv.Itoa(count+1)+"\n"), 0600)
    case "wenzwork-admin.exe":
        if len(os.Args) == 3 && os.Args[1] == "bootstrap" && os.Args[2] == "status" {
            fmt.Println("initialized\tadmin@example.test")
        }
    case "wenzwork-api.exe":
        time.Sleep(10 * time.Minute)
    }
}
'@
    $helperSourcePath = Join-Path $testRoot 'helper.go'
    $helperPath = Join-Path $testRoot 'helper.exe'
    [IO.File]::WriteAllText($helperSourcePath, $helperSource, $utf8)
    & go build -trimpath -o $helperPath $helperSourcePath
    if ($LASTEXITCODE -ne 0) { throw 'Could not build the Windows initialization test helper.' }
    foreach ($name in @('wenzwork-api.exe', 'wenzwork-migrate.exe', 'wenzwork-admin.exe')) {
        Copy-Item -LiteralPath $helperPath -Destination (Join-Path $packageRoot "bin\$name")
    }
    Copy-Item -LiteralPath $helperPath -Destination (Join-Path $toolsDirectory 'docker.exe')

    function Write-HostEnvironment {
        param([Parameter(Mandatory = $true)][bool]$IncludeDependencies)
        $lines = [Collections.Generic.List[string]]::new()
        $lines.Add('SYSTEM_ADMIN_EMAIL=admin@example.test')
        $lines.Add('SYSTEM_ADMIN_PASSWORD=administrator-password')
        $lines.Add('SYSTEM_SETUP_COMPLETED=false')
        if ($IncludeDependencies) {
            $lines.Add('DATABASE_URL=postgres://wenzwork:secret@127.0.0.1:54328/wenzwork?sslmode=disable')
            $lines.Add('REDIS_URL=redis://:secret@127.0.0.1:63798/0')
        }
        [IO.File]::WriteAllLines((Join-Path $packageRoot '.env'), $lines, $utf8)
    }

    $powerShellPath = (Get-Process -Id $PID).Path
    function Invoke-PackageInitialization {
        param([Parameter(Mandatory = $true)][string]$LogPath, [switch]$Upgrade)
        $arguments = @('-NoLogo', '-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', (Join-Path $packageRoot 'Init.ps1'))
        if ($Upgrade) { $arguments += '-Upgrade' }
        $previousErrorActionPreference = $ErrorActionPreference
        $ErrorActionPreference = 'Continue'
        try {
            $output = @(& $powerShellPath @arguments 2>&1)
            $exitCode = $LASTEXITCODE
        }
        finally {
            $ErrorActionPreference = $previousErrorActionPreference
        }
        [IO.File]::WriteAllLines($LogPath, @($output | ForEach-Object { [string]$_ }), $utf8)
        return $exitCode
    }

    foreach ($name in @('DATABASE_URL', 'REDIS_URL')) {
        [Environment]::SetEnvironmentVariable($name, $null, 'Process')
    }
    $env:WENZWORK_TEST_MIGRATION_COUNT = $migrationCount
    $env:WENZWORK_TEST_DOCKER_LOG = $dockerLog
    $env:PATH = "$toolsDirectory;$env:PATH"

    Remove-Item -LiteralPath (Join-Path $packageRoot '.env') -Force -ErrorAction SilentlyContinue
    $missingEnvironmentLog = Join-Path $testRoot 'missing-environment.log'
    Assert-True ((Invoke-PackageInitialization -LogPath $missingEnvironmentLog) -ne 0) 'Windows initialization continued immediately after creating a missing .env.'
    $environmentPath = Join-Path $packageRoot '.env'
    Assert-True (Test-Path -LiteralPath $environmentPath -PathType Leaf) 'Windows initialization did not create the missing .env.'
    Assert-True (
        [Convert]::ToBase64String([IO.File]::ReadAllBytes($environmentPath)) -ceq
        [Convert]::ToBase64String([IO.File]::ReadAllBytes((Join-Path $packageRoot 'config\host.env.example')))
    ) 'Windows initialization did not copy the Host environment template byte-for-byte.'
    $missingEnvironmentOutput = [IO.File]::ReadAllText($missingEnvironmentLog)
    Assert-True $missingEnvironmentOutput.Contains('Created ') 'Windows initialization did not report creating the missing .env.'
    Assert-True $missingEnvironmentOutput.Contains('Edit ') 'Windows initialization did not ask the operator to edit the new .env.'
    Assert-True (-not (Test-Path -LiteralPath $dockerLog)) 'Windows environment creation unexpectedly invoked Docker.'

    Write-HostEnvironment -IncludeDependencies $true
    Assert-True ((Invoke-PackageInitialization -LogPath (Join-Path $testRoot 'first.log')) -eq 0) 'First Windows initialization failed.'
    $deployedVersionPath = Join-Path $packageRoot 'runtime\state\deployed-version'
    Assert-True (Test-Path -LiteralPath $deployedVersionPath -PathType Leaf) 'First Windows initialization did not write the deployed version marker.'
    Assert-True ([IO.File]::ReadAllText($deployedVersionPath).Trim() -ceq 'v1.0.0') 'First Windows deployed version marker is incorrect.'
    Assert-True ([IO.File]::ReadAllText($migrationCount).Trim() -ceq '1') 'First Windows initialization did not run migrations exactly once.'
    Assert-True (-not (Test-Path -LiteralPath $dockerLog)) 'Configured Windows dependencies unexpectedly invoked Docker.'

    Write-HostEnvironment -IncludeDependencies $false
    $repeatedLog = Join-Path $testRoot 'repeated.log'
    Assert-True ((Invoke-PackageInitialization -LogPath $repeatedLog) -ne 0) 'Repeated Windows initialization accepted missing dependency URLs.'
    $repeatedOutput = [IO.File]::ReadAllText($repeatedLog)
    Assert-True $repeatedOutput.Contains('Detected deployed Host version v1.0.0; skipping managed PostgreSQL and Redis creation.') 'Repeated Windows initialization did not detect the deployed version.'
    Assert-True $repeatedOutput.Contains('DATABASE_URL must be set in .env.') 'Repeated Windows initialization did not fail closed for a missing database URL.'
    Assert-True (-not (Test-Path -LiteralPath $dockerLog)) 'Repeated Windows initialization attempted to create managed dependencies.'
    Assert-True ([IO.File]::ReadAllText($migrationCount).Trim() -ceq '1') 'Failed repeated Windows initialization unexpectedly ran migrations.'

    Write-HostEnvironment -IncludeDependencies $true
    $metadataPath = Join-Path $packageRoot 'config\package.env'
    $metadataText = [IO.File]::ReadAllText($metadataPath).Replace('WENZWORK_PACKAGE_VERSION=v1.0.0', 'WENZWORK_PACKAGE_VERSION=v1.1.0')
    [IO.File]::WriteAllText($metadataPath, $metadataText, $utf8)
    $upgradeLog = Join-Path $testRoot 'upgrade.log'
    Assert-True ((Invoke-PackageInitialization -LogPath $upgradeLog -Upgrade) -eq 0) 'Windows upgrade initialization failed.'
    Assert-True ([IO.File]::ReadAllText($upgradeLog).Contains('Detected deployed Host version v1.0.0; skipping managed PostgreSQL and Redis creation.')) 'Windows upgrade did not preserve the previous deployment marker.'
    Assert-True ([IO.File]::ReadAllText($deployedVersionPath).Trim() -ceq 'v1.1.0') 'Windows upgrade did not advance the deployed version marker.'
    Assert-True ([IO.File]::ReadAllText($migrationCount).Trim() -ceq '2') 'Windows upgrade did not run migrations.'
    Assert-True (-not (Test-Path -LiteralPath $dockerLog)) 'Windows upgrade attempted to create managed dependencies.'

    Copy-Item -LiteralPath (Join-Path $repositoryRoot 'deploy\package\windows\Start.ps1') -Destination (Join-Path $packageRoot 'Start.ps1') -Force
    & (Join-Path $packageRoot 'Start.ps1') -Background
    $pidFile = Join-Path $packageRoot 'runtime\pids\wenzwork.pid'
    Assert-True (Test-Path -LiteralPath $pidFile -PathType Leaf) 'Windows background start did not create its PID file.'
    Assert-True ([int]::TryParse(([IO.File]::ReadAllText($pidFile).Trim()), [ref]$startedProcessId)) 'Windows background start wrote an invalid PID.'
    $startedProcess = Get-Process -Id $startedProcessId -ErrorAction SilentlyContinue
    Assert-True ($null -ne $startedProcess) 'Windows background process exited unexpectedly.'
    $startedProcess.Kill()
    $startedProcess.WaitForExit()
    Remove-Item -LiteralPath $pidFile -Force
    $startedProcessId = 0

    Write-Host 'Windows Host initialization and background-start tests passed.'
}
finally {
    foreach ($name in @('WENZWORK_TEST_MIGRATION_COUNT', 'WENZWORK_TEST_DOCKER_LOG', 'DATABASE_URL', 'REDIS_URL')) {
        [Environment]::SetEnvironmentVariable($name, $null, 'Process')
    }
    $resolvedTestRoot = [IO.Path]::GetFullPath($testRoot)
    $resolvedTemporaryRoot = [IO.Path]::GetFullPath([IO.Path]::GetTempPath())
    if (-not $resolvedTestRoot.StartsWith($resolvedTemporaryRoot, [StringComparison]::OrdinalIgnoreCase)) {
        throw "Unsafe Windows initialization test cleanup path: $resolvedTestRoot"
    }
    if ($startedProcessId -gt 0) {
        Stop-Process -Id $startedProcessId -Force -ErrorAction SilentlyContinue
    }
    Remove-Item -LiteralPath $resolvedTestRoot -Recurse -Force -ErrorAction SilentlyContinue
}
