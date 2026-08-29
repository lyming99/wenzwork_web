#Requires -Version 5.1

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
Import-Module (Join-Path $PSScriptRoot 'lib\DeviceAgentCommon.psm1') -Force
Import-Module (Join-Path $PSScriptRoot '..\..\relay\windows\lib\RelayCommon.psm1') -Force

function Assert-True {
    param([bool]$Condition, [string]$Message)
    if (-not $Condition) { throw "DeviceAgentScripts.Tests: $Message" }
}

function Assert-Throws {
    param([scriptblock]$Action, [string]$Message)
    try { & $Action; throw "DeviceAgentScripts.Tests: command unexpectedly succeeded: $Message" }
    catch {
        if ($_.Exception.Message.StartsWith('DeviceAgentScripts.Tests: command unexpectedly succeeded:')) { throw }
    }
}

Assert-True ((Assert-AgentNetworkUrl -Url 'https://control.example.test') -eq 'https://control.example.test/') 'HTTPS URL was rejected'
Assert-True ((Assert-AgentNetworkUrl -Url 'http://localhost:8080') -eq 'http://localhost:8080/') 'loopback HTTP URL was rejected'
Assert-True ((Assert-AgentNetworkUrl -Url 'http://control.example.test:8080') -eq 'http://control.example.test:8080/') 'remote HTTP URL was rejected'
Assert-Throws { Assert-AgentNetworkUrl -Url 'ftp://control.example.test' } 'non-HTTP URL'
Assert-Throws { Assert-AgentNetworkUrl -Url 'https://user:pass@control.example.test' } 'URL credentials'
Assert-Throws { Assert-AgentNetworkUrl -Url 'https://control.example.test?token=secret' } 'URL query'
Assert-Throws { Assert-AgentNetworkUrl -Url 'https:///missing-host' } 'missing URL host'

$image = ConvertTo-AgentServiceImagePath -BinaryPath 'C:\Program Files\WenzWork\DeviceAgent\agent.exe' -EnvironmentFile 'C:\ProgramData\WenzWork\DeviceAgent\config\agent.env'
Assert-True ($image.Contains(' service --env-file ')) 'SCM ImagePath does not use the native service entry point'
Assert-True (-not $image.Contains('device_')) 'SCM ImagePath contains credential material'
Assert-Throws { ConvertTo-AgentServiceImagePath -BinaryPath 'C:\bad"path\agent.exe' -EnvironmentFile 'C:\safe\agent.env' } 'quoted service path'

