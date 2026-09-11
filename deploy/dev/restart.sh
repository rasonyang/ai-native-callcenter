#!/bin/sh
# SPDX-License-Identifier: Apache-2.0
# Dev restart: build, stop the old instance, wait for the instance lock to
# release, start, and confirm the new instance is actually serving. The lock
# outlives the process by however long graceful shutdown takes, which is why
# a plain kill-and-start races.
set -e

cd "$(dirname "$0")/../.."

# The SPA first, because it is served from inside the binary (go:embed
# web/dist) and go build will happily embed a stale one. Skipping this step
# costs a debugging session rather than a compile error: on 2026-08-26 a
# removed endpoint had already been replaced everywhere in web/src, and the
# browser went on calling the old one from a bundle built the day before —
# the failure looked like a bug in the new code and was not.
#
# Set AICC_SKIP_WEB_BUILD=1 to leave web/dist alone, for the loop where the
# Vite dev server is serving the frontend and only Go is changing.
#
# Quiet on success, whole output on failure: the bundler narrates its plugin
# configuration on every run, which would bury the one line this script exists
# to print — but a build that broke has to say why.
if [ "${AICC_SKIP_WEB_BUILD:-0}" != "1" ]; then
  if ! web_out=$(cd web && npm run build 2>&1); then
    printf '%s\n' "$web_out" >&2
    echo "the SPA did not build; the binary would have embedded a stale one" >&2
    exit 1
  fi
fi

go build -o /tmp/aicc ./cmd/aicc

pkill -f /tmp/aicc 2>/dev/null || true

# The application creates logs/ on its first start, but the marker has to
# exist before that start, and the directory is gitignored: a fresh checkout
# has no logs/ yet.
mkdir -p logs
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
