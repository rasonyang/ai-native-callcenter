import { createFileRoute } from '@tanstack/react-router'
import { useTranslation } from 'react-i18next'
import { useState } from 'react'
import { Headphones, LogOut, Mic, PhoneCall, Search } from 'lucide-react'
import { Popover } from 'radix-ui'

import { PageHeader } from '@/components/page-header'
import { Select } from '@/components/record-dialog'
import { describeError } from '@/lib/errors'
import { requireRole } from '@/lib/guards'
import { StatusPill } from '@/components/status-pill'
import {
  DataTable, TBody, THead, TableFooter, TableMessage, Td, Th, Tr,
} from '@/components/table'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { useElapsedSec, useForceLogout, useMonitorCall, useRoster } from '@/lib/agent'
import type { Availability, MonitorMode, RosterEntry } from '@/lib/api'
import { formatDuration } from '@/lib/utils'

/** Filter options, in the order the wallboard stacks them. */
const AVAILABILITIES: Availability[] = [
  'READY',
  'ON_CALL',
  'WRAP_UP',
  'NOT_READY',
  'DEVICE_UNREACHABLE',
  'LOGGED_OUT',
]

/** Live agent roster: who could take a call, and if not, why. */
export const Route = createFileRoute('/_app/supervisor/agents')({
  beforeLoad: ({ context }) => requireRole(context.user, 'SUPERVISOR'),
  component: AgentRoster,
})

function AgentRoster() {
  const { t } = useTranslation()
  const { data, isPending, isError, error } = useRoster(true)
  const [filter, setFilter] = useState('')
  const [availability, setAvailability] = useState('')

  const all = data?.items ?? []
  const needle = filter.trim().toLowerCase()
  const rows = all.filter((row) => {
    if (availability && row.availability !== availability) return false
    if (!needle) return true
    return (
      row.displayName.toLowerCase().includes(needle) ||
      row.username.toLowerCase().includes(needle) ||
      (row.extensionNumber ?? '').includes(needle)
    )
  })
  const online = all.filter((row) => row.state !== 'LOGGED_OUT').length

  return (
    <>
      <PageHeader
        title={t('nav.agents')}
        actions={
          <>
            <div className="relative">
              <Search className="pointer-events-none absolute left-2 top-1/2 size-3.5 -translate-y-1/2 text-muted-foreground" />
              <Input
                className="w-56 pl-7"
                placeholder={t('supervisor.searchAgents')}
                value={filter}
                onChange={(event) => setFilter(event.target.value)}
              />
            </div>
            <div className="w-40">
              <Select
                value={availability}
                onChange={setAvailability}
                ariaLabel={t('supervisor.status')}
                options={[
                  { value: '', label: t('supervisor.allStatuses') },
                  ...AVAILABILITIES.map((value) => ({
                    value,
                    label: t(`availability.${value}`),
                  })),
                ]}
              />
            </div>
          </>
        }
      />

      <DataTable>
        <THead>
          <Th>{t('supervisor.status')}</Th>
          <Th>{t('supervisor.name')}</Th>
          <Th>{t('supervisor.ext')}</Th>
          <Th>{t('supervisor.timeInState')}</Th>
          <Th>{t('supervisor.device')}</Th>
          <Th align="right">{t('supervisor.actions')}</Th>
        </THead>
        <TBody>
          {isPending && <TableMessage colSpan={6}>{t('common.loading')}</TableMessage>}
          {isError && <TableMessage colSpan={6}>{describeError(error, t)}</TableMessage>}
          {!isPending && rows.length === 0 && (
            <TableMessage colSpan={6}>{t('supervisor.noAgents')}</TableMessage>
          )}
          {rows.map((row) => (
            <AgentRow key={row.agentId} row={row} />
          ))}
        </TBody>
      </DataTable>

      {!isPending && all.length > 0 && (
        <div className="rounded-b-md border border-t-0 bg-card">
          <TableFooter>
            <span>{t('supervisor.onlineCount', { online, total: all.length })}</span>
            <span className="tabular">{rows.length}</span>
          </TableFooter>
        </div>
      )}
    </>
  )
}

