import { screen } from '@testing-library/react'
import { describe } from 'vitest'

import type { WebhookDelivery, WebhookSubscription } from '@/lib/webhooks'
import { renderApp } from '@/test/app'
import {
  asciiRun, chineseRun, digits, longUrl, maxLengthOf, mixedNote,
} from '@/test/hostile'
import { expectLayout, layoutTest, restoreLanguage } from '@/test/layout'

/**
 * Webhooks: subscriptions (name, URL, filter) and the selected one's recent
 * deliveries (last error). The name and the URL are bounded by the contract
 * (120 and 2048); the filter's DID list, the error text and the status are not.
 */

const at = '2026-10-20T09:12:00Z'
const uuid = (n: number) => `0199f3a2-7c41-7d22-9b1e-${String(n).padStart(12, '0')}`

function subscription(n: number, over: Partial<WebhookSubscription> = {}): WebhookSubscription {
  return {
    subscriptionId: uuid(n),
    name: `CRM sync ${n}`,
    url: `https://hooks.example-corp.test/aicc/${n}`,
    filter: {},
    isEnabled: true,
    hasAuthToken: true,
    createdAt: at,
    updatedAt: at,
    ...over,
  }
}

function delivery(n: number, over: Partial<WebhookDelivery> = {}): WebhookDelivery {
  return {
    deliveryId: uuid(1000 + n),
    callId: uuid(2000 + n),
    revision: 1,
    status: 'FAILED',
    attemptCount: 5,
    nextAttemptAt: at,
    lastStatusCode: 502,
    lastError: 'upstream returned 502 Bad Gateway',
    createdAt: at,
    ...over,
  }
}

const ordinarySubscriptions = [
  subscription(1, { filter: { callType: ['INBOUND'], did: ['95001'] } }),
  subscription(2, { name: 'Billing warehouse', isEnabled: false, hasAuthToken: false }),
  subscription(3, { name: '呼叫中心数据同步' }),
]

/** One hostile field per subscription, the rest ordinary, so a failure names its cause. */
const urlSubscriptions = [
  subscription(1, { url: longUrl(2048) }),
  subscription(2, { url: `https://${asciiRun(2000, 4)}.test/hook` }),
  subscription(3, { url: longUrl(2048, 8) }),
  subscription(4, { name: 'Ordinary' }),
]

const nameSubscriptions = [
  subscription(1, { name: maxLengthOf('WebhookSubscriptionWrite', 'name') }),
  subscription(2, { name: chineseRun(120) }),
  subscription(3, { name: mixedNote().slice(0, 120) }),
  subscription(4, { name: 'Ordinary' }),
]

const filterSubscriptions = [
  subscription(1, { filter: { callType: ['INBOUND', 'OUTBOUND'], did: [digits(40), digits(40, 9)], status: ['ANSWERED'] } }),
  subscription(2, { filter: { did: [asciiRun(3000, 6)] } }),
  subscription(3, { filter: { did: Array.from({ length: 30 }, (_, i) => digits(40, 50 + i)) } }),
  subscription(4, { name: 'Ordinary' }),
]

const hostileDeliveries = [
  delivery(1, { lastError: asciiRun(6000, 11) }),
  delivery(2, { lastError: chineseRun(3000, 12) }),
  delivery(3, { lastError: longUrl(2000, 13), status: 'PENDING', lastStatusCode: undefined }),
  delivery(4, { lastError: mixedNote(), status: 'DELIVERED' }),
  delivery(5, { revision: 2147483647, attemptCount: 2147483647, lastStatusCode: 599, lastError: '' }),
]

function renderWebhooks(subscriptions: WebhookSubscription[], deliveries: WebhookDelivery[]) {
  return renderApp('/admin/webhooks', {
    handle: ({ path, method }) => {
      if (method !== 'GET') return undefined
      if (path === '/webhook-subscriptions') return { items: subscriptions }
      if (/^\/webhook-subscriptions\/[^/]+\/deliveries/.test(path)) return { items: deliveries }
      return undefined
    },
  })
}

restoreLanguage()

describe('webhooks, ordinary data', () => {
  // The control: a screen that overflows with these is broken before any
  // hostile value arrives.
  layoutTest('webhooks (ordinary data)', async (ctx) => {
    const { user } = renderWebhooks(ordinarySubscriptions, [delivery(1), delivery(2, { status: 'DELIVERED', lastError: '' })])
    await user.click(await screen.findByText('Billing warehouse'))
    await screen.findAllByText('upstream returned 502 Bad Gateway')
    await expectLayout(ctx)
  })
})


describe('webhooks, URLs at the contract limit', () => {
  // The URL cell is `max-w-96 truncate`.
  layoutTest('webhooks (2048-character URLs)', async (ctx) => {
    renderWebhooks(urlSubscriptions, [])
    await screen.findByText('Ordinary')
    await expectLayout(ctx)
  })
})

describe('webhooks, names at the contract limit', () => {
  layoutTest('webhooks (120-character names)', async (ctx) => {
    renderWebhooks(nameSubscriptions, [])
    await screen.findByText('Ordinary')
    await expectLayout(ctx)
  })
})

describe('webhooks, filters with long values', () => {
  // Filter values are free strings (did, status), joined into one unbroken line.
  layoutTest('webhooks (long filters)', async (ctx) => {
    renderWebhooks(filterSubscriptions, [])
    await screen.findByText('Ordinary')
    await expectLayout(ctx)
  })
})

describe('webhooks, deliveries with hostile errors', () => {
  layoutTest('webhooks (hostile deliveries)', async (ctx) => {
    // An ordinary subscription: only the deliveries vary.
    const { user } = renderWebhooks([subscription(1, { name: 'Billing warehouse' })], hostileDeliveries)
    await user.click(await screen.findByText('Billing warehouse'))
    await screen.findAllByText('2147483647', { selector: 'td.tabular' })
    await expectLayout(ctx)
  })
})
