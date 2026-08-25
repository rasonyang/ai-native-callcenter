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
    expect(Object.keys(reasons).sort()).toEqual([...MISSED_REASONS].sort())
    for (const value of Object.values(reasons)) expect(value.trim()).not.toBe('')
  })

  // A word one language has and the other lacks is the same defect halfway.
  it('says the same set of things in both languages', () => {
    expect(Object.keys(en.cdr.missedReasons).sort()).toEqual(
      Object.keys(zh.cdr.missedReasons).sort(),
    )
  })
})
