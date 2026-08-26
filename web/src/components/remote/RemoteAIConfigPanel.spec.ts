import { flushPromises, mount } from '@vue/test-utils'
import { ref } from 'vue'
import { describe, expect, it, vi } from 'vitest'

import { remoteRPCKey, type RemoteAIConfiguration, type RemoteRPCClient } from '@/remote/rpcTypes'

import RemoteAIConfigPanel from './RemoteAIConfigPanel.vue'

const savedConfiguration: RemoteAIConfiguration = {
  id: 'default',
  revision: 4,
  name: '默认 AI',
  provider: 'openai-compatible',
  baseUrl: 'https://api.example.test/v1',
  nonSecretHeaders: {},
  model: 'model-a',
  systemPrompt: 'Be precise.',
  temperature: 0.7,
  reasoningEffort: 'automatic',
  maxTurnOutputTokens: 16000,
  maxActiveContextTokens: 120000,
  maxAgentRounds: 64,
  maxAgentToolCalls: 100,
  maxAgentNoProgressRounds: 8,
  requestTimeoutSeconds: 120,
  maxRetries: 2,
  retryBaseDelayMilliseconds: 350,
  showUsage: true,
  secretConfigured: true,
  enabled: true,
}

const buttonWithText = (wrapper: ReturnType<typeof mountPanel>['wrapper'], label: string) => {
  const button = wrapper.findAll('button').find((candidate) => candidate.text().includes(label))
  if (!button) throw new Error(`missing button: ${label}`)
  return button
}

const mountPanel = (
  writable = true,
  listOverride?: (input: Record<string, unknown>) => {
    items: RemoteAIConfiguration[]
    highWatermark: number
    nextCursor?: string | null
  },
) => {
  const call = vi.fn(async (method: string, input: Record<string, unknown> = {}) => {
    if (method === 'ai.config.list') {
      if (listOverride) return listOverride(input)
      return { items: [savedConfiguration], highWatermark: 4 }
    }
    if (method === 'ai.config.update') {
      return {
        ...savedConfiguration,
        id: input.id,
        revision: 5,
        name: input.name,
        provider: input.provider,
        baseUrl: input.baseUrl,
        nonSecretHeaders: input.nonSecretHeaders,
        model: input.model,
        systemPrompt: input.systemPrompt,
        temperature: input.temperature,
        reasoningEffort: input.reasoningEffort,
        maxTurnOutputTokens: input.maxTurnOutputTokens,
        maxActiveContextTokens: input.maxActiveContextTokens,
        requestTimeoutSeconds: input.requestTimeoutSeconds,
        maxRetries: input.maxRetries,
        retryBaseDelayMilliseconds: input.retryBaseDelayMilliseconds,
        showUsage: input.showUsage,
        secretConfigured: Boolean(input.secret),
        enabled: input.enabled,
      }
    }
    if (method === 'ai.config.delete') return { deleted: true, configId: input.id }
    throw new Error(`unexpected method: ${method}`)
  })
  const rpc = {
    connected: ref(true),
    reconnecting: ref(false),
    error: ref(''),
    connect: vi.fn(async () => undefined),
    close: vi.fn(async () => undefined),
    call,
    stream: vi.fn(),
    downloadFile: vi.fn(),
    downloadTaskLog: vi.fn(),
    uploadFile: vi.fn(),
  } as unknown as RemoteRPCClient

  return {
    call,
    wrapper: mount(RemoteAIConfigPanel, {
      props: { writable },
      global: { provide: { [remoteRPCKey as symbol]: rpc } },
    }),
  }
}

