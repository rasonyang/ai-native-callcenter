#!/bin/sh
# SPDX-License-Identifier: Apache-2.0
#
# Fails when web/package-lock.json resolves a package anywhere but
# registry.npmjs.org (#525). npm records the registry of the machine that ran
# `npm install`, so a lock written behind a mirror pins every other machine,
# the image build and the release included, to that mirror.
#
#   scripts/check-web-lockfile.sh          check
#   scripts/check-web-lockfile.sh --fix    rewrite the hosts in place
set -eu

lock="$(dirname "$0")/../web/package-lock.json"
want='https://registry.npmjs.org/'

if [ "${1:-}" = "--fix" ]; then
	# Only the host changes: the path (/<name>/-/<file>.tgz) is the same on
	# every npm-compatible registry, and integrity does not depend on the host.
	sed -E 's#("resolved": ")https://[^/]+/#\1'"$want"'#' "$lock" >"$lock.tmp"
	mv "$lock.tmp" "$lock"
	exit 0
fi

bad=$(grep '"resolved":' "$lock" | grep -vc "\"resolved\": \"$want" || true)
if [ "$bad" -ne 0 ]; then
	echo "web/package-lock.json: $bad package(s) resolve outside $want" >&2
	grep '"resolved":' "$lock" | grep -v "\"resolved\": \"$want" | head -3 >&2
	echo "A lock written behind an npm mirror pins every other machine to it." >&2
	echo "Fix it with: make web-lock-fix" >&2
	exit 1
fi
