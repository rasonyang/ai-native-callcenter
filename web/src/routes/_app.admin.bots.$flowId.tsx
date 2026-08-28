import { createFileRoute } from '@tanstack/react-router'
import { useTranslation } from 'react-i18next'
import { useEffect, useMemo, useRef, useState } from 'react'
import { Play } from 'lucide-react'

import { PageHeader } from '@/components/page-header'
import { Field, Input, RecordDialog } from '@/components/record-dialog'
import { DataTable, TBody, THead, TableMessage, Td, Th, Tr } from '@/components/table'
import { Button } from '@/components/ui/button'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { useNameThisPage } from '@/lib/breadcrumb'
import { describeError } from '@/lib/errors'
import {
  describeRule,
  locateNodes,
  sameSpec,
  specProblems,
  textFor,
  textListFor,
  unreachableNodes,
  useFlow,
  useFlowMutations,
  type FlowSpec,
  type SpecLang,
} from '@/lib/flows'
import { requireRole } from '@/lib/guards'
import { cn } from '@/lib/utils'
import { PublicationState } from '@/routes/_app.admin.bots.index'

/**
 * One flow: its draft, drawn as a graph and edited as JSON.
 *
 * The two halves are the same document. The JSON is authoritative — it is what
 * is stored and what the loader reads — and the graph is how an author sees
 * whether the phases actually join up. Clicking a phase moves the editor to
 * it rather than opening a form: there is one place a flow is changed.
 */
export const Route = createFileRoute('/_app/admin/bots/$flowId')({
  beforeLoad: ({ context }) => requireRole(context.user, 'ADMIN'),
  component: FlowDesigner,
})

