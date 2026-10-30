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

/** The breadcrumb's last segment shows all of its text. */
function expectBreadcrumbWhole(ctx: LayoutContext) {
  const last = document.querySelector('header')!.children[0].lastElementChild!.lastElementChild!
  if (last.scrollWidth > last.clientWidth + 1) {
    throw new LayoutViolation(
      `${ctx.screen} @ ${ctx.viewport.width} [${ctx.lang}]: the breadcrumb "${last.textContent}" is cut `
      + `(${last.clientWidth}px of ${last.scrollWidth}px) while the call field could have given way`,
    )
  }
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
