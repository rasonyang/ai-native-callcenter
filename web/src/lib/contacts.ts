import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import type { components } from '@/generated/api'

import { request } from './api'

/**
 * The contact book: who a phone number belongs to, and what the last person
 * who spoke to them wrote down.
 *
 * Deliberately small. This is not a CRM — it answers the one question the
 * cockpit asks when a call arrives, and gives the agent somewhere to put what
 * they learned.
 */

export type Contact = components['schemas']['Contact']

export type ContactWrite = components['schemas']['ContactWrite']

export interface ContactFilter {
  q?: string
  phoneNumber?: string
  limit?: number
  offset?: number
}

function query(params: Record<string, string | number | undefined>): string {
  const search = new URLSearchParams()
  for (const [key, value] of Object.entries(params)) {
    if (value !== undefined && value !== '') search.set(key, String(value))
  }
  const encoded = search.toString()
  return encoded ? `?${encoded}` : ''
}

export const contactApi = {
  list: (filter: ContactFilter) =>
    request<{ items: Contact[]; total: number }>(`/contacts${query({ ...filter })}`),

  create: (body: ContactWrite) =>
    request<Contact>('/contacts', { method: 'POST', body: JSON.stringify(body) }),

  update: (id: string, body: ContactWrite) =>
    request<Contact>(`/contacts/${id}`, { method: 'PUT', body: JSON.stringify(body) }),

  remove: (id: string) => request<void>(`/contacts/${id}`, { method: 'DELETE' }),
}

export const CONTACTS_KEY = ['contacts']

export function useContacts(filter: ContactFilter) {
  return useQuery({
    queryKey: [...CONTACTS_KEY, filter],
    queryFn: () => contactApi.list(filter),
    placeholderData: (previous) => previous,
  })
}

/**
 * The contact behind a live caller's number, or none.
 *
 * An exact-number lookup rather than a search: the cockpit is answering "who
 * is this", and a substring match would put somebody else's name on the card.
 */
export function useContactFor(phoneNumber: string | undefined) {
  const enabled = Boolean(phoneNumber)
  const result = useQuery({
    queryKey: [...CONTACTS_KEY, 'byNumber', phoneNumber],
    enabled,
    staleTime: 30_000,
    queryFn: () => contactApi.list({ phoneNumber, limit: 1 }),
  })
  return { ...result, contact: enabled ? result.data?.items?.[0] : undefined }
}

export function useContactMutations() {
  const queryClient = useQueryClient()
  const invalidate = () => void queryClient.invalidateQueries({ queryKey: CONTACTS_KEY })
  return {
    create: useMutation({ mutationFn: contactApi.create, onSuccess: invalidate }),
    update: useMutation({
      mutationFn: ({ id, body }: { id: string; body: ContactWrite }) =>
        contactApi.update(id, body),
      onSuccess: invalidate,
    }),
    remove: useMutation({ mutationFn: contactApi.remove, onSuccess: invalidate }),
  }
}
