import { afterEach, describe, expect, it, vi } from 'vitest'

import {
  isV2GrantInvalidatingLinkRejection,
  isV2RetryableFailure,
  isV2RetryableLinkFailure,
  isV2RecoverableLinkRejection,
  shouldRetainV2IssueIdempotency,
  V2ReconnectPolicy,
  v2FullJitterDelay,
  v2ReconnectAfterMillis,
  v2RetryAfterMillis,
} from './client'
import { ProtocolErrorCode } from '@/generated/remote/v2/common_pb'

const axiosFailure = (status: number, retryAfter?: string | number) => ({
  isAxiosError: true,
  response: { status, headers: retryAfter === undefined ? {} : { 'retry-after': retryAfter } },
})

describe('remote/v2 reconnect policy', () => {
  afterEach(() => {
    vi.restoreAllMocks()
    vi.useRealTimers()
  })

  it('keeps full jitter bounded while guaranteeing a non-zero retry floor', () => {
    vi.spyOn(Math, 'random').mockReturnValue(0)
    expect(v2FullJitterDelay(0)).toBe(250)
    expect(v2FullJitterDelay(20)).toBe(250)

    vi.spyOn(Math, 'random').mockReturnValue(0.999999)
    expect(v2FullJitterDelay(0)).toBe(500)
    expect(v2FullJitterDelay(20)).toBe(30_000)
  })

  it('treats Retry-After as a minimum and validates unsafe timer values', () => {
    vi.spyOn(Math, 'random').mockReturnValue(0)
    expect(v2FullJitterDelay(0, 12_000)).toBe(12_250)
    expect(v2ReconnectAfterMillis(0xffffffff)).toBe(0xffffffff)
    expect(v2FullJitterDelay(0, 0xffffffff)).toBe(0xffffffff + 250)
    expect(v2ReconnectAfterMillis(Number.POSITIVE_INFINITY)).toBe(0)
  })

  it('parses both delta-seconds and HTTP-date Retry-After values', () => {
    vi.useFakeTimers()
    vi.setSystemTime(new Date('2026-08-19T12:00:00Z'))
    expect(v2RetryAfterMillis(axiosFailure(503, '45'))).toBe(45_000)
    expect(v2RetryAfterMillis(axiosFailure(503, 'Wed, 19 Aug 2026 12:01:00 GMT'))).toBe(60_000)
  })

  it('retries offline/transient failures but stops blind authorization loops', () => {
    expect(isV2RetryableFailure(axiosFailure(404))).toBe(true)
    expect(isV2RetryableFailure(axiosFailure(503))).toBe(true)
    expect(isV2RetryableFailure(axiosFailure(403))).toBe(false)
    expect(isV2RetryableFailure(new Error('remote/v2 heartbeat_timeout'))).toBe(true)
    expect(isV2RetryableFailure(new Error('Controller identity validation failed.'))).toBe(false)
  })

  it('reuses an issue idempotency key only when the outcome is ambiguous', () => {
    expect(shouldRetainV2IssueIdempotency(axiosFailure(503))).toBe(true)
    expect(shouldRetainV2IssueIdempotency(axiosFailure(429))).toBe(true)
    expect(shouldRetainV2IssueIdempotency({ isAxiosError: true, response: undefined })).toBe(true)
    expect(shouldRetainV2IssueIdempotency(axiosFailure(404))).toBe(false)
    expect(shouldRetainV2IssueIdempotency(axiosFailure(403))).toBe(false)
  })

  it('does not turn cryptographic Link rejection into a blind reconnect loop', () => {
    expect(isV2RetryableLinkFailure(new Error('remote/v2 rekey timed out.'))).toBe(true)
    expect(isV2RetryableLinkFailure(new Error('设备身份握手验证失败。'))).toBe(false)
    expect(isV2RecoverableLinkRejection(ProtocolErrorCode.ROUTE_STALE)).toBe(true)
    expect(isV2RecoverableLinkRejection(ProtocolErrorCode.RESUME_EXPIRED)).toBe(true)
    expect(isV2RecoverableLinkRejection(ProtocolErrorCode.BACKPRESSURE)).toBe(true)
    expect(isV2RecoverableLinkRejection(ProtocolErrorCode.IDENTITY_INVALID)).toBe(false)
    expect(isV2GrantInvalidatingLinkRejection(ProtocolErrorCode.IDENTITY_INVALID)).toBe(true)
    expect(isV2RecoverableLinkRejection(ProtocolErrorCode.FRAME_INVALID)).toBe(false)
  })

  it('bounds one recovery episode by attempts and resets only after confirmation', () => {
    let now = Date.parse('2026-08-20T02:00:00Z')
    const policy = new V2ReconnectPolicy(
      3,
      5 * 60_000,
      () => now,
      (attempt) => 250 * (attempt + 1),
    )

    expect(policy.nextDelay()).toBe(250)
    expect(policy.nextDelay()).toBe(500)
    expect(policy.nextDelay()).toBe(750)
    expect(policy.attempts).toBe(3)
    expect(policy.nextDelay()).toBeUndefined()
    expect(policy.exhausted).toBe(true)

    policy.reset()
    now += 60_000
    expect(policy.attempts).toBe(0)
    expect(policy.nextDelay()).toBe(250)
  })

  it('uses the five-minute window instead of an attempt cap in production', () => {
    let now = Date.parse('2026-08-24T12:00:00Z')
    const policy = new V2ReconnectPolicy(
      0,
      5 * 60_000,
      () => now,
      () => 250,
    )
    for (let attempt = 0; attempt < 100; attempt += 1) {
      expect(policy.nextDelay()).toBe(250)
    }
    expect(policy.exhausted).toBe(false)
    now += 5 * 60_000
    expect(policy.exhausted).toBe(true)
  })

  it('clamps a server retry delay to the remaining recovery horizon', () => {
    let now = Date.parse('2026-08-20T02:00:00Z')
    const policy = new V2ReconnectPolicy(
      32,
      5 * 60_000,
      () => now,
      (_attempt, retryAfter) => retryAfter,
    )

    expect(policy.nextDelay(60 * 60_000)).toBe(5 * 60_000)
    now += 5 * 60_000
    expect(policy.durationExhausted).toBe(true)
    expect(policy.nextDelay()).toBeUndefined()
  })
})
