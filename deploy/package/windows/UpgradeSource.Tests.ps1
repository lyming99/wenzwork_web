#Requires -Version 5.1

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

function Assert-True {
    param([Parameter(Mandatory = $true)][bool]$Condition, [Parameter(Mandatory = $true)][string]$Message)
    if (-not $Condition) { throw $Message }
}

function Assert-Equal {
    param(
        [AllowNull()]$Actual,
        [AllowNull()]$Expected,
        [Parameter(Mandatory = $true)][string]$Message
    )
    if ($Actual -cne $Expected) { throw "$Message Expected '$Expected', got '$Actual'." }
}

$modulePath = Join-Path $PSScriptRoot 'PackageCommon.psm1'
$upgradePath = Join-Path $PSScriptRoot 'Upgrade.ps1'
$testRoot = Join-Path ([IO.Path]::GetTempPath()) ('wenzwork-windows-upgrade-source-test-' + [Guid]::NewGuid().ToString('N'))
$utf8 = [Text.UTF8Encoding]::new($false)

function New-ReleaseFixture {
    param([Parameter(Mandatory = $true)][string]$Version)
    $safeVersion = $Version -replace '[^A-Za-z0-9._-]', '-'
    $archiveName = "wenzwork-device-agent-deployment-$safeVersion-windows-amd64.tar.gz"
    $fixtureDirectory = Join-Path $testRoot $safeVersion
    [void](New-Item -ItemType Directory -Path $fixtureDirectory -Force)
    $archivePath = Join-Path $fixtureDirectory $archiveName
    [IO.File]::WriteAllBytes($archivePath, [Text.Encoding]::UTF8.GetBytes("fixture-$Version"))
    $sha256 = (Get-FileHash -LiteralPath $archivePath -Algorithm SHA256).Hash.ToLowerInvariant()
    $checksumsPath = Join-Path $fixtureDirectory 'DEPLOYMENT-SHA256SUMS'
    [IO.File]::WriteAllText($checksumsPath, "$sha256  $archiveName`r`n", $utf8)
    $release = [ordered]@{
        id = '00000000-0000-0000-0000-000000000001'
        project = 'web'
        version = $Version
        channel = 'stable'
        title = 'fixture'
        summary = ''
        releaseNotes = ''
        publishedAt = '2026-08-25T12:00:00Z'
        assets = @([ordered]@{
            id = '00000000-0000-0000-0000-000000000002'
            platform = 'windows'
            architecture = 'x64'
            fileName = $archiveName
            fileSizeBytes = (Get-Item -LiteralPath $archivePath).Length
            sha256 = $sha256
            signatureStatus = 'valid'
            downloadUrl = "/downloads/$archiveName"
        })
    }
    $catalogPath = Join-Path $fixtureDirectory 'catalog.json'
    [IO.File]::WriteAllText($catalogPath, ($release | ConvertTo-Json -Depth 8 -Compress), $utf8)
    $catalogListPath = Join-Path $fixtureDirectory 'catalog-list.json'
    [IO.File]::WriteAllText($catalogListPath, ([ordered]@{ items = @($release) } | ConvertTo-Json -Depth 8 -Compress), $utf8)
    $githubReleasePath = Join-Path $fixtureDirectory 'github-release.json'
    $githubRelease = [ordered]@{
        tag_name = $Version
        assets = @(
            [ordered]@{
                name = $archiveName
                url = 'https://api.github.com/repos/example/wenzwork/releases/assets/101'
            },
            [ordered]@{
                name = 'DEPLOYMENT-SHA256SUMS'
                url = 'https://api.github.com/repos/example/wenzwork/releases/assets/102'
            }
        )
    }
    [IO.File]::WriteAllText($githubReleasePath, ($githubRelease | ConvertTo-Json -Depth 8 -Compress), $utf8)
    return [pscustomobject]@{
        Version = $Version
        ArchiveName = $archiveName
        ArchivePath = $archivePath
        Sha256 = $sha256
        ChecksumsPath = $checksumsPath
        CatalogPath = $catalogPath
        CatalogListPath = $catalogListPath
        GitHubReleasePath = $githubReleasePath
    }
}

