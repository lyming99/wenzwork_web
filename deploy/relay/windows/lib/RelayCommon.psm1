#Requires -Version 5.1

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$script:RelayServiceName = 'WenzWorkRelay'
$script:RelayServiceDisplayName = 'WenzWork Relay'
$script:RelayTempPrefix = 'wenzwork-relay-windows-'

function Write-RelayLog {
    param([Parameter(Mandatory = $true)][string]$Message)
    Write-Host "[wenzwork-relay] $Message"
}

function Assert-RelayAdministrator {
    $identity = [Security.Principal.WindowsIdentity]::GetCurrent()
    $principal = New-Object Security.Principal.WindowsPrincipal($identity)
    if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
        throw 'Run this script from an elevated PowerShell session (Run as administrator).'
    }
}

function ConvertTo-RelayArchitecture {
    param([Parameter(Mandatory = $true)][string]$Architecture)

    switch ($Architecture.Trim().ToUpperInvariant()) {
        'AMD64' { return 'amd64' }
        'X86_64' { return 'amd64' }
        'X64' { return 'amd64' }
        'ARM64' { return 'arm64' }
        'AARCH64' { return 'arm64' }
        default { throw "Unsupported Windows Relay architecture: $Architecture. Supported architectures are AMD64 and ARM64." }
    }
}

