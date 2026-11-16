import { screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { ReactElement } from 'react'

import { renderWithProviders } from '@/test/harness'

import { Route } from './_app.admin.test-numbers'

const Page = Route.options.component as () => ReactElement

const json = (body: unknown, status = 200) =>
  new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })

interface Recorded {
  method: string
  path: string
  body: unknown
}

function stub(post: () => Response) {
  const requests: Recorded[] = []
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL, init: RequestInit = {}) => {
      const path = String(input).replace('/api/v1', '')
      const method = init.method ?? 'GET'
      requests.push({ method, path, body: typeof init.body === 'string' ? JSON.parse(init.body) : undefined })
      if (method === 'POST') return post()
      if (method === 'DELETE') return new Response(null, { status: 204 })
      return json({
        items: [{ number: '+8613800138000', note: 'QA desk phone', createdAt: '2026-10-20T09:12:00Z' }],
        total: 1,
      })
    }),
  )
  return requests
}

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('the list of test numbers', () => {
  it('lists the entries with their note and says what the list does', async () => {
    stub(() => json({}, 201))
    renderWithProviders(<Page />)
    expect(await screen.findByText('+8613800138000')).toBeTruthy()
    expect(screen.getByText('QA desk phone')).toBeTruthy()
    expect(screen.getByText(/do-not-call list still applies/)).toBeTruthy()
  })

  it('sends the number as typed with the note, and leaves the note out when empty', async () => {
    const requests = stub(() =>
      json({ number: '+8613800138001', createdAt: '2026-10-20T09:12:00Z' }, 201),
    )
    const { user } = renderWithProviders(<Page />)
    await screen.findByText('+8613800138000')
    await user.type(screen.getByLabelText('Number to add'), '0086 13800138001')
    await user.click(screen.getByRole('button', { name: 'Add' }))
    await screen.findByText('+8613800138001 is on the list.')
    expect(requests.find((r) => r.method === 'POST')?.body).toEqual({ number: '0086 13800138001' })

    await user.type(screen.getByLabelText('Number to add'), '13800138002')
    await user.type(screen.getByLabelText('Note'), ' QA lab ')
    await user.click(screen.getByRole('button', { name: 'Add' }))
    await waitFor(() => expect(requests.filter((r) => r.method === 'POST')).toHaveLength(2))
    expect(requests.filter((r) => r.method === 'POST')[1].body).toEqual({
      number: '13800138002',
      note: 'QA lab',
    })
  })

  it('says the list is full when the cap refuses an entry', async () => {
    stub(() =>
      json({ error: { code: 'TEST_NUMBER_LIMIT_REACHED', message: 'full', params: { limit: 100 } } }, 409),
    )
    const { user } = renderWithProviders(<Page />)
    await screen.findByText('+8613800138000')
    await user.type(screen.getByLabelText('Number to add'), '13800138003')
    await user.click(screen.getByRole('button', { name: 'Add' }))
    expect((await screen.findByRole('alert')).textContent).toContain('full (100 numbers)')
  })
})
