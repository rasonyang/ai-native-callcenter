import { createFileRoute } from '@tanstack/react-router'
import { useTranslation } from 'react-i18next'
import { useState } from 'react'
import { useQueries } from '@tanstack/react-query'
import { Pencil, Plus, Users, X } from 'lucide-react'
import { Dialog } from 'radix-ui'

import { PageHeader } from '@/components/page-header'
import { Field, Input, RecordDialog, Select, useRecordForm } from '@/components/record-dialog'
import { DataTable, TBody, THead, TableMessage, Td, Th, Tr } from '@/components/table'
import { Button } from '@/components/ui/button'
import { ConfirmDelete } from '@/routes/_app.admin.extensions'
import { describeError } from '@/lib/errors'
import { requireRole } from '@/lib/guards'
import { useUsers } from '@/lib/users'
import {
  OVERFLOW_TYPES, STRATEGIES, catalogApi, queueAgentsKey, useCatalogMutations,
  useQueueAgents, useQueues,
  type OverflowType, type Queue, type QueueDraft, type Strategy,
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
  const [editing, setEditing] = useRecordForm<QueueDraft>(saveQueue)
  const [staffing, setStaffing] = useState<Queue | null>(null)

  const rows = data?.items ?? []

  // One roster request per queue. A queue with nobody on it routes calls to
  // nobody, which is the kind of thing a list should show without being asked.
  const rosters = useQueries({
    queries: rows.map((queue) => ({
      queryKey: queueAgentsKey(queue.id),
      queryFn: () => catalogApi.queueAgents(queue.id),
    })),
  })

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
          <Th align="right">{t('supervisor.staffed')}</Th>
          <Th>{t('admin.maxWait')}</Th>
          <Th>{t('admin.overflow')}</Th>
          <Th>{t('admin.recording')}</Th>
          <Th align="right">{t('supervisor.actions')}</Th>
        </THead>
        <TBody>
          {isPending && <TableMessage colSpan={8}>{t('common.loading')}</TableMessage>}
          {isError && <TableMessage colSpan={8}>{describeError(error, t)}</TableMessage>}
          {!isPending && rows.length === 0 && (
            <TableMessage colSpan={8}>{t('admin.noQueues')}</TableMessage>
          )}
          {rows.map((row, index) => (
            <Tr key={row.id}>
              <Td>
                <span className="font-medium">{row.displayName}</span>
                <span className="ml-2 text-xs text-muted-foreground">{row.name}</span>
              </Td>
              <Td className="tabular">{row.extNumber}</Td>
              <Td className="text-xs text-muted-foreground">
                {t(`admin.strategies.${row.strategy}`)}
              </Td>
              <Td align="right" className="tabular">
                {rosters[index]?.data?.items.length ?? '—'}
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
                <Button
                  size="icon-sm"
                  variant="ghost"
                  title={t('admin.staff')}
                  onClick={() => setStaffing(row)}
                >
                  <Users />
                </Button>
                <Button
                  size="icon-sm"
                  variant="ghost"
                  title={t('common.edit')}
                  onClick={() => setEditing(row)}
                >
                  <Pencil />
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

      {staffing && (
        <StaffingDialog queue={staffing} onClose={() => setStaffing(null)} />
      )}

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
          {/* Allocated, not typed: whoever adds a queue is asking for a
              queue, not for 7004. Shown once it exists, and never editable —
              the switch, the dialplan and every routed call know it by that
              number. */}
          {editing.id && (
            <Field label={t('admin.queueExtension')} hint={t('admin.queueExtensionHint')}>
              <Input disabled value={editing.extNumber ?? ''} />
            </Field>
          )}
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

/**
 * Who a queue sends its callers to.
 *
 * Not part of the queue form: staffing is written the moment it is chosen,
 * because a tier reaches the switch on its own — putting it behind the form's
 * Save would promise a transaction that does not exist. So this is a panel
 * with a Close, not a form with a Save.
 *
 * The candidates are accounts that have an agent identity. An account without
 * one cannot be staffed at all, so it is not offered rather than offered and
 * refused.
 */
function StaffingDialog({ queue, onClose }: { queue: Queue; onClose: () => void }) {
  const { t } = useTranslation()
  const roster = useQueueAgents(queue.id)
  const users = useUsers()
  const { staffQueue, unstaffQueue } = useCatalogMutations()
  const [picked, setPicked] = useState('')

  const staffed = roster.data?.items ?? []
  const onQueue = new Set(staffed.map((a) => a.agentId))
  const candidates = (users.data?.items ?? [])
    .filter((u) => u.agentId && !onQueue.has(u.agentId))
    .map((u) => ({ value: u.agentId as string, label: `${u.displayName} (${u.username})` }))

  const busy = staffQueue.isPending || unstaffQueue.isPending
  const failure = staffQueue.error ?? unstaffQueue.error

  return (
    <Dialog.Root open onOpenChange={(open) => !open && onClose()}>
      <Dialog.Portal>
        <Dialog.Overlay className="fixed inset-0 z-40 bg-black/20" />
        <Dialog.Content className="fixed left-1/2 top-1/2 z-50 w-[440px] -translate-x-1/2 -translate-y-1/2 rounded-md border bg-card p-4 shadow-md">
          <Dialog.Title className="text-base font-medium">
            {t('admin.staffTitle', { name: queue.displayName || queue.name })}
          </Dialog.Title>
          <p className="mt-1 text-xs text-muted-foreground">{t('admin.staffHint')}</p>

          <div className="mt-4 flex items-baseline justify-between">
            <span className="text-xs uppercase tracking-wide text-muted-foreground">
              {t('admin.staffedAgents')}
            </span>
            <span className="tabular text-xs text-muted-foreground">{staffed.length}</span>
          </div>

          <div className="mt-1 max-h-[280px] overflow-y-auto rounded-md border">
            {roster.isPending && (
              <p className="px-2 py-2 text-xs text-muted-foreground">{t('common.loading')}</p>
            )}
            {!roster.isPending && staffed.length === 0 && (
              <p className="px-2 py-2 text-xs text-muted-foreground">{t('admin.noneStaffed')}</p>
            )}
            {staffed.map((agent) => (
              <div
                key={agent.agentId}
                className="flex h-9 items-center justify-between border-b px-2 last:border-b-0"
              >
                <span className="truncate">{agent.displayName}</span>
                <Button
                  size="icon-sm"
                  variant="ghost"
                  disabled={busy}
                  title={t('admin.unstaff')}
                  onClick={() =>
                    unstaffQueue.mutate({ queueID: queue.id, agentID: agent.agentId })
                  }
                >
                  <X />
                </Button>
              </div>
            ))}
          </div>

          <div className="mt-3 flex items-center gap-2">
            <span className="flex-1">
              <Select
                value={picked}
                ariaLabel={t('admin.addStaff')}
                disabled={busy || candidates.length === 0}
                onChange={setPicked}
                options={[
                  {
                    value: '',
                    label: candidates.length === 0 ? t('admin.allStaffed') : t('admin.addStaff'),
                  },
                  ...candidates,
                ]}
              />
            </span>
            <Button
              size="sm"
              variant="outline"
              disabled={busy || !picked}
              onClick={() =>
                staffQueue.mutate(
                  // Appended rather than inserted: with TOP_DOWN the position
                  // is the order callers are offered in, and everyone sharing
                  // position 1 leaves that order to the database.
                  { queueID: queue.id, agentID: picked, position: staffed.length + 1 },
                  { onSuccess: () => setPicked('') },
                )
              }
            >
              <Plus />
              {t('admin.staffAdd')}
            </Button>
          </div>

          {failure && (
            <p role="alert" className="mt-2 text-xs" style={{ color: 'var(--state-breach)' }}>
              {describeError(failure, t)}
            </p>
          )}

          <div className="mt-3 flex justify-end">
            <Dialog.Close asChild>
              <Button size="sm" variant="ghost">
                {t('common.close')}
              </Button>
            </Dialog.Close>
          </div>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  )
}
