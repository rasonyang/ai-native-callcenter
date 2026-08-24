import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import type { components } from '@/generated/api'

import { ApiError, request } from './api'

/**
 * Conversation flows: what the bot is, what it may do, and which version of
 * that answers the phone.
 *
 * The identity and publication wire types come from the generated contract, as
 * everywhere else. The spec itself does not: the contract carries it as an
 * opaque document on purpose, because its dialect belongs to the server's
 * loader — the same code a live call parses the published revision with. The
 * shapes below are this screen's reading of that document, used to draw it.
 * They are not a second contract: the server refuses anything it dislikes and
 * says why, and nothing here decides what may be saved.
 */

export type Flow = components['schemas']['Flow']
export type FlowDetail = components['schemas']['FlowDetail']
export type FlowRevision = components['schemas']['FlowRevision']

/** A caller-facing string: one wording everywhere, or one per language. */
export type FlowText = string | { zh?: string; en?: string }

export type FlowTextList = string[] | { zh?: string[]; en?: string[] }

/** The language a spec's wording is read in. Not the call's provider. */
export type SpecLang = 'en' | 'zh'

export interface FlowCondition {
  slot?: string
  op?: string
  value?: unknown
  all?: FlowCondition[]
  any?: FlowCondition[]
}

export interface FlowTransition {
  on?: string
  tool?: string
  condition?: FlowCondition
  target?: string
  priority?: number
}

export interface FlowNode {
  instruction?: FlowText
  tools?: string[]
  transitions?: FlowTransition[]
  isTerminal?: boolean
}

export interface FlowTool {
  description?: FlowText
  parameters?: {
    properties?: Record<string, { type?: string }>
    required?: string[]
  }
  http?: {
    path?: string
    method?: string
    successWhen?: { path?: string; equals?: string }
    errorFrom?: string
    result?: Record<string, string>
  }
}

export interface FlowGlobal {
  persona?: FlowText
  rules?: FlowTextList
  /** The bot's timbre, published with the persona rather than set per host. */
  voice?: string
  fallbackTarget?: string
  maxTurns?: number
  alwaysAllowedTools?: string[]
  apiBaseEnv?: string
  transitions?: FlowTransition[]
}

export interface FlowSpec {
  id?: string
  specVersion?: string
  entry?: string
  initialNode?: string
  global?: FlowGlobal
  nodes?: Record<string, FlowNode>
  tools?: Record<string, FlowTool>
}

/** The wording for one language, falling back to the other rather than to
 *  nothing: a missing translation should leave the reader the wrong language,
 *  not an empty box. This mirrors flow.Text.For on the server. */
export function textFor(value: FlowText | undefined, lang: SpecLang): string {
  if (value === undefined) return ''
  if (typeof value === 'string') return value
  return (lang === 'zh' ? value.zh || value.en : value.en || value.zh) ?? ''
}

export function textListFor(value: FlowTextList | undefined, lang: SpecLang): string[] {
  if (value === undefined) return []
  if (Array.isArray(value)) return value
  const preferred = lang === 'zh' ? value.zh : value.en
  const other = lang === 'zh' ? value.en : value.zh
  return (preferred?.length ? preferred : other) ?? []
}

/**
 * Phases no transition can reach.
 *
 * The loader does not refuse these, and it is right not to: an orphan phase
 * breaks nothing at runtime, the conversation simply never arrives. But it is
 * almost always a rename that was only half applied, so it is worth saying —
 * as a remark from this screen, not as a rule invented next to the server's.
 */
export function unreachableNodes(spec: FlowSpec): string[] {
  const nodes = spec.nodes ?? {}
  const reached = new Set<string>()
  const frontier: string[] = []

  const enter = (id: string | undefined) => {
    if (!id || !(id in nodes) || reached.has(id)) return
    reached.add(id)
    frontier.push(id)
  }

  enter(spec.initialNode)
  enter(spec.global?.fallbackTarget)
  for (const rule of spec.global?.transitions ?? []) enter(rule.target)

  while (frontier.length > 0) {
    const id = frontier.pop() as string
    for (const rule of nodes[id]?.transitions ?? []) enter(rule.target)
  }

  return Object.keys(nodes).filter((id) => !reached.has(id))
}

/**
 * Whether two specs are the same document.
 *
 * Key order is not a difference. The column is jsonb, so what comes back from
 * a save is the server's ordering rather than the author's, and comparing the
 * text would leave a freshly saved draft permanently marked unsaved — with
 * Publish, which requires a clean draft, permanently out of reach. This is the
 * same rule the server applies for hasUnpublishedChanges, which is jsonb's own.
 */
export function sameSpec(a: unknown, b: unknown): boolean {
  return canonical(a) === canonical(b)
}

function canonical(value: unknown): string {
  if (value === null || typeof value !== 'object') return JSON.stringify(value) ?? 'null'
  if (Array.isArray(value)) return `[${value.map(canonical).join(',')}]`
  const entries = Object.entries(value as Record<string, unknown>)
    .filter(([, v]) => v !== undefined)
    .toSorted(([left], [right]) => (left < right ? -1 : left > right ? 1 : 0))
  return `{${entries.map(([k, v]) => `${JSON.stringify(k)}:${canonical(v)}`).join(',')}}`
}

/**
 * Where each phase's block sits in the text, so selecting one in the graph
 * scrolls the editor to the lines that define it.
 *
 * It reads the text rather than the parsed document because that is what the
 * author is looking at; a position in a re-serialized copy would point at a
 * different line.
 */