function New-FakeDownloadCommand {
    param(
        [Parameter(Mandatory = $true)][ValidateSet('primary', 'secondary', 'public', 'api', 'api404', 'version', 'missing')][string]$Mode,
        [Parameter(Mandatory = $true)]$Fixture,
        [Parameter(Mandatory = $true)][AllowEmptyCollection()][Collections.Generic.List[object]]$Requests
    )
    $selectedMode = $Mode
    $selectedFixture = $Fixture
    $requestLog = $Requests
    return {
        param(
            [Parameter(Mandatory = $true)][string]$Uri,
            [Parameter(Mandatory = $true)][string]$Destination,
            [AllowEmptyString()][string]$Token = '',
            [string]$Accept = 'application/octet-stream'
        )
        $requestLog.Add([pscustomobject]@{ Uri = $Uri; Token = $Token; Accept = $Accept })
        $source = ''
        if ($selectedMode -ceq 'primary') {
            if ($Uri.StartsWith('https://work.wenzflow.com/api/', [StringComparison]::Ordinal)) { $source = $selectedFixture.CatalogPath }
            elseif ($Uri -ceq "https://work.wenzflow.com/downloads/$($selectedFixture.ArchiveName)") { $source = $selectedFixture.ArchivePath }
        }
        elseif ($selectedMode -ceq 'secondary') {
            if ($Uri.StartsWith('https://wenzwork.com/api/', [StringComparison]::Ordinal)) { $source = $selectedFixture.CatalogPath }
            elseif ($Uri -ceq "https://wenzwork.com/downloads/$($selectedFixture.ArchiveName)") { $source = $selectedFixture.ArchivePath }
        }
        elseif ($selectedMode -ceq 'version') {
            if ($Uri.StartsWith('https://work.wenzflow.com/api/v1/releases?', [StringComparison]::Ordinal)) { $source = $selectedFixture.CatalogListPath }
            elseif ($Uri -ceq "https://work.wenzflow.com/downloads/$($selectedFixture.ArchiveName)") { $source = $selectedFixture.ArchivePath }
        }
        elseif ($selectedMode -ceq 'api') {
            if ($Uri -ceq 'https://api.github.com/repos/example/wenzwork/releases/latest') { $source = $selectedFixture.GitHubReleasePath }
            elseif ($Uri -ceq 'https://api.github.com/repos/example/wenzwork/releases/assets/101') { $source = $selectedFixture.ArchivePath }
            elseif ($Uri -ceq 'https://api.github.com/repos/example/wenzwork/releases/assets/102') { $source = $selectedFixture.ChecksumsPath }
        }
        if ($selectedMode -in @('public', 'api404')) {
            if ($Uri -ceq 'https://github.com/example/wenzwork/releases/latest/download/DEPLOYMENT-SHA256SUMS') { $source = $selectedFixture.ChecksumsPath }
            elseif ($Uri -ceq "https://github.com/example/wenzwork/releases/latest/download/$($selectedFixture.ArchiveName)") { $source = $selectedFixture.ArchivePath }
        }
        if ([string]::IsNullOrWhiteSpace($source)) { return $false }
        Copy-Item -LiteralPath $source -Destination $Destination -Force
        return $true
    }.GetNewClosure()
}

