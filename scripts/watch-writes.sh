#!/usr/bin/env bash
# Is the backend still writing chunks to Turso? Prints the chunk count and the seconds since the
# last write every 10s, and says so when writes have stopped (count unchanged 3 times in a row).
# Needs python3 and backend/.env. Read-only. Ctrl+C to quit.
#   scripts/watch-writes.sh            # all chunks
#   scripts/watch-writes.sh 100000     # only chunks with chunk_x >= 100000 (the load test's area)
set -euo pipefail
cd "$(dirname "$0")/.."
[ -f backend/.env ] || { echo "backend/.env not found"; exit 1; }
set -a; . backend/.env; set +a

python3 -u - "${1:-}" <<'PY'
import json, os, sys, time, urllib.request

min_x = sys.argv[1]
where = f"WHERE chunk_x >= {int(min_x)}" if min_x else ""
url = os.environ["TURSO_DATABASE_URL"].replace("libsql://", "https://").rstrip("/") + "/v2/pipeline"
# Rows being re-saved keep the count the same, so also watch the newest updated_at: while the
# backend (or Turso's queue of earlier requests) is still writing, it keeps moving forward.
sql = f"SELECT count(*), max(updated_at), strftime('%s','now') FROM chunks {where}"

def ask():
    body = json.dumps({"requests": [{"type": "execute", "stmt": {"sql": sql}}]}).encode()
    req = urllib.request.Request(url, body, {"Authorization": "Bearer " + os.environ["TURSO_AUTH_TOKEN"], "Content-Type": "application/json"})
    r = json.load(urllib.request.urlopen(req, timeout=30))["results"][0]
    if r["type"] == "error":
        raise RuntimeError(r["error"]["message"])
    row = [c.get("value") for c in r["response"]["result"]["rows"][0]]
    return int(row[0]), int(row[1] or 0), int(row[2])

prev = None
quiet = 0
while True:
    try:
        n, newest, now = ask()
    except Exception as e:  # Turso busy: that itself suggests writes are still queued
        print(time.strftime("%H:%M:%S"), "query failed:", e)
        time.sleep(10)
        continue
    if n == 0:
        print(f"{time.strftime('%H:%M:%S')}  chunks=0  no rows match, so nothing is being written there")
        time.sleep(10)
        continue
    first = prev is None
    quiet = quiet + 1 if prev == (n, newest) else 0
    prev = (n, newest)
    verdict = "first check" if first else "STOPPED (nothing changed for 3 checks)" if quiet >= 3 else "still writing" if quiet == 0 else "no change yet..."
    print(f"{time.strftime('%H:%M:%S')}  chunks={n}  newest write={now - newest}s ago  {verdict}")
    time.sleep(10)
PY
