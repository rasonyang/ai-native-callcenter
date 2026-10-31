import { screen, waitFor } from '@testing-library/react'
import { describe } from 'vitest'

import i18n from '@/lib/i18n'
import { renderApp } from '@/test/app'
import { CALLER, callFixture, presenceFixture } from '@/test/harness'
import { digits } from '@/test/hostile'
import {
  LayoutViolation, expectLayout, layoutTest, restoreLanguage, type LayoutContext,
} from '@/test/layout'

/**
 * The agent's top bar in its widest states. The other agent layout tests draw
 * an idle agent; this one puts the bar on a call (the transfer button appears),
 * on a ringing offer (the answer button appears) and beside a phone chip that
 * offers an action, and holds each to the window at every viewport. The page
 * behind it is the lightest agent screen, so what is measured is the bar.
 */

restoreLanguage()

/**
 * Nothing in the top bar was dropped to make it fit: every button and link in
 * it is on screen and still has a name a screen reader can read, whether that
 * is its text, its aria-label or its title.
 */
async function expectBarReachable(ctx: LayoutContext) {
  await expectLayout(ctx)
  const header = document.querySelector('header')!
  const problems: string[] = []
  for (const control of header.querySelectorAll('button, a[href]')) {
    const rect = control.getBoundingClientRect()
    const name = (control.getAttribute('aria-label') ?? control.textContent ?? '').trim()
    if (!name) problems.push(`a control has no accessible name: ${control.outerHTML.slice(0, 120)}`)
    if (rect.width === 0 || rect.right > window.innerWidth || rect.left < 0) {
      problems.push(`"${name}" lies at ${Math.round(rect.left)}..${Math.round(rect.right)} of ${window.innerWidth}px`)
    }
  }
  if (problems.length > 0) throw new LayoutViolation(`${ctx.screen}: ${problems.join('; ')}`)
}

/** An agent mid-call: the number is the customer's, as the carrier supplied it. */
function onCall(number: string) {
  const call = callFixture('TALKING')
  call.parties[0].number = number
  call.parties[1].otherNumber = number
  return { presence: presenceFixture({ state: 'READY', availability: 'ON_CALL' }), calls: [call] }
}

const render = (
  backend: Parameters<typeof renderApp>[1],
  extension?: NonNullable<Parameters<typeof renderApp>[2]>['extension'],
) => renderApp('/agent/contacts', { contacts: [], ...backend }, { role: 'AGENT', extension })

/** The number on the line, as the call field draws it. */
function numberOnTheLine(number: string): HTMLElement {
  const el = document.querySelector<HTMLElement>(`[data-softphone-bar] [title="${number}"]`)
  if (!el) throw new Error(`the call field does not show ${number}`)
  return el
}

/** The number is on screen with at least its first characters, whatever else gives way. */
function expectNumberVisible(ctx: LayoutContext, number: string) {
  const el = numberOnTheLine(number)
  if (el.clientWidth < 30) {
    throw new LayoutViolation(
      `${ctx.screen} @ ${ctx.viewport.width} [${ctx.lang}]: the number ${number} is ${el.clientWidth}px wide in the call field`,
    )
  }
}

/** The number is shown whole: one line, nothing clamped. */
function expectNumberWhole(ctx: LayoutContext, number: string) {
  const el = numberOnTheLine(number)
  if (el.scrollHeight > el.clientHeight + 1 || el.scrollWidth > el.clientWidth + 1) {
    throw new LayoutViolation(
      `${ctx.screen} @ ${ctx.viewport.width} [${ctx.lang}]: the number ${number} is clamped: `
      + `${el.clientWidth}x${el.clientHeight}px box, ${el.scrollWidth}x${el.scrollHeight}px content`,
    )
  }
}

/**
 * The breadcrumb's last segment shows all of its text. Precise on purpose:
 * Chrome lays out in 1/64px, and a box a few units narrower than its text
 * already swaps the last character for an ellipsis while scrollWidth and
 * clientWidth, rounded to whole pixels, still agree. So the box's fractional
 * width is compared with the text's own, measured with a Range, which is not
 * clipped by the box.
 */
function expectBreadcrumbWhole(ctx: LayoutContext) {
  const last = document.querySelector('header')!.children[0].lastElementChild!.lastElementChild!
  const range = document.createRange()
  range.selectNodeContents(last)
  const textWidth = range.getBoundingClientRect().width
  const boxWidth = last.getBoundingClientRect().width
  if (textWidth > boxWidth + 0.005) {
    throw new LayoutViolation(
      `${ctx.screen} @ ${ctx.viewport.width} [${ctx.lang}]: the breadcrumb "${last.textContent}" is cut `
      + `(${boxWidth.toFixed(3)}px of ${textWidth.toFixed(3)}px) while the call field could have given way`,
    )
  }
}

/** The call field is wider than the floor it shrinks to (`min-w-32`, 128px). */
function callFieldIsAboveItsFloor(): boolean {
  const field = document.querySelector<HTMLElement>('[data-softphone-bar] > span.min-w-32')!
  return field.getBoundingClientRect().width > 128 + 1
}

