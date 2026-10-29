/**
 * The overflows the layout tests found in the product and that are not fixed
 * yet, one entry per follow-up issue. A `layoutTest` group that still overflows
 * today names its entry in the `knownIssue` option (layout.ts), so the issue
 * numbers live here and nowhere else. A group's combination then passes only
 * while the screen fails with a layout violation, and fails the day the screen
 * is fixed: that is the cue to delete the option, and the entry once no group
 * uses it.
 *
 * `issue` is the follow-up issue that tracks the fix.
 */

import type { LayoutContext } from './layout'

export interface KnownIssue {
  /** The follow-up issue that tracks the fix. */
  issue: number
  /** One line: what overflows and why. */
  reason: string
}

/**
 * The top bar and softphone bar do not shrink (the agent screens overflow the
 * window at 1024, and at 1280 in English), the breadcrumb is `shrink-0` so it
 * takes a long flow or campaign name whole, and a long name in the page `h1`.
 */
export const shellChrome: KnownIssue = {
  issue: 383,
  reason: 'the shell chrome (top bar, softphone bar, breadcrumb, page h1) does not shrink or wrap',
}

/**
 * A table cell holding free text or a number from a user, a customer or a flow
 * author has no width bound and nothing to break at, so a hostile value widens
 * the table.
 */
export const cellBounds: KnownIssue = {
  issue: 384,
  reason: 'a table cell with user-supplied text or digits has no width bound and no break',
}

/**
 * Non-table text that does not wrap: dialog titles, delete and revoke
 * popovers, the page description, the Persona and rules cards, and the facts,
 * userData, journey, transcript, recording and title of the call detail.
 */
export const textWrap: KnownIssue = {
  issue: 385,
  reason: 'text outside a table (dialog title, popover, description, card) does not wrap',
}

/**
 * Tables or cards that overflow at 1024 (or 1280) with ordinary, realistic
 * data: too many columns, or a fixed-width part, for the card at that width.
 */
export const narrowTables: KnownIssue = {
  issue: 386,
  reason: 'a table or card overflows at 1024 or 1280 with ordinary data',
}

/**
 * A `max-w-* truncate` cell whose bound the table's auto layout also treats as
 * the column's minimum width (`truncate` is nowrap), so the table cannot shrink
 * below it however narrow the window is (#379): My Calls and Callbacks notes,
 * webhook URLs and delivery errors, the audit Request column, lead notes.
 */
export const truncateFloor: KnownIssue = {
  issue: 379,
  reason: 'a max-w-* truncate cell is the column\'s minimum width, so the table cannot shrink below it',
}

/**
 * For a group that fails for several reasons at different combinations: every
 * entry whose predicate holds for the combination, or none.
 */
export function knownBy(
  ...rules: Array<[when: (ctx: LayoutContext) => boolean, issue: KnownIssue]>
): (ctx: LayoutContext) => KnownIssue[] | undefined {
  return (ctx) => {
    const hit = rules.filter(([when]) => when(ctx)).map(([, issue]) => issue)
    return hit.length > 0 ? hit : undefined
  }
}

/**
 * The agent top bar needs about 1305px in English and 1165px in Chinese (the
 * softphone bar is 678px and does not shrink), so the document scrolls
 * sideways at 1024, and at 1280 in English.
 */
export const agentBarOverflows = (ctx: LayoutContext) =>
  ctx.viewport.width === 1024 || (ctx.viewport.width === 1280 && ctx.lang === 'en')

export const at1024 = (ctx: LayoutContext) => ctx.viewport.width === 1024
export const below1920 = (ctx: LayoutContext) => ctx.viewport.width < 1920
