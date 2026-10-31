import { screen } from '@testing-library/react'
import { describe } from 'vitest'

import i18n from '@/lib/i18n'
import { renderApp } from '@/test/app'
import {
  CALL_ID, CALLER, callFixture, cdrFixture, contactFixture, openWrapUpFixture, presenceFixture,
} from '@/test/harness'
import { asciiRun, digits, mixedNote, words } from '@/test/hostile'
import {
  LayoutViolation, expectLayout, layoutTest, restoreLanguage, type LayoutContext,
} from '@/test/layout'

/**
 * The agent dashboard: three columns from 1280, two below it. The caller card
 * is the narrowest thing on it, so the cases put the longest values the
 * contract allows into it (a 200-character name, ten tags, a long company, a
 * long userData value) and hold the card's parts to the card and to each
 * other: the clock never covers the name row, nothing leaves the card, and
 * every wrap-up control stays in its card and on screen.
 */

restoreLanguage()

const SLOP = 1

const label = (ctx: LayoutContext) =>
  `${ctx.screen} @ ${ctx.viewport.width}x${ctx.viewport.height} [${ctx.lang}]`

const intersects = (a: DOMRect, b: DOMRect) =>
  a.left < b.right - SLOP && b.left < a.right - SLOP && a.top < b.bottom - SLOP && b.top < a.bottom - SLOP

const px = (n: number) => `${Math.round(n)}px`

function named(el: Element) {
  const text = (el.textContent ?? '').replace(/\s+/g, ' ').trim()
  return `<${el.tagName.toLowerCase()}> "${text.length > 40 ? `${text.slice(0, 40)}...` : text}"`
}

/** What the dashboard adds to `expectLayout`: the cards and their parts against each other. */
async function expectCockpit(ctx: LayoutContext) {
  // The page-level checks do not stop the dashboard's own, so one run names
  // every problem.
  const problems: string[] = []
  try {
    await expectLayout(ctx)
  } catch (error) {
    if (!(error instanceof LayoutViolation)) throw error
    problems.push(error.message)
  }
  const main = document.querySelector('main')!
  const mainRect = main.getBoundingClientRect()

  // Every card lies inside the page's content, and no two cards overlap.
  const cards = [...main.querySelectorAll('section')]
  for (const card of cards) {
    const rect = card.getBoundingClientRect()
    if (rect.left < mainRect.left - SLOP || rect.right > mainRect.right + SLOP) {
      problems.push(`card ${named(card)} spans ${px(rect.left)}..${px(rect.right)}, outside the page ${px(mainRect.left)}..${px(mainRect.right)}`)
    }
  }
  for (let i = 0; i < cards.length; i++) {
    for (let j = i + 1; j < cards.length; j++) {
      if (intersects(cards[i].getBoundingClientRect(), cards[j].getBoundingClientRect())) {
        problems.push(`cards ${named(cards[i])} and ${named(cards[j])} overlap`)
      }
    }
  }

  // The caller card.
  const caller = document.querySelector<HTMLElement>('[data-caller-card]')
  if (caller) {
    const cardRect = caller.getBoundingClientRect()
    for (const el of caller.querySelectorAll('*')) {
      const rect = el.getBoundingClientRect()
      if (rect.width === 0 && rect.height === 0) continue
      if (rect.right > cardRect.right + SLOP || rect.left < cardRect.left - SLOP) {
        problems.push(`${named(el)} in the caller card lies at ${px(rect.left)}..${px(rect.right)}, outside the card ${px(cardRect.left)}..${px(cardRect.right)}`)
      }
    }
    if (caller.scrollWidth > caller.clientWidth + SLOP) {
      problems.push(`the caller card scrolls sideways: ${caller.scrollWidth}px of ${caller.clientWidth}px`)
    }
    const clock = caller.querySelector('[data-caller-clock]')
    const nameRow = caller.querySelector('[data-caller-name-row]')
    if (clock && nameRow) {
      const clockRect = clock.getBoundingClientRect()
      for (const part of nameRow.children) {
        if (intersects(clockRect, part.getBoundingClientRect())) {
          problems.push(`the clock ${named(clock)} covers ${named(part)} of the name row`)
        }
      }
    }
    // Text that is cut shows its whole value on hover.
    for (const el of caller.querySelectorAll('.truncate')) {
      if (el.scrollWidth > el.clientWidth + SLOP && !el.getAttribute('title')) {
        problems.push(`${named(el)} is cut and has no title carrying the whole value`)
      }
    }
  }

  // The wrap-up controls lie in their card and in the window.
  const wrapUp = [...main.querySelectorAll('section')].find((s) =>
    s.querySelector('h2')?.textContent === i18n.t('agent.wrapUp'))
  if (wrapUp) {
    const cardRect = wrapUp.getBoundingClientRect()
    for (const control of wrapUp.querySelectorAll('select, textarea, button, [role="combobox"]')) {
      const rect = control.getBoundingClientRect()
      if (rect.width === 0) continue
      if (rect.left < cardRect.left - SLOP || rect.right > cardRect.right + SLOP) {
        problems.push(`wrap-up control ${named(control)} lies at ${px(rect.left)}..${px(rect.right)}, outside its card ${px(cardRect.left)}..${px(cardRect.right)}`)
      }
      if (rect.left < 0 || rect.right > window.innerWidth + SLOP) {
        problems.push(`wrap-up control ${named(control)} lies at ${px(rect.left)}..${px(rect.right)}, outside the window ${window.innerWidth}px`)
      }
    }
  }

  if (problems.length > 0) {
    throw new LayoutViolation(`${label(ctx)}: ${problems.length} dashboard problem(s)\n  ${problems.join('\n  ')}`)
  }
}

