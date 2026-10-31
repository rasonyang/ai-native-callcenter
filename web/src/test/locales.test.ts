import en from '@/locales/en/translation.json'
import zh from '@/locales/zh/translation.json'
import { describe, expect, it } from 'vitest'

/**
 * The reasons a call went unserved, in both languages.
 *
 * `t('cdr.missedReasons.' + reason)` cannot fail: react-i18next answers a key
 * it does not know with the key itself, so a missing entry renders as
 * "cdr.missedReasons.AGENTS_DID_NOT_ANSWER" on the report a supervisor opens
 * to find out why. That shipped — the reason has been decided since the ledger
 * was built and neither locale ever named it — and nothing failed.
 *
 * The list is written out rather than derived: the generated contract is types
 * only, erased before this test runs, so there is nothing at runtime to
 * enumerate. Its Go counterpart, TestEveryMissedReasonIsOneTheContractNames,
 * pins these same five to the assembler and the contract.
 */
const MISSED_REASONS = [
  'SHORT_ABANDONED',
  'ABANDONED_RINGING',
  'ABANDONED_WAITING',
  'AGENTS_DID_NOT_ANSWER',
  'NO_AVAILABLE_AGENT',
]

describe('the reason a call went unserved', () => {
  it.each([
    ['en', en],
    ['zh', zh],
  ])('is a sentence in %s, never a raw key', (_locale, bundle) => {
    const reasons = (bundle as { cdr: { missedReasons: Record<string, string> } }).cdr
      .missedReasons
    expect(Object.keys(reasons).toSorted()).toEqual(MISSED_REASONS.toSorted())
    for (const value of Object.values(reasons)) expect(value.trim()).not.toBe('')
  })
})

/**
 * The phone setup copy, in both languages.
 *
 * Every string an agent reads while their phone is not working is in this
 * group, and a missing one renders as "phone.chip.errors.WSS_LOST" on the one
 * screen whose whole job is telling them what to do about it. The chip's
 * labels are looked up by key from `phoneChipFor`, so nothing in the type
 * system notices when one is absent.
 */
const PHONE_CHIPS = [
  'setup', 'lost', 'connecting', 'ready', 'wrongAccount', 'displaced', 'overridden',
]

const PHONE_ERRORS = ['REGISTRATION_FAILED', 'WSS_LOST', 'MIC_UNAVAILABLE', 'MEDIA_FAILED']

/** Every key below `phone`, flattened, so neither language may omit one. */
function keysOf(value: unknown, prefix = ''): string[] {
  if (typeof value !== 'object' || value === null) return [prefix]
  return Object.entries(value as Record<string, unknown>).flatMap(([key, child]) =>
    keysOf(child, prefix ? `${prefix}.${key}` : key),
  )
}

/** The sentences themselves, so an empty one fails here rather than on screen. */
function sentencesOf(value: unknown): string[] {
  if (typeof value !== 'object' || value === null) return [String(value)]
  return Object.values(value as Record<string, unknown>).flatMap(sentencesOf)
}

describe('the phone setup copy', () => {
  it.each([
    ['en', en],
    ['zh', zh],
  ])('names every chip and every phone error in %s', (_locale, bundle) => {
    const phone = (bundle as { phone: { chip: Record<string, unknown> } }).phone
    expect(Object.keys(phone.chip).toSorted()).toEqual([...PHONE_CHIPS, 'errors'].toSorted())
    expect(Object.keys(phone.chip.errors as object).toSorted()).toEqual([...PHONE_ERRORS].toSorted())
    for (const sentence of sentencesOf(phone)) expect(sentence.trim()).not.toBe('')
  })

  it('says the same set of things in both languages', () => {
    expect(keysOf(en.phone).toSorted()).toEqual(keysOf(zh.phone).toSorted())
  })
})

/**
 * Every literal translation key the SPA uses, in both languages.
 *
 * react-i18next answers a key it does not know with the key itself, and the
 * app falls back from zh to en, so a missing entry renders as "agent.idle" on
 * the softphone bar and nothing fails (#396, #337). This reads the source,
 * collects each `t('…')`, `t("…")`, `t(`…`)` (no `${`), `i18n.t('…')` and
 * `<Trans i18nKey="…">`, and checks every one against each bundle directly, so
 * a zh gap is not hidden by the en fallback.
 *
 * Dynamic keys (`t(`reasons.${r}`)`, `t(variable)`) are out of reach of a
 * source scan; the per-group tests above cover those.
 */
const SOURCES = import.meta.glob(
  [
    '/src/**/*.{ts,tsx}',
    '!/src/**/*.test.{ts,tsx}',
    '!/src/test/**',
    '!/src/generated/**',
    '!/src/routeTree.gen.ts',
  ],
  { query: '?raw', import: 'default', eager: true },
) as Record<string, string>

const KEY_CALL = /(?<![\w$.])(?:i18n\.)?t\(\s*(?:'([^'\n]*)'|"([^"\n]*)"|`([^`$\n]*)`)\s*[,)]/g
const TRANS_KEY = /\bi18nKey=(?:"([^"\n]*)"|'([^'\n]*)'|\{`([^`$\n]*)`\})/g

type UsedKey = { key: string; where: string }

function usedKeys(): UsedKey[] {
  const found: UsedKey[] = []
  for (const [file, source] of Object.entries(SOURCES)) {
    for (const pattern of [KEY_CALL, TRANS_KEY]) {
      for (const match of source.matchAll(pattern)) {
        const key = match[1] ?? match[2] ?? match[3]
        const line = source.slice(0, match.index).split('\n').length
        found.push({ key, where: `${file.replace(/^\//, 'web/')}:${line}` })
      }
    }
  }
  return found
}

function lookup(bundle: unknown, key: string): unknown {
  let node = bundle
  for (const part of key.split('.')) {
    if (typeof node !== 'object' || node === null) return undefined
    node = (node as Record<string, unknown>)[part]
  }
  return node
}

function isText(value: unknown): boolean {
  return typeof value === 'string' && value.trim() !== ''
}

// A plural key resolves through its forms: English needs one and other, Chinese
// has only other. The bare key is accepted too when it is a string.
function resolves(bundle: unknown, key: string, forms: string[]): boolean {
  if (isText(lookup(bundle, key))) return true
  return forms.every((form) => isText(lookup(bundle, `${key}_${form}`)))
}

describe('every literal translation key the SPA uses', () => {
  const used = usedKeys()

  it('scans the whole source tree', () => {
    expect(Object.keys(SOURCES).length).toBeGreaterThan(75)
    expect(used.length).toBeGreaterThan(850)
  })

  it('is defined in both languages', () => {
    const missing: string[] = []
    for (const { key, where } of used) {
      if (!resolves(en, key, ['one', 'other'])) missing.push(`en  ${key}  (${where})`)
      if (!resolves(zh, key, ['other'])) missing.push(`zh  ${key}  (${where})`)
    }
    expect(missing, `keys with no entry:\n${missing.join('\n')}\n`).toEqual([])
  })
})
