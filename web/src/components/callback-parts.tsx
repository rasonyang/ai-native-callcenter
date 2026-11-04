import { useTranslation } from 'react-i18next'

import type { Callback, CallbackStatus } from '@/lib/ledger'

/** The status choices of a callback list; the empty string is "all". */
export const CALLBACK_FILTERS: Array<CallbackStatus | ''> = ['', 'OPEN', 'CLAIMED', 'DONE', 'DISMISSED']

/** Rows per page on both callback screens. */
export const CALLBACK_PAGE_SIZE = 50

const STATUS_COLOR: Record<CallbackStatus, string> = {
  OPEN: 'var(--state-ringing)',
  CLAIMED: 'var(--state-oncall)',
  DONE: 'var(--state-available)',
  DISMISSED: 'var(--state-offline)',
}

/**
 * What happened the last time somebody rang from this row. A dial is not a
 * kept promise — no answer, busy, "call me later" all leave the callback
 * open — so the outcome sits under the status for the agent to read before
 * deciding, rather than closing the row for them.
 */
export function LastAttempt({ callback, format }: { callback: Callback; format: Intl.DateTimeFormat }) {
  const { t } = useTranslation()
  if (!callback.lastAttemptAt) return null
  const when = format.format(new Date(callback.lastAttemptAt))
  const outcome = callback.lastAttemptStatus
    ? t(`callbacks.attempt.${callback.lastAttemptStatus}`, { defaultValue: callback.lastAttemptStatus })
    : t('callbacks.attempt.IN_PROGRESS')
  return (
    <span className="mt-0.5 block text-xs text-muted-foreground">
      {t('callbacks.lastAttempt', { when, outcome })}
    </span>
  )
}

/** A status as a coloured dot and its word; `isMine` marks the viewer's own claim. */
export function StatusPill({ status, isMine = false }: { status: CallbackStatus; isMine?: boolean }) {
  const { t } = useTranslation()
  return (
    <span className="flex items-center gap-1.5 whitespace-nowrap text-xs">
      <span className="size-2 shrink-0 rounded-full" style={{ backgroundColor: STATUS_COLOR[status] }} />
      {t(`callbacks.statuses.${status}`)}
      {status === 'CLAIMED' && isMine && (
        <span className="text-muted-foreground">{t('callbacks.byYou')}</span>
      )}
    </span>
  )
}
