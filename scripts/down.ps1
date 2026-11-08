# Stop local infrastructure and delete its data.
$ErrorActionPreference = 'Stop'
Set-Location (Join-Path $PSScriptRoot '..')
docker compose down -v
exit $LASTEXITCODE
