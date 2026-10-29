import openapiSource from '../../../docs/openapi.json?raw'

/**
 * Values a real operator or caller can put into a field, chosen to break
 * layouts: long, with no break opportunity, or in a script that wraps
 * differently. Deterministic: the same call returns the same string, so a
 * failure in CI reproduces on a laptop.
 *
 * Used by the layout tests (`*.layout.test.tsx`); each value is also what a
 * contract-valid request can carry, so a screen that cannot show it is wrong,
 * not the value.
 */

/** A small seeded generator (mulberry32): deterministic, no dependency. */
function rng(seed: number) {
  let a = seed >>> 0
  return () => {
    a = (a + 0x6d2b79f5) >>> 0
    let t = a
    t = Math.imul(t ^ (t >>> 15), t | 1)
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61)
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296
  }
}

const ALNUM = 'abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789'

function pick(next: () => number, alphabet: string): string {
  return alphabet[Math.floor(next() * alphabet.length)]
}

/** `length` ASCII letters and digits with no space or hyphen to break at. */
export function asciiRun(length = 6000, seed = 1): string {
  const next = rng(seed)
  let out = ''
  while (out.length < length) out += pick(next, ALNUM)
  return out
}

const SYLLABLES = ['bil', 'ling', 'ac', 'count', 'cred', 'it', 'ser', 'vice', 'ref', 'und', 'plan', 'out', 'age']

/**
 * `length` characters of pseudo-words separated by single spaces: as long as
 * the unbreakable runs, but a browser can wrap it. What a long but ordinary
 * description looks like; the control for the runs above.
 */
export function words(length = 6000, seed = 7): string {
  const next = rng(seed)
  let out = ''
  while (out.length < length) {
    const count = 1 + Math.floor(next() * 3)
    let word = ''
    for (let i = 0; i < count; i++) word += SYLLABLES[Math.floor(next() * SYLLABLES.length)]
    out += out === '' ? word : ` ${word}`
  }
  // Cut mid-word at worst, never leave a trailing space.
  return out.slice(0, length).trimEnd().padEnd(length, 'x')
}

/** `length` CJK ideographs with no punctuation and no space. */
export function chineseRun(length = 3000, seed = 2): string {
  const next = rng(seed)
  let out = ''
  for (let i = 0; i < length; i++) out += String.fromCodePoint(0x4e00 + Math.floor(next() * 0x2000))
  return out
}

/**
 * An http(s) URL of exactly `length` characters with a query string. Every
 * character is one a browser treats as unbreakable.
 */
export function longUrl(length = 2000, seed = 3): string {
  const head = 'https://crm.example-corp.test/api/v2/customers/lookup?'
  const next = rng(seed)
  let out = head
  let n = 0
  while (out.length < length) {
    const piece = `${n === 0 ? '' : '&'}q${n}=${asciiRun(24, Math.floor(next() * 1e6))}`
    out += piece
    n++
  }
  return out.slice(0, length)
}

/** `{ "n": { "n": ... "leaf" } }`, nested `depth` levels. */
export function nestedJson(depth = 30): unknown {
  let value: unknown = 'leaf-value-at-the-bottom'
  for (let i = 0; i < depth; i++) value = { [`level_${depth - i}`]: value }
  return value
}

/** A free-text note mixing Chinese, English and emoji, with real spaces. */
export function mixedNote(): string {
  return '客户反馈 billing 账单里有 two charges 重复扣费 😡 请在 24h 内 call back 并确认 refund 是否已到账 ✅，'
    + '否则升级给 supervisor 处理。Ref: ORD-' + '0'.repeat(12) + '10391 💳💳💳'
}

/** A digit string with no separators, as an account or card number might be. */
export function digits(length = 40, seed = 4): string {
  const next = rng(seed)
  let out = ''
  for (let i = 0; i < length; i++) out += pick(next, '0123456789')
  return out
}

/** A string of exactly `length` characters that cannot be broken. */
export function atLength(length: number, seed = 5): string {
  return asciiRun(length, seed)
}

interface Schema {
  properties?: Record<string, { maxLength?: number }>
}

const SCHEMAS = (JSON.parse(openapiSource) as { components: { schemas: Record<string, Schema> } })
  .components.schemas

/**
 * The longest value the contract accepts for `schema.property`, unbreakable:
 * `maxLengthOf('APIKeyWrite', 'name')` is 128 characters today and follows the
 * contract if it changes. Throws if the field has no `maxLength`, so a
 * test cannot silently fall back to a short value.
 */
export function maxLengthOf(schema: string, property: string, seed = 6): string {
  const limit = SCHEMAS[schema]?.properties?.[property]?.maxLength
  if (limit === undefined) throw new Error(`${schema}.${property} has no maxLength in docs/openapi.json`)
  return atLength(limit, seed)
}

/** One of each of the generic hostile values, by name, for a table-driven test. */
export function hostileStrings(): Record<string, string> {
  return {
    ascii6000: asciiRun(),
    chinese3000: chineseRun(),
    url2000: longUrl(),
    digits40: digits(),
    mixedNote: mixedNote(),
  }
}
