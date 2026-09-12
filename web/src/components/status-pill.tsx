import { useTranslation } from 'react-i18next'

import { AVAILABILITY_COLOR } from '@/lib/agent'
import type { Availability } from '@/lib/api'
import { cn } from '@/lib/utils'

/**
 * Colour appears only as a dot or a pill, never as a filled surface
 * (web/CLAUDE.md). The dot carries the state; the label names it.
 */
export function StatusDot({
  availability,
  color,
  className,
}: {
  availability?: Availability
  /**
   * A colour token for a dot that is not an agent's availability — a setup
   * step that is done, a phone that is connecting. The palette is the same
   * one; only the thing being coloured differs.
   */
  color?: string
  className?: string
}) {
  return (
    <span
      className={cn('inline-block size-2 shrink-0 rounded-full', className)}
      style={{
        backgroundColor: color ?? (availability ? AVAILABILITY_COLOR[availability] : undefined),
      }}
    />
  )
}

/** Dot plus label. A not-ready agent shows their reason, which is the useful part. */
export function StatusPill({
  availability,
  reason,
  className,
}: {
  availability: Availability
  reason?: string
  className?: string
}) {
  const { t } = useTranslation()
  const label =
    availability === 'NOT_READY' && reason
      ? t(`reasons.${reason}`)
      : t(`availability.${availability}`)

  return (
    <span
      className={cn('inline-flex items-center gap-1.5 whitespace-nowrap text-sm', className)}
      style={{ color: AVAILABILITY_COLOR[availability] }}
    >
      <StatusDot availability={availability} />
      {label}
    </span>
  )
}
