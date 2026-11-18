/**
 * Telephone numbers as the server writes them, mirrored from internal/phonenum
 * so that what this application writes out reads back as the same number.
 *
 * Every number the API returns for a campaign contact is canonical E.164: a
 * "+", a digit 1-9, then 7 to 14 more digits (phonenum.IsCanonical).
 */

/** Whether s is canonical E.164, as phonenum.IsCanonical decides it. */
export function isCanonical(s: string): boolean {
  return /^\+[1-9]\d{7,14}$/.test(s)
}

/** A Chinese mobile number: 1, then 3 to 9, then 9 digits (phonenum.isChineseMobile). */
function isChineseMobile(s: string): boolean {
  return /^1[3-9]\d{9}$/.test(s)
}

/**
 * A Chinese landline without its trunk 0: an area code that exists (10, or one
 * beginning with 2 to 9) and 9 to 11 digits in all (phonenum.isChineseLandline).
 */
function isChineseLandline(s: string): boolean {
  return /^(?:10|[2-9])\d+$/.test(s) && s.length >= 9 && s.length <= 11
}

/**
 * A canonical number written as it is dialled inside its country, without
 * "+" and the country code, when it is a number of `countryCode`: the national
 * form phonenum.Callback writes under the NATIONAL dial form. A Chinese
 * landline gets back the trunk 0 the canonical form drops ("+861062345678" is
 * "01062345678"); a mobile number, and a number of any other country code, is
 * the digits after the code. (phonenum.DialString's national form drops the
 * landline's 0, which is fine for a trunk and wrong for a person.)
 *
 * The import reads a number without "+" or "00" as national: it drops one
 * leading 0 and puts "+" and the campaign's country code in front
 * (phonenum.Canonical). Everything this returns reads back that way as the
 * number it came from. So a number stays as it is (E.164) when there is no
 * country code to strip, when it is of another country, when it is not
 * canonical, and when what follows the code starts with 0, which the import
 * would drop and so read as a different number.
 */
export function nationalNumber(e164: string, countryCode: string | undefined): string {
  if (!countryCode || !/^\d+$/.test(countryCode) || !isCanonical(e164)) return e164
  const prefix = `+${countryCode}`
  if (!e164.startsWith(prefix) || e164.length === prefix.length) return e164
  const significant = e164.slice(prefix.length)
  if (significant.startsWith('0')) return e164
  if (countryCode === '86' && !isChineseMobile(significant) && isChineseLandline(significant)) {
    return `0${significant}`
  }
  return significant
}
