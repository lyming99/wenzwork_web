#Requires -Version 5.1

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$relayModule = Join-Path $PSScriptRoot 'RelayCommon.psm1'
if (-not (Test-Path -LiteralPath $relayModule -PathType Leaf)) {
    $relayModule = Join-Path $PSScriptRoot '..\..\..\relay\windows\lib\RelayCommon.psm1'
}
Import-Module $relayModule -Force

$script:AgentServiceName = 'WenzWorkDeviceAgent'
$script:AgentDisplayName = 'WenzWork Device Agent'

function Write-AgentLog {
    param([Parameter(Mandatory = $true)][string]$Message)
    Write-Host "[wenzwork-device-agent] $Message"
}

function Get-AgentDefaultInstallRoot {
    $programFiles = [Environment]::GetFolderPath([Environment+SpecialFolder]::ProgramFiles)
    if ([string]::IsNullOrWhiteSpace($programFiles)) { $programFiles = $env:ProgramFiles }
    return Join-Path $programFiles 'WenzWork\DeviceAgent'
}

function Get-AgentApplicationRoot {
    $programData = [Environment]::GetFolderPath([Environment+SpecialFolder]::CommonApplicationData)
    if ([string]::IsNullOrWhiteSpace($programData)) { $programData = $env:ProgramData }
    return Join-Path $programData 'WenzWork\DeviceAgent'
}

function Get-AgentDataRoot { return Join-Path (Get-AgentApplicationRoot) 'data' }
function Get-AgentConfigRoot { return Join-Path (Get-AgentApplicationRoot) 'config' }
function Get-AgentBackupRoot { return Join-Path (Get-AgentApplicationRoot) 'backups' }

function Resolve-AgentManagedRoot {
    param(
        [Parameter(Mandatory = $true)][string]$Path,
        [Parameter(Mandatory = $true)][string]$Label
    )
    try { return Resolve-RelayInstallRoot -Path $Path }
    catch { throw "$Label is unsafe: $($_.Exception.Message)" }
}

function Get-AgentInstalledRoot {
    $metadata = Join-Path (Get-AgentConfigRoot) 'install.json'
    if (-not (Test-Path -LiteralPath $metadata -PathType Leaf)) {
        return Resolve-AgentManagedRoot -Path (Get-AgentDefaultInstallRoot) -Label 'Default install root'
    }
    if ((Get-Item -LiteralPath $metadata -Force).Attributes -band [IO.FileAttributes]::ReparsePoint) {
        throw 'Device Agent install metadata must not be a reparse point.'
    }
    try { $document = [IO.File]::ReadAllText($metadata) | ConvertFrom-Json }
    catch { throw "Device Agent install metadata is invalid: $($_.Exception.Message)" }
    if ($document.schemaVersion -ne 1 -or [string]::IsNullOrWhiteSpace($document.installRoot)) {
        throw 'Device Agent install metadata has an unsupported schema.'
    }
    return Resolve-AgentManagedRoot -Path $document.installRoot -Label 'Installed root'
}

function Write-AgentInstallMetadata {
    param([Parameter(Mandatory = $true)][string]$InstallRoot)
    $root = Resolve-AgentManagedRoot -Path $InstallRoot -Label 'Install root'
    $configRoot = Get-AgentConfigRoot
    if (-not (Test-Path -LiteralPath $configRoot -PathType Container)) {
        [void](New-Item -ItemType Directory -Path $configRoot -Force)
    }
    $document = [ordered]@{ schemaVersion = 1; installRoot = $root }
    Write-RelayAtomicText -Path (Join-Path $configRoot 'install.json') -Text (($document | ConvertTo-Json -Compress) + "`r`n")
    Protect-AgentPath -Path (Join-Path $configRoot 'install.json') -Directory:$false
}

function Assert-AgentAdministrator { Assert-RelayAdministrator }
function Get-AgentHostArchitecture { return Get-RelayHostArchitecture }

function Assert-AgentTrustedVerifier {
    param(
        [Parameter(Mandatory = $true)][string]$VerifierFile,
        [string]$VerifierSha256,
        [switch]$RequireHash
    )
    $trusted = Assert-RelayTrustedVerifier -VerifierFile $VerifierFile -VerifierSha256 $VerifierSha256 -RequireHash:$RequireHash
    $signature = Get-AuthenticodeSignature -FilePath $trusted
    if ($signature.Status -ne [Management.Automation.SignatureStatus]::Valid -or $null -eq $signature.SignerCertificate) {
        throw "Trusted release verifier has no valid Authenticode signature ($($signature.Status))."
    }
    return $trusted
}

