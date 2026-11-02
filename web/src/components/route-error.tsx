import { createContext, useContext, useEffect, type ReactNode } from 'react'
import { Link, useRouter } from '@tanstack/react-router'
import { useTranslation } from 'react-i18next'

import { LanguageSwitch } from '@/components/language-switch'
import { PageHeader } from '@/components/page-header'
import { Button } from '@/components/ui/button'
import { isServerUnreachable } from '@/lib/errors'

/**
 * Whether the error is drawn inside the application shell. The authenticated
 * layout provides it around its content, so the answer follows what is on the
 * screen and not the URL: an error of `/` or of the layout itself has no shell
 * to sit in, and draws its own frame.
 */
const InShellContext = createContext(false)

export function InShellProvider({ children }: { children: ReactNode }) {
  return <InShellContext.Provider value={true}>{children}</InShellContext.Provider>
}

/**
 * The error state of every route match: the router's default error component.
 *
 * What it says depends on the failure. A server that cannot be reached is not
 * the reader's doing and their session is intact; anything else is a page that
 * broke. The error's own message is never shown (it is a developer string, in
 * English); it goes to the console once, when the error first appears.
 */
export function RouteError({ error }: { error: unknown }) {
  const { t } = useTranslation()
  const router = useRouter()
  const isInShell = useContext(InShellContext)
  const isUnreachable = isServerUnreachable(error)
  const kind = isUnreachable ? 'unreachable' : 'unexpected'

  // React already logs an error a boundary catches; one the layout hands over
  // as a value (a failed session check that kept the shell) was never thrown,
  // so only that case logs here.
  useEffect(() => {
    if (!isCaughtByBoundary(error)) console.error(error)
  }, [error])

  const title = t(`routeError.${kind}.title`)
  const description = t(`routeError.${kind}.description`)
  const actions = (
    <>
      <Button onClick={() => void router.invalidate()}>{t('routeError.retry')}</Button>
      {!isUnreachable && (
        <Button asChild variant="outline">
          <Link to="/">{t('routeError.backToStart')}</Link>
        </Button>
      )}
    </>
  )

  if (isInShell) {
    return (
      <div role="alert">
        <PageHeader title={title} description={description} />
        <div className="flex items-center gap-2">{actions}</div>
      </div>
    )
  }

  return (
    <div className="flex min-h-screen items-center justify-center p-6">
      <div className="w-full max-w-sm">
        <div className="mb-4 flex items-baseline justify-between">
          <div>
            <div className="text-lg font-semibold">{t('app.name')}</div>
            <div className="text-xs text-muted-foreground">{t('app.tagline')}</div>
          </div>
          <LanguageSwitch />
        </div>
        <div role="alert" className="rounded-md border bg-card p-4">
          <h1 className="text-base font-medium">{title}</h1>
          <p className="mt-1 mb-4 text-xs text-muted-foreground">{description}</p>
          <div className="flex items-center gap-2">{actions}</div>
        </div>
      </div>
    </div>
  )
}

/** Errors the boundary caught are tagged so the effect above does not log them twice. */
const caught = new WeakSet<object>()

export function markCaught(error: unknown) {
  if (typeof error === 'object' && error !== null) caught.add(error)
}

function isCaughtByBoundary(error: unknown) {
  return typeof error === 'object' && error !== null && caught.has(error)
}
