import { create, fromBinary, toBinary } from '@bufbuild/protobuf'
import { describe, expect, it, vi } from 'vitest'

import { StreamCloseSchema } from '@/generated/remote/v2/channel_pb'
import { CarrierStreamRejectedSchema } from '@/generated/remote/v2/carrier_pb'
import {
  Direction,
  FrameType,
  ProtocolErrorCode,
  type EncryptedRecord,
} from '@/generated/remote/v2/common_pb'
import {
  LinkLeaseRenewedSchema,
  LinkReadySchema,
  type LinkAccept,
  type LinkEnvelope,
  type LinkLeaseRenewed,
} from '@/generated/remote/v2/device_link_pb'

import type { V2Carrier } from './carrier'
import { V2CarrierInterruptedError, V2Link, type V2LinkFrame } from './link'

type ResumeLinkHarness = {
  id: string
  active: boolean
  closed: boolean
  carrierReady: boolean
  carrier: V2Carrier
  binding: object
  carrierWaiters: Set<unknown>
  validateAccept: (binding: object, accept: LinkAccept) => object
  retryPendingRekey: () => Promise<void>
  readonly isResumePending: boolean
  resume: V2Link['resume']
  retryPendingResume: V2Link['retryPendingResume']
  attachCarrier: V2Link['attachCarrier']
  waitForCarrier: V2Link['waitForCarrier']
  handleCarrierInterrupted: V2Link['handleCarrierInterrupted']
  handleLinkEnvelope: V2Link['handleLinkEnvelope']
}

const resumeHarness = () => {
  const carrier = {
    isOpen: true,
    resume: vi.fn().mockResolvedValue(undefined),
  } as unknown as V2Carrier
  const binding = {}
  const link = Object.create(V2Link.prototype) as ResumeLinkHarness
  Object.assign(link, {
    id: crypto.randomUUID(),
    active: true,
    closed: false,
    carrierReady: true,
    carrier,
    binding,
    carrierWaiters: new Set(),
    validateAccept: vi.fn().mockReturnValue(binding),
    retryPendingRekey: vi.fn().mockResolvedValue(undefined),
  })
  return { link, carrier }
}

const acceptEnvelope = (linkId: string) =>
  ({
    linkId,
    body: { case: 'linkAccept', value: {} as LinkAccept },
  }) as LinkEnvelope

