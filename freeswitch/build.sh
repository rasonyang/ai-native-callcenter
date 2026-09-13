#!/bin/bash
# SPDX-License-Identifier: Apache-2.0
#
# Builds rasonyang/freeswitch-aicc from this repository.
#
#   VERSION=v0.1.0 freeswitch/build.sh
#
# The image carries the application's release tag, not the FreeSWITCH version:
# rasonyang/freeswitch-aicc:v0.1.0 and rasonyang/ai-native-callcenter:v0.1.0
# come from the same commit. The switch's Lua scripts and the application's
# migrations share the luacc.* contract, so a change to either needs a new tag
# on both, and a FreeSWITCH-versioned tag gave a changed scripts/ nothing to be
# named by. There is no latest: a tag that moves cannot be named.
#
# There is no staging step. The build context is freeswitch/ as it is in the
# working tree, and everything else the image needs — the FreeSWITCH sources,
# mod_audio_stream, the sound files — is fetched inside the build at a pin the
# Dockerfile names. Nothing is read from the machine running this script, which
# is the whole difference from the recipe this replaces: that one rsynced the
# build host's /usr/local/freeswitch/{conf,scripts,sounds} into a temporary
# context and shipped one office's credentials and addresses to Docker Hub.
#
#   VERSION     release tag for the image, required (e.g. v0.1.0, the value
#               `make image VERSION=...` uses for the application)
#   IMAGE       image name (default rasonyang/freeswitch-aicc)
#   FS_REF      FreeSWITCH source tag to build (default v1.11.3); recorded in the
#               freeswitch.version label, never used as an image tag
#   PLATFORM    target platforms (default linux/amd64,linux/arm64)
#   LOAD        set to 1 to build one platform and --load it into the local
#               daemon instead of pushing, for a local test
#   MAKE_JOBS   make concurrency (default nproc). Under QEMU emulation use 2-3:
#               gcc segfaults at random with more, and the failure looks like a
#               source problem rather than an emulation one.
#   SOUNDS      8000 (default) or none — see the Dockerfile
#   ALLOW_DIRTY set to 1 to push from a dirty working tree anyway
#
# On Apple Silicon, run Colima with --vz-rosetta so the amd64 half compiles
# under Rosetta; QEMU user-mode emulation is not reliable for gcc.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$HERE/.." && pwd)"
VERSION="${VERSION:-}"
if [ -z "$VERSION" ]; then
  echo "VERSION is required: the release tag the image is published under, e.g. VERSION=v0.1.0" >&2
  exit 1
fi
IMAGE="${IMAGE:-rasonyang/freeswitch-aicc}"
FS_REF="${FS_REF:-v1.11.3}"
PLATFORM="${PLATFORM:-linux/amd64,linux/arm64}"
LOAD="${LOAD:-}"
SOUNDS="${SOUNDS:-8000}"

# The revision goes into org.opencontainers.image.revision so a pushed image
# names the commit it came from. A dirty tree makes that label a lie, so a
# push from one is refused rather than mislabelled; a local --load build is
# somebody's own experiment and is only marked.
GIT_REVISION="$(git -C "$REPO" rev-parse HEAD 2>/dev/null || echo unknown)"
if [ -n "$(git -C "$REPO" status --porcelain 2>/dev/null)" ]; then
  GIT_REVISION="$GIT_REVISION-dirty"
  if [ -z "$LOAD" ] && [ "${ALLOW_DIRTY:-}" != "1" ]; then
    cat >&2 <<EOF
refusing to push from a dirty working tree.

The image is labelled with the commit it was built from, and $GIT_REVISION is
not a commit anybody else can check out. Commit first, or set ALLOW_DIRTY=1 if
you mean to push something unreproducible, or use LOAD=1 to build locally.
EOF
    exit 1
  fi
fi

OUTPUT=(--push)
if [ -n "$LOAD" ]; then
  case "$PLATFORM" in *,*) echo "LOAD=1 requires a single platform in PLATFORM" >&2; exit 1 ;; esac
  OUTPUT=(--load)
fi

docker buildx build --platform "$PLATFORM" \
  -f "$HERE/Dockerfile" \
  --build-arg FS_VERSION="$FS_REF" \
  --build-arg MAKE_JOBS="${MAKE_JOBS:-}" \
  --build-arg SOUNDS="$SOUNDS" \
  --build-arg GIT_REVISION="$GIT_REVISION" \
  --build-arg VERSION="$VERSION" \
  -t "$IMAGE:$VERSION" \
  "${OUTPUT[@]}" \
  "$HERE"

echo "== built $IMAGE:$VERSION (FreeSWITCH $FS_REF) for $PLATFORM (${OUTPUT[*]}) at $GIT_REVISION"

# The Docker Hub overview is DOCKERHUB.md. Publishing it is a separate step
# that needs a Hub credential, and the script that does it is the owner's:
# /usr/local/src/freeswitch/docker/aicc/hub-overview.sh, outside this
# repository, reading ~/.docker/config.json. Nothing here automates it.