function AgentRow({ row }: { row: RosterEntry }) {
  const { t } = useTranslation()
  const elapsedSec = useElapsedSec(row.enteredAt)
  const forceLogout = useForceLogout()

  return (
    <Tr>
      <Td>
        <StatusPill availability={row.availability} reason={row.reason} />
      </Td>
      <Td>
        <span className="font-medium">{row.displayName}</span>
        <span className="ml-2 text-xs text-muted-foreground">{row.username}</span>
      </Td>
      <Td className="tabular">{row.extensionNumber ?? '—'}</Td>
      <Td className="tabular">{formatDuration(elapsedSec)}</Td>
      <Td>
        {row.state === 'LOGGED_OUT' ? (
          <span className="text-xs text-muted-foreground">—</span>
        ) : row.isRegistered ? (
          <span className="text-xs text-muted-foreground">{t('supervisor.deviceOk')}</span>
        ) : (
          // A registered phone that stopped answering keepalives looks exactly
          // like a working one to its agent, so it is called out here.
          <span className="text-xs" style={{ color: 'var(--state-breach)' }}>
            {t('supervisor.deviceLost')}
          </span>
        )}
      </Td>
      <Td align="right">
        {row.isOnCall && row.currentCallId && <MonitorButtons row={row} />}
        {row.state !== 'LOGGED_OUT' && (
          <Popover.Root>
            <Popover.Trigger asChild>
              <Button size="icon-sm" variant="ghost" title={t('supervisor.forceLogout')}>
                <LogOut />
              </Button>
            </Popover.Trigger>
            <Popover.Portal>
              <Popover.Content
                align="end"
                sideOffset={4}
                className="z-50 w-64 rounded-md border bg-popover p-3 text-sm shadow-md"
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
    </Tr>
  )
}

/** The three ways into a call, in order of how much the agent notices. */
const MONITOR_MODES: { mode: MonitorMode; Icon: typeof Headphones }[] = [
  { mode: 'LISTEN', Icon: Headphones },
  { mode: 'WHISPER', Icon: Mic },
  { mode: 'BARGE', Icon: PhoneCall },
]

/**
 * Listen / whisper / barge. Rendered only while the agent is on a call: with
 * no leg to attach to there is nothing to offer, and a row of greyed icons
 * would only say so at length.
 *
 * One click is the whole gesture — the phone that rings is the supervisor's
 * own and the server knows which it is. The popover exists to say what the
 * mode does before it happens, and to carry a refusal when the switch has one.
 */
function MonitorButtons({ row }: { row: RosterEntry }) {
  const { t } = useTranslation()
  const monitor = useMonitorCall()
  const [mode, setMode] = useState<MonitorMode | null>(null)
  const callId = row.currentCallId

  const start = () => {
    if (!mode || !callId) return
    monitor.mutate(
      { callId, mode, agentId: row.agentId },
      { onSuccess: () => setMode(null) },
    )
  }

  return (
    <Popover.Root
      open={mode !== null}
      onOpenChange={(open) => {
        if (!open) {
          setMode(null)
          monitor.reset()
        }
      }}
    >
      <Popover.Anchor asChild>
        <span className="inline-flex">
          {MONITOR_MODES.map(({ mode: m, Icon }) => (
            <Button
              key={m}
              size="icon-sm"
              variant="ghost"
              disabled={!callId}
              aria-pressed={mode === m}
              title={t(`supervisor.monitor.${m}`)}
              onClick={() => setMode(m)}
            >
              <Icon />
            </Button>
          ))}
        </span>
      </Popover.Anchor>
      <Popover.Portal>
        <Popover.Content
          align="end"
          sideOffset={4}
          className="z-50 w-72 rounded-md border bg-popover p-3 text-sm shadow-md"
        >
          {mode && (
            <>
              <p className="mb-1 font-medium">{t(`supervisor.monitor.${mode}`)}</p>
              <p className="mb-2 text-xs text-muted-foreground">
                {t(`supervisor.monitor.${mode}Hint`, { name: row.displayName })}
              </p>
              <p className="mb-3 text-xs text-muted-foreground">
                {t('supervisor.monitor.yourPhoneHint')}
              </p>
              {monitor.isError && (
                <p className="mb-3 text-xs" style={{ color: 'var(--state-breach)' }}>
                  {describeError(monitor.error, t)}
                </p>
              )}
              <div className="flex justify-end gap-2">
                <Popover.Close asChild>
                  <Button size="sm" variant="ghost">
                    {t('common.cancel')}
                  </Button>
                </Popover.Close>
                <Button size="sm" disabled={monitor.isPending} onClick={start}>
                  {t('supervisor.monitor.start')}
                </Button>
              </div>
            </>
          )}
        </Popover.Content>
      </Popover.Portal>
    </Popover.Root>
  )
}
