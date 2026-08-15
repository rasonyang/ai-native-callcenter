import { createFileRoute } from '@tanstack/react-router'
import { useTranslation } from 'react-i18next'
import { useState } from 'react'
import { Plus } from 'lucide-react'

import { PageHeader } from '@/components/page-header'
import { Field, Input, RecordDialog, Select } from '@/components/record-dialog'
import { ConfirmDelete } from '@/routes/_app.admin.extensions'
import { DataTable, TBody, THead, TableMessage, Td, Th, Tr } from '@/components/table'
import { Button } from '@/components/ui/button'
import { useRoster } from '@/lib/agent'
import { useAgentAdminMutations, useUsers, type AgentDraft } from '@/lib/agents-admin'
import { useExtensions } from '@/lib/catalog'
import { describeError } from '@/lib/errors'
import { requireRole } from '@/lib/guards'
import type { RosterEntry } from '@/lib/api'

/**
 * Who is an agent, and which phone they are bound to.
 *
 * The binding is static configuration: an agent signs in at this extension and
 * nowhere else, and no two agents may share one.
 */
export const Route = createFileRoute('/_app/admin/agents')({
  beforeLoad: ({ context }) => requireRole(context.user, 'ADMIN'),
  component: AgentsAdmin,
})

function AgentsAdmin() {
  const { t } = useTranslation()
  const { data, isPending } = useRoster(true)
  const users = useUsers()
  const extensions = useExtensions()
  const { save, remove } = useAgentAdminMutations()
  const [editing, setEditing] = useState<AgentDraft | null>(null)

  const agents = data?.items ?? []
  // An account may hold only one agent identity, so the picker offers the
  // accounts that do not have one yet.
  const claimed = new Set(agents.map((a) => a.userId))
  const freeUsers = (users.data?.items ?? []).filter((u) => !claimed.has(u.userId))

  // An extension belongs to at most one agent; the one being edited keeps its
  // own so it does not vanish from its own picker.
  const boundElsewhere = new Set(
    agents
      .filter((a) => a.agentId !== editing?.agentId && a.defaultExtensionId)
      .map((a) => a.defaultExtensionId as string),
  )
  const freeExtensions = (extensions.data?.items ?? []).filter(
    (e) => e.kind === 'AGENT' && !boundElsewhere.has(e.id),
  )

  return (
    <>
      <PageHeader
        title={t('nav.agentIdentities')}
        description={t('admin.agentsHint')}
        actions={
          <Button
            size="lg"
            disabled={freeUsers.length === 0}
            title={freeUsers.length === 0 ? t('admin.noFreeUsers') : undefined}
            onClick={() => setEditing({ wrapUpTimeSec: 30, isAutoAnswer: false })}
          >
            <Plus />
            {t('admin.addAgent')}
          </Button>
        }
      />

      <DataTable>
        <THead>
          <Th>{t('supervisor.name')}</Th>
          <Th>{t('admin.callcenterName')}</Th>
          <Th>{t('admin.boundExtension')}</Th>
          <Th align="right">{t('admin.wrapUpTime')}</Th>
          <Th>{t('admin.autoAnswer')}</Th>
          <Th align="right">{t('supervisor.actions')}</Th>
        </THead>
        <TBody>
          {isPending && <TableMessage colSpan={6}>{t('common.loading')}</TableMessage>}
          {!isPending && agents.length === 0 && (
            <TableMessage colSpan={6}>{t('supervisor.noAgents')}</TableMessage>
          )}
          {agents.map((agent) => (
            <AgentRow
              key={agent.agentId}
              agent={agent}
              onEdit={() =>
                setEditing({
                  agentId: agent.agentId,
                  userId: agent.userId,
                  callcenterName: agent.callcenterName,
                  wrapUpTimeSec: agent.wrapUpTimeSec,
                  isAutoAnswer: agent.isAutoAnswer,
                  defaultExtensionId: agent.defaultExtensionId,
                })
              }
              onDelete={() => remove.mutate(agent.agentId)}
            />
          ))}
        </TBody>
      </DataTable>

      {editing && (
        <RecordDialog
          open
          onOpenChange={(open) => !open && setEditing(null)}
          title={editing.agentId ? t('admin.editAgent') : t('admin.addAgent')}
          isSaving={save.isPending}
          error={save.isError ? describeError(save.error, t) : undefined}
          onSubmit={() =>
            save.mutate(editing, { onSuccess: () => setEditing(null) })
          }
        >
          {!editing.agentId && (
            <Field label={t('admin.account')} hint={t('admin.accountHint')}>
              <Select
                value={editing.userId ?? ''}
                onChange={(userId) => setEditing({ ...editing, userId })}
                ariaLabel={t('admin.account')}
                options={[
                  { value: '', label: t('admin.chooseAccount') },
                  ...freeUsers.map((u) => ({
                    value: u.userId,
                    label: `${u.displayName} (${u.username})`,
                  })),
                ]}
              />
            </Field>
          )}
          <Field label={t('admin.callcenterName')} hint={t('admin.callcenterNameHint')}>
            <Input
              value={editing.callcenterName ?? ''}
              onChange={(e) => setEditing({ ...editing, callcenterName: e.target.value })}
            />
          </Field>
          <Field label={t('admin.boundExtension')} hint={t('admin.boundExtensionHint')}>
            <Select
              value={editing.defaultExtensionId ?? ''}
              onChange={(id) =>
                setEditing({ ...editing, defaultExtensionId: id === '' ? undefined : id })
              }
              ariaLabel={t('admin.boundExtension')}
              options={[
                { value: '', label: t('admin.noExtension') },
                ...freeExtensions.map((e) => ({
                  value: e.id,
                  label: `${e.number} · ${e.displayName}`,
                })),
              ]}
            />
          </Field>
          <Field label={t('admin.wrapUpTime')} hint={t('admin.wrapUpTimeHint')}>
            <Input
              type="number"
              min={0}
              value={editing.wrapUpTimeSec ?? 30}
              onChange={(e) =>
                setEditing({ ...editing, wrapUpTimeSec: Number(e.target.value) })
              }
            />
          </Field>
          <Field label={t('admin.autoAnswer')} hint={t('admin.autoAnswerHint')}>
            <Select
              value={editing.isAutoAnswer ? 'yes' : 'no'}
              onChange={(v) => setEditing({ ...editing, isAutoAnswer: v === 'yes' })}
              ariaLabel={t('admin.autoAnswer')}
              options={[
                { value: 'no', label: t('common.no') },
                { value: 'yes', label: t('common.yes') },
              ]}
            />
          </Field>
        </RecordDialog>
      )}
    </>
  )
}

