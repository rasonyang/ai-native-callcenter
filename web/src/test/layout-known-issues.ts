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

export interface KnownIssue {
  /** The follow-up issue that tracks the fix. */
  issue: number
  /** One line: what overflows and why. */
  reason: string
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
