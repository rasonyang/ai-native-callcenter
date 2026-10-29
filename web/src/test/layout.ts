import { page } from '@vitest/browser/context'
import { act } from '@testing-library/react'
import { afterEach, it } from 'vitest'

import type { KnownIssue } from './layout-known-issues'

import i18n from '@/lib/i18n'

/**
 * Layout assertions for the browser tests (`*.layout.test.tsx`).
 *
 * Every check is relational: one box against the box that is meant to contain
 * it. No pixel values and no screenshots, so a change to a padding or a font
 * size moves nothing, while a cell that sizes its table past its card fails at
 * any viewport. Each failure names the screen, the viewport, the language and
 * the element with the measured sizes, so a CI log alone explains it.
 */

export const VIEWPORTS = [
  { width: 1024, height: 768 },
  { width: 1280, height: 800 },
  { width: 1920, height: 1080 },
] as const

export const LANGUAGES = ['en', 'zh'] as const

export interface LayoutContext {
  /** The screen under test, as a failure message names it. */
  screen: string
  viewport: { width: number; height: number }
  lang: (typeof LANGUAGES)[number]
}

type KnownIssues = KnownIssue | KnownIssue[]

/**
 * What `expectLayout` and `expectDialogFits` throw when the screen overflows.
 * A known issue (see `layoutTest`) is satisfied only by this error, so a broken
 * stub or a timed-out query cannot pass as an overflow that is already tracked.
 */
export class LayoutViolation extends Error {
  override name = 'LayoutViolation'
}

/** Half a pixel of subpixel rounding is not an overflow. */
const SLOP = 1

const label = (ctx: LayoutContext) =>
  `${ctx.screen} @ ${ctx.viewport.width}x${ctx.viewport.height} [${ctx.lang}]`

/** An element, as a failure names it: tag, a stretch of its text, its classes. */
function describeEl(el: Element): string {
  if (el === document.documentElement) return '<html> (the document)'
  const text = (el.textContent ?? '').replace(/\s+/g, ' ').trim()
  const cut = text.length > 60 ? `${text.slice(0, 60)}...(${text.length} chars)` : text
  const cls = (el.getAttribute('class') ?? '').split(/\s+/).slice(0, 4).join(' ')
  return `<${el.tagName.toLowerCase()}${cls ? ` class="${cls}..."` : ''}> "${cut}"`
}

/** The three cells that take the most room, which is who widened a table. */
function widestCells(table: Element): string {
  return [...table.querySelectorAll('td, th')]
    .filter((cell) => cell.getAttribute('colspan') === null)
    .toSorted((a, b) => b.getBoundingClientRect().width - a.getBoundingClientRect().width)
    .slice(0, 3)
    .map((cell) => `${describeEl(cell)} = ${px(cell.getBoundingClientRect().width)}`)
    .join('; ')
}

const px = (n: number) => `${Math.round(n * 10) / 10}px`

/** Whether the element scrolls its own overflow, so content inside it is contained. */
function scrollsX(el: Element): boolean {
  const overflow = getComputedStyle(el).overflowX
  return overflow === 'auto' || overflow === 'scroll'
}

function isVisible(el: Element): boolean {
  const rect = el.getBoundingClientRect()
  return rect.width > 0 && rect.height > 0 && getComputedStyle(el).visibility !== 'hidden'
}

/**
 * Registers one test per viewport and language (six). Each runs `body` with
 * the viewport set and the interface language changed, after which `body`
 * renders its screen and calls `expectLayout`. The viewport is the browser
 * window, so it is also the shell's: the sidebar keeps its 220px and the page
 * gets what is left, as in production.
 */
