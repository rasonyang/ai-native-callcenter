#!/bin/sh
# SPDX-License-Identifier: Apache-2.0
#
# Pushes the rendered API reference into the site repository, which is what
# publishes it on rasonyang.dev (the api-docs job in
# .github/workflows/release.yml; Hugo serves static/ verbatim):
#
#   BLOG_DEPLOY_KEY=<ssh private key> scripts/api-docs-publish.sh <tag> <dir>
#
# <dir> holds what scripts/api-docs.sh rendered. The files land in
# static/aicc/api/<tag>/ of rasonyang/rasonyang.github.io —
# https://rasonyang.dev/aicc/api/<tag>/ — and a full release (a tag without
# a hyphen) also replaces static/aicc/api/ — https://rasonyang.dev/aicc/api/
# — the same latest rule as releases/latest. Re-running a tag produces no
# diff and no commit. The deploy key needs write access on the site
# repository; without it a tag build fails here rather than skipping
# quietly.

set -eu

die() {
    printf 'api-docs-publish: %s\n' "$*" >&2
    exit 1
}

[ $# -eq 2 ] || die "usage: scripts/api-docs-publish.sh <tag> <dir>"
tag=$1
dir=$2

[ -f "$dir/index.html" ] && [ -f "$dir/openapi.json" ] \
    || die "$dir must hold index.html and openapi.json; render it with scripts/api-docs.sh"
[ -n "${BLOG_DEPLOY_KEY:-}" ] \
    || die "BLOG_DEPLOY_KEY is not set; it is the ssh deploy key with write access on rasonyang/rasonyang.github.io"

work=$(mktemp -d "${TMPDIR:-/tmp}/aicc-api-docs.XXXXXX")
trap 'rm -rf "$work"' EXIT

# The deploy key sits alone in the temp dir, named for ssh through
# GIT_SSH_COMMAND on the clone and the push; it never enters the clone.
key=$work/deploy_key
( umask 077 && printf '%s\n' "$BLOG_DEPLOY_KEY" > "$key" )
mkdir -p "$HOME/.ssh"
ssh-keyscan github.com >> "$HOME/.ssh/known_hosts" 2>/dev/null
export GIT_SSH_COMMAND="ssh -i \"$key\" -o IdentitiesOnly=yes"

# main is the branch its pages.yml deploys.
git clone --quiet --depth 1 --branch main \
    git@github.com:rasonyang/rasonyang.github.io.git "$work/blog"

dest=$work/blog/static/aicc/api
mkdir -p "$dest/$tag"
cp "$dir/index.html" "$dir/openapi.json" "$dest/$tag/"

# A tag without a hyphen is a full release: its page is also the unversioned
# one, the same rule that makes it releases/latest on GitHub.
case $tag in
*-*) ;;
*)
    cp "$dir/index.html" "$dir/openapi.json" "$dest/"
    ;;
esac

git -C "$work/blog" add -A -- static/aicc/api
if git -C "$work/blog" diff --cached --quiet; then
    printf 'api-docs-publish: %s is already published; no diff, no commit\n' "$tag"
    exit 0
fi

git -C "$work/blog" \
    -c user.name='github-actions[bot]' \
    -c user.email='41898282+github-actions[bot]@users.noreply.github.com' \
    commit --quiet -m "aicc api reference: $tag"
git -C "$work/blog" push --quiet origin HEAD:main
printf 'api-docs-publish: pushed %s\n' "$tag"