function New-AgentTempDirectory { return New-RelayTempDirectory }
function Remove-AgentTempDirectory { param([string]$Path) Remove-RelayTempDirectory -Path $Path }

function Assert-AgentNetworkUrl {
    param([Parameter(Mandatory = $true)][string]$Url)
    if ($Url.Length -gt 2048 -or $Url -cne $Url.Trim()) { throw 'Control Plane URL is invalid.' }
    $uri = $null
    if (-not [Uri]::TryCreate($Url, [UriKind]::Absolute, [ref]$uri) -or $null -eq $uri -or
        [string]::IsNullOrWhiteSpace($uri.Host) -or -not [string]::IsNullOrEmpty($uri.UserInfo) -or
        -not [string]::IsNullOrEmpty($uri.Query) -or -not [string]::IsNullOrEmpty($uri.Fragment)) {
        throw 'Control Plane URL is invalid.'
    }
    if ($uri.Scheme -ceq 'http' -or $uri.Scheme -ceq 'https') {
        return $uri.AbsoluteUri
    }
    throw 'Control Plane URL must use HTTP or HTTPS.'
}

function Find-AgentPackageRoot {
    param([Parameter(Mandatory = $true)][string]$Root)
    if (Test-Path -LiteralPath (Join-Path $Root 'bin\wenzwork-device-agent.exe') -PathType Leaf) {
        return [IO.Path]::GetFullPath($Root)
    }
    $candidates = @(Get-ChildItem -LiteralPath $Root -Force -Directory | Where-Object {
        Test-Path -LiteralPath (Join-Path $_.FullName 'bin\wenzwork-device-agent.exe') -PathType Leaf
    })
    if ($candidates.Count -ne 1) { throw 'Archive must contain exactly one Device Agent package root.' }
    return $candidates[0].FullName
}

