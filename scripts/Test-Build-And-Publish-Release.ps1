#Requires -Version 7.0

[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

function Invoke-GitCommand {
    param(
        [Parameter(Mandatory = $true)][string]$Repository,
        [Parameter(Mandatory = $true)][string[]]$Arguments
    )

    $output = @(& git -C $Repository @Arguments 2>&1)
    $exitCode = $LASTEXITCODE
    if ($exitCode -ne 0) {
        throw "git $($Arguments -join ' ') failed with exit code $exitCode`n$($output -join [Environment]::NewLine)"
    }
    return $output
}

function Write-Utf8File {
    param(
        [Parameter(Mandatory = $true)][string]$Path,
        [Parameter(Mandatory = $true)][AllowEmptyString()][string]$Content
    )

    $parent = Split-Path -Parent $Path
    if (-not [string]::IsNullOrWhiteSpace($parent)) {
        $null = New-Item -ItemType Directory -Path $parent -Force
    }
    [IO.File]::WriteAllText($Path, $Content, [Text.UTF8Encoding]::new($false))
}

function Assert-Condition {
    param(
        [Parameter(Mandatory = $true)][bool]$Condition,
        [Parameter(Mandatory = $true)][string]$Message
    )

    if (-not $Condition) { throw $Message }
}

$entryScript = Join-Path $PSScriptRoot 'Build-And-Publish-Release.ps1'
if (-not (Test-Path -LiteralPath $entryScript -PathType Leaf)) {
    throw "Release entry script is missing: $entryScript"
}

$temporaryBase = [IO.Path]::GetFullPath([IO.Path]::GetTempPath())
$temporaryRoot = [IO.Path]::GetFullPath((Join-Path $temporaryBase "wenzwork-release-entry-test-$([Guid]::NewGuid().ToString('N'))"))
if (-not $temporaryRoot.StartsWith($temporaryBase, [StringComparison]::OrdinalIgnoreCase)) {
    throw "Refusing to create a release test fixture outside the temporary directory: $temporaryRoot"
}
$origin = Join-Path $temporaryRoot 'origin.git'
$repository = Join-Path $temporaryRoot 'repository'
$peer = Join-Path $temporaryRoot 'peer'

try {
    $null = New-Item -ItemType Directory -Path $temporaryRoot
    $null = Invoke-GitCommand -Repository $temporaryRoot -Arguments @('init', '--bare', '--initial-branch=main', $origin)
    $null = Invoke-GitCommand -Repository $temporaryRoot -Arguments @('init', '--initial-branch=main', $repository)
    $null = Invoke-GitCommand -Repository $repository -Arguments @('config', 'user.name', 'WenzWork Release Test')
    $null = Invoke-GitCommand -Repository $repository -Arguments @('config', 'user.email', 'release-test@wenzwork.invalid')
    $null = Invoke-GitCommand -Repository $repository -Arguments @('config', 'core.autocrlf', 'false')
    $null = Invoke-GitCommand -Repository $repository -Arguments @('config', 'core.quotepath', 'false')
    $null = Invoke-GitCommand -Repository $repository -Arguments @('remote', 'add', 'origin', $origin)

    $fixtureScripts = Join-Path $repository 'scripts'
    $null = New-Item -ItemType Directory -Path $fixtureScripts
    Copy-Item -LiteralPath $entryScript -Destination (Join-Path $fixtureScripts 'Build-And-Publish-Release.ps1')
    Write-Utf8File -Path (Join-Path $fixtureScripts 'Test-PublishedDeploymentRelease.ps1') -Content @'
param([Parameter(Mandatory = $true)][string]$Version)
$repositoryRoot = Split-Path -Parent $PSScriptRoot
return (Test-Path -LiteralPath (Join-Path $repositoryRoot ".git/published-$Version"))
'@
    Write-Utf8File -Path (Join-Path $fixtureScripts 'Build-DeploymentPackages.ps1') -Content @'
param([Parameter(Mandatory = $true)][string]$Version, [string]$OutputDirectory)
$repositoryRoot = Split-Path -Parent $PSScriptRoot
$headCommit = (& git -C $repositoryRoot rev-parse HEAD).Trim()
if ($LASTEXITCODE -ne 0) { throw 'Could not resolve the build commit.' }
$remoteCommit = (& git -C $repositoryRoot rev-parse origin/main).Trim()
if ($LASTEXITCODE -ne 0 -or $remoteCommit -cne $headCommit) { throw 'Build started before origin/main contained HEAD.' }
$changes = @(& git -C $repositoryRoot status --porcelain --untracked-files=normal)
if ($LASTEXITCODE -ne 0 -or $changes.Count -ne 0) { throw 'Build started from a dirty worktree.' }
[IO.File]::WriteAllText((Join-Path $repositoryRoot ".git/built-$Version"), $OutputDirectory)
'@
    Write-Utf8File -Path (Join-Path $fixtureScripts 'Publish-DeploymentRelease.ps1') -Content @'
param([Parameter(Mandatory = $true)][string]$Version, [string]$OutputDirectory)
$repositoryRoot = Split-Path -Parent $PSScriptRoot
if (-not (Test-Path -LiteralPath (Join-Path $repositoryRoot ".git/built-$Version"))) {
    throw 'Publish started before the build completed.'
}
[IO.File]::WriteAllText((Join-Path $repositoryRoot ".git/published-$Version"), $OutputDirectory)
'@
    Write-Utf8File -Path (Join-Path $repository 'README.md') -Content "initial`n"
    $null = Invoke-GitCommand -Repository $repository -Arguments @('add', '--all')
    $null = Invoke-GitCommand -Repository $repository -Arguments @('commit', '-m', 'initial')
    $null = Invoke-GitCommand -Repository $repository -Arguments @('push', '--set-upstream', 'origin', 'main')

    Write-Utf8File -Path (Join-Path $repository 'README.md') -Content "changed`n"
    Write-Utf8File -Path (Join-Path $repository 'docs/待发布任务.md') -Content "new file`n"
    $fixtureEntry = Join-Path $fixtureScripts 'Build-And-Publish-Release.ps1'
    & $fixtureEntry -Version '0.2.6'

    $headCommit = ([string](Invoke-GitCommand -Repository $repository -Arguments @('rev-parse', 'HEAD'))).Trim()
    $remoteCommit = ([string](Invoke-GitCommand -Repository $origin -Arguments @('rev-parse', 'refs/heads/main'))).Trim()
    Assert-Condition -Condition ($headCommit -ceq $remoteCommit) -Message 'The automatic release commit was not pushed to origin/main.'
    $commitMessage = ([string](Invoke-GitCommand -Repository $repository -Arguments @('log', '-1', '--format=%s'))).Trim()
    Assert-Condition -Condition ($commitMessage -ceq '发布 WenzWork v0.2.6') -Message "Unexpected automatic commit message: $commitMessage"
    $committedPaths = @(Invoke-GitCommand -Repository $repository -Arguments @('show', '--pretty=', '--name-only', 'HEAD'))
    Assert-Condition -Condition ($committedPaths -contains 'README.md') -Message 'The automatic commit omitted a tracked change.'
    Assert-Condition -Condition ($committedPaths -contains 'docs/待发布任务.md') -Message 'The automatic commit omitted an untracked file.'
    $worktreeStatus = @(Invoke-GitCommand -Repository $repository -Arguments @('status', '--porcelain', '--untracked-files=normal'))
    Assert-Condition -Condition ($worktreeStatus.Count -eq 0) -Message 'The successful release orchestration left source changes uncommitted.'
    Assert-Condition -Condition (Test-Path -LiteralPath (Join-Path $repository '.git/built-v0.2.6')) -Message 'The build step did not receive the normalized version.'
    Assert-Condition -Condition (Test-Path -LiteralPath (Join-Path $repository '.git/published-v0.2.6')) -Message 'The publish step was not invoked.'

    Remove-Item -LiteralPath (Join-Path $repository '.git/built-v0.2.6')
    Write-Utf8File -Path (Join-Path $repository 'after-release.txt') -Content "must remain uncommitted`n"
    $headBeforeAudit = ([string](Invoke-GitCommand -Repository $repository -Arguments @('rev-parse', 'HEAD'))).Trim()
    & $fixtureEntry -Version 'v0.2.6'
    $headAfterAudit = ([string](Invoke-GitCommand -Repository $repository -Arguments @('rev-parse', 'HEAD'))).Trim()
    Assert-Condition -Condition ($headAfterAudit -ceq $headBeforeAudit) -Message 'Auditing an existing release unexpectedly created a commit.'
    Assert-Condition -Condition (Test-Path -LiteralPath (Join-Path $repository 'after-release.txt')) -Message 'Auditing an existing release changed the worktree.'
    Assert-Condition -Condition (-not (Test-Path -LiteralPath (Join-Path $repository '.git/built-v0.2.6'))) -Message 'Auditing an existing release unexpectedly rebuilt packages.'
    Remove-Item -LiteralPath (Join-Path $repository 'after-release.txt')

    $null = Invoke-GitCommand -Repository $repository -Arguments @('switch', '-c', 'feature/release-test')
    Write-Utf8File -Path (Join-Path $repository 'feature-change.txt') -Content "feature`n"
    $featureHead = ([string](Invoke-GitCommand -Repository $repository -Arguments @('rev-parse', 'HEAD'))).Trim()
    $branchRejected = $false
    try {
        & $fixtureEntry -Version 'v0.2.7'
    }
    catch {
        $branchRejected = $_.Exception.Message -like '*must be created from the main branch*'
    }
    Assert-Condition -Condition $branchRejected -Message 'A new release from a non-main branch was not rejected.'
    $featureHeadAfter = ([string](Invoke-GitCommand -Repository $repository -Arguments @('rev-parse', 'HEAD'))).Trim()
    Assert-Condition -Condition ($featureHeadAfter -ceq $featureHead) -Message 'The branch guard created a commit before rejecting the release.'
    Assert-Condition -Condition (-not (Test-Path -LiteralPath (Join-Path $repository '.git/built-v0.2.7'))) -Message 'The branch guard ran the build step.'
    Remove-Item -LiteralPath (Join-Path $repository 'feature-change.txt')
    $null = Invoke-GitCommand -Repository $repository -Arguments @('switch', 'main')
    $null = Invoke-GitCommand -Repository $repository -Arguments @('branch', '-D', 'feature/release-test')

    $null = Invoke-GitCommand -Repository $temporaryRoot -Arguments @('clone', $origin, $peer)
    $null = Invoke-GitCommand -Repository $peer -Arguments @('config', 'user.name', 'WenzWork Release Peer')
    $null = Invoke-GitCommand -Repository $peer -Arguments @('config', 'user.email', 'release-peer@wenzwork.invalid')
    Write-Utf8File -Path (Join-Path $peer 'remote-change.txt') -Content "remote`n"
    $null = Invoke-GitCommand -Repository $peer -Arguments @('add', '--all')
    $null = Invoke-GitCommand -Repository $peer -Arguments @('commit', '-m', 'remote advance')
    $null = Invoke-GitCommand -Repository $peer -Arguments @('push', 'origin', 'main')
    Write-Utf8File -Path (Join-Path $repository 'local-change.txt') -Content "local`n"
    $localHead = ([string](Invoke-GitCommand -Repository $repository -Arguments @('rev-parse', 'HEAD'))).Trim()
    $behindRejected = $false
    try {
        & $fixtureEntry -Version 'v0.2.8'
    }
    catch {
        $behindRejected = $_.Exception.Message -like '*does not contain origin/main*'
    }
    Assert-Condition -Condition $behindRejected -Message 'A local main behind origin/main was not rejected before committing.'
    $localHeadAfter = ([string](Invoke-GitCommand -Repository $repository -Arguments @('rev-parse', 'HEAD'))).Trim()
    Assert-Condition -Condition ($localHeadAfter -ceq $localHead) -Message 'The remote ancestry guard created a local commit before rejecting the release.'
    Assert-Condition -Condition (-not (Test-Path -LiteralPath (Join-Path $repository '.git/built-v0.2.8'))) -Message 'The remote ancestry guard ran the build step.'

    Write-Host '[release-entry-test] PASS: automatic commit/push, immutable audit, branch guard, and remote ancestry guard.'
}
finally {
    if ($temporaryRoot.StartsWith($temporaryBase, [StringComparison]::OrdinalIgnoreCase) -and
        (Split-Path -Leaf $temporaryRoot).StartsWith('wenzwork-release-entry-test-', [StringComparison]::Ordinal) -and
        (Test-Path -LiteralPath $temporaryRoot)) {
        Remove-Item -LiteralPath $temporaryRoot -Recurse -Force
    }
}
