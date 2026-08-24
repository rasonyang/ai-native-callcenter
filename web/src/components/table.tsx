import type { ReactNode } from 'react'

import { cn } from '@/lib/utils'

/**
 * The one table in this product: 36px rows, 13px cells, 12px uppercase
 * headers, borders instead of shadows. Every list screen uses it as-is.
 */
export function DataTable({ children }: { children: ReactNode }) {
  return (
    <div className="overflow-x-auto rounded-md border bg-card">
      <table className="w-full border-collapse text-sm">{children}</table>
    </div>
  )
}

export function THead({ children }: { children: ReactNode }) {
  return (
    <thead>
      <tr className="border-b">{children}</tr>
    </thead>
  )
}

export function Th({
  children,
  align = 'left',
  className,
}: {
  children?: ReactNode
  align?: 'left' | 'right'
  className?: string
}) {
  return (
    <th
      className={cn(
        'h-9 px-4 text-xs font-medium uppercase tracking-wide text-muted-foreground',
        align === 'right' ? 'text-right' : 'text-left',
        className,
      )}
    >
      {children}
    </th>
  )
}

export function TBody({ children }: { children: ReactNode }) {
  return <tbody>{children}</tbody>
}

export function Tr({
  children,
  className,
  onClick,
}: {
  children: ReactNode
  className?: string
  /** Set only where the whole row is the link to a detail page. */
  onClick?: () => void
}) {
  return (
    <tr
      onClick={onClick}
      className={cn('h-9 border-b last:border-0 hover:bg-muted', className)}
    >
      {children}
    </tr>
  )
}

export function Td({
  children,
  align = 'left',
  className,
  colSpan,
}: {
  children?: ReactNode
  align?: 'left' | 'right'
  className?: string
  colSpan?: number
}) {
  return (
    <td colSpan={colSpan} className={cn('px-4', align === 'right' ? 'text-right' : 'text-left', className)}>
      {children}
    </td>
  )
}

/** A muted row for the empty, loading and failed states. */
export function TableMessage({ colSpan, children }: { colSpan: number; children: ReactNode }) {
  return (
    <tr>
      <td colSpan={colSpan} className="px-4 py-6 text-xs text-muted-foreground">
        {children}
      </td>
    </tr>
  )
}

/** The footer strip that carries counts and paging. */
export function TableFooter({ children }: { children: ReactNode }) {
  return (
    <div className="flex items-center justify-between border-t px-4 py-2 text-xs text-muted-foreground">
      {children}
    </div>
  )
}
