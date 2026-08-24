import { createFileRoute } from '@tanstack/react-router'
import { useTranslation } from 'react-i18next'
import { useRef, useState } from 'react'
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
  catalogApi, generateSIPPassword, useCatalogMutations, useExtensions, useQueues,
  type Extension, type ExtensionDraft, type ExtensionKind,
} from '@/lib/catalog'
import { useUsers } from '@/lib/users'

/** The SIP endpoints the switch will accept a registration for. */
export const Route = createFileRoute('/_app/admin/extensions')({
  beforeLoad: ({ context }) => requireRole(context.user, 'ADMIN'),
  component: ExtensionsPage,
})

const KINDS: ExtensionKind[] = ['AGENT', 'QUEUE']

/**
 * Which target a kind carries. Two kinds, because two is what an extension can
 * be: somebody's phone, or a way into a queue. BOT and PLAIN were retired —
 * PLAIN said what an unbound AGENT already says, and a BOT extension routed
 * nothing, since a caller reaches a bot through the DID that names its flow.
 */
const TARGET_OF: Record<ExtensionKind, 'agent' | 'queue'> = {
  AGENT: 'agent',
  QUEUE: 'queue',
}

/** The lowest number not already taken — the same rule the server allocates by. */
function nextFreeNumber(taken: Set<string>, low: number, high: number): string {
  for (let n = low; n <= high; n++) {
    if (!taken.has(String(n))) return String(n)
  }
  return ''
}

const POOL_LOW = 1000
const POOL_HIGH = 1999

/**
 * What is wrong with the number being typed, said while it is being typed.
 *
 * Out of range and already taken are both refusals the server would make on
 * submit; finding out then means retyping a form that looked finished.
 */
function numberProblem(
  editing: ExtensionDraft | null,
  taken: Set<string>,
  rows: Extension[],
): 'range' | 'taken' | null {
  if (!editing || editing.id) return null
  const number = (editing.number ?? '').trim()
  if (number === '') return null
  const n = Number(number)
  if (!/^\d+$/.test(number) || n < POOL_LOW || n > POOL_HIGH) return 'range'
  if (taken.has(number) && !rows.some((e) => e.id === editing.id)) return 'taken'
  return null
}

function ExtensionsPage() {
  const { t } = useTranslation()
  const { data, isPending, isError, error } = useExtensions()
  const { saveExtension, deleteExtension } = useCatalogMutations()
  const [editing, setEditing] = useState<ExtensionDraft | null>(null)
  const [revealing, setRevealing] = useState<Extension | null>(null)
  // The freshly minted password lives here and nowhere else: not in form
  // state, not in an input's value, not in anything a draft or a devtools
  // panel would carry. State holds only whether one exists.
  const minted = useRef<string | null>(null)
  const [hasMinted, setHasMinted] = useState(false)
  const queues = useQueues()
  const users = useUsers()

  const rows = data?.items ?? []
  const taken = new Set(rows.map((e) => e.number))
  const numberError = numberProblem(editing, taken, rows)

  // An account may hold one phone, so the picker offers the ones that have
  // none — the collision is shown before the form is sent, not after.
  const boundElsewhere = new Set(
    rows.filter((e) => e.id !== editing?.id && e.agentId).map((e) => e.agentId as string),
  )
  const freeAgents = (users.data?.items ?? []).filter(
    (u) => u.agentId && !boundElsewhere.has(u.agentId),
  )

  return (
    <>
      <PageHeader
        title={t('nav.extensions')}
        description={t('admin.extensionsHint')}
        actions={
          <Button
            size="sm"
            onClick={() => {
              minted.current = null
              setHasMinted(false)
              setEditing({
                kind: 'AGENT',
                isEnabled: true,
                // Prefilled, still editable: the operator is usually asking
                // for the next desk, not for a particular number.
                number: nextFreeNumber(new Set(rows.map((e) => e.number)), POOL_LOW, POOL_HIGH),
              })
            }}
          >
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
          <Field
            label={t('admin.number')}
            hint={
              numberError === 'range'
                ? t('admin.numberOutOfRange', { low: POOL_LOW, high: POOL_HIGH })
                : numberError === 'taken'
                  ? t('admin.numberTaken')
                  : undefined
            }
          >
            <Input
              autoFocus
              disabled={Boolean(editing.id)}
              aria-invalid={numberError !== null}
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
          <Field label={t('admin.kind')} hint={t(`admin.kindHints.${editing.kind ?? 'AGENT'}`)}>
            <Select
              value={editing.kind ?? 'AGENT'}
              onChange={(next) => {
                const kind = next as ExtensionKind
                // Changing what a number serves drops the target it served
                // before — a leftover one is a claim nothing honours. Asked
                // about once, because it is the operator's work being thrown
                // away, not ours.
                const hadTarget = Boolean(editing.agentId ?? editing.queueId)
                if (hadTarget && TARGET_OF[kind] !== TARGET_OF[editing.kind ?? 'AGENT']) {
                  if (!window.confirm(t('admin.kindChangeClearsTarget'))) return
                }
                setEditing({ ...editing, kind, agentId: undefined, queueId: undefined })
              }}
              options={KINDS.map((k) => ({ value: k, label: t(`admin.kinds.${k}`) }))}
            />
          </Field>

          {/* One selector, chosen by kind. PLAIN shows none, which is the
              whole of what PLAIN means. */}
          {TARGET_OF[editing.kind ?? 'AGENT'] === 'agent' && (
            <Field label={t('admin.targetAgent')} hint={t('admin.targetAgentHint')}>
              <Select
                value={editing.agentId ?? ''}
                onChange={(agentId) => setEditing({ ...editing, agentId: agentId || undefined })}
                options={[
                  { value: '', label: t('admin.targetNone') },
                  ...freeAgents.map((u) => ({
                    value: u.agentId as string,
                    label: `${u.displayName} (${u.username})`,
                  })),
                ]}
              />
            </Field>
          )}
          {TARGET_OF[editing.kind ?? 'AGENT'] === 'queue' && (
            <Field label={t('admin.targetQueue')}>
              <Select
                value={editing.queueId ?? ''}
                onChange={(queueId) => setEditing({ ...editing, queueId: queueId || undefined })}
                options={[
                  { value: '', label: t('admin.targetNone') },
                  ...(queues.data?.items ?? []).map((q) => ({ value: q.id, label: q.displayName })),
                ]}
              />
            </Field>
          )}
          {/* Three states, and none of them renders a placeholder cipher into
              an input. A row of dots in a password box is a lie about what is
              there, and the browser will happily submit it back.

              New: generate, then show the plaintext once, beside a Copy. It is
              held in a ref rather than form state so it is never part of what
              a re-render, a devtools panel or a serialized draft can carry.

              Existing: no field at all — Copy reads it through the audited
              endpoint, Reset replaces it. */}
          {editing.id ? (
            <Field label={t('admin.password')} hint={t('admin.passwordExistingHint')}>
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
            </Field>
          ) : (
            <Field label={t('admin.password')} hint={t('admin.passwordNewHint')}>
              {hasMinted ? (
                <span className="flex items-center gap-2">
                  <code className="flex-1 truncate rounded-md border bg-background px-2 py-1.5 font-mono text-sm">
                    {minted.current}
                  </code>
                  <Button
                    type="button"
                    size="sm"
                    variant="outline"
                    onClick={() => void navigator.clipboard.writeText(minted.current ?? '')}
                  >
                    {t('admin.copy')}
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
          )}
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
