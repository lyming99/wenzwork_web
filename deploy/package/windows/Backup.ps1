#Requires -Version 5.1

[CmdletBinding()]
param(
    [Parameter(Position = 0)]
    [ValidateSet('backup', 'restore')]
    [string]$Command = 'backup',

    [Parameter(Position = 1)]
    [string]$Path,

    [switch]$ConfirmRestore
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$root = [IO.Path]::GetFullPath($PSScriptRoot)
Import-Module (Join-Path $root 'runtime\lib\PackageCommon.psm1') -Force

$postgresContainer = 'wenzwork-postgres'
$postgresDataDirectory = '/var/lib/postgresql/data'
$containerImage = ''
$helperId = ''
$temporary = ''
$rollbackArchive = ''
$operation = $Command
$operationCompleted = $false
$hostWasRunning = $false
$hostStopped = $false
$postgresWasRunning = $false
$postgresStopped = $false
$volumeMutated = $false
$preserveTemporary = $false

function Remove-HelperContainer {
    if (-not [string]::IsNullOrWhiteSpace($script:helperId)) {
        & docker rm -f $script:helperId 2>$null | Out-Null
        $script:helperId = ''
    }
}

function Test-PostgresRunning {
    $value = @(& docker inspect -f '{{.State.Running}}' $script:postgresContainer 2>$null)
    return $LASTEXITCODE -eq 0 -and ($value -join '').Trim() -eq 'true'
}

function Wait-PostgresReady {
    foreach ($attempt in 1..60) {
        & docker exec $script:postgresContainer pg_isready -U wenzwork -d wenzwork 2>$null | Out-Null
        if ($LASTEXITCODE -eq 0) { return $true }
        Start-Sleep -Seconds 1
    }
    return $false
}

function Stop-PackageRuntime {
    $pidFile = Join-Path $script:root 'runtime\pids\wenzwork.pid'
    if (Test-Path -LiteralPath $pidFile -PathType Leaf) {
        $pidValue = 0
        [void][int]::TryParse(([IO.File]::ReadAllText($pidFile).Trim()), [ref]$pidValue)
        if ($pidValue -gt 0 -and (Get-Process -Id $pidValue -ErrorAction SilentlyContinue)) {
            $script:hostWasRunning = $true
        }
        & (Join-Path $script:root 'Stop.ps1')
        $script:hostStopped = $true
    }
    if (Test-PostgresRunning) {
        $script:postgresWasRunning = $true
        Write-PackageLog -Message "Stopping $script:postgresContainer for a consistent volume snapshot..."
        & docker stop -t 60 $script:postgresContainer | Out-Null
        if ($LASTEXITCODE -ne 0) { throw "Could not stop $script:postgresContainer." }
        $script:postgresStopped = $true
    }
}

function Restore-PackageRuntimeState {
    if ($script:postgresWasRunning -and $script:postgresStopped) {
        & docker start $script:postgresContainer | Out-Null
        if ($LASTEXITCODE -ne 0 -or -not (Wait-PostgresReady)) { return $false }
        $script:postgresStopped = $false
    }
    if ($script:hostWasRunning -and $script:hostStopped) {
        try { & (Join-Path $script:root 'Start.ps1') -Background }
        catch { return $false }
        $script:hostStopped = $false
    }
    return $true
}

function New-VolumeArchive {
    param([Parameter(Mandatory = $true)][string]$Output)
    Remove-HelperContainer
    $arguments = @(
        'create', '--volumes-from', "${script:postgresContainer}:ro", $script:containerImage,
        'sh', '-ceu',
        'test -f /var/lib/postgresql/data/PG_VERSION; tar -czf /tmp/postgresql.tar.gz -C /var/lib/postgresql/data .'
    )
    $result = @(& docker @arguments)
    if ($LASTEXITCODE -ne 0 -or $result.Count -eq 0) { throw 'Could not create the volume backup helper container.' }
    $script:helperId = ([string]$result[-1]).Trim()
    & docker start -a $script:helperId | Out-Null
    if ($LASTEXITCODE -ne 0) { throw 'Could not archive the PostgreSQL container volume.' }
    & docker cp "${script:helperId}:/tmp/postgresql.tar.gz" $Output | Out-Null
    if ($LASTEXITCODE -ne 0) { throw 'Could not copy the container volume archive.' }
    Remove-HelperContainer
    if (-not (Test-Path -LiteralPath $Output -PathType Leaf) -or (Get-Item -LiteralPath $Output).Length -eq 0) {
        throw 'The PostgreSQL container volume archive is empty.'
    }
}

function Set-VolumeFromArchive {
    param([Parameter(Mandatory = $true)][string]$Archive)
    Remove-HelperContainer
    $command = @'
data=/var/lib/postgresql/data
find "$data" -mindepth 1 -maxdepth 1 -exec rm -rf -- {} \;
tar -xzf /tmp/postgresql.tar.gz -C "$data"
test -f "$data/PG_VERSION"
'@
    $result = @(& docker create --volumes-from $script:postgresContainer $script:containerImage sh -ceu $command)
    if ($LASTEXITCODE -ne 0 -or $result.Count -eq 0) { throw 'Could not create the volume restore helper container.' }
    $script:helperId = ([string]$result[-1]).Trim()
    & docker cp $Archive "${script:helperId}:/tmp/postgresql.tar.gz" | Out-Null
    if ($LASTEXITCODE -ne 0) { throw 'Could not copy the restore archive into the helper container.' }
    & docker start -a $script:helperId | Out-Null
    if ($LASTEXITCODE -ne 0) { throw 'Could not replace the PostgreSQL container volume.' }
    Remove-HelperContainer
}

function Get-VolumePostgresVersion {
    Remove-HelperContainer
    $result = @(& docker create --volumes-from "${script:postgresContainer}:ro" $script:containerImage sh -ceu 'cat /var/lib/postgresql/data/PG_VERSION')
    if ($LASTEXITCODE -ne 0 -or $result.Count -eq 0) { throw 'Could not inspect the PostgreSQL container volume.' }
    $script:helperId = ([string]$result[-1]).Trim()
    $version = @(& docker start -a $script:helperId)
    if ($LASTEXITCODE -ne 0) { throw 'Managed PostgreSQL volume has no PG_VERSION.' }
    Remove-HelperContainer
    $value = ($version -join '').Trim()
    if ($value -notmatch '^[0-9]+$') { throw 'Managed PostgreSQL volume has no valid PG_VERSION.' }
    return $value
}

function Get-ArchivePostgresVersion {
    param([Parameter(Mandatory = $true)][string]$Archive)
    $value = @(& tar.exe -xOzf $Archive './PG_VERSION' 2>$null)
    if ($LASTEXITCODE -ne 0) {
        $value = @(& tar.exe -xOzf $Archive 'PG_VERSION' 2>$null)
    }
    $version = ($value -join '').Trim()
    if ($LASTEXITCODE -ne 0 -or $version -notmatch '^[0-9]+$') {
        throw 'Backup archive has no valid PG_VERSION.'
    }
    return $version
}

function Assert-VolumeArchive {
    param([Parameter(Mandatory = $true)][string]$Archive)
    $item = Get-Item -LiteralPath $Archive -ErrorAction Stop
    if ($item.PSIsContainer -or $item.Attributes.HasFlag([IO.FileAttributes]::ReparsePoint) -or
        -not $item.Name.EndsWith('.tar.gz', [StringComparison]::OrdinalIgnoreCase)) {
        throw 'Backup archive must be a regular .tar.gz file, not a directory or reparse point.'
    }
    $entries = @(& tar.exe -tzf $item.FullName)
    if ($LASTEXITCODE -ne 0 -or $entries.Count -eq 0) { throw 'Backup archive is not a readable tar.gz file.' }
    foreach ($entryValue in $entries) {
        $entry = ([string]$entryValue) -replace '^[.][\/]', ''
        if ($entry.StartsWith('/') -or $entry.StartsWith('\') -or $entry -match '(^|[\/])[.][.]([\/]|$)') {
            throw "Unsafe backup archive entry: $entry"
        }
    }
    $verbose = @(& tar.exe -tvzf $item.FullName)
    if ($LASTEXITCODE -ne 0) { throw 'Could not inspect backup archive entry types.' }
    foreach ($line in $verbose) {
        if (-not [string]::IsNullOrEmpty($line) -and $line.Substring(0, 1) -notin @('-', 'd')) {
            throw 'Backup archive contains a link or special file.'
        }
    }
    $archiveVersion = Get-ArchivePostgresVersion -Archive $item.FullName
    $volumeVersion = Get-VolumePostgresVersion
    if ($archiveVersion -cne $volumeVersion) {
        throw "PostgreSQL major version mismatch: archive=$archiveVersion container=$volumeVersion"
    }
    return $item.FullName
}

function Assert-ManagedPostgres {
    $metadata = Assert-PackageTree -Root $script:root
    if ($metadata.WENZWORK_PACKAGE_COMPONENT -cne 'host' -or $metadata.WENZWORK_PACKAGE_PLATFORM -cne 'windows') {
        throw 'Backup.ps1 requires a Windows Host deployment package.'
    }
    Initialize-PackageRuntimeDirectories -Root $script:root
    [void](Import-PackageEnvironment -Path (Join-Path $script:root '.env'))
    Set-PackageComponentDefaults -Root $script:root -Metadata $metadata
    foreach ($name in @('GITHUB_ACCESS_TOKEN', 'GH_TOKEN', 'GITHUB_TOKEN')) {
        [Environment]::SetEnvironmentVariable($name, $null, 'Process')
    }
    if (-not (Get-Command docker -ErrorAction SilentlyContinue)) { throw 'docker is required.' }
    if (-not (Get-Command tar.exe -ErrorAction SilentlyContinue)) { throw 'tar.exe is required.' }
    $databaseUrl = [Environment]::GetEnvironmentVariable('DATABASE_URL', 'Process')
    if ($databaseUrl -notmatch '^postgres(?:ql)?://wenzwork:[^@]+@(?:127[.]0[.]0[.]1|localhost):54328/wenzwork(?:[?].*)?$') {
        throw 'DATABASE_URL does not point to the managed wenzwork-postgres container.'
    }
    & docker container inspect $script:postgresContainer 2>$null | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "Managed PostgreSQL container $script:postgresContainer does not exist." }
    $image = @(& docker inspect -f '{{.Config.Image}}' $script:postgresContainer)
    if ($LASTEXITCODE -ne 0 -or [string]::IsNullOrWhiteSpace(($image -join '').Trim())) {
        throw 'Could not resolve the PostgreSQL container image.'
    }
    $script:containerImage = ($image -join '').Trim()
    [void](Get-VolumePostgresVersion)
    return $metadata
}

function New-BackupFileName {
    $timestamp = [DateTime]::Now.ToString('yyyyMMddHHmmss')
    $suffix = [Guid]::NewGuid().ToString('N').Substring(0, 5)
    return "postgresql_$timestamp$suffix.tar.gz"
}

function Backup-ContainerVolume {
    param([AllowEmptyString()][string]$RequestedPath)
    if ([string]::IsNullOrWhiteSpace($RequestedPath)) {
        $RequestedPath = Join-Path $script:root "cache\backups\$(New-BackupFileName)"
    }
    elseif (-not [IO.Path]::IsPathRooted($RequestedPath)) {
        $RequestedPath = Join-Path (Get-Location).Path $RequestedPath
    }
    $output = [IO.Path]::GetFullPath($RequestedPath)
    if (-not $output.EndsWith('.tar.gz', [StringComparison]::OrdinalIgnoreCase)) {
        throw 'Backup output must use the .tar.gz extension.'
    }
    if (Test-Path -LiteralPath $output) { throw "Backup output already exists: $output" }
    $parent = Split-Path -Parent $output
    [void](New-Item -ItemType Directory -Path $parent -Force)
    $script:temporary = Join-Path $parent ".wenzwork-volume-backup.$([Guid]::NewGuid().ToString('N'))"
    [void](New-Item -ItemType Directory -Path $script:temporary)
    $staged = Join-Path $script:temporary 'postgresql.tar.gz'

    Stop-PackageRuntime
    Write-PackageLog -Message "Archiving the $script:postgresContainer data volume..."
    New-VolumeArchive -Output $staged
    [void](Assert-VolumeArchive -Archive $staged)
    Move-Item -LiteralPath $staged -Destination $output
    if (-not (Restore-PackageRuntimeState)) { throw 'Backup was created, but the original runtime could not be restarted.' }
    $script:operationCompleted = $true
    Write-PackageLog -Message "PostgreSQL container volume backup created: $output"
    Write-PackageLog -Message "SHA-256: $(Get-PackageFileSha256 -Path $output)"
}

function Restore-ContainerVolume {
    param([Parameter(Mandatory = $true)][string]$RequestedPath)
    if (-not $ConfirmRestore) { throw 'Restore requires -ConfirmRestore.' }
    if (-not [IO.Path]::IsPathRooted($RequestedPath)) {
        $RequestedPath = Join-Path (Get-Location).Path $RequestedPath
    }
    $archive = Assert-VolumeArchive -Archive ([IO.Path]::GetFullPath($RequestedPath))
    $archiveVersion = Get-ArchivePostgresVersion -Archive $archive
    $script:temporary = Join-Path $script:root "cache\backups\.wenzwork-volume-restore.$([Guid]::NewGuid().ToString('N'))"
    [void](New-Item -ItemType Directory -Path $script:temporary)
    $script:rollbackArchive = Join-Path $script:temporary 'postgresql-before-restore.tar.gz'

    Stop-PackageRuntime
    Write-PackageLog -Message 'Creating an automatic rollback snapshot of the current container volume...'
    New-VolumeArchive -Output $script:rollbackArchive
    [void](Assert-VolumeArchive -Archive $script:rollbackArchive)

    Write-PackageLog -Message "Replacing the $script:postgresContainer data volume from $([IO.Path]::GetFileName($archive))..."
    $script:volumeMutated = $true
    Set-VolumeFromArchive -Archive $archive
    & docker start $script:postgresContainer | Out-Null
    if ($LASTEXITCODE -ne 0) { throw 'Could not start the restored PostgreSQL container.' }
    $script:postgresStopped = $false
    if (-not (Wait-PostgresReady)) { throw 'Restored PostgreSQL container did not become ready.' }
    $restoredVersion = @(& docker exec $script:postgresContainer cat "$script:postgresDataDirectory/PG_VERSION")
    if ($LASTEXITCODE -ne 0 -or ($restoredVersion -join '').Trim() -cne $archiveVersion) {
        throw 'Restored PostgreSQL volume version verification failed.'
    }
    if ($script:hostWasRunning) {
        & (Join-Path $script:root 'Start.ps1') -Background
        $script:hostStopped = $false
    }
    if (-not $script:postgresWasRunning) {
        & docker stop -t 60 $script:postgresContainer | Out-Null
        if ($LASTEXITCODE -ne 0) { throw 'Could not return PostgreSQL to its previous stopped state.' }
        $script:postgresStopped = $true
    }
    $script:volumeMutated = $false
    $script:operationCompleted = $true
    Write-PackageLog -Message "PostgreSQL container volume restored from $archive"
}

function Restore-RollbackVolume {
    try {
        Write-PackageLog -Message 'Restore failed; restoring the original PostgreSQL container volume...'
        if (Test-PostgresRunning) {
            & docker stop -t 60 $script:postgresContainer | Out-Null
            if ($LASTEXITCODE -ne 0) { return $false }
        }
        $script:postgresStopped = $true
        Set-VolumeFromArchive -Archive $script:rollbackArchive
        $script:volumeMutated = $false
        if ($script:postgresWasRunning) {
            & docker start $script:postgresContainer | Out-Null
            if ($LASTEXITCODE -ne 0 -or -not (Wait-PostgresReady)) { return $false }
            $script:postgresStopped = $false
        }
        if ($script:hostWasRunning) {
            & (Join-Path $script:root 'Start.ps1') -Background
            $script:hostStopped = $false
        }
        return $true
    }
    catch { return $false }
}

try {
    [void](Assert-ManagedPostgres)
    switch ($Command) {
        'backup' { Backup-ContainerVolume -RequestedPath $Path }
        'restore' {
            if ([string]::IsNullOrWhiteSpace($Path)) { throw 'Restore requires a backup tar.gz path.' }
            Restore-ContainerVolume -RequestedPath $Path
        }
    }
}
catch {
    if (-not $operationCompleted) {
        if ($operation -eq 'restore' -and $volumeMutated -and
            -not [string]::IsNullOrWhiteSpace($rollbackArchive) -and
            (Test-Path -LiteralPath $rollbackArchive -PathType Leaf)) {
            if (-not (Restore-RollbackVolume)) {
                $preserveTemporary = $true
                Write-Warning "Automatic volume rollback failed; keep Host stopped and recover $rollbackArchive manually."
            }
        }
        elseif (-not (Restore-PackageRuntimeState)) {
            Write-Warning 'The original containers could not be returned to their previous state.'
        }
    }
    throw
}
finally {
    Remove-HelperContainer
    if (-not $preserveTemporary -and -not [string]::IsNullOrWhiteSpace($temporary)) {
        Remove-Item -LiteralPath $temporary -Recurse -Force -ErrorAction SilentlyContinue
    }
}
