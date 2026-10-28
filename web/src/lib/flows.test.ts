import { describe, expect, it } from 'vitest'

import { ApiError, type ApiErrorBody } from '@/lib/api'
import {
  describeRule, flowRefusal, globalEntries, locateNodes, sameSpec, starterSpec, textFor,
  textListFor, unreachableNodes,
  type FlowSpec,
} from '@/lib/flows'

/**
 * The flows screen's own reading of a spec. The server owns what may be saved;
 * these are the four judgements the screen makes on its own, and each of them
 * is wrong in a way that is invisible on the page.
 */

const CHAIN: FlowSpec = {
  id: 'probe',
  specVersion: 'v2',
  initialNode: 'welcome',
  global: { persona: 'A helpful assistant.', fallbackTarget: 'goodbye' },
  nodes: {
    welcome: {
      instruction: 'Greet the caller.',
      transitions: [{ on: 'TOOL_RESULT', tool: 'lookup', target: 'answer' }],
    },
    answer: { instruction: 'Answer.' },
    goodbye: { instruction: 'Say goodbye.', isTerminal: true },
    orphan: { instruction: 'Nothing reaches me.' },
  },
}

describe('sameSpec', () => {
  // The column is jsonb, which keeps neither key order nor whitespace. Compare
  // the text and every saved draft stays marked unsaved for ever, with Publish
  // — which wants a clean draft — permanently out of reach.
  // Order in a list is the order rules are evaluated in, so it is a change.
  it.each([
    ['key order', { id: 'probe', nodes: { a: { tools: ['x'] } } },
      { nodes: { a: { tools: ['x'] } }, id: 'probe' }, true],
    ['a changed value', { global: { maxTurns: 30 } }, { global: { maxTurns: 40 } }, false],
    ['a reordered list', { tools: ['a', 'b'] }, { tools: ['b', 'a'] }, false],
  ])('judges %s', (_case, a, b, same) => {
    expect(sameSpec(a, b)).toBe(same)
  })
})

describe('unreachableNodes', () => {
  it('names a phase nothing transitions to', () => {
    expect(unreachableNodes(CHAIN)).toEqual(['orphan'])
  })

  // The engine can move a call to closingTarget with no transition naming it
  // — from the NO_INPUT default or the turns-without-a-tool wall — so it must
  // count as reached the same way fallbackTarget does.
  it('counts the global closing target as reached', () => {
    const withClosing: FlowSpec = {
      initialNode: 'welcome',
      global: { closingTarget: 'farewell' },
      nodes: {
        welcome: { instruction: 'Greet.' },
        farewell: { instruction: 'Say goodbye.', isTerminal: true },
      },
    }
    expect(unreachableNodes(withClosing)).toEqual([])
  })

  // A flow whose phases form a cycle is legitimate; walking it must terminate.
  it('terminates on a cycle', () => {
    const looped: FlowSpec = {
      initialNode: 'a',
      nodes: {
        a: { transitions: [{ target: 'b' }] },
        b: { transitions: [{ target: 'a' }] },
      },
    }
    expect(unreachableNodes(looped)).toEqual([])
  })
})

describe('locateNodes', () => {
  // The editor scrolls to what the graph selected, so the range has to be the
  // phase's whole block — brace counting, not the first closing brace.
  it('spans a phase with nested objects', () => {
    const text = JSON.stringify(CHAIN, null, 2)
    const found = locateNodes(text, ['welcome'])
    const [start, end] = found.welcome

    const block = text.slice(start, end)
    expect(block.startsWith('"welcome"')).toBe(true)
    expect(block).toContain('"target": "answer"')
    expect(JSON.parse(`{${block}}`)).toHaveProperty('welcome.instruction')
  })

  it('says nothing about a phase that is not in the text', () => {
    expect(locateNodes(JSON.stringify(CHAIN, null, 2), ['nope'])).toEqual({})
  })
})

describe('textFor', () => {
  it('reads a bare string as the same wording in every language', () => {
    expect(textFor('Hello', 'zh')).toBe('Hello')
  })

  // A missing translation should leave the reader the wrong language rather
  // than an empty box — the same fallback the server applies.
  it('falls back to the other language', () => {
    expect(textFor({ en: 'Hello' }, 'zh')).toBe('Hello')
    expect(textListFor({ en: ['One'] }, 'zh')).toEqual(['One'])
  })
})

describe('describeRule', () => {
  it.each([
    [{ on: 'TOOL_RESULT', tool: 'lookup', condition: { slot: 'result.ok', op: 'EQ', value: true },
      target: 'answer' }, 'lookup · ok = true'],
    [{ on: 'NO_INPUT', target: 'goodbye' }, 'no input'],
    [{ tool: 'lookup', target: 'answer', condition: { all: [
      { slot: 'result.ok', op: 'EQ', value: true },
      { slot: 'result.count', op: 'GTE', value: 3 },
    ] } }, 'lookup · ok = true & count ≥ 3'],
  ] as const)('reads %o as %s', (rule, text) => {
    expect(describeRule(rule as Parameters<typeof describeRule>[0])).toBe(text)
  })
})

