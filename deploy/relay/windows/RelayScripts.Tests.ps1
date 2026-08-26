#Requires -Version 5.1

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
Import-Module (Join-Path $PSScriptRoot 'lib\RelayCommon.psm1') -Force

$script:Assertions = 0

function Assert-Equal {
    param($Expected, $Actual, [string]$Message)
    $script:Assertions++
    if ($Expected -ne $Actual) { throw "$Message (expected '$Expected', got '$Actual')" }
}

function Assert-True {
    param([bool]$Condition, [string]$Message)
    $script:Assertions++
    if (-not $Condition) { throw $Message }
}

function Assert-Throws {
    param([scriptblock]$Action, [string]$Message)
    $script:Assertions++
    try { & $Action; throw "Expected failure did not occur: $Message" }
    catch {
        if ($_.Exception.Message.StartsWith('Expected failure did not occur:')) { throw }
    }
}

Assert-Equal 'amd64' (ConvertTo-RelayArchitecture 'AMD64') 'AMD64 must map to amd64'
Assert-Equal 'amd64' (ConvertTo-RelayArchitecture 'x86_64') 'x86_64 must map to amd64'
Assert-Equal 'arm64' (ConvertTo-RelayArchitecture 'ARM64') 'ARM64 must map to arm64'
Assert-Equal 'arm64' (ConvertTo-RelayArchitecture 'aarch64') 'aarch64 must map to arm64'
Assert-Throws { ConvertTo-RelayArchitecture 'x86' } '32-bit Windows must be rejected'
Assert-True (@('amd64', 'arm64') -contains (Get-RelayHostArchitecture)) 'current Windows host must resolve to a supported architecture'

