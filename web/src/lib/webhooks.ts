import { useQuery } from '@tanstack/react-query'

import type { components } from '@/generated/api'
import { request } from './api'

/** A place finished calls are delivered to. */
export type WebhookSubscription = components['schemas']['WebhookSubscription']

/** One attempt-set at telling one subscription about one revision of a call. */
export type WebhookDelivery = components['schemas']['WebhookDelivery']

export type WebhookFilter = components['schemas']['WebhookFilter']

const KEY = ['webhook-subscriptions'] as const

export const webhookApi = {
  list: () =>
    request<components['schemas']['WebhookSubscriptionList']>('/webhook-subscriptions'),
  deliveries: (subscriptionId: string, limit = 50) =>
    request<components['schemas']['WebhookDeliveryList']>(
      `/webhook-subscriptions/${subscriptionId}/deliveries?limit=${limit}`,
    ),
}

export function useWebhookSubscriptions() {
  return useQuery({ queryKey: KEY, queryFn: webhookApi.list })
}

/**
 * A subscription's recent attempts. Enabled only once one is selected, because
 * the endpoint is per subscription and there is nothing to ask for until then.
 */
export function useWebhookDeliveries(subscriptionId: string | undefined) {
  return useQuery({
    queryKey: [...KEY, subscriptionId, 'deliveries'],
    queryFn: () => webhookApi.deliveries(subscriptionId!),
    enabled: Boolean(subscriptionId),
  })
}
