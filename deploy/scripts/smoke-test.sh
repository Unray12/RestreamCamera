#!/bin/sh
# End-to-end control-plane smoke test (requires curl and jq).
#
#   ./scripts/smoke-test.sh
#   BASE_URL=http://127.0.0.1:18080 MEDIA_API=http://127.0.0.1:19997 \
#   CAMERAS=200 KEEP=1 ./scripts/smoke-test.sh
set -eu

BASE_URL="${BASE_URL:-http://127.0.0.1:8080}"
MEDIA_API="${MEDIA_API:-http://127.0.0.1:9997}"
USER_NAME="${USER_NAME:-admin}"
PASSWORD="${PASSWORD:-admin}"
CAMERAS="${CAMERAS:-20}"
KEEP="${KEEP:-0}"
WITH_PUBLISHER="${WITH_PUBLISHER:-0}"
PUBLISH_NETWORK="${PUBLISH_NETWORK:-restreamcamera-dev_default}"
PUBLISH_URL="${PUBLISH_URL:-rtsp://mediamtx:8554}"
STAMP="$(date +%H%M%S)"
API="$BASE_URL/api/v1"

command -v jq >/dev/null || { echo "jq is required"; exit 1; }

echo "1/6 login"
TOKEN="$(curl -fsS -X POST "$API/auth/login" -H 'Content-Type: application/json' \
  -d "{\"username\":\"$USER_NAME\",\"password\":\"$PASSWORD\"}" | jq -r .token)"
AUTH="Authorization: Bearer $TOKEN"

echo "2/6 create a publisher source"
SRC_JSON="$(curl -fsS -X POST "$API/sources" -H "$AUTH" -H 'Content-Type: application/json' \
  -d "{\"name\":\"smoke-$STAMP\",\"kind\":\"publisher\",\"transport\":\"tcp\"}")"
SOURCE_ID="$(echo "$SRC_JSON" | jq -r .id)"
SOURCE_PATH="$(echo "$SRC_JSON" | jq -r .path)"
echo "    source path: $SOURCE_PATH"

PUBLISHER=""
if [ "$WITH_PUBLISHER" = "1" ]; then
  PUBLISHER="rc-smoke-ffmpeg-$STAMP"
  echo "    starting a synthetic publisher (ffmpeg)"
  docker run -d --rm --name "$PUBLISHER" --network "$PUBLISH_NETWORK" linuxserver/ffmpeg \
    -re -f lavfi -i "testsrc2=size=640x360:rate=15" -f lavfi -i "sine=frequency=1000" \
    -c:v libx264 -preset ultrafast -tune zerolatency -g 15 -pix_fmt yuv420p \
    -c:a aac -b:a 64k -f rtsp -rtsp_transport tcp "$PUBLISH_URL/$SOURCE_PATH" >/dev/null
  i=0
  while :; do
    STATUS="$(curl -fsS "$API/sources" -H "$AUTH" | jq -r --arg id "$SOURCE_ID" '.items[] | select(.id==$id) | .status')"
    [ "$STATUS" = "online" ] && break
    i=$((i + 1))
    [ "$i" -gt 60 ] && { echo "publisher did not come online"; docker rm -f "$PUBLISHER" >/dev/null; exit 1; }
    sleep 0.5
  done
  echo "    source is online"
  sleep 2
fi

echo "3/6 create $CAMERAS virtual cameras"
PREFIX="smk$STAMP"
CREATED="$(curl -fsS -X POST "$API/cameras/bulk" -H "$AUTH" -H 'Content-Type: application/json' \
  -d "{\"sourceId\":\"$SOURCE_ID\",\"count\":$CAMERAS,\"prefix\":\"$PREFIX\",\"startIndex\":1,\"group\":\"smoke\",\"maxReaders\":8,\"onDemand\":true}" \
  | jq -r .created)"
[ "$CREATED" = "$CAMERAS" ] || { echo "created $CREATED cameras, expected $CAMERAS"; exit 1; }

echo "4/6 wait for MediaMTX"
WANT=$((CAMERAS + 1))
i=0
while :; do
  COUNT="$(curl -fsS "$MEDIA_API/v3/config/paths/list?itemsPerPage=1000" | jq -r .itemCount)"
  [ "$COUNT" -ge "$WANT" ] && break
  i=$((i + 1))
  [ "$i" -gt 40 ] && { echo "MediaMTX has $COUNT paths, expected $WANT"; exit 1; }
  sleep 0.5
done
echo "    $COUNT paths configured"

echo "5/6 load test"
RUN_ID="$(curl -fsS -X POST "$API/loadtest" -H "$AUTH" -H 'Content-Type: application/json' \
  -d "{\"count\":$CAMERAS,\"concurrency\":16,\"timeoutMs\":8000}" | jq -r .id)"
while :; do
  RUN="$(curl -fsS "$API/loadtest/$RUN_ID" -H "$AUTH")"
  [ "$(echo "$RUN" | jq -r .status)" = "done" ] && break
  sleep 0.5
done
OK="$(echo "$RUN" | jq -r .ok)"
FAIL="$(echo "$RUN" | jq -r .fail)"
P95="$(echo "$RUN" | jq -r .p95Ms)"
echo "    ok=$OK fail=$FAIL p95=${P95}ms"

echo "6/6 overview"
curl -fsS "$API/overview" -H "$AUTH" | jq -c '{cameras: .overview.camerasTotal, ready: .overview.camerasReady, idle: .overview.camerasIdle, managedPaths: .stats.managedPaths}'

if [ -n "$PUBLISHER" ]; then
  docker rm -f "$PUBLISHER" >/dev/null
  echo "    stopped the synthetic publisher"
fi
[ "$KEEP" = "1" ] || curl -fsS -X DELETE "$API/sources/$SOURCE_ID" -H "$AUTH" >/dev/null

[ "$FAIL" = "0" ] || { echo "FAIL: $FAIL DESCRIBE requests failed"; exit 1; }
echo "PASS"