function Resolve-AgentPackageSource {
    param(
        [string]$PackageDirectory,
        [string]$PackageFile,
        [string]$ArtifactUrl,
        [string]$ChecksumsFile,
        [string]$ChecksumsUrl,
        [string]$ChecksumsSignatureFile,
        [string]$ChecksumsSignatureUrl,
        [Parameter(Mandatory = $true)][string]$SigningKeyFile,
        [Parameter(Mandatory = $true)][string]$VerifierFile,
        [Parameter(Mandatory = $true)][string]$WorkDirectory
    )
    $sourceCount = @(@($PackageDirectory, $PackageFile, $ArtifactUrl) | Where-Object { -not [string]::IsNullOrWhiteSpace($_) }).Count
    if ($sourceCount -ne 1) { throw 'Select exactly one Device Agent package source.' }
    if (-not [string]::IsNullOrWhiteSpace($PackageDirectory)) {
        if (-not (Test-Path -LiteralPath $PackageDirectory -PathType Container)) { throw 'Package directory does not exist.' }
        return Find-AgentPackageRoot -Root $PackageDirectory
    }

    $archive = $PackageFile
    if (-not [string]::IsNullOrWhiteSpace($ArtifactUrl)) {
        $uri = [Uri](Assert-RelayNetworkUrl -Url $ArtifactUrl)
        $name = [IO.Path]::GetFileName($uri.AbsolutePath)
        if ($name -notmatch '\.(tar\.gz|tgz|zip)$') { throw 'Artifact URL must end with .tar.gz, .tgz, or .zip.' }
        $archive = Join-Path $WorkDirectory $name
        Invoke-RelayDownload -Url $uri.AbsoluteUri -Destination $archive
    }
    if (-not (Test-Path -LiteralPath $archive -PathType Leaf)) { throw 'Device Agent package archive does not exist.' }
    if (-not [string]::IsNullOrWhiteSpace($ChecksumsUrl)) {
        if (-not [string]::IsNullOrWhiteSpace($ChecksumsFile)) { throw 'Select one SHA256SUMS source.' }
        $ChecksumsFile = Join-Path $WorkDirectory 'SHA256SUMS'
        Invoke-RelayDownload -Url $ChecksumsUrl -Destination $ChecksumsFile
    }
    if (-not [string]::IsNullOrWhiteSpace($ChecksumsSignatureUrl)) {
        if (-not [string]::IsNullOrWhiteSpace($ChecksumsSignatureFile)) { throw 'Select one SHA256SUMS signature source.' }
        $ChecksumsSignatureFile = Join-Path $WorkDirectory 'SHA256SUMS.sig'
        Invoke-RelayDownload -Url $ChecksumsSignatureUrl -Destination $ChecksumsSignatureFile
    }
    if ([string]::IsNullOrWhiteSpace($ChecksumsFile) -or [string]::IsNullOrWhiteSpace($ChecksumsSignatureFile)) {
        throw 'Archive installation requires SHA256SUMS and its signature.'
    }
    Assert-RelayBundle -VerifierFile $VerifierFile -Archive $archive -ChecksumsFile $ChecksumsFile `
        -ChecksumsSignatureFile $ChecksumsSignatureFile -SigningKeyFile $SigningKeyFile
    $extract = Join-Path $WorkDirectory 'package'
    Expand-RelayArchive -Archive $archive -Destination $extract
    return Find-AgentPackageRoot -Root $extract
}

function Read-AgentVersion {
    param([Parameter(Mandatory = $true)][string]$PackageRoot)
    $path = Join-Path $PackageRoot 'VERSION'
    if (-not (Test-Path -LiteralPath $path -PathType Leaf) -or (Get-Item -LiteralPath $path).Length -gt 128) {
        throw 'Device Agent VERSION is missing or invalid.'
    }
    $version = [IO.File]::ReadAllText($path).Trim()
    if ($version -notmatch '^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$') { throw 'Device Agent VERSION is invalid.' }
    return $version
}

function Assert-AgentReleaseTree {
    param(
        [Parameter(Mandatory = $true)][string]$PackageRoot,
        [Parameter(Mandatory = $true)][string]$VerifierFile,
        [Parameter(Mandatory = $true)][string]$HostArchitecture,
        [Parameter(Mandatory = $true)][string]$SigningKeyFile,
        [string]$ExpectedVersion
    )
    $required = @(
        'bin\wenzwork-device-agent.exe', 'bin\relayctl.exe', 'scripts\Install.ps1', 'scripts\Upgrade.ps1',
        'scripts\Healthcheck.ps1', 'scripts\Uninstall.ps1', 'scripts\lib\DeviceAgentCommon.psm1',
        'scripts\lib\RelayCommon.psm1', 'VERSION', 'release-manifest.json',
        'release-signing-public-key.pem', 'device-agent.env.example'
    )
    foreach ($relative in $required) {
        $path = Join-Path $PackageRoot $relative
        if (-not (Test-Path -LiteralPath $path -PathType Leaf) -or ((Get-Item -LiteralPath $path -Force).Attributes -band [IO.FileAttributes]::ReparsePoint)) {
            throw "Device Agent package is missing a regular file: $relative"
        }
    }
    foreach ($relative in @('bin\wenzwork-device-agent.exe', 'bin\relayctl.exe')) {
        $signature = Get-AuthenticodeSignature -FilePath (Join-Path $PackageRoot $relative)
        if ($signature.Status -ne [Management.Automation.SignatureStatus]::Valid -or $null -eq $signature.SignerCertificate) {
            throw "Device Agent executable has no valid Authenticode signature: $relative ($($signature.Status))."
        }
    }
    $reparse = @(Get-ChildItem -LiteralPath $PackageRoot -Recurse -Force | Where-Object { $_.Attributes -band [IO.FileAttributes]::ReparsePoint })
    if ($reparse.Count -ne 0) { throw 'Device Agent release tree contains a reparse point.' }
    $version = Read-AgentVersion -PackageRoot $PackageRoot
    if (-not [string]::IsNullOrWhiteSpace($ExpectedVersion) -and $version -cne $ExpectedVersion) { throw 'Device Agent release version is unexpected.' }
    $architecture = ConvertTo-RelayArchitecture -Architecture $HostArchitecture
    [void](Assert-RelayManifestTarget -ManifestPath (Join-Path $PackageRoot 'release-manifest.json') `
        -HostArchitecture $architecture -ExpectedVersion $version)
    $trustedHash = (Get-FileHash -LiteralPath $SigningKeyFile -Algorithm SHA256).Hash
    $packagedHash = (Get-FileHash -LiteralPath (Join-Path $PackageRoot 'release-signing-public-key.pem') -Algorithm SHA256).Hash
    if (-not $trustedHash.Equals($packagedHash, [StringComparison]::OrdinalIgnoreCase)) {
        throw 'Packaged Release public key does not match the trusted key.'
    }
    Invoke-RelayVerifier -VerifierFile $VerifierFile -Arguments @(
        'release', 'verify', '--root', [IO.Path]::GetFullPath($PackageRoot), '--manifest', 'release-manifest.json',
        '--expected-version', $version, '--expected-platform', 'windows', '--expected-architecture', $architecture, '--protocol-version', '1'
    )
    return $version
}