function FlowDesigner() {
  const { flowId } = Route.useParams()
  const { t, i18n } = useTranslation()
  const { data, isPending, isError, error } = useFlow(flowId)
  const { save, publish } = useFlowMutations(flowId)

  const [name, setName] = useState('')
  const [text, setText] = useState('')
  const [lang, setLang] = useState<SpecLang>(i18n.language.startsWith('zh') ? 'zh' : 'en')
  const [selected, setSelected] = useState<string | null>(null)
  const [publishing, setPublishing] = useState(false)
  const [note, setNote] = useState('')
  // The last document that parsed, so an in-progress edit does not blank the
  // graph beside it — a half-typed brace is not a reason to lose the picture.
  const [spec, setSpec] = useState<FlowSpec>({})
  const [parseError, setParseError] = useState<string | null>(null)
  const loaded = useRef<string | null>(null)

  // The trail ends in the flow's own name, which only exists once it is here.
  useNameThisPage(data?.flow.name)

  // The server's copy seeds the editor once. Refetches after a save must not
  // overwrite what the author has typed since.
  useEffect(() => {
    if (!data || loaded.current === flowId) return
    loaded.current = flowId
    setName(data.flow.name)
    setText(JSON.stringify(data.draftSpec, null, 2))
    setSpec(data.draftSpec as FlowSpec)
  }, [data, flowId])

  useEffect(() => {
    const timer = setTimeout(() => {
      if (text === '') return
      try {
        setSpec(JSON.parse(text) as FlowSpec)
        setParseError(null)
      } catch (e) {
        setParseError(e instanceof Error ? e.message : 'invalid JSON')
      }
    }, 250)
    return () => clearTimeout(timer)
  }, [text])

  const nodeRanges = useMemo(() => locateNodes(text, Object.keys(spec.nodes ?? {})), [text, spec])
  const orphans = useMemo(() => unreachableNodes(spec), [spec])
  const dateFormat = useMemo(
    () =>
      new Intl.DateTimeFormat(i18n.language, {
        dateStyle: 'medium',
        timeStyle: 'short',
      }),
    [i18n.language],
  )

  if (isPending) {
    return <p className="text-xs text-muted-foreground">{t('common.loading')}</p>
  }
  if (isError || !data) {
    return <p className="text-xs text-muted-foreground">{describeError(error, t)}</p>
  }

  const problems = specProblems(save.error)
  // Not a text comparison: the column is jsonb, so a saved draft comes back in
  // the server's key order rather than the author's, and comparing the text
  // would leave every saved flow marked unsaved — and Publish, which wants a
  // clean draft, permanently disabled.
  const isDirty = name !== data.flow.name || parseError !== null || !sameSpec(spec, data.draftSpec)

  const onSave = () => {
    if (parseError) return
    save.mutate({ name: name.trim() || data.flow.name, spec })
  }

  return (
    <>
      <PageHeader
        title={data.flow.name}
        description={`${data.flow.slug} · ${t('bots.revisions', { count: data.revisions.length })}`}
        actions={
          <>
            <PublicationState flow={data.flow} />
            <div className="flex items-center rounded-md border p-0.5">
              {(['en', 'zh'] as const).map((option) => (
                <button
                  key={option}
                  type="button"
                  onClick={() => setLang(option)}
                  className={cn(
                    'h-6 rounded-[4px] px-2 text-xs',
                    lang === option ? 'bg-primary/10 text-primary' : 'text-muted-foreground',
                  )}
                >
                  {option === 'zh' ? '中文' : 'EN'}
                </button>
              ))}
            </div>
            <Button
              size="sm"
              variant="ghost"
              disabled={!isDirty || parseError !== null || save.isPending}
              onClick={onSave}
            >
              {t('bots.saveDraft')}
            </Button>
            <Button size="sm" onClick={() => setPublishing(true)} disabled={isDirty}>
              {t('bots.publish')}
            </Button>
          </>
        }
      />

      <div className="mb-3 flex items-center gap-2">
        <label htmlFor="flow-name" className="text-xs text-muted-foreground">
          {t('bots.name')}
        </label>
        <Input
          id="flow-name"
          className="h-8 w-72"
          value={name}
          onChange={(e) => setName(e.target.value)}
        />
        {isDirty && <span className="text-xs text-muted-foreground">{t('bots.unsaved')}</span>}
      </div>

      {parseError && <Notice tone="breach">{t('bots.notJson', { message: parseError })}</Notice>}
      {problems.length > 0 && (
        <Notice tone="breach">
          <span className="font-medium">{t('bots.refused')}</span>
          <ul className="mt-1 list-disc pl-4">
            {problems.map((problem) => (
              // Verbatim from the loader. These name phases, tools and
              // operators out of the document itself, and a translation would
              // be a paraphrase of the only precise thing we have.
              <li key={problem} className="font-mono text-xs">
                {problem}
              </li>
            ))}
          </ul>
        </Notice>
      )}
      {problems.length === 0 && save.isError && (
        <Notice tone="breach">{describeError(save.error, t)}</Notice>
      )}
      {publish.isError && <Notice tone="breach">{describeError(publish.error, t)}</Notice>}
      {orphans.length > 0 && (
        <Notice tone="ringing">{t('bots.unreachable', { nodes: orphans.join(', ') })}</Notice>
      )}

      <Tabs defaultValue="designer">
        <TabsList>
          <TabsTrigger value="designer">{t('bots.tabs.designer')}</TabsTrigger>
          <TabsTrigger value="tools">{t('bots.tabs.tools')}</TabsTrigger>
          <TabsTrigger value="persona">{t('bots.tabs.persona')}</TabsTrigger>
          <TabsTrigger value="history">{t('bots.tabs.history')}</TabsTrigger>
        </TabsList>

        <TabsContent value="designer" className="pt-3">
          <div className="grid grid-cols-[minmax(0,5fr)_minmax(0,6fr)] gap-3">
            <SpecEditor
              value={text}
              onChange={setText}
              highlight={selected ? (nodeRanges[selected] ?? null) : null}
              onCursor={(position) => {
                for (const [id, [start, end]] of Object.entries(nodeRanges)) {
                  if (position >= start && position <= end) {
                    setSelected(id)
                    return
                  }
                }
              }}
            />
            <PhaseGraph
              spec={spec}
              lang={lang}
              selected={selected}
              onSelect={setSelected}
              orphans={orphans}
            />
          </div>
        </TabsContent>

        <TabsContent value="tools" className="pt-3">
          <ToolTable spec={spec} lang={lang} />
        </TabsContent>

        <TabsContent value="persona" className="pt-3">
          <Persona spec={spec} lang={lang} />
        </TabsContent>

        <TabsContent value="history" className="pt-3">
          <DataTable>
            <THead>
              <Th>{t('bots.revision')}</Th>
              <Th>{t('bots.note')}</Th>
              <Th align="right">{t('bots.createdAt')}</Th>
            </THead>
            <TBody>
              {data.revisions.length === 0 && (
                <TableMessage colSpan={3}>{t('bots.neverPublished')}</TableMessage>
              )}
              {data.revisions.map((revision) => (
                <Tr key={revision.revisionId}>
                  <Td className="font-mono text-xs">
                    {revision.revisionId.slice(0, 8)}
                    {revision.isPublished && (
                      <span className="ml-2 text-xs text-primary">{t('bots.live')}</span>
                    )}
                  </Td>
                  <Td className="text-muted-foreground">{revision.note || '—'}</Td>
                  <Td align="right" className="tabular text-muted-foreground">
                    {dateFormat.format(new Date(revision.createdAt))}
                  </Td>
                </Tr>
              ))}
            </TBody>
          </DataTable>
        </TabsContent>
      </Tabs>

      <RecordDialog
        open={publishing}
        onOpenChange={(open) => !open && setPublishing(false)}
        title={t('bots.publishTitle', { name: data.flow.name })}
        submitLabel={t('bots.publish')}
        isSaving={publish.isPending}
        error={publish.isError ? describeError(publish.error, t) : undefined}
        onSubmit={() =>
          publish.mutate(note.trim(), {
            onSuccess: () => {
              setPublishing(false)
              setNote('')
            },
          })
        }
      >
        <p className="text-xs text-muted-foreground">{t('bots.publishHint')}</p>
        <Field label={t('bots.note')}>
          <Input value={note} onChange={(e) => setNote(e.target.value)} maxLength={200} />
        </Field>
      </RecordDialog>
    </>
  )
}

