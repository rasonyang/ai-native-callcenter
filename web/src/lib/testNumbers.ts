import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import type { components } from '@/generated/api'

import { request } from './api'

/**
 * The list of test numbers: numbers exempt from the cross-campaign frequency
 * window, so a team can test campaigns on its own phones.
 */

export type TestNumber = components['schemas']['TestNumber']

/** The most numbers the list holds (the contract's cap). */
export const TEST_NUMBER_LIMIT = 100

export interface TestNumberFilter {
  prefix?: string
  limit?: number
  offset?: number
}

export const TEST_NUMBERS_KEY = ['test-numbers'] as const

export const testNumbersApi = {
  /** `prefix` matches the start of the canonical number; the + travels encoded. */
  list: (filter: TestNumberFilter) => {
    const search = new URLSearchParams()
    for (const [key, value] of Object.entries(filter)) {
      if (value !== undefined && value !== '') search.set(key, String(value))
    }
    const encoded = search.toString()
    return request<components['schemas']['TestNumberList']>(
      `/test-numbers${encoded ? `?${encoded}` : ''}`,
    )
  },
  /** Any common form; the answer carries the canonical E.164 number. */
  add: (body: components['schemas']['TestNumberWrite']) =>
    request<TestNumber>('/test-numbers', { method: 'POST', body: JSON.stringify(body) }),
  remove: (number: string) =>
    request<void>(`/test-numbers/${encodeURIComponent(number)}`, { method: 'DELETE' }),
}

export function useTestNumbers(filter: TestNumberFilter) {
  return useQuery({
    queryKey: [...TEST_NUMBERS_KEY, filter],
    queryFn: () => testNumbersApi.list(filter),
  })
}

export function useTestNumberMutations() {
  const queryClient = useQueryClient()
  const refresh = () => void queryClient.invalidateQueries({ queryKey: TEST_NUMBERS_KEY })
  return {
    add: useMutation({ mutationFn: testNumbersApi.add, onSuccess: refresh }),
    remove: useMutation({ mutationFn: testNumbersApi.remove, onSuccess: refresh }),
  }
}
