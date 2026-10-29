import { describe, expect, it } from 'vitest'

import {
  asciiRun, chineseRun, digits, hostileStrings, longUrl, maxLengthOf, mixedNote, nestedJson,
} from './hostile'

describe('the hostile values the layout tests feed screens', () => {
  it('have the lengths and shapes their names promise', () => {
    expect(asciiRun()).toHaveLength(6000)
    expect(asciiRun()).not.toMatch(/\s/)
    expect([...chineseRun()]).toHaveLength(3000)
    const url = longUrl()
    expect(url).toHaveLength(2000)
    expect(new URL(url).search.length).toBeGreaterThan(1900)
    expect(url).not.toMatch(/\s/)
    expect(digits()).toMatch(/^\d{40}$/)
    expect(mixedNote()).toMatch(/[一-鿿]/)
    expect(mixedNote()).toMatch(/[a-z]/i)
    expect(mixedNote()).toMatch(/\p{Extended_Pictographic}/u)
    expect(JSON.stringify(nestedJson(30)).match(/level_/g)).toHaveLength(30)
  })

  it('are deterministic, so a CI failure reproduces locally', () => {
    expect(hostileStrings()).toEqual(hostileStrings())
  })

  it('take their field limits from the contract', () => {
    // The two the issue names; if the contract moves, the values move with it.
    expect(maxLengthOf('APIKeyWrite', 'name')).toHaveLength(128)
    expect(maxLengthOf('FlowCreate', 'name')).toHaveLength(120)
    expect(() => maxLengthOf('FlowCreate', 'nonexistent')).toThrow(/no maxLength/)
  })
})