$testRoot = Join-Path ([IO.Path]::GetTempPath()) ('wenzwork-device-agent-test-' + [Guid]::NewGuid().ToString('N'))
try {
    [void](New-Item -ItemType Directory -Path $testRoot)
    $dataRoot = Join-Path $testRoot 'data'
    $statePath = Join-Path $dataRoot 'state\agent-state.json'
    [void](New-Item -ItemType Directory -Path (Split-Path -Parent $statePath) -Force)
    $junctionTarget = Join-Path $testRoot 'junction-target'
    $unsafeJunction = Join-Path $dataRoot 'unsafe-junction'
    [void](New-Item -ItemType Directory -Path $junctionTarget)
    [void](New-Item -ItemType Junction -Path $unsafeJunction -Target $junctionTarget)
    Assert-Throws { Assert-AgentRegularDataTree -Path $dataRoot -Label 'Test data' } 'junction inside data tree'
    Remove-Item -LiteralPath $unsafeJunction -Force
    Assert-AgentRegularDataTree -Path $dataRoot -Label 'Test data'
    $hardLinkSource = Join-Path $dataRoot 'hardlink-source.txt'
    $unsafeHardLink = Join-Path $dataRoot 'unsafe-hardlink.txt'
    [IO.File]::WriteAllText($hardLinkSource, 'hardlink fixture')
    [void](New-Item -ItemType HardLink -Path $unsafeHardLink -Target $hardLinkSource)
    Assert-Throws { Assert-AgentRegularDataTree -Path $dataRoot -Label 'Test data' } 'hardlink inside data tree'
    Remove-Item -LiteralPath $unsafeHardLink -Force
    Remove-Item -LiteralPath $hardLinkSource -Force
    Assert-AgentRegularDataTree -Path $dataRoot -Label 'Test data'
    $accessKey = 'device_' + ('k' * 43)
    $environment = Join-Path $testRoot 'agent.env'
    [IO.File]::WriteAllLines($environment, @(
        'WENZWORK_CONTROL_URL=http://control.example.test:8080',
        "WENZWORK_DEVICE_ACCESS_KEY=$accessKey",
        "WENZWORK_DEVICE_DIRECT_ACCESS_KEY=$accessKey",
        "WENZWORK_DEVICE_STATE_FILE=$statePath",
        "WENZWORK_DEVICE_WORKSPACE=$(Join-Path $dataRoot 'workspace')",
        'WENZWORK_AGENT_SECRET_STORE=native'
    ))
    $values = Assert-AgentEnvironmentFile -Path $environment -ExpectedStatePath $statePath
    Assert-True ($values.WENZWORK_AGENT_SECRET_STORE -eq 'native') 'native DPAPI SecretStore was rejected'
    $invalidDirectEnvironment = Join-Path $testRoot 'invalid-direct.env'
    (Get-Content -LiteralPath $environment) -replace '^WENZWORK_DEVICE_DIRECT_ACCESS_KEY=.*$', 'WENZWORK_DEVICE_DIRECT_ACCESS_KEY=invalid' | Set-Content -LiteralPath $invalidDirectEnvironment
    Assert-Throws { Assert-AgentEnvironmentFile -Path $invalidDirectEnvironment -ExpectedStatePath $statePath } 'invalid direct Access Key'
    Add-Content -LiteralPath $environment -Value 'UNSAFE_KEY=value'
    Assert-Throws { Assert-AgentEnvironmentFile -Path $environment -ExpectedStatePath $statePath } 'unknown environment key'

    $archiveSource = Join-Path $testRoot 'archive-source'
    [void](New-Item -ItemType Directory -Path $archiveSource)
    [IO.File]::WriteAllText((Join-Path $archiveSource 'file.txt'), 'safe')
    $tar = Join-Path $env:SystemRoot 'System32\tar.exe'
    $safeArchive = Join-Path $testRoot 'safe.tar.gz'
    & $tar -czf $safeArchive -C $archiveSource file.txt
    if ($LASTEXITCODE -ne 0) { throw 'Could not create safe tar fixture.' }
    $safeDestination = Join-Path $testRoot 'safe-extracted'
    Expand-RelayArchive -Archive $safeArchive -Destination $safeDestination
    Assert-True (([IO.File]::ReadAllText((Join-Path $safeDestination 'file.txt'))) -eq 'safe') 'safe tar archive was not extracted'
    $duplicateArchive = Join-Path $testRoot 'duplicate.tar.gz'
    & $tar -czf $duplicateArchive -C $archiveSource file.txt file.txt
    if ($LASTEXITCODE -ne 0) { throw 'Could not create duplicate tar fixture.' }
    Assert-Throws { Expand-RelayArchive -Archive $duplicateArchive -Destination (Join-Path $testRoot 'duplicate-extracted') } 'duplicate tar path'

    $package = Join-Path $testRoot 'package'
    $required = @(
        'bin\wenzwork-device-agent.exe', 'bin\relayctl.exe', 'scripts\Install.ps1', 'scripts\Upgrade.ps1',
        'scripts\Healthcheck.ps1', 'scripts\Uninstall.ps1', 'scripts\lib\DeviceAgentCommon.psm1',
        'scripts\lib\RelayCommon.psm1', 'release-signing-public-key.pem', 'device-agent.env.example'
    )
    foreach ($relative in $required) {
        $path = Join-Path $package $relative
        [void](New-Item -ItemType Directory -Path (Split-Path -Parent $path) -Force)
        [IO.File]::WriteAllText($path, 'fixture')
    }
    Copy-Item -LiteralPath (Join-Path $env:SystemRoot 'System32\whoami.exe') -Destination (Join-Path $package 'bin\wenzwork-device-agent.exe') -Force
    Copy-Item -LiteralPath (Join-Path $env:SystemRoot 'System32\whoami.exe') -Destination (Join-Path $package 'bin\relayctl.exe') -Force
    [IO.File]::WriteAllText((Join-Path $package 'VERSION'), 'v1.2.3')
    $architecture = Get-AgentHostArchitecture
    $manifest = [ordered]@{
        schemaVersion = 1; version = 'v1.2.3'; platform = 'windows'; architecture = $architecture
        protocolMin = 1; protocolMax = 1; commit = ('a' * 40); buildTimeUnix = 1; signingKeyId = 'test'; files = @()
    }
    [IO.File]::WriteAllText((Join-Path $package 'release-manifest.json'), ($manifest | ConvertTo-Json -Depth 5 -Compress))
    $trustedKey = Join-Path $testRoot 'trusted.pem'
    Copy-Item -LiteralPath (Join-Path $package 'release-signing-public-key.pem') -Destination $trustedKey
    $verifier = Join-Path $testRoot 'verifier.cmd'
    [IO.File]::WriteAllText($verifier, "@echo off`r`nexit /b 0`r`n")
    $version = Assert-AgentReleaseTree -PackageRoot $package -VerifierFile $verifier -HostArchitecture $architecture -SigningKeyFile $trustedKey
    Assert-True ($version -eq 'v1.2.3') 'valid package target was rejected'
    $manifest.architecture = if ($architecture -eq 'amd64') { 'arm64' } else { 'amd64' }
    [IO.File]::WriteAllText((Join-Path $package 'release-manifest.json'), ($manifest | ConvertTo-Json -Depth 5 -Compress))
    Assert-Throws { Assert-AgentReleaseTree -PackageRoot $package -VerifierFile $verifier -HostArchitecture $architecture -SigningKeyFile $trustedKey } 'wrong package architecture'
}
finally {
    if (Test-Path -LiteralPath $testRoot) { Remove-Item -LiteralPath $testRoot -Recurse -Force }
}

