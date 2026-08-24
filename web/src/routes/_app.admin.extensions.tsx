import { createFileRoute } from '@tanstack/react-router'
import { useTranslation } from 'react-i18next'
import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { KeyRound, Plus, Trash2 } from 'lucide-react'
import { Dialog, Popover } from 'radix-ui'

import { PageHeader } from '@/components/page-header'
import { Field, Input, RecordDialog, Select } from '@/components/record-dialog'
import { DataTable, TBody, THead, TableMessage, Td, Th, Tr } from '@/components/table'
import { Button } from '@/components/ui/button'
import { describeError } from '@/lib/errors'
import { requireRole } from '@/lib/guards'
import {
  catalogApi, generateSIPPassword, useCatalogMutations, useExtensions,
  type Extension, type ExtensionDraft, type ExtensionKind,
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
  const [revealing, setRevealing] = useState<Extension | null>(null)

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
                {row.kind === 'AGENT' && (
                  <Button size="sm" variant="ghost" onClick={() => setRevealing(row)}>
                    <KeyRound />
                    {t('admin.reveal')}
                  </Button>
                )}
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
            {/* Generated rather than invented: a credential somebody types
                from memory is the one that ends up being 1234, and nothing
                downstream ever asks how it was chosen. */}
            <span className="flex gap-2">
              <Input
                type="password"
                autoComplete="new-password"
                className="flex-1"
                value={editing.password ?? ''}
                onChange={(e) => setEditing({ ...editing, password: e.target.value })}
              />
              <Button
                type="button"
                size="sm"
                variant="outline"
                onClick={() => setEditing({ ...editing, password: generateSIPPassword() })}
              >
                {t('admin.generate')}
              </Button>
            </span>
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
      {revealing && (
        <RevealPassword extension={revealing} onClose={() => setRevealing(null)} />
      )}
    </>
  )
}

/**
 * What a phone was given, shown once and on request.
 *
 * Fetched only when asked, never carried by the list: the read is recorded in
 * the audit trail, and a credential that arrives with every page load could
 * not be. Stored in clear on purpose (D4) — the a1-hash alternative is bound
 * to the SIP realm, this deployment's realm follows the host address, and that
 * address has already moved twice.
 */
function RevealPassword({
  extension,
  onClose,
}: {
  extension: Extension
  onClose: () => void
}) {
  const { t } = useTranslation()
  const [copied, setCopied] = useState(false)
  const { data, isPending, isError, error } = useQuery({
    queryKey: ['catalog', 'extensions', extension.id, 'password'],
    queryFn: () => catalogApi.extensionPassword(extension.id),
    // Not cached: the next reveal should be a fresh read, so that every
    // disclosure leaves its own row in the trail.
    gcTime: 0,
    staleTime: 0,
  })

  return (
    <Dialog.Root open onOpenChange={(open) => !open && onClose()}>
      <Dialog.Portal>
        <Dialog.Overlay className="fixed inset-0 z-40 bg-black/20" />
        <Dialog.Content className="fixed left-1/2 top-1/2 z-50 w-[440px] -translate-x-1/2 -translate-y-1/2 rounded-md border bg-card p-4 shadow-md">
          <Dialog.Title className="text-base font-medium">
            {t('admin.revealTitle', { number: extension.number })}
          </Dialog.Title>
          <p className="mt-1 text-xs text-muted-foreground">{t('admin.revealHint')}</p>

          <div className="mt-3 flex items-center gap-2">
            <code className="flex-1 truncate rounded-md border bg-background px-2 py-1.5 font-mono text-sm">
              {isPending
                ? t('common.loading')
                : isError
                  ? describeError(error, t)
                  : data?.password}
            </code>
            <Button
              size="sm"
              variant="outline"
              disabled={!data?.password}
              onClick={() => {
                if (!data?.password) return
                void navigator.clipboard.writeText(data.password)
                setCopied(true)
              }}
            >
              {copied ? t('admin.copied') : t('admin.copy')}
            </Button>
          </div>

          <div className="mt-3 flex justify-end">
            <Dialog.Close asChild>
              <Button size="sm" variant="ghost">
                {t('common.cancel')}
              </Button>
            </Dialog.Close>
          </div>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
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
