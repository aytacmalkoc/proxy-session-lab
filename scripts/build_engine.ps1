$ErrorActionPreference = 'Stop'
$ProjectRoot = Split-Path $PSScriptRoot -Parent
$EngineGo = if ($env:PROXY_SESSION_LAB_GO) { $env:PROXY_SESSION_LAB_GO } else { 'go' }
if (-not (Get-Command $EngineGo -ErrorAction SilentlyContinue)) {
    throw 'Go 1.27+ is required. Set PROXY_SESSION_LAB_GO to an installed Go executable.'
}
Push-Location (Join-Path $ProjectRoot 'engine')
try {
    New-Item -ItemType Directory -Force 'bin' | Out-Null
    & $EngineGo build -trimpath -o 'bin/proxy-lab-engine.exe' './cmd/proxy-lab-engine'
    if ($LASTEXITCODE -ne 0) { throw 'Go engine build failed' }
    & './bin/proxy-lab-engine.exe' version
    if ($LASTEXITCODE -ne 0) { throw 'Go engine version check failed' }
} finally {
    Pop-Location
}
