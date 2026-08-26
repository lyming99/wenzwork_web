import { flushPromises, mount } from '@vue/test-utils'
import { ref } from 'vue'
import { describe, expect, it, vi } from 'vitest'

import { remoteRPCKey, type RemoteFileEntry, type RemoteRPCClient } from '@/remote/rpcTypes'

import RemoteFilesPanel from './RemoteFilesPanel.vue'

const entry: RemoteFileEntry = {
  id: 'readme',
  revision: 7,
  name: 'README.md',
  relativePath: 'README.md',
  kind: 'file',
  category: 'text',
  extension: '.md',
  size: 9,
  modifiedAt: '2026-08-08T00:00:00Z',
  readable: true,
  writable: true,
}

const mountPanel = (writable: boolean) => {
  const call = vi.fn(async (method: string) => {
    if (method === 'file.list') {
      return {
        items: [entry],
        nextCursor: null,
        highWatermark: 7,
        resetRequired: false,
      }
    }
    if (method === 'file.details') {
      return {
        entry,
        category: 'text',
        extension: '.md',
        text: { readable: true, editable: true, encoding: 'utf-8', maximumBytes: 524288 },
      }
    }
    throw new Error(`unexpected method: ${method}`)
  })
  const downloadFile = vi.fn(async () => new Blob(['# Preview']))
  const uploadFile = vi.fn(async () => ({ revision: 8, entry: { ...entry, revision: 8 } }))
  const rpc = {
    connected: ref(true),
    reconnecting: ref(false),
    error: ref(''),
    connect: vi.fn(async () => undefined),
    close: vi.fn(async () => undefined),
    call,
    stream: vi.fn(),
    downloadFile,
    uploadFile,
  } as unknown as RemoteRPCClient
  return {
    call,
    downloadFile,
    uploadFile,
    wrapper: mount(RemoteFilesPanel, {
      props: {
        writable,
        projectId: '11111111-1111-4111-8111-111111111111',
      },
      global: { provide: { [remoteRPCKey as symbol]: rpc } },
    }),
  }
}

const buttonWithText = (wrapper: ReturnType<typeof mountPanel>['wrapper'], label: string) => {
  const button = wrapper.findAll('button').find((candidate) => candidate.text().includes(label))
  if (!button) throw new Error(`missing button: ${label}`)
  return button
}

describe('RemoteFilesPanel', () => {
  it('keeps all file mutations disabled with a receive-only scope', async () => {
    const mounted = mountPanel(false)
    await flushPromises()

    expect(mounted.wrapper.text()).toContain('当前授权为只读')
    for (const label of ['新建文件夹', '新建文本', '上传文件', '改名', '移动', '删除']) {
      expect(buttonWithText(mounted.wrapper, label).attributes('disabled')).toBeDefined()
    }
    expect(buttonWithText(mounted.wrapper, '下载').attributes('disabled')).toBeUndefined()

    mounted.wrapper.unmount()
  })

  it('downloads a bounded text preview and saves through revision-bound atomic upload', async () => {
    const mounted = mountPanel(true)
    await flushPromises()
    await buttonWithText(mounted.wrapper, 'README.md').trigger('click')
    await flushPromises()

    expect(
      (mounted.wrapper.get('.file-preview textarea').element as HTMLTextAreaElement).value,
    ).toBe('# Preview')
    expect(mounted.downloadFile).toHaveBeenCalledWith(
      'README.md',
      undefined,
      { projectId: '11111111-1111-4111-8111-111111111111' },
      7,
    )
    await mounted.wrapper.get('.file-preview textarea').setValue('# Updated')
    await buttonWithText(mounted.wrapper, '保存').trigger('click')
    await flushPromises()
    expect(mounted.uploadFile).toHaveBeenCalledWith(
      'README.md',
      expect.any(File),
      undefined,
      { projectId: '11111111-1111-4111-8111-111111111111' },
      7,
    )
    expect(mounted.call).not.toHaveBeenCalledWith('file.write-text', expect.anything())

    mounted.wrapper.unmount()
  })
})
