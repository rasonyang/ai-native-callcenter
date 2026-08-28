import { createFileRoute } from '@tanstack/react-router'
import { useTranslation } from 'react-i18next'
import { useState } from 'react'
import { KeyRound, Pencil, Plus } from 'lucide-react'

import { PageHeader } from '@/components/page-header'
import { Field, Input, RecordDialog, Select, useRecordForm } from '@/components/record-dialog'
import { DataTable, TBody, THead, TableMessage, Td, Th, Tr } from '@/components/table'
import { Button } from '@/components/ui/button'
import type { Role } from '@/lib/api'
import { describeError } from '@/lib/errors'
import { requireRole } from '@/lib/guards'
import { useUserMutations, useUsers, type User, type UserDraft } from '@/lib/users'

/**
 * The people who use this product, and what each of them was given.
 *
 * Creating an account is the one action here that writes three things: the
 * account, the ACD identity of somebody who takes calls, and the phone they
 * take them at. An administrator gets only the first — they are a user but not
 * an agent, and the roster is not a list of everyone with a login.
 *
 * The phone's number is allocated, never chosen: an operator adding a
 * supervisor is not deciding on 1042, they are asking for a desk.
 */
export const Route = createFileRoute('/_app/admin/users')({
  beforeLoad: ({ context }) => requireRole(context.user, 'ADMIN'),
  component: UsersAdmin,
})

const ROLES: Role[] = ['AGENT', 'SUPERVISOR', 'ADMIN']

