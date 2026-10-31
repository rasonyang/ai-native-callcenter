import type { ReactNode } from 'react'

import { pillClass } from '@/components/ui/badge'
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

/**
 * A cell that shows a value somebody typed. It owns the width bound, so no
 * page writes `max-w-* truncate` by hand again (#379, #384).
 *
 * Why not a plain `max-w-* truncate` on the cell: in table auto layout a
 * cell's min-content width is a floor for its column. `truncate` is
 * `white-space: nowrap`, so its min-content is the whole value, and a value
 * with no break opportunity floors the column at its full length however
 * small the bound is. A few such cells outgrow the card and push the row's
 * buttons out of it.
 *
 * The content sits in a one-column grid whose track is
 * `minmax(0, max-content)`. Under min-content sizing the track is 0, so the
 * cell never floors its column at any viewport. Under max-content sizing the
 * track is the value's natural width, capped by `maxWidth` on the wrapper, so
 * with room to spare an ordinary value shows in full. A `w-0` wrapper (as in
 * `DetailRow`) would zero max-content too and collapse the column even when
 * there is room. The track never grows past the cell, so a long value clips
 * or wraps inside whatever width the column was given.
 *
 * - `truncate` (default): one line, an ellipsis, the full value in `title`.
 *   For names, numbers, ids, URLs, errors and short notes.
 * - `wrap`: the whole value stays readable and wraps, breaking anywhere when
 *   it has no spaces. `overflow-wrap: anywhere` (`wrap-anywhere`) is what
 *   lowers min-content; `break-words` does not, so it would not help here.
 * - `pill`: a truncating value drawn as a `Badge`. `justify-self-start` sizes
 *   the pill to its text, which for a nowrap value means its min-content, so
 *   `max-w-full` is what keeps a long one inside the track.
 *
 * `maxWidth` is a full Tailwind class (`max-w-md`); size it for a real value
 * (a phone number is `max-w-40`, not 40 digits wide).
 *
 * `minWidth` is a content-limited floor, a literal `min-w-upto-N` class
 * (`src/index.css`): `min-width: calc-size(max-content, min(size, N * 4px))`.
 * Why not a fixed floor: when the table is narrower than its columns' natural
 * widths, Chromium gives each column its min-content plus a share of the rest
 * in proportion to (max-content - min-content). With a min of 0 every column
 * shrinks in proportion, so a short ordinary value (`support-zh`, a pill, a
 * name) is cut while a free-text column could have absorbed it; a fixed floor
 * reserves its width even for a '—' and overflows tables with no slack. The
 * content-limited floor is min(the value's own width, N): a short value has
 * min-content == max-content and never shrinks, an empty one reserves only its
 * own width, and a long one floors at N so only the difference shrinks. Pick N
 * as the width an ordinary value of that kind needs: a phone number or DID 32
 * (E.164 is at most 15 digits and a `+`), or 29 where the table is tight (a
 * +86 mobile is 14 characters, 114px), an IPv4 address 28, an extension 16,
 * a short name 24 to 32. N is the content width: the cell's own `px-4` is on
 * top. Free text (notes, messages, request bodies, URLs, errors) gets none and
 * absorbs the shrink. The floors of a table, plus its columns that cannot
 * shrink, must fit the 739px card at 1024, or the table overflows again (the
 * layout tests fail it). 739 is the card with a classic 15px scrollbar on
 * `main` (Windows, or a Mac that always shows scrollbars); the layout tests keep
 * that scrollbar on. Overlay scrollbars give 754.
 *
 * A tight table can pair two: `min-w-upto-24 xl:min-w-upto-36` keeps the
 * 1024 floor small and raises it where the card has room (1280 and up).
 *
 * A browser without `calc-size()` drops the declaration and the cell has no
 * floor: more truncation, never an overflow.
 */
export function CellText({
  children,
  maxWidth,
  minWidth,
  wrap = false,
  pill = false,
  title,
  className,
}: {
  children: ReactNode
  maxWidth: string
  /** A content-limited floor, `min-w-upto-N`, sized for a real value of this kind. See above. */
  minWidth?: string
  wrap?: boolean
  pill?: boolean
  /** The full value, for a truncated cell. Defaults to the children when they are text. */
  title?: string
  className?: string
}) {
  return (
    <div className={cn('grid grid-cols-[minmax(0,max-content)]', maxWidth, minWidth)}>
      <span
        title={wrap ? undefined : (title ?? (typeof children === 'string' ? children : undefined))}
        className={cn(
          wrap ? 'wrap-anywhere' : 'truncate',
          pill && !wrap && ['max-w-full justify-self-start leading-5', pillClass],
          className,
        )}
      >
        {children}
      </span>
    </div>
  )
}

/**
 * A full-width row under a record: an opened spec, a player, a form. It owns
 * the colSpan, the padding and a row that does not highlight on hover. The
 * wrapper is w-0 min-w-full, so it has no intrinsic width: whatever it holds
 * fills the card and scrolls inside its own box. Without that, a long
 * unbroken line sizes the table to itself and pushes the other columns, and
 * the row's buttons, off the right edge. The wrapper does not scroll itself
 * (it would clip a focus ring); the content brings its own overflow.
 */
export function DetailRow({ columns, children }: { columns: number; children: ReactNode }) {
  return (
    <Tr className="h-auto hover:bg-transparent">
      <Td colSpan={columns} className="pb-3">
        <div className="w-0 min-w-full">{children}</div>
      </Td>
    </Tr>
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
