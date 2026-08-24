import { describe, expect, it } from 'vitest'

import {
  describeRule, locateNodes, sameSpec, textFor, textListFor, unreachableNodes,
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
  it('does not count key order or formatting as a change', () => {
    const asWritten = { id: 'probe', specVersion: 'v2', nodes: { a: { tools: ['x'] } } }
    const asReturned = { nodes: { a: { tools: ['x'] } }, specVersion: 'v2', id: 'probe' }

    expect(sameSpec(asWritten, asReturned)).toBe(true)
    expect(JSON.stringify(asWritten)).not.toBe(JSON.stringify(asReturned))
  })

  it('counts a changed value as a change', () => {
    expect(sameSpec({ global: { maxTurns: 30 } }, { global: { maxTurns: 40 } })).toBe(false)
  })

  // Order in a list is the order rules are evaluated in, so it is a change.
  it('counts a reordered list as a change', () => {
    expect(sameSpec({ tools: ['a', 'b'] }, { tools: ['b', 'a'] })).toBe(false)
  })
})

describe('unreachableNodes', () => {
  it('names a phase nothing transitions to', () => {
    expect(unreachableNodes(CHAIN)).toEqual(['orphan'])
  })

  it('counts the entry phase and the global fallback as reached', () => {
    const found = unreachableNodes(CHAIN)
    expect(found).not.toContain('welcome')
    expect(found).not.toContain('goodbye')
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
  it('reads a tool result with a condition', () => {
    expect(
      describeRule({
        on: 'TOOL_RESULT',
        tool: 'lookup',
        condition: { slot: 'result.ok', op: 'EQ', value: true },
        target: 'answer',
      }),
    ).toBe('lookup · ok = true')
  })

  it('reads a rule that fires on silence', () => {
    expect(describeRule({ on: 'NO_INPUT', target: 'goodbye' })).toBe('no input')
  })

  it('joins a conjunction', () => {
    expect(
      describeRule({
        tool: 'lookup',
        condition: {
          all: [
            { slot: 'result.ok', op: 'EQ', value: true },
            { slot: 'result.count', op: 'GTE', value: 3 },
          ],
        },
        target: 'answer',
      }),
    ).toBe('lookup · ok = true & count ≥ 3')
  })
})