export function layoutTest(
  screen: string,
  body: (ctx: LayoutContext) => Promise<void>,
  options: {
    /**
     * A defect found by this test and not yet fixed: an entry of the registry
     * in `layout-known-issues.ts` (or a function choosing one per combination,
     * for a group that fails for two reasons). A combination it covers passes
     * only if `body` fails with a `LayoutViolation`, which is what
     * `expectLayout` and `expectDialogFits` throw. Any other error (a broken
     * stub, a query that timed out) still fails the test, so a known issue
     * cannot hide a test that no longer renders its screen. If `body` passes,
     * the test fails: the issue no longer reproduces, so delete the option.
     * Never use it to relax an assertion.
     * VITE_LAYOUT_STRICT=1 npm run test:browser treats every known issue as a
     * failure, to read what each one measures.
     */
    knownIssue?: KnownIssues | ((ctx: LayoutContext) => KnownIssues | undefined)
    /** Limits `knownIssue` to the combinations that overflow today; default all. */
    knownIssueWhen?: (ctx: LayoutContext) => boolean
    timeoutMs?: number
  } = {},
) {
  const { knownIssue, knownIssueWhen, timeoutMs = 30_000 } = options
  for (const viewport of VIEWPORTS) {
    for (const lang of LANGUAGES) {
      const ctx: LayoutContext = { screen, viewport, lang }
      const known = import.meta.env.VITE_LAYOUT_STRICT || !(knownIssueWhen?.(ctx) ?? true)
        ? undefined
        : typeof knownIssue === 'function' ? knownIssue(ctx) : knownIssue
      it(`${screen} ${viewport.width}x${viewport.height} ${lang}`, async () => {
        await page.viewport(viewport.width, viewport.height)
        await act(async () => {
          await i18n.changeLanguage(lang)
        })
        if (!known) {
          await body(ctx)
          return
        }
        try {
          await body(ctx)
        } catch (error) {
          if (error instanceof LayoutViolation) return
          throw error
        }
        throw new Error(
          `${label(ctx)}: the known issue ${[known].flat().map((k) => `"${k.reason}" (#${k.issue})`).join(' and ')} `
          + `no longer reproduces; delete the knownIssue option of this group.`,
        )
      }, timeoutMs)
    }
  }
}

/** Call once per file that uses `layoutTest`: puts the language back after each. */
export function restoreLanguage() {
  afterEach(async () => {
    await act(async () => {
      await i18n.changeLanguage('en')
    })
  })
}

/**
 * Waits until text metrics are final: fonts loaded, and two frames with no
 * change in the document's size. Measuring before the Inter face arrives would
 * compare a fallback font's widths with production's.
 */
export async function settle() {
  await document.fonts.ready
  let last = ''
  for (let i = 0; i < 20; i++) {
    // Sequential on purpose: each frame is compared with the one before it.
    // oxlint-disable-next-line no-await-in-loop
    await new Promise((resolve) => requestAnimationFrame(() => resolve(null)))
    const now = `${document.documentElement.scrollWidth}x${document.documentElement.scrollHeight}`
    if (now === last) return
    last = now
  }
}

export interface LayoutOptions {
  /** Limits the table checks to one subtree; default is the whole document. */
  within?: ParentNode
}

/**
 * Collects every layout violation on screen and fails once with all of them.
 *
 * 1. Nothing scrolls the page sideways: the document and every `main` (the
 *    shell's own scroll container, which is where a too-wide page actually
 *    scrolls, since the shell is exactly viewport-high).
 * 2. Each table is no wider than the card (its scroll wrapper) around it, and
 *    the card lies inside the page's content box.
 * 3. Each button or link in a row lies inside the card and inside the window.
 * 4. A row made of one full-width cell keeps its content inside that cell, and
 *    anything in it that scrolls does so inside its own box.
 * 5. No cell's content spills out of the cell.
 */
export async function expectLayout(ctx: LayoutContext, options: LayoutOptions = {}) {
  await settle()
  const root = options.within ?? document
  const problems: string[] = []
  const add = (rule: string, el: Element, detail: string) =>
    problems.push(`  [${rule}] ${describeEl(el)}: ${detail}`)

  // 1. No horizontal scroll.
  const scrollers: Element[] = [document.documentElement, ...document.querySelectorAll('main')]
  for (const el of scrollers) {
    if (el.scrollWidth > el.clientWidth + SLOP) {
      add('no-page-scroll', el,
        `scrolls sideways: scrollWidth ${el.scrollWidth}px > clientWidth ${el.clientWidth}px`)
    }
  }

  const main = document.querySelector('main')
  const mainRect = main?.getBoundingClientRect()

  for (const table of root.querySelectorAll('table')) {
    const card = table.parentElement
    if (!card) continue
    const tableRect = table.getBoundingClientRect()
    const cardRect = card.getBoundingClientRect()

    // 2. The table fits its card.
    if (tableRect.width > card.clientWidth + SLOP) {
      add('table-fits-card', table,
        `table is ${px(tableRect.width)} wide in a card ${px(card.clientWidth)} wide `
        + `(table scrollWidth ${table.scrollWidth}px); widest cells: ${widestCells(table)}`)
    }
    if (mainRect && (cardRect.right > mainRect.right + SLOP || cardRect.left < mainRect.left - SLOP)) {
      add('card-in-page', card,
        `card spans ${px(cardRect.left)}..${px(cardRect.right)} but the page content is `
        + `${px(mainRect.left)}..${px(mainRect.right)}`)
    }

    for (const row of table.querySelectorAll('tr')) {
      // 3. Actions stay reachable.
      for (const action of row.querySelectorAll('button, a[href], [role="button"]')) {
        if (!isVisible(action)) continue
        const rect = action.getBoundingClientRect()
        if (rect.right > cardRect.right + SLOP || rect.left < cardRect.left - SLOP) {
          add('action-in-card', action,
            `lies at ${px(rect.left)}..${px(rect.right)}, outside its card `
            + `${px(cardRect.left)}..${px(cardRect.right)}`)
        }
        if (rect.right > window.innerWidth + SLOP) {
          add('action-on-screen', action,
            `right edge ${px(rect.right)} is past the window's ${window.innerWidth}px`)
        }
      }

      // 4. A full-width detail row.
      const cells = [...row.children].filter((c) => c.tagName === 'TD')
      if (cells.length === 1 && Number(cells[0].getAttribute('colspan') ?? 1) > 1) {
        const cell = cells[0]
        const cellRect = cell.getBoundingClientRect()
        if (cellRect.right > cardRect.right + SLOP) {
          add('detail-in-card', cell,
            `detail cell ends at ${px(cellRect.right)}, past its card's ${px(cardRect.right)}`)
        }
        for (const inner of cell.querySelectorAll('*')) {
          if (!scrollsX(inner)) continue
          const rect = inner.getBoundingClientRect()
          if (rect.right > cardRect.right + SLOP) {
            add('detail-scrolls-inside', inner,
              `scroll box ends at ${px(rect.right)}, past its card's ${px(cardRect.right)}; `
              + `its content must scroll inside it, not size it`)
          }
        }
      }
    }

    // 5. A cell's content stays in the cell. A scroll box inside it is its
    // own business (4); only the cell's direct overflow counts here.
    // A cell that clips (`truncate`) or scrolls contains its own content by
    // design; only a cell with overflow: visible can spill into its neighbours.
    for (const cell of table.querySelectorAll('td, th')) {
      if (getComputedStyle(cell).overflowX !== 'visible') continue
      if (cell.scrollWidth > cell.clientWidth + SLOP) {
        add('cell-contains-content', cell,
          `content is ${cell.scrollWidth}px wide in a cell ${cell.clientWidth}px wide; `
          + `a value that cannot wrap must scroll inside its own box (overflow-auto), `
          + `or the cell needs a width bound and a way to break the value`)
      }
    }
  }

  if (problems.length > 0) {
    throw new LayoutViolation(`${label(ctx)}: ${problems.length} layout problem(s)\n${problems.join('\n')}`)
  }
}
