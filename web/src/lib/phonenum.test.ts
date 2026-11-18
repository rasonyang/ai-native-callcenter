import { describe, expect, it } from 'vitest'

import { isCanonical, nationalNumber } from './phonenum'

/**
 * What the import makes of a value (phonenum.Canonical, rules 1 to 3): a
 * leading "+" stays, "00" becomes "+", anything else loses one leading 0 and
 * gets "+" and the country code. Kept here to check the round trip.
 */
function canonicalAsImported(raw: string, countryCode: string): string {
  const s = raw.replace(/[ \-.()]/g, '')
  if (s.startsWith('+')) return s
  if (s.startsWith('00')) return `+${s.slice(2)}`
  return `+${countryCode}${s.replace(/^0/, '')}`
}

describe('the national form of a number', () => {
  it('writes a mobile number of the default country code without + and the code', () => {
    expect(nationalNumber('+8613800138000', '86')).toBe('13800138000')
    expect(nationalNumber('+8619900000000', '86')).toBe('19900000000')
  })

  it('gives a Chinese landline back its trunk 0, so the leading 0 survives', () => {
    // Beijing (10), a two-digit area code and a three-digit one.
    expect(nationalNumber('+861060000000', '86')).toBe('01060000000')
    expect(nationalNumber('+862160000000', '86')).toBe('02160000000')
    expect(nationalNumber('+8675560000000', '86')).toBe('075560000000')
  })

  it('keeps a number of another country code in its international form', () => {
    expect(nationalNumber('+447700900123', '86')).toBe('+447700900123')
    expect(nationalNumber('+12025550123', '86')).toBe('+12025550123')
  })

  it('writes the digits after the code for a country without a landline rule', () => {
    expect(nationalNumber('+12025550123', '1')).toBe('2025550123')
    expect(nationalNumber('+447700900123', '44')).toBe('7700900123')
  })

  it('keeps E.164 without a country code, or for a value that is not canonical', () => {
    expect(nationalNumber('+8613800138000', undefined)).toBe('+8613800138000')
    expect(nationalNumber('+8613800138000', '')).toBe('+8613800138000')
    expect(nationalNumber('13800138000', '86')).toBe('13800138000')
    expect(nationalNumber('+86', '86')).toBe('+86')
  })

  it('keeps E.164 when the part after the code starts with 0, which the import would drop', () => {
    expect(nationalNumber('+86000000000', '86')).toBe('+86000000000')
  })

  it('writes a +86 number that is neither a mobile nor a landline as its digits', () => {
    expect(nationalNumber('+8610000000', '86')).toBe('10000000')
  })

  it('reads back through the import as the number it came from', () => {
    const numbers = [
      '+8613800138000', '+861060000000', '+862160000000', '+8675560000000', '+8610000000',
      '+447700900123', '+12025550123', '+86000000000',
    ]
    for (const e164 of numbers) {
      expect(isCanonical(e164)).toBe(true)
      expect(canonicalAsImported(nationalNumber(e164, '86'), '86')).toBe(e164)
    }
  })
})
