import { render } from '@testing-library/react'
import { describe, expect, it } from 'vitest'

import { DataTable, DetailRow, TBody } from './table'

describe('DetailRow', () => {
  it('is one row with one cell spanning the columns, holding a zero-width wrapper', () => {
    const { container } = render(
      <DataTable>
        <TBody>
          <DetailRow columns={4}>
            <p>spec</p>
          </DetailRow>
        </TBody>
      </DataTable>,
    )
    const rows = container.querySelectorAll('tr')
    expect(rows).toHaveLength(1)
    const cells = rows[0].querySelectorAll('td')
    expect(cells).toHaveLength(1)
    expect(cells[0].colSpan).toBe(4)
    const wrapper = cells[0].firstElementChild as HTMLElement
    expect(wrapper.classList.contains('w-0')).toBe(true)
    expect(wrapper.classList.contains('min-w-full')).toBe(true)
    expect(wrapper.textContent).toBe('spec')
    expect(rows[0].classList.contains('hover:bg-transparent')).toBe(true)
  })
})
