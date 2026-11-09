# Starts everything: Postgres and Redis (Docker), then the API with the console, and a first event.
# Afterwards open http://localhost:8080 . scripts/down.ps1 closes it all again.
#
# The server runs in the background under scripts/dev-api.ps1, which restarts it if it exits (so the crash
# demos can show recovery). Its log is logs/api.log (errors: logs/api.err.log). Fault injection is ON in this development setup:
# the admin token for the Failures and Checker pages is printed below.
#
# To change how the server runs, set variables first, for example:
#   $env:STRATEGY = 'locking'; $env:ROOM_RATE = '0'; scripts/up.ps1
$ErrorActionPreference = 'Stop'
$root = Join-Path $PSScriptRoot '..'
Set-Location $root

# 1. The stores.
if (-not (Get-Command docker -ErrorAction SilentlyContinue)) { Write-Host 'Docker is not installed.'; exit 1 }
docker info *> $null
if ($LASTEXITCODE -ne 0) { Write-Host 'Docker is not running: start Docker Desktop, then run this again.'; exit 1 }
docker compose up -d --wait
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
Write-Host 'postgres: localhost:55432   redis: localhost:56379'

# 2. The API and the console.
$addr = if ($env:HTTP_ADDR) { $env:HTTP_ADDR } else { ':8080' }
$port = ($addr -split ':')[-1]
$base = "http://127.0.0.1:$port"
function Get-Health { try { Invoke-RestMethod "$base/healthz" -TimeoutSec 2 } catch { $null } }

if (Get-Health) {
    Write-Host "the API is already running at $base"
} else {
    New-Item -ItemType Directory -Force logs | Out-Null
    $script = Join-Path $PSScriptRoot 'dev-api.ps1'
    $out = Join-Path (Resolve-Path logs) 'api.log'
    $err = Join-Path (Resolve-Path logs) 'api.err.log'
    $launcher = Start-Process powershell -WindowStyle Hidden -PassThru -RedirectStandardOutput $out -RedirectStandardError $err `
        -ArgumentList '-NoProfile', '-File', $script
    Write-Host 'starting the API (the first start builds it, which can take a minute)...'
    $up = $false
    for ($i = 0; $i -lt 120 -and -not $up; $i++) {
        Start-Sleep -Seconds 1
        $up = [bool](Get-Health)
        if (-not $up -and $launcher.HasExited) { break }
    }
    if (-not $up) {
        Write-Host 'the API did not come up. Last lines of its log:'
        foreach ($f in $out, $err) { if (Test-Path $f) { Get-Content $f -Tail 8 } }
        exit 1
    }
}

# 3. A first event, so the seat map is not empty.
$events = Invoke-RestMethod "$base/events"
if (@($events.events).Count -eq 0) {
    Invoke-RestMethod "$base/events" -Method Post -ContentType 'application/json' -Body '{"name":"Opening night","items":60}' | Out-Null
    Write-Host 'created a first event: Opening night, 60 seats'
}

$token = if ($env:ADMIN_TOKEN) { $env:ADMIN_TOKEN } else { 'dev-token' }
Write-Host ''
Write-Host "everything is up:  http://localhost:$port"
Write-Host "admin token (Failures and Checker pages): $token"
Write-Host 'close it all with: scripts/down.ps1'
