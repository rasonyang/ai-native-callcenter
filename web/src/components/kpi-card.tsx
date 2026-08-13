import type { ReactNode } from 'react'

import { cn } from '@/lib/utils'

/**
 * A single number with its label above and context below: 12px secondary
 * label, 24px semibold value, 12px note (web/CLAUDE.md).
 */
export function KpiCard({
  label,
  value,
  note,
  tone,
}: {
  label: string
  value: ReactNode
  note?: ReactNode
  /** A token name such as --state-breach, for values that demand attention. */
  tone?: string
}) {
  return (
    <div className="rounded-md border bg-card p-4">
      <div className="text-xs text-muted-foreground">{label}</div>
      <div
        className={cn('tabular mt-1 text-2xl font-semibold')}
        style={tone ? { color: `var(${tone})` } : undefined}
      >
        {value}
      </div>
      {note !== undefined && <div className="mt-1 text-xs text-muted-foreground">{note}</div>}
    </div>
  )
}
