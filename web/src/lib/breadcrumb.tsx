import { createContext, useContext, useEffect, useState, type ReactNode } from 'react'

/**
 * The last breadcrumb segment, when the page is about one thing.
 *
 * It cannot be derived from the path: /admin/bots/019ffd60-… names a flow by
 * an id nobody reads, and the name the operator recognises only exists once
 * the page has loaded it. So the page says it, and the shell displays it.
 */
const DetailContext = createContext<{
  detail?: string
  setDetail: (detail?: string) => void
}>({ setDetail: () => {} })

export function BreadcrumbDetailProvider({ children }: { children: ReactNode }) {
  const [detail, setDetail] = useState<string | undefined>()
  return <DetailContext value={{ detail, setDetail }}>{children}</DetailContext>
}

/** What the shell should show as the last segment right now. */
export function useBreadcrumbDetail(): string | undefined {
  return useContext(DetailContext).detail
}

/**
 * Names what this page is about. Passing undefined — which is what a page
 * still loading has to pass — leaves the trail at "Group / Section" rather
 * than showing a gap, and the name appears when it is actually known.
 */
export function useNameThisPage(detail: string | undefined) {
  const { setDetail } = useContext(DetailContext)
  useEffect(() => {
    setDetail(detail)
    // Cleared on the way out, or the next page inherits a title that was
    // never about it.
    return () => setDetail(undefined)
  }, [detail, setDetail])
}
