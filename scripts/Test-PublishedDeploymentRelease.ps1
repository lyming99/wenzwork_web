#Requires -Version 7.0

[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [ValidatePattern('^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$')]
    [string]$Version,
    [string]$Repository,
    [switch]$AllowPrerelease
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$scriptDirectory = [IO.Path]::GetFullPath($PSScriptRoot)
$repositoryRoot = [IO.Path]::GetFullPath((Join-Path $scriptDirectory '..'))

function Write-AuditLog {
    param([Parameter(Mandatory = $true)][string]$Message)
    Write-Host "[deployment-publish] $Message"
}

function Assert-LastExitCode {
    param([Parameter(Mandatory = $true)][string]$Operation)
    if ($LASTEXITCODE -ne 0) { throw "$Operation failed with exit code $LASTEXITCODE." }
}

function Get-HttpStatusCode {
    param([Parameter(Mandatory = $true)][Management.Automation.ErrorRecord]$ErrorRecord)
    if ($null -eq $ErrorRecord.Exception.Response) { return $null }
    return [int]$ErrorRecord.Exception.Response.StatusCode
}

function Read-DotEnvValue {
    param([Parameter(Mandatory = $true)][string]$Path, [Parameter(Mandatory = $true)][string]$Name)
    if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) { return '' }
    foreach ($lineValue in [IO.File]::ReadAllLines($Path)) {
        $line = $lineValue.Trim()
        if ($line -match "^$([regex]::Escape($Name))\s*=(.*)$") {
            return $Matches[1].Trim().Trim('"').Trim("'")
        }
    }
    return ''
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

function Get-RepositoryName {
    $remote = (& git -C $repositoryRoot remote get-url origin).Trim()
    Assert-LastExitCode -Operation 'Read git origin'
    if ($remote -notmatch 'github\.com[/:]([^/]+)/([^/]+)$') {
        throw "Could not derive GitHub repository from origin: $remote"
    }
    $repositoryName = $Matches[2] -replace '\.git$', ''
    return "$($Matches[1])/$repositoryName"
}

function Get-RequiredProperty {
    param(
        [Parameter(Mandatory = $true)][object]$InputObject,
        [Parameter(Mandatory = $true)][string]$Name,
        [Parameter(Mandatory = $true)][string]$Description
    )
    $property = $InputObject.PSObject.Properties[$Name]
    if ($null -eq $property) { throw "$Description is missing $Name." }
    return $property.Value
}

function Get-AssetBytes {
    param([Parameter(Mandatory = $true)][object]$Asset)
    $downloadHeaders = $script:jsonHeaders.Clone()
    $downloadHeaders.Accept = 'application/octet-stream'
    try {
        $response = Invoke-WebRequest -Method Get -Uri ([string]$Asset.url) -Headers $downloadHeaders
        if ($response.Content -isnot [byte[]]) {
            throw "GitHub asset $($Asset.name) was not returned as binary content."
        }
        $bytes = [byte[]]$response.Content
        $sha256 = [Security.Cryptography.SHA256]::Create()
        try {
            $digest = ([BitConverter]::ToString($sha256.ComputeHash($bytes)) -replace '-', '').ToLowerInvariant()
        }
        finally {
            $sha256.Dispose()
        }
        if ($bytes.Length -ne [long]$Asset.size -or "sha256:$digest" -cne [string]$Asset.digest) {
            throw "Downloaded GitHub asset $($Asset.name) does not match its published size and SHA-256 digest."
        }
        return ,$bytes
    }
    finally {
        $downloadHeaders.Authorization = ''
    }
}

$originRepository = Get-RepositoryName
if ([string]::IsNullOrWhiteSpace($Repository)) { $Repository = $originRepository }
if ($Repository -notmatch '^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$') { throw "Invalid GitHub repository: $Repository" }
if ($Repository -cne $originRepository) { throw "Requested repository $Repository does not match git origin $originRepository." }
& git -C $repositoryRoot fetch origin main --tags
Assert-LastExitCode -Operation 'Fetch origin/main and release tags'

$token = Get-GitHubToken
$script:jsonHeaders = @{
    Accept = 'application/vnd.github+json'
    Authorization = "Bearer $token"
    'X-GitHub-Api-Version' = '2022-11-28'
    'User-Agent' = 'wenzwork-deployment-publisher'
}
try {
    try {
        $release = Invoke-RestMethod -Method Get -Uri "https://api.github.com/repos/$Repository/releases/tags/$Version" -Headers $script:jsonHeaders
    }
    catch {
        if ((Get-HttpStatusCode -ErrorRecord $_) -eq 404) {
            return $false
        }
        throw
    }

    if ([bool](Get-RequiredProperty -InputObject $release -Name draft -Description 'GitHub release')) {
        return $false
    }
    if ([string](Get-RequiredProperty -InputObject $release -Name tag_name -Description 'GitHub release') -cne $Version) {
        throw "GitHub returned a release for a different tag than $Version."
    }
    if ([bool](Get-RequiredProperty -InputObject $release -Name prerelease -Description 'GitHub release') -and -not $AllowPrerelease) {
        throw "Published GitHub release $Version is a Pre-release, not a formal Release."
    }

    $tagCommit = (& git -C $repositoryRoot rev-parse -q --verify "refs/tags/$Version`^{commit}").Trim()
    Assert-LastExitCode -Operation "Resolve release tag $Version"
    if ($tagCommit -notmatch '^[0-9a-f]{40}$') { throw "Tag $Version did not resolve to a complete commit ID." }
    & git -C $repositoryRoot merge-base --is-ancestor $tagCommit origin/main
    Assert-LastExitCode -Operation "Verify tag $Version is present on origin/main"
    $targetCommitish = [string](Get-RequiredProperty -InputObject $release -Name target_commitish -Description 'GitHub release')
    if ($targetCommitish -match '^[0-9a-fA-F]{40}$' -and $targetCommitish.ToLowerInvariant() -cne $tagCommit) {
        throw "Published release target $targetCommitish does not match tag commit $tagCommit."
    }

    Write-AuditLog -Message "Auditing existing published release $Version."
    $remoteAssets = @(Get-RequiredProperty -InputObject $release -Name assets -Description 'GitHub release')
    if ($remoteAssets.Count -lt 20) { throw "Published release must contain at least 20 deployment assets; found $($remoteAssets.Count)." }
    $assetsByName = [Collections.Generic.Dictionary[string, object]]::new([StringComparer]::Ordinal)
    foreach ($asset in $remoteAssets) {
        $name = [string](Get-RequiredProperty -InputObject $asset -Name name -Description 'GitHub release asset')
        if (-not $assetsByName.TryAdd($name, $asset)) { throw "Published release contains duplicate asset name: $name" }
    }

    foreach ($metadataName in @('DEPLOYMENT-RELEASE-MANIFEST.json', 'DEPLOYMENT-SHA256SUMS')) {
        if (-not $assetsByName.ContainsKey($metadataName)) { throw "Published release is missing $metadataName." }
    }
    $manifestBytes = Get-AssetBytes -Asset $assetsByName['DEPLOYMENT-RELEASE-MANIFEST.json']
    $checksumsBytes = Get-AssetBytes -Asset $assetsByName['DEPLOYMENT-SHA256SUMS']
    try {
        $manifest = [Text.Encoding]::UTF8.GetString($manifestBytes) | ConvertFrom-Json
    }
    catch {
        throw "Published DEPLOYMENT-RELEASE-MANIFEST.json is invalid JSON: $($_.Exception.Message)"
    }
    if ([int](Get-RequiredProperty -InputObject $manifest -Name schemaVersion -Description 'Release Manifest') -ne 1) {
        throw 'Published Release Manifest uses an unsupported schema.'
    }
    if ([string](Get-RequiredProperty -InputObject $manifest -Name version -Description 'Release Manifest') -cne $Version) {
        throw 'Published Release Manifest version does not match the requested tag.'
    }
    if ([string](Get-RequiredProperty -InputObject $manifest -Name repository -Description 'Release Manifest') -cne $Repository) {
        throw 'Published Release Manifest repository does not match git origin.'
    }
    if ([string](Get-RequiredProperty -InputObject $manifest -Name commit -Description 'Release Manifest') -cne $tagCommit) {
        throw 'Published Release Manifest commit does not match the release tag.'
    }
    if ([bool](Get-RequiredProperty -InputObject $manifest -Name dirty -Description 'Release Manifest')) {
        throw 'Published Release Manifest records a dirty build.'
    }
    $builtAtValue = Get-RequiredProperty -InputObject $manifest -Name builtAtUtc -Description 'Release Manifest'
    if ($builtAtValue -is [DateTime]) {
        if ($builtAtValue.Kind -ne [DateTimeKind]::Utc) { throw 'Published Release Manifest builtAtUtc is not UTC.' }
    }
    else {
        $builtAt = [DateTimeOffset]::MinValue
        if (-not [DateTimeOffset]::TryParse([string]$builtAtValue, [Globalization.CultureInfo]::InvariantCulture, [Globalization.DateTimeStyles]::RoundtripKind, [ref]$builtAt)) {
            throw 'Published Release Manifest has an invalid builtAtUtc value.'
        }
        if ($builtAt.Offset -ne [TimeSpan]::Zero) { throw 'Published Release Manifest builtAtUtc is not UTC.' }
    }
    $packages = @(Get-RequiredProperty -InputObject $manifest -Name packages -Description 'Release Manifest')
    if ([int](Get-RequiredProperty -InputObject $manifest -Name packageCount -Description 'Release Manifest') -ne 18 -or $packages.Count -ne 18) {
        throw "Published Release Manifest must describe exactly 18 packages."
    }

    $safeVersion = $Version -replace '[^A-Za-z0-9._-]', '-'
    $expectedPackages = [Collections.Generic.Dictionary[string, object]]::new([StringComparer]::Ordinal)
    foreach ($component in @('host', 'relay', 'device-agent')) {
        foreach ($platform in @('linux', 'windows', 'darwin')) {
            foreach ($architecture in @('amd64', 'arm64')) {
                $name = "wenzwork-$component-deployment-$safeVersion-$platform-$architecture.tar.gz"
                $expectedPackages.Add($name, [pscustomobject]@{
                    component = $component
                    platform = $platform
                    architecture = $architecture
                })
            }
        }
    }

    $checksumEntries = [Collections.Generic.Dictionary[string, string]]::new([StringComparer]::Ordinal)
    $checksumText = [Text.Encoding]::UTF8.GetString($checksumsBytes)
    foreach ($line in @($checksumText -split "`r?`n" | Where-Object { $_.Length -gt 0 })) {
        if ($line -notmatch '^([0-9a-f]{64})  ([^\\/]+\.tar\.gz)$') { throw "Published checksum line is invalid: $line" }
        if (-not $checksumEntries.TryAdd($Matches[2], $Matches[1])) { throw "Published checksums contain duplicate package: $($Matches[2])" }
    }
    if ($checksumEntries.Count -ne 18) { throw "Published checksums must contain exactly 18 packages." }

    $seenPackages = [Collections.Generic.HashSet[string]]::new([StringComparer]::Ordinal)
    foreach ($package in $packages) {
        $name = [string](Get-RequiredProperty -InputObject $package -Name name -Description 'Release Manifest package')
        if (-not $expectedPackages.ContainsKey($name) -or -not $seenPackages.Add($name)) {
            throw "Published Release Manifest contains an unexpected or duplicate package: $name"
        }
        $expected = $expectedPackages[$name]
        if ([string](Get-RequiredProperty -InputObject $package -Name component -Description "Release Manifest package $name") -cne $expected.component -or
            [string](Get-RequiredProperty -InputObject $package -Name platform -Description "Release Manifest package $name") -cne $expected.platform -or
            [string](Get-RequiredProperty -InputObject $package -Name architecture -Description "Release Manifest package $name") -cne $expected.architecture) {
            throw "Published Release Manifest target metadata is invalid for $name."
        }
        $size = [long](Get-RequiredProperty -InputObject $package -Name size -Description "Release Manifest package $name")
        $digest = [string](Get-RequiredProperty -InputObject $package -Name sha256 -Description "Release Manifest package $name")
        if (-not $assetsByName.ContainsKey($name) -or -not $checksumEntries.ContainsKey($name)) {
            throw "Published release metadata is missing package $name."
        }
        $remoteSize = [long](Get-RequiredProperty -InputObject $assetsByName[$name] -Name size -Description "GitHub release asset $name")
        $remoteDigest = [string](Get-RequiredProperty -InputObject $assetsByName[$name] -Name digest -Description "GitHub release asset $name")
        if ($digest -notmatch '^[0-9a-f]{64}$' -or $checksumEntries[$name] -cne $digest -or
            $remoteSize -ne $size -or $remoteDigest -cne "sha256:$digest") {
            throw "Published size or SHA-256 metadata differs for $name."
        }
    }
    if ($seenPackages.Count -ne $expectedPackages.Count) { throw 'Published Release Manifest is missing an expected deployment target.' }

    $releaseUrl = [string](Get-RequiredProperty -InputObject $release -Name html_url -Description 'GitHub release')
    Write-AuditLog -Message "Published release $releaseUrl has 20 verified deployment assets for tag commit $tagCommit."
    return $true
}
finally {
    $script:jsonHeaders.Authorization = ''
    $token = ''
}
