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
  /**
   * A line the bot says as written on entering this phase, rather than a brief
   * it improvises from. Optional: most phases leave the words to the model.
   */
  announce?: FlowText
  tools?: string[]
  transitions?: FlowTransition[]
  isTerminal?: boolean
}

/** One argument the model fills in: a JSON Schema property, the parts a reader wants. */
export interface FlowParameter {
  type?: string
  description?: string
  enum?: Array<string | number>
}

export interface FlowTool {
  description?: FlowText
  parameters?: {
    properties?: Record<string, FlowParameter>
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
  /**
   * The flow's own goodbye phase: a terminal node the engine moves a call to
   * on its own — after repeated silence in a phase where no rule can fire on
   * NO_INPUT, or once tool-less replies exceed maxTurnsWithoutTool. A
   * backstop for a model that keeps asking "anything else?", not a
   * replacement for a closing phase that hangs up on a decline. Must name a
   * terminal phase.
   */
  closingTarget?: string
  maxTurns?: number
  /**
   * The most tool-less replies to the caller one phase may make: a reply
   * counts when the model finishes a turn that followed caller speech and
   * made no tool call, and a tool call or a phase change starts the count
   * over. Once the count exceeds this, the call moves to closingTarget after
   * the caller has heard that reply. 0 or unset is off; setting it requires
   * closingTarget.
   */
  maxTurnsWithoutTool?: number
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
 * One way into a phase that no phase owns: a global rule, which any phase can
 * fire, or a target the engine sends a call to on its own — the fallback when
 * a call runs too long, the closing target when it will not end by itself.
 */
export type GlobalEntry =
  | { kind: 'rule'; to: string; rule: FlowTransition }
  | { kind: 'fallback' | 'closing'; to: string }

/**
 * Every global way into a phase, in the order the loader would consider them,
 * limited to targets that exist. The one list the reachability check and the
 * graph both read, so a new engine-chosen target is added in one place.
 */
export function globalEntries(spec: FlowSpec): GlobalEntry[] {
  const nodes = spec.nodes ?? {}
  const exists = (id: string | undefined): id is string =>
    Boolean(id) && Boolean(nodes[id as string])
  const entries: GlobalEntry[] = []
  for (const rule of spec.global?.transitions ?? []) {
    if (exists(rule.target)) entries.push({ kind: 'rule', to: rule.target, rule })
  }
  const fallback = spec.global?.fallbackTarget
  if (exists(fallback)) entries.push({ kind: 'fallback', to: fallback })
  const closing = spec.global?.closingTarget
  if (exists(closing)) entries.push({ kind: 'closing', to: closing })
  return entries
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
  for (const entry of globalEntries(spec)) enter(entry.to)

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
 * validation report. It is the smallest flow that does — a working phase, the
 * goodbye phase the engine closes a call in (global.closingTarget), a persona,
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
      // A goodbye phase the engine can reach on its own, so a caller who
      // stops answering is not re-prompted forever (issue #9). No
      // maxTurnsWithoutTool: the starter's one working phase has no tools, so
      // a wall would count every exchange of the call.
      closingTarget: 'goodbye',
      transitions: [{ on: 'TOOL_RESULT', tool: 'hangup', target: 'goodbye' }],
    },
    nodes: {
      welcome: {
        // The greeting as a line rather than as an instruction, because a
        // first flow is also where an author learns the difference — and
        // because a deployment whose provider only says what it is given
        // needs one on every phase a call cannot leave.
        announce: {
          en: 'Thanks for calling. How can I help you today?',
          zh: '感谢致电，请问有什么可以帮您？',
        },
        instruction: {
          en: 'Find out what the caller needs and help them with it. When they say ' +
            'they need nothing else, call hangup at once and do not ask again.',
          zh: '了解来电者的需求，并帮助他们解决。对方说没有其它需要了，就立即调用 hangup，不要再问一次。',
        },
        tools: [],
      },
      goodbye: {
        announce: {
          en: 'Thank you for calling, goodbye.',
          zh: '感谢来电，再见。',
        },
        instruction: {
          en: 'The line hangs up once this has been said; ask no further questions.',
          zh: '说完这句线路会自动挂断，不要再问问题。',
        },
        tools: [],
        isTerminal: true,
      },
    },
  }
}
