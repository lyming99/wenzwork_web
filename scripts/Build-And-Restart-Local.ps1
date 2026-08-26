[CmdletBinding()]
param(
    [switch]$DryRun,
    [switch]$SkipDependencies,
    # Backward-compatible no-op; the BAT wrapper no longer pauses on exit.
    [switch]$NoPause
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$repoRoot = [IO.Path]::GetFullPath((Split-Path -Parent $PSScriptRoot))
$packageRoot = Join-Path $repoRoot 'dist\client-test-windows-amd64'
$managementRoot = Join-Path $packageRoot 'management'
$relayRoot = Join-Path $packageRoot 'relay'
$agentRoot = Join-Path $packageRoot 'device-agent'
$temporaryRoot = Join-Path $repoRoot '.tmp'
$runID = '{0}-{1}' -f (Get-Date -Format 'yyyyMMdd-HHmmss'), ([Guid]::NewGuid().ToString('N').Substring(0, 8))
$stageRoot = Join-Path $temporaryRoot ("local-build-$runID")
$backupRoot = Join-Path $temporaryRoot ("local-build-backup-$runID")

$artifactFiles = @(
    'management\bin\wenzwork-api.exe',
    'management\bin\wenzwork-admin.exe',
    'management\bin\wenzwork-migrate.exe',
    'relay\wenzwork-relay-server.exe',
    'relay\relayctl.exe',
    'device-agent\device-agent.exe'
)
$artifactDirectories = @(
    'management\web',
    'management\migrations'
)

$services = @(
    [pscustomobject]@{
        Name = 'management'
        Root = $managementRoot
        Script = Join-Path $managementRoot 'Start-Management.ps1'
        Executable = Join-Path $managementRoot 'bin\wenzwork-api.exe'
    },
    [pscustomobject]@{
        Name = 'relay'
        Root = $relayRoot
        Script = Join-Path $relayRoot 'Start-Relay.ps1'
        Executable = Join-Path $relayRoot 'wenzwork-relay-server.exe'
    },
    [pscustomobject]@{
        Name = 'device-agent'
        Root = $agentRoot
        Script = Join-Path $agentRoot 'Start-DeviceAgent.ps1'
        Executable = Join-Path $agentRoot 'device-agent.exe'
    }
)

$mutex = $null
$mutexAcquired = $false
$servicesStopped = $false
$deploymentStarted = $false
$backupReady = $false
$keepBackup = $false
$exitCode = 0
$missingBeforeDeployment = New-Object 'System.Collections.Generic.HashSet[string]' ([StringComparer]::OrdinalIgnoreCase)

function Write-Step {
    param([Parameter(Mandatory = $true)][string]$Message)
    Write-Host ("`n==> {0}" -f $Message) -ForegroundColor Cyan
}

function Assert-FileExists {
    param([Parameter(Mandatory = $true)][string]$Path)
    if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) {
        throw "Required file is missing: $Path"
    }
}

function Assert-DirectoryExists {
    param([Parameter(Mandatory = $true)][string]$Path)
    if (-not (Test-Path -LiteralPath $Path -PathType Container)) {
        throw "Required directory is missing: $Path"
    }
}

function Assert-CommandExists {
    param([Parameter(Mandatory = $true)][string]$Name)
    if ($null -eq (Get-Command $Name -ErrorAction SilentlyContinue)) {
        throw "Required command is not available on PATH: $Name"
    }
}

