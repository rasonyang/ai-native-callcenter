import { createContext, useContext, useEffect, useRef, useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'

import { CALLS_KEY, PRESENCE_KEY, ROSTER_KEY, WAITING_KEY, WRAP_UP_KEY } from './agent'
import { CALLBACKS_KEY, CDRS_KEY, REPORTS_KEY } from './ledger'
import { connectEvents, type AiccEvent, type EventType } from './events'

/**
 * Refreshes the caches an event invalidates.
 *
 * The event carries enough to render a change, but the roster's derived
 * availability combines persisted presence with observed device state, so the
 * authoritative answer is refetched rather than reconstructed in the browser.
 */
function applyToCache(queryClient: ReturnType<typeof useQueryClient>, event: AiccEvent) {
  if (event.type.startsWith('AGENT_') || event.type.startsWith('DEVICE_')) {
    void queryClient.invalidateQueries({ queryKey: ROSTER_KEY })
    void queryClient.invalidateQueries({ queryKey: PRESENCE_KEY })
    // Signing in, going ready, entering after-call work: each of them moves
    // what the agent's own day is made of — and after-call work beginning is
    // what opens the record the cockpit asks them to confirm.
    void queryClient.invalidateQueries({ queryKey: REPORTS_KEY })
    void queryClient.invalidateQueries({ queryKey: WRAP_UP_KEY })
  }
  if (event.type.startsWith('PARTY_') || event.type === 'CALL_USER_DATA') {
    // A leg moved: the cockpit and the supervisor's live view both read the
    // call snapshot, which is authoritative on the switch.
    void queryClient.invalidateQueries({ queryKey: CALLS_KEY })
  }
  if (event.type.startsWith('QUEUE_')) {
    // Somebody joined a queue, was answered or gave up: the agent's waiting
    // list moved. The event carries the queue's new depth, but the list is the
    // authoritative order and it is one small request.
    void queryClient.invalidateQueries({ queryKey: WAITING_KEY })
  }
  if (event.type.startsWith('CALLBACK_')) {
    void queryClient.invalidateQueries({ queryKey: CALLBACKS_KEY })
  }
  if (event.type === 'CALL_CDR') {
    // A finished call moves every ledger-backed screen.
    void queryClient.invalidateQueries({ queryKey: CDRS_KEY })
    void queryClient.invalidateQueries({ queryKey: REPORTS_KEY })
  }
}

export type StreamStatus = 'connected' | 'reconnecting' | 'offline'

type Listener = (event: AiccEvent) => void

/** Runs one listener without letting its failure reach the others. */
function notify(listener: Listener, event: AiccEvent) {
  try {
    listener(event)
  } catch (error) {
    console.error('event listener failed', event.type, error)
  }
}

/**
 * Opens the one application-wide event stream and reports its status.
 *
 * A SYSTEM_RESET means the client fell outside the server's replay window:
 * every query is invalidated so the UI resyncs from snapshots before trusting
 * incremental updates again.
 */
export function useEventStream(enabled: boolean) {
  const queryClient = useQueryClient()
  const [status, setStatus] = useState<StreamStatus>('offline')
  const listeners = useRef(new Map<EventType | '*', Listener[]>())

  useEffect(() => {
    if (!enabled) {
      setStatus('offline')
      return
    }

    const close = connectEvents({
      onOpen: () => {
        setStatus('connected')
        // Anything fetched before the stream attached may have missed events
        // in the gap, so snapshots are refetched once the tail is live. This
        // also covers every reconnect.
        void queryClient.invalidateQueries()
      },
      onError: () => setStatus('reconnecting'),
      onReset: () => void queryClient.invalidateQueries(),
      onEvent: (event) => {
        applyToCache(queryClient, event)
        // Each listener is isolated. Without this a listener that throws takes
        // the rest of the listeners for that event with it, so a fault in a
        // secondary panel could starve the softphone's cache updates — the
        // one thing on this screen that must never stop.
        for (const listener of listeners.current.get(event.type) ?? []) notify(listener, event)
        for (const listener of listeners.current.get('*') ?? []) notify(listener, event)
      },
    })

    return () => {
      close()
      setStatus('offline')
    }
  }, [enabled, queryClient])

  return { status, listeners: listeners.current }
}

/**
 * Makes the stream's listener registry reachable from anywhere below the app
 * shell, so a panel can tail events without opening a second EventSource —
 * the application has exactly one, by design.
 */
interface EventStreamValue {
  status: StreamStatus
  listeners: Map<EventType | '*', Listener[]>
}

const EventStreamContext = createContext<EventStreamValue | null>(null)

export const EventStreamProvider = EventStreamContext.Provider

/**
 * The stream's health, for panels that must be honest about not hearing it.
 * Defaults to offline outside the provider, which is the safe reading.
 */
export function useStreamStatus(): StreamStatus {
  return useContext(EventStreamContext)?.status ?? 'offline'
}

/**
 * Subscribes to one event type for the lifetime of the component.
 *
 * The handler is held in a ref so a caller may pass an inline closure without
 * re-subscribing on every render, which would drop events in the gap.
 */
export function useEventListener(type: EventType, handler: Listener) {
  const registry = useContext(EventStreamContext)?.listeners
  const ref = useRef(handler)
  ref.current = handler

  useEffect(() => {
    if (!registry) return
    const listener: Listener = (event) => ref.current(event)
    const existing = registry.get(type) ?? []
    registry.set(type, [...existing, listener])
    return () => {
      registry.set(type, (registry.get(type) ?? []).filter((l) => l !== listener))
    }
  }, [registry, type])
}
