#Requires -Version 7.0

[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][ValidateNotNullOrEmpty()][string]$Version,
    [Parameter(Mandatory = $true)][ValidateSet('web', 'desktop', 'mobile')][string]$Project,
    [Parameter(Mandatory = $true)][string]$AssetManifestPath,
    [ValidateSet('stable', 'beta')][string]$Channel = 'stable',
    [string]$SoftwareName,
    [string]$Title,
    [string]$UpdateNotes,
    [string]$ReleaseBaseUrl,
    [string]$AccessKey,
    [switch]$Draft
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

function Get-ConfiguredValue {
    param([string]$Explicit, [string[]]$EnvironmentNames, [string]$Fallback = '')
    if (-not [string]::IsNullOrWhiteSpace($Explicit)) { return $Explicit.Trim() }
    foreach ($name in $EnvironmentNames) {
        $value = [Environment]::GetEnvironmentVariable($name)
        if (-not [string]::IsNullOrWhiteSpace($value)) { return $value.Trim() }
    }
    return $Fallback
}

function Get-ApiBaseUrl {
    param([string]$BaseUrl)
    $candidate = (Get-ConfiguredValue -Explicit $BaseUrl -EnvironmentNames @('WENZWORK_RELEASE_HOST', 'WENZWORK_RELEASE_URL', 'WENZWORK_RELEASE_BASE_URL', 'RELEASE_HOST') -Fallback 'http://localhost:8080').TrimEnd('/')
    $uri = $null
    if (-not [Uri]::TryCreate($candidate, [UriKind]::Absolute, [ref]$uri) -or $uri.Scheme -notin @('http', 'https') -or -not [string]::IsNullOrEmpty($uri.UserInfo) -or -not [string]::IsNullOrEmpty($uri.Query) -or -not [string]::IsNullOrEmpty($uri.Fragment)) {
        throw "ReleaseBaseUrl must be an absolute HTTP(S) URL: $candidate"
    }
    if ($candidate.EndsWith('/api/v1', [StringComparison]::OrdinalIgnoreCase)) { return $candidate }
    return "$candidate/api/v1"
}

function Get-ProblemDetail {
    param([string]$Body, [int]$StatusCode)
    try {
        $problem = $Body | ConvertFrom-Json -ErrorAction Stop
        if (-not [string]::IsNullOrWhiteSpace([string]$problem.detail)) { return [string]$problem.detail }
        if (-not [string]::IsNullOrWhiteSpace([string]$problem.title)) { return [string]$problem.title }
    }
    catch { }
    return "HTTP $StatusCode"
}

