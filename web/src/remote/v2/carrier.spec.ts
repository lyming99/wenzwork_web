import { create } from '@bufbuild/protobuf'
import { afterEach, describe, expect, it, vi } from 'vitest'

import { EncryptedRecordSchema, FrameType } from '@/generated/remote/v2/common_pb'
import { LinkEnvelopeSchema, LinkInitSchema } from '@/generated/remote/v2/device_link_pb'

import {
  V2Carrier,
  V2CarrierAdmissionError,
  V2CarrierBackpressureError,
  validateV2CarrierEndpoint,
} from './carrier'

describe('remote/v2 Carrier endpoint validation', () => {
  it('allows only credential-free v2 websocket endpoints', () => {
    expect(() =>
      validateV2CarrierEndpoint(new URL('wss://relay.example.test/v2/connect')),
    ).not.toThrow()
    expect(() =>
      validateV2CarrierEndpoint(new URL('wss://user@relay.example.test/v2/connect')),
    ).toThrow('无凭据')
    expect(() => validateV2CarrierEndpoint(new URL('wss://relay.example.test/v1/connect'))).toThrow(
      '/v2/connect',
    )
  })

  it('explains browser mixed-content failures before connecting', () => {
    expect(() =>
      validateV2CarrierEndpoint(new URL('ws://relay.example.test/v2/connect'), 'https:'),
    ).toThrow('HTTPS 页面不能连接 ws://')
  })
})