function Test-SamePath {
    param(
        [Parameter(Mandatory = $true)][string]$Left,
        [Parameter(Mandatory = $true)][string]$Right
    )
    return [StringComparer]::OrdinalIgnoreCase.Equals(
        [IO.Path]::GetFullPath($Left).TrimEnd('\'),
        [IO.Path]::GetFullPath($Right).TrimEnd('\')
    )
}

function Assert-SafeTemporaryPath {
    param([Parameter(Mandatory = $true)][string]$Path)
    $fullPath = [IO.Path]::GetFullPath($Path).TrimEnd('\')
    $fullTemporaryRoot = [IO.Path]::GetFullPath($temporaryRoot).TrimEnd('\')
    $prefix = $fullTemporaryRoot + [IO.Path]::DirectorySeparatorChar
    if (-not $fullPath.StartsWith($prefix, [StringComparison]::OrdinalIgnoreCase)) {
        throw "Refusing to remove a path outside the repository temporary directory: $fullPath"
    }
}

function Remove-TemporaryTree {
    param([Parameter(Mandatory = $true)][string]$Path)
    if (-not (Test-Path -LiteralPath $Path)) {
        return
    }
    Assert-SafeTemporaryPath -Path $Path
    Remove-Item -LiteralPath $Path -Recurse -Force
}

function Assert-SafeArtifactDirectory {
    param([Parameter(Mandatory = $true)][string]$Path)
    foreach ($relativePath in $artifactDirectories) {
        if (Test-SamePath -Left $Path -Right (Join-Path $packageRoot $relativePath)) {
            return
        }
    }
    throw "Refusing to replace an unexpected directory: $Path"
}

function Reset-ArtifactDirectory {
    param(
        [Parameter(Mandatory = $true)][string]$Source,
        [Parameter(Mandatory = $true)][string]$Destination
    )
    Assert-DirectoryExists -Path $Source
    Assert-SafeArtifactDirectory -Path $Destination
    if (Test-Path -LiteralPath $Destination) {
        Remove-Item -LiteralPath $Destination -Recurse -Force
    }
    $parent = Split-Path -Parent $Destination
    New-Item -ItemType Directory -Force -Path $parent | Out-Null
    Copy-Item -LiteralPath $Source -Destination $Destination -Recurse -Force
}

function Invoke-NativeCommand {
    param(
        [Parameter(Mandatory = $true)][string]$FilePath,
        [Parameter(Mandatory = $true)][string[]]$ArgumentList,
        [Parameter(Mandatory = $true)][string]$WorkingDirectory,
        [Parameter(Mandatory = $true)][string]$Description
    )

    Write-Host ("    {0}" -f $Description)
    Push-Location -LiteralPath $WorkingDirectory
    try {
        & $FilePath @ArgumentList
        $commandExitCode = $LASTEXITCODE
    }
    finally {
        Pop-Location
    }
    if ($commandExitCode -ne 0) {
        throw "$Description failed with exit code $commandExitCode."
    }
}

function Get-TargetProcesses {
    param([Parameter(Mandatory = $true)][string]$ExecutablePath)

    $targetPath = [IO.Path]::GetFullPath($ExecutablePath)
    $processName = [IO.Path]::GetFileNameWithoutExtension($ExecutablePath)
    foreach ($process in @(Get-Process -Name $processName -ErrorAction SilentlyContinue)) {
        try {
            if ($null -ne $process.Path -and (Test-SamePath -Left $process.Path -Right $targetPath)) {
                Write-Output $process
            }
        }
        catch {
            # Processes owned by another account may not expose Path. They are intentionally ignored.
        }
    }
}

function Stop-TargetService {
    param([Parameter(Mandatory = $true)]$Service)

    $processes = @(Get-TargetProcesses -ExecutablePath $Service.Executable)
    if ($processes.Count -eq 0) {
        Write-Host ("    {0}: already stopped" -f $Service.Name)
        return
    }

    foreach ($process in $processes) {
        Write-Host ("    {0}: stopping PID {1}" -f $Service.Name, $process.Id)
        Stop-Process -Id $process.Id -ErrorAction SilentlyContinue
    }

    $deadline = (Get-Date).AddSeconds(10)
    do {
        $remaining = @(Get-TargetProcesses -ExecutablePath $Service.Executable)
        if ($remaining.Count -eq 0) {
            return
        }
        Start-Sleep -Milliseconds 250
    } while ((Get-Date) -lt $deadline)

    foreach ($process in $remaining) {
        Stop-Process -Id $process.Id -Force -ErrorAction SilentlyContinue
    }
    Start-Sleep -Milliseconds 500
    if (@(Get-TargetProcesses -ExecutablePath $Service.Executable).Count -ne 0) {
        throw ("Could not stop {0}." -f $Service.Name)
    }
}

function Stop-AllTargetServices {
    Write-Step 'Stopping local services (device-agent -> relay -> management)'
    for ($index = $services.Count - 1; $index -ge 0; $index--) {
        Stop-TargetService -Service $services[$index]
    }
}

function Get-DotEnvValue {
    param(
        [Parameter(Mandatory = $true)][string]$Path,
        [Parameter(Mandatory = $true)][string]$Name
    )
    foreach ($line in Get-Content -LiteralPath $Path -Encoding UTF8) {
        if ($line -match ('^\s*' + [Regex]::Escape($Name) + '\s*=\s*(.*)\s*$')) {
            $value = $Matches[1].Trim()
            if ($value.Length -ge 2) {
                if (($value.StartsWith('"') -and $value.EndsWith('"')) -or
                    ($value.StartsWith("'") -and $value.EndsWith("'"))) {
                    $value = $value.Substring(1, $value.Length - 2)
                }
            }
            return $value
        }
    }
    return $null
}

function Get-ManagementHealthUrl {
    $environmentFile = Join-Path $managementRoot '.env'
    $httpAddress = Get-DotEnvValue -Path $environmentFile -Name 'HTTP_ADDR'
    if (-not [string]::IsNullOrWhiteSpace($httpAddress) -and $httpAddress -match ':(\d+)$') {
        return ("http://localhost:{0}/api/v1/health/live" -f $Matches[1])
    }

    $publicBaseUrl = Get-DotEnvValue -Path $environmentFile -Name 'PUBLIC_BASE_URL'
    if (-not [string]::IsNullOrWhiteSpace($publicBaseUrl)) {
        return ($publicBaseUrl.TrimEnd('/') + '/api/v1/health/live')
    }
    return 'http://localhost:8080/api/v1/health/live'
}

function Wait-ManagementHealth {
    param(
        [Parameter(Mandatory = $true)][string]$Url,
        [int]$TimeoutSeconds = 45
    )
    $deadline = (Get-Date).AddSeconds($TimeoutSeconds)
    do {
        try {
            $requestParameters = @{
                Uri = $Url
                Method = 'Get'
                TimeoutSec = 2
                ErrorAction = 'Stop'
            }
            if ($PSVersionTable.PSVersion.Major -lt 6) {
                $requestParameters['UseBasicParsing'] = $true
            }
            $response = Invoke-WebRequest @requestParameters
            if ([int]$response.StatusCode -eq 200) {
                Write-Host ("    management: health check passed ({0})" -f $Url)
                return
            }
        }
        catch {
            # The API can take a moment to become reachable after migrations.
        }
        Start-Sleep -Milliseconds 500
    } while ((Get-Date) -lt $deadline)
    throw "Management health check timed out: $Url"
}

function Start-TargetService {
    param([Parameter(Mandatory = $true)]$Service)

    Assert-FileExists -Path $Service.Script
    Assert-FileExists -Path $Service.Executable
    $logsDirectory = Join-Path $Service.Root 'logs'
    New-Item -ItemType Directory -Force -Path $logsDirectory | Out-Null
    $startedAt = Get-Date
    $logPath = Join-Path $logsDirectory ($startedAt.ToString('yyyyMMdd-HHmmss-fff') + '.log')
    ("[{0:yyyy-MM-dd HH:mm:ss.fff}] Starting {1}" -f $startedAt, $Service.Name) |
        Out-File -LiteralPath $logPath -Encoding utf8

    $escapedScript = $Service.Script.Replace("'", "''")
    $escapedLog = $logPath.Replace("'", "''")
    $backgroundCommand = @"
`$ErrorActionPreference = 'Stop'
try {
    & '$escapedScript' *>> '$escapedLog'
    if (`$null -eq `$LASTEXITCODE) { exit 0 }
    exit `$LASTEXITCODE
}
catch {
    (`$_.Exception.ToString()) | Out-File -LiteralPath '$escapedLog' -Append -Encoding utf8
    exit 1
}
"@
    $encodedCommand = [Convert]::ToBase64String([Text.Encoding]::Unicode.GetBytes($backgroundCommand))
    $shellExecutable = (Get-Process -Id $PID).Path
    $shellArguments = @('-NoLogo', '-NoProfile')
    if ([IO.Path]::GetFileName($shellExecutable) -ieq 'powershell.exe') {
        $shellArguments += @('-ExecutionPolicy', 'Bypass')
    }
    $shellArguments += @('-EncodedCommand', $encodedCommand)

    $launcher = Start-Process -FilePath $shellExecutable -ArgumentList $shellArguments `
        -WorkingDirectory $Service.Root -WindowStyle Hidden -PassThru

    $deadline = (Get-Date).AddSeconds(30)
    do {
        $targetProcesses = @(Get-TargetProcesses -ExecutablePath $Service.Executable)
        if ($targetProcesses.Count -gt 0) {
            break
        }
        $launcher.Refresh()
        if ($launcher.HasExited) {
            throw ("{0} exited during startup. See log: {1}" -f $Service.Name, $logPath)
        }
        Start-Sleep -Milliseconds 250
    } while ((Get-Date) -lt $deadline)

    if ($targetProcesses.Count -eq 0) {
        throw ("{0} did not start within 30 seconds. See log: {1}" -f $Service.Name, $logPath)
    }
    Start-Sleep -Milliseconds 1500
    if (@(Get-TargetProcesses -ExecutablePath $Service.Executable).Count -eq 0) {
        throw ("{0} stopped immediately after startup. See log: {1}" -f $Service.Name, $logPath)
    }
    Write-Host ("    {0}: started; log: {1}" -f $Service.Name, $logPath)
}

function Start-AllTargetServices {
    param([Parameter(Mandatory = $true)][string]$ManagementHealthUrl)

    Write-Step 'Starting local services (management -> relay -> device-agent)'
    Start-TargetService -Service $services[0]
    Wait-ManagementHealth -Url $ManagementHealthUrl
    Start-TargetService -Service $services[1]
    Start-TargetService -Service $services[2]
}

function Build-Artifacts {
    Write-Step 'Building Web, management, relay, and device-agent artifacts'
    New-Item -ItemType Directory -Force -Path $stageRoot | Out-Null

    Invoke-NativeCommand -FilePath 'corepack' -ArgumentList @('pnpm', '--dir', 'web', 'build') `
        -WorkingDirectory $repoRoot -Description 'Build Web'

    $webOutput = Join-Path $repoRoot 'web\dist'
    Assert-FileExists -Path (Join-Path $webOutput 'index.html')
    $stagedWeb = Join-Path $stageRoot 'management\web'
    New-Item -ItemType Directory -Force -Path (Split-Path -Parent $stagedWeb) | Out-Null
    Copy-Item -LiteralPath $webOutput -Destination $stagedWeb -Recurse -Force

    $sourceMigrations = Join-Path $repoRoot 'server\migrations'
    Assert-DirectoryExists -Path $sourceMigrations
    $stagedMigrations = Join-Path $stageRoot 'management\migrations'
    Copy-Item -LiteralPath $sourceMigrations -Destination $stagedMigrations -Recurse -Force

    $oldGoOS = [Environment]::GetEnvironmentVariable('GOOS', 'Process')
    $oldGoArch = [Environment]::GetEnvironmentVariable('GOARCH', 'Process')
    try {
        $env:GOOS = 'windows'
        $env:GOARCH = 'amd64'
        $goBuilds = @(
            @('management\bin\wenzwork-api.exe', './cmd/api', 'Build management API'),
            @('management\bin\wenzwork-admin.exe', './cmd/admin', 'Build management admin CLI'),
            @('management\bin\wenzwork-migrate.exe', './cmd/migrate', 'Build management migrate CLI'),
            @('relay\wenzwork-relay-server.exe', './cmd/relay-server', 'Build relay server'),
            @('relay\relayctl.exe', './cmd/relayctl', 'Build relayctl'),
            @('device-agent\device-agent.exe', './cmd/device-agent', 'Build device-agent')
        )
        foreach ($build in $goBuilds) {
            $outputPath = Join-Path $stageRoot $build[0]
            New-Item -ItemType Directory -Force -Path (Split-Path -Parent $outputPath) | Out-Null
            Invoke-NativeCommand -FilePath 'go' `
                -ArgumentList @('build', '-buildvcs=false', '-trimpath', '-o', $outputPath, $build[1]) `
                -WorkingDirectory (Join-Path $repoRoot 'server') -Description $build[2]
            Assert-FileExists -Path $outputPath
        }
    }
    finally {
        [Environment]::SetEnvironmentVariable('GOOS', $oldGoOS, 'Process')
        [Environment]::SetEnvironmentVariable('GOARCH', $oldGoArch, 'Process')
    }
}

function Backup-CurrentArtifacts {
    Write-Step 'Backing up current deployable artifacts'
    New-Item -ItemType Directory -Force -Path $backupRoot | Out-Null
    foreach ($relativePath in @($artifactFiles + $artifactDirectories)) {
        $source = Join-Path $packageRoot $relativePath
        if (-not (Test-Path -LiteralPath $source)) {
            $null = $missingBeforeDeployment.Add($relativePath)
            continue
        }
        $destination = Join-Path $backupRoot $relativePath
        New-Item -ItemType Directory -Force -Path (Split-Path -Parent $destination) | Out-Null
        if (Test-Path -LiteralPath $source -PathType Container) {
            Copy-Item -LiteralPath $source -Destination $destination -Recurse -Force
        }
        else {
            Copy-Item -LiteralPath $source -Destination $destination -Force
        }
    }
    $script:backupReady = $true
}

function Deploy-StagedArtifacts {
    Write-Step 'Deploying staged artifacts without touching local configuration or runtime data'
    $script:deploymentStarted = $true
    foreach ($relativePath in $artifactFiles) {
        $source = Join-Path $stageRoot $relativePath
        $destination = Join-Path $packageRoot $relativePath
        Assert-FileExists -Path $source
        New-Item -ItemType Directory -Force -Path (Split-Path -Parent $destination) | Out-Null
        Copy-Item -LiteralPath $source -Destination $destination -Force
    }
    foreach ($relativePath in $artifactDirectories) {
        Reset-ArtifactDirectory -Source (Join-Path $stageRoot $relativePath) `
            -Destination (Join-Path $packageRoot $relativePath)
    }
}

function Restore-PreviousArtifacts {
    if (-not $backupReady) {
        return
    }
    Write-Step 'Restoring the previous artifacts'
    foreach ($relativePath in $artifactFiles) {
        $destination = Join-Path $packageRoot $relativePath
        if ($missingBeforeDeployment.Contains($relativePath)) {
            if (Test-Path -LiteralPath $destination -PathType Leaf) {
                Remove-Item -LiteralPath $destination -Force
            }
            continue
        }
        $source = Join-Path $backupRoot $relativePath
        Assert-FileExists -Path $source
        Copy-Item -LiteralPath $source -Destination $destination -Force
    }
    foreach ($relativePath in $artifactDirectories) {
        $destination = Join-Path $packageRoot $relativePath
        if ($missingBeforeDeployment.Contains($relativePath)) {
            Assert-SafeArtifactDirectory -Path $destination
            if (Test-Path -LiteralPath $destination) {
                Remove-Item -LiteralPath $destination -Recurse -Force
            }
            continue
        }
        Reset-ArtifactDirectory -Source (Join-Path $backupRoot $relativePath) -Destination $destination
    }
}

function Invoke-Preflight {
    Write-Step 'Running preflight checks'
    Assert-DirectoryExists -Path $packageRoot
    Assert-DirectoryExists -Path (Join-Path $repoRoot 'server')
    Assert-DirectoryExists -Path (Join-Path $repoRoot 'web')
    Assert-FileExists -Path (Join-Path $managementRoot '.env')
    Assert-FileExists -Path (Join-Path $relayRoot 'relay.env')
    Assert-FileExists -Path (Join-Path $agentRoot 'agent.env')
    Assert-FileExists -Path (Join-Path $packageRoot 'Start-Dependencies.ps1')
    foreach ($service in $services) {
        Assert-FileExists -Path $service.Script
    }
    Assert-CommandExists -Name 'corepack'
    Assert-CommandExists -Name 'go'
    Write-Host '    Required directories, configuration files, and build tools are present.'
}

function Start-LocalDependencies {
    if ($SkipDependencies) {
        Write-Host '    Docker dependency startup was skipped by request.'
        return
    }

    Write-Step 'Checking Docker Desktop and starting local dependencies (best effort)'
    try {
        if ($null -eq (Get-Command 'docker' -ErrorAction SilentlyContinue)) {
            Write-Warning 'Docker is not available on PATH. Dependency startup was skipped; build and service restart will continue.'
            return
        }

        & docker info --format '{{.ServerVersion}}' *> $null
        $dockerInfoExitCode = $LASTEXITCODE
        if ($dockerInfoExitCode -ne 0) {
            Write-Warning 'Docker Desktop is not running or the Docker engine is unavailable. Dependency startup was skipped; build and service restart will continue.'
            return
        }

        & (Join-Path $packageRoot 'Start-Dependencies.ps1')
        $dependencyExitCode = $LASTEXITCODE
        if ($dependencyExitCode -ne 0) {
            Write-Warning ("Local dependency startup returned exit code {0}. Build and service restart will continue." -f $dependencyExitCode)
            return
        }

        Write-Host '    Docker dependencies are running.'
    }
    catch {
        Write-Warning ("Docker dependencies could not be started: {0} Build and service restart will continue." -f $_.Exception.Message)
    }
}

function Show-DryRunPlan {
    Write-Step 'Dry-run plan (no processes or files were changed)'
    if ($SkipDependencies) {
        Write-Host '    1. Leave Docker dependencies unchanged (-SkipDependencies).'
    }
    else {
        Write-Host '    1. Best-effort check/start Docker dependencies; continue on Docker failure.'
    }
    Write-Host '    2. Stop device-agent, relay, then management.'
    Write-Host '    3. Build Web and six Windows amd64 Go executables into .tmp.'
    Write-Host '    4. Back up and deploy only Web, migrations, and executable artifacts.'
    Write-Host '    5. Start management, relay, then device-agent with timestamped logs.'
    Write-Host ("    Package root: {0}" -f $packageRoot)
}

try {
    $mutex = New-Object Threading.Mutex($false, 'Local\WenzWorkLocalBuildRestart')
    $mutexAcquired = $mutex.WaitOne(0)
    if (-not $mutexAcquired) {
        throw 'Another local build/restart operation is already running.'
    }

    Invoke-Preflight
    $managementHealthUrl = Get-ManagementHealthUrl
    if ($DryRun) {
        Show-DryRunPlan
    }
    else {
        Start-LocalDependencies
        Stop-AllTargetServices
        $servicesStopped = $true
        Build-Artifacts
        Backup-CurrentArtifacts
        Deploy-StagedArtifacts
        Start-AllTargetServices -ManagementHealthUrl $managementHealthUrl
        $servicesStopped = $false
        Write-Step 'Build and restart completed successfully'
        Write-Host '    Existing .env files, secrets, runtime, workspace, and logs were preserved.'
    }
}
catch {
    $exitCode = 1
    Write-Host ("`nERROR: {0}" -f $_.Exception.Message) -ForegroundColor Red

    if ($servicesStopped -or $deploymentStarted) {
        try {
            Stop-AllTargetServices
            if ($deploymentStarted) {
                Restore-PreviousArtifacts
            }
            Start-AllTargetServices -ManagementHealthUrl (Get-ManagementHealthUrl)
            Write-Host '    Recovery completed: the previous service set was restarted.' -ForegroundColor Yellow
        }
        catch {
            $keepBackup = $backupReady
            Write-Host ("    Recovery also failed: {0}" -f $_.Exception.Message) -ForegroundColor Red
            if ($keepBackup) {
                Write-Host ("    Artifact backup was kept at: {0}" -f $backupRoot) -ForegroundColor Yellow
            }
        }
    }
}
finally {
    try {
        Remove-TemporaryTree -Path $stageRoot
        if (-not $keepBackup) {
            Remove-TemporaryTree -Path $backupRoot
        }
    }
    catch {
        Write-Warning ("Temporary directory cleanup failed: {0}" -f $_.Exception.Message)
    }

    if ($mutexAcquired -and $null -ne $mutex) {
        $mutex.ReleaseMutex()
    }
    if ($null -ne $mutex) {
        $mutex.Dispose()
    }
}

exit $exitCode
