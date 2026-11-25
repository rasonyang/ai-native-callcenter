/**
 * Calendar days and wall-clock times in a named time zone (an IANA name such
 * as Asia/Shanghai), whatever zone the browser is in.
 *
 * The call ledger dates its calls in the deployment's zone (`CDRList.timeZone`)
 * and a day filter must select the same calls the export of that day holds, so
 * a day is turned into instants in that zone, never in the browser's. Built on
 * `Intl.DateTimeFormat`, which carries the zone rules, so a day across a
 * daylight-saving change is 23 or 25 hours long, as it is on the server.
 */

/** A calendar day, `YYYY-MM-DD`, as an `<input type="date">` holds it. */
export type CalendarDay = string

const DAY_MS = 86_400_000

const DAY_PATTERN = /^(\d{4})-(\d{2})-(\d{2})$/

/** The parts of a wall-clock reading. */
interface WallClock {
  year: number
  month: number
  day: number
  hour: number
  minute: number
  second: number
}

const formatters = new Map<string, Intl.DateTimeFormat>()

/** One formatter per zone: building one is the expensive part. Throws on an unknown zone. */
function formatterFor(timeZone: string): Intl.DateTimeFormat {
  let formatter = formatters.get(timeZone)
  if (!formatter) {
    formatter = new Intl.DateTimeFormat('en-US', {
      timeZone,
      year: 'numeric',
      month: '2-digit',
      day: '2-digit',
      hour: '2-digit',
      minute: '2-digit',
      second: '2-digit',
      hourCycle: 'h23',
    })
    formatters.set(timeZone, formatter)
  }
  return formatter
}

/** What a clock in the zone reads at the instant. */
function wallClockAt(instantMs: number, timeZone: string): WallClock {
  const parts: Record<string, number> = {}
  for (const part of formatterFor(timeZone).formatToParts(new Date(instantMs))) {
    if (part.type !== 'literal') parts[part.type] = Number(part.value)
  }
  return {
    year: parts.year,
    month: parts.month,
    day: parts.day,
    // Some engines still write midnight as 24 under hourCycle h23.
    hour: parts.hour % 24,
    minute: parts.minute,
    second: parts.second,
  }
}

/** How far the zone's clock is ahead of UTC at the instant, in milliseconds. */
function offsetAt(instantMs: number, timeZone: string): number {
  const wall = wallClockAt(instantMs, timeZone)
  const asUtc = Date.UTC(wall.year, wall.month - 1, wall.day, wall.hour, wall.minute, wall.second)
  return asUtc - (instantMs - (((instantMs % 1000) + 1000) % 1000))
}

const pad = (n: number, width = 2) => String(n).padStart(width, '0')

function dayOf(wall: Pick<WallClock, 'year' | 'month' | 'day'>): CalendarDay {
  return `${pad(wall.year, 4)}-${pad(wall.month)}-${pad(wall.day)}`
}

/** Whether the value is a well-formed calendar day that exists. */
export function isCalendarDay(value: string): value is CalendarDay {
  const match = DAY_PATTERN.exec(value)
  if (!match) return false
  const [, y, m, d] = match.map(Number)
  const date = new Date(Date.UTC(y, m - 1, d))
  return date.getUTCFullYear() === y && date.getUTCMonth() === m - 1 && date.getUTCDate() === d
}

/** The calendar day `days` after (or before, when negative) the given one. */
export function addDays(day: CalendarDay, days: number): CalendarDay {
  const [y, m, d] = day.split('-').map(Number)
  const date = new Date(Date.UTC(y, m - 1, d) + days * DAY_MS)
  return dayOf({ year: date.getUTCFullYear(), month: date.getUTCMonth() + 1, day: date.getUTCDate() })
}

/**
 * The first instant of the day in the zone: its midnight, or, where the zone
 * skips midnight (a daylight-saving change at 00:00), the moment the clock
 * jumps into the day.
 *
 * The wall time is read with each offset the zone has around that day; of the
 * instants that gives, the earliest whose own reading falls on the day is it.
 */
export function startOfDayIn(day: CalendarDay, timeZone: string): Date {
  const [y, m, d] = day.split('-').map(Number)
  const midnightAsUtc = Date.UTC(y, m - 1, d)
  const candidates = [-DAY_MS, 0, DAY_MS]
    .map((shift) => midnightAsUtc - offsetAt(midnightAsUtc + shift, timeZone))
    .filter((instant) => dayOf(wallClockAt(instant, timeZone)) === day)
    .toSorted((a, b) => a - b)
  return new Date(candidates[0] ?? midnightAsUtc - offsetAt(midnightAsUtc, timeZone))
}

/** An instant as RFC 3339 in UTC, without the milliseconds a whole second does not need. */
export function toRfc3339(instant: Date): string {
  return instant.toISOString().replace(/\.000Z$/, 'Z')
}

/**
 * The window of whole days `first`..`last` (both included) in the zone, as a
 * `from` (inclusive) and `to` (exclusive) pair of RFC 3339 instants: `from` is
 * the first day's start, `to` the start of the day after the last one. An
 * absent or malformed end leaves that side open.
 */
export function dayWindowIn(
  first: string | undefined,
  last: string | undefined,
  timeZone: string,
): { from?: string; to?: string } {
  return {
    from: first && isCalendarDay(first) ? toRfc3339(startOfDayIn(first, timeZone)) : undefined,
    to: last && isCalendarDay(last) ? toRfc3339(startOfDayIn(addDays(last, 1), timeZone)) : undefined,
  }
}

/**
 * An instant as the zone's clock reads it, `YYYY-MM-DD HH:MM:SS`: the year
 * always shows, and the same shape reads the same in every language. A zone
 * this browser does not know shows the instant in UTC with its `Z`, which is
 * still true.
 */
export function formatInZone(instant: string | Date, timeZone: string): string {
  const ms = new Date(instant).getTime()
  if (Number.isNaN(ms)) return String(instant)
  let wall: WallClock
  try {
    wall = wallClockAt(ms, timeZone)
  } catch {
    return toRfc3339(new Date(ms - (((ms % 1000) + 1000) % 1000)))
  }
  return `${dayOf(wall)} ${pad(wall.hour)}:${pad(wall.minute)}:${pad(wall.second)}`
}
