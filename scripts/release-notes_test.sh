#!/bin/sh
# SPDX-License-Identifier: Apache-2.0
#
# Tests for scripts/release-notes.sh's `## API changes` section:
#
#   sh scripts/release-notes_test.sh
#
# The script finds its repository from its own path, so a copy of it runs from
# a fixture repo built here — the fixture's own tags, docs/openapi.json
# revisions and CHANGELOG.md drive the assertions. Copies of the repository's
# go.mod and go.sum let `go tool oasdiff` resolve the pinned oasdiff inside
# the fixture. Nothing outside a temporary directory is touched; git and go
# must be on PATH.

here=$(cd "$(dirname "$0")" && pwd)

failures=0
tests=0
TMP=$(mktemp -d "${TMPDIR:-/tmp}/aicc-release-notes-test.XXXXXX")
trap 'rm -rf "$TMP"' EXIT

eq() { # name got want
    tests=$((tests + 1))
    if [ "$2" = "$3" ]; then
        printf 'ok   %s\n' "$1"
    else
        failures=$((failures + 1))
        printf 'FAIL %s\n  got:  %s\n  want: %s\n' "$1" "$2" "$3"
    fi
}

has() { # name file pattern
    tests=$((tests + 1))
    if grep -qF -- "$3" "$2"; then
        printf 'ok   %s\n' "$1"
    else
        failures=$((failures + 1))
        printf 'FAIL %s\n  missing in %s: %s\n' "$1" "$2" "$3"
    fi
}

nothas() { # name file pattern
    tests=$((tests + 1))
    if grep -qF -- "$3" "$2"; then
        failures=$((failures + 1))
        printf 'FAIL %s\n  unexpected in %s: %s\n' "$1" "$2" "$3"
    else
        printf 'ok   %s\n' "$1"
    fi
}

notes() { # tag -> writes $TMP/notes-<tag>.md
    if bash "$FIX/scripts/release-notes.sh" "$1" >"$TMP/notes-$1.md" 2>"$TMP/notes-$1.err"; then
        eq "notes $1: exits 0" 0 0
    else
        eq "notes $1: exits 0" "$?" 0
    fi
}

# ------------------------------------------------------ the fixture repo ----
#
# One linear history, tagged as a line of releases:
#
#   c0  no docs/openapi.json yet                          v0.0.1
#   c1  spec A                                            v0.1.0
#   c2  spec B: spec A + POST /queues                     v0.2.0-rc.1
#   c3  spec C: spec B − DELETE /calls, GET /calls gains
#       a required `state` query parameter                v0.2.0-rc.2
#   c4  spec C again + CHANGELOG.md                       v0.2.0
#   c5  spec C again, an unrelated file changes           v0.3.0-rc.1
#
# B→C (the delta a pre-release shows) is two breaking changes; A→C (the delta
# the full release shows) is the same two plus the endpoint the rc line added.

FIX="$TMP/repo"
mkdir -p "$FIX/scripts" "$FIX/docs"
cp "$here/release-notes.sh" "$FIX/scripts/release-notes.sh"
cp "$here/../go.mod" "$here/../go.sum" "$FIX/"

spec_a() { cat >"$FIX/docs/openapi.json" <<'EOF'
{
  "openapi": "3.1.0",
  "info": { "title": "Fixture API", "version": "0.1.0" },
  "paths": {
    "/calls": {
      "get": {
        "operationId": "listCalls",
        "responses": { "200": { "description": "ok" } }
      },
      "delete": {
        "operationId": "deleteCalls",
        "responses": { "204": { "description": "gone" } }
      }
    },
    "/agents/{id}": {
      "get": {
        "operationId": "getAgent",
        "parameters": [
          { "name": "id", "in": "path", "required": true, "schema": { "type": "string" } }
        ],
        "responses": { "200": { "description": "ok" } }
      }
    }
  }
}
EOF
}

spec_b() { cat >"$FIX/docs/openapi.json" <<'EOF'
{
  "openapi": "3.1.0",
  "info": { "title": "Fixture API", "version": "0.2.0-rc.1" },
  "paths": {
    "/calls": {
      "get": {
        "operationId": "listCalls",
        "responses": { "200": { "description": "ok" } }
      },
      "delete": {
        "operationId": "deleteCalls",
        "responses": { "204": { "description": "gone" } }
      }
    },
    "/agents/{id}": {
      "get": {
        "operationId": "getAgent",
        "parameters": [
          { "name": "id", "in": "path", "required": true, "schema": { "type": "string" } }
        ],
        "responses": { "200": { "description": "ok" } }
      }
    },
    "/queues": {
      "post": {
        "operationId": "createQueue",
        "responses": { "201": { "description": "made" } }
      }
    }
  }
}
EOF
}

spec_c() { cat >"$FIX/docs/openapi.json" <<'EOF'
{
  "openapi": "3.1.0",
  "info": { "title": "Fixture API", "version": "0.2.0" },
  "paths": {
    "/calls": {
      "get": {
        "operationId": "listCalls",
        "parameters": [
          { "name": "state", "in": "query", "required": true, "schema": { "type": "string" } }
        ],
        "responses": { "200": { "description": "ok" } }
      }
    },
    "/agents/{id}": {
      "get": {
        "operationId": "getAgent",
        "parameters": [
          { "name": "id", "in": "path", "required": true, "schema": { "type": "string" } }
        ],
        "responses": { "200": { "description": "ok" } }
      }
    },
    "/queues": {
      "post": {
        "operationId": "createQueue",
        "responses": { "201": { "description": "made" } }
      }
    }
  }
}
EOF
}

