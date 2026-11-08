# E1, the oversell test: every strategy, repeated runs. Extra arguments go to `bench e1`.
# Windows runs are for development only; final numbers come from the Linux VM.
$ErrorActionPreference = 'Stop'
Set-Location (Join-Path $PSScriptRoot '..')
& "$PSScriptRoot\build.ps1"
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
& bin\bench.exe e1 @args
exit $LASTEXITCODE