$upgradeText = Get-Content -Raw -LiteralPath (Join-Path $PSScriptRoot 'Upgrade.ps1')
$uninstallText = Get-Content -Raw -LiteralPath (Join-Path $PSScriptRoot 'Uninstall.ps1')
$moduleText = Get-Content -Raw -LiteralPath (Join-Path $PSScriptRoot 'lib\DeviceAgentCommon.psm1')
$releaseWorkflow = Get-Content -Raw -LiteralPath (Join-Path $PSScriptRoot '..\..\..\.github\workflows\release.yml')
Assert-True ($upgradeText.Contains('New-AgentBackup')) 'upgrade does not back up the complete data set'
Assert-True ($upgradeText.Contains('Restore-AgentBackup')) 'upgrade does not restore data after failure'
Assert-True ($upgradeText.Contains('Set-AgentServiceBinary')) 'upgrade does not switch the SCM ImagePath'
Assert-True ($uninstallText.Contains('Configuration, identity, secrets, backups, and all business data remain')) 'default uninstall does not preserve business data'
Assert-True ($moduleText.Contains('Assert-RelayBundle')) 'outer package is not authenticated by the trusted verifier'
Assert-True ((Get-Content -Raw -LiteralPath (Join-Path $PSScriptRoot '..\..\relay\windows\lib\RelayCommon.psm1')).Contains('archive uncompressed size exceeds 1 GiB')) 'Windows tar extraction has no uncompressed-size limit'
Assert-True ($moduleText.Contains("'release', 'verify'")) 'release manifest is not verified'
Assert-True ($moduleText.Contains('service --env-file')) 'service registration does not use an env file'
foreach ($target in @('linux-amd64', 'linux-arm64', 'windows-amd64', 'windows-arm64', 'darwin-amd64', 'darwin-arm64')) {
    Assert-True ($releaseWorkflow.Contains("wenzwork-device-agent-`$safe_tag-$target.tar.gz")) "release workflow is missing $target package"
}
Assert-True ($releaseWorkflow.Contains('signtool verify /pa /all /v')) 'Windows Authenticode signature is not verified'
Assert-True ($releaseWorkflow.Contains('codesign --verify --strict')) 'macOS Developer ID signature is not verified'
Assert-True ($releaseWorkflow.Contains('notarytool submit')) 'macOS package is not submitted for notarization'
Assert-True ($releaseWorkflow.Contains('notarytool log')) 'macOS notarization log is not audited'
Assert-True ($releaseWorkflow.Contains('spctl --assess --type execute')) 'macOS Gatekeeper acceptance is not verified'
Assert-True ($releaseWorkflow.Contains('signed native executable is missing')) 'unsigned fallback is not fail-closed'
Assert-True ($releaseWorkflow.Contains('linux_lifecycle_integration_test.sh')) 'formal Release does not exercise Linux lifecycle rollback'
Assert-True ($releaseWorkflow.Contains('filesystem_boundary_integration_test.sh')) 'formal Release does not reject unsafe filesystem mount boundaries'
Assert-True ($releaseWorkflow.Contains('disk_full_integration_test.sh')) 'formal Release does not exercise Agent state recovery on a full filesystem'
Assert-True ($releaseWorkflow.Contains('corepack pnpm check')) 'formal Release does not rerun the complete Web and Go source gate'
Assert-True ($releaseWorkflow.Contains("permissions:`n      contents: write")) 'Release upload job does not isolate write permission'
Assert-True ($releaseWorkflow.Contains('actions/workflows/ci.yml/runs')) 'formal Release does not require successful full CI for the released commit'
Assert-True ($releaseWorkflow.Contains('actions: read')) 'Release preflight cannot read CI workflow evidence'

Write-Host 'DeviceAgentScripts.Tests: PASS'
