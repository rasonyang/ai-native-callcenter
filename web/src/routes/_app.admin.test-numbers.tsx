import { createFileRoute } from '@tanstack/react-router'
import { Search } from 'lucide-react'
import { useState } from 'react'
import { useTranslation } from 'react-i18next'

import { PageHeader } from '@/components/page-header'
import { Pager } from '@/components/pager'
import { CellText, DataTable, TBody, THead, TableMessage, Td, Th, Tr } from '@/components/table'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { describeError, fieldErrorText } from '@/lib/errors'
import { requireRole } from '@/lib/guards'
import { TEST_NUMBER_LIMIT, useTestNumberMutations, useTestNumbers } from '@/lib/testNumbers'
import { ConfirmDelete } from '@/routes/_app.admin.extensions'

/**
 * The list of test numbers: exempt from the cross-campaign frequency window and the daily dial cap.
 * A number may be typed in any common form; the server answers with the
 * canonical one, which is what the list shows.
 */
export const Route = createFileRoute('/_app/admin/test-numbers')({
  beforeLoad: ({ context }) => requireRole(context.user, 'ADMIN', 'SUPERVISOR'),
  component: TestNumbersPage,
})

const PAGE_SIZE = 50

function TestNumbersPage() {
  const { t, i18n } = useTranslation()
  const [search, setSearch] = useState('')
  const [page, setPage] = useState(0)
  const [number, setNumber] = useState('')
  const [note, setNote] = useState('')
  const [added, setAdded] = useState<string | null>(null)
  const prefix = search.replace(/[\s-]/g, '')
  const { data, isPending, isError, error } = useTestNumbers({
    prefix: prefix || undefined,
    limit: PAGE_SIZE,
    offset: page * PAGE_SIZE,
  })
  const { add, remove } = useTestNumberMutations()
  const rows = data?.items ?? []
  const time = new Intl.DateTimeFormat(i18n.language, {
    year: 'numeric', month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit',
  })

  const fieldError = fieldErrorText(add.error, 'number', t)

  return (
    <>
      <PageHeader title={t('nav.testNumbers')} description={t('testNumbers.hint')} />
      <p className="mb-4 max-w-2xl text-xs text-muted-foreground">
        {t('testNumbers.scope', { limit: TEST_NUMBER_LIMIT })}
      </p>

      <form
        className="mb-4 flex max-w-2xl items-start gap-2"
        onSubmit={(event) => {
          event.preventDefault()
          setAdded(null)
          add.mutate(
            { number, ...(note.trim() ? { note: note.trim() } : {}) },
            {
              onSuccess: (entry) => {
                setAdded(entry.number)
                setNumber('')
                setNote('')
              },
            },
          )
        }}
      >
        <div className="min-w-0 flex-1 space-y-1">
          <div className="flex gap-2">
            <Input
              aria-label={t('testNumbers.numberToAdd')}
              aria-invalid={Boolean(fieldError)}
              className="min-w-0 flex-1"
              placeholder={t('testNumbers.numberPlaceholder')}
              value={number}
              onChange={(e) => setNumber(e.target.value)}
            />
            <Input
              aria-label={t('testNumbers.noteToAdd')}
              className="min-w-0 flex-1"
              maxLength={200}
              placeholder={t('testNumbers.notePlaceholder')}
              value={note}
              onChange={(e) => setNote(e.target.value)}
            />
          </div>
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
              {t('testNumbers.added', { number: added })}
            </p>
          )}
        </div>
        <Button type="submit" size="lg" disabled={add.isPending}>
          {t('testNumbers.add')}
        </Button>
      </form>

      <div className="relative mb-3 w-64">
        <Search className="pointer-events-none absolute left-2 top-1/2 size-3.5 -translate-y-1/2 text-muted-foreground" />
        <Input
          aria-label={t('testNumbers.search')}
          className="pl-7"
          placeholder={t('testNumbers.searchPlaceholder')}
          value={search}
          onChange={(e) => {
            setSearch(e.target.value)
            setPage(0)
          }}
        />
      </div>

      <DataTable>
        <THead>
          <Th>{t('testNumbers.number')}</Th>
          <Th>{t('testNumbers.note')}</Th>
          <Th>{t('testNumbers.addedAt')}</Th>
          <Th align="right">{t('supervisor.actions')}</Th>
        </THead>
        <TBody>
          {isPending && <TableMessage colSpan={4}>{t('common.loading')}</TableMessage>}
          {isError && <TableMessage colSpan={4}>{describeError(error, t)}</TableMessage>}
          {!isPending && !isError && rows.length === 0 && (
            <TableMessage colSpan={4}>{t('testNumbers.none')}</TableMessage>
          )}
          {rows.map((row) => (
            <Tr key={row.number}>
              <Td className="tabular font-medium">
                <CellText maxWidth="max-w-48" minWidth="min-w-upto-32" title={row.number}>
                  {row.number}
                </CellText>
              </Td>
              <Td className="text-xs text-muted-foreground">
                {row.note ? (
                  <CellText maxWidth="max-w-48" minWidth="min-w-upto-32" title={row.note}>
                    {row.note}
                  </CellText>
                ) : (
                  '—'
                )}
              </Td>
              <Td className="tabular text-xs text-muted-foreground">
                {time.format(new Date(row.createdAt))}
              </Td>
              <Td align="right">
                <ConfirmDelete
                  label={t('testNumbers.removeConfirm', { number: row.number })}
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
