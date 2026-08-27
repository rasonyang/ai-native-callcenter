import { createFileRoute } from '@tanstack/react-router'
import { useTranslation } from 'react-i18next'
import { useRef, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { KeyRound, Trash2 } from 'lucide-react'
import { Dialog, Popover } from 'radix-ui'

import { PageHeader } from '@/components/page-header'
import { Field, Input, RecordDialog, Select } from '@/components/record-dialog'
import { DataTable, TBody, THead, TableMessage, Td, Th, Tr } from '@/components/table'
import { Button } from '@/components/ui/button'
import { describeError } from '@/lib/errors'
import { useUsers } from '@/lib/users'
import { requireRole } from '@/lib/guards'
import {
  catalogApi, generateSIPPassword, useCatalogMutations, useExtensions,
  type Extension, type ExtensionDraft,
} from '@/lib/catalog'

/** The SIP endpoints the switch will accept a registration for. */
export const Route = createFileRoute('/_app/admin/extensions')({
  beforeLoad: ({ context }) => requireRole(context.user, 'ADMIN'),
  component: ExtensionsPage,
})

function ExtensionsPage() {
  const { t } = useTranslation()
  const { data, isPending, isError, error } = useExtensions()
  const { saveExtension, deleteExtension } = useCatalogMutations()
  const users = useUsers()
  const [editing, setEditing] = useState<ExtensionDraft | null>(null)
  const [revealing, setRevealing] = useState<Extension | null>(null)
  // The freshly minted password lives here and nowhere else: not in form
  // state, not in an input's value, not in anything a draft or a devtools
  // panel would carry. State holds only whether one exists.
  const minted = useRef<string | null>(null)
  const [hasMinted, setHasMinted] = useState(false)

  const rows = data?.items ?? []

  // Who each phone belongs to. The extension carries the agent's id; the name
  // an operator recognises lives on the account.
  const ownerOf = (agentId?: string) => {
    if (!agentId) return undefined
    const account = (users.data?.items ?? []).find((u) => u.agentId === agentId)
    return account ? `${account.displayName} (${account.username})` : undefined
  }


  return (
    <>
      <PageHeader
        title={t('nav.extensions')}
        description={t('admin.extensionsHint')}
        actions={undefined}
      />

      <DataTable>
        <THead>
          <Th>{t('admin.number')}</Th>
          <Th>{t('admin.displayName')}</Th>
          <Th>{t('admin.assignedTo')}</Th>
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
              <Td className="text-muted-foreground">
                {/* Who sits here. Named rather than "assigned": the question
                    an operator brings to this table is whose phone 1008 is,
                    and a yes does not answer it. A phone arrives with its
                    account now, so an unassigned one is one whose account was
                    deleted — worth seeing, not worth hiding behind a blank. */}
                {ownerOf(row.agentId) ?? (
                  <span className="text-xs">{t('admin.unassigned')}</span>
                )}
              </Td>
              <Td className="text-xs text-muted-foreground">
                {row.isEnabled ? t('common.yes') : t('common.no')}
              </Td>
              <Td align="right">
                {/* Laid out rather than left to inline baselines, which put
                    the middle button a few pixels below its neighbours. */}
                <span className="flex items-center justify-end gap-1">
                  <Button size="sm" variant="ghost" onClick={() => setRevealing(row)}>
                    <KeyRound />
                    {t('admin.reveal')}
                  </Button>
                  <Button size="sm" variant="ghost" onClick={() => setEditing(row)}>
                    {t('common.edit')}
                  </Button>
                  <ConfirmDelete
                    label={t('admin.deleteExtensionConfirm', { number: row.number })}
                    onConfirm={() => deleteExtension.mutate(row.id)}
                  />
                </span>
              </Td>
            </Tr>
          ))}
        </TBody>
      </DataTable>

      {editing && (
        <RecordDialog
          open
          onOpenChange={(open) => {
            if (open) return
            // Leaving the form forgets it: shown once, and once only.
            minted.current = null
            setHasMinted(false)
            setEditing(null)
          }}
          title={editing.id ? t('admin.editExtension') : t('admin.addExtension')}
          isSaving={saveExtension.isPending}
          error={saveExtension.isError ? describeError(saveExtension.error, t) : undefined}
          onSubmit={() =>
            // The secret joins the payload here and only here.
            saveExtension.mutate(
              { ...editing, password: minted.current ?? undefined },
              {
                onSuccess: () => {
                  minted.current = null
                  setHasMinted(false)
                  setEditing(null)
                },
              },
            )
          }
        >
          <Field label={t('admin.displayName')}>
            <Input
              value={editing.displayName ?? ''}
              onChange={(e) => setEditing({ ...editing, displayName: e.target.value })}
            />
          </Field>
          {/* One field for both cases. A minted secret looks the same whether
              it was generated for a new phone or reset on an existing one —
              and it has to be shown either way: a reset that draws nothing
              reads as a dead button, while the credential it staged is real
              and lands on the next save. */}
          <Field
            label={t('admin.password')}
            hint={
              hasMinted
                ? editing.id
                  ? t('admin.passwordMintedHint')
                  : t('admin.passwordNewHint')
                : editing.id
                  ? t('admin.passwordExistingHint')
                  : t('admin.passwordNewHint')
            }
          >
            {hasMinted ? (
              <span className="flex items-center gap-2">
                <code className="flex-1 truncate rounded-md border bg-background px-2 py-1.5 font-mono text-sm">
                  {minted.current}
                </code>
                <CopyButton key={minted.current} value={minted.current ?? ''} />
              </span>
            ) : editing.id ? (
              <span className="flex gap-2">
                <Button
                  type="button"
                  size="sm"
                  variant="outline"
                  onClick={() => setRevealing(rows.find((e) => e.id === editing.id) ?? null)}
                >
                  <KeyRound />
                  {t('admin.reveal')}
                </Button>
                <Button
                  type="button"
                  size="sm"
                  variant="outline"
                  onClick={() => {
                    minted.current = generateSIPPassword()
                    setHasMinted(true)
                  }}
                >
                  {t('admin.resetPassword')}
                </Button>
              </span>
            ) : (
              <Button
                type="button"
                size="sm"
                variant="outline"
                onClick={() => {
                  minted.current = generateSIPPassword()
                  setHasMinted(true)
                }}
              >
                {t('admin.generate')}
              </Button>
            )}
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
 * Puts a secret on the clipboard and says so.
 *
 * The confirmation is the whole point: a credential copies silently, so a
 * button that does not change is indistinguishable from one that did nothing,
 * and the reader's next move is to select the text by hand. It stays
 * confirmed — re-copying the same string has nothing new to report.
 */
function CopyButton({ value }: { value: string }) {
  const { t } = useTranslation()
  const [copied, setCopied] = useState(false)
  return (
    <Button
      type="button"
      size="sm"
      variant="outline"
      disabled={!value}
      onClick={() => {
        if (!value) return
        void navigator.clipboard.writeText(value)
        setCopied(true)
      }}
    >
      {copied ? t('admin.copied') : t('admin.copy')}
    </Button>
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
            <CopyButton key={data?.password} value={data?.password ?? ''} />
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
