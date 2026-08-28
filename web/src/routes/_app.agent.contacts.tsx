import { createFileRoute } from '@tanstack/react-router'
import { useTranslation } from 'react-i18next'
import { useState } from 'react'
import { Pencil, Plus } from 'lucide-react'

import { PageHeader } from '@/components/page-header'
import { Field, Input, RecordDialog, useRecordForm } from '@/components/record-dialog'
import { DataTable, TBody, THead, TableMessage, Td, Th, Tr } from '@/components/table'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { ConfirmDelete } from '@/routes/_app.admin.extensions'
import { describeError } from '@/lib/errors'
import { requireRole } from '@/lib/guards'
import { useContactMutations, useContacts, type Contact } from '@/lib/contacts'

/**
 * The contact book: who a phone number belongs to, and what the last person
 * who spoke to them wrote down.
 *
 * Deliberately small. It answers the one question the cockpit asks when a call
 * arrives — who is this — and gives the agent somewhere to put what they
 * learned. A call centre that needs opportunities and order history integrates
 * a CRM; this is not one.
 */
export const Route = createFileRoute('/_app/agent/contacts')({
  beforeLoad: ({ context }) => requireRole(context.user, 'AGENT'),
  component: ContactsPage,
})

const PAGE_SIZE = 50

/** A contact being edited, or created when it has no id yet. */
type ContactDraft = Partial<Contact> & { tagsText?: string }

