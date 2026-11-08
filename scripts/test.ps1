# Run all tests. Integration tests need the infrastructure from scripts/up.ps1.
$ErrorActionPreference = 'Stop'
Set-Location (Join-Path $PSScriptRoot '..')
go vet ./...
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
go test ./... @args
exit $LASTEXITCODE
