# E4, failures under load: real API kills, Redis and Postgres restarts, an expiry race. Needs scripts/up.ps1.
# Extra arguments go to `bench e4`. Windows runs are for development only; final numbers come from the Linux VM.
$ErrorActionPreference = 'Stop'
Set-Location (Join-Path $PSScriptRoot '..')
& "$PSScriptRoot\build.ps1"
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
& bin\bench.exe e4 @args
exit $LASTEXITCODE