function Install-AgentReleaseTree {
    param(
        [Parameter(Mandatory = $true)][string]$PackageRoot,
        [Parameter(Mandatory = $true)][string]$InstallRoot,
        [Parameter(Mandatory = $true)][string]$Version,
        [Parameter(Mandatory = $true)][string]$VerifierFile,
        [Parameter(Mandatory = $true)][string]$HostArchitecture,
        [Parameter(Mandatory = $true)][string]$SigningKeyFile
    )
    $root = Resolve-AgentManagedRoot -Path $InstallRoot -Label 'Install root'
    $releases = Join-Path $root 'releases'
    if (-not (Test-Path -LiteralPath $releases)) { [void](New-Item -ItemType Directory -Path $releases -Force) }
    $destination = Join-Path $releases $Version
    if (Test-Path -LiteralPath $destination) {
        $item = Get-Item -LiteralPath $destination -Force
        if (-not $item.PSIsContainer -or ($item.Attributes -band [IO.FileAttributes]::ReparsePoint)) { throw 'Existing release destination is unsafe.' }
        [void](Assert-AgentReleaseTree -PackageRoot $destination -VerifierFile $VerifierFile -HostArchitecture $HostArchitecture -SigningKeyFile $SigningKeyFile -ExpectedVersion $Version)
        return $destination
    }
    $stage = Join-Path $releases ('.stage.' + [Guid]::NewGuid().ToString('N'))
    try {
        [void](New-Item -ItemType Directory -Path $stage)
        Get-ChildItem -LiteralPath $PackageRoot -Force | Copy-Item -Destination $stage -Recurse -Force
        [void](Assert-AgentReleaseTree -PackageRoot $stage -VerifierFile $VerifierFile -HostArchitecture $HostArchitecture -SigningKeyFile $SigningKeyFile -ExpectedVersion $Version)
        [IO.Directory]::Move($stage, $destination)
    }
    finally {
        if (Test-Path -LiteralPath $stage) { Remove-Item -LiteralPath $stage -Recurse -Force }
    }
    return $destination
}

function Assert-AgentEnvironmentFile {
    param(
        [Parameter(Mandatory = $true)][string]$Path,
        [Parameter(Mandatory = $true)][string]$ExpectedStatePath
    )
    if (-not (Test-Path -LiteralPath $Path -PathType Leaf) -or ((Get-Item -LiteralPath $Path -Force).Attributes -band [IO.FileAttributes]::ReparsePoint)) {
        throw 'Device Agent environment must be a regular file.'
    }
    $allowed = @('WENZWORK_CONTROL_URL', 'WENZWORK_DEVICE_ACCESS_KEY', 'WENZWORK_DEVICE_STATE_FILE', 'WENZWORK_DEVICE_WORKSPACE', 'WENZWORK_AGENT_SECRET_STORE', 'WENZWORK_AGENT_FEATURE_FLAGS', 'WENZWORK_DEVICE_TLS_CA_FILE')
    $values = @{}
    foreach ($line in [IO.File]::ReadAllLines($Path)) {
        if ([string]::IsNullOrWhiteSpace($line) -or $line.TrimStart().StartsWith('#')) { continue }
        if ($line -notmatch '^([A-Z0-9_]+)=(.*)$') { throw 'Device Agent environment contains an invalid line.' }
        $key = $Matches[1]
        $value = $Matches[2]
        if ($allowed -notcontains $key -or $values.ContainsKey($key)) { throw "Device Agent environment key is forbidden or duplicated: $key" }
        $values[$key] = $value
    }
    foreach ($required in @('WENZWORK_CONTROL_URL', 'WENZWORK_DEVICE_ACCESS_KEY', 'WENZWORK_DEVICE_STATE_FILE', 'WENZWORK_AGENT_SECRET_STORE')) {
        if (-not $values.ContainsKey($required)) { throw "Device Agent environment is missing $required." }
    }
    [void](Assert-AgentNetworkUrl -Url $values.WENZWORK_CONTROL_URL)
    if ($values.WENZWORK_DEVICE_ACCESS_KEY -notmatch '^device_[A-Za-z0-9_-]{43}$') { throw 'Device Access Key is invalid.' }
    $actualState = [IO.Path]::GetFullPath($values.WENZWORK_DEVICE_STATE_FILE)
    $expectedState = [IO.Path]::GetFullPath($ExpectedStatePath)
    if (-not $actualState.Equals($expectedState, [StringComparison]::OrdinalIgnoreCase)) { throw "State file must be $expectedState." }
    if ($values.WENZWORK_AGENT_SECRET_STORE -notin @('native', 'file')) { throw 'Windows service SecretStore must be native or file.' }
    return $values
}

