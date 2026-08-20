#!/bin/sh
# SPDX-License-Identifier: Apache-2.0
# Dev restart: build, stop the old instance, wait for the instance lock to
# release, start, and confirm the new instance is actually serving. The lock
# outlives the process by however long graceful shutdown takes, which is why
# a plain kill-and-start races.
set -e

cd "$(dirname "$0")/../.."
go build -o /tmp/aicc ./cmd/aicc

pkill -f /tmp/aicc 2>/dev/null || true

marker="logs/.restart-marker"
touch "$marker"

attempt=0
while [ $attempt -lt 15 ]; do
  attempt=$((attempt + 1))
  # Recording settings come from .env: dev records through mod_http_cache
  # straight into SeaweedFS (deploy/dev/docker-compose.yml), no local spool.
  nohup /tmp/aicc >/dev/null 2>&1 &
  sleep 3

  latest=$(find logs -name 'aicc-*.log' -newer "$marker" | sort | tail -1)
  if [ -n "$latest" ] && grep -q "voice leg listening" "$latest" 2>/dev/null; then
    rm -f "$marker"
    echo "started: $latest"
    exit 0
  fi
done

rm -f "$marker"
echo "failed to start; last log:" >&2
tail -3 "$(ls -t logs/aicc-*.log | head -1)" >&2
exit 1