function Get-RelayHostArchitecture {
    if ([Environment]::OSVersion.Platform -ne [PlatformID]::Win32NT) {
        throw 'The Windows Relay scripts can only run on Windows.'
    }

    $architecture = ''
    try {
        $architecture = [Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString()
    }
    catch {
        # Older .NET Framework installations do not expose RuntimeInformation.
    }
    if ([string]::IsNullOrWhiteSpace($architecture)) {
        $architecture = $env:PROCESSOR_ARCHITEW6432
    }
    if ([string]::IsNullOrWhiteSpace($architecture)) {
        $architecture = $env:PROCESSOR_ARCHITECTURE
    }
    if ([string]::IsNullOrWhiteSpace($architecture)) {
        throw 'Could not determine the Windows host architecture.'
    }
    return ConvertTo-RelayArchitecture -Architecture $architecture
}

function Get-RelayDefaultInstallRoot {
    $programFiles = [Environment]::GetFolderPath([Environment+SpecialFolder]::ProgramFiles)
    if ([string]::IsNullOrWhiteSpace($programFiles)) {
        $programFiles = $env:ProgramFiles
    }
    return Join-Path $programFiles 'WenzWork\Relay'
}

function Read-RelayInstallRoot {
    param(
        [AllowEmptyString()][string]$Value,
        [Parameter(Mandatory = $true)][bool]$WasExplicit,
        [switch]$NonInteractive,
        [scriptblock]$Prompt
    )

    $defaultRoot = Get-RelayDefaultInstallRoot
    if ($WasExplicit) {
        if ([string]::IsNullOrWhiteSpace($Value)) { throw '-InstallRoot was provided but is empty.' }
        return Resolve-RelayInstallRoot -Path $Value
    }
    if ($NonInteractive) {
        Write-RelayLog "No -InstallRoot was provided; using the default work/install directory: $defaultRoot"
        return Resolve-RelayInstallRoot -Path $defaultRoot
    }
    if ($null -eq $Prompt) {
        $Prompt = { param($Message) Read-Host $Message }
    }
    $answer = & $Prompt "Relay work/install directory [$defaultRoot]"
    if ([string]::IsNullOrWhiteSpace($answer)) { $answer = $defaultRoot }
    return Resolve-RelayInstallRoot -Path $answer
}

function Resolve-RelayInstallRoot {
    param([Parameter(Mandatory = $true)][string]$Path)

    if ([string]::IsNullOrWhiteSpace($Path) -or $Path.IndexOfAny([char[]]"`0`r`n*?`"") -ge 0) {
        throw 'Relay install root is invalid.'
    }
    if (-not [IO.Path]::IsPathRooted($Path) -or $Path -notmatch '^[A-Za-z]:[\\/]') {
        throw 'Relay install root must be an absolute path on a local Windows drive.'
    }

    $fullPath = [IO.Path]::GetFullPath($Path).TrimEnd([IO.Path]::DirectorySeparatorChar, [IO.Path]::AltDirectorySeparatorChar)
    $volumeRoot = [IO.Path]::GetPathRoot($fullPath).TrimEnd([IO.Path]::DirectorySeparatorChar, [IO.Path]::AltDirectorySeparatorChar)
    if ($fullPath.Equals($volumeRoot, [StringComparison]::OrdinalIgnoreCase)) {
        throw 'Relay install root must not be a drive root.'
    }

    $protectedRoots = @(
        $env:SystemRoot,
        [Environment]::GetFolderPath([Environment+SpecialFolder]::Windows),
        [Environment]::GetFolderPath([Environment+SpecialFolder]::ProgramFiles),
        [Environment]::GetFolderPath([Environment+SpecialFolder]::CommonApplicationData),
        [Environment]::GetFolderPath([Environment+SpecialFolder]::UserProfile)
    )
    foreach ($protected in $protectedRoots) {
        if (-not [string]::IsNullOrWhiteSpace($protected)) {
            $normalized = [IO.Path]::GetFullPath($protected).TrimEnd('\', '/')
            if ($fullPath.Equals($normalized, [StringComparison]::OrdinalIgnoreCase)) {
                throw "Relay install root is too broad: $fullPath"
            }
        }
    }

    if (Test-Path -LiteralPath $fullPath) {
        $item = Get-Item -LiteralPath $fullPath -Force
        if (-not $item.PSIsContainer -or ($item.Attributes -band [IO.FileAttributes]::ReparsePoint)) {
            throw 'Relay install root must be a real directory, not a file, junction, or symbolic link.'
        }
    }
    $cursor = $fullPath
    while (-not [string]::IsNullOrWhiteSpace($cursor)) {
        if (Test-Path -LiteralPath $cursor) {
            $ancestor = Get-Item -LiteralPath $cursor -Force
            if ($ancestor.Attributes -band [IO.FileAttributes]::ReparsePoint) {
                throw "Relay install root must not traverse a junction or symbolic link: $cursor"
            }
        }
        $next = [IO.Path]::GetDirectoryName($cursor)
        if ([string]::IsNullOrWhiteSpace($next) -or $next.Equals($cursor, [StringComparison]::OrdinalIgnoreCase)) { break }
        $cursor = $next
    }
    return $fullPath
}

function Assert-RelayNetworkUrl {
    param([Parameter(Mandatory = $true)][string]$Url)

    if ($Url.Length -gt 2048) { throw 'Network URL is too long.' }
    $uri = $null
    if (-not [Uri]::TryCreate($Url.Trim(), [UriKind]::Absolute, [ref]$uri) -or
        -not [string]::IsNullOrEmpty($uri.UserInfo) -or
        -not [string]::IsNullOrEmpty($uri.Query) -or
        -not [string]::IsNullOrEmpty($uri.Fragment)) {
        throw 'Network URLs must be absolute and must not contain credentials, a query, or a fragment.'
    }
    if ($uri.Scheme -eq 'https' -or $uri.Scheme -eq 'http') {
        return $uri.AbsoluteUri
    }
    throw 'Network URLs must use HTTP or HTTPS.'
}

function Assert-RelayHealthUrl {
    param([Parameter(Mandatory = $true)][string]$Url)

    $uri = $null
    if (-not [Uri]::TryCreate($Url, [UriKind]::Absolute, [ref]$uri) -or
        $uri.Scheme -ne 'http' -or
        ($uri.Host -ne 'localhost' -and $uri.Host -ne '127.0.0.1' -and $uri.Host -ne '[::1]' -and $uri.Host -ne '::1') -or
        -not [string]::IsNullOrEmpty($uri.UserInfo) -or -not [string]::IsNullOrEmpty($uri.Query) -or -not [string]::IsNullOrEmpty($uri.Fragment)) {
        throw 'Relay health URL must be an absolute loopback HTTP URL without credentials, query, or fragment.'
    }
    return $uri.AbsoluteUri
}

function New-RelayTempDirectory {
    $base = [IO.Path]::GetFullPath([IO.Path]::GetTempPath()).TrimEnd('\', '/')
    $path = Join-Path $base ($script:RelayTempPrefix + [Guid]::NewGuid().ToString('N'))
    [void](New-Item -ItemType Directory -Path $path)
    return $path
}

function Remove-RelayTempDirectory {
    param([Parameter(Mandatory = $true)][string]$Path)

    $base = [IO.Path]::GetFullPath([IO.Path]::GetTempPath()).TrimEnd('\', '/')
    $fullPath = [IO.Path]::GetFullPath($Path).TrimEnd('\', '/')
    $parent = [IO.Path]::GetDirectoryName($fullPath).TrimEnd('\', '/')
    $leaf = [IO.Path]::GetFileName($fullPath)
    if (-not $parent.Equals($base, [StringComparison]::OrdinalIgnoreCase) -or -not $leaf.StartsWith($script:RelayTempPrefix, [StringComparison]::Ordinal)) {
        throw "Refusing to remove unexpected temporary path: $fullPath"
    }
    if (Test-Path -LiteralPath $fullPath) {
        Remove-Item -LiteralPath $fullPath -Recurse -Force
    }
}

function Write-RelayAtomicBytes {
    param(
        [Parameter(Mandatory = $true)][string]$Path,
        [Parameter(Mandatory = $true)][byte[]]$Bytes
    )

    $parent = [IO.Path]::GetDirectoryName([IO.Path]::GetFullPath($Path))
    if (-not (Test-Path -LiteralPath $parent -PathType Container)) {
        [void](New-Item -ItemType Directory -Path $parent)
    }
    $temporary = Join-Path $parent ('.relay-write-' + [Guid]::NewGuid().ToString('N'))
    $backup = Join-Path $parent ('.relay-backup-' + [Guid]::NewGuid().ToString('N'))
    try {
        [IO.File]::WriteAllBytes($temporary, $Bytes)
        if (Test-Path -LiteralPath $Path -PathType Leaf) {
            [IO.File]::Replace($temporary, $Path, $backup, $true)
            Remove-Item -LiteralPath $backup -Force
        }
        else {
            [IO.File]::Move($temporary, $Path)
        }
    }
    finally {
        if (Test-Path -LiteralPath $temporary) {
            Remove-Item -LiteralPath $temporary -Force
        }
        if (Test-Path -LiteralPath $backup) {
            Remove-Item -LiteralPath $backup -Force
        }
    }
}

function Write-RelayAtomicText {
    param(
        [Parameter(Mandatory = $true)][string]$Path,
        [Parameter(Mandatory = $true)][AllowEmptyString()][string]$Text
    )
    $encoding = New-Object Text.UTF8Encoding($false)
    Write-RelayAtomicBytes -Path $Path -Bytes $encoding.GetBytes($Text)
}

function Assert-RelaySecretFileAcl {
    param([Parameter(Mandatory = $true)][string]$Path)

    if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) {
        throw "Access Key file does not exist: $Path"
    }
    $item = Get-Item -LiteralPath $Path -Force
    if ($item.Attributes -band [IO.FileAttributes]::ReparsePoint) {
        throw 'Access Key file must not be a junction or symbolic link.'
    }

    $forbidden = @('S-1-1-0', 'S-1-5-11', 'S-1-5-32-545')
    $rules = (Get-Acl -LiteralPath $Path).GetAccessRules($true, $true, [Security.Principal.SecurityIdentifier])
    foreach ($rule in $rules) {
        if ($rule.AccessControlType -eq [Security.AccessControl.AccessControlType]::Allow -and
            $forbidden -contains $rule.IdentityReference.Value -and
            ($rule.FileSystemRights -band [Security.AccessControl.FileSystemRights]::ReadData)) {
            throw 'Access Key file grants read access to Everyone, Authenticated Users, or Users. Restrict its ACL first.'
        }
    }
}

function Read-RelayAccessKey {
    param(
        [switch]$FromStdin,
        [string]$File,
        [scriptblock]$SecurePrompt
    )

    if ($FromStdin -and -not [string]::IsNullOrWhiteSpace($File)) {
        throw 'Select only one Access Key input mode.'
    }

    $key = ''
    if ($FromStdin) {
        if (-not [Console]::IsInputRedirected) {
            throw '-AccessKeyStdin requires redirected standard input; omit it for the hidden interactive prompt.'
        }
        $key = [Console]::In.ReadLine()
    }
    elseif (-not [string]::IsNullOrWhiteSpace($File)) {
        Assert-RelaySecretFileAcl -Path $File
        $info = Get-Item -LiteralPath $File
        if ($info.Length -gt 256) {
            throw 'Access Key file is too large.'
        }
        $key = [IO.File]::ReadAllText($info.FullName)
    }
    else {
        if ($null -eq $SecurePrompt) {
            $secure = Read-Host 'Access Key' -AsSecureString
        }
        else {
            $secure = & $SecurePrompt 'Access Key'
        }
        if ($secure -isnot [Security.SecureString]) {
            throw 'The Access Key prompt did not return a SecureString.'
        }
        $pointer = [IntPtr]::Zero
        try {
            $pointer = [Runtime.InteropServices.Marshal]::SecureStringToBSTR($secure)
            $key = [Runtime.InteropServices.Marshal]::PtrToStringBSTR($pointer)
        }
        finally {
            if ($pointer -ne [IntPtr]::Zero) {
                [Runtime.InteropServices.Marshal]::ZeroFreeBSTR($pointer)
            }
        }
    }

    if ($null -eq $key -or $key.Length -gt 256) { throw 'Access Key is invalid.' }
    $key = $key.Trim()
    if ($key -notmatch '^relay_[A-Za-z0-9_-]{43}$') {
        throw 'Access Key is invalid.'
    }
    return $key
}

function Write-RelayEnvironment {
    param(
        [Parameter(Mandatory = $true)][string]$Path,
        [Parameter(Mandatory = $true)][string]$AccessKey,
        [Parameter(Mandatory = $true)][string]$ManagementUrl,
        [Parameter(Mandatory = $true)][string]$Version
    )

    if ($AccessKey -notmatch '^relay_[A-Za-z0-9_-]{43}$') { throw 'Access Key is invalid.' }
    $validatedUrl = Assert-RelayNetworkUrl -Url $ManagementUrl
    if ($Version -notmatch '^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$') { throw 'Relay version is invalid.' }
    $contents = "RELAY_ACCESS_KEY=$AccessKey`r`nRELAY_MANAGEMENT_URL=$validatedUrl`r`nRELAY_VERSION=$Version`r`n"
    Write-RelayAtomicText -Path $Path -Text $contents
}

function Update-RelayEnvironmentVersion {
    param(
        [Parameter(Mandatory = $true)][string]$Path,
        [Parameter(Mandatory = $true)][string]$Version
    )

    if ($Version -notmatch '^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$') { throw 'Relay version is invalid.' }
    if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) { throw 'Relay environment file is missing.' }
    $text = [IO.File]::ReadAllText($Path)
    if ($text.IndexOf([char]0) -ge 0) { throw 'Relay environment file is invalid.' }

    $lines = $text -split "`r?`n"
    $accessKeys = @($lines | Where-Object { $_ -match '^RELAY_ACCESS_KEY=' })
    $managementUrls = @($lines | Where-Object { $_ -match '^RELAY_MANAGEMENT_URL=' })
    $versions = @($lines | Where-Object { $_ -match '^RELAY_VERSION=' })
    if ($accessKeys.Count -ne 1 -or ($accessKeys[0] -replace '^RELAY_ACCESS_KEY=', '') -notmatch '^relay_[A-Za-z0-9_-]{43}$') {
        throw 'Relay environment file does not contain exactly one valid Access Key.'
    }
    if ($managementUrls.Count -ne 1) { throw 'Relay environment file does not contain exactly one management URL.' }
    [void](Assert-RelayNetworkUrl -Url ($managementUrls[0] -replace '^RELAY_MANAGEMENT_URL=', ''))
    if ($versions.Count -gt 1) { throw 'Relay environment file contains duplicate version entries.' }

    $result = New-Object Collections.Generic.List[string]
    $updated = $false
    foreach ($line in $lines) {
        if ($line -match '^RELAY_VERSION=') {
            $result.Add("RELAY_VERSION=$Version")
            $updated = $true
        }
        elseif ($line.Length -gt 0) {
            $result.Add($line)
        }
    }
    if (-not $updated) { $result.Add("RELAY_VERSION=$Version") }
    Write-RelayAtomicText -Path $Path -Text (($result -join "`r`n") + "`r`n")
}

function Read-RelayVersion {
    param([Parameter(Mandatory = $true)][string]$PackageRoot)
    $path = Join-Path $PackageRoot 'VERSION'
    if (-not (Test-Path -LiteralPath $path -PathType Leaf) -or (Get-Item -LiteralPath $path).Length -gt 128) {
        throw 'Relay package VERSION is missing or invalid.'
    }
    $version = [IO.File]::ReadAllText($path).Trim()
    if ($version -notmatch '^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$') { throw 'Relay package VERSION is invalid.' }
    return $version
}

function Assert-RelayManifestTarget {
    param(
        [Parameter(Mandatory = $true)][string]$ManifestPath,
        [Parameter(Mandatory = $true)][string]$HostArchitecture,
        [string]$ExpectedVersion
    )

    $architecture = ConvertTo-RelayArchitecture -Architecture $HostArchitecture
    if (-not (Test-Path -LiteralPath $ManifestPath -PathType Leaf)) { throw 'Relay release manifest is missing.' }
    $info = Get-Item -LiteralPath $ManifestPath
    if ($info.Length -le 0 -or $info.Length -gt 1MB -or ($info.Attributes -band [IO.FileAttributes]::ReparsePoint)) {
        throw 'Relay release manifest file type or size is invalid.'
    }
    try {
        $manifest = [IO.File]::ReadAllText($info.FullName) | ConvertFrom-Json
    }
    catch {
        throw "Relay release manifest is not valid JSON: $($_.Exception.Message)"
    }
    if ($null -eq $manifest -or $manifest.schemaVersion -ne 1 -or $manifest.platform -cne 'windows' -or
        $manifest.architecture -cne $architecture -or $manifest.version -notmatch '^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$') {
        throw "Relay release manifest does not match this windows/$architecture host."
    }
    if (-not [string]::IsNullOrWhiteSpace($ExpectedVersion) -and $manifest.version -cne $ExpectedVersion) {
        throw 'Relay release manifest version does not match VERSION.'
    }
    return $manifest
}

function Assert-RelayTrustedVerifier {
    param(
        [Parameter(Mandatory = $true)][string]$VerifierFile,
        [string]$VerifierSha256,
        [switch]$RequireHash
    )

    if (-not (Test-Path -LiteralPath $VerifierFile -PathType Leaf)) { throw "Trusted relayctl verifier does not exist: $VerifierFile" }
    $item = Get-Item -LiteralPath $VerifierFile -Force
    if ($item.Attributes -band [IO.FileAttributes]::ReparsePoint) { throw 'Trusted relayctl verifier must not be a symbolic link.' }
    if ($RequireHash -and [string]::IsNullOrWhiteSpace($VerifierSha256)) {
        throw '-VerifierSha256 is required for a bootstrap verifier.'
    }
    if (-not [string]::IsNullOrWhiteSpace($VerifierSha256)) {
        if ($VerifierSha256 -notmatch '^[0-9A-Fa-f]{64}$') { throw 'Verifier SHA-256 is invalid.' }
        $actual = (Get-FileHash -LiteralPath $item.FullName -Algorithm SHA256).Hash
        if (-not $actual.Equals($VerifierSha256, [StringComparison]::OrdinalIgnoreCase)) {
            throw 'Trusted relayctl verifier SHA-256 does not match.'
        }
    }
    return $item.FullName
}

function Invoke-RelayVerifier {
    param(
        [Parameter(Mandatory = $true)][string]$VerifierFile,
        [Parameter(Mandatory = $true)][string[]]$Arguments
    )

    $output = @(& $VerifierFile @Arguments 2>&1)
    if ($LASTEXITCODE -ne 0) {
        $message = ($output | ForEach-Object { $_.ToString() }) -join [Environment]::NewLine
        throw "Relay verifier rejected the input (exit $LASTEXITCODE): $message"
    }
}

function Assert-RelayBundle {
    param(
        [Parameter(Mandatory = $true)][string]$VerifierFile,
        [Parameter(Mandatory = $true)][string]$Archive,
        [Parameter(Mandatory = $true)][string]$ChecksumsFile,
        [Parameter(Mandatory = $true)][string]$ChecksumsSignatureFile,
        [Parameter(Mandatory = $true)][string]$SigningKeyFile
    )

    foreach ($path in @($Archive, $ChecksumsFile, $ChecksumsSignatureFile, $SigningKeyFile)) {
        if (-not (Test-Path -LiteralPath $path -PathType Leaf)) { throw "Required Relay bundle file is missing: $path" }
        if ((Get-Item -LiteralPath $path -Force).Attributes -band [IO.FileAttributes]::ReparsePoint) {
            throw "Relay bundle input must not be a symbolic link: $path"
        }
    }
    Invoke-RelayVerifier -VerifierFile $VerifierFile -Arguments @(
        'release', 'verify-bundle', '--archive', [IO.Path]::GetFullPath($Archive),
        '--checksums', [IO.Path]::GetFullPath($ChecksumsFile),
        '--signature', [IO.Path]::GetFullPath($ChecksumsSignatureFile),
        '--public-key', [IO.Path]::GetFullPath($SigningKeyFile)
    )
}

function Invoke-RelayDownload {
    param(
        [Parameter(Mandatory = $true)][string]$Url,
        [Parameter(Mandatory = $true)][string]$Destination
    )

    $current = [Uri](Assert-RelayNetworkUrl -Url $Url)
    [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
    Add-Type -AssemblyName System.Net.Http
    $handler = New-Object Net.Http.HttpClientHandler
    $handler.AllowAutoRedirect = $false
    $client = New-Object Net.Http.HttpClient($handler)
    $client.Timeout = [TimeSpan]::FromMinutes(10)
    try {
        for ($redirects = 0; $redirects -le 5; $redirects++) {
            $response = $client.GetAsync($current, [Net.Http.HttpCompletionOption]::ResponseHeadersRead).GetAwaiter().GetResult()
            try {
                if ([int]$response.StatusCode -ge 300 -and [int]$response.StatusCode -lt 400) {
                    if ($null -eq $response.Headers.Location -or $redirects -eq 5) { throw 'Relay download redirect is invalid or excessive.' }
                    $next = $response.Headers.Location
                    if (-not $next.IsAbsoluteUri) { $next = New-Object Uri($current, $next) }
                    $current = [Uri](Assert-RelayNetworkUrl -Url $next.AbsoluteUri)
                    continue
                }
                if (-not $response.IsSuccessStatusCode) { throw "Relay download failed with HTTP $([int]$response.StatusCode)." }
                $contentLength = $response.Content.Headers.ContentLength
                if ($null -ne $contentLength -and [Int64]$contentLength -gt 1GB) {
                    throw 'Relay download exceeds the 1 GiB safety limit.'
                }
                $stream = $response.Content.ReadAsStreamAsync().GetAwaiter().GetResult()
                try {
                    $file = New-Object IO.FileStream($Destination, [IO.FileMode]::CreateNew, [IO.FileAccess]::Write, [IO.FileShare]::None)
                    try {
                        $buffer = New-Object byte[] 81920
                        $total = [Int64]0
                        while (($read = $stream.Read($buffer, 0, $buffer.Length)) -gt 0) {
                            $total += $read
                            if ($total -gt 1GB) { throw 'Relay download exceeds the 1 GiB safety limit.' }
                            $file.Write($buffer, 0, $read)
                        }
                    }
                    finally { $file.Dispose() }
                }
                finally { $stream.Dispose() }
                return
            }
            finally { $response.Dispose() }
        }
    }
    finally {
        $client.Dispose()
        $handler.Dispose()
    }
}

function Test-RelayArchiveEntryPath {
    param([Parameter(Mandatory = $true)][string]$Entry)
    if ([string]::IsNullOrWhiteSpace($Entry) -or $Entry.IndexOfAny([char[]]"`0`r`n:") -ge 0 -or
        $Entry.StartsWith('/') -or $Entry.StartsWith('\') -or $Entry.Contains('\') -or
        $Entry -match '(^|/)\.\.(/|$)') {
        return $false
    }
    return $true
}

function Expand-RelayArchive {
    param(
        [Parameter(Mandatory = $true)][string]$Archive,
        [Parameter(Mandatory = $true)][string]$Destination
    )

    if (-not (Test-Path -LiteralPath $Archive -PathType Leaf) -or
        ((Get-Item -LiteralPath $Archive -Force).Attributes -band [IO.FileAttributes]::ReparsePoint)) {
        throw 'Relay archive must be a regular file.'
    }
    if ((Get-Item -LiteralPath $Archive -Force).Length -gt 1GB) { throw 'Relay archive exceeds the 1 GiB safety limit.' }
    if (Test-Path -LiteralPath $Destination) { throw 'Relay archive destination already exists.' }
    [void](New-Item -ItemType Directory -Path $Destination)
    $lower = $Archive.ToLowerInvariant()
    if ($lower.EndsWith('.zip')) {
        Add-Type -AssemblyName System.IO.Compression.FileSystem
        $zip = [IO.Compression.ZipFile]::OpenRead($Archive)
        try {
            if ($zip.Entries.Count -gt 512) { throw 'Relay archive contains too many entries.' }
            $totalSize = [Int64]0
            $seen = New-Object 'Collections.Generic.HashSet[string]' ([StringComparer]::OrdinalIgnoreCase)
            $destinationFull = [IO.Path]::GetFullPath($Destination).TrimEnd('\') + '\'
            foreach ($entry in $zip.Entries) {
                if (-not (Test-RelayArchiveEntryPath -Entry $entry.FullName)) { throw "Relay archive contains an unsafe path: $($entry.FullName)" }
                if (-not $seen.Add($entry.FullName)) { throw "Relay archive contains a duplicate path: $($entry.FullName)" }
                $unixType = (($entry.ExternalAttributes -shr 16) -band 0xF000)
                if ($unixType -ne 0 -and $unixType -ne 0x8000 -and $unixType -ne 0x4000) {
                    throw "Relay archive contains a link or special file: $($entry.FullName)"
                }
                $target = [IO.Path]::GetFullPath((Join-Path $Destination ($entry.FullName -replace '/', '\')))
                if (-not $target.StartsWith($destinationFull, [StringComparison]::OrdinalIgnoreCase)) { throw 'Relay archive path escapes the extraction directory.' }
                if ($entry.FullName.EndsWith('/')) {
                    [void](New-Item -ItemType Directory -Path $target -Force)
                    continue
                }
                $totalSize += $entry.Length
                if ($totalSize -gt 1GB) { throw 'Relay archive uncompressed size exceeds 1 GiB.' }
                $parent = [IO.Path]::GetDirectoryName($target)
                if (-not (Test-Path -LiteralPath $parent)) { [void](New-Item -ItemType Directory -Path $parent) }
                $inputStream = $entry.Open()
                try {
                    $outputStream = New-Object IO.FileStream($target, [IO.FileMode]::CreateNew, [IO.FileAccess]::Write, [IO.FileShare]::None)
                    try { $inputStream.CopyTo($outputStream) } finally { $outputStream.Dispose() }
                }
                finally { $inputStream.Dispose() }
            }
        }
        finally { $zip.Dispose() }
        return
    }

    if (-not ($lower.EndsWith('.tar.gz') -or $lower.EndsWith('.tgz'))) { throw 'Relay archive must be a .tar.gz, .tgz, or .zip file.' }
    $tar = Join-Path $env:SystemRoot 'System32\tar.exe'
    if (-not (Test-Path -LiteralPath $tar -PathType Leaf)) { throw 'Windows tar.exe is required to extract Relay .tar.gz archives.' }
    $entries = @(& $tar -tzf $Archive 2>&1)
    if ($LASTEXITCODE -ne 0 -or $entries.Count -eq 0 -or $entries.Count -gt 512) { throw 'Relay tar archive listing failed or contains too many entries.' }
    $seen = New-Object 'Collections.Generic.HashSet[string]' ([StringComparer]::OrdinalIgnoreCase)
    foreach ($entryValue in $entries) {
        $entry = $entryValue.ToString()
        if (-not (Test-RelayArchiveEntryPath -Entry $entry)) { throw "Relay archive contains an unsafe path: $entry" }
        if (-not $seen.Add($entry)) { throw "Relay archive contains a duplicate path: $entry" }
    }
    $verbose = @(& $tar -tvzf $Archive 2>&1)
    if ($LASTEXITCODE -ne 0 -or $verbose.Count -ne $entries.Count) { throw 'Relay tar archive metadata listing failed.' }
    $totalSize = [Int64]0
    foreach ($lineValue in $verbose) {
        $line = $lineValue.ToString()
        if ($line.Length -eq 0 -or ($line[0] -ne '-' -and $line[0] -ne 'd')) { throw 'Relay tar archive contains a link or special file.' }
        if ($line -notmatch '^[-d][rwxStTs-]{9}\s+\d+\s+\S+\s+\S+\s+(\d+)\s+') {
            throw 'Relay tar archive contains unsupported metadata.'
        }
        $entrySize = [Int64]$Matches[1]
        if ($entrySize -gt (1GB - $totalSize)) { throw 'Relay archive uncompressed size exceeds 1 GiB.' }
        $totalSize += $entrySize
    }
    & $tar -xzf $Archive -C $Destination
    if ($LASTEXITCODE -ne 0) { throw 'Relay tar archive extraction failed.' }
}

function Find-RelayPackageRoot {
    param([Parameter(Mandatory = $true)][string]$Root)

    if (Test-Path -LiteralPath (Join-Path $Root 'bin\wenzwork-relay-server.exe') -PathType Leaf) { return [IO.Path]::GetFullPath($Root) }
    $candidates = @(Get-ChildItem -LiteralPath $Root -Force -Directory | Where-Object {
        Test-Path -LiteralPath (Join-Path $_.FullName 'bin\wenzwork-relay-server.exe') -PathType Leaf
    })
    if ($candidates.Count -ne 1) { throw 'Relay archive must contain exactly one package root.' }
    return $candidates[0].FullName
}

function Resolve-RelayPackageSource {
    param(
        [string]$PackageDirectory,
        [string]$PackageFile,
        [string]$ArtifactUrl,
        [string]$ChecksumsFile,
        [string]$ChecksumsUrl,
        [string]$ChecksumsSignatureFile,
        [string]$ChecksumsSignatureUrl,
        [string]$SigningKeyFile,
        [Parameter(Mandatory = $true)][string]$VerifierFile,
        [Parameter(Mandatory = $true)][string]$WorkDirectory
    )

    $sourceCount = @(@($PackageDirectory, $PackageFile, $ArtifactUrl) | Where-Object { -not [string]::IsNullOrWhiteSpace($_) }).Count
    if ($sourceCount -ne 1) { throw 'Select exactly one of -PackageDirectory, -PackageFile, or -ArtifactUrl.' }
    if (-not [string]::IsNullOrWhiteSpace($PackageDirectory)) {
        if (-not (Test-Path -LiteralPath $PackageDirectory -PathType Container)) { throw 'Relay package directory does not exist.' }
        return Find-RelayPackageRoot -Root $PackageDirectory
    }

    $archive = $PackageFile
    if (-not [string]::IsNullOrWhiteSpace($ArtifactUrl)) {
        if (-not [string]::IsNullOrWhiteSpace($PackageFile)) { throw 'Select only one Relay package source.' }
        $artifactUri = [Uri](Assert-RelayNetworkUrl -Url $ArtifactUrl)
        $name = [IO.Path]::GetFileName($artifactUri.AbsolutePath)
        if ($name -notmatch '\.(tar\.gz|tgz|zip)$') { throw 'Relay artifact URL must end with .tar.gz, .tgz, or .zip.' }
        $archive = Join-Path $WorkDirectory $name
        Invoke-RelayDownload -Url $artifactUri.AbsoluteUri -Destination $archive
    }
    if (-not (Test-Path -LiteralPath $archive -PathType Leaf)) { throw 'Relay package archive does not exist.' }

    if (-not [string]::IsNullOrWhiteSpace($ChecksumsUrl)) {
        if (-not [string]::IsNullOrWhiteSpace($ChecksumsFile)) { throw 'Select only one SHA256SUMS source.' }
        $ChecksumsFile = Join-Path $WorkDirectory 'SHA256SUMS'
        Invoke-RelayDownload -Url $ChecksumsUrl -Destination $ChecksumsFile
    }
    if (-not [string]::IsNullOrWhiteSpace($ChecksumsSignatureUrl)) {
        if (-not [string]::IsNullOrWhiteSpace($ChecksumsSignatureFile)) { throw 'Select only one SHA256SUMS signature source.' }
        $ChecksumsSignatureFile = Join-Path $WorkDirectory 'SHA256SUMS.sig'
        Invoke-RelayDownload -Url $ChecksumsSignatureUrl -Destination $ChecksumsSignatureFile
    }
    if ([string]::IsNullOrWhiteSpace($ChecksumsFile) -or [string]::IsNullOrWhiteSpace($ChecksumsSignatureFile) -or [string]::IsNullOrWhiteSpace($SigningKeyFile)) {
        throw 'Archive installation requires SHA256SUMS, its signature, and the trusted Release signing public key.'
    }
    Assert-RelayBundle -VerifierFile $VerifierFile -Archive $archive -ChecksumsFile $ChecksumsFile -ChecksumsSignatureFile $ChecksumsSignatureFile -SigningKeyFile $SigningKeyFile
    $extract = Join-Path $WorkDirectory 'package'
    Expand-RelayArchive -Archive $archive -Destination $extract
    return Find-RelayPackageRoot -Root $extract
}

function Assert-RelayReleaseTree {
    param(
        [Parameter(Mandatory = $true)][string]$PackageRoot,
        [Parameter(Mandatory = $true)][string]$VerifierFile,
        [Parameter(Mandatory = $true)][string]$HostArchitecture,
        [string]$ExpectedVersion
    )

    $required = @('bin\wenzwork-relay-server.exe', 'bin\relayctl.exe', 'VERSION', 'release-manifest.json')
    foreach ($relative in $required) {
        $path = Join-Path $PackageRoot $relative
        if (-not (Test-Path -LiteralPath $path -PathType Leaf) -or ((Get-Item -LiteralPath $path -Force).Attributes -band [IO.FileAttributes]::ReparsePoint)) {
            throw "Relay package is missing a regular file: $relative"
        }
    }
    $version = Read-RelayVersion -PackageRoot $PackageRoot
    if (-not [string]::IsNullOrWhiteSpace($ExpectedVersion) -and $version -cne $ExpectedVersion) { throw 'Relay release version is unexpected.' }
    $architecture = ConvertTo-RelayArchitecture -Architecture $HostArchitecture
    [void](Assert-RelayManifestTarget -ManifestPath (Join-Path $PackageRoot 'release-manifest.json') -HostArchitecture $architecture -ExpectedVersion $version)
    Invoke-RelayVerifier -VerifierFile $VerifierFile -Arguments @(
        'release', 'verify', '--root', [IO.Path]::GetFullPath($PackageRoot), '--manifest', 'release-manifest.json',
        '--expected-version', $version, '--expected-platform', 'windows', '--expected-architecture', $architecture, '--protocol-version', '1'
    )
    return $version
}

function Install-RelayReleaseTree {
    param(
        [Parameter(Mandatory = $true)][string]$PackageRoot,
        [Parameter(Mandatory = $true)][string]$InstallRoot,
        [Parameter(Mandatory = $true)][string]$Version,
        [Parameter(Mandatory = $true)][string]$VerifierFile,
        [Parameter(Mandatory = $true)][string]$HostArchitecture
    )

    $root = Resolve-RelayInstallRoot -Path $InstallRoot
    $releases = Join-Path $root 'releases'
    if (-not (Test-Path -LiteralPath $releases)) { [void](New-Item -ItemType Directory -Path $releases -Force) }
    $destination = Join-Path $releases $Version
    if (Test-Path -LiteralPath $destination) {
        $item = Get-Item -LiteralPath $destination -Force
        if (-not $item.PSIsContainer -or ($item.Attributes -band [IO.FileAttributes]::ReparsePoint)) { throw 'Existing Relay release destination is unsafe.' }
        [void](Assert-RelayReleaseTree -PackageRoot $destination -VerifierFile $VerifierFile -HostArchitecture $HostArchitecture -ExpectedVersion $Version)
        return $destination
    }

    $stage = Join-Path $releases ('.stage.' + [Guid]::NewGuid().ToString('N'))
    try {
        [void](New-Item -ItemType Directory -Path $stage)
        Get-ChildItem -LiteralPath $PackageRoot -Force | Copy-Item -Destination $stage -Recurse -Force
        [void](Assert-RelayReleaseTree -PackageRoot $stage -VerifierFile $VerifierFile -HostArchitecture $HostArchitecture -ExpectedVersion $Version)
        [IO.Directory]::Move($stage, $destination)
    }
    finally {
        if (Test-Path -LiteralPath $stage) { Remove-Item -LiteralPath $stage -Recurse -Force }
    }
    return $destination
}

function Invoke-RelaySc {
    param([Parameter(Mandatory = $true)][string[]]$Arguments)
    $sc = Join-Path $env:SystemRoot 'System32\sc.exe'
    $output = @(& $sc @Arguments 2>&1)
    if ($LASTEXITCODE -ne 0) {
        throw "sc.exe failed (exit $LASTEXITCODE): $((($output | ForEach-Object { $_.ToString() }) -join [Environment]::NewLine))"
    }
}

function Test-RelayServiceExists {
    return $null -ne (Get-Service -Name $script:RelayServiceName -ErrorAction SilentlyContinue)
}

function ConvertTo-RelayServiceImagePath {
    param([Parameter(Mandatory = $true)][string]$BinaryPath)
    $fullPath = [IO.Path]::GetFullPath($BinaryPath)
    if ($fullPath.IndexOf('"') -ge 0) { throw 'Relay service binary path is invalid.' }
    return '"' + $fullPath + '"'
}

function New-RelayServiceRegistration {
    param([Parameter(Mandatory = $true)][string]$BinaryPath)

    if (Test-RelayServiceExists) { throw "$($script:RelayServiceName) already exists; use upgrade.ps1." }
    $imagePath = ConvertTo-RelayServiceImagePath -BinaryPath $BinaryPath
    Invoke-RelaySc -Arguments @('create', $script:RelayServiceName, 'binPath=', $imagePath, 'start=', 'auto', 'obj=', 'NT AUTHORITY\LocalService', 'DisplayName=', $script:RelayServiceDisplayName)
    try {
        Invoke-RelaySc -Arguments @('description', $script:RelayServiceName, 'WenzWork Relay node service')
        Invoke-RelaySc -Arguments @('sidtype', $script:RelayServiceName, 'unrestricted')
        Invoke-RelaySc -Arguments @('failure', $script:RelayServiceName, 'reset=', '86400', 'actions=', 'restart/5000/restart/15000/restart/30000')
        Invoke-RelaySc -Arguments @('failureflag', $script:RelayServiceName, '1')
    }
    catch {
        try { Invoke-RelaySc -Arguments @('delete', $script:RelayServiceName) } catch { }
        throw
    }
}

function Get-RelayServiceRegistryPath {
    return "HKLM:\SYSTEM\CurrentControlSet\Services\$($script:RelayServiceName)"
}

function Set-RelayServiceEnvironment {
    param([Parameter(Mandatory = $true)][string]$EnvironmentFile)
    $fullPath = [IO.Path]::GetFullPath($EnvironmentFile)
    if ($fullPath.IndexOfAny([char[]]"`0`r`n") -ge 0) { throw 'Relay environment file path is invalid.' }
    $registryPath = Get-RelayServiceRegistryPath
    if (-not (Test-Path -LiteralPath $registryPath)) { throw 'Relay service registry key is missing.' }
    [void](New-ItemProperty -LiteralPath $registryPath -Name 'Environment' -PropertyType MultiString -Value @("RELAY_ENV_FILE=$fullPath") -Force)
}

function Get-RelayServiceEnvironmentFile {
    $registryPath = Get-RelayServiceRegistryPath
    $values = @((Get-ItemProperty -LiteralPath $registryPath -Name Environment -ErrorAction Stop).Environment)
    $matches = @($values | Where-Object { $_ -like 'RELAY_ENV_FILE=*' })
    if ($matches.Count -ne 1) { throw 'Relay service does not have exactly one RELAY_ENV_FILE setting.' }
    return $matches[0].Substring('RELAY_ENV_FILE='.Length)
}

function Get-RelayServiceImagePath {
    $value = (Get-ItemProperty -LiteralPath (Get-RelayServiceRegistryPath) -Name ImagePath -ErrorAction Stop).ImagePath
    if ($value -notmatch '^"([^"]+)"$') { throw 'Relay service ImagePath is not managed by these scripts.' }
    return $matches[1]
}

function Set-RelayServiceBinary {
    param([Parameter(Mandatory = $true)][string]$BinaryPath)
    $imagePath = ConvertTo-RelayServiceImagePath -BinaryPath $BinaryPath
    Invoke-RelaySc -Arguments @('config', $script:RelayServiceName, 'binPath=', $imagePath)
}

function Get-RelayServiceSid {
    $account = New-Object Security.Principal.NTAccount("NT SERVICE\$($script:RelayServiceName)")
    try { return $account.Translate([Security.Principal.SecurityIdentifier]) }
    catch { throw 'Could not resolve the WenzWorkRelay service SID after registration.' }
}

function Set-RelayInstallAcl {
    param([Parameter(Mandatory = $true)][string]$InstallRoot)
    $root = Resolve-RelayInstallRoot -Path $InstallRoot
    $serviceSid = (Get-RelayServiceSid).Value
    $icacls = Join-Path $env:SystemRoot 'System32\icacls.exe'
    $output = @(& $icacls $root '/inheritance:r' '/grant:r' '*S-1-5-18:(OI)(CI)(F)' '*S-1-5-32-544:(OI)(CI)(F)' "*$serviceSid`:(OI)(CI)(RX)" '/T' '/C' 2>&1)
    if ($LASTEXITCODE -ne 0) { throw "Could not secure Relay install ACLs: $((($output | ForEach-Object { $_.ToString() }) -join [Environment]::NewLine))" }
}

function Wait-RelayServiceState {
    param(
        [Parameter(Mandatory = $true)][ValidateSet('Running', 'Stopped')][string]$State,
        [ValidateRange(1, 300)][int]$WaitSeconds = 30
    )
    $deadline = [DateTime]::UtcNow.AddSeconds($WaitSeconds)
    do {
        $service = Get-Service -Name $script:RelayServiceName -ErrorAction Stop
        if ($service.Status.ToString() -eq $State) { return }
        Start-Sleep -Milliseconds 250
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "WenzWorkRelay did not reach service state $State within $WaitSeconds seconds."
}

function Start-RelayService {
    param([ValidateRange(1, 300)][int]$WaitSeconds = 30)
    $service = Get-Service -Name $script:RelayServiceName -ErrorAction Stop
    if ($service.Status -ne [ServiceProcess.ServiceControllerStatus]::Running) { Start-Service -Name $script:RelayServiceName }
    Wait-RelayServiceState -State Running -WaitSeconds $WaitSeconds
}

function Stop-RelayService {
    param([ValidateRange(1, 300)][int]$WaitSeconds = 30)
    $service = Get-Service -Name $script:RelayServiceName -ErrorAction Stop
    if ($service.Status -ne [ServiceProcess.ServiceControllerStatus]::Stopped) { Stop-Service -Name $script:RelayServiceName -Force }
    Wait-RelayServiceState -State Stopped -WaitSeconds $WaitSeconds
}

function Remove-RelayServiceRegistration {
    if (-not (Test-RelayServiceExists)) { return }
    try { Stop-RelayService -WaitSeconds 30 } catch { }
    Invoke-RelaySc -Arguments @('delete', $script:RelayServiceName)
    $deadline = [DateTime]::UtcNow.AddSeconds(30)
    while ((Test-RelayServiceExists) -and [DateTime]::UtcNow -lt $deadline) { Start-Sleep -Milliseconds 250 }
    if (Test-RelayServiceExists) { throw 'WenzWorkRelay service deletion did not complete.' }
}

function Test-RelayHealth {
    param(
        [ValidateSet('ready', 'live')][string]$Mode = 'ready',
        [ValidateRange(0, 300)][int]$WaitSeconds = 0,
        [string]$BaseUrl = 'http://127.0.0.1:19090'
    )
    $base = $BaseUrl.TrimEnd('/')
    $url = Assert-RelayHealthUrl -Url "$base/health/$Mode"
    $deadline = [DateTime]::UtcNow.AddSeconds($WaitSeconds)
    do {
        try {
            $response = Invoke-WebRequest -Uri $url -UseBasicParsing -TimeoutSec 3 -ErrorAction Stop
            if ($response.StatusCode -eq 200) { return $true }
        }
        catch {
            if ([DateTime]::UtcNow -ge $deadline) { throw "Relay $Mode health check failed: $($_.Exception.Message)" }
        }
        if ([DateTime]::UtcNow -ge $deadline) { throw "Relay $Mode health check failed." }
        Start-Sleep -Seconds 1
    } while ($true)
}

function Write-RelayCurrentMetadata {
    param(
        [Parameter(Mandatory = $true)][string]$InstallRoot,
        [Parameter(Mandatory = $true)][string]$Version,
        [Parameter(Mandatory = $true)][string]$ReleasePath
    )
    $metadata = [ordered]@{ schemaVersion = 1; serviceName = $script:RelayServiceName; version = $Version; releasePath = [IO.Path]::GetFullPath($ReleasePath) }
    Write-RelayAtomicText -Path (Join-Path $InstallRoot 'current.json') -Text (($metadata | ConvertTo-Json -Compress) + "`r`n")
}

function Remove-RelayInstalledFiles {
    param(
        [Parameter(Mandatory = $true)][string]$InstallRoot,
        [switch]$Purge
    )
    $root = Resolve-RelayInstallRoot -Path $InstallRoot
    if (-not (Test-Path -LiteralPath $root)) { return }
    if ($Purge) {
        Remove-Item -LiteralPath $root -Recurse -Force
        return
    }
    foreach ($name in @('releases', 'current.json')) {
        $path = Join-Path $root $name
        if (Test-Path -LiteralPath $path) { Remove-Item -LiteralPath $path -Recurse -Force }
    }
}

Export-ModuleMember -Function *-Relay*
