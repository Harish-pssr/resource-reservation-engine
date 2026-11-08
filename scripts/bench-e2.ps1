# e2: see "bench e2 -h". Extra arguments go to `bench e2`.
# Windows runs are for development only; final numbers come from the Linux VM.
$ErrorActionPreference = 'Stop'
Set-Location (Join-Path $PSScriptRoot '..')
& "$PSScriptRoot\build.ps1"
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
& bin\bench.exe e2 @args
exit $LASTEXITCODE
