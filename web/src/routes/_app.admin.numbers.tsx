import { createFileRoute } from '@tanstack/react-router'
import { useTranslation } from 'react-i18next'
import { Pencil, Plus } from 'lucide-react'

import { PageHeader } from '@/components/page-header'
import { Field, Input, RecordDialog, Select, useRecordForm } from '@/components/record-dialog'
import { CellText, DataTable, TBody, THead, TableMessage, Td, Th, Tr } from '@/components/table'
import { Button } from '@/components/ui/button'
import { ConfirmDelete } from '@/routes/_app.admin.extensions'
import { describeError, fieldErrorText } from '@/lib/errors'
import { useFlows } from '@/lib/flows'
import { requireRole } from '@/lib/guards'
import { useCatalogMutations, useDIDs, useQueues, withMaxOutboundChannels, type DIDDraft } from '@/lib/catalog'

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
  const flows = useFlows()
  const { data: queues } = useQueues()
  const { saveDID, deleteDID } = useCatalogMutations()
  const [editing, setEditing] = useRecordForm<DIDDraft>(saveDID)

  const rows = data?.items ?? []
  const queueOptions = [
    { value: '', label: t('admin.noFallbackQueue') },
    ...(queues?.items ?? []).map((q) => ({ value: q.id, label: q.displayName })),
  ]
  const flowName = (id?: string) =>
    (flows.data?.items ?? []).find((f) => f.flowId === id)?.name ?? t('admin.noFlow')
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
            // Every boolean the server defaults is seeded here, allowInbound
            // included. An absent key is not false to a decoder: the body is
            // read into catalog.NewDID(), which is inbound, so a form that
            // simply never mentioned the field had a number the operator saw
            // unticked saved as one callers can reach — and refused for want
            // of the flow that answers it, naming a field the form said was
            // off. The checkbox now starts where the server does.
            onClick={() =>
              setEditing({
                language: 'en',
                isEnabled: true,
                isRecordingEnabled: true,
                allowInbound: true,
              })
            }
          >
            <Plus />
            {t('admin.addNumber')}
          </Button>
        }
      />

      <DataTable>
        <THead>
          <Th>{t('admin.number')}</Th>
          <Th>{t('admin.direction')}</Th>
          <Th className="hidden xl:table-cell">{t('admin.language')}</Th>
          <Th align="right">{t('admin.maxOutboundChannels')}</Th>
          <Th>{t('admin.botFlow')}</Th>
          <Th>{t('admin.fallbackQueue')}</Th>
          <Th className="hidden xl:table-cell">{t('admin.recording')}</Th>
          <Th align="right">{t('supervisor.actions')}</Th>
        </THead>
        <TBody>
          {isPending && <TableMessage colSpan={8}>{t('common.loading')}</TableMessage>}
          {isError && <TableMessage colSpan={8}>{describeError(error, t)}</TableMessage>}
          {!isPending && rows.length === 0 && (
            <TableMessage colSpan={8}>{t('admin.noNumbers')}</TableMessage>
          )}
          {rows.map((row) => (
            <Tr key={row.id}>
              <Td className="tabular font-medium">
                <CellText maxWidth="max-w-40" minWidth="min-w-upto-32">{row.number}</CellText>
                {/* The description labels the number, so it sits under it, and
                    Language and Recording show from xl up (both are in the edit
                    dialog): nine columns do not fit the 754px card at 1024. */}
                {row.description && (
                  <CellText maxWidth="max-w-48" className="text-xs font-normal text-muted-foreground">
                    {row.description}
                  </CellText>
                )}
              </Td>
              <Td className="text-xs text-muted-foreground">
                {directionOf(row, t)}
                {row.isDefaultOutbound && (
                  <span className="ml-1.5 text-primary">{t('admin.defaultOutboundMark')}</span>
                )}
              </Td>
              <Td className="hidden text-xs text-muted-foreground xl:table-cell">{row.language}</Td>
              <Td align="right" className="tabular text-xs text-muted-foreground">
                {row.allowOutbound ? (row.maxOutboundChannels ?? t('admin.noChannelLimit')) : '—'}
              </Td>
              <Td className="text-xs text-muted-foreground">
                {/* Named, not an id fragment: the flow catalogue exists now. */}
                <CellText maxWidth="max-w-40" minWidth="min-w-upto-24">{flowName(row.flowId)}</CellText>
              </Td>
              <Td className="text-xs text-muted-foreground">
                <CellText maxWidth="max-w-40" minWidth="min-w-upto-24">{queueName(row.fallbackQueueId)}</CellText>
              </Td>
              <Td className="hidden text-xs text-muted-foreground xl:table-cell">
                {row.isRecordingEnabled ? t('common.yes') : t('common.no')}
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
          <Field label={t('admin.number')} error={fieldErrorText(saveDID.error, 'number', t)}>
            <Input
              autoFocus
              disabled={Boolean(editing.id)}
              value={editing.number ?? ''}
              onChange={(e) => setEditing({ ...editing, number: e.target.value })}
            />
          </Field>
          {/* Two checkboxes rather than a choice: a number that both takes
              calls and places them is ordinary. */}
          <Field label={t('admin.direction')} error={
              fieldErrorText(saveDID.error, 'allowInbound', t) ??
              fieldErrorText(saveDID.error, 'isDefaultOutbound', t)
            } hint={t('admin.directionHint')}>
            <span className="flex flex-col gap-1.5 pt-1">
              <label className="flex items-center gap-2 text-sm">
                <input
                  type="checkbox"
                  checked={editing.allowInbound ?? false}
                  onChange={(e) => setEditing({ ...editing, allowInbound: e.target.checked })}
                />
                {t('admin.allowInbound')}
              </label>
              <label className="flex items-center gap-2 text-sm">
                <input
                  type="checkbox"
                  checked={editing.allowOutbound ?? false}
                  onChange={(e) =>
                    setEditing({
                      ...editing,
                      allowOutbound: e.target.checked,
                      // A number that cannot dial out cannot be the one calls
                      // go out from; unticking one unticks the other rather
                      // than leaving a contradiction for the server to refuse.
                      isDefaultOutbound: e.target.checked && editing.isDefaultOutbound,
                    })
                  }
                />
                {t('admin.allowOutbound')}
              </label>
              {/* Indented under the box it depends on: this is not a third
                  direction, it is a property of dialling out. */}
              {editing.allowOutbound && (
                <label className="ml-6 flex items-center gap-2 text-sm">
                  <input
                    type="checkbox"
                    checked={editing.isDefaultOutbound ?? false}
                    onChange={(e) =>
                      setEditing({ ...editing, isDefaultOutbound: e.target.checked })
                    }
                  />
                  {t('admin.isDefaultOutbound')}
                </label>
              )}
            </span>
          </Field>
          {editing.allowOutbound && (
            <Field
              label={t('admin.maxOutboundChannels')}
              error={fieldErrorText(saveDID.error, 'maxOutboundChannels', t)}
              hint={t('admin.maxOutboundChannelsHint')}
            >
              <Input
                type="number"
                min={1}
                max={100000}
                placeholder={t('admin.noChannelLimit')}
                value={editing.maxOutboundChannels ?? ''}
                onChange={(e) => setEditing(withMaxOutboundChannels(editing, e.target.value))}
              />
            </Field>
          )}
          {editing.allowInbound && (
            <Field label={t('admin.botFlow')} error={fieldErrorText(saveDID.error, 'flowId', t)} hint={t('admin.botFlowHint')}>
              <Select
                value={editing.flowId ?? ''}
                onChange={(id) => setEditing({ ...editing, flowId: id === '' ? undefined : id })}
                options={[
                  { value: '', label: t('admin.noFlow') },
                  ...(flows.data?.items ?? []).map((f) => ({ value: f.flowId, label: f.name })),
                ]}
              />
            </Field>
          )}
          <Field label={t('admin.language')} error={fieldErrorText(saveDID.error, 'language', t)} hint={t('admin.languageHint')}>
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

/**
 * Which way calls go through a number, in one phrase.
 *
 * Both is the ordinary case and has to read as one thing rather than as two
 * flags the reader has to combine.
 */
function directionOf(
  did: { allowInbound: boolean; allowOutbound: boolean },
  t: (key: string) => string,
): string {
  if (did.allowInbound && did.allowOutbound) return t('admin.directions.both')
  if (did.allowOutbound) return t('admin.directions.outbound')
  return t('admin.directions.inbound')
}
