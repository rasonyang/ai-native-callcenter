import { expect } from 'vitest'

import { LayoutViolation, type LayoutContext } from './layout'

/**
 * The dialog counterpart of `expectLayout`, for screens whose hostile value
 * also appears in a dialog (a title that names the record, a field that echoes
 * it). Relational as well: the dialog lies inside the window, and nothing
 * inside it reaches past its right edge, or holds more content than its box,
 * unless a clipping or scrolling ancestor inside the dialog contains it.
 */

const SLOP = 1

const px = (n: number) => `${Math.round(n * 10) / 10}px`

function clippedBetween(el: Element, dialog: Element): boolean {
  for (let node = el.parentElement; node && node !== dialog; node = node.parentElement) {
    if (getComputedStyle(node).overflowX !== 'visible') return true
  }
  return false
}

export function expectDialogFits(ctx: LayoutContext) {
  const dialog = document.querySelector('[role="dialog"]')
  const where = `${ctx.screen} dialog @ ${ctx.viewport.width}x${ctx.viewport.height} [${ctx.lang}]`
  expect(dialog, `${where}: no dialog is open`).not.toBeNull()
  if (!dialog) return
  const box = dialog.getBoundingClientRect()
  const problems: string[] = []
  if (box.left < -SLOP || box.right > window.innerWidth + SLOP) {
    problems.push(`  the dialog lies at ${px(box.left)}..${px(box.right)} in a ${window.innerWidth}px window`)
  }
  // Text that cannot wrap overflows its block without enlarging the block's
  // box, so the box check below cannot see it; scrollWidth can.
  for (const el of [dialog, ...dialog.querySelectorAll('*')]) {
    if (el.clientWidth === 0 || getComputedStyle(el).overflowX !== 'visible') continue
    if (el.scrollWidth > el.clientWidth + SLOP) {
      const text = (el.textContent ?? '').replace(/\s+/g, ' ').trim().slice(0, 50)
      problems.push(
        `  <${el.tagName.toLowerCase()}> "${text}" has ${el.scrollWidth}px of content in a box ${el.clientWidth}px wide`,
      )
    }
  }
  for (const el of dialog.querySelectorAll('*')) {
    const rect = el.getBoundingClientRect()
    if (rect.width === 0 || rect.height === 0) continue
    if (rect.right <= box.right + SLOP || clippedBetween(el, dialog)) continue
    const text = (el.textContent ?? '').replace(/\s+/g, ' ').trim().slice(0, 50)
    problems.push(
      `  <${el.tagName.toLowerCase()}> "${text}" ends at ${px(rect.right)}, past the dialog's ${px(box.right)}`,
    )
  }
  if (problems.length > 0) {
    throw new LayoutViolation(`${where}: ${problems.length} problem(s)\n${problems.join('\n')}`)
  }
}
