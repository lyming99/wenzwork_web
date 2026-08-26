const encoder = new TextEncoder()

export interface PaginationBudgetOptions {
  maximumPages: number
  maximumItems: number
  maximumBytes: number
  maximumCursorBytes?: number
  maximumDurationMilliseconds?: number
  now?: () => number
}

export class PaginationBudgetExceededError extends Error {
  constructor(message: string, options?: ErrorOptions) {
    super(message, options)
    this.name = 'PaginationBudgetExceededError'
  }
}

/**
 * Tracks the cumulative cost of a single automatic pagination pass.
 * Callers check before issuing a request and admit the returned items before
 * merging them into component state.
 */
export class PaginationBudget {
  private readonly startedAt: number
  private readonly cursors = new Set<string>()
  private pages = 0
  private items = 0
  private bytes = 0

  constructor(
    private readonly label: string,
    private readonly options: PaginationBudgetOptions,
  ) {
    if (
      options.maximumPages < 1 ||
      options.maximumItems < 1 ||
      options.maximumBytes < 1 ||
      (options.maximumCursorBytes ?? 512) < 1 ||
      (options.maximumDurationMilliseconds !== undefined && options.maximumDurationMilliseconds < 1)
    ) {
      throw new RangeError('分页预算必须为正数。')
    }
    this.startedAt = this.now()
  }

  assertCanRequestPage() {
    this.assertDuration()
    if (this.pages >= this.options.maximumPages) {
      throw new PaginationBudgetExceededError(`${this.label}页数超过客户端安全上限。`)
    }
  }

  admitPage<T>(items: readonly T[]) {
    this.assertDuration()
    if (!Array.isArray(items)) {
      throw new PaginationBudgetExceededError(`${this.label}分页数据格式无效。`)
    }
    if (this.pages >= this.options.maximumPages) {
      throw new PaginationBudgetExceededError(`${this.label}页数超过客户端安全上限。`)
    }
    const pageBytes = this.estimatedJsonBytes(items)
    if (items.length > this.options.maximumItems - this.items) {
      throw new PaginationBudgetExceededError(`${this.label}条目数量超过客户端安全上限。`)
    }
    if (pageBytes > this.options.maximumBytes - this.bytes) {
      throw new PaginationBudgetExceededError(`${this.label}累计字节数超过客户端安全上限。`)
    }
    this.pages += 1
    this.items += items.length
    this.bytes += pageBytes
  }

  admitCursor(value: unknown): string | undefined {
    if (value === null || value === undefined) return undefined
    if (typeof value !== 'string' || value.length === 0) {
      throw new PaginationBudgetExceededError(`${this.label}分页游标无效。`)
    }
    if (encoder.encode(value).byteLength > (this.options.maximumCursorBytes ?? 512)) {
      throw new PaginationBudgetExceededError(`${this.label}分页游标超过客户端安全上限。`)
    }
    if (this.cursors.has(value)) {
      throw new PaginationBudgetExceededError(`${this.label}分页游标重复。`)
    }
    this.cursors.add(value)
    return value
  }

  private estimatedJsonBytes(value: unknown) {
    try {
      return encoder.encode(JSON.stringify(value)).byteLength
    } catch (error) {
      throw new PaginationBudgetExceededError(`${this.label}包含无法编码的数据。`, {
        cause: error,
      })
    }
  }

  private assertDuration() {
    const maximum = this.options.maximumDurationMilliseconds
    if (maximum !== undefined && this.now() - this.startedAt >= maximum) {
      throw new PaginationBudgetExceededError(`${this.label}单轮读取时间超过客户端安全上限。`)
    }
  }

  private now() {
    return (this.options.now ?? Date.now)()
  }
}
