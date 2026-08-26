#Requires -Version 7.0

[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$Version,
    [string]$OutputDirectory,
    [string]$Repository,
    [string]$Title,
    [string]$NotesFile,
    [switch]$Prerelease,
    [switch]$KeepDraft,
    [switch]$ReplaceAssets
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$scriptDirectory = [IO.Path]::GetFullPath($PSScriptRoot)
$repositoryRoot = [IO.Path]::GetFullPath((Join-Path $scriptDirectory '..'))
$publishedAuditScript = Join-Path $scriptDirectory 'Test-PublishedDeploymentRelease.ps1'
if ([string]::IsNullOrWhiteSpace($OutputDirectory)) { $OutputDirectory = Join-Path $repositoryRoot 'dist' }
$OutputDirectory = [IO.Path]::GetFullPath($OutputDirectory)
if ($Version -notmatch '^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$') { throw "Invalid release version: $Version" }
if (-not (Test-Path -LiteralPath $publishedAuditScript -PathType Leaf)) { throw "Published release audit script is missing: $publishedAuditScript" }

function Write-PublishLog {
    param([string]$Message)
    Write-Host "[deployment-publish] $Message"
}

function Assert-LastExitCode {
    param([string]$Operation)
    if ($LASTEXITCODE -ne 0) { throw "$Operation failed with exit code $LASTEXITCODE." }
}

function Read-DotEnvValue {
    param([string]$Path, [string]$Name)
    if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) { return '' }
    $value = ''
    foreach ($lineValue in [IO.File]::ReadAllLines($Path)) {
        $line = $lineValue.Trim()
        if ($line -match "^$([regex]::Escape($Name))\s*=(.*)$") {
            $value = $Matches[1].Trim().Trim('"').Trim("'")
        }
    }
    return $value
}

function Get-GitHubToken {
    foreach ($name in @('GITHUB_TOKEN', 'GH_TOKEN')) {
        $value = [Environment]::GetEnvironmentVariable($name)
        if (-not [string]::IsNullOrWhiteSpace($value)) { return $value }
    }
    $value = Read-DotEnvValue -Path (Join-Path $repositoryRoot '.env') -Name 'GITHUB_ACCESS_TOKEN'
    if (-not [string]::IsNullOrWhiteSpace($value)) { return $value }
    $credentialLines = @('protocol=https', 'host=github.com', '') | git credential fill
    Assert-LastExitCode -Operation 'Read git credential'
    foreach ($line in $credentialLines) {
        if ($line -match '^password=(.+)$' -and -not [string]::IsNullOrWhiteSpace($Matches[1])) {
            return $Matches[1]
        }
    }
    throw 'No GitHub token is available from GITHUB_TOKEN, GH_TOKEN, .env, or git credential manager.'
}

$publishedAuditParameters = @{ Version = $Version }
if (-not [string]::IsNullOrWhiteSpace($Repository)) { $publishedAuditParameters.Repository = $Repository }
if ($Prerelease) { $publishedAuditParameters.AllowPrerelease = $true }
$publishedAuditResult = @(& $publishedAuditScript @publishedAuditParameters)
if ($publishedAuditResult.Count -ne 1 -or $publishedAuditResult[0] -isnot [bool]) {
    throw 'Published release audit returned an invalid result.'
}
if ($publishedAuditResult[0]) { return }

function Invoke-GitHubJson {
    param(
        [ValidateSet('GET', 'POST', 'PATCH', 'DELETE')][string]$Method,
        [string]$Uri,
        [AllowNull()][object]$Body
    )
    $parameters = @{
        Method = $Method
        Uri = $Uri
        Headers = $script:jsonHeaders
    }
    if ($null -ne $Body) {
        $parameters.ContentType = 'application/json'
        $parameters.Body = $Body | ConvertTo-Json -Depth 8 -Compress
    }
    return Invoke-RestMethod @parameters
}

function Get-ReleaseByTag {
    param([string]$Tag)
    try {
        return Invoke-GitHubJson -Method GET -Uri "https://api.github.com/repos/$Repository/releases/tags/$Tag" -Body $null
    }
    catch {
        if ($null -ne $_.Exception.Response -and [int]$_.Exception.Response.StatusCode -eq 404) {
            $releaseResponse = Invoke-GitHubJson -Method GET -Uri "https://api.github.com/repos/$Repository/releases?per_page=100" -Body $null
            $allReleases = @()
            foreach ($item in $releaseResponse) { $allReleases += $item }
            $matches = @($allReleases | Where-Object {
                if ($null -eq $_) { return $false }
                $tagProperty = $_.PSObject.Properties['tag_name']
                if ($null -ne $tagProperty -and [string]$tagProperty.Value -ceq $Tag) { return $true }
                $draftProperty = $_.PSObject.Properties['draft']
                $nameProperty = $_.PSObject.Properties['name']
                $targetProperty = $_.PSObject.Properties['target_commitish']
                return $null -ne $draftProperty -and [bool]$draftProperty.Value -and
                    $null -ne $nameProperty -and [string]$nameProperty.Value -ceq $Title -and
                    $null -ne $targetProperty -and [string]$targetProperty.Value -ceq $commit
            })
            if ($matches.Count -gt 1) { throw "GitHub contains duplicate releases for tag $Tag." }
            if ($matches.Count -eq 1) { return $matches[0] }
            return $null
        }
        throw
    }
}