function Protect-AgentPath {
    param(
        [Parameter(Mandatory = $true)][string]$Path,
        [switch]$Directory
    )
    $arguments = @($Path, '/inheritance:r', '/grant:r', '*S-1-5-18:(F)', '*S-1-5-32-544:(F)')
    if ($Directory) { $arguments += @('/t', '/c') }
    $output = @(& (Join-Path $env:SystemRoot 'System32\icacls.exe') @arguments 2>&1)
    if ($LASTEXITCODE -ne 0) { throw "Could not protect Device Agent path $Path`: $($output -join [Environment]::NewLine)" }
}

function Install-AgentEnvironmentFile {
    param(
        [Parameter(Mandatory = $true)][string]$Source,
        [Parameter(Mandatory = $true)][string]$Destination,
        [Parameter(Mandatory = $true)][string]$ExpectedStatePath
    )
    [void](Assert-AgentEnvironmentFile -Path $Source -ExpectedStatePath $ExpectedStatePath)
    Write-RelayAtomicBytes -Path $Destination -Bytes ([IO.File]::ReadAllBytes($Source))
    Protect-AgentPath -Path $Destination
}

function Invoke-AgentSc {
    param([Parameter(Mandatory = $true)][string[]]$Arguments)
    Invoke-RelaySc -Arguments $Arguments
}

function Test-AgentServiceExists { return $null -ne (Get-Service -Name $script:AgentServiceName -ErrorAction SilentlyContinue) }

function ConvertTo-AgentServiceImagePath {
    param(
        [Parameter(Mandatory = $true)][string]$BinaryPath,
        [Parameter(Mandatory = $true)][string]$EnvironmentFile
    )
    foreach ($value in @($BinaryPath, $EnvironmentFile)) {
        if ($value.IndexOf('"') -ge 0 -or $value.IndexOfAny([char[]]"`0`r`n") -ge 0) { throw 'Service path contains an unsafe character.' }
    }
    return ('"{0}" service --env-file "{1}"' -f [IO.Path]::GetFullPath($BinaryPath), [IO.Path]::GetFullPath($EnvironmentFile))
}

function Install-AgentService {
    param(
        [Parameter(Mandatory = $true)][string]$BinaryPath,
        [Parameter(Mandatory = $true)][string]$EnvironmentFile
    )
    if (Test-AgentServiceExists) { throw 'WenzWorkDeviceAgent is already installed; use Upgrade.ps1.' }
    $image = ConvertTo-AgentServiceImagePath -BinaryPath $BinaryPath -EnvironmentFile $EnvironmentFile
    Invoke-AgentSc -Arguments @('create', $script:AgentServiceName, 'binPath=', $image, 'start=', 'auto', 'DisplayName=', $script:AgentDisplayName)
    Invoke-AgentSc -Arguments @('description', $script:AgentServiceName, 'WenzWork end-to-end encrypted remote device agent')
    Invoke-AgentSc -Arguments @('failure', $script:AgentServiceName, 'reset=', '86400', 'actions=', 'restart/5000/restart/15000/restart/60000')
    Invoke-AgentSc -Arguments @('failureflag', $script:AgentServiceName, '1')
    Invoke-AgentSc -Arguments @('sidtype', $script:AgentServiceName, 'unrestricted')
}

function Set-AgentServiceBinary {
    param(
        [Parameter(Mandatory = $true)][string]$BinaryPath,
        [Parameter(Mandatory = $true)][string]$EnvironmentFile
    )
    $image = ConvertTo-AgentServiceImagePath -BinaryPath $BinaryPath -EnvironmentFile $EnvironmentFile
    Invoke-AgentSc -Arguments @('config', $script:AgentServiceName, 'binPath=', $image, 'start=', 'auto')
}

