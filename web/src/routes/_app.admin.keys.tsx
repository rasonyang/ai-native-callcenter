import { createFileRoute } from '@tanstack/react-router'
import { useTranslation } from 'react-i18next'
import { useEffect, useMemo, useRef, useState } from 'react'
import { Ban, ChevronDown, ChevronRight, Pencil } from 'lucide-react'
import { Popover } from 'radix-ui'

import { PageHeader } from '@/components/page-header'
import { Field, Input, RecordDialog, useRecordForm } from '@/components/record-dialog'
import { DataTable, TBody, THead, TableMessage, Td, Th, Tr } from '@/components/table'
import { Button } from '@/components/ui/button'
import { describeError, fieldErrorText } from '@/lib/errors'
import { requireRole } from '@/lib/guards'
import {
  SCOPES, SCOPE_DESCRIPTIONS, useAPIKeyMutations, useAPIKeys,
  type APIKey, type Scope,
} from '@/lib/keys'

/**
 * The credentials a system authenticates with.
 *
 * A key is not a lesser login. It reaches the same API a person's session
 * does, holding exactly the capabilities it was issued with — so this page is
 * about scopes and about revocation, and about nothing else: there is no
 * password to reset, no "disabled" to toggle back, and no secret to look up.
 *
 * The secret exists once, in the response to the create. It is stored as a
 * digest, so nothing here, in the database, or in an administrator's hands can
 * produce it again — which is why the dialog that shows it says so, and why a
 * lost key is reissued rather than recovered.
 */
export const Route = createFileRoute('/_app/admin/keys')({
  beforeLoad: ({ context }) => requireRole(context.user, 'ADMIN'),
  component: KeysPage,
})

interface KeyDraft {
  id?: string
  name: string
  scopes: string[]
}

