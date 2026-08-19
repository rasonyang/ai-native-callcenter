import { screen, waitFor, within } from '@testing-library/react'
import { afterEach, expect, it, vi } from 'vitest'
import type { ReactElement } from 'react'

import { Route } from '@/routes/_app.agent.contacts'
import { contactFixture, installBackend, renderWithProviders, type Backend } from '@/test/harness'

/**
 * The contact book. Small on purpose: who a number belongs to, and what the
 * last person who spoke to them wrote down.
 */

const Contacts = Route.options.component as () => ReactElement

afterEach(() => {
  vi.unstubAllGlobals()
})

async function renderContacts(backend: Partial<Backend> = {}) {
  const api = installBackend(backend)
  return { api, ...renderWithProviders(<Contacts />) }
}

it('lists contacts with their tags and when they last called', async () => {
  await renderContacts({
    contacts: [
      contactFixture({ lastCallAt: new Date('2026-08-18T09:12:00Z').toISOString() }),
      contactFixture({
        id: '00000000-0000-4000-8000-0000000000e2',
        phoneNumber: '+14085550166',
        name: 'Emily Carter',
        company: 'Harbor Foods',
        tags: [],
        notes: '',
      }),
    ],
  })

  expect(await screen.findByText('Zhang Wei')).toBeInTheDocument()
  const table = screen.getByRole('table')
  expect(within(table).getByText('VIP')).toBeInTheDocument()
  expect(within(table).getByText('Emily Carter')).toBeInTheDocument()
  // A number nobody has called yet says so rather than showing a blank cell.
  expect(within(table).getByText(/never/i)).toBeInTheDocument()
})

it('searches by whatever the agent remembers', async () => {
  const { api, user } = await renderContacts({ contacts: [contactFixture()] })

  await user.type(await screen.findByLabelText(/search/i), 'novanet')
  await waitFor(() => expect(api.requests.some((r) => r.path.includes('q=novanet'))).toBe(true))
})

it('creates a contact, splitting the tags the agent typed on one line', async () => {
  const { api, user } = await renderContacts({ contacts: [] })

  await user.click(await screen.findByRole('button', { name: /add contact/i }))
  const dialog = await screen.findByRole('dialog')
  await user.type(within(dialog).getByLabelText(/^number$/i), '+8613700990011')
  await user.type(within(dialog).getByLabelText(/^name$/i), 'Li Na')
  await user.type(within(dialog).getByLabelText(/^tags$/i), 'VIP, billing')
  await user.click(within(dialog).getByRole('button', { name: /save/i }))

  await waitFor(() =>
    expect(api.commands).toContainEqual(
      expect.objectContaining({
        method: 'POST',
        path: '/contacts',
        body: expect.objectContaining({
          phoneNumber: '+8613700990011',
          name: 'Li Na',
          tags: ['VIP', 'billing'],
        }),
      }),
    ),
  )
})

it('edits a contact in place', async () => {
  const { api, user } = await renderContacts({ contacts: [contactFixture()] })

  await screen.findByText('Zhang Wei')
  await user.click(screen.getByRole('button', { name: /edit/i }))
  const dialog = await screen.findByRole('dialog')
  const notes = within(dialog).getByLabelText(/^notes$/i)
  await user.clear(notes)
  await user.type(notes, 'Moved to the Shanghai office.')
  await user.click(within(dialog).getByRole('button', { name: /save/i }))

  await waitFor(() =>
    expect(api.commands).toContainEqual(
      expect.objectContaining({
        method: 'PUT',
        path: `/contacts/${contactFixture().id}`,
        body: expect.objectContaining({ notes: 'Moved to the Shanghai office.' }),
      }),
    ),
  )
})

// Deleting is confirmed in place: a contact is somebody's notes about a
// customer, and one stray click should not take them.
it('confirms before deleting', async () => {
  const { api, user } = await renderContacts({ contacts: [contactFixture()] })

  await screen.findByText('Zhang Wei')
  await user.click(screen.getByRole('button', { name: /^delete$/i }))
  expect(api.commands).toHaveLength(0)

  const confirm = await screen.findByRole('dialog')
  await user.click(within(confirm).getByRole('button', { name: /^delete$/i }))
  await waitFor(() =>
    expect(api.commands).toContainEqual(
      expect.objectContaining({ method: 'DELETE', path: `/contacts/${contactFixture().id}` }),
    ),
  )
})
