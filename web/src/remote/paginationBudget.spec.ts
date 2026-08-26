import { describe, expect, it } from 'vitest'

import { PaginationBudget, PaginationBudgetExceededError } from './paginationBudget'

const createBudget = (overrides: Record<string, unknown> = {}) =>
  new PaginationBudget('测试目录', {
    maximumPages: 2,
    maximumItems: 3,
    maximumBytes: 64,
    maximumCursorBytes: 4,
    ...overrides,
  })

describe('PaginationBudget', () => {
  it('rejects another request after the page budget is exhausted', () => {
    const budget = createBudget()
    budget.assertCanRequestPage()
    budget.admitPage([])
    budget.assertCanRequestPage()
    budget.admitPage([])

    expect(() => budget.assertCanRequestPage()).toThrowError(
      new PaginationBudgetExceededError('测试目录页数超过客户端安全上限。'),
    )
  })

  it('checks cumulative items and encoded bytes before admission', () => {
    const itemBudget = createBudget()
    itemBudget.admitPage([1, 2])
    expect(() => itemBudget.admitPage([3, 4])).toThrow(/条目数量/)

    const byteBudget = createBudget({ maximumBytes: 8 })
    expect(() => byteBudget.admitPage(['12345678'])).toThrow(/累计字节数/)
  })

  it('rejects repeated, empty, and oversized UTF-8 cursors', () => {
    const repeated = createBudget()
    expect(repeated.admitCursor('next')).toBe('next')
    expect(() => repeated.admitCursor('next')).toThrow(/游标重复/)
    expect(() => createBudget().admitCursor('')).toThrow(/游标无效/)
    expect(() => createBudget().admitCursor('分页')).toThrow(/游标超过/)
  })

  it('enforces a wall-clock budget before another page is retained', () => {
    let now = 10
    const budget = createBudget({
      maximumDurationMilliseconds: 100,
      now: () => now,
    })
    budget.admitPage([])
    now = 110

    expect(() => budget.admitPage([])).toThrow(/单轮读取时间/)
  })
})