function KeysPage() {
  const { t, i18n } = useTranslation()
  const { data, isPending, isError, error } = useAPIKeys()
  const { createKey, updateKey, revokeKey } = useAPIKeyMutations()
  const [editing, setEditing] = useRecordForm<KeyDraft>(createKey, updateKey)
  // The freshly issued secret lives here and nowhere else: not in form state,
  // not in a query cache, not in anything a re-render would carry back after
  // the reader has dismissed it.
  const issued = useRef<{ name: string; secret: string } | null>(null)
  const [hasIssued, setHasIssued] = useState(false)

  const rows = data?.items ?? []
  const saving = createKey.isPending || updateKey.isPending
  const saveError = createKey.isError ? createKey.error : updateKey.isError ? updateKey.error : null

  const timeFormat = useMemo(
    () =>
      new Intl.DateTimeFormat(i18n.language, {
        year: 'numeric', month: 'short', day: 'numeric',
        hour: '2-digit', minute: '2-digit',
      }),
    [i18n.language],
  )
  const when = (iso?: string | null) => (iso ? timeFormat.format(new Date(iso)) : '—')

  return (
    <>
      <PageHeader
        title={t('keys.title')}
        description={t('keys.description')}
        actions={
          <Button size="sm" onClick={() => setEditing({ name: '', scopes: [] })}>
            {t('keys.issue')}
          </Button>
        }
      />

      <DataTable>
        <THead>
          <Th>{t('keys.name')}</Th>
          <Th>{t('keys.prefix')}</Th>
          <Th>{t('keys.scopes')}</Th>
          <Th>{t('keys.lastUsed')}</Th>
          <Th>{t('keys.status')}</Th>
          <Th align="right">{t('supervisor.actions')}</Th>
        </THead>
        <TBody>
          {isPending && <TableMessage colSpan={6}>{t('common.loading')}</TableMessage>}
          {isError && <TableMessage colSpan={6}>{describeError(error, t)}</TableMessage>}
          {!isPending && !isError && rows.length === 0 && (
            <TableMessage colSpan={6}>{t('keys.none')}</TableMessage>
          )}
          {rows.map((key) => (
            <Tr key={key.id}>
              <Td className="font-medium">{key.name}</Td>
              <Td className="font-mono text-xs text-muted-foreground">{key.keyPrefix}…</Td>
              <Td className="text-xs text-muted-foreground">
                {key.scopes.length === 0 ? t('keys.noScopes') : key.scopes.join(' ')}
              </Td>
              {/* Written on every authenticated request, so an operator
                  deciding whether to revoke is reading the truth rather than
                  a cached approximation. A refused request is not a use, so a
                  revoked key's time stops where it stopped working. */}
              <Td className="tabular text-xs text-muted-foreground">{when(key.lastUsedAt)}</Td>
              <Td>
                <StatusPill status={key.status} />
              </Td>
              <Td align="right">
                <span className="flex items-center justify-end gap-1">
                  <Button
                    size="icon-sm"
                    variant="ghost"
                    title={t('common.edit')}
                    disabled={key.status === 'REVOKED'}
                    onClick={() => setEditing({ id: key.id, name: key.name, scopes: key.scopes })}
                  >
                    <Pencil />
                  </Button>
                  {/* Revoked keys stay in the list: a revoked key is what an
                      audit row from last month refers to, and a list that hid
                      them would leave that row pointing at nothing. */}
                  {key.status === 'ENABLED' && (
                    <ConfirmRevoke
                      label={t('keys.revokeConfirm', { name: key.name })}
                      onConfirm={() => revokeKey.mutate(key.id)}
                    />
                  )}
                </span>
              </Td>
            </Tr>
          ))}
        </TBody>
      </DataTable>

      {revokeKey.isError && (
        <p role="alert" className="mt-3 text-xs" style={{ color: 'var(--state-breach)' }}>
          {describeError(revokeKey.error, t)}
        </p>
      )}

      {editing && (
        <RecordDialog
          open
          onOpenChange={(open) => {
            if (open) return
            issued.current = null
            setHasIssued(false)
            setEditing(null)
          }}
          title={editing.id ? t('keys.edit') : t('keys.issue')}
          isSaving={saving}
          error={saveError ? describeError(saveError, t) : undefined}
          submitLabel={editing.id ? undefined : t('keys.issue')}
          onSubmit={() => {
            if (editing.id) {
              updateKey.mutate(
                { id: editing.id, name: editing.name, scopes: editing.scopes },
                { onSuccess: () => setEditing(null) },
              )
              return
            }
            createKey.mutate(
              { name: editing.name, scopes: editing.scopes },
              {
                onSuccess: (created) => {
                  issued.current = { name: created.name, secret: created.secret }
                  setHasIssued(true)
                  setEditing(null)
                },
              },
            )
          }}
        >
          <Field
            label={t('keys.name')}
            hint={t('keys.nameHint')}
            error={fieldErrorText(saveError, 'name', t)}
          >
            <Input
              value={editing.name}
              onChange={(e) => setEditing({ ...editing, name: e.target.value })}
            />
          </Field>
          <Field
            label={t('keys.scopes')}
            hint={t('keys.scopesHint')}
            error={fieldErrorText(saveError, 'scopes', t)}
          >
            <ScopePicker
              selected={editing.scopes}
              onChange={(scopes) => setEditing({ ...editing, scopes })}
            />
          </Field>
        </RecordDialog>
      )}

      {hasIssued && issued.current && (
        <IssuedSecret
          name={issued.current.name}
          secret={issued.current.secret}
          onClose={() => {
            issued.current = null
            setHasIssued(false)
          }}
        />
      )}
    </>
  )
}