function ContactsPage() {
  const { t, i18n } = useTranslation()
  const [search, setSearch] = useState('')
  const [page, setPage] = useState(0)
  const { data, isPending, isError, error } = useContacts({
    q: search || undefined,
    limit: PAGE_SIZE,
    offset: page * PAGE_SIZE,
  })
  const { create, update, remove } = useContactMutations()
  const [editing, setEditing] = useRecordForm<ContactDraft>(create, update)

  const rows = data?.items ?? []
  const total = data?.total ?? 0
  const pages = Math.max(1, Math.ceil(total / PAGE_SIZE))
  const saving = create.isPending || update.isPending
  const saveError = create.error ?? update.error
  const dateFormat = new Intl.DateTimeFormat(i18n.language, {
    month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit',
  })

  const save = () => {
    if (!editing) return
    const body = {
      phoneNumber: (editing.phoneNumber ?? '').trim(),
      name: editing.name ?? '',
      company: editing.company ?? '',
      email: editing.email ?? '',
      notes: editing.notes ?? '',
      tags: splitTags(editing.tagsText ?? (editing.tags ?? []).join(', ')),
    }
    const done = { onSuccess: () => setEditing(null) }
    if (editing.id) {
      update.mutate({ id: editing.id, body }, done)
    } else {
      create.mutate(body, done)
    }
  }

  return (
    <>
      <PageHeader
        title={t('nav.contacts')}
        description={t('contacts.hint')}
        actions={
          <div className="flex items-center gap-2">
            <Input
              className="h-8 w-56"
              placeholder={t('contacts.search')}
              aria-label={t('contacts.search')}
              value={search}
              onChange={(event) => {
                setSearch(event.target.value)
                setPage(0)
              }}
            />
            <Button size="sm" onClick={() => setEditing({})}>
              <Plus />
              {t('contacts.add')}
            </Button>
          </div>
        }
      />

      <DataTable>
        <THead>
          <Th>{t('contacts.name')}</Th>
          <Th>{t('contacts.number')}</Th>
          <Th>{t('contacts.company')}</Th>
          <Th>{t('contacts.tags')}</Th>
          <Th>{t('contacts.lastCall')}</Th>
          <Th align="right">{t('supervisor.actions')}</Th>
        </THead>
        <TBody>
          {isPending && <TableMessage colSpan={6}>{t('common.loading')}</TableMessage>}
          {isError && <TableMessage colSpan={6}>{describeError(error, t)}</TableMessage>}
          {!isPending && !isError && rows.length === 0 && (
            <TableMessage colSpan={6}>
              {search ? t('contacts.noMatches') : t('contacts.empty')}
            </TableMessage>
          )}
          {rows.map((row) => (
            <Tr key={row.id}>
              <Td className="font-medium">{row.name || '—'}</Td>
              <Td className="tabular">{row.phoneNumber}</Td>
              <Td className="text-xs text-muted-foreground">{row.company || '—'}</Td>
              <Td>
                <span className="flex flex-wrap items-center gap-1">
                  {row.tags.map((tag) => (
                    <Badge key={tag}>{tag}</Badge>
                  ))}
                </span>
              </Td>
              <Td className="tabular text-xs text-muted-foreground">
                {row.lastCallAt ? dateFormat.format(new Date(row.lastCallAt)) : t('contacts.never')}
              </Td>
              <Td align="right">
                <Button
                  size="icon-sm"
                  variant="ghost"
                  title={t('common.edit')}
                  onClick={() => setEditing(row)}
                >
                  <Pencil />
                </Button>
                <ConfirmDelete
                  label={t('contacts.deleteConfirm', { name: row.name || row.phoneNumber })}
                  onConfirm={() => remove.mutate(row.id)}
                />
              </Td>
            </Tr>
          ))}
        </TBody>
      </DataTable>

      {total > PAGE_SIZE && (
        <div className="mt-3 flex items-center justify-between text-xs text-muted-foreground">
          <span className="tabular">{t('contacts.pageOf', { page: page + 1, pages, total })}</span>
          <span className="flex gap-2">
            <Button size="sm" variant="ghost" disabled={page === 0} onClick={() => setPage(page - 1)}>
              {t('common.previous')}
            </Button>
            <Button
              size="sm"
              variant="ghost"
              disabled={page + 1 >= pages}
              onClick={() => setPage(page + 1)}
            >
              {t('common.next')}
            </Button>
          </span>
        </div>
      )}

      {editing && (
        <RecordDialog
          open
          onOpenChange={(open) => !open && setEditing(null)}
          title={editing.id ? t('contacts.edit') : t('contacts.add')}
          isSaving={saving}
          error={saveError ? describeError(saveError, t) : undefined}
          onSubmit={save}
        >
          <Field label={t('contacts.number')} hint={t('contacts.numberHint')}>
            <Input
              autoFocus
              className="tabular"
              inputMode="tel"
              value={editing.phoneNumber ?? ''}
              onChange={(e) => setEditing({ ...editing, phoneNumber: e.target.value })}
            />
          </Field>
          <Field label={t('contacts.name')}>
            <Input
              value={editing.name ?? ''}
              onChange={(e) => setEditing({ ...editing, name: e.target.value })}
            />
          </Field>
          <Field label={t('contacts.company')}>
            <Input
              value={editing.company ?? ''}
              onChange={(e) => setEditing({ ...editing, company: e.target.value })}
            />
          </Field>
          <Field label={t('contacts.email')}>
            <Input
              type="email"
              value={editing.email ?? ''}
              onChange={(e) => setEditing({ ...editing, email: e.target.value })}
            />
          </Field>
          <Field label={t('contacts.tags')} hint={t('contacts.tagsHint')}>
            <Input
              value={editing.tagsText ?? (editing.tags ?? []).join(', ')}
              onChange={(e) => setEditing({ ...editing, tagsText: e.target.value })}
            />
          </Field>
          <Field label={t('contacts.notes')} hint={t('contacts.notesHint')}>
            <textarea
              className="min-h-16 w-full rounded-md border bg-card p-2 text-sm outline-none placeholder:text-muted-foreground focus-visible:border-ring focus-visible:ring-2 focus-visible:ring-ring/30"
              value={editing.notes ?? ''}
              onChange={(e) => setEditing({ ...editing, notes: e.target.value })}
            />
          </Field>
        </RecordDialog>
      )}
    </>
  )
}

/** Tags are typed as one comma-separated line, which is how people type them. */
function splitTags(text: string): string[] {
  return text
    .split(',')
    .map((tag) => tag.trim())
    .filter(Boolean)
}
