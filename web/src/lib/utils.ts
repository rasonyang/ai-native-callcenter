import { clsx, type ClassValue } from 'clsx'
import { twMerge } from 'tailwind-merge'

/** Merges conditional class names, letting later Tailwind utilities win. */
export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs))
}

/** Formats a date for the active locale via Intl, never a hand-rolled format. */
export function formatDateTime(value: string | Date, locale: string): string {
  const date = typeof value === 'string' ? new Date(value) : value
  return new Intl.DateTimeFormat(locale, {
    dateStyle: 'medium',
    timeStyle: 'medium',
  }).format(date)
}

/**
 * Formats a duration in seconds as m:ss / h:mm:ss for timers and KPIs.
 *
 * `padMinutes` keeps the minutes two digits under the hour (`00:07`). A clock
 * that ticks live beside other elements needs a constant width or the layout
 * jumps as it crosses a minute; a duration read once in a table does not, and
 * reads better unpadded.
 */
export function formatDuration(totalSec: number, options?: { padMinutes?: boolean }): string {
  const sec = Math.max(0, Math.floor(totalSec))
  const hours = Math.floor(sec / 3600)
  const minutes = Math.floor((sec % 3600) / 60)
  const seconds = sec % 60
  const mm = String(minutes).padStart(hours > 0 || options?.padMinutes ? 2 : 1, '0')
  const ss = String(seconds).padStart(2, '0')
  return hours > 0 ? `${hours}:${mm}:${ss}` : `${mm}:${ss}`
}
