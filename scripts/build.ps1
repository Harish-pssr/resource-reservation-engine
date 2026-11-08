# Build the binaries into bin/.
$ErrorActionPreference = 'Stop'
Set-Location (Join-Path $PSScriptRoot '..')
New-Item -ItemType Directory -Force bin | Out-Null
foreach ($c in 'api', 'loadgen', 'bench') {
    go build -o "bin/$c.exe" "./cmd/$c"
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
}
Write-Host 'built bin/api.exe bin/loadgen.exe bin/bench.exe'