function UsersAdmin() {
  const { t } = useTranslation()
  const { data, isPending, isError, error } = useUsers()
  const { create, update, resetPassword } = useUserMutations()
  const [editing, setEditing] = useRecordForm<UserDraft>(create, update)
  const [resetting, setResetting] = useState<User | null>(null)
  const [newPassword, setNewPassword] = useState('')

  const rows = data?.items ?? []
  const isNew = editing !== null && !editing.userId
  const saving = create.isPending || update.isPending
  const saveError = create.isError ? create.error : update.isError ? update.error : undefined

  const submit = () => {
    if (!editing) return
    const username = (editing.username ?? '').trim()
    const displayName = (editing.displayName ?? '').trim() || username
    const role = (editing.role ?? 'AGENT') as Role

    if (editing.userId) {
      update.mutate(
        {
          userId: editing.userId,
          body: { username, displayName, role, status: editing.status ?? 'ACTIVE' },
        },
        { onSuccess: () => setEditing(null) },
      )
      return
    }
    create.mutate(
      { username, displayName, role, password: editing.password ?? '' },
      { onSuccess: () => setEditing(null) },
    )
  }

  return (
    <>
      <PageHeader
        title={t('nav.users')}
        description={t('users.hint')}
        actions={
          <Button size="sm" onClick={() => setEditing({ role: 'AGENT', status: 'ACTIVE' })}>
            <Plus />
            {t('users.newUser')}
          </Button>
        }
      />

      <DataTable>
        <THead>
          <Th>{t('users.username')}</Th>
          <Th>{t('users.displayName')}</Th>
          <Th>{t('users.role')}</Th>
          <Th>{t('users.status')}</Th>
          <Th>{t('users.extension')}</Th>
          <Th align="right" />
        </THead>
        <TBody>
          {isPending && <TableMessage colSpan={6}>{t('common.loading')}</TableMessage>}
          {isError && <TableMessage colSpan={6}>{describeError(error, t)}</TableMessage>}
          {!isPending && !isError && rows.length === 0 && (
            <TableMessage colSpan={6}>{t('users.noUsers')}</TableMessage>
          )}
          {rows.map((user) => (
            <Tr key={user.userId}>
              <Td className="font-medium">{user.username}</Td>
              <Td className="text-muted-foreground">{user.displayName}</Td>
              <Td>{t(`roles.${user.role}`)}</Td>
              <Td>
                <span
                  className="flex items-center gap-1.5 text-sm"
                  style={
                    user.status === 'SUSPENDED'
                      ? { color: 'var(--state-breach)' }
                      : undefined
                  }
                >
                  {t(`users.statuses.${user.status}`)}
                </span>
              </Td>
              <Td className="tabular">
                {/* Absent, not blank: an administrator has no phone because
                    they take no calls, which is different from a phone that
                    was unbound. */}
                {user.extensionNumber ?? (
                  <span className="text-muted-foreground">{t('users.noPhone')}</span>
                )}
              </Td>
              <Td align="right">
                <span className="flex justify-end gap-1">
                  <Button
                    size="icon-sm"
                    variant="ghost"
                    title={t('users.resetPassword')}
                    onClick={() => {
                      setResetting(user)
                      setNewPassword('')
                    }}
                  >
                    <KeyRound />
                  </Button>
                  <Button
                    size="icon-sm"
                    variant="ghost"
                    title={t('common.edit')}
                    onClick={() => setEditing({ ...user })}
                  >
                    <Pencil />
                  </Button>
                </span>
              </Td>
            </Tr>
          ))}
        </TBody>
      </DataTable>

      <RecordDialog
        open={editing !== null}
        onOpenChange={(open) => !open && setEditing(null)}
        title={isNew ? t('users.newUser') : t('users.editUser')}
        isSaving={saving}
        error={saveError ? describeError(saveError, t) : undefined}
        onSubmit={submit}
      >
        <Field label={t('users.username')} hint={isNew ? t('users.usernameHint') : undefined}>
          <Input
            value={editing?.username ?? ''}
            onChange={(e) => setEditing({ ...editing, username: e.target.value })}
          />
        </Field>
        <Field label={t('users.displayName')}>
          <Input
            value={editing?.displayName ?? ''}
            onChange={(e) => setEditing({ ...editing, displayName: e.target.value })}
          />
        </Field>
        <Field label={t('users.role')} hint={isNew ? t('users.roleHint') : undefined}>
          <Select
            value={editing?.role ?? 'AGENT'}
            onChange={(role) => setEditing({ ...editing, role: role as Role })}
            options={ROLES.map((role) => ({ value: role, label: t(`roles.${role}`) }))}
          />
        </Field>
        {isNew ? (
          <Field label={t('users.initialPassword')} hint={t('users.passwordHint')}>
            <Input
              type="password"
              autoComplete="new-password"
              value={editing?.password ?? ''}
              onChange={(e) => setEditing({ ...editing, password: e.target.value })}
            />
          </Field>
        ) : (
          <Field label={t('users.status')} hint={t('users.statusHint')}>
            <Select
              value={editing?.status ?? 'ACTIVE'}
              onChange={(status) =>
                setEditing({ ...editing, status: status as User['status'] })
              }
              options={(['ACTIVE', 'SUSPENDED'] as const).map((status) => ({
                value: status,
                label: t(`users.statuses.${status}`),
              }))}
            />
          </Field>
        )}
      </RecordDialog>

      <RecordDialog
        open={resetting !== null}
        onOpenChange={(open) => !open && setResetting(null)}
        title={t('users.resetTitle', { username: resetting?.username ?? '' })}
        submitLabel={t('users.resetPassword')}
        isSaving={resetPassword.isPending}
        error={resetPassword.isError ? describeError(resetPassword.error, t) : undefined}
        onSubmit={() => {
          if (!resetting) return
          resetPassword.mutate(
            { userId: resetting.userId, password: newPassword },
            { onSuccess: () => setResetting(null) },
          )
        }}
      >
        <p className="text-xs text-muted-foreground">{t('users.resetHint')}</p>
        <Field label={t('users.newPassword')} hint={t('users.passwordHint')}>
          <Input
            type="password"
            autoComplete="new-password"
            value={newPassword}
            onChange={(e) => setNewPassword(e.target.value)}
          />
        </Field>
      </RecordDialog>
    </>
  )
}
