// SPDX-License-Identifier: Apache-2.0
//
// Emits every bounded integer query parameter as a lookup table, so the
// running server asks the contract what a page may ask for instead of each
// handler or store remembering a number.
//
//   internal/api/pagebounds.gen.go
//
// Run through scripts/api-generate.sh, never directly.
//
// The generated wrapper binds parameters but never checks them against the
// declared minimum and maximum, so without this table a `limit` above the
// contract's bound sailed through to a store, which silently answered the
// default page — fewer rows for a larger limit (#622). Now the bound is
// stated once, here, and enforced in one middleware.
import { readFileSync, writeFileSync } from 'node:fs'

const CONTRACT = 'docs/openapi.json'
const GO_OUT = 'internal/api/pagebounds.gen.go'

const METHODS = ['get', 'put', 'post', 'delete', 'options', 'head', 'patch', 'trace']

const spec = JSON.parse(readFileSync(CONTRACT, 'utf8'))

function resolveRef(obj) {
  for (let o = obj, i = 0; o && o.$ref; i++) {
    if (i >= 16) throw new Error(`${CONTRACT}: $ref cycle at ${o.$ref}`)
    const m = /^#\/components\/(parameters|schemas)\/(.+)$/.exec(o.$ref)
    if (!m) throw new Error(`${CONTRACT}: unsupported $ref ${o.$ref}`)
    o = spec.components?.[m[1]]?.[m[2]]
  }
  return obj && obj.$ref ? undefined : obj
}

const rows = []
for (const [path, item] of Object.entries(spec.paths ?? {})) {
  for (const method of METHODS) {
    const op = item[method]
    if (!op) continue
    const where = `${method.toUpperCase()} ${path}`
    const params = []
    for (const raw of op.parameters ?? []) {
      const p = resolveRef(raw)
      if (!p || p.in !== 'query' || !p.name) continue
      const schema = resolveRef(p.schema) ?? {}
      const hasMin = schema.minimum !== undefined
      const hasMax = schema.maximum !== undefined
      if (!hasMin && !hasMax) continue
      if (schema.type !== 'integer') {
        throw new Error(`${CONTRACT}: ${where} parameter "${p.name}" bounds a non-integer schema — bounds are only understood for integers`)
      }
      for (const [k, v] of [['minimum', schema.minimum], ['maximum', schema.maximum]]) {
        if (v !== undefined && !Number.isInteger(v)) {
          throw new Error(`${CONTRACT}: ${where} parameter "${p.name}" ${k} ${v} is not an integer`)
        }
      }
      params.push({ name: p.name, min: schema.minimum, max: schema.maximum, hasMin, hasMax })
    }
    if (params.length) rows.push({ key: where, params })
  }
}
if (rows.length === 0) throw new Error(`${CONTRACT}: no bounded query parameters found`)
rows.sort((a, b) => (a.key < b.key ? -1 : a.key > b.key ? 1 : 0))
for (const r of rows) r.params.sort((a, b) => (a.name < b.name ? -1 : 1))

const q = (s) => JSON.stringify(s)

const go = `// SPDX-License-Identifier: Apache-2.0

// Code generated from ${CONTRACT} (bounded query parameters) by scripts/gen-pagebounds.mjs. DO NOT EDIT.

package api

// QueryBound is one query parameter's declared range: the contract's
// ${'`'}minimum${'`'} and ${'`'}maximum${'`'}. The Has* fields say which are declared —
// a bound of 0 is a real bound, not an absent one.
type QueryBound struct {
\tMin, Max       int
\tHasMin, HasMax bool
}

// QueryBoundsByRoute is every integer query parameter the contract bounds,
// keyed "METHOD /path" then parameter name — the same route key
// OperationSecurityByRoute uses. A route absent from it bounds nothing.
var QueryBoundsByRoute = map[string]map[string]QueryBound{
${rows.map((r) => `\t${q(r.key)}: {
${r.params.map((p) => `\t\t${q(p.name)}: {Min: ${p.min ?? 0}, Max: ${p.max ?? 0}, HasMin: ${p.hasMin}, HasMax: ${p.hasMax}},`).join('\n')}
\t},`).join('\n')}
}
`

writeFileSync(GO_OUT, go)
process.stderr.write(`gen-pagebounds: ${rows.length} operations -> ${GO_OUT}\n`)
