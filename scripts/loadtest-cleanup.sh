#!/usr/bin/env bash
# Remove the test users ("load-N") and test chunks that scripts/loadtest.sh left in the game
# database. Only touches those ids and chunks far from the real world. Works while the
# server runs (SQLite waits for its turn).
set -euo pipefail
cd "$(dirname "$0")/.."

cid=${BACKEND_CONTAINER:-$(docker compose ps -aq backend)}
[ -n "$cid" ] || { echo "no backend container found (docker compose up -d)"; exit 1; }
vol=$(docker inspect -f '{{range .Mounts}}{{if eq .Destination "/data"}}{{.Name}}{{end}}{{end}}' "$cid")
[ -n "$vol" ] || { echo "the backend has no /data volume"; exit 1; }

echo "waiting 5s so the backend finishes saving the test chunks first..."
sleep 5
# the same user as the server (65532), so the database files keep their owner
docker run --rm --user 65532:65532 -e HOME=/tmp -e GOCACHE=/tmp/gocache -v "$vol":/data -v "$PWD/backend":/src:ro golang:1.24-alpine sh -c '
  cp -r /src /tmp/w && cd /tmp/w && go mod tidy >/dev/null &&
  go run ./cmd/loadtest -cleanup -db /data/minesweeper.db -chunk-base 900000 "$@"' sh "$@"
