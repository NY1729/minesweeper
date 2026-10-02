#!/usr/bin/env bash
# Remove the test users ("load-N") and test chunks that scripts/loadtest.sh left in Turso.
# Uses the credentials in backend/.env. Only touches those ids and chunks far from the real world.
set -euo pipefail
cd "$(dirname "$0")/.."
[ -f backend/.env ] || { echo "backend/.env not found"; exit 1; }

echo "waiting 5s so the backend finishes saving the test chunks first..."
sleep 5
docker run --rm --env-file backend/.env -v "$PWD/backend":/src:ro golang:1.24-alpine sh -c '
  cp -r /src /w && cd /w && go mod tidy >/dev/null &&
  go run ./cmd/loadtest -cleanup -chunk-base 900000 "$@"' sh "$@"
