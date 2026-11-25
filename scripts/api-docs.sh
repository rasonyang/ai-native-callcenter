#!/bin/sh
# SPDX-License-Identifier: Apache-2.0
#
# Renders the versioned API reference a release publishes to rasonyang.dev
# (the api-docs job in .github/workflows/release.yml):
#
#   scripts/api-docs.sh <outdir>
#
# writes into <outdir>
#   index.html    a page that loads Scalar's standalone API reference from
#                 jsDelivr at a pinned exact version and points it at the
#                 sibling contract; the test-request button is hidden — the
#                 page is read-only and the contract's relative /api/v1
#                 server resolves nowhere on rasonyang.dev
#   openapi.json  docs/openapi.json, byte for byte — the same bytes the
#                 binary embeds
#
# The contract is a sibling file the page fetches, so check the render over
# HTTP, never file://:
#
#   make api-docs && (cd dist/api-docs && python3 -m http.server)

set -eu

die() {
    printf 'api-docs: %s\n' "$*" >&2
    exit 1
}

[ $# -eq 1 ] || die "usage: scripts/api-docs.sh <outdir>"
out=$1

root=$(cd "$(dirname "$0")/.." && pwd)
[ -f "$root/docs/openapi.json" ] || die "docs/openapi.json is missing"

mkdir -p "$out"
cp "$root/docs/openapi.json" "$out/openapi.json"

# The standalone bundle mounts a div before the script element carrying
# id=api-reference and reads data-url / data-configuration off that element,
# so the loader and its configuration are one tag. Keep the jsDelivr URL an
# exact version: a page already pushed under a tag must keep rendering the
# way it was cut, so the pin is bumped deliberately, never ranged.
cat > "$out/index.html" <<'HTML'
<!doctype html>
<html lang="en">
  <head>
    <meta charset="utf-8" />
    <meta name="viewport" content="width=device-width, initial-scale=1" />
    <title>AI Native Call Center API reference</title>
  </head>
  <body>
    <script
      id="api-reference"
      data-url="openapi.json"
      data-configuration='{"hideTestRequestButton":true}'
      src="https://cdn.jsdelivr.net/npm/@scalar/api-reference@1.78.1/dist/browser/standalone.js"
    ></script>
    <noscript>The API reference needs JavaScript; the contract itself is at openapi.json.</noscript>
  </body>
</html>
HTML

printf 'api-docs: %s\n' "$out/index.html" "$out/openapi.json"
