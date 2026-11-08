# Run the API for the console and restart it whenever it exits, so the crash demos can show recovery.
# Fault injection is ON here (it is a development tool): the admin token is printed. Needs scripts/up.ps1.
# Override settings with environment variables first, for example: $env:STRATEGY = 'fastpath'; $env:ROOM_RATE = '50'
$ErrorActionPreference = 'Stop'
Set-Location (Join-Path $PSScriptRoot '..')
go build -o bin/api.exe ./cmd/api
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
if (-not $env:FAULTS_ENABLED) { $env:FAULTS_ENABLED = 'true' }
if (-not $env:ADMIN_TOKEN) { $env:ADMIN_TOKEN = 'dev-token' }
if (-not $env:PAYMENT_PROFILE) { $env:PAYMENT_PROFILE = 'fast' }
if (-not $env:HOLD_TTL) { $env:HOLD_TTL = '60s' }
if (-not $env:HTTP_ADDR) { $env:HTTP_ADDR = ':8080' }
Write-Host "console: http://localhost:8080   admin token: $env:ADMIN_TOKEN   (Ctrl-C stops)"
while ($true) {
    & bin\api.exe
    Write-Host "the API exited with status $LASTEXITCODE; restarting in 1 s"
    Start-Sleep -Seconds 1
}
