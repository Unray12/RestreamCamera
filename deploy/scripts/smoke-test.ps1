# End-to-end control-plane smoke test: login, create a source, fan out N
# virtual cameras, wait for MediaMTX to apply them, then run a DESCRIBE load
# test through the real auth callback.
#
#   .\scripts\smoke-test.ps1
#   .\scripts\smoke-test.ps1 -Cameras 200 -BaseUrl http://127.0.0.1:18080 `
#                            -MediaApi http://127.0.0.1:19997 -Keep
#   .\scripts\smoke-test.ps1 -WithPublisher   # also pushes a synthetic stream
param(
  [string]$BaseUrl = "http://127.0.0.1:8080",
  [string]$MediaApi = "http://127.0.0.1:9997",
  [string]$User = "admin",
  [string]$Password = "admin",
  [int]$Cameras = 20,
  [switch]$WithPublisher,
  [string]$PublishNetwork = "restreamcamera-dev_default",
  [string]$PublishUrl = "rtsp://mediamtx:8554",
  [switch]$Keep
)

$ErrorActionPreference = "Stop"
$api = "$BaseUrl/api/v1"
$stamp = Get-Date -Format "HHmmss"

function Fail($msg) {
  Write-Host "FAIL: $msg" -ForegroundColor Red
  exit 1
}

Write-Host "1/6 login as $User"
try {
  $login = Invoke-RestMethod -Method Post -Uri "$api/auth/login" -ContentType "application/json" `
    -Body (@{ username = $User; password = $Password } | ConvertTo-Json)
} catch {
  Fail "cannot login: $($_.Exception.Message)"
}
$headers = @{ Authorization = "Bearer $($login.token)" }

Write-Host "2/6 create a publisher source"
$source = Invoke-RestMethod -Method Post -Uri "$api/sources" -Headers $headers -ContentType "application/json" `
  -Body (@{ name = "smoke-$stamp"; kind = "publisher"; transport = "tcp" } | ConvertTo-Json)
Write-Host "    source path: $($source.path)"

$publisher = $null
if ($WithPublisher) {
  Write-Host "    starting a synthetic publisher (ffmpeg)"
  $publisher = "rc-smoke-ffmpeg-$stamp"
  docker run -d --rm --name $publisher --network $PublishNetwork linuxserver/ffmpeg `
    -re -f lavfi -i "testsrc2=size=640x360:rate=15" -f lavfi -i "sine=frequency=1000" `
    -c:v libx264 -preset ultrafast -tune zerolatency -g 15 -pix_fmt yuv420p `
    -c:a aac -b:a 64k -f rtsp -rtsp_transport tcp "$PublishUrl/$($source.path)" | Out-Null
  if ($LASTEXITCODE -ne 0) { Fail "cannot start the ffmpeg publisher container" }

  $deadline = (Get-Date).AddSeconds(30)
  do {
    Start-Sleep -Milliseconds 500
    $cur = (Invoke-RestMethod -Uri "$api/sources" -Headers $headers).items |
      Where-Object { $_.id -eq $source.id }
  } while ($cur.status -ne "online" -and (Get-Date) -lt $deadline)
  if ($cur.status -ne "online") { Fail "publisher did not come online ($($cur.lastError))" }
  Write-Host "    source is online"
  Start-Sleep -Seconds 2
}

Write-Host "3/6 create $Cameras virtual cameras"
$prefix = "smk$stamp"
$bulk = Invoke-RestMethod -Method Post -Uri "$api/cameras/bulk" -Headers $headers -ContentType "application/json" `
  -Body (@{
    sourceId = $source.id; count = $Cameras; prefix = $prefix; startIndex = 1
    group = "smoke"; maxReaders = 8; onDemand = $true
  } | ConvertTo-Json)
if ($bulk.created -ne $Cameras) { Fail "created $($bulk.created) cameras, expected $Cameras" }

Write-Host "4/6 wait for MediaMTX to apply the paths"
$want = $Cameras + 1
$deadline = (Get-Date).AddSeconds(30)
$paths = $null
do {
  Start-Sleep -Milliseconds 700
  try {
    $paths = Invoke-RestMethod -Uri "$MediaApi/v3/config/paths/list?itemsPerPage=1000"
  } catch {
    Fail "MediaMTX API unreachable at $MediaApi ($($_.Exception.Message))"
  }
} while ($paths.itemCount -lt $want -and (Get-Date) -lt $deadline)
if ($paths.itemCount -ne $want) { Fail "MediaMTX has $($paths.itemCount) paths, expected $want" }
Write-Host "    $($paths.itemCount) paths configured"

Write-Host "5/6 load test: $Cameras concurrent DESCRIBE requests"
$run = Invoke-RestMethod -Method Post -Uri "$api/loadtest" -Headers $headers -ContentType "application/json" `
  -Body (@{ count = $Cameras; concurrency = [math]::Min(32, $Cameras); timeoutMs = 8000 } | ConvertTo-Json)
$deadline = (Get-Date).AddSeconds(120)
do {
  Start-Sleep -Milliseconds 500
  $run = Invoke-RestMethod -Uri "$api/loadtest/$($run.id)" -Headers $headers
} while ($run.status -ne "done" -and (Get-Date) -lt $deadline)
if ($run.status -ne "done") { Fail "load test did not finish" }

Write-Host ("    ok={0} fail={1} p50={2}ms p95={3}ms" -f $run.ok, $run.fail, $run.p50Ms, $run.p95Ms)

Write-Host "6/6 overview"
$ov = Invoke-RestMethod -Uri "$api/overview" -Headers $headers
Write-Host ("    cameras={0} ready={1} idle={2} managedPaths={3}" -f `
  $ov.overview.camerasTotal, $ov.overview.camerasReady, $ov.overview.camerasIdle, $ov.stats.managedPaths)

if ($publisher) {
  docker rm -f $publisher 2>$null | Out-Null
  Write-Host "    stopped the synthetic publisher"
}

if (-not $Keep) {
  Invoke-RestMethod -Method Delete -Uri "$api/sources/$($source.id)" -Headers $headers | Out-Null
  Write-Host "    cleaned up source $($source.id)"
}

if ($run.fail -ne 0) { Fail "$($run.fail) DESCRIBE requests failed" }
Write-Host "PASS" -ForegroundColor Green