describe('starterSpec', () => {
  /**
   * A first flow is where an author learns the difference between a brief and
   * a line, so the starter shows both. It also has to publish as it stands on
   * any deployment, and one whose provider only says what it is given needs
   * the entry phase to carry its greeting.
   */
  it('gives its one phase a line of its own, in both languages', () => {
    const welcome = starterSpec('probe').nodes?.welcome
    expect(textFor(welcome?.announce, 'en')).not.toBe('')
    expect(textFor(welcome?.announce, 'zh')).not.toBe('')
    expect(textFor(welcome?.instruction, 'en')).not.toBe(textFor(welcome?.announce, 'en'))
  })

  // A caller's decline is otherwise invisible to the engine, so the starter
  // shows the fix (issue #9) from a first author's very first flow: a
  // terminal goodbye phase named as the closing target.
  it('names a terminal closing target the caller cannot decline out of', () => {
    const spec = starterSpec('probe')
    const target = spec.global?.closingTarget
    expect(target).toBeTruthy()
    expect(spec.nodes?.[target as string]?.isTerminal).toBe(true)
    expect(unreachableNodes(spec)).toEqual([])
  })
})

describe('globalEntries', () => {
  // The one list the reachability check and the graph both read: every
  // global rule, then the fallback, then the closing target — only where the
  // target exists — so two entries into one phase are both kept.
  it('lists rules, the fallback and the closing target that exist', () => {
    const spec: FlowSpec = {
      initialNode: 'welcome',
      global: {
        fallbackTarget: 'handoff',
        closingTarget: 'farewell',
        transitions: [
          { on: 'TOOL_RESULT', tool: 'hangup', target: 'farewell' },
          { on: 'TOOL_RESULT', tool: 'nowhere', target: 'missing' },
        ],
      },
      nodes: {
        welcome: { instruction: 'Greet.' },
        handoff: { instruction: 'Transfer.' },
        farewell: { instruction: 'Say goodbye.', isTerminal: true },
      },
    }
    expect(globalEntries(spec).map((entry) => [entry.kind, entry.to])).toEqual([
      ['rule', 'farewell'],
      ['fallback', 'handoff'],
      ['closing', 'farewell'],
    ])
  })
})

const refusal = (status: number, code: ApiErrorBody['code'], params?: Record<string, unknown>) =>
  new ApiError(status, { code, message: 'refused', params })

describe('flowRefusal', () => {
  // What the editor lists beyond the code's own sentence: one reading per
  // refusal that names something, so the notice and the dialog agree.
  it("reads the loader's problems, the phases at fault and the campaigns in the way", () => {
    expect(
      flowRefusal(
        refusal(422, 'VALIDATION_FAILED', {
          problems: ['phase "billing_review" has no instruction', 7, 'the flow has no id'],
        }),
      ),
    ).toEqual({
      kind: 'PROBLEMS',
      items: ['phase "billing_review" has no instruction', 'the flow has no id'],
    })
    expect(
      flowRefusal(refusal(422, 'TERMINAL_ANNOUNCE_REQUIRED', { nodes: ['goodbye', 'handed_over'] })),
    ).toEqual({ kind: 'PHASES', items: ['goodbye', 'handed_over'] })
    expect(
      flowRefusal(
        refusal(409, 'CAMPAIGN_FLOW_HAS_TRANSFER', { campaigns: 3, rule: 'PUBLISH_BLOCKED' }),
      ),
    ).toEqual({ kind: 'CAMPAIGNS', count: 3 })
  })

  // Everything else keeps the code's own sentence: a refusal that names
  // nothing must not become an empty list, and the same code refusing a
  // campaign form or a number's rebinding is not a publish refusal.
  it.each([
    ['STORAGE_DOWN', refusal(503, 'STORAGE_DOWN')],
    ['NOT_FOUND', refusal(404, 'NOT_FOUND')],
    ['a network failure', new TypeError('Failed to fetch')],
    ['VALIDATION_FAILED without params', refusal(422, 'VALIDATION_FAILED')],
    ['VALIDATION_FAILED with no problems', refusal(422, 'VALIDATION_FAILED', { problems: [] })],
    [
      'VALIDATION_FAILED naming a field',
      refusal(422, 'VALIDATION_FAILED', { field: 'note', rule: 'NOTE_TOO_LONG' }),
    ],
    [
      'TERMINAL_ANNOUNCE_REQUIRED naming no phase',
      refusal(422, 'TERMINAL_ANNOUNCE_REQUIRED', { nodes: [] }),
    ],
    [
      "the campaign form's refusal",
      refusal(422, 'CAMPAIGN_FLOW_HAS_TRANSFER', {
        field: 'didId',
        rule: 'FLOW_CAN_TRANSFER',
        didNumber: '95099',
      }),
    ],
    [
      'a rebinding refusal',
      refusal(409, 'CAMPAIGN_FLOW_HAS_TRANSFER', { campaigns: 2, rule: 'DID_REBIND_BLOCKED' }),
    ],
    [
      'a publish block without a count',
      refusal(409, 'CAMPAIGN_FLOW_HAS_TRANSFER', { rule: 'PUBLISH_BLOCKED' }),
    ],
  ])('names nothing for %s', (_name, error) => {
    expect(flowRefusal(error)).toBeNull()
  })
})
