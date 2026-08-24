import { createFileRoute, useNavigate } from '@tanstack/react-router'
import { useTranslation } from 'react-i18next'
import { useMemo, useState } from 'react'
import { Plus } from 'lucide-react'

import { PageHeader } from '@/components/page-header'
import { Field, Input, RecordDialog } from '@/components/record-dialog'
import { DataTable, TBody, THead, TableMessage, Td, Th, Tr } from '@/components/table'
import { Button } from '@/components/ui/button'
import { useDIDs } from '@/lib/catalog'
import { describeError } from '@/lib/errors'
import { useFlowMutations, useFlows, starterSpec, type Flow } from '@/lib/flows'
import { requireRole } from '@/lib/guards'

/**
 * The bot flows.
 *
 * A flow is what the bot is and what it may do; a revision is the copy of it
 * that answers the phone. This list is about the distance between those two,
 * because that is the thing an operator cannot see from the numbers screen.
 */
export const Route = createFileRoute('/_app/admin/bots/')({
  beforeLoad: ({ context }) => requireRole(context.user, 'ADMIN'),
  component: BotFlowsPage,
})

function BotFlowsPage() {
  const { t, i18n } = useTranslation()
  const navigate = useNavigate()
  const { data, isPending, isError, error } = useFlows()
  const { data: dids } = useDIDs()
  const { create } = useFlowMutations()
  const [draft, setDraft] = useState<{ slug: string; name: string } | null>(null)

  const rows = data?.items ?? []

  // Which numbers answer with which flow. The join is done here rather than on
  // the server: both lists are this page's to read, and a flow does not own
  // the numbers pointing at it.
  const numbersByFlow = useMemo(() => {
    const byFlow = new Map<string, string[]>()
    for (const did of dids?.items ?? []) {
      if (!did.flowId) continue
      byFlow.set(did.flowId, [...(byFlow.get(did.flowId) ?? []), did.number])
    }
    return byFlow
  }, [dids])

  const dateFormat = useMemo(
    () => new Intl.DateTimeFormat(i18n.language, { dateStyle: 'medium', timeStyle: 'short' }),
    [i18n.language],
  )

  const submit = () => {
    if (!draft) return
    const slug = draft.slug.trim()
    create.mutate(
      { slug, name: draft.name.trim() || slug, spec: starterSpec(slug) },
      {
        onSuccess: (flow) => {
          setDraft(null)
          void navigate({ to: '/admin/bots/$flowId', params: { flowId: flow.flowId } })
        },
      },
    )
  }

  return (
    <>
      <PageHeader
        title={t('nav.bots')}
        description={t('bots.hint')}
        actions={
          <Button size="sm" onClick={() => setDraft({ slug: '', name: '' })}>
            <Plus />
            {t('bots.newFlow')}
          </Button>
        }
      />

      <DataTable>
        <THead>
          <Th>{t('bots.flow')}</Th>
          <Th>{t('bots.numbers')}</Th>
          <Th>{t('bots.status')}</Th>
          <Th>{t('bots.publishedAt')}</Th>
          <Th align="right">{t('bots.updatedAt')}</Th>
        </THead>
        <TBody>
          {isPending && <TableMessage colSpan={5}>{t('common.loading')}</TableMessage>}
          {isError && <TableMessage colSpan={5}>{describeError(error, t)}</TableMessage>}
          {!isPending && !isError && rows.length === 0 && (
            <TableMessage colSpan={5}>{t('bots.noFlows')}</TableMessage>
          )}
          {rows.map((flow) => (
            <Tr
              key={flow.flowId}
              className="cursor-pointer"
              onClick={() =>
                navigate({ to: '/admin/bots/$flowId', params: { flowId: flow.flowId } })
              }
            >
              <Td>
                <span className="font-medium">{flow.name}</span>
                <span className="ml-2 font-mono text-xs text-muted-foreground">{flow.slug}</span>
              </Td>
              <Td className="tabular text-muted-foreground">
                {numbersByFlow.get(flow.flowId)?.join(' · ') ?? '—'}
              </Td>
              <Td>
                <PublicationState flow={flow} />
              </Td>
              <Td className="tabular text-muted-foreground">
                {flow.publishedAt ? dateFormat.format(new Date(flow.publishedAt)) : '—'}
              </Td>
              <Td align="right" className="tabular text-muted-foreground">
                {dateFormat.format(new Date(flow.updatedAt))}
              </Td>
            </Tr>
          ))}
        </TBody>
      </DataTable>

      <RecordDialog
        open={draft !== null}
        onOpenChange={(open) => !open && setDraft(null)}
        title={t('bots.newFlow')}
        isSaving={create.isPending}
        error={create.isError ? describeError(create.error, t) : undefined}
        onSubmit={submit}
      >
        <Field label={t('bots.slug')} hint={t('bots.slugHint')}>
          <Input
            value={draft?.slug ?? ''}
            onChange={(e) => setDraft({ slug: e.target.value, name: draft?.name ?? '' })}
            placeholder="novanet_support"
          />
        </Field>
        <Field label={t('bots.name')}>
          <Input
            value={draft?.name ?? ''}
            onChange={(e) => setDraft({ slug: draft?.slug ?? '', name: e.target.value })}
          />
        </Field>
      </RecordDialog>
    </>
  )
}

/**
 * Three states, not two.
 *
 * "Published" is the only one where what is written is what callers hear. A
 * flow with edits behind it is live — on the older revision — and one that has
 * never been published answers nothing at all, which a number pointing at it
 * does not reveal anywhere else.
 */
export function PublicationState({ flow }: { flow: Flow }) {
  const { t } = useTranslation()

  if (!flow.publishedRevisionId) {
    return <Dot color="var(--state-offline)">{t('bots.states.draft')}</Dot>
  }
  if (flow.hasUnpublishedChanges) {
    return <Dot color="var(--state-ringing)">{t('bots.states.unpublishedChanges')}</Dot>
  }
  return <Dot color="var(--state-available)">{t('bots.states.published')}</Dot>
}

function Dot({ color, children }: { color: string; children: string }) {
  return (
    <span className="flex items-center gap-1.5 text-sm">
      <span className="size-1.5 rounded-full" style={{ backgroundColor: color }} />
      {children}
    </span>
  )
}
