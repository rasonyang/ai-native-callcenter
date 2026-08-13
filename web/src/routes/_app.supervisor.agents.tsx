import { createFileRoute } from '@tanstack/react-router'
import { useTranslation } from 'react-i18next'
import { useState } from 'react'
import { Popover } from 'radix-ui'

import { StateDot } from '@/components/state-dot'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { useElapsedSec, useForceLogout, useRoster } from '@/lib/agent'
import type { RosterEntry } from '@/lib/api'
import { formatDuration } from '@/lib/utils'

/** Live agent roster: who could take a call, and if not, why. */
export const Route = createFileRoute('/_app/supervisor/agents')({ component: AgentRoster })

function AgentRoster() {
  const { t } = useTranslation()
  const { data, isPending, isError } = useRoster(true)
  const [filter, setFilter] = useState('')

  const rows = (data?.items ?? []).filter((row) => {
    const needle = filter.trim().toLowerCase()
    if (!needle) return true
    return (
      row.displayName.toLowerCase().includes(needle) ||
      row.username.toLowerCase().includes(needle) ||
      (row.extensionNumber ?? '').includes(needle)
    )
  })

  return (
    <section className="rounded-md border bg-card">
      <header className="flex items-center justify-between gap-4 border-b p-4">
        <h1 className="text-base font-medium">{t('nav.agents')}</h1>
        <Input
          className="w-56"
          placeholder={t('common.search')}
          value={filter}
          onChange={(event) => setFilter(event.target.value)}
        />
      </header>

      <table className="w-full text-sm">
        <thead>
          <tr className="border-b text-xs uppercase tracking-wide text-muted-foreground">
            <Th>{t('supervisor.agent')}</Th>
            <Th>{t('supervisor.status')}</Th>
            <Th>{t('supervisor.timeInState')}</Th>
            <Th>{t('agent.extension')}</Th>
            <Th>{t('supervisor.device')}</Th>
            <Th className="text-right">{t('supervisor.actions')}</Th>
          </tr>
        </thead>
        <tbody>
          {isPending && (
            <tr>
              <td colSpan={6} className="p-4 text-xs text-muted-foreground">
                {t('common.loading')}
              </td>
            </tr>
          )}
          {isError && (
            <tr>
              <td colSpan={6} className="p-4 text-xs text-destructive">
                {t('errors.STORAGE_DOWN')}
              </td>
            </tr>
          )}
          {!isPending && rows.length === 0 && (
            <tr>
              <td colSpan={6} className="p-4 text-xs text-muted-foreground">
                {t('supervisor.noAgents')}
              </td>
            </tr>
          )}
          {rows.map((row) => (
            <AgentRow key={row.agentId} row={row} />
          ))}
        </tbody>
      </table>
    </section>
  )
}

function AgentRow({ row }: { row: RosterEntry }) {
  const { t } = useTranslation()
  const elapsedSec = useElapsedSec(row.enteredAt)
  const forceLogout = useForceLogout()

  const status =
    row.availability === 'NOT_READY' && row.reason
      ? t(`reasons.${row.reason}`)
      : t(`availability.${row.availability}`)

  return (
    <tr className="h-9 border-b last:border-0 hover:bg-muted">
      <Td>
        <span className="font-medium">{row.displayName}</span>
        <span className="ml-2 text-xs text-muted-foreground">{row.username}</span>
      </Td>
      <Td>
        <span className="inline-flex items-center gap-1.5">
          <StateDot availability={row.availability} />
          {status}
        </span>
      </Td>
      <Td className="tabular">{formatDuration(elapsedSec)}</Td>
      <Td className="tabular">{row.extensionNumber ?? '—'}</Td>
      <Td>
        {row.state === 'LOGGED_OUT' ? (
          <span className="text-xs text-muted-foreground">—</span>
        ) : row.isRegistered ? (
          <span className="text-xs text-muted-foreground">{t('supervisor.deviceOk')}</span>
        ) : (
          // A registered phone that stopped answering keepalives looks exactly
          // like a working one to the agent, so it is called out here.
          <span className="text-xs text-destructive">{t('supervisor.deviceLost')}</span>
        )}
      </Td>
      <Td className="text-right">
        {row.state !== 'LOGGED_OUT' && (
          <Popover.Root>
            <Popover.Trigger asChild>
              <Button size="sm" variant="ghost">
                {t('supervisor.forceLogout')}
              </Button>
            </Popover.Trigger>
            <Popover.Portal>
              <Popover.Content
                align="end"
                sideOffset={4}
                className="z-50 w-64 rounded-md border bg-popover p-3 text-sm"
              >
                <p className="mb-3 text-xs text-muted-foreground">
                  {t('supervisor.forceLogoutConfirm', { name: row.displayName })}
                </p>
                <div className="flex justify-end gap-2">
                  <Popover.Close asChild>
                    <Button size="sm" variant="ghost">
                      {t('common.cancel')}
                    </Button>
                  </Popover.Close>
                  <Popover.Close asChild>
                    <Button
                      size="sm"
                      variant="destructive"
                      onClick={() => forceLogout.mutate(row.agentId)}
                    >
                      {t('supervisor.forceLogout')}
                    </Button>
                  </Popover.Close>
                </div>
              </Popover.Content>
            </Popover.Portal>
          </Popover.Root>
        )}
      </Td>
    </tr>
  )
}

function Th({ children, className }: { children: React.ReactNode; className?: string }) {
  return <th className={`px-4 py-2 text-left font-medium ${className ?? ''}`}>{children}</th>
}

function Td({ children, className }: { children: React.ReactNode; className?: string }) {
  return <td className={`px-4 ${className ?? ''}`}>{children}</td>
}
