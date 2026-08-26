import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

import { create, fromBinary, toBinary } from '@bufbuild/protobuf'
import { describe, expect, it } from 'vitest'

import { Direction, FrameType } from '@/generated/remote/v2/common_pb'
import {
  TerminalInputSchema,
  TerminalStreamFrameSchema,
  TerminalStreamHelloSchema,
} from '@/generated/remote/v2/message_pb'

import {
  base64UrlToBytes,
  bytesToBase64Url,
  canonicalCarrierProof,
  canonicalLinkInitTranscript,
  canonicalLinkTranscript,
  canonicalRecordMetadata,
  canonicalRekey,
  deriveChannelKey,
  deriveControlKey,
  deriveRekeyRoot,
  deriveRootKey,
  deriveStreamKey,
  linkConfirmationMac,
  recordNonce,
  sealRecord,
  signCarrierProof,
  signLinkInit,
  type V2HandshakeBinding,
  type V2RecordMetadata,
} from './crypto'

interface GoldenVectors {
  version: number
  binding: Record<string, string | number>
  sharedSecret: string
  record: Record<string, string | number>
  terminalStream: {
    sessionId: string
    hello: Record<string, string | number>
    input: Record<string, string | number>
  }
  rekey: Record<string, string | number>
  carrierProof: Record<string, string | number>
  identitySeeds: Record<string, string>
  expected: Record<string, string>
}

const golden = JSON.parse(
  readFileSync(resolve(process.cwd(), '../api/remote/v2/golden_vectors.json'), 'utf8'),
) as GoldenVectors

const bytes = (value: string) => base64UrlToBytes(value)
const text = (value: string | number) => String(value)
const number = (value: string | number) => Number(value)
const bigint = (value: string | number) => BigInt(value)

