import fs from 'node:fs/promises'
import path from 'node:path'
import ts from 'typescript'
import { describe, expect, it } from 'vitest'

/**
 * Static gate: a full-width row is a DetailRow, never a hand-built
 * `<Td colSpan>`. Content in such a cell sizes the whole table (a long
 * unbroken line once pushed a History row's buttons off the right edge), and
 * the fix belongs in components/table.tsx, not in each page.
 *
 * Exceptions are keyed by file and enclosing function, never a line number, so
 * they survive edits. An exception that matches nothing fails too.
 */

const SRC = path.resolve(import.meta.dirname, '..')

const EXCEPTIONS: Record<string, string> = {
  'routes/_app.admin.bots.$flowId.tsx#ToolTable':
    'a note spanning three ordinary columns of the Tools table; not a detail row under a record',
}

/** Keys (`file#function`) of every raw `<td colSpan>` or `<Td colSpan>` in a source. */
function findColSpanCells(file: string, source: string): string[] {
  const sf = ts.createSourceFile(file, source, ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX)
  const found: string[] = []
  const visit = (node: ts.Node) => {
    if (ts.isJsxOpeningElement(node) || ts.isJsxSelfClosingElement(node)) {
      const tag = node.tagName.getText(sf)
      const hasColSpan = node.attributes.properties.some(
        (a) => ts.isJsxAttribute(a) && a.name.getText(sf) === 'colSpan',
      )
      if ((tag === 'Td' || tag === 'td') && hasColSpan) found.push(`${file}#${enclosingName(node, sf)}`)
    }
    ts.forEachChild(node, visit)
  }
  visit(sf)
  return found
}

function enclosingName(node: ts.Node, sf: ts.SourceFile): string {
  for (let n: ts.Node | undefined = node.parent; n; n = n.parent) {
    if (ts.isFunctionDeclaration(n) && n.name) return n.name.text
    if (ts.isVariableDeclaration(n) && n.initializer && (ts.isArrowFunction(n.initializer) || ts.isFunctionExpression(n.initializer))) {
      return n.name.getText(sf)
    }
  }
  return '<module>'
}

/** Cells that are neither registered nor allowed, as failure lines. */
function violations(cells: string[], exceptions: Record<string, string>): string[] {
  return cells.filter((key) => !(key in exceptions))
}

function staleExceptions(cells: string[], exceptions: Record<string, string>): string[] {
  return Object.keys(exceptions).filter((key) => !cells.includes(key))
}

async function sourceFiles(dir: string): Promise<string[]> {
  const out: string[] = []
  for (const entry of await fs.readdir(dir, { withFileTypes: true })) {
    const full = path.join(dir, entry.name)
    if (entry.isDirectory()) {
      if (entry.name === 'generated' || entry.name === 'test') continue
      out.push(...(await sourceFiles(full)))
    } else if (entry.name.endsWith('.tsx') && !/\.test\.tsx?$/.test(entry.name)) {
      out.push(full)
    }
  }
  return out
}

describe('findColSpanCells', () => {
  it('flags a Td with colSpan', () => {
    const src = `function Bad() { return <Tr><Td colSpan={4}>x</Td></Tr> }`
    expect(findColSpanCells('routes/a.tsx', src)).toEqual(['routes/a.tsx#Bad'])
  })

  it('catches colSpan after className on a multi-line tag', () => {
    const src = `const Bad = () => (
  <Td
    className="pb-3"
    align="left"
    colSpan={COLUMN_COUNT}
  >x</Td>
)`
    expect(findColSpanCells('routes/a.tsx', src)).toEqual(['routes/a.tsx#Bad'])
  })

  it('flags a raw lowercase td', () => {
    const src = `function Bad() { return <tr><td colSpan={2}>x</td></tr> }`
    expect(findColSpanCells('routes/a.tsx', src)).toEqual(['routes/a.tsx#Bad'])
  })

  it('does not flag TableMessage, DetailRow or a Td without colSpan', () => {
    const src = `function Ok() { return <><TableMessage colSpan={3}>x</TableMessage><DetailRow columns={3}>y</DetailRow><Td>z</Td></> }`
    expect(findColSpanCells('routes/a.tsx', src)).toEqual([])
  })

  it('passes a registered exception and fails an unregistered one', () => {
    const key = 'routes/a.tsx#Note'
    const cells = findColSpanCells('routes/a.tsx', `function Note() { return <Td colSpan={3}>x</Td> }`)
    expect(violations(cells, { [key]: 'why' })).toEqual([])
    expect(violations(cells, {})).toEqual([key])
  })

  it('reports a registered exception that matches nothing', () => {
    expect(staleExceptions([], { 'routes/gone.tsx#Gone': 'why' })).toEqual(['routes/gone.tsx#Gone'])
  })
})

describe('the source tree', () => {
  it('builds no full-width row by hand', async () => {
    const cells: string[] = []
    for (const file of await sourceFiles(SRC)) {
      const rel = path.relative(SRC, file).split(path.sep).join('/')
      if (rel === 'components/table.tsx') continue
      cells.push(...findColSpanCells(rel, await fs.readFile(file, 'utf8')))
    }
    const bad = violations(cells, EXCEPTIONS)
    expect(
      bad,
      `A <Td colSpan> outside components/table.tsx: ${bad.join(', ')}. A full-width row under a record is a <DetailRow columns={n}> ` +
        `(it keeps the content from sizing the table). A spanning note across ordinary columns is registered in EXCEPTIONS with a reason.`,
    ).toEqual([])
    const stale = staleExceptions(cells, EXCEPTIONS)
    expect(stale, `EXCEPTIONS entries that match no cell any more: ${stale.join(', ')}. Remove them.`).toEqual([])
  })
})
