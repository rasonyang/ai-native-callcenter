// SPDX-License-Identifier: Apache-2.0
//
// Emits the scope vocabulary as code, from the one place it is defined:
// the root `x-scopes` object of docs/openapi.json.
//
//   internal/api/scopes.gen.go   Go constants + AllScopes + descriptions + IsScope
//   web/src/generated/scopes.ts  TS union type + SCOPES + SCOPE_DESCRIPTIONS
//
// Run through scripts/api-generate.sh, never directly — that script is the
// single generation entry point, and `make api-check` diffs both outputs.
//
// The descriptions ride along on purpose: the Admin UI's key-creation form
// labels each scope, and a form that hand-writes those labels is a second
// vocabulary waiting to drift from the contract.
import { readFileSync, writeFileSync, mkdirSync } from 'node:fs'

const CONTRACT = 'docs/openapi.json'
const GO_OUT = 'internal/api/scopes.gen.go'
const TS_OUT = 'web/src/generated/scopes.ts'

const spec = JSON.parse(readFileSync(CONTRACT, 'utf8'))
const vocab = spec['x-scopes']
if (!vocab || typeof vocab !== 'object' || Object.keys(vocab).length === 0) {
  throw new Error(`${CONTRACT}: root x-scopes is missing or empty — there is no vocabulary to generate from`)
}

// Sorted, so the output is byte-stable whatever order the contract lists them in.
const names = Object.keys(vocab).sort()

const NAME_RE = /^[a-z]+(:[a-z]+){1,2}$/
for (const name of names) {
  if (!NAME_RE.test(name)) {
    throw new Error(`${CONTRACT}: scope "${name}" is not 资源:动作[:范围] (lowercase, 2–3 colon-separated segments)`)
  }
  if (typeof vocab[name] !== 'string' || vocab[name].trim() === '') {
    throw new Error(`${CONTRACT}: scope "${name}" has no description`)
  }
}

// "calls:read:own" -> "ScopeCallsReadOwn"
const goIdent = (name) =>
  'Scope' + name.split(':').map((s) => s[0].toUpperCase() + s.slice(1)).join('')

const idents = new Map()
for (const name of names) {
  const id = goIdent(name)
  if (idents.has(id)) {
    throw new Error(`${CONTRACT}: scopes "${idents.get(id)}" and "${name}" both produce the Go identifier ${id}`)
  }
  idents.set(id, name)
}

const goQuote = (s) => JSON.stringify(s)
const width = Math.max(...names.map((n) => goIdent(n).length))

const go = `// SPDX-License-Identifier: Apache-2.0

// Code generated from ${CONTRACT} (root x-scopes) by scripts/gen-scopes.mjs. DO NOT EDIT.

package api

// The scope vocabulary. A scope is a capability, never a role: the names are
// ${'`'}resource:action[:range]${'`'} and no single one of them is a role's alias
// (docs/auth/TASKS.md §4 N1, docs/design/07-naming.md §7).
//
// The values are the wire strings. They are untyped string constants so they
// drop straight into the []string the contract's request bodies use.
const (
${names.map((n) => `\t// ${goIdent(n)} — ${vocab[n]}\n\t${goIdent(n).padEnd(width)} = ${goQuote(n)}`).join('\n\n')}
)

// AllScopes is the complete vocabulary, sorted by name.
var AllScopes = []string{
${names.map((n) => `\t${goIdent(n)},`).join('\n')}
}

// ScopeDescriptions is what each scope means, as the contract states it.
var ScopeDescriptions = map[string]string{
${names.map((n) => `\t${goIdent(n)}: ${goQuote(vocab[n])},`).join('\n')}
}

// IsScope reports whether name is in the vocabulary. An unknown name is
// refused rather than ignored: a key silently missing a capability fails
// later, somewhere else, for a reason nobody can see.
func IsScope(name string) bool {
\t_, ok := ScopeDescriptions[name]
\treturn ok
}
`

const ts = `/**
 * This file was auto-generated from ${CONTRACT} (root x-scopes)
 * by scripts/gen-scopes.mjs.
 * Do not make direct changes to the file.
 */

/** A capability an API key or a session may hold. */
export type Scope =
${names.map((n) => `    | '${n}'`).join('\n')}

/** The complete vocabulary, sorted by name. */
export const SCOPES: readonly Scope[] = [
${names.map((n) => `    '${n}',`).join('\n')}
] as const

/** What each scope means, as the contract states it. */
export const SCOPE_DESCRIPTIONS: Record<Scope, string> = {
${names.map((n) => `    '${n}': ${JSON.stringify(vocab[n])},`).join('\n')}
}
`

mkdirSync('web/src/generated', { recursive: true })
writeFileSync(GO_OUT, go)
writeFileSync(TS_OUT, ts)
process.stderr.write(`gen-scopes: ${names.length} scopes -> ${GO_OUT}, ${TS_OUT}\n`)
