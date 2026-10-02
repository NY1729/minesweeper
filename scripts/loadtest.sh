#!/usr/bin/env bash
# Load test the backend from the server itself, inside the docker network, so Cloudflare is not
# in the path (Cloudflare allows ~5 connections per client IP and rejects the spoofed
# CF-Connecting-IP header the tool uses to look like many different players).
#
#   scripts/loadtest.sh -n 300 -dur 60s          # 300 players, each looking at its own area
#   scripts/loadtest.sh -mode crowded -n 100     # everyone looks at the same area (worst case)
#   scripts/loadtest.sh -h                       # all options
#
# Defaults are chosen for the production database: flags only (no score changes, so the
# ranking is untouched) in an area far from real players. It still writes test users and
# chunks to the database: run scripts/loadtest-cleanup.sh afterwards.
set -euo pipefail
cd "$(dirname "$0")/.."

cid=${BACKEND_CONTAINER:-$(docker compose ps -q backend)}
[ -n "$cid" ] || { echo "the backend container is not running (docker compose up -d)"; exit 1; }
net=$(docker inspect -f '{{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}' "$cid" | awk '{print $1}')
host=$(docker inspect -f '{{.Name}}' "$cid" | tr -d /)
# in the compose network the service name "backend" resolves; a bare container name works too
[ "$host" = "${host#*-backend-}" ] || host=backend

stats=$(mktemp)
trap 'kill $sampler 2>/dev/null || true; rm -f "$stats"' EXIT
( while true; do docker stats --no-stream --format '{{.CPUPerc}} {{.MemUsage}}' "$cid" >> "$stats" 2>/dev/null || break; done ) &
sampler=$!

echo "backend container: $cid (network $net, host $host)"
docker run --rm --network "$net" --ulimit nofile=65536:65536 -v "$PWD/backend":/src:ro golang:1.24-alpine sh -c '
  host=$1; shift
  cp -r /src /w && cd /w && go mod tidy >/dev/null &&
  go run ./cmd/loadtest -url "ws://$host:8080/api/ws" -health "http://$host:8080/api/health" \
    -spoof-ip -flags-only -chunk-base 900000 "$@"' sh "$host" "$@"

kill $sampler 2>/dev/null || true
awk '
  { c = $1; sub("%", "", c); if (c + 0 > cpu) cpu = c + 0; sum += c; n++
    m = $2; u = m; gsub("[0-9.]", "", u); sub("[A-Za-z]+", "", m)
    mb = (u == "GiB") ? m * 1024 : (u == "KiB") ? m / 1024 : (u == "B") ? m / 1048576 : m + 0
    if (mb > mem) mem = mb }
  END { if (n) printf "backend container: CPU peak %.0f%% avg %.0f%% (100%% = one core) | memory peak %.0f MiB\n", cpu, sum / n, mem }' "$stats"
echo "now run scripts/loadtest-cleanup.sh to remove the test data from the database"