function AgentRow({
  agent,
  onEdit,
  onDelete,
}: {
  agent: RosterEntry
  onEdit: () => void
  onDelete: () => void
}) {
  const { t } = useTranslation()
  return (
    <Tr>
      <Td>
        <span className="font-medium">{agent.displayName}</span>
        <span className="ml-2 text-xs text-muted-foreground">{agent.username}</span>
      </Td>
      <Td className="text-muted-foreground">{agent.callcenterName}</Td>
      <Td className="tabular">
        {agent.defaultExtensionNumber || (
          // An agent without a phone cannot sign in at all, so it is called out
          // rather than left as an empty cell.
          <span style={{ color: 'var(--state-breach)' }}>{t('admin.noExtension')}</span>
        )}
      </Td>
      <Td align="right" className="tabular">
        {agent.wrapUpTimeSec}s
      </Td>
      <Td>{agent.isAutoAnswer ? t('common.yes') : t('common.no')}</Td>
      <Td align="right">
        <div className="flex items-center justify-end gap-1">
          <Button size="sm" variant="ghost" onClick={onEdit}>
            {t('common.edit')}
          </Button>
          <ConfirmDelete
            label={t('admin.deleteAgentConfirm', { name: agent.displayName })}
            onConfirm={onDelete}
          />
        </div>
      </Td>
    </Tr>
  )
}
