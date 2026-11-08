# Start local Postgres and Redis and wait until both are healthy.
$ErrorActionPreference = 'Stop'
Set-Location (Join-Path $PSScriptRoot '..')
docker compose up -d --wait
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
Write-Host 'postgres: localhost:55432   redis: localhost:56379'