describe('remote/v2 shared cryptographic golden vectors', () => {
  it('matches terminal v4 raw-byte protobuf vectors', () => {
    const terminal = golden.terminalStream
    const hello = terminal.hello
    const encoded = toBinary(
      TerminalStreamFrameSchema,
      create(TerminalStreamFrameSchema, {
        sessionId: terminal.sessionId,
        body: {
          case: 'hello',
          value: create(TerminalStreamHelloSchema, {
            afterOutputSequence: bigint(hello.afterOutputSequence),
            afterInputSequence: bigint(hello.afterInputSequence),
            afterResizeSequence: bigint(hello.afterResizeSequence),
            outputCreditBytes: Number(hello.outputCreditBytes),
          }),
        },
      }),
    )
    expect(bytesToBase64Url(encoded)).toBe(text(hello.base64Url))

    const input = terminal.input
    const decoded = fromBinary(TerminalStreamFrameSchema, bytes(text(input.base64Url)))
    expect(decoded.sessionId).toBe(terminal.sessionId)
    expect(decoded.body.case).toBe('input')
    if (decoded.body.case !== 'input') throw new Error('terminal input vector is invalid')
    expect(decoded.body.value.sequence).toBe(bigint(input.sequence))
    expect(bytesToBase64Url(decoded.body.value.data)).toBe(text(input.dataBase64Url))
    expect(
      toBinary(
        TerminalInputSchema,
        create(TerminalInputSchema, {
          sequence: bigint(input.sequence),
          data: bytes(text(input.dataBase64Url)),
        }),
      ).length,
    ).toBeGreaterThan(0)
  })

  it('matches the Go-produced transcript, keys, nonce, AEAD and signatures', () => {
    expect(golden.version).toBe(1)
    const source = golden.binding
    const binding: V2HandshakeBinding = {
      grantId: text(source.grantId),
      linkId: text(source.linkId),
      clientId: text(source.clientId),
      deviceId: text(source.deviceId),
      relayNodeId: text(source.relayNodeId),
      relayCellId: text(source.relayCellId),
      targetConnectionEpoch: bigint(source.targetConnectionEpoch),
      clientIdentityVersion: bigint(source.clientIdentityVersion),
      deviceIdentityVersion: bigint(source.deviceIdentityVersion),
      clientEphemeralPublic: bytes(text(source.clientEphemeralPublic)),
      deviceEphemeralPublic: bytes(text(source.deviceEphemeralPublic)),
      clientChallenge: bytes(text(source.clientChallenge)),
      deviceChallenge: bytes(text(source.deviceChallenge)),
      expiresAtUnixMilli: bigint(source.expiresAtUnixMilli),
    }
    const initBinding: V2HandshakeBinding = {
      ...binding,
      deviceIdentityVersion: 0n,
      deviceEphemeralPublic: new Uint8Array(),
      deviceChallenge: new Uint8Array(),
    }
    const record: V2RecordMetadata = {
      linkId: binding.linkId,
      channelId: text(golden.record.channelId),
      streamId: text(golden.record.streamId),
      keyId: bigint(golden.record.keyId),
      direction: Direction.CLIENT_TO_DEVICE,
      frameType: FrameType.RPC_REQUEST,
      streamSequence: bigint(golden.record.streamSequence),
    }
    const expected = golden.expected
    const root = deriveRootKey(bytes(golden.sharedSecret), binding)
    const stream = deriveStreamKey(
      root,
      binding.linkId,
      record.keyId,
      record.direction,
      record.channelId,
      record.streamId,
    )

    expect(bytesToBase64Url(canonicalLinkInitTranscript(initBinding))).toBe(expected.initTranscript)
    expect(bytesToBase64Url(canonicalLinkTranscript(binding))).toBe(expected.handshakeTranscript)
    expect(bytesToBase64Url(root)).toBe(expected.rootKey)
    expect(bytesToBase64Url(linkConfirmationMac(root, binding))).toBe(expected.linkConfirmationMac)
    expect(
      bytesToBase64Url(deriveControlKey(root, binding.linkId, record.keyId, record.direction)),
    ).toBe(expected.controlKey)
    expect(
      bytesToBase64Url(
        deriveChannelKey(root, binding.linkId, record.keyId, record.direction, record.channelId),
      ),
    ).toBe(expected.channelKey)
    expect(bytesToBase64Url(stream)).toBe(expected.streamKey)
    expect(bytesToBase64Url(canonicalRecordMetadata(record))).toBe(expected.associatedData)
    expect(bytesToBase64Url(recordNonce(stream, record))).toBe(expected.nonce)
    expect(bytesToBase64Url(sealRecord(stream, bytes(text(golden.record.plaintext)), record))).toBe(
      expected.ciphertext,
    )
    expect(
      bytesToBase64Url(
        deriveRekeyRoot(
          root,
          bytes(text(golden.rekey.sharedSecret)),
          binding.linkId,
          text(golden.rekey.rekeyId),
          bigint(golden.rekey.keyId),
        ),
      ),
    ).toBe(expected.rekeyRoot)
    expect(
      bytesToBase64Url(
        canonicalRekey(
          'init',
          binding.linkId,
          text(golden.rekey.rekeyId),
          bigint(golden.rekey.keyId),
          bytes(text(golden.rekey.ephemeralPublic)),
        ),
      ),
    ).toBe(expected.canonicalRekeyInit)
    expect(bytesToBase64Url(signLinkInit(bytes(golden.identitySeeds.client), initBinding))).toBe(
      expected.initSignature,
    )
    expect(
      bytesToBase64Url(
        signCarrierProof(bytes(golden.identitySeeds.client), {
          grantId: binding.grantId,
          carrierId: text(golden.carrierProof.carrierId),
          carrierEpoch: bigint(golden.carrierProof.carrierEpoch),
          challenge: bytes(text(golden.carrierProof.challenge)),
        }),
      ),
    ).toBe(expected.carrierProofSignature)
    expect(
      bytesToBase64Url(
        canonicalCarrierProof({
          grantId: binding.grantId,
          carrierId: text(golden.carrierProof.carrierId),
          carrierEpoch: bigint(golden.carrierProof.carrierEpoch),
          challenge: bytes(text(golden.carrierProof.challenge)),
        }),
      ),
    ).not.toBe('')
    expect(number(golden.record.direction)).toBe(Direction.CLIENT_TO_DEVICE)
    expect(number(golden.record.frameType)).toBe(FrameType.RPC_REQUEST)
  })
})