function Invoke-TestResolution {
    param(
        [Parameter(Mandatory = $true)][string]$Name,
        [Parameter(Mandatory = $true)][string]$Mode,
        [Parameter(Mandatory = $true)]$Fixture,
        [Parameter(Mandatory = $true)][Collections.IDictionary]$Environment,
        [AllowEmptyString()][string]$Version = ''
    )
    $temporary = Join-Path $testRoot $Name
    [void](New-Item -ItemType Directory -Path $temporary -Force)
    $requests = [Collections.Generic.List[object]]::new()
    $downloadCommand = New-FakeDownloadCommand -Mode $Mode -Fixture $Fixture -Requests $requests
    $download = Resolve-PackageUpgradeDownload -Metadata $script:metadata -Environment $Environment -Version $Version `
        -TemporaryDirectory $temporary -DownloadCommand $downloadCommand
    return [pscustomobject]@{ Download = $download; Requests = $requests }
}

[void](New-Item -ItemType Directory -Path $testRoot)
try {
    Import-Module $modulePath -Force
    $script:metadata = [ordered]@{
        WENZWORK_PACKAGE_COMPONENT = 'device-agent'
        WENZWORK_PACKAGE_PLATFORM = 'windows'
        WENZWORK_PACKAGE_ARCHITECTURE = 'amd64'
        WENZWORK_PACKAGE_VERSION = 'v1.0.0'
        WENZWORK_PACKAGE_ASSET_BASENAME = 'wenzwork-device-agent-deployment'
        WENZWORK_PACKAGE_CHECKSUM_ASSET = 'DEPLOYMENT-SHA256SUMS'
        WENZWORK_GITHUB_REPOSITORY = 'example/wenzwork'
    }
    $anonymousEnvironment = [ordered]@{
        GITHUB_RELEASE_REPOSITORY = 'example/wenzwork'
        GITHUB_ACCESS_TOKEN = ''
    }

    $primaryFixture = New-ReleaseFixture -Version 'v1.1.0'
    $primary = Invoke-TestResolution -Name 'primary-result' -Mode primary -Fixture $primaryFixture -Environment $anonymousEnvironment
    Assert-Equal $primary.Download.Source 'https://work.wenzflow.com' 'The primary official source was not selected.'
    Assert-Equal (Get-PackageFileSha256 -Path $primary.Download.PackageFile) $primaryFixture.Sha256 'The primary source archive changed during download.'
    Assert-True ($primary.Requests.Count -eq 2) 'A fallback source was contacted after the primary source succeeded.'
    Assert-True $primary.Requests[0].Uri.Contains('/api/v1/releases/latest?project=web&channel=stable&platform=windows&architecture=x64') 'The primary catalog query did not map amd64 to x64.'

    $secondaryFixture = New-ReleaseFixture -Version 'v1.2.0'
    $secondary = Invoke-TestResolution -Name 'secondary-result' -Mode secondary -Fixture $secondaryFixture -Environment $anonymousEnvironment
    Assert-Equal $secondary.Download.Source 'https://wenzwork.com' 'The secondary official source was not selected.'
    Assert-True ($secondary.Requests[0].Uri.StartsWith('https://work.wenzflow.com/', [StringComparison]::Ordinal)) 'The secondary source was attempted before the primary source.'
    Assert-True ($secondary.Requests[1].Uri.StartsWith('https://wenzwork.com/', [StringComparison]::Ordinal)) 'The secondary source was not attempted after the primary source failed.'
    Assert-True (-not (@($secondary.Requests.Uri | Where-Object { $_ -like 'https://github.com/*' }).Count)) 'GitHub was contacted after the secondary source succeeded.'

    $publicFixture = New-ReleaseFixture -Version 'v1.3.0'
    $public = Invoke-TestResolution -Name 'public-result' -Mode public -Fixture $publicFixture -Environment $anonymousEnvironment
    Assert-Equal $public.Download.Source 'github-public' 'The public GitHub Release fallback was not selected.'
    Assert-True (-not (@($public.Requests.Uri | Where-Object { $_ -like 'https://api.github.com/*' }).Count)) 'An anonymous GitHub API request was made instead of using public Release downloads.'
    Assert-True (@($public.Requests.Uri | Where-Object { $_ -like 'https://github.com/*/releases/latest/download/*' }).Count -eq 2) 'The public GitHub checksum and archive were not both downloaded.'

    $tokenEnvironment = [ordered]@{
        GITHUB_RELEASE_REPOSITORY = 'example/wenzwork'
        GITHUB_ACCESS_TOKEN = 'github_pat_fixture_read_only'
    }
    $apiFixture = New-ReleaseFixture -Version 'v1.4.0'
    $api = Invoke-TestResolution -Name 'api-result' -Mode api -Fixture $apiFixture -Environment $tokenEnvironment
    Assert-Equal $api.Download.Source 'github-api' 'The authenticated GitHub API source was not selected.'
    $authenticatedRequests = @($api.Requests | Where-Object { $_.Uri -like 'https://api.github.com/*' })
    Assert-True ($authenticatedRequests.Count -eq 3) 'The authenticated GitHub metadata and two assets were not requested.'
    Assert-True (-not @($authenticatedRequests | Where-Object { $_.Token -cne 'github_pat_fixture_read_only' }).Count) 'The GitHub token was omitted from an authenticated API request.'

    $api404Fixture = New-ReleaseFixture -Version 'v1.5.0'
    $api404 = Invoke-TestResolution -Name 'api-404-result' -Mode api404 -Fixture $api404Fixture -Environment $tokenEnvironment
    Assert-Equal $api404.Download.Source 'github-public' 'A GitHub API 404 did not fall back to the public Release page.'
    $apiRequestIndex = -1
    $publicRequestIndex = -1
    for ($index = 0; $index -lt $api404.Requests.Count; $index++) {
        if ($api404.Requests[$index].Uri -like 'https://api.github.com/*' -and $apiRequestIndex -lt 0) { $apiRequestIndex = $index }
        if ($api404.Requests[$index].Uri -like 'https://github.com/*' -and $publicRequestIndex -lt 0) { $publicRequestIndex = $index }
    }
    Assert-True ($apiRequestIndex -ge 0 -and $publicRequestIndex -gt $apiRequestIndex) 'The public GitHub fallback did not follow the failed API request.'

    $versionFixture = New-ReleaseFixture -Version 'v1.6.0'
    $versioned = Invoke-TestResolution -Name 'version-result' -Mode version -Fixture $versionFixture -Environment $anonymousEnvironment -Version '1.6.0'
    Assert-Equal $versioned.Download.Source 'https://work.wenzflow.com' 'A version-specific official package was not selected.'
    Assert-True $versioned.Requests[0].Uri.Contains('/api/v1/releases?project=web&channel=stable&platform=windows&architecture=x64&limit=50') 'A version-specific lookup did not use the catalog list endpoint.'
    Assert-Equal ([IO.Path]::GetFileName($versioned.Download.PackageFile)) $versionFixture.ArchiveName 'A version without v did not resolve the v-prefixed package.'

    $missingFixture = New-ReleaseFixture -Version 'v1.7.0'
    $missingDirectory = Join-Path $testRoot 'missing-result'
    [void](New-Item -ItemType Directory -Path $missingDirectory)
    $missingRequests = [Collections.Generic.List[object]]::new()
    $missingCommand = New-FakeDownloadCommand -Mode missing -Fixture $missingFixture -Requests $missingRequests
    $missingMessage = ''
    try {
        [void](Resolve-PackageUpgradeDownload -Metadata $script:metadata -Environment $anonymousEnvironment `
            -TemporaryDirectory $missingDirectory -DownloadCommand $missingCommand)
    }
    catch {
        $missingMessage = $_.Exception.Message
    }
    Assert-True $missingMessage.Contains('Upgrade package was not found at work.wenzflow.com, wenzwork.com, or github.com.') 'All-source failure did not provide an actionable summary.'
    Assert-True (-not $missingMessage.Contains('Invoke-RestMethod')) 'All-source failure leaked the old Invoke-RestMethod exception.'

    Assert-True (Test-PackageRemoteUri -Uri 'https://releases.example.test') 'HTTPS release URLs were rejected.'
    Assert-True (Test-PackageRemoteUri -Uri 'http://127.0.0.1:18080') 'Loopback HTTP release URLs were rejected.'
    Assert-True (-not (Test-PackageRemoteUri -Uri 'http://releases.example.test')) 'Non-loopback HTTP release URLs were accepted.'
    Assert-True (-not (Test-PackageRemoteUri -Uri 'https://token@releases.example.test')) 'Credential-bearing release URLs were accepted.'
    $upgradeText = [IO.File]::ReadAllText($upgradePath)
    Assert-True $upgradeText.Contains('Resolve-PackageUpgradeDownload') 'Upgrade.ps1 does not use the multi-source resolver.'
    Assert-True (-not $upgradeText.Contains('Invoke-RestMethod')) 'Upgrade.ps1 still directly calls the GitHub latest-release API.'

    Write-Host 'Windows upgrade source priority and GitHub 404 fallback tests passed.'
}
finally {
    Remove-Module PackageCommon -Force -ErrorAction SilentlyContinue
    $resolvedTestRoot = [IO.Path]::GetFullPath($testRoot)
    $resolvedTemporaryRoot = [IO.Path]::GetFullPath([IO.Path]::GetTempPath())
    if (-not $resolvedTestRoot.StartsWith($resolvedTemporaryRoot, [StringComparison]::OrdinalIgnoreCase)) {
        throw "Unsafe Windows upgrade source test cleanup path: $resolvedTestRoot"
    }
    Remove-Item -LiteralPath $resolvedTestRoot -Recurse -Force -ErrorAction SilentlyContinue
}
