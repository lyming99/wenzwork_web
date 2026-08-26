import { describe, expect, it, vi } from 'vitest'

import type { RemoteScope } from '@/api/remote'
import type { RemoteAgentCapabilities } from '@/remote/peerClient'

import type { V2Link } from './link'
import { V2RPCClient, type V2RPCDependencies } from './rpcClient'

const dependencies: V2RPCDependencies = {
  scopeForMethod: (): RemoteScope => 'remote.peer.query',
  parseCapabilities: (value) => value as RemoteAgentCapabilities,
  timeoutFor: () => 1000,
  projectRequired: () => false,
}

describe('remote/v2 RPC recovery cleanup', () => {
  it('queues a failed StreamClose and drains it after confirmed Link recovery', async () => {
    const sendEncrypted = vi
      .fn()
      .mockRejectedValueOnce(new Error('Carrier closed'))
      .mockResolvedValue(undefined)
    const link = {
      deviceId: crypto.randomUUID(),
      deviceIdentityKeyVersion: 1,
      isActive: true,
      on: vi.fn(() => () => undefined),
      sendEncrypted,
      channels: { release: vi.fn() },
    } as unknown as V2Link
    const rpc = new V2RPCClient(link, dependencies)
    const internal = rpc as unknown as {
      closeStream(pending: { channelId: string; streamId: string }): Promise<void>
    }
    const pending = {
      channelId: crypto.randomUUID(),
      streamId: crypto.randomUUID(),
    }

    await internal.closeStream(pending)
    expect(rpc.staleStreamCount).toBe(1)

    await rpc.closeStaleStreams()
    expect(rpc.staleStreamCount).toBe(0)
    expect(sendEncrypted).toHaveBeenCalledTimes(2)

    rpc.dispose()
  })

  it.each([
    'conversation.send',
    'conversation.chat.send',
    'chat.send',
    'conversation.regenerate',
  ])(
    'does not close a possibly committed durable Stream for %s',
    async (method) => {
      const sendEncrypted = vi.fn().mockResolvedValue(undefined)
      const release = vi.fn()
      const link = {
        deviceId: crypto.randomUUID(),
        deviceIdentityKeyVersion: 1,
        isActive: true,
        on: vi.fn(() => () => undefined),
        sendEncrypted,
        channels: { release },
      } as unknown as V2Link
      const rpc = new V2RPCClient(link, dependencies)
      const streamId = crypto.randomUUID()
      const result = new Promise<never>((_resolve, reject) => {
        const internal = rpc as unknown as {
          pending: Map<string, Record<string, unknown>>
        }
        internal.pending.set(streamId, {
          channelId: crypto.randomUUID(),
          streamId,
          operationId: crypto.randomUUID(),
          attemptId: crypto.randomUUID(),
          method,
          projectId: undefined,
          scope: 'remote.peer.ai.chat',
          streamKind: 1,
          channelRetained: true,
          resolve: vi.fn(),
          reject,
          onEvent: undefined,
          timer: setTimeout(() => undefined, 60_000),
        })
      })

      rpc.fail(new Error('Carrier closed'))

      await expect(result).rejects.toMatchObject({ possiblyCommitted: true })
      expect(sendEncrypted).not.toHaveBeenCalled()
      expect(rpc.staleStreamCount).toBe(0)
      expect(release).toHaveBeenCalledTimes(1)
      rpc.dispose()
    },
  )
})