$releaseManifestPath = Join-Path $OutputDirectory 'DEPLOYMENT-RELEASE-MANIFEST.json'
$checksumsPath = Join-Path $OutputDirectory 'DEPLOYMENT-SHA256SUMS'
if (-not (Test-Path -LiteralPath $releaseManifestPath -PathType Leaf)) { throw 'DEPLOYMENT-RELEASE-MANIFEST.json is missing.' }
if (-not (Test-Path -LiteralPath $checksumsPath -PathType Leaf)) { throw 'DEPLOYMENT-SHA256SUMS is missing.' }
$manifest = Get-Content -Raw -LiteralPath $releaseManifestPath | ConvertFrom-Json
if ($manifest.version -cne $Version) { throw "Deployment output version is $($manifest.version), expected $Version." }
if ($manifest.dirty) { throw 'Refusing to publish packages built from a dirty worktree.' }
if ($manifest.packageCount -ne 18) { throw "A full release requires 18 deployment packages; found $($manifest.packageCount)." }
if ([string]::IsNullOrWhiteSpace($Repository)) { $Repository = [string]$manifest.repository }
if ($Repository -notmatch '^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$') { throw "Invalid GitHub repository: $Repository" }
if ($manifest.repository -cne $Repository) { throw "Manifest repository $($manifest.repository) does not match $Repository." }

& (Join-Path $scriptDirectory 'Test-DeploymentPackages.ps1') -OutputDirectory $OutputDirectory -Version $Version
Assert-LastExitCode -Operation 'Verify deployment packages before publication'

$commit = (& git -C $repositoryRoot rev-parse $manifest.commit).Trim()
Assert-LastExitCode -Operation 'Resolve release commit'
if ($commit -cne $manifest.commit) { throw 'Release manifest commit is not a complete git commit ID.' }
& git -C $repositoryRoot fetch origin main --tags
Assert-LastExitCode -Operation 'Fetch release refs'
& git -C $repositoryRoot merge-base --is-ancestor $commit origin/main
Assert-LastExitCode -Operation 'Verify release commit is on origin/main'

$tagCommit = ''
& git -C $repositoryRoot rev-parse -q --verify "refs/tags/$Version" *> $null
if ($LASTEXITCODE -eq 0) {
    $tagCommit = (& git -C $repositoryRoot rev-list -n 1 $Version).Trim()
    Assert-LastExitCode -Operation 'Resolve existing release tag'
    if ($tagCommit -cne $commit) { throw "Tag $Version points to $tagCommit, expected $commit." }
}
else {
    & git -C $repositoryRoot tag -a $Version $commit -m "WenzWork $Version"
    Assert-LastExitCode -Operation "Create tag $Version"
}
& git -C $repositoryRoot push origin "refs/tags/$Version"
Assert-LastExitCode -Operation "Push tag $Version"