function Get-AssetDefinitions {
    param([string]$ManifestPath)
    $resolved = [IO.Path]::GetFullPath($ManifestPath)
    if (-not (Test-Path -LiteralPath $resolved -PathType Leaf)) { throw "Asset manifest is missing: $resolved" }
    $manifest = Get-Content -Raw -LiteralPath $resolved | ConvertFrom-Json
    $directory = Split-Path -Parent $resolved
    $definitions = [Collections.Generic.List[object]]::new()
    $isDeploymentManifest = $null -ne $manifest.PSObject.Properties['packages']
    $items = if ($isDeploymentManifest) { @($manifest.packages) } elseif ($null -ne $manifest.PSObject.Properties['assets']) { @($manifest.assets) } else { @() }
    if ($items.Count -eq 0) { throw 'Asset manifest does not contain any packages or assets.' }
    foreach ($item in $items) {
        $relativePath = if ($null -ne $item.PSObject.Properties['path'] -and -not [string]::IsNullOrWhiteSpace([string]$item.path)) { [string]$item.path } elseif ($null -ne $item.PSObject.Properties['name']) { [string]$item.name } else { '' }
        $assetPath = if ([IO.Path]::IsPathFullyQualified($relativePath)) { $relativePath } else { Join-Path $directory $relativePath }
        $assetPath = [IO.Path]::GetFullPath($assetPath)
        if (-not (Test-Path -LiteralPath $assetPath -PathType Leaf)) { throw "Release asset is missing: $assetPath" }
        $platform = switch ([string]$item.platform) {
            'darwin' { 'macos' }
            default { [string]$item.platform }
        }
        $architecture = switch ([string]$item.architecture) {
            'amd64' { 'x64' }
            default { [string]$item.architecture }
        }
        if ($platform -notin @('web', 'windows', 'macos', 'linux', 'android', 'ios')) { throw "Unsupported asset platform: $platform" }
        if ($architecture -notin @('x64', 'arm64', 'universal')) { throw "Unsupported asset architecture: $architecture" }
        $signatureStatus = if ($null -ne $item.PSObject.Properties['signatureStatus']) { [string]$item.signatureStatus } else { 'unknown' }
        if ([string]::IsNullOrWhiteSpace($signatureStatus)) { $signatureStatus = 'unknown' }
        if ($signatureStatus -notin @('unknown', 'unsigned', 'valid')) { throw "Unsupported signature status: $signatureStatus" }
        $definitions.Add([ordered]@{ path = $assetPath; platform = $platform; architecture = $architecture; signatureStatus = $signatureStatus })
    }
    if ($isDeploymentManifest) {
        foreach ($metadataName in @('DEPLOYMENT-RELEASE-MANIFEST.json', 'DEPLOYMENT-SHA256SUMS')) {
            $metadataPath = Join-Path $directory $metadataName
            if (-not (Test-Path -LiteralPath $metadataPath -PathType Leaf)) { throw "Deployment release metadata is missing: $metadataPath" }
            $definitions.Add([ordered]@{ path = [IO.Path]::GetFullPath($metadataPath); platform = 'web'; architecture = 'universal'; signatureStatus = 'unknown' })
        }
    }
    return @($definitions)
}

if ([string]::IsNullOrWhiteSpace($Version) -or $Version.Trim().Length -gt 50 -or $Version -match '[\x00-\x1f\x7f]') {
    throw 'Version is required and must contain at most 50 printable characters.'
}
$Version = $Version.Trim()
$resolvedAccessKey = Get-ConfiguredValue -Explicit $AccessKey -EnvironmentNames @('WENZWORK_RELEASE_ACCESS_KEY', 'RELEASE_ACCESS_KEY')
if ($resolvedAccessKey -notmatch '^release_[A-Za-z0-9_-]{43}$') {
    throw 'Release Access Key is required. Pass -AccessKey or set WENZWORK_RELEASE_ACCESS_KEY.'
}
$apiBaseUrl = Get-ApiBaseUrl -BaseUrl $ReleaseBaseUrl
$assetDefinitions = @(Get-AssetDefinitions -ManifestPath $AssetManifestPath)
$handler = [Net.Http.HttpClientHandler]::new()
$handler.AllowAutoRedirect = $false
$client = [Net.Http.HttpClient]::new($handler)
$client.Timeout = [Threading.Timeout]::InfiniteTimeSpan
$client.DefaultRequestHeaders.Authorization = [Net.Http.Headers.AuthenticationHeaderValue]::new('Bearer', $resolvedAccessKey)

