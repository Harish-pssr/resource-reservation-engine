# Closes everything this project starts: the restart loops, the API servers, any benchmark or load
# processes, and the Postgres and Redis containers.
#
#   scripts/down.ps1           stops it all and KEEPS the database and Redis data (scripts/up.ps1 brings it back)
#   scripts/down.ps1 -Clean    also removes the containers and DELETES their data
param([switch]$Clean)
$ErrorActionPreference = 'Continue'
Set-Location (Join-Path $PSScriptRoot '..')

# 1. The restart loops first, otherwise they would start the servers again as soon as we stop them.
#    Only a shell running up.ps1 in its internal server-loop mode is matched, and never this script itself.
$loops = Get-CimInstance Win32_Process | Where-Object {
    $_.ProcessId -ne $PID -and $_.Name -match '^(bash|powershell|pwsh)(\.exe)?$' -and $_.CommandLine -match 'up\.ps1.*-RunApiLoop'
}
foreach ($p in $loops) { Stop-Process -Id $p.ProcessId -Force -ErrorAction SilentlyContinue }
Write-Host "restart loops stopped: $(@($loops).Count)"

# 2. The servers and tools themselves.
$names = 'api', 'api-ui', 'bench', 'loadgen'
$killed = 0
foreach ($n in $names) {
    foreach ($p in @(Get-Process -Name $n -ErrorAction SilentlyContinue)) { Stop-Process -Id $p.Id -Force -ErrorAction SilentlyContinue; $killed++ }
}
Write-Host "processes stopped: $killed ($($names -join ', '))"

# 3. The stores.
if (Get-Command docker -ErrorAction SilentlyContinue) {
    if ($Clean) { docker compose down -v; Write-Host 'containers removed and data deleted' }
    else { docker compose stop; Write-Host 'containers stopped; data kept (scripts/up.ps1 starts them again)' }
} else {
    Write-Host 'docker is not installed: nothing to stop there'
}

# 4. Check, so the script says plainly whether anything is left.
$left = @(Get-Process -Name $names -ErrorAction SilentlyContinue)
if ($left.Count -gt 0) { Write-Host "WARNING: still running: $($left.Name -join ', ')"; exit 1 }
Write-Host 'everything is closed'
