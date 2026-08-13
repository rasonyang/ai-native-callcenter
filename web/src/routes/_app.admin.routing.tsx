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
import {
  OVERFLOW_TYPES, STRATEGIES, useCatalogMutations, useQueues,
  type OverflowType, type Queue, type Strategy,
} from '@/lib/catalog'
import { formatDuration } from '@/lib/utils'

/**
 * Queues and how they distribute. A change here is written to the database and
 * pushed to the switch, so it applies to the next caller rather than at the
 * next restart.
 */
export const Route = createFileRoute('/_app/admin/routing')({
  beforeLoad: ({ context }) => requireRole(context.user, 'ADMIN'),
  component: RoutingPage,
})

function RoutingPage() {
  const { t } = useTranslation()
  const { data, isPending, isError, error } = useQueues()
  const { saveQueue, deleteQueue } = useCatalogMutations()
  const [editing, setEditing] = useState<Partial<Queue> | null>(null)

  const rows = data?.items ?? []

  return (
    <>
      <PageHeader
        title={t('nav.routing')}
        description={t('admin.routingHint')}
        actions={
          <Button
            size="sm"
            onClick={() =>
              setEditing({
                strategy: 'LONGEST_IDLE_AGENT',
                isEnabled: true,
                isRecordingEnabled: true,
                maxWaitSec: 300,
                slaThresholdSec: 20,
                ronaDelaySec: 10,
                overflow: { type: 'ANNOUNCE_HANGUP' },
              })
            }
          >
            <Plus />
            {t('admin.addQueue')}
          </Button>
        }
      />

      <DataTable>
        <THead>
          <Th>{t('admin.queueName')}</Th>
          <Th>{t('admin.queueExtension')}</Th>
          <Th>{t('admin.strategy')}</Th>
          <Th>{t('admin.maxWait')}</Th>
          <Th>{t('admin.overflow')}</Th>
          <Th>{t('admin.recording')}</Th>
          <Th align="right">{t('supervisor.actions')}</Th>
        </THead>
        <TBody>
          {isPending && <TableMessage colSpan={7}>{t('common.loading')}</TableMessage>}
          {isError && <TableMessage colSpan={7}>{describeError(error, t)}</TableMessage>}
          {!isPending && rows.length === 0 && (
            <TableMessage colSpan={7}>{t('admin.noQueues')}</TableMessage>
          )}
          {rows.map((row) => (
            <Tr key={row.id}>
              <Td>
                <span className="font-medium">{row.displayName}</span>
                <span className="ml-2 text-xs text-muted-foreground">{row.name}</span>
              </Td>
              <Td className="tabular">{row.extNumber}</Td>
              <Td className="text-xs text-muted-foreground">
                {t(`admin.strategies.${row.strategy}`)}
              </Td>
              <Td className="tabular">
                {row.maxWaitSec > 0 ? formatDuration(row.maxWaitSec) : t('admin.noLimit')}
              </Td>
              <Td className="text-xs text-muted-foreground">
                {t(`admin.overflowTypes.${row.overflow.type}`)}
              </Td>
              <Td className="text-xs text-muted-foreground">
                {row.isRecordingEnabled ? t('common.yes') : t('common.no')}
              </Td>
              <Td align="right">
                <Button size="sm" variant="ghost" onClick={() => setEditing(row)}>
                  {t('common.edit')}
                </Button>
                <ConfirmDelete
                  label={t('admin.deleteQueueConfirm', { name: row.displayName })}
                  onConfirm={() => deleteQueue.mutate(row.id)}
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
          title={editing.id ? t('admin.editQueue') : t('admin.addQueue')}
          isSaving={saveQueue.isPending}
          error={saveQueue.isError ? describeError(saveQueue.error, t) : undefined}
          onSubmit={() => saveQueue.mutate(editing, { onSuccess: () => setEditing(null) })}
        >
          <Field label={t('admin.queueName')} hint={t('admin.queueNameHint')}>
            <Input
              autoFocus
              disabled={Boolean(editing.id)}
              value={editing.name ?? ''}
              onChange={(e) => setEditing({ ...editing, name: e.target.value })}
            />
          </Field>
          <Field label={t('admin.displayName')}>
            <Input
              value={editing.displayName ?? ''}
              onChange={(e) => setEditing({ ...editing, displayName: e.target.value })}
            />
          </Field>
          <Field label={t('admin.queueExtension')} hint={t('admin.queueExtensionHint')}>
            <Input
              disabled={Boolean(editing.id)}
              value={editing.extNumber ?? ''}
              onChange={(e) => setEditing({ ...editing, extNumber: e.target.value })}
            />
          </Field>
          <Field label={t('admin.strategy')}>
            <Select
              value={editing.strategy ?? 'LONGEST_IDLE_AGENT'}
              onChange={(v) => setEditing({ ...editing, strategy: v as Strategy })}
              options={STRATEGIES.map((s) => ({ value: s, label: t(`admin.strategies.${s}`) }))}
            />
          </Field>
          <Field label={t('admin.maxWait')} hint={t('admin.maxWaitHint')}>
            <Input
              type="number"
              min={0}
              value={editing.maxWaitSec ?? 0}
              onChange={(e) => setEditing({ ...editing, maxWaitSec: Number(e.target.value) })}
            />
          </Field>
          <Field label={t('admin.overflow')}>
            <Select
              value={editing.overflow?.type ?? 'ANNOUNCE_HANGUP'}
              onChange={(v) =>
                setEditing({
                  ...editing,
                  overflow: { ...editing.overflow, type: v as OverflowType },
                })
              }
              options={OVERFLOW_TYPES.map((o) => ({
                value: o,
                label: t(`admin.overflowTypes.${o}`),
              }))}
            />
          </Field>
          {editing.overflow?.type !== 'ANNOUNCE_HANGUP' && (
            <Field label={t('admin.overflowTarget')} hint={t('admin.overflowTargetHint')}>
              <Input
                value={editing.overflow?.target ?? ''}
                onChange={(e) =>
                  setEditing({
                    ...editing,
                    overflow: {
                      type: editing.overflow?.type ?? 'FORWARD',
                      ...editing.overflow,
                      target: e.target.value,
                    },
                  })
                }
              />
            </Field>
          )}
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
