import { createFileRoute } from '@tanstack/react-router'
import { Search } from 'lucide-react'
import { useState } from 'react'
import { useTranslation } from 'react-i18next'

import { PageHeader } from '@/components/page-header'
import { Pager } from '@/components/pager'
import { DataTable, TBody, THead, TableMessage, Td, Th, Tr } from '@/components/table'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { useDncMutations, useDncNumbers } from '@/lib/dnc'
import { describeError, fieldErrorText } from '@/lib/errors'
import { requireRole } from '@/lib/guards'
import { ConfirmDelete } from '@/routes/_app.admin.extensions'

/**
 * The do-not-call list. A number may be typed in any common form; the server
 * answers with the canonical one, which is what the list shows.
 */
export const Route = createFileRoute('/_app/admin/dnc')({
  beforeLoad: ({ context }) => requireRole(context.user, 'ADMIN', 'SUPERVISOR'),
  component: DncPage,
})

const PAGE_SIZE = 50

function DncPage() {
  const { t, i18n } = useTranslation()
  const [search, setSearch] = useState('')
  const [page, setPage] = useState(0)
  const [number, setNumber] = useState('')
  const [added, setAdded] = useState<string | null>(null)
  const prefix = search.replace(/[\s-]/g, '')
  const { data, isPending, isError, error } = useDncNumbers({
    prefix: prefix || undefined,
    limit: PAGE_SIZE,
    offset: page * PAGE_SIZE,
  })
  const { add, remove } = useDncMutations()
  const rows = data?.items ?? []
  const time = new Intl.DateTimeFormat(i18n.language, {
    year: 'numeric', month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit',
  })

  const fieldError = fieldErrorText(add.error, 'number', t)

  return (
    <>
      <PageHeader title={t('nav.dnc')} description={t('dnc.hint')} />

      <form
        className="mb-4 flex max-w-xl items-start gap-2"
        onSubmit={(event) => {
          event.preventDefault()
          setAdded(null)
          add.mutate(number, {
            onSuccess: (entry) => {
              setAdded(entry.number)
              setNumber('')
            },
          })
        }}
      >
        <div className="flex-1 space-y-1">
          <Input
            aria-label={t('dnc.numberToAdd')}
            aria-invalid={Boolean(fieldError)}
            placeholder={t('dnc.numberPlaceholder')}
            value={number}
            onChange={(e) => setNumber(e.target.value)}
          />
          {fieldError && (
            <p role="alert" className="text-xs" style={{ color: 'var(--state-breach)' }}>
              {fieldError}
            </p>
          )}
          {add.isError && !fieldError && (
            <p role="alert" className="text-xs" style={{ color: 'var(--state-breach)' }}>
              {describeError(add.error, t)}
            </p>
          )}
          {added && !add.isError && (
            <p role="status" className="tabular text-xs text-muted-foreground">
              {t('dnc.added', { number: added })}
            </p>
          )}
        </div>
        <Button type="submit" size="lg" disabled={add.isPending}>
          {t('dnc.add')}
        </Button>
      </form>

      <div className="relative mb-3 w-64">
        <Search className="pointer-events-none absolute left-2 top-1/2 size-3.5 -translate-y-1/2 text-muted-foreground" />
        <Input
          aria-label={t('dnc.search')}
          className="pl-7"
          placeholder={t('dnc.searchPlaceholder')}
          value={search}
          onChange={(e) => {
            setSearch(e.target.value)
            setPage(0)
          }}
        />
      </div>

      <DataTable>
        <THead>
          <Th>{t('dnc.number')}</Th>
          <Th>{t('dnc.source')}</Th>
          <Th>{t('dnc.addedAt')}</Th>
          <Th align="right">{t('supervisor.actions')}</Th>
        </THead>
        <TBody>
          {isPending && <TableMessage colSpan={4}>{t('common.loading')}</TableMessage>}
          {isError && <TableMessage colSpan={4}>{describeError(error, t)}</TableMessage>}
          {!isPending && !isError && rows.length === 0 && (
            <TableMessage colSpan={4}>{t('dnc.none')}</TableMessage>
          )}
          {rows.map((row) => (
            <Tr key={row.number}>
              <Td className="tabular font-medium">{row.number}</Td>
              <Td className="text-xs text-muted-foreground">{t(`dnc.sources.${row.source}`)}</Td>
              <Td className="tabular text-xs text-muted-foreground">
                {time.format(new Date(row.createdAt))}
              </Td>
              <Td align="right">
                <ConfirmDelete
                  label={t('dnc.removeConfirm', { number: row.number })}
                  onConfirm={() => remove.mutate(row.number)}
                />
              </Td>
            </Tr>
          ))}
        </TBody>
      </DataTable>
      {remove.isError && (
        <p role="alert" className="mt-2 text-xs" style={{ color: 'var(--state-breach)' }}>
          {describeError(remove.error, t)}
        </p>
      )}
      <Pager page={page} pageSize={PAGE_SIZE} total={data?.total ?? 0} onPage={setPage} />
    </>
  )
}
