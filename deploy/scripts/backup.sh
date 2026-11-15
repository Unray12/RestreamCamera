#!/bin/sh
# Backup the SQLite database of a running RestreamCamera instance.
#
#   ./scripts/backup.sh                 # uses docker-compose.yml
#   COMPOSE_FILE=docker-compose.dev.yml ./scripts/backup.sh
set -eu

cd "$(dirname "$0")/.."

COMPOSE_FILE="${COMPOSE_FILE:-docker-compose.yml}"
COMPOSE="docker compose -f $COMPOSE_FILE"
STAMP="$(date +%Y%m%d-%H%M%S)"
NAME="restream-$STAMP.db"

mkdir -p backups
$COMPOSE exec -T api sh -c 'mkdir -p /data/backups'
$COMPOSE exec -T api restreamd backup "/data/backups/$NAME"
$COMPOSE cp "api:/data/backups/$NAME" "backups/$NAME"
$COMPOSE exec -T api rm -f "/data/backups/$NAME"

echo "backup written to backups/$NAME"