describe('RemoteAIConfigPanel', () => {
  it('lists, creates, and deletes device-local configurations without revealing secrets', async () => {
    Object.defineProperty(window, 'confirm', {
      configurable: true,
      value: vi.fn(() => true),
    })
    const mounted = mountPanel()
    await flushPromises()

    expect(mounted.wrapper.text()).toContain('默认 AI · model-a')
    expect(mounted.wrapper.text()).toContain('设备已配置密钥')
    expect((mounted.wrapper.get('[name="aiApiKey"]').element as HTMLInputElement).value).toBe('')

    await buttonWithText(mounted.wrapper, '新增配置').trigger('click')
    await mounted.wrapper.get('[name="aiConfigName"]').setValue('备用 AI')
    await mounted.wrapper.get('[name="aiBaseUrl"]').setValue('https://backup.example.test/v1')
    await mounted.wrapper.get('[name="aiModel"]').setValue('model-b')
    await mounted.wrapper.get('[name="aiApiKey"]').setValue('secret-only-on-device')
    await mounted.wrapper.get('form').trigger('submit')
    await flushPromises()

    expect(mounted.call).toHaveBeenCalledWith(
      'ai.config.update',
      expect.objectContaining({
        id: expect.stringMatching(/^config-/),
        expectedRevision: 0,
        name: '备用 AI',
        provider: 'openai-compatible',
        baseUrl: 'https://backup.example.test/v1',
        model: 'model-b',
        secretAction: 'replace',
        secret: 'secret-only-on-device',
      }),
    )
    const update = mounted.call.mock.calls.find(([method]) => method === 'ai.config.update')?.[1]
    expect(update).not.toHaveProperty('maxAgentRounds')
    expect(update).not.toHaveProperty('maxAgentToolCalls')
    expect(update).not.toHaveProperty('maxAgentNoProgressRounds')
    expect(mounted.wrapper.text()).toContain('配置已在目标设备本地保存')
    expect((mounted.wrapper.get('[name="aiApiKey"]').element as HTMLInputElement).value).toBe('')

    await buttonWithText(mounted.wrapper, '删除配置').trigger('click')
    await flushPromises()
    expect(mounted.call).toHaveBeenCalledWith(
      'ai.config.delete',
      expect.objectContaining({ id: expect.stringMatching(/^config-/), expectedRevision: 5 }),
    )
    expect(mounted.wrapper.text()).toContain('配置已从目标设备删除')

    mounted.wrapper.unmount()
  })

  it('keeps mutation controls disabled for a query-only peer scope', async () => {
    const mounted = mountPanel(false)
    await flushPromises()

    expect(mounted.wrapper.text()).toContain('当前授权为只读')
    expect(buttonWithText(mounted.wrapper, '新增配置').attributes('disabled')).toBeDefined()
    expect(buttonWithText(mounted.wrapper, '保存到设备').attributes('disabled')).toBeDefined()
    expect(buttonWithText(mounted.wrapper, '删除配置').attributes('disabled')).toBeDefined()
    expect(buttonWithText(mounted.wrapper, '在设备上测试').attributes('disabled')).toBeUndefined()
    expect(mounted.call).not.toHaveBeenCalledWith('ai.config.update', expect.anything())

    mounted.wrapper.unmount()
  })

  it('loads every AI configuration cursor page', async () => {
    const mounted = mountPanel(true, (input) =>
      input.cursor === 'page-2'
        ? {
            items: [{ ...savedConfiguration, id: 'second', name: '第二配置' }],
            highWatermark: 4,
            nextCursor: null,
          }
        : {
            items: [{ ...savedConfiguration, id: 'first', name: '第一配置' }],
            highWatermark: 4,
            nextCursor: 'page-2',
          },
    )
    await flushPromises()

    expect(mounted.wrapper.text()).toContain('第一配置')
    expect(mounted.wrapper.text()).toContain('第二配置')
    expect(mounted.call).toHaveBeenCalledWith('ai.config.list', { limit: 100 })
    expect(mounted.call).toHaveBeenCalledWith('ai.config.list', { cursor: 'page-2', limit: 100 })

    mounted.wrapper.unmount()
  })

  it('stops AI configuration aggregation at the cumulative page budget', async () => {
    let pages = 0
    const mounted = mountPanel(true, () => {
      pages += 1
      return {
        items: [],
        highWatermark: pages,
        nextCursor: `page-${pages + 1}`,
      }
    })
    await flushPromises()

    expect(pages).toBe(16)
    expect(mounted.wrapper.get('[role="alert"]').text()).toContain(
      'AI 配置列表页数超过客户端安全上限',
    )
    mounted.wrapper.unmount()
  })
})
