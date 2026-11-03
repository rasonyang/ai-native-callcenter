#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# DigitalOcean User Data / cloud-init script for an SSH-only sales workspace preview.
# Paste this whole script into Create Droplet > Additional Options > Startup scripts.
# It does not accept customer records or API keys and does not enable real calls.
set -Eeuo pipefail
exec > >(tee -a /var/log/aicc-sales-bootstrap.log) 2>&1

if [[ "$(id -u)" != "0" ]]; then
  echo "This script must run as root under cloud-init." >&2
  exit 1
fi
if [[ "$(. /etc/os-release && echo "$ID:$VERSION_ID")" != "ubuntu:24.04" ]]; then
  echo "Use a fresh Ubuntu 24.04 LTS x86_64 Droplet." >&2
  exit 1
fi

export DEBIAN_FRONTEND=noninteractive
apt-get update -y
apt-get install -y ca-certificates curl git openssl

# Install Docker Engine and Compose from Docker's supported Ubuntu repository.
install -m 0755 -d /etc/apt/keyrings
curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc
chmod a+r /etc/apt/keyrings/docker.asc
. /etc/os-release
cat > /etc/apt/sources.list.d/docker.sources <<DOCKER_REPO
Types: deb
URIs: https://download.docker.com/linux/ubuntu
Suites: ${UBUNTU_CODENAME:-$VERSION_CODENAME}
Components: stable
Architectures: $(dpkg --print-architecture)
Signed-By: /etc/apt/keyrings/docker.asc
DOCKER_REPO
apt-get update -y
apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
systemctl enable --now docker

# Cloud metadata supplies an address actually assigned to this Droplet.
# No IP, password, token or SSH key is baked into this file.
PUBLIC_IP="$(curl --fail --silent --show-error --max-time 15 \
  http://169.254.169.254/metadata/v1/interfaces/public/0/ipv4/address)"
if [[ ! "$PUBLIC_IP" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "Droplet metadata did not return a public IPv4 address." >&2
  exit 1
fi
if ! ip -4 -o addr show | grep -Eq "[[:space:]]${PUBLIC_IP}/"; then
  echo "The metadata IPv4 is not bound on a local network interface." >&2
  exit 1
fi

# Build THIS fork's React/Go code, never the original author's release image.
# The preview helper generates its own secrets and binds HTTP/SIP to localhost.
git clone --depth 1 --single-branch --branch feat/sales-workbench \
  https://github.com/siyingchao007-maker/ai-native-callcenter.git \
  /opt/ai-native-callcenter
bash /opt/ai-native-callcenter/scripts/sales-preview-up.sh "$PUBLIC_IP"

# Check the actual web endpoint instead of claiming success on a Docker exit code.
for n in $(seq 1 30); do
  if curl --fail --silent --show-error --max-time 3 \
    http://127.0.0.1:8080/login >/dev/null 2>&1; then
    echo "Sales workspace HTTP endpoint responds on 127.0.0.1:8080."
    echo "Use SSH local-port forwarding for access; do NOT open port 8080 to the public Internet."
    exit 0
  fi
  sleep 5
done
echo "Web service did not become healthy. Check Docker Compose logs." >&2
cd /opt/ai-native-callcenter/deploy
docker compose ps -a
docker compose logs --tail=100 aicc
exit 1
