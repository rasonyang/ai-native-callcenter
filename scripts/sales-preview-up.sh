#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Local/private preview of this fork's sales workspace.
# The published upstream release does NOT contain the fork's web changes.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DEPLOY_DIR="$REPO_ROOT/deploy"
ENV_FILE="$DEPLOY_DIR/.env"
EXTERNAL_IP="${1:-}"

if [[ -z "$EXTERNAL_IP" ]]; then
  printf 'Usage: %s <host-LAN-IP>\n' "$0" >&2
  printf 'Find the LAN IP of the Docker host; it must be an address that the host owns.\n' >&2
  exit 2
fi
if [[ ! "$EXTERNAL_IP" =~ ^[a-zA-Z0-9.:_-]+$ ]]; then
  printf 'Invalid host address. Use a host-owned LAN IP address.\n' >&2
  exit 2
fi
for command in docker openssl git; do
  if ! command -v "$command" >/dev/null 2>&1; then
    printf '%s is required.\n' "$command" >&2
    exit 1
  fi
done
if ! docker compose version >/dev/null 2>&1; then
  printf 'Docker Compose v2 is required.\n' >&2
  exit 1
fi
if [[ -e "$ENV_FILE" ]]; then
  printf 'Refusing to overwrite %s. Back it up and configure the existing stack manually.\n' "$ENV_FILE" >&2
  exit 1
fi

if [[ "$(git -C "$REPO_ROOT" branch --show-current)" != "feat/sales-workbench" ]]; then
  printf 'This preview is intended for the feat/sales-workbench branch.\n' >&2
  exit 1
fi

umask 077
cp "$DEPLOY_DIR/.env.example" "$ENV_FILE"
# FS_EXTERNAL_IP is the only required un-commented host topology setting.
sed -i.bak "s/^FS_EXTERNAL_IP=$/FS_EXTERNAL_IP=$EXTERNAL_IP/" "$ENV_FILE"
rm -f "$ENV_FILE.bak"

# No model key, no public carrier gateway, no listening ports on public interfaces.
{
  printf '\n# Private sales-workbench preview: no real outbound connectivity.\n'
  printf 'AICC_PROVIDER=none\n'
  printf 'AICC_OUTBOUND_ENDPOINT=\n'
  printf 'HTTP_BIND=127.0.0.1\n'
  printf 'SIP_BIND=127.0.0.1\n'
  printf 'AICC_SEED=demo\n'
  printf 'POSTGRES_PASSWORD=%s\n' "$(openssl rand -hex 24)"
  printf 'LUA_PASSWORD=%s\n' "$(openssl rand -hex 24)"
  printf 'ESL_PASSWORD=%s\n' "$(openssl rand -hex 24)"
  printf 'AICC_SEED_PASSWORD=%s\n' "$(openssl rand -hex 18)"
} >> "$ENV_FILE"
chmod 600 "$ENV_FILE"

printf 'Starting the fork from source with a private demo dataset.\n'
(
  cd "$DEPLOY_DIR"
  docker compose up -d --build
  docker compose ps
)
printf '\nOpen http://127.0.0.1:8080 on the server, or tunnel it using:\n'
printf '  ssh -L 8080:127.0.0.1:8080 USER@SERVER\n'
printf '\nDemo administrator: admin\n'
printf 'Password: read AICC_SEED_PASSWORD from deploy/.env locally. Never paste it into chat.\n'
printf 'No carrier trunk is configured; actual outbound calling is disabled.\n'