const TAGS = ['vip', 'company', 'renewal', 'complaint', 'priority', 'follow-up', 'gold', 'trial', 'partner', 'escalated']

/** The agent mid-call with `number`; the caller's leg and the agent's own carry it. */
function onCall(number: string, userData?: Record<string, unknown>) {
  const call = callFixture('TALKING')
  call.parties[0].number = number
  call.parties[1].otherNumber = number
  if (userData) call.userData = userData
  return { presence: presenceFixture({ state: 'READY', availability: 'ON_CALL' }), calls: [call] }
}

/** The call just ended, its record open and fileable. */
function wrappingUp(number: string) {
  return {
    presence: presenceFixture({ state: 'READY', availability: 'WRAP_UP', wrapUpCallId: CALL_ID }),
    calls: [],
    currentWrapUp: openWrapUpFixture(),
    myCDRs: [cdrFixture({ callId: CALL_ID, fromNumber: number })],
  }
}

const render = (backend: Parameters<typeof renderApp>[1]) =>
  renderApp('/agent', backend, { role: 'AGENT' })

const hostileContact = (overrides = {}) => contactFixture({
  name: asciiRun(200, 1),
  company: asciiRun(200, 2),
  tags: TAGS,
  notes: mixedNote(),
  ...overrides,
})

describe('the agent dashboard', () => {
  layoutTest('agent dashboard (idle)', async (ctx) => {
    render({ presence: presenceFixture() })
    await screen.findByText(i18n.t('agent.noContact'))
    await expectCockpit(ctx)
  })

  layoutTest('agent dashboard (a known contact on a call)', async (ctx) => {
    render({
      ...onCall(CALLER, { ticketId: 'ORD-10391', plan: 'Gold', region: 'East' }),
      contacts: [contactFixture({ name: 'Zhang Wei', company: 'NovaNet', tags: ['vip', 'company', 'renewal'] })],
    })
    await screen.findByText('Zhang Wei')
    await expectCockpit(ctx)
  })

  // The page scrolls below xl, so the wrap-up form must not sit under a tall
  // transcript: on an ordinary call its note field and Done button are on the
  // first screen (1024x768 measures the textarea at 659..723 and Done at
  // 731..763 of 768).
  layoutTest('agent dashboard (the wrap-up form is on the first screen during a call)', async (ctx) => {
    render({
      ...onCall(CALLER, { ticketId: 'ORD-10391' }),
      contacts: [contactFixture({ tags: ['vip', 'company'], notes: 'Prefers callbacks after 16:00.' })],
    })
    await screen.findAllByText('Zhang Wei')
    await expectCockpit(ctx)
    const problems: string[] = []
    const done = screen.getByRole('button', { name: i18n.t('agent.done') })
    for (const control of [screen.getByLabelText(i18n.t('agent.wrapUpNote')), done]) {
      const rect = control.getBoundingClientRect()
      if (rect.bottom > window.innerHeight + SLOP) {
        problems.push(`${named(control)} ends at ${px(rect.bottom)}, below the first screen's ${window.innerHeight}px`)
      }
    }
    if (problems.length > 0) throw new LayoutViolation(`${label(ctx)}: ${problems.join('; ')}`)
  })

  layoutTest('agent dashboard (an unknown 14-digit number on a call)', async (ctx) => {
    render({ ...onCall('+8613800138000'), contacts: [] })
    await screen.findAllByText('+8613800138000')
    await expectCockpit(ctx)
  })

  layoutTest('agent dashboard (wrap-up pending, contact known)', async (ctx) => {
    render({
      ...wrappingUp(CALLER),
      contacts: [contactFixture({ tags: ['vip', 'company'], lastCallAt: '2026-10-20T09:12:00Z' })],
    })
    await screen.findByLabelText(i18n.t('agent.wrapUpNote'))
    await screen.findByText('Zhang Wei')
    await expectCockpit(ctx)
  })
})

describe('the agent dashboard with hostile values', () => {
  layoutTest('agent dashboard (hostile contact on a call)', async (ctx) => {
    render({
      ...onCall(CALLER, {
        ticketId: asciiRun(200, 3), [asciiRun(60, 4)]: 'x', note: words(300, 5), account: digits(40, 6),
      }),
      contacts: [hostileContact()],
    })
    await screen.findByText(asciiRun(200, 1))
    await expectCockpit(ctx)
  })

  layoutTest('agent dashboard (breakable 200-character name and company)', async (ctx) => {
    render({
      ...onCall(CALLER),
      contacts: [hostileContact({ name: words(200, 8), company: words(200, 9) })],
    })
    await screen.findByText(words(200, 8))
    await expectCockpit(ctx)
  })

  layoutTest('agent dashboard (a 40-digit number on a call)', async (ctx) => {
    render({ ...onCall(digits(40, 1), { account: digits(40, 2) }), contacts: [] })
    await screen.findAllByText(digits(40, 1))
    await expectCockpit(ctx)
  })

  layoutTest('agent dashboard (hostile contact while wrapping up)', async (ctx) => {
    render({ ...wrappingUp(CALLER), contacts: [hostileContact()] })
    await screen.findByLabelText(i18n.t('agent.wrapUpNote'))
    await screen.findByText(asciiRun(200, 1))
    await expectCockpit(ctx)
  })

  layoutTest('agent dashboard (40-digit number while wrapping up)', async (ctx) => {
    render({ ...wrappingUp(digits(40, 1)), contacts: [] })
    await screen.findByLabelText(i18n.t('agent.wrapUpNote'))
    await expectCockpit(ctx)
  })
})