$safeRoot = Join-Path ([IO.Path]::GetTempPath()) 'wenzwork-relay-absolute-test'
Assert-Equal ([IO.Path]::GetFullPath($safeRoot).TrimEnd('\')) (Resolve-RelayInstallRoot $safeRoot) 'absolute install root should be normalized'
Assert-Throws { Resolve-RelayInstallRoot '.\relay' } 'relative install root must be rejected'
Assert-Throws { Resolve-RelayInstallRoot ([IO.Path]::GetPathRoot($safeRoot)) } 'drive root must be rejected'
Assert-Throws { Resolve-RelayInstallRoot $env:SystemRoot } 'Windows root must be rejected'

$defaultInstallRoot = Resolve-RelayInstallRoot (Get-RelayDefaultInstallRoot)
$nonInteractiveRoot = Read-RelayInstallRoot -Value '' -WasExplicit $false -NonInteractive -Prompt { throw 'non-interactive path selection must not prompt' }
Assert-Equal $defaultInstallRoot $nonInteractiveRoot 'non-interactive omission must use the documented default root without prompting'
$explicitRoot = Read-RelayInstallRoot -Value $safeRoot -WasExplicit $true -Prompt { throw 'explicit path selection must not prompt' }
Assert-Equal ([IO.Path]::GetFullPath($safeRoot).TrimEnd('\')) $explicitRoot 'explicit install root must be preserved without prompting'
Assert-Throws { Read-RelayInstallRoot -Value '' -WasExplicit $true -NonInteractive } 'an explicitly empty install root must not be confused with omission'
$promptCapture = New-Object PSObject -Property @{ Message = ''; Answer = '' }
$defaultPrompt = { param($Message) $promptCapture.Message = $Message; return $promptCapture.Answer }.GetNewClosure()
$interactiveDefaultRoot = Read-RelayInstallRoot -Value '' -WasExplicit $false -Prompt $defaultPrompt
Assert-Equal $defaultInstallRoot $interactiveDefaultRoot 'pressing Enter at the install-root prompt must select the default'
Assert-True ($promptCapture.Message.Contains($defaultInstallRoot)) 'interactive install-root prompt must display the clear default path'
$promptCapture.Answer = $safeRoot
$interactiveCustomRoot = Read-RelayInstallRoot -Value '' -WasExplicit $false -Prompt $defaultPrompt
Assert-Equal ([IO.Path]::GetFullPath($safeRoot).TrimEnd('\')) $interactiveCustomRoot 'interactive install-root prompt must accept a custom absolute path'

Assert-True ((Assert-RelayNetworkUrl 'https://management.example.test/relay') -like 'https://*') 'HTTPS management URL should be accepted'
Assert-True ((Assert-RelayNetworkUrl 'http://127.0.0.1:8080/dev') -like 'http://*') 'loopback development HTTP should be accepted'
Assert-True ((Assert-RelayNetworkUrl 'http://management.example.test:8080/') -like 'http://*') 'operator-selected remote HTTP should be accepted'
Assert-Throws { Assert-RelayNetworkUrl 'https://user:pass@management.example.test/' } 'URL credentials must be rejected'
Assert-Throws { Assert-RelayNetworkUrl 'https://management.example.test/?key=secret' } 'URL query must be rejected'
Assert-Throws { Assert-RelayHealthUrl 'http://192.0.2.1:19090/health/ready' } 'health checks must remain loopback-only'

$testAccessKey = 'relay_' + ('a' * 43)
$securePromptCapture = New-Object PSObject -Property @{ Message = '' }
$securePrompt = {
    param($Message)
    $securePromptCapture.Message = $Message
    return ConvertTo-SecureString $testAccessKey -AsPlainText -Force
}.GetNewClosure()
Assert-Equal $testAccessKey (Read-RelayAccessKey -SecurePrompt $securePrompt) 'interactive Access Key input must securely unwrap the hidden prompt result'
Assert-Equal 'Access Key' $securePromptCapture.Message 'interactive Access Key prompt must be explicit'
Assert-Throws { Read-RelayAccessKey -SecurePrompt { return 'plaintext' } } 'interactive Access Key prompt must return a SecureString'

$testRoot = Join-Path ([IO.Path]::GetTempPath()) ('wenzwork-relay-windows-tests-' + [Guid]::NewGuid().ToString('N'))
[void](New-Item -ItemType Directory -Path $testRoot)
try {
    foreach ($architecture in @('amd64', 'arm64')) {
        $manifestPath = Join-Path $testRoot "manifest-$architecture.json"
        $manifest = [ordered]@{
            schemaVersion = 1
            version = '1.2.3'
            platform = 'windows'
            architecture = $architecture
            protocolMin = 1
            protocolMax = 1
            commit = ('a' * 40)
            buildTimeUnix = 1
            signingKeyId = 'test'
            files = @([ordered]@{ path = 'bin/relayctl.exe'; sha256 = ('b' * 64); size = 1 })
        }
        Write-RelayAtomicText -Path $manifestPath -Text ($manifest | ConvertTo-Json -Depth 5 -Compress)
        $parsed = Assert-RelayManifestTarget -ManifestPath $manifestPath -HostArchitecture $architecture -ExpectedVersion '1.2.3'
        Assert-Equal $architecture $parsed.architecture "windows/$architecture manifest should be accepted"
    }

    $wrongPlatformPath = Join-Path $testRoot 'manifest-linux.json'
    Write-RelayAtomicText -Path $wrongPlatformPath -Text '{"schemaVersion":1,"version":"1.2.3","platform":"linux","architecture":"amd64"}'
    Assert-Throws { Assert-RelayManifestTarget -ManifestPath $wrongPlatformPath -HostArchitecture 'amd64' -ExpectedVersion '1.2.3' } 'Linux manifest must be rejected on Windows'
    Assert-Throws { Assert-RelayManifestTarget -ManifestPath (Join-Path $testRoot 'manifest-amd64.json') -HostArchitecture 'arm64' -ExpectedVersion '1.2.3' } 'cross-architecture manifest must be rejected'

    $environmentPath = Join-Path $testRoot 'config\relay.env'
    $fakeKey = 'relay_' + ('A' * 43)
    Write-RelayEnvironment -Path $environmentPath -AccessKey $fakeKey -ManagementUrl 'https://management.example.test/' -Version '1.2.3'
    $environmentBytes = [IO.File]::ReadAllBytes($environmentPath)
    Assert-True ($environmentBytes.Length -gt 3 -and -not ($environmentBytes[0] -eq 0xEF -and $environmentBytes[1] -eq 0xBB -and $environmentBytes[2] -eq 0xBF)) 'environment file must be UTF-8 without BOM'
    Update-RelayEnvironmentVersion -Path $environmentPath -Version '2.0.0'
    $environmentText = [IO.File]::ReadAllText($environmentPath)
    Assert-True ($environmentText.Contains("RELAY_ACCESS_KEY=$fakeKey")) 'version update must preserve Access Key'
    Assert-True ($environmentText.Contains('RELAY_VERSION=2.0.0')) 'version update must write new version'
    Assert-True (-not $environmentText.Contains('RELAY_VERSION=1.2.3')) 'version update must remove old version'

    $verifierPath = Join-Path $testRoot 'relayctl.exe'
    Write-RelayAtomicBytes -Path $verifierPath -Bytes ([byte[]](1, 2, 3, 4))
    $verifierHash = (Get-FileHash -LiteralPath $verifierPath -Algorithm SHA256).Hash
    Assert-Equal ([IO.Path]::GetFullPath($verifierPath)) (Assert-RelayTrustedVerifier -VerifierFile $verifierPath -VerifierSha256 $verifierHash -RequireHash) 'bootstrap verifier hash should be checked'
    Assert-Throws { Assert-RelayTrustedVerifier -VerifierFile $verifierPath -VerifierSha256 ('0' * 64) -RequireHash } 'wrong verifier hash must be rejected'
    Assert-Throws { Assert-RelayTrustedVerifier -VerifierFile $verifierPath -RequireHash } 'bootstrap verifier without hash must be rejected'

    Assert-True (Test-RelayArchiveEntryPath 'bin/relayctl.exe') 'normal archive entry should be accepted'
    Assert-True (-not (Test-RelayArchiveEntryPath '../escape.txt')) 'parent traversal archive entry must be rejected'
    Assert-True (-not (Test-RelayArchiveEntryPath 'bin\relayctl.exe')) 'backslash archive entry must be rejected'
    Assert-True (-not (Test-RelayArchiveEntryPath 'C:/escape.txt')) 'drive-qualified archive entry must be rejected'
}
finally {
    if (Test-Path -LiteralPath $testRoot) { Remove-Item -LiteralPath $testRoot -Recurse -Force }
}

$requiredScripts = @('Install.ps1', 'Upgrade.ps1', 'Start.ps1', 'Stop.ps1', 'Healthcheck.ps1', 'Uninstall.ps1')
foreach ($name in $requiredScripts) {
    $path = Join-Path $PSScriptRoot $name
    Assert-True (Test-Path -LiteralPath $path -PathType Leaf) "$name must exist"
    $tokens = $null
    $parseErrors = $null
    [void][Management.Automation.Language.Parser]::ParseFile($path, [ref]$tokens, [ref]$parseErrors)
    Assert-Equal 0 $parseErrors.Count "$name must parse without errors"
}
$modulePath = Join-Path $PSScriptRoot 'lib\RelayCommon.psm1'
$moduleTokens = $null
$moduleErrors = $null
[void][Management.Automation.Language.Parser]::ParseFile($modulePath, [ref]$moduleTokens, [ref]$moduleErrors)
Assert-Equal 0 $moduleErrors.Count 'RelayCommon.psm1 must parse without errors'

$moduleText = [IO.File]::ReadAllText($modulePath)
$installText = [IO.File]::ReadAllText((Join-Path $PSScriptRoot 'Install.ps1'))
$upgradeText = [IO.File]::ReadAllText((Join-Path $PSScriptRoot 'Upgrade.ps1'))
Assert-True ($moduleText.Contains("'release', 'verify-bundle'")) 'outer bundle must use trusted relayctl verify-bundle'
$sourceFunction = $moduleText.Substring($moduleText.IndexOf('function Resolve-RelayPackageSource', [StringComparison]::Ordinal))
Assert-True ($sourceFunction.IndexOf('Assert-RelayBundle -VerifierFile', [StringComparison]::Ordinal) -lt $sourceFunction.IndexOf('Expand-RelayArchive -Archive', [StringComparison]::Ordinal)) 'bundle verification must precede extraction'
Assert-True ($moduleText.Contains("'--expected-platform', 'windows'")) 'release manifest must be pinned to Windows'
Assert-True ($moduleText.Contains("'NT AUTHORITY\LocalService'")) 'service must use the native LocalService account'
Assert-True ($moduleText.Contains('RELAY_ENV_FILE=')) 'service must receive only the protected env-file pointer'
Assert-True ($moduleText.Contains("'config', `$script:RelayServiceName, 'binPath='")) 'SCM ImagePath must be the atomic release switch'
Assert-True ($installText.Contains('[switch]$AccessKeyStdin') -and $installText.Contains('[string]$AccessKeyFile')) 'Install must support non-command-line Access Key inputs'
Assert-True (-not ($installText -match '\[string\]\s*\$AccessKey(?:\s|,|\))')) 'Install must not expose a plaintext AccessKey parameter'
Assert-True ($installText.Contains('-VerifierSha256') -and $installText.Contains('-RequireHash')) 'Install must pin the bootstrap verifier hash'
Assert-True ($installText.Contains("`$PSBoundParameters.ContainsKey('InstallRoot')")) 'Install must distinguish an omitted root from an explicitly bound value'
Assert-True ($installText.Contains('-NonInteractive') -and $installText.Contains('Non-interactive installation requires -AccessKeyStdin or -AccessKeyFile.')) 'non-interactive install must never block waiting for an Access Key'
Assert-True ($installText.Contains("Read-Host 'Management URL [https://wenzwork.com]'")) 'interactive management URL prompt must remain available'
Assert-True ($moduleText.Contains("Read-Host 'Access Key' -AsSecureString")) 'interactive secure Access Key prompt must remain available'
Assert-True ($moduleText.Contains('[Console]::IsInputRedirected')) 'stdin Access Key mode must reject an interactive console that would echo or block'
Assert-True ($upgradeText.Contains('$previousBinary') -and $upgradeText.Contains('rollback also failed')) 'Upgrade must preserve and restore the previous release on failure'

Write-Host "Relay Windows scripts: PASS ($script:Assertions assertions)"