describe('remote/v2 Carrier weak-network bounds', () => {
  afterEach(() => vi.useRealTimers())

  it('preserves a pre-Ready authentication rejection for Grant cache invalidation', async () => {
    class FakeSocket extends EventTarget {
      readyState: number = WebSocket.OPEN
      bufferedAmount = 0
      send() {}
      close() {
        this.readyState = WebSocket.CLOSED
      }
    }
    const socket = new FakeSocket()
    const CarrierConstructor = V2Carrier as unknown as new (
      socket: WebSocket,
      id: string,
      epoch: bigint,
    ) => V2Carrier
    const carrier = new CarrierConstructor(socket as unknown as WebSocket, crypto.randomUUID(), 1n)
    const ready = carrier.waitReady(1000)

    socket.dispatchEvent(
      new CloseEvent('close', { reason: 'remote/v2 client authentication failed' }),
    )

    await expect(ready).rejects.toBeInstanceOf(V2CarrierAdmissionError)
  })

  it('fails closed when asynchronous inbound work exceeds its memory budget', () => {
    class FakeSocket extends EventTarget {
      readyState: number = WebSocket.OPEN
      bufferedAmount = 0
      closeCalls = 0

      send() {}

      close() {
        this.closeCalls += 1
        this.readyState = WebSocket.CLOSED
      }
    }

    const socket = new FakeSocket()
    const CarrierConstructor = V2Carrier as unknown as new (
      socket: WebSocket,
      id: string,
      epoch: bigint,
    ) => V2Carrier
    const carrier = new CarrierConstructor(socket as unknown as WebSocket, crypto.randomUUID(), 1n)
    let failure = ''
    carrier.setHandlers({ onClose: (error) => (failure = error.message) })

    // Dispatch synchronously, as browsers do. Decoding yields to a microtask,
    // so an unbounded Promise chain would retain every payload here.
    for (let index = 0; index < 80; index += 1) {
      socket.dispatchEvent(new MessageEvent('message', { data: new ArrayBuffer(1) }))
    }

    expect(failure).toBe('remote/v2 receive_backpressure')
    expect(carrier.isOpen).toBe(false)
    expect(socket.closeCalls).toBe(1)
  })

  it('detects a one-way acknowledgement blackhole', async () => {
    vi.useFakeTimers()
    class FakeSocket extends EventTarget {
      readyState: number = WebSocket.OPEN
      bufferedAmount = 0
      sent = 0
      send() {
        this.sent += 1
      }
      close() {
        this.readyState = WebSocket.CLOSED
      }
    }
    const socket = new FakeSocket()
    const CarrierConstructor = V2Carrier as unknown as new (
      socket: WebSocket,
      id: string,
      epoch: bigint,
    ) => V2Carrier
    const carrier = new CarrierConstructor(socket as unknown as WebSocket, crypto.randomUUID(), 1n)
    const heartbeat = carrier as unknown as {
      startHeartbeat: (seconds: number) => void
      nextPacket: bigint
      lastPeerAcknowledged: bigint
      lastAckProgressAt: number
    }
    let failure = ''
    carrier.setHandlers({ onClose: (error) => (failure = error.message) })
    heartbeat.startHeartbeat(1)
    heartbeat.nextPacket = 1n
    heartbeat.lastPeerAcknowledged = 0n
    heartbeat.lastAckProgressAt = -16_000

    await vi.advanceTimersByTimeAsync(15_000)

    expect(failure).toBe('remote/v2 ack_timeout')
    expect(socket.sent).toBe(0)
    expect(carrier.isOpen).toBe(false)
  })

  it('keeps bulk memory bounded and lets control traffic pass a slow socket', async () => {
    class SlowSocket extends EventTarget {
      readyState: number = WebSocket.OPEN
      bufferedAmount = 2 << 20
      sent = 0
      send() {
        this.sent += 1
      }
      close() {
        this.readyState = WebSocket.CLOSED
      }
    }
    const socket = new SlowSocket()
    const CarrierConstructor = V2Carrier as unknown as new (
      socket: WebSocket,
      id: string,
      epoch: bigint,
    ) => V2Carrier
    const carrier = new CarrierConstructor(socket as unknown as WebSocket, crypto.randomUUID(), 1n)
    const bulk = create(LinkEnvelopeSchema, {
      linkId: 'link',
      body: {
        case: 'encrypted',
        value: create(EncryptedRecordSchema, {
          linkId: 'link',
          frameType: FrameType.FILE_CHUNK,
          ciphertext: new Uint8Array(64 << 10),
        }),
      },
    })
    const bulkResults = Array.from({ length: 200 }, () =>
      carrier.sendLink(bulk).then(
        () => undefined,
        (failure: unknown) => failure,
      ),
    )
    const control = carrier.sendLink(
      create(LinkEnvelopeSchema, {
        linkId: 'control-link',
        body: {
          case: 'linkInit',
          value: create(LinkInitSchema, { linkId: 'control-link' }),
        },
      }),
    )

    await control
    expect(socket.sent).toBe(1)
    expect(carrier.queueDepths.bulk).toBeGreaterThan(0)
    expect(carrier.queueDepths.bulk).toBeLessThanOrEqual(256)

    carrier.close()
    const failures = await Promise.all(bulkResults)
    expect(failures.some((failure) => failure instanceof V2CarrierBackpressureError)).toBe(true)
    expect(carrier.queueDepths).toEqual({ control: 0, interactive: 0, bulk: 0 })
  })

  it('fails closed before the browser native socket buffer can grow without bound', async () => {
    class SaturatedSocket extends EventTarget {
      readyState: number = WebSocket.OPEN
      bufferedAmount = 17 << 20
      send() {}
      close() {
        this.readyState = WebSocket.CLOSED
      }
    }
    const socket = new SaturatedSocket()
    const CarrierConstructor = V2Carrier as unknown as new (
      socket: WebSocket,
      id: string,
      epoch: bigint,
    ) => V2Carrier
    const carrier = new CarrierConstructor(socket as unknown as WebSocket, crypto.randomUUID(), 1n)
    let failure = ''
    carrier.setHandlers({ onClose: (error) => (failure = error.message) })

    await expect(
      carrier.sendLink(
        create(LinkEnvelopeSchema, {
          linkId: 'control-link',
          body: {
            case: 'linkInit',
            value: create(LinkInitSchema, { linkId: 'control-link' }),
          },
        }),
      ),
    ).rejects.toThrow('socket_backpressure')
    expect(failure).toBe('remote/v2 socket_backpressure')
    expect(carrier.queueDepths).toEqual({ control: 0, interactive: 0, bulk: 0 })
  })
})
