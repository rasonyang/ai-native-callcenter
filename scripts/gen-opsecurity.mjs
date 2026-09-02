// SPDX-License-Identifier: Apache-2.0
//
// Emits each operation's `security` block as a lookup table, so the running
// server asks the contract what a request needs instead of a hand-placed
// guard beside every route.
//
//   internal/api/opsecurity.gen.go
//
// Run through scripts/api-generate.sh, never directly.
//
// oapi-codegen emits nothing at all about security (`grep -i securit
// internal/api/api.gen.go` = 0), so without this the contract's authorization
// rules would exist only as JSON nobody executes — and "the contract is the
// product" would be an assertion rather than something the server obeys.
//
// The shape of an OpenAPI `security` value: the array is an OR of
// alternatives, each alternative an AND of schemes. This contract uses exactly
// two alternatives — a cookie one (with csrfHeader on mutating operations) and
// a bearer one — and a scheme a request does not satisfy simply does not open
// that alternative. A credential with no alternative of its own cannot reach
// the operation whatever scopes it holds; that asymmetry is P9's whitelist and
// has to survive into the table, which is why a flat scope list will not do.
import { readFileSync, writeFileSync } from 'node:fs'

const CONTRACT = 'docs/openapi.json'
const GO_OUT = 'internal/api/opsecurity.gen.go'

const COOKIE = 'cookieSession'
const CSRF = 'csrfHeader'
const BEARER = 'apiKeyBearer'
const METHODS = ['get', 'put', 'post', 'delete', 'options', 'head', 'patch', 'trace']

const spec = JSON.parse(readFileSync(CONTRACT, 'utf8'))
const schemes = spec.components?.securitySchemes ?? {}
for (const name of [COOKIE, CSRF, BEARER]) {
  if (!schemes[name]) {
    throw new Error(`${CONTRACT}: securityScheme "${name}" is gone — this generator encodes the contract's two credential kinds and must be revisited, not silently skipped`)
  }
}
if (spec.security !== undefined) {
  throw new Error(`${CONTRACT}: a root-level "security" is back. Every operation declares its own so a missing one is a miss, not a silent inheritance`)
}
const vocab = spec['x-scopes'] ?? {}

const rows = []
for (const [path, item] of Object.entries(spec.paths ?? {})) {
  for (const method of METHODS) {
    const op = item[method]
    if (!op) continue
    const where = `${method.toUpperCase()} ${path}`
    if (!op.operationId) throw new Error(`${CONTRACT}: ${where} has no operationId`)
    const sec = op.security
    if (sec === undefined) {
      throw new Error(`${CONTRACT}: ${where} (${op.operationId}) declares no security. There is no global default to fall back on — a missing declaration is a miss`)
    }

    let cookie = null
    let bearer = null
    let needsCSRF = false
    for (const alt of sec) {
      const names = Object.keys(alt)
      const unknown = names.filter((n) => n !== COOKIE && n !== CSRF && n !== BEARER)
      if (unknown.length) throw new Error(`${CONTRACT}: ${where} names unknown scheme(s) ${unknown.join(', ')}`)
      for (const n of names) {
        for (const s of alt[n]) {
          if (!(s in vocab)) throw new Error(`${CONTRACT}: ${where} requires scope "${s}", which is not in root x-scopes`)
        }
      }
      if (names.includes(COOKIE)) {
        if (cookie) throw new Error(`${CONTRACT}: ${where} has two cookie alternatives`)
        cookie = alt[COOKIE]
        needsCSRF = names.includes(CSRF)
      } else if (names.includes(BEARER)) {
        if (bearer) throw new Error(`${CONTRACT}: ${where} has two bearer alternatives`)
        bearer = alt[BEARER]
      } else {
        throw new Error(`${CONTRACT}: ${where} has an alternative naming only ${names.join(', ')}`)
      }
    }
    // The CSRF header protects a credential the browser sends by itself. It
    // therefore belongs to the cookie alternative and to mutating methods —
    // the contract says so operation by operation, and a drift between the
    // two is a contract bug, caught here rather than at runtime.
    const mutating = !['get', 'head', 'options', 'trace'].includes(method)
    if (cookie && needsCSRF !== mutating) {
      throw new Error(`${CONTRACT}: ${where} — csrfHeader ${needsCSRF ? 'is required on a safe method' : 'is missing from a mutating method'}`)
    }
    rows.push({
      key: where,
      operationId: op.operationId,
      anonymous: sec.length === 0,
      cookie,
      bearer,
      needsCSRF,
    })
  }
}
if (rows.length === 0) throw new Error(`${CONTRACT}: no operations found`)
rows.sort((a, b) => (a.key < b.key ? -1 : a.key > b.key ? 1 : 0))

const q = (s) => JSON.stringify(s)
const slice = (v) => (v === null ? 'nil' : `[]string{${v.map(q).join(', ')}}`)

const go = `// SPDX-License-Identifier: Apache-2.0

// Code generated from ${CONTRACT} (per-operation security) by scripts/gen-opsecurity.mjs. DO NOT EDIT.

package api

// OperationSecurity is one operation's ${'`'}security${'`'} block, as the contract
// states it: which credentials reach the operation, and what each must carry.
type OperationSecurity struct {
\t// OperationID names the operation in the contract.
\tOperationID string

\t// IsAnonymous is a contract ${'`'}security: []${'`'} — the operation takes no
\t// credential at all.
\tIsAnonymous bool

\t// SessionScopes and KeyScopes are what a browser session, respectively an
\t// API key, must hold to reach this operation.
\t//
\t// nil and empty are different answers. nil means that credential has no
\t// alternative here and cannot reach the operation whatever it holds — the
\t// contract's two deliberate asymmetries, POST /auth/logout (a key has no
\t// session to end) and POST /recordings/{recordingId}/reviews (a human
\t// judgement is attributed to a person). Empty means it reaches the
\t// operation while holding nothing in particular: authenticated is the
\t// whole requirement.
\tSessionScopes []string
\tKeyScopes     []string

\t// NeedsCSRF is the csrfHeader scheme on the session alternative. It rides
\t// with the cookie because a cookie travels by itself; a key is presented
\t// deliberately on every request and is never asked for it.
\tNeedsCSRF bool
}

// OperationSecurityByRoute is every operation the contract declares, keyed
// "METHOD /path" with the path exactly as the contract spells it — which is
// chi's route pattern with the server's /api/v1 prefix removed.
var OperationSecurityByRoute = map[string]OperationSecurity{
${rows.map((r) => `\t${q(r.key)}: {
\t\tOperationID:   ${q(r.operationId)},${r.anonymous ? '\n\t\tIsAnonymous:   true,' : ''}
\t\tSessionScopes: ${slice(r.cookie)},
\t\tKeyScopes:     ${slice(r.bearer)},${r.needsCSRF ? '\n\t\tNeedsCSRF:     true,' : ''}
\t},`).join('\n')}
}

// SecurityForRoute answers what the operation mounted at this method and path
// requires. The path is the contract's, so a caller holding chi's route
// pattern strips the server's own prefix first.
//
// ok is false when the route is not in the contract at all — which is a
// routing table that has drifted, not a request to let through.
func SecurityForRoute(method, path string) (OperationSecurity, bool) {
	sec, ok := OperationSecurityByRoute[method+" "+path]
	return sec, ok
}
`

writeFileSync(GO_OUT, go)
process.stderr.write(`gen-opsecurity: ${rows.length} operations -> ${GO_OUT}\n`)
