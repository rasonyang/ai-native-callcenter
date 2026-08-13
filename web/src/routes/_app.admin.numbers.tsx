import { createFileRoute } from '@tanstack/react-router'
import { useTranslation } from 'react-i18next'
import { useState } from 'react'
import { Plus } from 'lucide-react'

import { PageHeader } from '@/components/page-header'
import { Field, Input, RecordDialog, Select } from '@/components/record-dialog'
import { DataTable, TBody, THead, TableMessage, Td, Th, Tr } from '@/components/table'
import { Button } from '@/components/ui/button'
import { ConfirmDelete } from '@/routes/_app.admin.extensions'
import { describeError } from '@/lib/errors'
import { requireRole } from '@/lib/guards'
import { useCatalogMutations, useDIDs, useQueues, type DID } from '@/lib/catalog'

/**
 * External numbers.
 *
 * Every number answers with a bot; the queue named here is only where the
 * caller goes when the bot cannot take the call at all.
 */
export const Route = createFileRoute('/_app/admin/numbers')({
  beforeLoad: ({ context }) => requireRole(context.user, 'ADMIN'),
  component: NumbersPage,
})

function NumbersPage() {
  const { t } = useTranslation()
  const { data, isPending, isError, error } = useDIDs()
  const { data: queues } = useQueues()
  const { saveDID, deleteDID } = useCatalogMutations()
  const [editing, setEditing] = useState<Partial<DID> | null>(null)

  const rows = data?.items ?? []
  const queueOptions = [
    { value: '', label: t('admin.noFallbackQueue') },
    ...(queues?.items ?? []).map((q) => ({ value: q.id, label: q.displayName })),
  ]
  const queueName = (id?: string) =>
    queues?.items.find((q) => q.id === id)?.displayName ?? '—'

  return (
    <>
      <PageHeader
        title={t('nav.numbers')}
        description={t('admin.numbersHint')}
        actions={
          <Button
            size="sm"
            onClick={() => setEditing({ language: 'en', isEnabled: true, isRecordingEnabled: true })}
          >
            <Plus />
            {t('admin.addNumber')}
          </Button>
        }
      />

      <DataTable>
        <THead>
          <Th>{t('admin.number')}</Th>
          <Th>{t('admin.language')}</Th>
          <Th>{t('admin.botFlow')}</Th>
          <Th>{t('admin.fallbackQueue')}</Th>
          <Th>{t('admin.recording')}</Th>
          <Th>{t('admin.description')}</Th>
          <Th align="right">{t('supervisor.actions')}</Th>
        </THead>
        <TBody>
          {isPending && <TableMessage colSpan={7}>{t('common.loading')}</TableMessage>}
          {isError && <TableMessage colSpan={7}>{describeError(error, t)}</TableMessage>}
          {!isPending && rows.length === 0 && (
            <TableMessage colSpan={7}>{t('admin.noNumbers')}</TableMessage>
          )}
          {rows.map((row) => (
            <Tr key={row.id}>
              <Td className="tabular font-medium">{row.number}</Td>
              <Td className="text-xs text-muted-foreground">{row.language}</Td>
              <Td className="text-xs text-muted-foreground">
                {/* The flow catalogue arrives with the AI voice leg. */}
                {row.flowId ? row.flowId.slice(0, 8) : t('admin.flowPending')}
              </Td>
              <Td className="text-xs text-muted-foreground">{queueName(row.fallbackQueueId)}</Td>
              <Td className="text-xs text-muted-foreground">
                {row.isRecordingEnabled ? t('common.yes') : t('common.no')}
              </Td>
              <Td className="text-xs text-muted-foreground">{row.description || '—'}</Td>
              <Td align="right">
                <Button size="sm" variant="ghost" onClick={() => setEditing(row)}>
                  {t('common.edit')}
                </Button>
                <ConfirmDelete
                  label={t('admin.deleteNumberConfirm', { number: row.number })}
                  onConfirm={() => deleteDID.mutate(row.id)}
                />
              </Td>
            </Tr>
          ))}
        </TBody>
      </DataTable>

      {editing && (
        <RecordDialog
          open
          onOpenChange={(open) => !open && setEditing(null)}
          title={editing.id ? t('admin.editNumber') : t('admin.addNumber')}
          isSaving={saveDID.isPending}
          error={saveDID.isError ? describeError(saveDID.error, t) : undefined}
          onSubmit={() => saveDID.mutate(editing, { onSuccess: () => setEditing(null) })}
        >
          <Field label={t('admin.number')}>
            <Input
              autoFocus
              disabled={Boolean(editing.id)}
              value={editing.number ?? ''}
              onChange={(e) => setEditing({ ...editing, number: e.target.value })}
            />
          </Field>
          <Field label={t('admin.language')} hint={t('admin.languageHint')}>
            <Select
              value={editing.language ?? 'en'}
              onChange={(language) => setEditing({ ...editing, language })}
              options={[
                { value: 'en', label: 'English' },
                { value: 'zh', label: '中文' },
              ]}
            />
          </Field>
          <Field label={t('admin.fallbackQueue')} hint={t('admin.fallbackQueueHint')}>
            <Select
              value={editing.fallbackQueueId ?? ''}
              onChange={(id) =>
                setEditing({ ...editing, fallbackQueueId: id === '' ? undefined : id })
              }
              options={queueOptions}
            />
          </Field>
          <Field label={t('admin.description')}>
            <Input
              value={editing.description ?? ''}
              onChange={(e) => setEditing({ ...editing, description: e.target.value })}
            />
          </Field>
          <Field label={t('admin.recording')}>
            <Select
              value={String(editing.isRecordingEnabled ?? true)}
              onChange={(v) => setEditing({ ...editing, isRecordingEnabled: v === 'true' })}
              options={[
                { value: 'true', label: t('common.yes') },
                { value: 'false', label: t('common.no') },
              ]}
            />
          </Field>
        </RecordDialog>
      )}
    </>
  )
}
