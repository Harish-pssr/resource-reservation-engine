# Run the API for the console and restart it whenever it exits, so the crash demos can show recovery.
# Fault injection is ON here (it is a development tool): the admin token is printed. Needs scripts/up.ps1.
# Override settings with environment variables first, for example: $env:STRATEGY = 'locking'; $env:ROOM_RATE = '0'
$ErrorActionPreference = 'Stop'
Set-Location (Join-Path $PSScriptRoot '..')
go build -o bin/api.exe ./cmd/api
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
if (-not $env:FAULTS_ENABLED) { $env:FAULTS_ENABLED = 'true' }
if (-not $env:ADMIN_TOKEN) { $env:ADMIN_TOKEN = 'dev-token' }
if (-not $env:PAYMENT_PROFILE) { $env:PAYMENT_PROFILE = 'fast' }
# By default show the whole system: the Redis fast path and the waiting room (admits 30 buyers a second).
if (-not $env:STRATEGY) { $env:STRATEGY = 'fastpath' }
if (-not $env:ROOM_RATE) { $env:ROOM_RATE = '30' }
if (-not $env:HOLD_TTL) { $env:HOLD_TTL = '60s' }
if (-not $env:HTTP_ADDR) { $env:HTTP_ADDR = ':8080' }
Write-Host "console: http://localhost:8080   admin token: $env:ADMIN_TOKEN   (Ctrl-C stops)"
while ($true) {
    # Through cmd with 2>&1: in Windows PowerShell 5.1 a native command's stderr lines become error records when
    # the output is redirected, and with ErrorActionPreference Stop the first log line would end this loop.
    cmd /c 'bin\api.exe 2>&1'
    Write-Host "the API exited with status $LASTEXITCODE; restarting in 1 s"
    Start-Sleep -Seconds 1
}
