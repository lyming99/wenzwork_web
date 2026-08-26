#Requires -Version 7.0

[CmdletBinding()]
param(
    [Parameter(Mandatory = $true, Position = 0)]
    [Alias('v')]
    [ValidatePattern('^v?[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$')]
    [string]$Version
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

function Assert-LastExitCode {
    param([Parameter(Mandatory = $true)][string]$Operation)
    if ($LASTEXITCODE -ne 0) {
        throw "$Operation failed with exit code $LASTEXITCODE."
    }
}

if (-not $Version.StartsWith('v', [StringComparison]::OrdinalIgnoreCase)) {
    $Version = "v$Version"
}
elseif ($Version.StartsWith('V', [StringComparison]::Ordinal)) {
    $Version = "v$($Version.Substring(1))"
}

$scriptDirectory = [IO.Path]::GetFullPath($PSScriptRoot)
$repositoryRoot = [IO.Path]::GetFullPath((Join-Path $scriptDirectory '..'))
$outputDirectory = Join-Path $repositoryRoot 'dist'
$buildScript = Join-Path $scriptDirectory 'Build-DeploymentPackages.ps1'
$publishScript = Join-Path $scriptDirectory 'Publish-DeploymentRelease.ps1'
$publishedAuditScript = Join-Path $scriptDirectory 'Test-PublishedDeploymentRelease.ps1'

foreach ($requiredFile in @($buildScript, $publishScript, $publishedAuditScript)) {
    if (-not (Test-Path -LiteralPath $requiredFile -PathType Leaf)) {
        throw "Required release script is missing: $requiredFile"
    }
}

$publishedAuditResult = @(& $publishedAuditScript -Version $Version)
if ($publishedAuditResult.Count -ne 1 -or $publishedAuditResult[0] -isnot [bool]) {
    throw 'Published release audit returned an invalid result.'
}
if ($publishedAuditResult[0]) {
    Write-Host "[wenzwork-release] $Version is already published and passed the immutable remote asset audit."
    return
}

$branch = [string](& git -C $repositoryRoot symbolic-ref --quiet --short HEAD)
Assert-LastExitCode -Operation 'Resolve the current git branch'
$branch = $branch.Trim()
if ($branch -cne 'main') {
    throw "A new release must be created from the main branch; current branch is $branch."
}

$unmergedFiles = @(& git -C $repositoryRoot diff --name-only --diff-filter=U)
Assert-LastExitCode -Operation 'Inspect unmerged files'
if ($unmergedFiles.Count -gt 0) {
    throw "A new release cannot continue with unresolved merges: $($unmergedFiles -join ', ')"
}

& git -C $repositoryRoot fetch origin main --tags
Assert-LastExitCode -Operation 'Fetch origin/main and release tags'
& git -C $repositoryRoot merge-base --is-ancestor origin/main HEAD
if ($LASTEXITCODE -ne 0) {
    throw 'Local main does not contain origin/main. Pull or reconcile the remote changes before publishing.'
}

$pendingChanges = @(& git -C $repositoryRoot status --porcelain --untracked-files=normal)
Assert-LastExitCode -Operation 'Inspect release worktree'
if ($pendingChanges.Count -gt 0) {
    Write-Host "[wenzwork-release] Committing all tracked and untracked release changes for $Version..."
    & git -C $repositoryRoot add --all -- .
    Assert-LastExitCode -Operation 'Stage release changes'
    & git -C $repositoryRoot diff --cached --check
    Assert-LastExitCode -Operation 'Validate staged release changes'
    & git -C $repositoryRoot commit -m "发布 WenzWork $Version"
    Assert-LastExitCode -Operation 'Commit release changes'
}

$remainingChanges = @(& git -C $repositoryRoot status --porcelain --untracked-files=normal)
Assert-LastExitCode -Operation 'Verify release worktree after commit'
if ($remainingChanges.Count -gt 0) {
    throw 'The release commit did not leave a clean worktree. Review changes produced by commit hooks before retrying.'
}

$headCommit = (& git -C $repositoryRoot rev-parse HEAD).Trim()
Assert-LastExitCode -Operation 'Resolve release commit'
Write-Host "[wenzwork-release] Pushing release commit $headCommit to origin/main..."
& git -C $repositoryRoot push origin HEAD:refs/heads/main
Assert-LastExitCode -Operation 'Push release commit to origin/main'
& git -C $repositoryRoot fetch origin main
Assert-LastExitCode -Operation 'Refresh origin/main after push'
& git -C $repositoryRoot merge-base --is-ancestor $headCommit origin/main
Assert-LastExitCode -Operation 'Verify the release commit is present on origin/main'

Write-Host "[wenzwork-release] Building and validating $Version..."
& $buildScript -Version $Version -OutputDirectory $outputDirectory
Assert-LastExitCode -Operation 'Build deployment release packages'

Write-Host "[wenzwork-release] Publishing $Version..."
& $publishScript -Version $Version -OutputDirectory $outputDirectory
Assert-LastExitCode -Operation 'Publish deployment release'

$finalAuditResult = @(& $publishedAuditScript -Version $Version)
if ($finalAuditResult.Count -ne 1 -or $finalAuditResult[0] -isnot [bool] -or -not $finalAuditResult[0]) {
    throw 'Published release did not pass the final immutable remote asset audit.'
}