/**
 * The vocabulary, from the contract, grouped by the resource each scope names.
 *
 * Both the names and the sentences beside them are generated from
 * docs/openapi.json's `x-scopes` (web/src/generated/scopes.ts). Typing them out
 * here would be a second vocabulary that drifts the first time a scope is
 * added — and the day it drifts, this form offers capabilities the server does
 * not know and refuses the ones it does.
 *
 * The grouping is derived the same way, from the names themselves: a scope is
 * `resource:action[:range]` (docs/auth/TASKS.md §4 N1), so its first segment is
 * the resource and no list of groups has to be maintained here either. A
 * resource nobody has written a label for still gets a group, headed by its own
 * name — a new capability must never be one that quietly fails to appear.
 *
 * There is no select-all across groups, and that is deliberate. A key holding
 * every scope can issue further keys (keys:manage) and reset passwords
 * (users:write): it is the master credential this whole model exists to
 * replace, and putting it one click away would bring AICC_API_KEY back. Per
 * group is a different question — "does this integration touch calls at all?" —
 * and that one is worth answering in one click.
 */
function ScopePicker({
  selected,
  onChange,
}: {
  selected: string[]
  onChange: (next: string[]) => void
}) {
  const { t } = useTranslation()

  const groups = useMemo(() => {
    const byResource = new Map<string, Scope[]>()
    for (const scope of SCOPES) {
      const resource = scope.split(':')[0]
      byResource.set(resource, [...(byResource.get(resource) ?? []), scope])
    }
    return [...byResource].map(([resource, scopes]) => ({ resource, scopes }))
  }, [])

  // Open where there is something to see. A form being edited starts showing
  // what the key already holds; a new one starts closed, which is what turns
  // twenty rows into ten.
  const [open, setOpen] = useState<string[]>(() =>
    groups.filter((g) => g.scopes.some((s) => selected.includes(s))).map((g) => g.resource),
  )

  const setMany = (scopes: Scope[], checked: boolean) =>
    onChange(
      checked
        ? [...new Set([...selected, ...scopes])].toSorted()
        : selected.filter((s) => !scopes.includes(s as Scope)),
    )

  return (
    <div className="max-h-72 divide-y overflow-y-auto rounded-md border">
      {groups.map(({ resource, scopes }) => {
        const chosen = scopes.filter((s) => selected.includes(s))
        const isOpen = open.includes(resource)
        return (
          <div key={resource}>
            <div className="flex items-center gap-2 px-2 py-1.5">
              <button
                type="button"
                className="flex min-w-0 flex-1 items-center gap-1.5 text-left"
                aria-expanded={isOpen}
                onClick={() =>
                  setOpen(
                    isOpen ? open.filter((r) => r !== resource) : [...open, resource],
                  )
                }
              >
                {isOpen ? (
                  <ChevronDown className="size-3.5 shrink-0 text-muted-foreground" />
                ) : (
                  <ChevronRight className="size-3.5 shrink-0 text-muted-foreground" />
                )}
                <span className="font-mono text-xs">{resource}</span>
                {/* The label is UI copy for a grouping the names already imply,
                    so a resource with no wording falls back to its own name
                    rather than to a blank. */}
                <span className="truncate text-xs text-muted-foreground">
                  {t(`keys.resources.${resource}`, { defaultValue: '' })}
                </span>
                <span className="tabular ml-auto shrink-0 text-xs text-muted-foreground">
                  {chosen.length > 0 ? `${chosen.length}/${scopes.length}` : scopes.length}
                </span>
              </button>
              <GroupToggle
                label={t('keys.selectGroup', { resource })}
                checked={chosen.length === scopes.length}
                partial={chosen.length > 0 && chosen.length < scopes.length}
                onChange={(checked) => setMany(scopes, checked)}
              />
            </div>
            {isOpen && (
              <div className="space-y-1 border-t px-2 py-1.5">
                {scopes.map((scope) => (
                  <label
                    key={scope}
                    className="flex cursor-pointer items-start gap-2 rounded-sm p-1 hover:bg-muted"
                  >
                    <input
                      type="checkbox"
                      className="mt-0.5"
                      checked={selected.includes(scope)}
                      onChange={() => setMany([scope], !selected.includes(scope))}
                    />
                    <span className="min-w-0">
                      <span className="block font-mono text-xs">{scope}</span>
                      <span className="block text-xs text-muted-foreground">
                        {SCOPE_DESCRIPTIONS[scope]}
                      </span>
                    </span>
                  </label>
                ))}
              </div>
            )}
          </div>
        )
      })}
    </div>
  )
}