function Notice({ tone, children }: { tone: 'breach' | 'ringing'; children: React.ReactNode }) {
  return (
    <div
      role="alert"
      className="mb-3 rounded-md border bg-card px-3 py-2 text-xs"
      style={{ color: `var(--state-${tone})` }}
    >
      {children}
    </div>
  )
}

// --- The editor -------------------------------------------------------------

const LINE_HEIGHT = 20

/**
 * A textarea over a highlighted copy of its own text.
 *
 * Monochrome: the design system allows one accent, so structure is carried by
 * weight rather than a syntax palette, and the accent is spent on the one
 * thing that matters here — which phase is being looked at.
 */
function SpecEditor({
  value,
  onChange,
  highlight,
  onCursor,
}: {
  value: string
  onChange: (next: string) => void
  highlight: [number, number] | null
  onCursor: (position: number) => void
}) {
  const { t } = useTranslation()
  const area = useRef<HTMLTextAreaElement | null>(null)
  const behind = useRef<HTMLPreElement | null>(null)
  const html = useMemo(() => paint(value, highlight), [value, highlight])

  useEffect(() => {
    if (!highlight || !area.current) return
    const line = value.slice(0, highlight[0]).split('\n').length
    const top = Math.max(0, (line - 3) * LINE_HEIGHT)
    area.current.scrollTop = top
    if (behind.current) behind.current.scrollTop = top
    // Only when the selection moves; following `value` would fight the typist.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [highlight])

  const sync = () => {
    if (!area.current || !behind.current) return
    behind.current.scrollTop = area.current.scrollTop
    behind.current.scrollLeft = area.current.scrollLeft
  }

  const shared =
    'absolute inset-0 m-0 overflow-auto whitespace-pre p-3 font-mono text-[13px] leading-5'

  return (
    <div className="relative h-[620px] overflow-hidden rounded-md border bg-card">
      <pre
        ref={behind}
        aria-hidden
        className={cn(shared, 'pointer-events-none')}
        dangerouslySetInnerHTML={{ __html: html }}
      />
      <textarea
        ref={area}
        value={value}
        aria-label={t('bots.specEditor')}
        spellCheck={false}
        wrap="off"
        onChange={(e) => onChange(e.target.value)}
        onScroll={sync}
        onSelect={(e) => onCursor(e.currentTarget.selectionStart ?? 0)}
        className={cn(
          shared,
          'resize-none border-0 bg-transparent text-transparent caret-foreground outline-none',
        )}
      />
    </div>
  )
}

function escapeHtml(source: string): string {
  return source.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
}

function colorize(escaped: string): string {
  return escaped.replace(
    /("(?:[^"\\]|\\.)*")(\s*:)?|\b(true|false|null)\b|-?\d+(?:\.\d+)?/g,
    (match, str?: string, colon?: string, keyword?: string) => {
      if (str) {
        return colon
          ? `<span class="font-medium">${str}</span>${colon}`
          : `<span class="text-muted-foreground">${str}</span>`
      }
      if (keyword) return `<span class="text-muted-foreground">${keyword}</span>`
      return `<span class="text-muted-foreground tabular">${match}</span>`
    },
  )
}

function paint(source: string, range: [number, number] | null): string {
  if (!range) return colorize(escapeHtml(source))
  const [start, end] = range
  return (
    colorize(escapeHtml(source.slice(0, start))) +
    '<mark class="bg-primary/10 text-inherit">' +
    colorize(escapeHtml(source.slice(start, end))) +
    '</mark>' +
    colorize(escapeHtml(source.slice(end)))
  )
}

// --- The graph --------------------------------------------------------------

const NODE_W = 208
const NODE_H = 92
const COL_GAP = 240
const ROW_GAP = 168
const PAD = 20
const LABEL_H = 18
const GLOBAL_DROP = 28
const MIN_SCALE = 0.6
const LABEL =
  'absolute z-10 -translate-x-1/2 -translate-y-1/2 whitespace-nowrap rounded-md border bg-card px-1.5 text-xs leading-4 text-muted-foreground'

type Edge = {
  /** null: a global rule, which every phase can fire. */
  from: string | null
  to: string
  label: string
  index: number
  count: number
}

/** The arrows the global rules and the fallback add, one per target entry. */
function globalEdges(spec: FlowSpec, anyPhase: string, fallback: string): Edge[] {
  const nodes = spec.nodes ?? {}
  const rules: Array<{ to: string; label: string }> = (spec.global?.transitions ?? [])
    .filter((rule) => rule.target && nodes[rule.target])
    .map((rule) => ({
      to: rule.target as string,
      label: `${anyPhase} · ${describeRule(rule)}`,
    }))
  const fb = spec.global?.fallbackTarget
  if (fb && nodes[fb]) rules.push({ to: fb, label: fallback })
  const perTarget: Record<string, number> = {}
  return rules.map((rule) => {
    const index = perTarget[rule.to] ?? 0
    perTarget[rule.to] = index + 1
    return { from: null, ...rule, index, count: 0 }
  })
}

/**
 * Phases in the order a call reaches them, laid out downwards: the entry phase
 * on the first row, everything one transition away on the next, and so on.
 *
 * Depth runs down rather than across so every arrow points the same way and
 * its label sits in the gap between two rows. Laid out across, a straight
 * chain puts each phase level with the one before it and the labels land on
 * top of the boxes they describe.
 *
 * The last row is what only the global rules reach — where a call goes when it
 * leaves the path it was on — followed by anything nothing reaches at all.
 */
function layout(spec: FlowSpec) {
  const nodes = spec.nodes ?? {}
  const rows: string[][] = []
  const placed = new Set<string>()

  let frontier = spec.initialNode && nodes[spec.initialNode] ? [spec.initialNode] : []
  frontier.forEach((id) => placed.add(id))
  while (frontier.length > 0) {
    rows.push(frontier)
    const next: string[] = []
    for (const id of frontier) {
      for (const rule of nodes[id]?.transitions ?? []) {
        if (rule.target && nodes[rule.target] && !placed.has(rule.target)) {
          placed.add(rule.target)
          next.push(rule.target)
        }
      }
    }
    frontier = next
  }

  const globalTargets = [
    ...new Set(
      [
        ...(spec.global?.transitions ?? []).map((r) => r.target),
        spec.global?.fallbackTarget,
      ].filter((id): id is string => Boolean(id) && Boolean(nodes[id as string])),
    ),
  ].filter((id) => !placed.has(id))
  globalTargets.forEach((id) => placed.add(id))
  if (globalTargets.length > 0) rows.push(globalTargets)

  const rest = Object.keys(nodes).filter((id) => !placed.has(id))
  if (rest.length > 0) rows.push(rest)

  const at: Record<string, { x: number; y: number }> = {}
  rows.forEach((row, rowIndex) =>
    row.forEach((id, columnIndex) => {
      at[id] = { x: PAD + columnIndex * COL_GAP, y: PAD + rowIndex * ROW_GAP }
    }),
  )
  const widest = Math.max(1, ...rows.map((row) => row.length))
  return {
    at,
    width: PAD * 2 + (widest - 1) * COL_GAP + NODE_W,
    height: PAD * 2 + Math.max(1, rows.length) * ROW_GAP,
  }
}

function PhaseGraph({
  spec,
  lang,
  selected,
  onSelect,
  orphans,
}: {
  spec: FlowSpec
  lang: SpecLang
  selected: string | null
  onSelect: (id: string) => void
  orphans: string[]
}) {
  const { t } = useTranslation()
  const board = useMemo(() => layout(spec), [spec])
  const nodes = spec.nodes ?? {}
  const orphaned = new Set(orphans)

  // Every arrow the loader would follow, including the ones no phase owns:
  // the global rules and the fallback reach their target from *any* phase, so
  // they are drawn entering the target alone, dashed, rather than from one
  // node that would be a lie about where the call came from. Without them the
  // phases only the global rules reach look orphaned, which is the opposite of
  // what they are — they are the phases every path can end in.
  const edges: Edge[] = [
    ...Object.entries(nodes).flatMap(([from, node]) =>
      (node.transitions ?? []).map((rule, index, all) => ({
        from,
        to: rule.target ?? '',
        label: describeRule(rule),
        index,
        count: all.length,
      })),
    ),
    ...globalEdges(spec, t('bots.anyPhase'), t('bots.fallbackTarget')),
  ]

  // The graph is laid out at one size and shrunk to fit its box, so a nine-
  // phase flow is seen whole instead of being cut at the right edge and
  // finished below the fold. Scrolling stays for a flow too wide to shrink
  // legibly.
  const box = useRef<HTMLDivElement | null>(null)
  const [scale, setScale] = useState(1)
  useEffect(() => {
    const el = box.current
    if (!el) return
    const fit = () => {
      const s = Math.min(
        1,
        (el.clientWidth - 2) / board.width,
        (el.clientHeight - 2) / board.height,
      )
      setScale(Math.max(MIN_SCALE, s))
    }
    fit()
    const observer = new ResizeObserver(fit)
    observer.observe(el)
    return () => observer.disconnect()
  }, [board.width, board.height])

  return (
    <div ref={box} className="h-[620px] overflow-auto rounded-md border bg-background">
      <div style={{ width: board.width * scale, height: board.height * scale }}>
        <div
          className="relative origin-top-left"
          style={{
            width: board.width,
            height: board.height,
            transform: `scale(${scale})`,
          }}
        >
          <svg className="absolute inset-0" width={board.width} height={board.height}>
            <defs>
              <marker
                id="phase-arrow"
                viewBox="0 0 8 8"
                refX="7"
                refY="4"
                markerWidth="7"
                markerHeight="7"
                orient="auto-start-reverse"
              >
                <path d="M 0 0 L 8 4 L 0 8 z" fill="var(--muted-foreground)" />
              </marker>
            </defs>
            {edges.map((edge, i) => {
              const to = board.at[edge.to]
              if (!to) return null
              const tx = to.x + NODE_W / 2 + (edge.from === null ? edge.index * 40 : 0)
              const ty = to.y - 4
              if (edge.from === null) {
                // From nowhere in particular: a short dashed drop into the phase.
                return (
                  <path
                    key={i}
                    d={`M ${tx} ${ty - GLOBAL_DROP} L ${tx} ${ty}`}
                    stroke="var(--muted-foreground)"
                    strokeWidth="1.2"
                    strokeDasharray="3 3"
                    fill="none"
                    markerEnd="url(#phase-arrow)"
                  />
                )
              }
              const from = board.at[edge.from]
              if (!from) return null
              const sx = from.x + NODE_W / 2 + (edge.index - (edge.count - 1) / 2) * 40
              const sy = from.y + NODE_H
              return (
                <path
                  key={i}
                  d={`M ${sx} ${sy} C ${sx} ${sy + 40}, ${tx} ${ty - 40}, ${tx} ${ty}`}
                  stroke="var(--muted-foreground)"
                  strokeWidth="1.2"
                  fill="none"
                  markerEnd="url(#phase-arrow)"
                />
              )
            })}
          </svg>

          {edges.map((edge, i) => {
            const to = board.at[edge.to]
            if (!to || !edge.label) return null
            if (edge.from === null) {
              return (
                <span
                  key={`label-${i}`}
                  className={cn(LABEL, 'border-dashed')}
                  style={{
                    left: to.x + NODE_W / 2 + edge.index * 40,
                    top: to.y - 4 - GLOBAL_DROP - LABEL_H / 2,
                  }}
                >
                  {edge.label}
                </span>
              )
            }
            const from = board.at[edge.from]
            if (!from) return null
            const sx = from.x + NODE_W / 2 + (edge.index - (edge.count - 1) / 2) * 40
            // Sibling labels leave the same node 40px apart and are wider than
            // that, so at one height they sit on each other. Each takes its own
            // line instead, in the order its arrow leaves the node.
            const stagger = (edge.index - (edge.count - 1) / 2) * (LABEL_H + 4)
            return (
              <span
                key={`label-${i}`}
                className={LABEL}
                style={{
                  left: (sx + to.x + NODE_W / 2) / 2,
                  top: (from.y + NODE_H + to.y) / 2 + stagger,
                }}
              >
                {edge.label}
              </span>
            )
          })}

          {Object.entries(nodes).map(([id, node]) => {
            const position = board.at[id]
            if (!position) return null
            return (
              <button
                key={id}
                type="button"
                onClick={() => onSelect(id)}
                className={cn(
                  'absolute overflow-hidden rounded-md border bg-card p-2.5 text-left',
                  node.isTerminal && 'border-foreground/40',
                  orphaned.has(id) && 'border-dashed',
                  selected === id && 'border-primary',
                )}
                style={{
                  left: position.x,
                  top: position.y,
                  width: NODE_W,
                  height: NODE_H,
                }}
              >
                <span className="flex items-center gap-1.5">
                  {id === spec.initialNode && (
                    <Play className="size-3 shrink-0 text-muted-foreground" />
                  )}
                  <span className="truncate font-mono text-[13px] font-medium">{id}</span>
                  {node.isTerminal && (
                    <span
                      className="ml-auto shrink-0 text-xs text-muted-foreground"
                      title={t('bots.terminal')}
                    >
                      ■
                    </span>
                  )}
                </span>
                <span className="mt-1 line-clamp-2 text-xs text-muted-foreground">
                  {textFor(node.instruction, lang)}
                </span>
                <span className="mt-1 flex flex-wrap gap-1">
                  {(node.tools ?? []).map((tool) => (
                    <span
                      key={tool}
                      className="rounded-md border px-1 font-mono text-xs leading-4 text-muted-foreground"
                    >
                      {tool}
                    </span>
                  ))}
                </span>
              </button>
            )
          })}
        </div>
      </div>
    </div>
  )
}

// --- The other tabs ---------------------------------------------------------

function ToolTable({ spec, lang }: { spec: FlowSpec; lang: SpecLang }) {
  const { t } = useTranslation()
  const tools = Object.entries(spec.tools ?? {})

  return (
    <DataTable>
      <THead>
        <Th>{t('bots.tool')}</Th>
        <Th>{t('bots.description')}</Th>
        <Th>{t('bots.httpPath')}</Th>
        <Th>{t('bots.parameters')}</Th>
        <Th>{t('bots.resultSlots')}</Th>
      </THead>
      <TBody>
        {tools.length === 0 && <TableMessage colSpan={5}>{t('bots.noTools')}</TableMessage>}
        {tools.map(([toolName, tool]) => (
          <Tr key={toolName} className="h-auto">
            <Td className="py-2 align-top font-mono text-xs font-medium">{toolName}</Td>
            <Td className="max-w-72 py-2 align-top text-muted-foreground">
              {textFor(tool.description, lang)}
            </Td>
            <Td className="py-2 align-top font-mono text-xs">
              {tool.http?.method ?? 'POST'} {tool.http?.path ?? '—'}
            </Td>
            <Td className="py-2 align-top font-mono text-xs text-muted-foreground">
              {Object.keys(tool.parameters?.properties ?? {}).join(', ') || '—'}
            </Td>
            <Td className="py-2 align-top font-mono text-xs text-muted-foreground">
              {Object.entries(tool.http?.result ?? {})
                .map(([slot, path]) => `${slot} ← ${path}`)
                .join('; ') || '—'}
            </Td>
          </Tr>
        ))}
      </TBody>
    </DataTable>
  )
}

function Persona({ spec, lang }: { spec: FlowSpec; lang: SpecLang }) {
  const { t } = useTranslation()
  const rules = textListFor(spec.global?.rules, lang)

  return (
    <div className="grid max-w-3xl gap-4">
      <section className="rounded-md border bg-card p-4">
        <h2 className="text-xs font-medium uppercase tracking-wide text-muted-foreground">
          {t('bots.persona')}
        </h2>
        <p className="mt-1.5 text-sm">{textFor(spec.global?.persona, lang) || '—'}</p>
      </section>

      <section className="rounded-md border bg-card p-4">
        <h2 className="text-xs font-medium uppercase tracking-wide text-muted-foreground">
          {t('bots.rules')}
        </h2>
        <ul className="mt-1.5 list-disc pl-4">
          {rules.map((rule) => (
            <li key={rule} className="text-sm text-muted-foreground">
              {rule}
            </li>
          ))}
          {rules.length === 0 && <li className="text-sm text-muted-foreground">—</li>}
        </ul>
      </section>

      <section className="rounded-md border bg-card p-4">
        <h2 className="text-xs font-medium uppercase tracking-wide text-muted-foreground">
          {t('bots.runtime')}
        </h2>
        <dl className="mt-1.5 grid grid-cols-2 gap-x-4 gap-y-2 text-sm">
          {/* The voice is published with the persona rather than set per
              deployment: it is part of the bot's character. Empty means the
              provider's own default answers. */}
          <Fact label={t('bots.voice')}>{spec.global?.voice || t('bots.providerDefault')}</Fact>
          <Fact label={t('bots.maxTurns')}>
            <span className="tabular">{spec.global?.maxTurns ?? '—'}</span>
          </Fact>
          <Fact label={t('bots.fallbackTarget')}>
            <span className="font-mono text-xs">{spec.global?.fallbackTarget || '—'}</span>
          </Fact>
          <Fact label={t('bots.alwaysAllowed')}>
            <span className="font-mono text-xs">
              {(spec.global?.alwaysAllowedTools ?? []).join(', ') || '—'}
            </span>
          </Fact>
        </dl>
      </section>
    </div>
  )
}

function Fact({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div>
      <dt className="text-xs text-muted-foreground">{label}</dt>
      <dd className="mt-0.5">{children}</dd>
    </div>
  )
}