$token = Get-GitHubToken
$script:jsonHeaders = @{
    Accept = 'application/vnd.github+json'
    Authorization = "Bearer $token"
    'X-GitHub-Api-Version' = '2022-11-28'
    'User-Agent' = 'wenzwork-deployment-publisher'
}
try {
    if ([string]::IsNullOrWhiteSpace($Title)) { $Title = "WenzWork $Version" }
    if (-not [string]::IsNullOrWhiteSpace($NotesFile)) {
        $notes = Get-Content -Raw -LiteralPath $NotesFile
    }
    else {
        $notes = @(
            "WenzWork $Version cross-platform deployment packages."
            ''
            '- Host, Relay, and Device Agent'
            '- Linux, Windows, and macOS (darwin)'
            '- amd64 and arm64'
            '- 18 SHA-256 verified archives'
            ''
            'Use DEPLOYMENT-SHA256SUMS before extraction. Each archive also contains PACKAGE-MANIFEST.json and native lifecycle scripts.'
        ) -join [Environment]::NewLine
    }

    $releaseWasPublished = $false
    $release = Get-ReleaseByTag -Tag $Version
    if ($null -eq $release) {
        $release = Invoke-GitHubJson -Method POST -Uri "https://api.github.com/repos/$Repository/releases" -Body @{
            tag_name = $Version
            target_commitish = $commit
            name = $Title
            body = $notes
            draft = $true
            prerelease = [bool]$Prerelease
        }
        Write-PublishLog -Message "Created draft release $Version."
    }
    else {
        $releaseWasPublished = -not [bool]$release.draft
        if ($release.target_commitish -ne $commit -and $release.tag_name -ne $Version) {
            throw 'Existing release target does not match the requested tag.'
        }
        if ($releaseWasPublished) {
            Write-PublishLog -Message "Auditing existing published release $Version."
        }
        else {
            $release = Invoke-GitHubJson -Method PATCH -Uri "https://api.github.com/repos/$Repository/releases/$($release.id)" -Body @{
                tag_name = $Version
                target_commitish = $commit
                name = $Title
                body = $notes
                draft = $true
                prerelease = [bool]$Prerelease
            }
            Write-PublishLog -Message "Using existing draft release $Version."
        }
    }

    $files = [Collections.Generic.List[string]]::new()
    foreach ($package in $manifest.packages) { $files.Add((Join-Path $OutputDirectory $package.name)) }
    $files.Add($checksumsPath)
    $files.Add($releaseManifestPath)
    $existingAssets = @($release.assets)
    foreach ($file in $files) {
        if (-not (Test-Path -LiteralPath $file -PathType Leaf)) { throw "Release asset is missing: $file" }
        $name = [IO.Path]::GetFileName($file)
        $hash = (Get-FileHash -LiteralPath $file -Algorithm SHA256).Hash.ToLowerInvariant()
        $size = (Get-Item -LiteralPath $file).Length
        $existing = @($existingAssets | Where-Object { $_.name -ceq $name })
        if ($existing.Count -gt 1) { throw "Release contains duplicate asset names: $name" }
        if ($existing.Count -eq 1) {
            $remoteDigest = [string]$existing[0].digest
            if ($existing[0].size -eq $size -and $remoteDigest -ceq "sha256:$hash") {
                Write-PublishLog -Message "Asset already verified: $name"
                continue
            }
            if ($releaseWasPublished) { throw "Published release asset $name differs; refusing to mutate it." }
            if (-not $ReplaceAssets) { throw "Release asset $name differs; pass -ReplaceAssets to replace it." }
            [void](Invoke-GitHubJson -Method DELETE -Uri "https://api.github.com/repos/$Repository/releases/assets/$($existing[0].id)" -Body $null)
        }
        if ($releaseWasPublished) { throw "Published release is missing $name; refusing to mutate it." }
        $encodedName = [Uri]::EscapeDataString($name)
        $uploadHeaders = @{
            Accept = 'application/vnd.github+json'
            Authorization = "Bearer $token"
            'X-GitHub-Api-Version' = '2022-11-28'
            'User-Agent' = 'wenzwork-deployment-publisher'
        }
        $uploaded = Invoke-RestMethod -Method Post -Headers $uploadHeaders -Uri "https://uploads.github.com/repos/$Repository/releases/$($release.id)/assets?name=$encodedName" -ContentType 'application/octet-stream' -InFile $file
        if ($uploaded.size -ne $size -or [string]$uploaded.digest -cne "sha256:$hash") {
            throw "GitHub did not confirm the size and SHA-256 digest for $name."
        }
        Write-PublishLog -Message "Uploaded and verified $name."
    }

    $release = Get-ReleaseByTag -Tag $Version
    $remoteAssets = @($release.assets)
    foreach ($file in $files) {
        $name = [IO.Path]::GetFileName($file)
        $hash = (Get-FileHash -LiteralPath $file -Algorithm SHA256).Hash.ToLowerInvariant()
        $size = (Get-Item -LiteralPath $file).Length
        $asset = @($remoteAssets | Where-Object { $_.name -ceq $name })
        if ($asset.Count -ne 1 -or $asset[0].size -ne $size -or [string]$asset[0].digest -cne "sha256:$hash") {
            throw "Remote release audit failed for $name."
        }
    }
    if ($releaseWasPublished) {
        Write-PublishLog -Message "Published release $($release.html_url) has $($files.Count) verified assets."
    }
    elseif (-not $KeepDraft) {
        $release = Invoke-GitHubJson -Method PATCH -Uri "https://api.github.com/repos/$Repository/releases/$($release.id)" -Body @{
            tag_name = $Version
            target_commitish = $commit
            draft = $false
            prerelease = [bool]$Prerelease
        }
        Write-PublishLog -Message "Published $($release.html_url) with $($files.Count) verified assets."
    }
    else {
        Write-PublishLog -Message "Draft is ready at $($release.html_url) with $($files.Count) verified assets."
    }
}
finally {
    $script:jsonHeaders.Authorization = ''
    $token = ''
}
