import { useCallback, useEffect, useRef, useState } from 'react'

import { downloadCsv } from './download'

export interface CsvExportProgress {
  done: number
  total: number
}

/** Builds the file's text, reporting progress; rejects on failure or when signal aborts. */
export type BuildCsv = (
  signal: AbortSignal,
  onProgress: (progress: CsvExportProgress) => void,
) => Promise<string>

export type CsvExportState =
  | { phase: 'idle' }
  | { phase: 'running'; progress?: CsvExportProgress }
  | { phase: 'failed'; error: unknown }

/**
 * One export at a time, built in the browser. The file is handed over only
 * when the build has finished: a cancel, a failed request or leaving the
 * screen writes nothing. A failure is kept to be shown until the next start;
 * a cancel is not a failure and shows nothing.
 */
export function useCsvExport() {
  const [state, setState] = useState<CsvExportState>({ phase: 'idle' })
  const running = useRef<AbortController | null>(null)

  // Leaving the screen cancels: nothing is downloaded behind the reader's back.
  useEffect(() => () => running.current?.abort(), [])

  const start = useCallback(async (fileName: string, build: BuildCsv) => {
    running.current?.abort()
    const controller = new AbortController()
    running.current = controller
    const isCurrent = () => running.current === controller
    setState({ phase: 'running' })
    try {
      const text = await build(controller.signal, (progress) => {
        if (isCurrent()) setState({ phase: 'running', progress })
      })
      if (!isCurrent() || controller.signal.aborted) return
      running.current = null
      downloadCsv(fileName, text)
      setState({ phase: 'idle' })
    } catch (error) {
      if (!isCurrent()) return
      running.current = null
      // Stops whatever the build still has in flight.
      const wasCancelled = controller.signal.aborted
      controller.abort()
      setState(wasCancelled ? { phase: 'idle' } : { phase: 'failed', error })
    }
  }, [])

  const cancel = useCallback(() => {
    running.current?.abort()
    running.current = null
    setState({ phase: 'idle' })
  }, [])

  return { state, start, cancel }
}
