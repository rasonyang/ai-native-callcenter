import { useEffect, useRef, useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'

import { connectEvents, type AiccEvent, type EventType } from './events'

export type StreamStatus = 'connected' | 'reconnecting' | 'offline'

type Listener = (event: AiccEvent) => void

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
  const listeners = useRef(new Map<EventType | '*', Set<Listener>>())

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
        for (const listener of listeners.current.get(event.type) ?? []) listener(event)
        for (const listener of listeners.current.get('*') ?? []) listener(event)
      },
    })

    return () => {
      close()
      setStatus('offline')
    }
  }, [enabled, queryClient])

  return { status, listeners: listeners.current }
}
