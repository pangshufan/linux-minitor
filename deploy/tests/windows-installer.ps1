# Integration test for a disposable Windows CI runner; registers a real service.
#Requires -RunAsAdministrator
$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
$package = Join-Path $env:RUNNER_TEMP ('monitor-installer-test-' + [guid]::NewGuid().ToString('N'))
$configPath = Join-Path $env:ProgramData 'Monitor\agent.json'
if (Get-Service MonitorAgent -ErrorAction SilentlyContinue) { throw 'Test requires a clean runner without MonitorAgent.' }
New-Item -ItemType Directory -Path (Join-Path $package 'bin') -Force | Out-Null
Copy-Item (Join-Path $root 'deploy\windows\install-agent.ps1') $package
& go build -mod=readonly -o (Join-Path $package 'bin\monitor-agent.exe') ./agent
if ($LASTEXITCODE -ne 0) { throw 'Agent build failed.' }
$global:InstallerAnswers = [Collections.Generic.Queue[string]]::new()
function global:Read-Host {
    param([string]$Prompt, [switch]$AsSecureString)
    if ($global:InstallerAnswers.Count -eq 0) { throw "Unexpected prompt: $Prompt" }
    return $global:InstallerAnswers.Dequeue()
}
try {
    foreach ($answer in @('', '127.0.0.1:18443', 'windows-installer-test', 'n', 'n', 'y', 'y')) {
        $global:InstallerAnswers.Enqueue($answer)
    }
    & (Join-Path $package 'install-agent.ps1')
    if ($global:InstallerAnswers.Count) { throw 'Installer did not consume all answers.' }
    $config = Get-Content -LiteralPath $configPath -Raw | ConvertFrom-Json
    if ($config.'host-id' -ne 'windows-installer-test' -or $config.'tls-enabled') { throw 'Incorrect generated configuration.' }
    $registry = 'HKLM:\SYSTEM\CurrentControlSet\Services\MonitorAgent'
    $entries = @((Get-ItemProperty $registry -Name Environment).Environment)
    if ('GODEBUG=tlsmlkem=0' -notin $entries) { throw 'Service-specific TLS compatibility was not installed.' }
    $service = Get-CimInstance Win32_Service -Filter "Name='MonitorAgent'"
    $expected = '"{0}" -config "{1}"' -f (Join-Path $env:ProgramFiles 'Monitor\monitor-agent.exe'), $configPath
    if ($service.PathName -ne $expected -or $service.State -ne 'Running') { throw 'Incorrect service command or state.' }

    # Updating must preserve unrelated service environment settings and make a backup.
    Set-ItemProperty $registry -Name Environment -Value @('MONITOR_INSTALLER_TEST=kept', 'GODEBUG=tlsmlkem=0,asynctimerchan=1')
    foreach ($answer in @('', '', 'n', 'n', 'n', 'y')) { $global:InstallerAnswers.Enqueue($answer) }
    & (Join-Path $package 'install-agent.ps1') -ConfigPath $configPath
    $entries = @((Get-ItemProperty $registry -Name Environment).Environment)
    if ('MONITOR_INSTALLER_TEST=kept' -notin $entries -or 'GODEBUG=asynctimerchan=1' -notin $entries) {
        throw 'Update lost unrelated service environment entries.'
    }
    if (-not (Get-ChildItem (Split-Path $configPath) -Directory -Filter 'backup-*')) { throw 'Missing backup.' }
    Write-Host 'Windows installer service creation/update and TLS environment tests passed.'
} finally {
    Remove-Item Function:\Read-Host -ErrorAction SilentlyContinue
    if (Get-Service MonitorAgent -ErrorAction SilentlyContinue) { Stop-Service MonitorAgent }
    & sc.exe delete MonitorAgent | Out-Null
    Remove-Item -LiteralPath $package -Recurse -Force
}