describe('the agent top bar on a call', () => {
  layoutTest('agent bar (the number is shown)', async (ctx) => {
    render(onCall('+8613800138000'))
    await screen.findByTitle(i18n.t('call.transfer'))
    await expectBarReachable(ctx)
    expectNumberVisible(ctx, '+8613800138000')
    // Where the header has room the number is not clamped at all.
    if (ctx.viewport.width === 1920 || (ctx.viewport.width === 1280 && ctx.lang === 'zh')) {
      expectNumberWhole(ctx, '+8613800138000')
    }
    if (ctx.viewport.width === 1280 && ctx.lang === 'en') expectBreadcrumbWhole(ctx)
  })

  layoutTest('agent bar (a 40-digit number is shown)', async (ctx) => {
    render(onCall(digits(40, 1)))
    await screen.findByTitle(i18n.t('call.transfer'))
    await expectBarReachable(ctx)
    expectNumberVisible(ctx, digits(40, 1))
  })

  // The call field gives way before the breadcrumb does.
  layoutTest('agent bar (idle, breadcrumb)', async (ctx) => {
    render({ presence: presenceFixture() })
    await screen.findByTitle(i18n.t('call.hangup'))
    await expectBarReachable(ctx)
    if (ctx.viewport.width === 1280 && ctx.lang === 'en') expectBreadcrumbWhole(ctx)
  })

  layoutTest('agent bar (ringing offer)', async (ctx) => {
    render({
      presence: presenceFixture(),
      calls: [callFixture('RINGING')],
    })
    await screen.findByTitle(i18n.t('call.answer'))
    await expectBarReachable(ctx)
  })

  layoutTest('agent bar (ringing offer, 40-digit number)', async (ctx) => {
    const call = callFixture('RINGING')
    call.parties[0].number = digits(40, 2)
    call.parties[1].otherNumber = digits(40, 2)
    render({ presence: presenceFixture(), calls: [call] })
    await screen.findByTitle(i18n.t('call.answer'))
    await expectBarReachable(ctx)
  })
})

describe('the agent top bar beside a phone chip with an action', () => {
  // The extension is signed in as somebody else's account: the chip says so
  // and offers Re-provision, the widest label and an extra button.
  layoutTest('agent bar (phone chip, re-provision)', async (ctx) => {
    render({ presence: presenceFixture() }, { state: { account: '1999' } })
    await screen.findByText(i18n.t('phone.reprovision'))
    await expectBarReachable(ctx)
  })

  layoutTest('agent bar (phone chip, re-provision, on a call)', async (ctx) => {
    render(onCall(CALLER), { state: { account: '1999' } })
    await screen.findByText(i18n.t('phone.reprovision'))
    await screen.findByTitle(i18n.t('call.transfer'))
    await expectBarReachable(ctx)
  })

  // No extension on this page at all: the chip is the way into setting it up.
  layoutTest('agent bar (phone chip, set up)', async (ctx) => {
    render({ presence: presenceFixture({ isDeviceRegistered: false }) }, { answersHello: false })
    await screen.findByText(i18n.t('phone.chip.setup'))
    await expectBarReachable(ctx)
  })
})

const signedOut = () => ({ presence: presenceFixture({ state: 'LOGGED_OUT', availability: 'LOGGED_OUT' }) })

describe('the signed-out agent top bar', () => {
  layoutTest('agent bar (signed out)', async (ctx) => {
    render(signedOut())
    await screen.findByText(i18n.t('agent.signInPrompt'))
    await expectBarReachable(ctx)
  })

  // The longest refusal copy the interface has, in the bar's own line.
  layoutTest('agent bar (signed out, long refusal)', async (ctx) => {
    const { user } = renderApp('/agent/contacts', {
      contacts: [],
      ...signedOut(),
      handle: ({ path }) =>
        path === '/agent/login'
          ? new Response(
            JSON.stringify({ error: { code: 'CAMPAIGN_FLOW_HAS_TRANSFER', message: 'refused' } }),
            { status: 409, headers: { 'content-type': 'application/json' } },
          )
          : undefined,
    }, { role: 'AGENT' })
    await user.click(await screen.findByRole('button', { name: i18n.t('agent.signIn') }))
    await waitFor(() => {
      if (screen.queryByText(i18n.t('agent.signInPrompt'))) throw new Error('the refusal is not shown yet')
    })
    await expectBarReachable(ctx)
  })
})

describe('the breadcrumb on the agent dashboard, on a call', () => {
  // The call field gives way first: wherever it is above its floor the
  // breadcrumb's last segment (the section name) is whole; at its floor the
  // breadcrumb may truncate.
  for (const number of ['18688886666', '+8613800138000']) {
    layoutTest(`agent dashboard breadcrumb (${number})`, async (ctx) => {
      renderApp('/agent', { contacts: [], ...onCall(number) }, { role: 'AGENT' })
      // The cockpit has its own transfer button; wait for the bar's.
      await waitFor(() => {
        if (!document.querySelector('header')?.querySelector(`[title="${i18n.t('call.transfer')}"]`)) {
          throw new Error('the bar is not on the call yet')
        }
      })
      await expectBarReachable(ctx)
      numberOnTheLine(number)
      if (callFieldIsAboveItsFloor()) expectBreadcrumbWhole(ctx)
    })
  }
})
