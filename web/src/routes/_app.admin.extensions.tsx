import { createFileRoute } from '@tanstack/react-router'
import { useTranslation } from 'react-i18next'
import { useState } from 'react'
import { Plus, Trash2 } from 'lucide-react'
import { Popover } from 'radix-ui'

import { PageHeader } from '@/components/page-header'
import { Field, Input, RecordDialog, Select } from '@/components/record-dialog'
import { DataTable, TBody, THead, TableMessage, Td, Th, Tr } from '@/components/table'
import { Button } from '@/components/ui/button'
import { describeError } from '@/lib/errors'
import { requireRole } from '@/lib/guards'
import {
  useCatalogMutations, useExtensions, type ExtensionDraft, type ExtensionKind,
} from '@/lib/catalog'

/** The SIP endpoints the switch will accept a registration for. */
export const Route = createFileRoute('/_app/admin/extensions')({
  beforeLoad: ({ context }) => requireRole(context.user, 'ADMIN'),
  component: ExtensionsPage,
})

const KINDS: ExtensionKind[] = ['AGENT', 'BOT', 'PLAIN']

function ExtensionsPage() {
  const { t } = useTranslation()
  const { data, isPending, isError, error } = useExtensions()
  const { saveExtension, deleteExtension } = useCatalogMutations()
  const [editing, setEditing] = useState<ExtensionDraft | null>(null)

  const rows = data?.items ?? []

  return (
    <>
      <PageHeader
        title={t('nav.extensions')}
        description={t('admin.extensionsHint')}
        actions={
          <Button size="sm" onClick={() => setEditing({ kind: 'AGENT', isEnabled: true })}>
            <Plus />
            {t('admin.addExtension')}
          </Button>
        }
      />

      <DataTable>
        <THead>
          <Th>{t('admin.number')}</Th>
          <Th>{t('admin.displayName')}</Th>
          <Th>{t('admin.kind')}</Th>
          <Th>{t('admin.enabled')}</Th>
          <Th align="right">{t('supervisor.actions')}</Th>
        </THead>
        <TBody>
          {isPending && <TableMessage colSpan={5}>{t('common.loading')}</TableMessage>}
          {isError && <TableMessage colSpan={5}>{describeError(error, t)}</TableMessage>}
          {!isPending && rows.length === 0 && (
            <TableMessage colSpan={5}>{t('admin.noExtensions')}</TableMessage>
          )}
          {rows.map((row) => (
            <Tr key={row.id}>
              <Td className="tabular font-medium">{row.number}</Td>
              <Td>{row.displayName}</Td>
              <Td className="text-xs text-muted-foreground">{t(`admin.kinds.${row.kind}`)}</Td>
              <Td className="text-xs text-muted-foreground">
                {row.isEnabled ? t('common.yes') : t('common.no')}
              </Td>
              <Td align="right">
                <Button size="sm" variant="ghost" onClick={() => setEditing(row)}>
                  {t('common.edit')}
                </Button>
                <ConfirmDelete
                  label={t('admin.deleteExtensionConfirm', { number: row.number })}
                  onConfirm={() => deleteExtension.mutate(row.id)}
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
          title={editing.id ? t('admin.editExtension') : t('admin.addExtension')}
          isSaving={saveExtension.isPending}
          error={saveExtension.isError ? describeError(saveExtension.error, t) : undefined}
          onSubmit={() =>
            saveExtension.mutate(editing, { onSuccess: () => setEditing(null) })
          }
        >
          <Field label={t('admin.number')}>
            <Input
              autoFocus
              disabled={Boolean(editing.id)}
              value={editing.number ?? ''}
              onChange={(e) => setEditing({ ...editing, number: e.target.value })}
            />
          </Field>
          <Field label={t('admin.displayName')}>
            <Input
              value={editing.displayName ?? ''}
              onChange={(e) => setEditing({ ...editing, displayName: e.target.value })}
            />
          </Field>
          <Field label={t('admin.kind')}>
            <Select
              value={editing.kind ?? 'AGENT'}
              onChange={(kind) => setEditing({ ...editing, kind: kind as ExtensionKind })}
              options={KINDS.map((k) => ({ value: k, label: t(`admin.kinds.${k}`) }))}
            />
          </Field>
          <Field
            label={t('admin.password')}
            hint={editing.id ? t('admin.passwordUnchangedHint') : undefined}
          >
            <Input
              type="password"
              autoComplete="new-password"
              value={editing.password ?? ''}
              onChange={(e) => setEditing({ ...editing, password: e.target.value })}
            />
          </Field>
          <Field label={t('admin.enabled')}>
            <Select
              value={String(editing.isEnabled ?? true)}
              onChange={(v) => setEditing({ ...editing, isEnabled: v === 'true' })}
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

/** Deleting configuration is confirmed in place, never on a hover. */
export function ConfirmDelete({ label, onConfirm }: { label: string; onConfirm: () => void }) {
  const { t } = useTranslation()
  return (
    <Popover.Root>
      <Popover.Trigger asChild>
        <Button size="sm" variant="ghost" title={t('common.delete')}>
          <Trash2 />
        </Button>
      </Popover.Trigger>
      <Popover.Portal>
        <Popover.Content
          align="end"
          sideOffset={4}
          className="z-50 w-64 rounded-md border bg-popover p-3 text-sm shadow-md"
        >
          <p className="mb-3 text-xs text-muted-foreground">{label}</p>
          <div className="flex justify-end gap-2">
            <Popover.Close asChild>
              <Button size="sm" variant="ghost">
                {t('common.cancel')}
              </Button>
            </Popover.Close>
            <Popover.Close asChild>
              <Button size="sm" variant="destructive" onClick={onConfirm}>
                {t('common.delete')}
              </Button>
            </Popover.Close>
          </div>
        </Popover.Content>
      </Popover.Portal>
    </Popover.Root>
  )
}
