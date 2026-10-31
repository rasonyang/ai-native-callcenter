import type { ComponentProps } from 'react'

import { cn } from '@/lib/utils'

/** The pill's look, shared with a truncating table cell (`CellText pill`). */
export const pillClass = 'h-5 rounded-full border px-2 text-xs text-muted-foreground'

/**
 * A neutral outlined pill for short facts beside a heading or in a cell:
 * call type, queue name, a recorded marker. Colour never fills it — the
 * semantic palette belongs to call state alone (web/CLAUDE.md).
 */
export function Badge({ className, ...props }: ComponentProps<'span'>) {
  return (
    <span
      className={cn('inline-flex items-center whitespace-nowrap', pillClass, className)}
      {...props}
    />
  )
}