try {
    $uploadedAssets = [Collections.Generic.List[object]]::new()
    $releaseCreated = $false
    foreach ($asset in $assetDefinitions) {
        $file = Get-Item -LiteralPath $asset.path
        $digest = (Get-FileHash -LiteralPath $file.FullName -Algorithm SHA256).Hash.ToLowerInvariant()
        $query = [Web.HttpUtility]::ParseQueryString('')
        $query['project'] = $Project
        $query['version'] = $Version
        $query['platform'] = $asset.platform
        $query['architecture'] = $asset.architecture
        $query['fileName'] = $file.Name
        $query['fileSizeBytes'] = [string]$file.Length
        $query['sha256'] = $digest
        $query['signatureStatus'] = $asset.signatureStatus
        $uri = "$apiBaseUrl/release-push/assets?$($query.ToString())"
        Write-Host "[release-push] Uploading $($file.Name) ($($asset.platform)/$($asset.architecture))..."
        $stream = [IO.File]::Open($file.FullName, [IO.FileMode]::Open, [IO.FileAccess]::Read, [IO.FileShare]::Read)
        try {
            $content = [Net.Http.StreamContent]::new($stream)
            $content.Headers.ContentType = [Net.Http.Headers.MediaTypeHeaderValue]::new('application/octet-stream')
            $content.Headers.ContentLength = $file.Length
            $response = $client.PostAsync($uri, $content).GetAwaiter().GetResult()
            $responseBody = $response.Content.ReadAsStringAsync().GetAwaiter().GetResult()
            try {
                if (-not $response.IsSuccessStatusCode) {
                    $detail = Get-ProblemDetail -Body $responseBody -StatusCode ([int]$response.StatusCode)
                    throw "Upload failed for $($file.Name): $detail"
                }
                $uploaded = $responseBody | ConvertFrom-Json
            }
            finally {
                $response.Dispose()
                $content.Dispose()
            }
        }
        finally {
            $stream.Dispose()
        }
        $uploadedAssets.Add([ordered]@{
            platform = [string]$uploaded.platform
            architecture = [string]$uploaded.architecture
            fileName = [string]$uploaded.fileName
            fileSizeBytes = [long]$uploaded.fileSizeBytes
            sha256 = [string]$uploaded.sha256
            signatureStatus = [string]$uploaded.signatureStatus
            source = 'local'
            objectKey = [string]$uploaded.objectKey
            downloadUrl = ''
        })
    }

    $request = [ordered]@{
        project = $Project
        version = $Version
        channel = $Channel
        publish = -not $Draft
        assets = @($uploadedAssets)
    }
    if (-not [string]::IsNullOrWhiteSpace($SoftwareName)) { $request.softwareName = $SoftwareName.Trim() }
    if (-not [string]::IsNullOrWhiteSpace($Title)) { $request.title = $Title.Trim() }
    if (-not [string]::IsNullOrWhiteSpace($UpdateNotes)) { $request.releaseNotes = $UpdateNotes.Trim() }
    $json = $request | ConvertTo-Json -Depth 8 -Compress
    $jsonContent = [Net.Http.StringContent]::new($json, [Text.UTF8Encoding]::new($false), 'application/json')
    try {
        $response = $client.PostAsync("$apiBaseUrl/release-push", $jsonContent).GetAwaiter().GetResult()
        $responseBody = $response.Content.ReadAsStringAsync().GetAwaiter().GetResult()
        try {
            if (-not $response.IsSuccessStatusCode) {
                $detail = Get-ProblemDetail -Body $responseBody -StatusCode ([int]$response.StatusCode)
                throw "Release finalize failed: $detail"
            }
            if ($response.StatusCode -notin @([Net.HttpStatusCode]::OK, [Net.HttpStatusCode]::Created)) {
                throw "Release finalize returned unexpected HTTP status $([int]$response.StatusCode)."
            }
            $releaseCreated = $response.StatusCode -eq [Net.HttpStatusCode]::Created
            $published = $responseBody | ConvertFrom-Json
        }
        finally {
            $response.Dispose()
        }
    }
    finally {
        $jsonContent.Dispose()
    }
    if ([string]$published.release.project -cne $Project -or [string]$published.release.version -cne $Version) {
        throw 'Release finalize returned a different project or version than requested.'
    }
    $operation = if ($releaseCreated) { 'created' } else { 'updated' }
    Write-Host "[release-push] $operation $($published.release.project) $($published.release.version) -> $($published.release.status), assets: $($published.release.assets.Count)."
}
finally {
    $client.Dispose()
    $handler.Dispose()
}
