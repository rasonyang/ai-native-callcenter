import { describe, expect, it } from 'vitest'

import { addDays, dayWindowIn, formatInZone, isCalendarDay, startOfDayIn } from './zoned-day'

/**
 * A calendar day in the deployment's zone, as instants. The server dates and
 * exports calls in that zone, so the day a filter selects must start at the
 * same instant there, whatever zone the browser runs in. The browser's own
 * zone is not involved in any of these: each case names its zone.
 */

const iso = (d: Date) => d.toISOString()

describe('the start of a calendar day in a zone', () => {
  it('is local midnight in a zone with no daylight saving', () => {
    expect(iso(startOfDayIn('2026-12-01', 'Asia/Shanghai'))).toBe('2026-11-30T16:00:00.000Z')
    expect(iso(startOfDayIn('2027-01-01', 'Asia/Shanghai'))).toBe('2026-12-31T16:00:00.000Z')
  })

  it('is midnight itself in UTC', () => {
    expect(iso(startOfDayIn('2026-12-01', 'UTC'))).toBe('2026-12-01T00:00:00.000Z')
    expect(iso(startOfDayIn('2028-02-29', 'UTC'))).toBe('2028-02-29T00:00:00.000Z')
  })

  it('follows the offset of each side of a daylight-saving change', () => {
    // 2026-03-08: Los Angeles moves from PST (-8) to PDT (-7) at 02:00.
    expect(iso(startOfDayIn('2026-03-08', 'America/Los_Angeles'))).toBe('2026-03-08T08:00:00.000Z')
    expect(iso(startOfDayIn('2026-03-09', 'America/Los_Angeles'))).toBe('2026-03-09T07:00:00.000Z')
    // 2026-11-01: back from PDT to PST at 02:00.
    expect(iso(startOfDayIn('2026-11-01', 'America/Los_Angeles'))).toBe('2026-11-01T07:00:00.000Z')
    expect(iso(startOfDayIn('2026-11-02', 'America/Los_Angeles'))).toBe('2026-11-02T08:00:00.000Z')
  })

  it('is the moment the clock jumps in when the zone skips midnight', () => {
    // Santiago springs forward at 00:00 (to 01:00) on 2026-09-06.
    const start = startOfDayIn('2026-09-06', 'America/Santiago')
    expect(formatInZone(start, 'America/Santiago')).toBe('2026-09-06 01:00:00')
    expect(formatInZone(new Date(start.getTime() - 1000), 'America/Santiago')).toBe('2026-09-05 23:59:59')
  })
})

/** How long a window of whole days in Los Angeles is, in hours. */
function hours(first: string, last: string): number {
  const { from, to } = dayWindowIn(first, last, 'America/Los_Angeles')
  return (Date.parse(to as string) - Date.parse(from as string)) / 3_600_000
}

describe('a window of whole days', () => {
  it('runs from the first day’s start to the start of the day after the last', () => {
    expect(dayWindowIn('2026-12-01', '2026-12-01', 'Asia/Shanghai')).toEqual({
      from: '2026-11-30T16:00:00Z',
      to: '2026-12-01T16:00:00Z',
    })
    expect(dayWindowIn('2026-12-01', '2026-12-31', 'Asia/Shanghai')).toEqual({
      from: '2026-11-30T16:00:00Z',
      to: '2026-12-31T16:00:00Z',
    })
  })

  it('is 23 and 25 hours long on the days the clocks change', () => {
    expect(hours('2026-03-08', '2026-03-08')).toBe(23)
    expect(hours('2026-11-01', '2026-11-01')).toBe(25)
    expect(hours('2026-11-02', '2026-11-02')).toBe(24)
  })

  it('leaves a side open when its day is absent or malformed', () => {
    expect(dayWindowIn('2026-12-01', undefined, 'UTC')).toEqual({ from: '2026-12-01T00:00:00Z', to: undefined })
    expect(dayWindowIn('', '2026-12-01', 'UTC')).toEqual({ from: undefined, to: '2026-12-02T00:00:00Z' })
    expect(dayWindowIn('2026-02-30', '12/01/2026', 'UTC')).toEqual({ from: undefined, to: undefined })
  })
})

describe('a calendar day', () => {
  it('is a real date in YYYY-MM-DD', () => {
    expect(isCalendarDay('2026-12-01')).toBe(true)
    expect(isCalendarDay('2028-02-29')).toBe(true)
    expect(isCalendarDay('2026-02-29')).toBe(false)
    expect(isCalendarDay('2026-1-1')).toBe(false)
    expect(isCalendarDay('')).toBe(false)
  })

  it('steps across month and year ends', () => {
    expect(addDays('2026-12-31', 1)).toBe('2027-01-01')
    expect(addDays('2026-03-01', -1)).toBe('2026-02-28')
  })
})

describe('an instant on the zone’s clock', () => {
  it('shows the zone’s date, with the year, and its time', () => {
    expect(formatInZone('2026-11-30T15:59:50Z', 'Asia/Shanghai')).toBe('2026-11-30 23:59:50')
    expect(formatInZone('2026-11-30T16:00:05Z', 'Asia/Shanghai')).toBe('2026-12-01 00:00:05')
    expect(formatInZone('2026-11-30T16:00:05Z', 'UTC')).toBe('2026-11-30 16:00:05')
    expect(formatInZone('2026-11-30T16:00:05Z', 'America/Los_Angeles')).toBe('2026-11-30 08:00:05')
  })

  it('falls back to UTC, marked as such, for a zone the browser does not know', () => {
    expect(formatInZone('2026-11-30T16:00:05.250Z', 'Not/AZone')).toBe('2026-11-30T16:00:05Z')
  })
})