cd "$FIX" || exit 1
git init -q
git config user.name 'fixture'
git config user.email 'fixture@example.com'
git config commit.gpgsign false
git config tag.gpgsign false

commit() { git add -A && git commit -qm "$1"; }

echo seed >file.txt
commit 'seed'
git tag -a v0.0.1 -m v0.0.1

spec_a
commit 'spec A'
git tag -a v0.1.0 -m v0.1.0

spec_b
commit 'spec B'
git tag -a v0.2.0-rc.1 -m v0.2.0-rc.1

spec_c
commit 'spec C'
git tag -a v0.2.0-rc.2 -m v0.2.0-rc.2

cat >CHANGELOG.md <<'EOF'
# Changelog

## v0.2.0

- fixture changelog body for the full release

## v0.1.0

- fixture changelog body for the first tagged contract
EOF
commit 'the release'
git tag -a v0.2.0 -m v0.2.0

echo more >>file.txt
commit 'unrelated change'
git tag -a v0.3.0-rc.1 -m v0.3.0-rc.1

cd "$here" || exit 1

# -------------------------------------------------------------- tests ----

# A first tag: v0.0.1 has no earlier `v*` tag at all.
notes v0.0.1
has 'v0.0.1: API section present' "$TMP/notes-v0.0.1.md" '## API changes'
has 'v0.0.1: no earlier tag' "$TMP/notes-v0.0.1.md" 'No previous release to compare'
nothas 'v0.0.1: no changelog subsections' "$TMP/notes-v0.0.1.md" '### All changes'
has 'v0.0.1: images footer' "$TMP/notes-v0.0.1.md" 'docker pull rasonyang/ai-native-callcenter:v0.0.1'

# v0.1.0: an earlier tag exists but carries no contract.
notes v0.1.0
has 'v0.1.0: contract-less previous' "$TMP/notes-v0.1.0.md" 'No previous release to compare'
nothas 'v0.1.0: no changelog subsections' "$TMP/notes-v0.1.0.md" '### All changes'

# A pre-release compares with the previous tag of any kind — here the last rc.
notes v0.2.0-rc.2
has 'v0.2.0-rc.2: compares with previous tag' "$TMP/notes-v0.2.0-rc.2.md" 'compared with `v0.2.0-rc.1`'
nothas 'v0.2.0-rc.2: not with the previous full release' "$TMP/notes-v0.2.0-rc.2.md" 'compared with `v0.1.0`'
has 'v0.2.0-rc.2: breaking changes first' "$TMP/notes-v0.2.0-rc.2.md" '### Breaking changes'
has 'v0.2.0-rc.2: removed endpoint is breaking' "$TMP/notes-v0.2.0-rc.2.md" 'DELETE /calls'
has 'v0.2.0-rc.2: required parameter is breaking' "$TMP/notes-v0.2.0-rc.2.md" 'request parameter `state`'
has 'v0.2.0-rc.2: whole changelog follows' "$TMP/notes-v0.2.0-rc.2.md" '### All changes'

# A full release compares with the previous full release, so the delta covers
# the whole rc line: POST /queues arrived in rc.1 and v0.1.0 never had it.
notes v0.2.0
has 'v0.2.0: compares with previous full release' "$TMP/notes-v0.2.0.md" 'compared with `v0.1.0`'
nothas 'v0.2.0: not with the last rc' "$TMP/notes-v0.2.0.md" 'compared with `v0.2.0-rc.2`'
has 'v0.2.0: rc-era endpoint in the delta' "$TMP/notes-v0.2.0.md" 'POST /queues'
has 'v0.2.0: removed endpoint is breaking' "$TMP/notes-v0.2.0.md" 'DELETE /calls'
has 'v0.2.0: CHANGELOG body used' "$TMP/notes-v0.2.0.md" 'fixture changelog body for the full release'
has 'v0.2.0: API section present' "$TMP/notes-v0.2.0.md" '## API changes'
order=$(grep -n '^## ' "$TMP/notes-v0.2.0.md" | sed 's/[0-9]*://' | tr '\n' ' ')
eq 'v0.2.0: API changes before the images' "$order" '## API changes ## The images '

# The contract did not move between v0.2.0 and v0.3.0-rc.1.
notes v0.3.0-rc.1
has 'v0.3.0-rc.1: unchanged contract' "$TMP/notes-v0.3.0-rc.1.md" 'No API changes'
has 'v0.3.0-rc.1: compare base named' "$TMP/notes-v0.3.0-rc.1.md" 'compared with `v0.2.0`'
has 'v0.3.0-rc.1: commit-subject body used' "$TMP/notes-v0.3.0-rc.1.md" '## Changes since v0.2.0'
has 'v0.3.0-rc.1: commit listed' "$TMP/notes-v0.3.0-rc.1.md" 'unrelated change'
nothas 'v0.3.0-rc.1: no changelog subsections' "$TMP/notes-v0.3.0-rc.1.md" '### All changes'

printf '\n%d tests, %d failed\n' "$tests" "$failures"
[ "$failures" = 0 ]
