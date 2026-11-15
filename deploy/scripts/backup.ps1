# Backup the SQLite database of a running RestreamCamera instance.
#
#   .\scripts\backup.ps1
#   .\scripts\backup.ps1 -ComposeFile docker-compose.dev.yml
param(
  [string]$ComposeFile = "docker-compose.yml"
)

$ErrorActionPreference = "Stop"
Set-Location (Join-Path $PSScriptRoot "..")

$stamp = Get-Date -Format "yyyyMMdd-HHmmss"
$name = "restream-$stamp.db"

New-Item -ItemType Directory -Force -Path "backups" | Out-Null
docker compose -f $ComposeFile exec -T api sh -c "mkdir -p /data/backups"
docker compose -f $ComposeFile exec -T api restreamd backup "/data/backups/$name"
docker compose -f $ComposeFile cp "api:/data/backups/$name" "backups/$name"
docker compose -f $ComposeFile exec -T api rm -f "/data/backups/$name"

Write-Host "backup written to backups/$name"
