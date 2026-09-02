# E2E Redis 8 prerequisite (TestBillingE2E only; production code never depends on this script).
# Idempotent: running container -> reuse; stopped container -> docker start; absent -> create.
# Any other port/name conflict exits with a clear prerequisite-environment error (never hijacks).
# Usage:  pwsh -File scripts/e2e-redis.ps1   then   $env:C3API_REDIS_ADDR = "localhost:16379"
param(
    [string]$Name = 'c3api-test-redis',
    [int]$Port = 16379
)
$ErrorActionPreference = 'Stop'

function Invoke-Fatal([string]$msg) {
    Write-Host "E2E-REDIS-PREREQ-ERROR: $msg"
    exit 1
}

docker container inspect $Name > $null 2>&1
if ($LASTEXITCODE -eq 0) {
    $running = (docker inspect -f '{{.State.Running}}' $Name).Trim()
    if ($running -eq 'true') {
        Write-Host "container $Name already running -> reuse"
    } else {
        Write-Host "container $Name exists but stopped -> docker start"
        docker start $Name > $null
        if ($LASTEXITCODE -ne 0) { Invoke-Fatal "docker start $Name failed (exit $LASTEXITCODE)" }
    }
} else {
    # absent -> create; but a port already claimed by another container/process is a
    # prerequisite-environment error (fail clearly, do not hijack or silently re-map).
    $owner = (docker ps --filter "publish=$Port" --format '{{.Names}}') -join ','
    if ($LASTEXITCODE -ne 0) { Invoke-Fatal "docker ps failed (is the Docker daemon running?)" }
    if ($owner) {
        Invoke-Fatal ("port $Port is published by other container(s) '$owner'; if it is a healthy Redis 8 you may reuse it directly: docker exec $owner redis-cli ping  # expect PONG, then set C3API_REDIS_ADDR=localhost:$Port")
    }
    if (Get-NetTCPConnection -LocalPort $Port -State Listen -ErrorAction SilentlyContinue) {
        Invoke-Fatal "port $Port is occupied by a non-Docker listener; free it before creating $Name"
    }
    Write-Host "container $Name absent -> docker run -d --name $Name -p ${Port}:6379 redis:8-alpine"
    docker run -d --name $Name -p "${Port}:6379" redis:8-alpine > $null
    if ($LASTEXITCODE -ne 0) { Invoke-Fatal "docker run $Name failed (exit $LASTEXITCODE)" }
}

# strict PONG assertion (bounded wait for container readiness)
$pong = ''
for ($i = 0; $i -lt 20; $i++) {
    $pong = ((docker exec $Name redis-cli ping) 2>$null | Out-String).Trim()
    if ($LASTEXITCODE -eq 0 -and $pong -eq 'PONG') { break }
    Start-Sleep -Milliseconds 500
}
if ($pong -ne 'PONG') { Invoke-Fatal "docker exec $Name redis-cli ping returned '$pong' (expected PONG)" }

Write-Host "Redis 8 ready: $Name @ localhost:$Port (PONG) -> `$env:C3API_REDIS_ADDR=`"localhost:$Port`""