function Wait-AgentServiceState {
    param([Parameter(Mandatory = $true)][ValidateSet('Running', 'Stopped')][string]$State, [int]$WaitSeconds = 30)
    $deadline = [DateTime]::UtcNow.AddSeconds($WaitSeconds)
    do {
        $service = Get-Service -Name $script:AgentServiceName -ErrorAction Stop
        if ($service.Status.ToString() -eq $State) { return }
        Start-Sleep -Milliseconds 250
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "WenzWorkDeviceAgent did not reach $State within $WaitSeconds seconds."
}

function Start-AgentService {
    param([int]$WaitSeconds = 30)
    $service = Get-Service -Name $script:AgentServiceName -ErrorAction Stop
    if ($service.Status -ne [ServiceProcess.ServiceControllerStatus]::Running) { Start-Service -Name $script:AgentServiceName }
    Wait-AgentServiceState -State Running -WaitSeconds $WaitSeconds
}

function Stop-AgentService {
    param([int]$WaitSeconds = 45)
    $service = Get-Service -Name $script:AgentServiceName -ErrorAction Stop
    if ($service.Status -ne [ServiceProcess.ServiceControllerStatus]::Stopped) { Stop-Service -Name $script:AgentServiceName -Force }
    Wait-AgentServiceState -State Stopped -WaitSeconds $WaitSeconds
}

function Remove-AgentService {
    if (-not (Test-AgentServiceExists)) { return }
    try { Stop-AgentService -WaitSeconds 45 } catch { }
    Invoke-AgentSc -Arguments @('delete', $script:AgentServiceName)
    $deadline = [DateTime]::UtcNow.AddSeconds(30)
    while ((Test-AgentServiceExists) -and [DateTime]::UtcNow -lt $deadline) { Start-Sleep -Milliseconds 250 }
    if (Test-AgentServiceExists) { throw 'WenzWorkDeviceAgent service deletion did not complete.' }
}

function Test-AgentHealth {
    param(
        [Parameter(Mandatory = $true)][string]$ReleasePath,
        [ValidateRange(0, 300)][int]$WaitSeconds = 0
    )
    $version = Read-AgentVersion -PackageRoot $ReleasePath
    $binary = Join-Path $ReleasePath 'bin\wenzwork-device-agent.exe'
    $deadline = [DateTime]::UtcNow.AddSeconds($WaitSeconds)
    do {
        try {
            $service = Get-Service -Name $script:AgentServiceName -ErrorAction Stop
            $reported = @(& $binary version 2>$null)
            if ($LASTEXITCODE -eq 0 -and $service.Status -eq [ServiceProcess.ServiceControllerStatus]::Running -and ($reported -join '').Trim() -ceq $version) {
                Start-Sleep -Seconds 2
                if ((Get-Service -Name $script:AgentServiceName).Status -eq [ServiceProcess.ServiceControllerStatus]::Running) { return $true }
            }
        }
        catch {
            if ([DateTime]::UtcNow -ge $deadline) { throw }
        }
        if ([DateTime]::UtcNow -ge $deadline) { throw 'Device Agent health check failed.' }
        Start-Sleep -Seconds 1
    } while ($true)
}

function Write-AgentCurrentMetadata {
    param(
        [Parameter(Mandatory = $true)][string]$InstallRoot,
        [Parameter(Mandatory = $true)][string]$Version,
        [Parameter(Mandatory = $true)][string]$ReleasePath
    )
    $document = [ordered]@{ schemaVersion = 1; serviceName = $script:AgentServiceName; version = $Version; releasePath = [IO.Path]::GetFullPath($ReleasePath) }
    Write-RelayAtomicText -Path (Join-Path $InstallRoot 'current.json') -Text (($document | ConvertTo-Json -Compress) + "`r`n")
}

function Assert-AgentRegularDataTree {
    param(
        [Parameter(Mandatory = $true)][string]$Path,
        [Parameter(Mandatory = $true)][string]$Label
    )
    if (-not (Test-Path -LiteralPath $Path -PathType Container) -or
        ((Get-Item -LiteralPath $Path -Force).Attributes -band [IO.FileAttributes]::ReparsePoint)) {
        throw "$Label must be a real directory."
    }
    $reparse = @(Get-ChildItem -LiteralPath $Path -Recurse -Force | Where-Object { $_.Attributes -band [IO.FileAttributes]::ReparsePoint })
    if ($reparse.Count -ne 0) { throw "$Label contains a junction, symbolic link, or other reparse point." }
    $hardLinks = @(Get-ChildItem -LiteralPath $Path -Recurse -Force -File | Where-Object { $_.LinkType -eq 'HardLink' })
    if ($hardLinks.Count -ne 0) { throw "$Label contains a multiply linked file." }
}

function New-AgentBackup {
    param(
        [Parameter(Mandatory = $true)][string]$DataRoot,
        [Parameter(Mandatory = $true)][string]$EnvironmentFile,
        [Parameter(Mandatory = $true)][string]$SourceVersion
    )
    $data = Resolve-AgentManagedRoot -Path $DataRoot -Label 'Data root'
    $backupRoot = Resolve-AgentManagedRoot -Path (Get-AgentBackupRoot) -Label 'Backup root'
    if (-not (Test-Path -LiteralPath $data -PathType Container) -or -not (Test-Path -LiteralPath $EnvironmentFile -PathType Leaf)) {
        throw 'Managed data and environment must exist before backup.'
    }
    Assert-AgentRegularDataTree -Path $data -Label 'Managed Agent data'
    if ((Get-Item -LiteralPath $EnvironmentFile -Force).Attributes -band [IO.FileAttributes]::ReparsePoint) {
        throw 'Managed Agent environment must not be a reparse point.'
    }
    if (-not (Test-Path -LiteralPath $backupRoot)) { [void](New-Item -ItemType Directory -Path $backupRoot -Force) }
    $name = ([DateTime]::UtcNow.ToString('yyyyMMddTHHmmssZ') + '-' + $SourceVersion)
    $destination = Join-Path $backupRoot $name
    if (Test-Path -LiteralPath $destination) { $destination += '-' + [Guid]::NewGuid().ToString('N') }
    $stage = Join-Path $backupRoot ('.backup.' + [Guid]::NewGuid().ToString('N'))
    try {
        [void](New-Item -ItemType Directory -Path (Join-Path $stage 'data') -Force)
        [void](New-Item -ItemType Directory -Path (Join-Path $stage 'config') -Force)
        Get-ChildItem -LiteralPath $data -Force | Copy-Item -Destination (Join-Path $stage 'data') -Recurse -Force
        Copy-Item -LiteralPath $EnvironmentFile -Destination (Join-Path $stage 'config\agent.env') -Force
        Assert-AgentRegularDataTree -Path (Join-Path $stage 'data') -Label 'Staged Agent backup'
        [IO.File]::WriteAllText((Join-Path $stage 'BACKUP-METADATA'), "schemaVersion=1`r`nsourceVersion=$SourceVersion`r`ncreatedAt=$([DateTime]::UtcNow.ToString('O'))`r`n", (New-Object Text.UTF8Encoding($false)))
        [IO.Directory]::Move($stage, $destination)
    }
    finally {
        if (Test-Path -LiteralPath $stage) { Remove-Item -LiteralPath $stage -Recurse -Force }
    }
    Protect-AgentPath -Path $destination -Directory
    return $destination
}

function Restore-AgentBackup {
    param(
        [Parameter(Mandatory = $true)][string]$BackupPath,
        [Parameter(Mandatory = $true)][string]$DataRoot,
        [Parameter(Mandatory = $true)][string]$EnvironmentFile
    )
    $backup = Resolve-AgentManagedRoot -Path $BackupPath -Label 'Backup path'
    $backupPrefix = [IO.Path]::GetFullPath((Get-AgentBackupRoot)).TrimEnd('\') + '\'
    if (-not $backup.StartsWith($backupPrefix, [StringComparison]::OrdinalIgnoreCase)) {
        throw 'Backup path is outside the managed backup root.'
    }
    $backupData = Join-Path $backup 'data'
    $backupEnvironment = Join-Path $backup 'config\agent.env'
    $backupMetadata = Join-Path $backup 'BACKUP-METADATA'
    if (-not (Test-Path -LiteralPath $backupData -PathType Container) -or
        -not (Test-Path -LiteralPath $backupEnvironment -PathType Leaf) -or
        -not (Test-Path -LiteralPath $backupMetadata -PathType Leaf)) {
        throw 'Backup is incomplete.'
    }
    foreach ($requiredPath in @($backupData, $backupEnvironment, $backupMetadata)) {
        if ((Get-Item -LiteralPath $requiredPath -Force).Attributes -band [IO.FileAttributes]::ReparsePoint) {
            throw "Backup contains an unsafe path: $requiredPath"
        }
    }
    Assert-AgentRegularDataTree -Path $backupData -Label 'Agent backup data'
    $data = Resolve-AgentManagedRoot -Path $DataRoot -Label 'Data root'
    [void](Assert-AgentEnvironmentFile -Path $backupEnvironment -ExpectedStatePath (Join-Path $data 'state\agent-state.json'))
    $suffix = [DateTime]::UtcNow.ToString('yyyyMMddTHHmmssZ') + '.' + [Guid]::NewGuid().ToString('N')
    $failed = $data + '.failed.' + $suffix
    $restore = $data + '.restore.' + $suffix
    $restoreFailed = $data + '.restore-failed.' + $suffix
    foreach ($candidate in @($failed, $restore, $restoreFailed)) {
        if (Test-Path -LiteralPath $candidate) { throw "Rollback staging path already exists: $candidate" }
    }

    $movedOriginal = $false
    try {
        [void](New-Item -ItemType Directory -Path $restore)
        Get-ChildItem -LiteralPath $backupData -Force | Copy-Item -Destination $restore -Recurse -Force
        Assert-AgentRegularDataTree -Path $restore -Label 'Staged Agent restore'
        Protect-AgentPath -Path $restore -Directory
        $environmentBytes = [IO.File]::ReadAllBytes($backupEnvironment)

        if (Test-Path -LiteralPath $data) {
            [IO.Directory]::Move($data, $failed)
            $movedOriginal = $true
        }
        try {
            [IO.Directory]::Move($restore, $data)
        }
        catch {
            $activationError = $_
            if ($movedOriginal -and -not (Test-Path -LiteralPath $data)) {
                try { [IO.Directory]::Move($failed, $data) }
                catch { throw "Could not activate the staged restore or put the original data back. Activation: $($activationError.Exception.Message). Recovery: $($_.Exception.Message)" }
            }
            throw "Could not activate the staged restore; original data was left in place: $($activationError.Exception.Message)"
        }

        try {
            Write-RelayAtomicBytes -Path $EnvironmentFile -Bytes $environmentBytes
            Protect-AgentPath -Path $EnvironmentFile
        }
        catch {
            $environmentError = $_
            try {
                [IO.Directory]::Move($data, $restoreFailed)
                if ($movedOriginal) { [IO.Directory]::Move($failed, $data) }
            }
            catch {
                throw "Environment restore failed and original data could not be put back. Environment: $($environmentError.Exception.Message). Recovery: $($_.Exception.Message)"
            }
            throw "Environment restore failed; original data was put back: $($environmentError.Exception.Message)"
        }

        Assert-AgentRegularDataTree -Path $data -Label 'Restored Agent data'
        if ($movedOriginal) {
            Write-AgentLog "Failed-upgrade data retained at $failed for diagnostics."
            return $failed
        }
        return $null
    }
    finally {
        if (Test-Path -LiteralPath $restore) { Remove-Item -LiteralPath $restore -Recurse -Force }
    }
}

function Remove-AgentOldBackups {
    param([ValidateRange(1, 100)][int]$Keep = 5)
    $backupRoot = Resolve-AgentManagedRoot -Path (Get-AgentBackupRoot) -Label 'Backup root'
    if (-not (Test-Path -LiteralPath $backupRoot -PathType Container)) { return }
    $backups = @(Get-ChildItem -LiteralPath $backupRoot -Directory -Force | Where-Object {
        -not $_.Name.StartsWith('.backup.') -and (Test-Path -LiteralPath (Join-Path $_.FullName 'BACKUP-METADATA') -PathType Leaf)
    } | Sort-Object Name -Descending)
    foreach ($backup in @($backups | Select-Object -Skip $Keep)) {
        $expectedParent = [IO.Path]::GetFullPath($backupRoot).TrimEnd('\') + '\'
        if (-not $backup.FullName.StartsWith($expectedParent, [StringComparison]::OrdinalIgnoreCase) -or ($backup.Attributes -band [IO.FileAttributes]::ReparsePoint)) {
            throw 'Refusing to prune an unsafe backup path.'
        }
        Remove-Item -LiteralPath $backup.FullName -Recurse -Force
    }
}

function Remove-AgentInstalledBinaries {
    param([Parameter(Mandatory = $true)][string]$InstallRoot)
    $root = Resolve-AgentManagedRoot -Path $InstallRoot -Label 'Install root'
    if (Test-Path -LiteralPath $root) { Remove-Item -LiteralPath $root -Recurse -Force }
}

function Remove-AgentAllData {
    $root = Resolve-AgentManagedRoot -Path (Get-AgentApplicationRoot) -Label 'Application data root'
    if (Test-Path -LiteralPath $root) { Remove-Item -LiteralPath $root -Recurse -Force }
}

Export-ModuleMember -Function *-Agent*