describe('remote/v2 Link transport feedback', () => {
  it('negotiates and acknowledges the 30-minute encrypted Device lease', async () => {
    const link = Object.create(V2Link.prototype) as unknown as {
      id: string
      activeKeyId: bigint
      active: boolean
      closed: boolean
      carrierReady: boolean
      leaseRenewalSequence: bigint
      leaseRenewalInterval?: number
      leaseDuration?: number
      leaseExpiresAt?: number
      leaseRenewalTimer?: ReturnType<typeof setTimeout>
      leaseExpiryTimer?: ReturnType<typeof setTimeout>
      pendingLeaseRenewal?: { sequence: bigint }
      readyResolve: () => void
      sendMessage: ReturnType<typeof vi.fn>
      dispatch: (record: EncryptedRecord, plaintext: Uint8Array) => Promise<void>
      renewLease: () => Promise<void>
      handleLeaseRenewed: (value: LinkLeaseRenewed) => void
    }
    Object.assign(link, {
      id: crypto.randomUUID(),
      activeKeyId: 1n,
      active: false,
      closed: false,
      carrierReady: true,
      leaseRenewalSequence: 0n,
      readyResolve: vi.fn(),
      sendMessage: vi.fn().mockResolvedValue(undefined),
    })
    const ready = create(LinkReadySchema, {
      linkId: link.id,
      activeKeyId: 1n,
      leaseRenewalIntervalSeconds: 1800,
      leaseDurationSeconds: 5400,
    })
    await link.dispatch(
      {
        frameType: FrameType.LINK_READY,
        direction: Direction.DEVICE_TO_CLIENT,
      } as EncryptedRecord,
      toBinary(LinkReadySchema, ready),
    )

    const renewing = link.renewLease()
    await Promise.resolve()
    expect(link.sendMessage).toHaveBeenCalledWith(
      FrameType.LINK_LEASE_RENEW,
      'v2-control',
      'v2-control',
      expect.anything(),
      expect.objectContaining({ linkId: link.id, renewalSequence: 1n }),
    )
    link.handleLeaseRenewed(
      create(LinkLeaseRenewedSchema, {
        linkId: link.id,
        renewalSequence: 1n,
        leaseRenewalIntervalSeconds: 1800,
        leaseDurationSeconds: 5400,
      }),
    )
    await renewing

    expect(link.active).toBe(true)
    expect(link.leaseRenewalSequence).toBe(1n)
    expect(link.leaseExpiresAt).toBeGreaterThan(Date.now() + 89 * 60_000)
    if (link.leaseRenewalTimer) clearTimeout(link.leaseRenewalTimer)
    if (link.leaseExpiryTimer) clearTimeout(link.leaseExpiryTimer)
  })

  it('contains Relay backpressure to the rejected Stream', () => {
    const linkId = crypto.randomUUID()
    const rejectedStream = crypto.randomUUID()
    const siblingStream = crypto.randomUUID()
    const link = Object.create(V2Link.prototype) as {
      closed: boolean
      id: string
      activeKeyId: bigint
      activeStreams: Map<string, string>
      listeners: Map<FrameType, Set<(frame: V2LinkFrame) => void>>
      handleCarrierStreamRejected: V2Link['handleCarrierStreamRejected']
    }
    link.closed = false
    link.id = linkId
    link.activeKeyId = 1n
    link.activeStreams = new Map([
      [rejectedStream, 'channel'],
      [siblingStream, 'channel'],
    ])
    let closedStream = ''
    link.listeners = new Map([
      [
        FrameType.STREAM_CLOSE,
        new Set([
          (frame: V2LinkFrame) => {
            closedStream = fromBinary(StreamCloseSchema, frame.plaintext).streamId
          },
        ]),
      ],
    ])

    const handled = link.handleCarrierStreamRejected(
      create(CarrierStreamRejectedSchema, {
        linkId,
        channelId: 'channel',
        streamId: rejectedStream,
        reason: ProtocolErrorCode.BACKPRESSURE,
      }),
    )

    expect(handled).toBe(true)
    expect(closedStream).toBe(rejectedStream)
    expect(link.activeStreams.has(rejectedStream)).toBe(false)
    expect(link.activeStreams.has(siblingStream)).toBe(true)
  })

  it('waits for the Device LinkAccept before declaring Carrier resume ready', async () => {
    const { link, carrier: previousCarrier } = resumeHarness()
    const replacement = {
      isOpen: true,
      resume: vi.fn().mockResolvedValue(undefined),
    } as unknown as V2Carrier
    link.attachCarrier(replacement)

    let resumed = false
    let carrierReady = false
    const waiting = link.waitForCarrier(1000).then(() => {
      carrierReady = true
    })
    const recovery = link.resume([], 1000).then(() => {
      resumed = true
    })
    await Promise.resolve()
    expect(replacement.resume).toHaveBeenCalledTimes(1)
    expect(resumed).toBe(false)
    expect(carrierReady).toBe(false)

    // A late callback from the superseded Carrier must not cancel recovery.
    link.handleCarrierInterrupted(previousCarrier, new Error('old Carrier closed'))
    await Promise.resolve()
    expect(resumed).toBe(false)

    await link.handleLinkEnvelope(acceptEnvelope(link.id))
    await Promise.all([recovery, waiting])
    expect(link.validateAccept).toHaveBeenCalledTimes(1)
    expect(resumed).toBe(true)
    expect(carrierReady).toBe(true)
  })

  it('does not deliver encrypted application frames before resume proof', async () => {
    const { link } = resumeHarness()
    const replacement = {
      isOpen: true,
      resume: vi.fn().mockResolvedValue(undefined),
    } as unknown as V2Carrier
    const handleEncrypted = vi.fn().mockResolvedValue(undefined)
    Object.assign(link, { handleEncrypted })
    link.attachCarrier(replacement)

    const recovery = link.resume([], 1000)
    await Promise.resolve()
    const encrypted = {
      linkId: link.id,
      body: { case: 'encrypted', value: {} as EncryptedRecord },
    } as LinkEnvelope
    await link.handleLinkEnvelope(encrypted)
    expect(handleEncrypted).not.toHaveBeenCalled()

    await link.handleLinkEnvelope(acceptEnvelope(link.id))
    await recovery
    await link.handleLinkEnvelope(encrypted)
    expect(handleEncrypted).toHaveBeenCalledTimes(1)
  })

  it('retransmits a pending Resume without creating a competing LinkAccept waiter', async () => {
    const { link, carrier } = resumeHarness()
    const recovery = link.resume([], 1000)
    await Promise.resolve()

    expect(link.isResumePending).toBe(true)
    await link.retryPendingResume([])
    expect(carrier.resume).toHaveBeenCalledTimes(2)

    await link.handleLinkEnvelope(acceptEnvelope(link.id))
    await recovery
    expect(link.isResumePending).toBe(false)
    expect(link.active).toBe(true)
  })

  it('fails an interrupted resume promptly while retaining resumable Link state', async () => {
    const { link } = resumeHarness()
    const replacement = {
      isOpen: true,
      resume: vi.fn().mockResolvedValue(undefined),
    } as unknown as V2Carrier
    link.attachCarrier(replacement)

    const recovery = link.resume([], 1000)
    await Promise.resolve()
    link.handleCarrierInterrupted(replacement, new Error('current Carrier closed'))

    await expect(recovery).rejects.toBeInstanceOf(V2CarrierInterruptedError)
    expect(link.active).toBe(true)
    expect(link.closed).toBe(false)
  })
})
