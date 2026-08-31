#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Regenerates every artifact derived from docs/openapi.json (the API contract):
#   internal/api/api.gen.go      Go types + chi ServerInterface (oapi-codegen)
#   web/src/generated/api.ts     TypeScript types (openapi-typescript)
#   internal/api/scopes.gen.go   Go scope constants        (scripts/gen-scopes.mjs)
#   web/src/generated/scopes.ts  TS scope union + labels   (scripts/gen-scopes.mjs)
#   internal/api/opsecurity.gen.go  per-operation security (scripts/gen-opsecurity.mjs)
#
# This script is the single generation entry point — `make api-generate` and
# `go generate ./internal/api` both land here, so the committed output is
# byte-identical no matter which door was used.
set -euo pipefail
cd "$(dirname "$0")/.."

go tool oapi-codegen -config oapi-codegen.yaml docs/openapi.json

# Every Go source file in this repo carries the SPDX header; oapi-codegen
# cannot emit one itself, so it is stamped here, above the generated marker.
tmp="$(mktemp)"
printf '// SPDX-License-Identifier: Apache-2.0\n\n' | cat - internal/api/api.gen.go > "$tmp"
mv "$tmp" internal/api/api.gen.go
gofmt -w internal/api/api.gen.go

mkdir -p web/src/generated
web/node_modules/.bin/openapi-typescript docs/openapi.json -o web/src/generated/api.ts

# The scope vocabulary. Neither generator above emits it — oapi-codegen and
# openapi-typescript both ignore `security` and the root `x-scopes` extension —
# so it is generated here from the same contract, into the same two directories
# `make api-check` already diffs.
node scripts/gen-scopes.mjs

# Each operation's own `security` block, as a table the router obeys. Without
# it the contract's authorization rules would be JSON nobody executes.
node scripts/gen-opsecurity.mjs

gofmt -w internal/api/scopes.gen.go internal/api/opsecurity.gen.go
