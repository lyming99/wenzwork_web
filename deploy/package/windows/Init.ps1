#Requires -Version 5.1

[CmdletBinding()]
param([switch]$Upgrade)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$root = [IO.Path]::GetFullPath($PSScriptRoot)
Import-Module (Join-Path $root 'runtime\lib\PackageCommon.psm1') -Force

$metadata = Assert-PackageTree -Root $root
if ($metadata.WENZWORK_PACKAGE_PLATFORM -ne 'windows') {
    throw "This package targets $($metadata.WENZWORK_PACKAGE_PLATFORM), not Windows."
}
$architecture = Get-PackageHostArchitecture
if ($metadata.WENZWORK_PACKAGE_ARCHITECTURE -ne $architecture) {
    throw "This package targets $($metadata.WENZWORK_PACKAGE_ARCHITECTURE), but this host is $architecture."
}
Initialize-PackageRuntimeDirectories -Root $root
$environment = Initialize-PackageEnvironment -Root $root -Metadata $metadata
if ($environment.Created) {
    throw "Edit $($environment.Path), then run Init.ps1 again."
}
$deployedVersionDirectory = Join-Path $root 'runtime\state'
$deployedVersionPath = Join-Path $deployedVersionDirectory 'deployed-version'

function Assert-DeployedVersionDirectory {
    $item = Get-Item -LiteralPath $deployedVersionDirectory -Force
    if (-not $item.PSIsContainer -or ($item.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) {
        throw 'Runtime state directory must be a regular directory and must not be a reparse point.'
    }
}

function Get-DeployedPackageVersion {
    Assert-DeployedVersionDirectory
    $item = Get-Item -LiteralPath $deployedVersionPath -Force -ErrorAction SilentlyContinue
    if ($null -eq $item) { return '' }
    if (-not $item.PSIsContainer -and ($item.Attributes -band [IO.FileAttributes]::ReparsePoint) -eq 0) {
        $value = [IO.File]::ReadAllText($item.FullName).Trim()
        if ($value -match '^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$') { return $value }
        throw 'Deployed version marker is invalid.'
    }
    throw 'Deployed version marker must be a regular file and must not be a reparse point.'
}

function Set-CurrentPackageVersionDeployed {
    Assert-DeployedVersionDirectory
    $temporary = "$deployedVersionPath.tmp.$([Guid]::NewGuid().ToString('N'))"
    try {
        [IO.File]::WriteAllText(
            $temporary,
            "$($metadata.WENZWORK_PACKAGE_VERSION)`r`n",
            [Text.UTF8Encoding]::new($false)
        )
        Move-Item -LiteralPath $temporary -Destination $deployedVersionPath -Force
    }
    finally {
        Remove-Item -LiteralPath $temporary -Force -ErrorAction SilentlyContinue
    }
}

function New-HostDependencyPassword {
    $bytes = [byte[]]::new(24)
    $generator = [Security.Cryptography.RandomNumberGenerator]::Create()
    try { $generator.GetBytes($bytes) }
    finally { $generator.Dispose() }
    return ([BitConverter]::ToString($bytes)).Replace('-', '').ToLowerInvariant()
}

function Initialize-HostDependencies {
    param([Parameter(Mandatory = $true)][string]$EnvironmentPath)
    $databaseUrl = [Environment]::GetEnvironmentVariable('DATABASE_URL', 'Process')
    $redisUrl = [Environment]::GetEnvironmentVariable('REDIS_URL', 'Process')
    if (-not [string]::IsNullOrWhiteSpace($databaseUrl) -and -not [string]::IsNullOrWhiteSpace($redisUrl)) { return }
    if (-not [string]::IsNullOrWhiteSpace($databaseUrl) -or -not [string]::IsNullOrWhiteSpace($redisUrl)) {
        throw 'Configure DATABASE_URL and REDIS_URL together, or leave both empty for managed Docker services.'
    }
    if (-not (Get-Command docker -ErrorAction SilentlyContinue)) {
        throw 'Docker is required when DATABASE_URL or REDIS_URL is not configured.'
    }
    if ([string]::IsNullOrWhiteSpace($databaseUrl)) {
        & docker container inspect wenzwork-postgres 2>$null | Out-Null
        if ($LASTEXITCODE -eq 0) { throw 'wenzwork-postgres already exists but DATABASE_URL is missing; recover its credential or remove the stale container.' }
        $databasePassword = New-HostDependencyPassword
        Write-PackageLog -Message 'Starting the managed PostgreSQL container...'
        & docker run -d --name wenzwork-postgres --restart unless-stopped -e POSTGRES_USER=wenzwork -e "POSTGRES_PASSWORD=$databasePassword" -e POSTGRES_DB=wenzwork -p 127.0.0.1:54328:5432 -v wenzwork-postgres-data:/var/lib/postgresql/data postgres:17-alpine | Out-Null
        if ($LASTEXITCODE -ne 0) { throw 'Could not start managed PostgreSQL.' }
        $databaseUrl = "postgres://wenzwork:$databasePassword@127.0.0.1:54328/wenzwork?sslmode=disable"
        Set-PackageEnvironmentValue -Path $EnvironmentPath -Name 'DATABASE_URL' -Value $databaseUrl
        [Environment]::SetEnvironmentVariable('DATABASE_URL', $databaseUrl, 'Process')
    }
    if ([string]::IsNullOrWhiteSpace($redisUrl)) {
        & docker container inspect wenzwork-redis 2>$null | Out-Null
        if ($LASTEXITCODE -eq 0) { throw 'wenzwork-redis already exists but REDIS_URL is missing; recover its credential or remove the stale container.' }
        $redisPassword = New-HostDependencyPassword
        Write-PackageLog -Message 'Starting the managed Redis container...'
        & docker run -d --name wenzwork-redis --restart unless-stopped -p 127.0.0.1:63798:6379 -v wenzwork-redis-data:/data redis:8-alpine redis-server --appendonly yes --requirepass $redisPassword | Out-Null
        if ($LASTEXITCODE -ne 0) { throw 'Could not start managed Redis.' }
        $redisUrl = "redis://:$redisPassword@127.0.0.1:63798/0"
        Set-PackageEnvironmentValue -Path $EnvironmentPath -Name 'REDIS_URL' -Value $redisUrl
        [Environment]::SetEnvironmentVariable('REDIS_URL', $redisUrl, 'Process')
    }
    foreach ($attempt in 1..60) {
        & docker exec wenzwork-postgres pg_isready -U wenzwork -d wenzwork 2>$null | Out-Null
        if ($LASTEXITCODE -eq 0) { break }
        Start-Sleep -Seconds 1
    }
    if ($LASTEXITCODE -ne 0) { throw 'Managed PostgreSQL did not become ready.' }
    foreach ($attempt in 1..60) {
        $reply = @(& docker exec -e "REDISCLI_AUTH=$redisPassword" wenzwork-redis redis-cli -h 127.0.0.1 -p 6379 ping 2>$null)
        if ($reply -contains 'PONG') { break }
        Start-Sleep -Seconds 1
    }
    if ($reply -notcontains 'PONG') { throw 'Managed Redis did not become ready.' }
}

if ($metadata.WENZWORK_PACKAGE_COMPONENT -eq 'host') {
    $environmentPath = $environment.Path
    [void](Import-PackageEnvironment -Path $environmentPath)
    Set-PackageComponentDefaults -Root $root -Metadata $metadata
    foreach ($name in @('GITHUB_ACCESS_TOKEN', 'GH_TOKEN', 'GITHUB_TOKEN')) {
        [Environment]::SetEnvironmentVariable($name, $null, 'Process')
    }
    $deployedVersion = Get-DeployedPackageVersion
    if ([string]::IsNullOrWhiteSpace($deployedVersion)) {
        Initialize-HostDependencies -EnvironmentPath $environmentPath
    }
    else {
        Write-PackageLog -Message "Detected deployed Host version $deployedVersion; skipping managed PostgreSQL and Redis creation."
    }
    foreach ($name in @('DATABASE_URL', 'REDIS_URL', 'SYSTEM_ADMIN_EMAIL')) {
        if ([string]::IsNullOrWhiteSpace([Environment]::GetEnvironmentVariable($name, 'Process'))) {
            throw "$name must be set in .env."
        }
    }
    if (-not (Test-Path -LiteralPath (Join-Path $root 'migrations') -PathType Container)) {
        throw 'Host migrations are missing.'
    }
    Push-Location $root
    try {
        Write-PackageLog -Message 'Applying Host database migrations...'
        & (Join-Path $root 'bin\wenzwork-migrate.exe') up
        if ($LASTEXITCODE -ne 0) { throw 'Host database migration failed.' }
        $statusLines = @(& (Join-Path $root 'bin\wenzwork-admin.exe') bootstrap status)
        if ($LASTEXITCODE -ne 0 -or $statusLines.Count -eq 0) { throw 'Could not read Host administrator status.' }
        $status = [string]$statusLines[-1]
        $initializeAdministrator = $false
        if ($status -eq 'uninitialized') {
            $email = [Environment]::GetEnvironmentVariable('SYSTEM_ADMIN_EMAIL')
            $password = [Environment]::GetEnvironmentVariable('SYSTEM_ADMIN_PASSWORD')
            if ([string]::IsNullOrWhiteSpace($email) -or [string]::IsNullOrWhiteSpace($password)) {
                throw 'Set SYSTEM_ADMIN_EMAIL and SYSTEM_ADMIN_PASSWORD in .env, then run Init.ps1 again.'
            }
            $initializeAdministrator = $true
        }
        elseif (-not $status.StartsWith("initialized`t")) {
            throw "Unexpected Host bootstrap status: $status"
        }
        else {
            Write-PackageLog -Message 'Host administrator is already initialized.'
        }
        if ($initializeAdministrator) {
            [Environment]::SetEnvironmentVariable('BOOTSTRAP_ADMIN_EMAIL', $email, 'Process')
            [Environment]::SetEnvironmentVariable('BOOTSTRAP_ADMIN_PASSWORD', $password, 'Process')
            [Environment]::SetEnvironmentVariable(
                'BOOTSTRAP_ADMIN_DISPLAY_NAME',
                [Environment]::GetEnvironmentVariable('SYSTEM_ADMIN_DISPLAY_NAME'),
                'Process'
            )
            & (Join-Path $root 'bin\wenzwork-admin.exe') bootstrap
            if ($LASTEXITCODE -ne 0) { throw 'Host administrator bootstrap failed.' }
        }
    }
    finally {
        Pop-Location
        [Environment]::SetEnvironmentVariable('BOOTSTRAP_ADMIN_PASSWORD', $null, 'Process')
    }
    Set-CurrentPackageVersionDeployed
}
else {
    Write-PackageLog -Message "Edit $root\.env before the first start."
}

Write-PackageLog -Message "$($metadata.WENZWORK_PACKAGE_COMPONENT) $($metadata.WENZWORK_PACKAGE_VERSION) initialized."
