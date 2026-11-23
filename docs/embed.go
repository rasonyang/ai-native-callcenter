// SPDX-License-Identifier: Apache-2.0

// Package docs embeds the documents the deployment or the CLI must carry
// inside the binary: the API contract it serves, and the agent skill
// `aicc skill` prints.
//
// docs/openapi.json is the single source of truth for this HTTP API, and a
// deployment that cannot hand out its own description asks every integrator
// to trust a copy they found somewhere else — one that may describe a
// different version of the software they are actually talking to.
//
// The files are embedded rather than read from disk for the same reason the
// SPA is (see web/embed.go): go:embed cannot reach across directories with
// "..", and a missing file is then a compile error rather than a 404
// discovered in production. There is no build step and nothing to deploy
// alongside the binary.
package docs

import _ "embed"

// Contract is docs/openapi.json, byte for byte.
//
//go:embed openapi.json
var Contract []byte

// Skill is docs/SKILL.md, byte for byte: the agent skill `aicc skill` prints.
// It lives in the binary so the skill a coding agent registers always matches
// the release that printed it.
//
//go:embed SKILL.md
var Skill []byte