export function locateNodes(text: string, ids: string[]): Record<string, [number, number]> {
  const found: Record<string, [number, number]> = {}
  const nodesAt = text.indexOf('"nodes"')
  if (nodesAt < 0) return found

  for (const id of ids) {
    const match = new RegExp(`"${escapeRegExp(id)}"\\s*:\\s*\\{`).exec(text.slice(nodesAt))
    if (!match) continue
    const keyAt = nodesAt + match.index
    const braceAt = text.indexOf('{', keyAt + id.length)
    let depth = 0
    for (let i = braceAt; i < text.length; i++) {
      if (text[i] === '{') depth++
      else if (text[i] === '}') {
        depth--
        if (depth === 0) {
          found[id] = [keyAt, i + 1]
          break
        }
      }
    }
  }
  return found
}

function escapeRegExp(value: string): string {
  return value.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
}

const OPERATORS: Record<string, string> = {
  EQ: '=', NE: '≠', GT: '>', LT: '<', GTE: '≥', LTE: '≤',
  IS_NULL: 'is null', IS_NOT_NULL: 'is set',
  IS_EMPTY: 'is empty', IS_NOT_EMPTY: 'is not empty',
  IN: 'in', NOT_IN: 'not in', CONTAINS: 'contains', NOT_CONTAINS: 'excludes',
}

/** What fires a transition, short enough to sit on an arrow. */
export function describeRule(rule: FlowTransition): string {
  const trigger = rule.tool ? rule.tool : rule.on === 'NO_INPUT' ? 'no input' : 'any result'
  const condition = describeCondition(rule.condition)
  return condition ? `${trigger} · ${condition}` : trigger
}

export function describeCondition(condition: FlowCondition | undefined): string {
  if (!condition) return ''
  if (condition.all?.length) return condition.all.map(describeCondition).join(' & ')
  if (condition.any?.length) return condition.any.map(describeCondition).join(' | ')
  if (!condition.slot) return ''
  const operator = OPERATORS[condition.op ?? ''] ?? condition.op ?? ''
  const slot = condition.slot.replace(/^result\./, '')
  return condition.value === undefined
    ? `${slot} ${operator}`
    : `${slot} ${operator} ${String(condition.value)}`
}

/** The problems the loader reported, if this failure is one it reported. */
export function specProblems(error: unknown): string[] {
  if (!(error instanceof ApiError)) return []
  const problems = error.params.problems
  if (!Array.isArray(problems)) return []
  return problems.filter((p): p is string => typeof p === 'string')
}

export const flowsApi = {
  list: () => request<{ items: Flow[] }>('/flows'),

  get: (flowId: string) => request<FlowDetail>(`/flows/${flowId}`),

  create: (slug: string, name: string, spec: FlowSpec) =>
    request<Flow>('/flows', {
      method: 'POST',
      body: JSON.stringify({ slug, name, spec }),
    }),

  updateDraft: (flowId: string, name: string, spec: FlowSpec) =>
    request<Flow>(`/flows/${flowId}`, {
      method: 'PUT',
      body: JSON.stringify({ name, spec }),
    }),

  publish: (flowId: string, note: string) =>
    request<Flow>(`/flows/${flowId}/publish`, {
      method: 'POST',
      body: JSON.stringify(note ? { note } : {}),
    }),
}

export const FLOWS_KEY = ['flows'] as const

export function useFlows() {
  return useQuery({ queryKey: FLOWS_KEY, queryFn: flowsApi.list })
}

export function useFlow(flowId: string) {
  return useQuery({
    queryKey: [...FLOWS_KEY, flowId],
    queryFn: () => flowsApi.get(flowId),
  })
}

/**
 * The three writes, sharing one invalidation: saving a draft changes whether
 * the flow has unpublished changes, and publishing changes what every number
 * pointing at it says — the list is wrong after either.
 */
export function useFlowMutations(flowId?: string) {
  const queryClient = useQueryClient()
  const invalidate = () => {
    void queryClient.invalidateQueries({ queryKey: FLOWS_KEY })
  }

  return {
    create: useMutation({
      mutationFn: ({ slug, name, spec }: { slug: string; name: string; spec: FlowSpec }) =>
        flowsApi.create(slug, name, spec),
      onSuccess: invalidate,
    }),
    save: useMutation({
      mutationFn: ({ name, spec }: { name: string; spec: FlowSpec }) =>
        flowsApi.updateDraft(flowId ?? '', name, spec),
      onSuccess: invalidate,
    }),
    publish: useMutation({
      mutationFn: (note: string) => flowsApi.publish(flowId ?? '', note),
      onSuccess: invalidate,
    }),
  }
}

/**
 * The starting point a new flow is created from.
 *
 * Not an empty object: the loader refuses one, and an author's first
 * experience of the editor should be a spec that already publishes, not a
 * validation report. It is the smallest flow that does — one phase, a persona,
 * and the built-ins every flow gets.
 */
export function starterSpec(slug: string): FlowSpec {
  return {
    id: slug,
    specVersion: 'v2',
    initialNode: 'welcome',
    global: {
      persona: {
        en: 'You are the voice assistant for this hotline.',
        zh: '你是这条热线的语音助手。',
      },
      rules: {
        en: ['This is a spoken phone conversation: keep every reply to one or two short sentences.'],
        zh: ['这是电话语音对话：每次只说一两句短句。'],
      },
      alwaysAllowedTools: ['transfer_to_agent', 'hangup'],
      maxTurns: 40,
    },
    nodes: {
      welcome: {
        instruction: {
          en: 'Greet the caller and ask how you can help.',
          zh: '问候来电者，并询问需要什么帮助。',
        },
        tools: [],
      },
    },
  }
}
