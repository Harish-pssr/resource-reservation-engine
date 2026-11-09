# Starts everything: Postgres and Redis (Docker), then the API with the console, and a first event.
# Afterwards open http://localhost:8080 . scripts/down.ps1 closes it all again.
#
# The server runs in the background and is restarted if it exits (so the crash demos can show recovery).
# Its log is logs/api.log (errors: logs/api.err.log). Fault injection is ON in this development setup:
# the admin token for the Failures and Checker pages is printed at the end.
#
# To change how the server runs, set variables first, for example:
#   $env:STRATEGY = 'locking'; $env:ROOM_RATE = '0'; scripts/up.ps1
param(
    # Internal: how up runs the restarting server in the background. Do not pass it yourself.
    [switch]$RunApiLoop
)
$ErrorActionPreference = 'Stop'
$root = Join-Path $PSScriptRoot '..'
Set-Location $root

# ---- internal mode: build the API and run it, restarting it whenever it exits ----
if ($RunApiLoop) {
    go build -o bin/api.exe ./cmd/api
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
    if (-not $env:FAULTS_ENABLED) { $env:FAULTS_ENABLED = 'true' }
    if (-not $env:ADMIN_TOKEN) { $env:ADMIN_TOKEN = 'dev-token' }
    if (-not $env:PAYMENT_PROFILE) { $env:PAYMENT_PROFILE = 'fast' }
    if (-not $env:HOLD_TTL) { $env:HOLD_TTL = '60s' }
    if (-not $env:HTTP_ADDR) { $env:HTTP_ADDR = ':8080' }
    # By default show the whole system: the Redis fast path and the waiting room (admits 30 buyers a second).
    if (-not $env:STRATEGY) { $env:STRATEGY = 'fastpath' }
    if (-not $env:ROOM_RATE) { $env:ROOM_RATE = '30' }
    while ($true) {
        # Through cmd with 2>&1: in Windows PowerShell 5.1 a native command's stderr lines become error records when
        # the output is redirected, and with ErrorActionPreference Stop the first log line would end this loop.
        cmd /c 'bin\api.exe 2>&1'
        Write-Host "the API exited with status $LASTEXITCODE; restarting in 1 s"
        Start-Sleep -Seconds 1
    }
}

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
    $out = Join-Path (Resolve-Path logs) 'api.log'
    $err = Join-Path (Resolve-Path logs) 'api.err.log'
    # This same script, started again in the internal mode, hidden, with its output in the log files.
    $launcher = Start-Process powershell -WindowStyle Hidden -PassThru -RedirectStandardOutput $out -RedirectStandardError $err `
        -ArgumentList '-NoProfile', '-File', "`"$PSCommandPath`"", '-RunApiLoop'
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
