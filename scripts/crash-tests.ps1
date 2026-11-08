# The real-kill scenarios of docs/crash-analysis.md: each crash row, CRASH_REPS times (default 5), against
# the real API binary. Needs scripts/up.ps1 first. Raw evidence goes to $env:CRASH_RESULTS.
$ErrorActionPreference = 'Stop'
Set-Location (Join-Path $PSScriptRoot '..')
if (-not $env:CRASH_REPS) { $env:CRASH_REPS = '5' }
# An absolute path: the test process runs in the package directory.
if (-not $env:CRASH_RESULTS) { $env:CRASH_RESULTS = Join-Path (Get-Location) 'benchmark-scratch\crash-results.jsonl' }
New-Item -ItemType Directory -Force (Split-Path $env:CRASH_RESULTS) | Out-Null
Set-Content -Path $env:CRASH_RESULTS -Value $null
go test ./internal/crashtest -count=1 -v -timeout 30m @args
$runs = (Get-Content $env:CRASH_RESULTS | Measure-Object -Line).Lines
$kills = (Select-String -Path $env:CRASH_RESULTS -Pattern '"killed":true').Count
Write-Host "raw evidence: $env:CRASH_RESULTS ($runs runs, $kills real kills)"
exit $LASTEXITCODE
