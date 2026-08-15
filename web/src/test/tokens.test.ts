import fs from 'node:fs/promises'
import path from 'node:path'
import { beforeAll, describe, expect, it } from 'vitest'
import { compile } from 'tailwindcss'

/**
 * Guards on the token layer, asserted against the real compiled stylesheet.
 *
 * Both invariants below were live defects found by measuring the running app
 * against the reference (see `docs/ui-spec.md`, pass 2), and neither is
 * visible in a component test: jsdom implements no `@layer` at all — a layered
 * rule simply does not apply there — so the cascade has to be checked on the
 * compiled CSS instead. The rendered result is verified in a real browser; the
 * job here is to fail the moment the stylesheet stops being able to produce it.
 */

const CSS_PATH = path.resolve(import.meta.dirname, '../index.css')

/** The utilities these assertions look at. Tailwind only emits what is used. */
const CANDIDATES = [
  'border',
  'border-transparent',
  'text-xs',
  'text-sm',
  'text-base',
  'text-lg',
  'text-xl',
]

let css = ''

beforeAll(async () => {
  const source = await fs.readFile(CSS_PATH, 'utf8')
  const compiler = await compile(source, {
    base: path.dirname(CSS_PATH),
    loadStylesheet: async (id, base) => {
      // Only Tailwind's own entry point matters here; the font package
      // contributes @font-face rules and nothing this file asserts on.
      const file = id === 'tailwindcss' ? 'tailwindcss/index.css' : null
      return {
        path: id,
        base,
        content: file
          ? await fs.readFile(path.resolve(import.meta.dirname, '../../node_modules', file), 'utf8')
          : '',
      }
    },
  })
  css = compiler.build(CANDIDATES)
})

/**
 * Walks the compiled CSS and reports the `@layer` a declaration sits in, or
 * `null` when it is unlayered. Unlayered rules outrank every layered one
 * regardless of specificity, which is the whole point of the first test.
 */
function layerOf(sheet: string, needle: string): string | null | undefined {
  const at = sheet.indexOf(needle)
  if (at === -1) return undefined

  let layer: string | null = null
  let depth = 0
  let layerDepth = -1
  const opener = /@layer\s+([\w-]+)\s*\{|\{|\}/g
  let m: RegExpExecArray | null
  while ((m = opener.exec(sheet)) !== null && m.index < at) {
    if (m[1] !== undefined) {
      layer = m[1]
      layerDepth = depth
      depth++
    } else if (m[0] === '{') {
      depth++
    } else {
      depth--
      if (depth <= layerDepth) {
        layer = null
        layerDepth = -1
      }
    }
  }
  return layer
}

describe('the default border colour', () => {
  // The bug: `* { border-color: var(--border) }` sat outside every layer, so
  // it beat `.border-transparent`, `.border-primary` and every other border
  // utility on every element in the product — silently, since the default
  // colour is the one most elements want anyway.
  it('is layered, so border-colour utilities can still win', () => {
    const universal = css.match(/\*\s*\{\s*border-color:\s*var\(--border\);?\s*\}/)
    expect(universal, 'no universal default border colour in the stylesheet').not.toBeNull()
    expect(layerOf(css, universal![0])).toBe('base')
  })

  it('leaves .border-transparent able to paint nothing', () => {
    expect(css).toMatch(/\.border-transparent\s*\{\s*border-color:\s*transparent/)
    expect(layerOf(css, '.border-transparent')).toBe('utilities')
  })

  it('has no unlayered universal rule at all', () => {
    // Anything matching `*` from outside a layer would reintroduce the same
    // class of bug for a different property.
    const universal = /(^|\})\s*\*\s*(,[^{]*)?\{/g
    for (const match of css.matchAll(universal)) {
      const at = match.index + match[0].length
      expect(layerOf(css.slice(0, at + 1), css.slice(at - match[0].length, at))).not.toBeNull()
    }
  })
})

describe('the type scale', () => {
  // Tailwind's defaults are ratios tuned to a 14px `sm`; against this
  // product's 13px base they resolve to 18.57px and leave every row half a
  // pixel short of the design. The scale is absolute for that reason.
  const expected: Array<[string, string, string]> = [
    ['text-xs', '12px', '16px'],
    ['text-sm', '13px', '20px'],
    ['text-base', '14px', '20px'],
    ['text-lg', '16px', '24px'],
    ['text-xl', '20px', '28px'],
  ]

  it.each(expected)('%s is %s / %s', (utility, size, lineHeight) => {
    const rule = new RegExp(`\\.${utility}\\s*\\{([^}]*)\\}`)
    const body = css.match(rule)?.[1]
    expect(body, `${utility} is not in the compiled stylesheet`).toBeDefined()
    expect(body).toContain(`font-size: ${size}`)
    // Tailwind emits the paired line-height as an overridable custom property
    // with the theme value as its fallback.
    expect(body).toMatch(
      new RegExp(`line-height:\\s*(var\\(--tw-leading,\\s*)?${lineHeight}`),
    )
  })

  it('sets an absolute line-height on the body, not Preflight’s ratio', async () => {
    const source = await fs.readFile(CSS_PATH, 'utf8')
    expect(source).toMatch(/body\s*\{[^}]*line-height:\s*20px/)
  })
})
