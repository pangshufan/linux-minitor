#Requires -RunAsAdministrator
param(
    [string]$ConfigPath = ""
)

$ErrorActionPreference = 'Stop'
function Ask([string]$Label, [string]$Default = '') {
    $suffix = if ($Default) { " [$Default]" } else { '' }
    $value = Read-Host "$Label$suffix"
    if ([string]::IsNullOrWhiteSpace($value)) { return $Default }
    return $value.Trim()
}
function Ask-Yes([string]$Label, [bool]$Default = $true) {
    while ($true) {
        $value = Ask "$Label (y/n)" $(if ($Default) { 'y' } else { 'n' })
        if ($value -match '^(y|yes|n|no)$') { return $value -match '^(y|yes)$' }
    }
}
function Set-Config($Config, [string]$Name, $Value) {
    $Config | Add-Member -MemberType NoteProperty -Name $Name -Value $Value -Force
}
function Protect-Directory([string]$Path) {
    New-Item -ItemType Directory -Path $Path -Force | Out-Null
    & icacls.exe $Path /inheritance:r /grant:r '*S-1-5-18:(OI)(CI)F' '*S-1-5-32-544:(OI)(CI)F' | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "Cannot protect directory: $Path" }
}

$temp = Join-Path ([IO.Path]::GetTempPath()) ('monitor-install-' + [guid]::NewGuid().ToString('N'))
try {
    Write-Host 'Monitor Agent interactive installer (Windows x64)'
    $source = Join-Path $PSScriptRoot 'bin\monitor-agent.exe'
    if (-not (Test-Path -LiteralPath $source -PathType Leaf)) { throw "Missing Agent executable: $source" }
    Protect-Directory $temp
    if (-not $ConfigPath) {
        $ConfigPath = Ask 'Certificate ZIP, extracted certificate directory, or existing agent.json (Enter for manual/plaintext setup)'
    }
    $config = [pscustomobject]@{}
    $origin = ''
    if ($ConfigPath) {
        $candidate = (Resolve-Path -LiteralPath $ConfigPath).ProviderPath
        if ([IO.Path]::GetExtension($candidate) -eq '.zip') {
            Add-Type -AssemblyName System.IO.Compression.FileSystem
            $archive = [IO.Compression.ZipFile]::OpenRead($candidate)
            try {
                foreach ($name in @('agent.json', 'agent.crt', 'agent.key', 'ca.crt')) {
                    $entry = $archive.GetEntry($name)
                    if ($null -eq $entry -or $entry.Length -gt 1MB) { throw "Missing or oversized certificate package file: $name" }
                    [IO.Compression.ZipFileExtensions]::ExtractToFile($entry, (Join-Path $temp $name), $true)
                }
            } finally { $archive.Dispose() }
            $origin = $temp
            $config = Get-Content -LiteralPath (Join-Path $temp 'agent.json') -Raw | ConvertFrom-Json
        } else {
            if (Test-Path -LiteralPath $candidate -PathType Container) { $candidate = Join-Path $candidate 'agent.json' }
            $origin = Split-Path -Parent $candidate
            $config = Get-Content -LiteralPath $candidate -Raw | ConvertFrom-Json
        }
    }
    $server = Ask 'Server gRPC address (host:port)' $config.server
    $validServer = $server -match '^(\[[^\]\s]+\]|[^:\s/]+):([0-9]+)$'
    if (-not $validServer -or [double]$Matches[2] -lt 1 -or [double]$Matches[2] -gt 65535) {
        throw 'Invalid Server address; use host:port or [IPv6]:port.'
    }
    $defaultID = if ($config.'host-id') { $config.'host-id' } else { $env:COMPUTERNAME }
    $hostID = Ask 'Unique host ID (must match certificate identity)' $defaultID
    if ($hostID -notmatch '^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$') { throw 'Invalid host ID.' }
    $tlsDefault = if ($null -ne $config.'tls-enabled') { [bool]$config.'tls-enabled' } else { $true }
    $tls = Ask-Yes 'Enable mutual TLS' $tlsDefault
    $configDir = Join-Path $env:ProgramData 'Monitor'
    Set-Config $config 'server' $server
    Set-Config $config 'host-id' $hostID
    Set-Config $config 'tls-enabled' $tls
    $certs = @{}
    if ($tls) {
        foreach ($item in @(@('agent.crt', 'tls-cert'), @('agent.key', 'tls-key'), @('ca.crt', 'tls-ca'))) {
            $name, $key = $item
            $bundled = if ($origin) { Join-Path $origin $name } else { '' }
            $default = if ($bundled -and (Test-Path -LiteralPath $bundled -PathType Leaf)) { $bundled } else { $config.$key }
            $path = (Resolve-Path -LiteralPath (Ask "Path to $name" $default)).ProviderPath
            $certs[$name] = [IO.File]::ReadAllBytes($path)
            Set-Config $config $key (Join-Path $configDir $name)
        }
    } else {
        foreach ($key in @('tls-cert', 'tls-key', 'tls-ca')) { $config.PSObject.Properties.Remove($key) }
    }
    if (Ask-Yes 'Set or replace the shared authentication token' $false) {
        $secret = Read-Host 'Agent token (input hidden)' -AsSecureString
        $pointer = [Runtime.InteropServices.Marshal]::SecureStringToBSTR($secret)
        try { Set-Config $config 'auth-token' ([Runtime.InteropServices.Marshal]::PtrToStringBSTR($pointer)) }
        finally { [Runtime.InteropServices.Marshal]::ZeroFreeBSTR($pointer) }
    }
    $registryPath = 'HKLM:\SYSTEM\CurrentControlSet\Services\MonitorAgent'
    $oldEnvironment = @()
    if (Test-Path $registryPath) {
        $oldEnvironment = @((Get-ItemProperty -LiteralPath $registryPath -Name Environment -ErrorAction SilentlyContinue).Environment)
    }
    $compatDefault = @($oldEnvironment | Where-Object { $_ -match '^GODEBUG=.*tlsmlkem=0' }).Count -gt 0
    $compat = Ask-Yes 'Enable TLS compatibility (tlsmlkem=0; choose y only if it fixed your handshake issue)' $compatDefault
    Write-Host 'Existing process-services settings will be preserved.'
    Write-Host "Target: $server; Host: $hostID; TLS: $tls"
    Write-Host "Install to $env:ProgramFiles\Monitor; configuration to $configDir"
    if (-not (Ask-Yes 'Install/update and start MonitorAgent')) { Write-Host 'Cancelled; installation unchanged.'; return }
    Protect-Directory $configDir
    $backup = Join-Path $configDir ('backup-' + (Get-Date -Format yyyyMMdd-HHmmss) + '-' + $PID)
    Protect-Directory $backup
    foreach ($name in @('agent.json', 'agent.crt', 'agent.key', 'ca.crt')) {
        $old = Join-Path $configDir $name
        if (Test-Path -LiteralPath $old) { Copy-Item -LiteralPath $old -Destination $backup }
    }
    $destination = Join-Path $env:ProgramFiles 'Monitor'
    $exe = Join-Path $destination 'monitor-agent.exe'
    $service = Get-Service -Name MonitorAgent -ErrorAction SilentlyContinue
    if (Test-Path -LiteralPath $exe) { Copy-Item -LiteralPath $exe -Destination $backup }
    if ($service) {
        & reg.exe export 'HKLM\SYSTEM\CurrentControlSet\Services\MonitorAgent' (Join-Path $backup 'MonitorAgent.reg') /y | Out-Null
        if ($LASTEXITCODE -ne 0) { throw 'Cannot back up the existing service configuration.' }
        Stop-Service -Name MonitorAgent
        $service.WaitForStatus('Stopped', [TimeSpan]::FromSeconds(30))
    }
    New-Item -ItemType Directory -Path $destination -Force | Out-Null
    if ([IO.Path]::GetFullPath($source) -ne [IO.Path]::GetFullPath($exe)) { Copy-Item -LiteralPath $source -Destination $exe -Force }
    foreach ($name in $certs.Keys) { [IO.File]::WriteAllBytes((Join-Path $configDir $name), $certs[$name]) }
    $configFile = Join-Path $configDir 'agent.json'
    [IO.File]::WriteAllText($configFile, ($config | ConvertTo-Json -Depth 100), (New-Object Text.UTF8Encoding($false)))
    foreach ($file in @($configFile) + @($certs.Keys | ForEach-Object { Join-Path $configDir $_ })) {
        # Reset existing file ACLs too, so an older explicit Users permission cannot survive.
        & icacls.exe $file /reset | Out-Null
        if ($LASTEXITCODE -ne 0) { throw "Cannot protect configuration file: $file" }
    }
    $command = '"{0}" -config "{1}"' -f $exe, $configFile
    if ($service) {
        $instance = Get-CimInstance Win32_Service -Filter "Name='MonitorAgent'"
        $changed = Invoke-CimMethod -InputObject $instance -MethodName Change -Arguments @{ PathName = $command; StartMode = 'Automatic' }
        if ($changed.ReturnValue -ne 0) { throw "Cannot update the existing service command (code $($changed.ReturnValue))." }
    } else {
        New-Service -Name MonitorAgent -DisplayName 'Monitor Agent' -BinaryPathName $command -StartupType Automatic | Out-Null
    }
    # Preserve other service environment entries and other GODEBUG settings.
    $entries = @($oldEnvironment | Where-Object { $_ -and $_ -notmatch '^GODEBUG=' })
    $debug = @($oldEnvironment | Where-Object { $_ -match '^GODEBUG=' } | Select-Object -Last 1)
    $settings = @()
    if ($debug.Count) { $settings = @($debug[0].Substring(8).Split(',') | Where-Object { $_ -and $_ -notmatch '^tlsmlkem=' }) }
    if ($compat) { $settings += 'tlsmlkem=0' }
    if ($settings.Count) { $entries += 'GODEBUG=' + ($settings -join ',') }
    if ($entries.Count) {
        New-ItemProperty -LiteralPath $registryPath -Name Environment -PropertyType MultiString -Value ([string[]]$entries) -Force | Out-Null
    } else { Remove-ItemProperty -LiteralPath $registryPath -Name Environment -ErrorAction SilentlyContinue }
    Start-Service -Name MonitorAgent
    Start-Sleep -Seconds 2
    if ((Get-Service MonitorAgent).Status -ne 'Running') { throw 'MonitorAgent did not remain running.' }
    Write-Host "Installed. Backup: $backup"
    Write-Host 'Service running does not prove Server registration; verify the host is online in Web.'
    Get-Service MonitorAgent
} catch {
    Write-Error $_
    exit 1
} finally {
    if (Test-Path -LiteralPath $temp) { Remove-Item -LiteralPath $temp -Recurse -Force }
}
