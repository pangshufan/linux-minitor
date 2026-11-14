#Requires -RunAsAdministrator
param(
    [string]$ConfigPath = "$env:ProgramData\Monitor\agent.json"
)

$ErrorActionPreference = 'Stop'
$source = Join-Path $PSScriptRoot 'bin\monitor-agent.exe'
if (-not (Test-Path -LiteralPath $source -PathType Leaf)) {
    throw "Agent executable missing: $source"
}
if (Get-Service -Name MonitorAgent -ErrorAction SilentlyContinue) {
    throw 'MonitorAgent already exists. Stop the service, replace its executable, then start it again.'
}
$config = (Resolve-Path -LiteralPath $ConfigPath).ProviderPath
$destination = Join-Path $env:ProgramFiles 'Monitor'
$exe = Join-Path $destination 'monitor-agent.exe'
New-Item -ItemType Directory -Path $destination -Force | Out-Null
Copy-Item -LiteralPath $source -Destination $exe -Force
$command = '"{0}" -config "{1}"' -f $exe, $config
New-Service -Name MonitorAgent -DisplayName 'Monitor Agent' -BinaryPathName $command -StartupType Automatic | Out-Null
Start-Service -Name MonitorAgent
Get-Service -Name MonitorAgent
