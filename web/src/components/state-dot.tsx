import { useTranslation } from 'react-i18next'

import { AVAILABILITY_COLOR } from '@/lib/agent'
import type { Availability } from '@/lib/api'
import { cn } from '@/lib/utils'

/**
 * The one place colour carries meaning: a small dot, never a filled surface.
 * A pulse marks the states that demand attention rather than merely describe.
 */
export function StateDot({
  availability,
  className,
}: {
  availability: Availability
  className?: string
}) {
  const pulses = availability === 'ON_CALL' || availability === 'DEVICE_UNREACHABLE'
  return (
    <span
      className={cn('inline-block size-2 shrink-0 rounded-full', pulses && 'animate-pulse', className)}
      style={{ backgroundColor: AVAILABILITY_COLOR[availability] }}
    />
  )
}

/** Dot plus label, for tables and the softphone bar. */
export function StatePill({
  availability,
  reason,
  className,
}: {
  availability: Availability
  reason?: string
  className?: string
}) {
  const { t } = useTranslation()
  // A not-ready agent's reason is more useful than the word "not ready".
  const label =
    availability === 'NOT_READY' && reason
      ? t(`reasons.${reason}`)
      : t(`availability.${availability}`)

  return (
    <span className={cn('inline-flex items-center gap-1.5 text-xs', className)}>
      <StateDot availability={availability} />
      {label}
    </span>
  )
}