/**
 * A group's own checkbox, which is three-valued: none, some, all.
 *
 * "Some" has to look different from "none" or a collapsed group with two of
 * its six scopes ticked reads as untouched, and the reader opens it to find
 * out — which is the scanning this grouping exists to save.
 */
function GroupToggle({
  label,
  checked,
  partial,
  onChange,
}: {
  label: string
  checked: boolean
  partial: boolean
  onChange: (checked: boolean) => void
}) {
  const ref = useRef<HTMLInputElement>(null)
  useEffect(() => {
    if (ref.current) ref.current.indeterminate = partial
  }, [partial])

  return (
    <input
      ref={ref}
      type="checkbox"
      aria-label={label}
      title={label}
      className="shrink-0 cursor-pointer"
      checked={checked}
      onChange={(e) => onChange(e.target.checked)}
    />
  )
}

/** ENABLED authenticates; REVOKED is an ending. There is no third state. */
function StatusPill({ status }: { status: APIKey['status'] }) {
  const { t } = useTranslation()
  const enabled = status === 'ENABLED'
  return (
    <span
      className="inline-flex items-center gap-1.5 rounded-full border px-2 py-0.5 text-xs"
      style={{ color: enabled ? 'var(--state-available)' : 'var(--text-secondary)' }}
    >
      <span
        className="size-1.5 rounded-full"
        style={{ background: enabled ? 'var(--state-available)' : 'var(--state-offline)' }}
      />
      {t(`keys.statuses.${status}`)}
    </span>
  )
}

/**
 * The secret, shown once.
 *
 * A modal rather than a line in the table, because this is the only moment it
 * will ever exist and a reader who scrolls past it has lost the key. The
 * sentence says so plainly — "reissue" is the recovery path, and an operator
 * who does not know that will file a support request instead.
 */
function IssuedSecret({
  name,
  secret,
  onClose,
}: {
  name: string
  secret: string
  onClose: () => void
}) {
  const { t } = useTranslation()
  const [copied, setCopied] = useState(false)

  return (
    <RecordDialog
      open
      onOpenChange={(open) => {
        if (!open) onClose()
      }}
      title={t('keys.issuedTitle', { name })}
      submitLabel={t('common.done')}
      onSubmit={onClose}
    >
      <Field label={t('keys.secret')} hint={t('keys.secretHint')}>
        <span className="flex items-center gap-2">
          <code className="flex-1 truncate rounded-md border bg-background px-2 py-1.5 font-mono text-sm">
            {secret}
          </code>
          <Button
            type="button"
            size="sm"
            variant="outline"
            onClick={() => {
              void navigator.clipboard.writeText(secret)
              setCopied(true)
            }}
          >
            {copied ? t('admin.copied') : t('admin.copy')}
          </Button>
        </span>
      </Field>
      <Field label={t('keys.howToSend')}>
        <code className="block truncate rounded-md border bg-background px-2 py-1.5 font-mono text-xs text-muted-foreground">
          Authorization: Bearer {secret}
        </code>
      </Field>
    </RecordDialog>
  )
}

/** Revocation is terminal, so it is confirmed in place like a deletion. */
function ConfirmRevoke({ label, onConfirm }: { label: string; onConfirm: () => void }) {
  const { t } = useTranslation()
  return (
    <Popover.Root>
      <Popover.Trigger asChild>
        <Button size="icon-sm" variant="ghost" title={t('keys.revoke')}>
          <Ban />
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
                {t('keys.revoke')}
              </Button>
            </Popover.Close>
          </div>
        </Popover.Content>
      </Popover.Portal>
    </Popover.Root>
  )
}
