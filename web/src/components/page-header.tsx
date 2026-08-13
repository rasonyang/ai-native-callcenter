import type { ReactNode } from 'react'

/**
 * The page heading every screen uses: title, optional one-line description,
 * and the actions that belong to the page on the right.
 *
 * Reused verbatim rather than re-implemented per page (web/CLAUDE.md).
 */
export function PageHeader({
  title,
  description,
  actions,
}: {
  title: string
  description?: string
  actions?: ReactNode
}) {
  return (
    <div className="mb-4 flex items-start justify-between gap-4">
      <div>
        <h1 className="text-xl font-semibold">{title}</h1>
        {description && <p className="mt-1 text-xs text-muted-foreground">{description}</p>}
      </div>
      {actions && <div className="flex shrink-0 items-center gap-2">{actions}</div>}
    </div>
  )
}
