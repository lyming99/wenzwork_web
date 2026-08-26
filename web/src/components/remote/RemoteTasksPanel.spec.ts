import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { createPinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import {
  agentEventConnectionKey,
  type AgentEventConnection,
  type AgentEventReset,
  type AgentStateEvent,
} from '@/remote/agentEvents'
import { remoteRPCKey, type RemoteRPCClient } from '@/remote/rpcTypes'

import RemoteTasksPanel from './RemoteTasksPanel.vue'

const projectId = '33333333-3333-4333-8333-333333333333'
const runId = '88888888-8888-4888-8888-888888888888'

const makeTask = (
  id: string,
  title: string,
  status: string,
  overrides: Record<string, unknown> = {},
) => ({
  definition: {
    id,
    projectId,
    kind: 'codex',
    title,
    cwd: '.',
    config: {
      promptSource: 'customText',
      promptText: `Prompt for ${title}`,
      attachedFilePaths: [],
      model: 'gpt-5.6',
      launchMode: 'cli',
      goalMode: true,
      reasoningEffort: 'high',
    },
    scope: 'topLevel',
    execution: {
      relation: 'dependency',
      mode: 'serial',
      relatedTaskIds: [],
      runImmediately: false,
      resumeCliSession: false,
    },
    ...((overrides.definition as Record<string, unknown> | undefined) ?? {}),
  },
  definitionRevision: 1,
  status,
  revision: 3,
  changeSequence: 9,
  createdAt: '2026-08-18T00:00:00Z',
  updatedAt: '2026-08-18T00:00:01Z',
  logAvailable: false,
  logState: 'none',
  logGeneration: 0,
  logSizeBytes: 0,
  ...(status === 'running' ? { startedAt: '2026-08-18T00:00:01Z' } : {}),
  ...Object.fromEntries(Object.entries(overrides).filter(([key]) => key !== 'definition')),
})

const runningTask = makeTask('11111111-1111-4111-8111-111111111111', 'Running task', 'running', {
  currentRunId: runId,
  logAvailable: true,
  logState: 'active',
  logGeneration: 1,
  logSizeBytes: 7,
})
const queuedTask = makeTask('22222222-2222-4222-8222-222222222222', 'Queued task', 'queued')
const waitingTask = makeTask('88888888-8888-4888-8888-888888888888', 'Waiting task', 'waiting')
const failedTask = makeTask('44444444-4444-4444-8444-444444444444', 'Failed task', 'failed')
const acceptanceTask = makeTask(
  '55555555-5555-4555-8555-555555555555',
  'Acceptance task',
  'awaitingAcceptance',
)
const completedTask = makeTask(
  '66666666-6666-4666-8666-666666666666',
  'Completed task',
  'completed',
)
const changesTask = makeTask(
  '77777777-7777-4777-8777-777777777777',
  'Changes requested task',
  'changesRequested',
)

const call = vi.fn()
const getCapabilities = vi.fn()
const downloadTaskLog = vi.fn()
const pauseDownloads = vi.fn()
const resumeDownloads = vi.fn()
const rpc = {
  call,
  getCapabilities,
  downloadTaskLog,
  pauseDownloads,
  resumeDownloads,
  connected: { value: true },
  reconnecting: { value: false },
  error: { value: '' },
} as unknown as RemoteRPCClient

let listedTasks = [runningTask]
let eventHandler: ((event: AgentStateEvent) => void | Promise<void>) | undefined
let resetHandler: ((reset: AgentEventReset) => void | Promise<void>) | undefined
const agentEvents = {
  onEvent: (handler: (event: AgentStateEvent) => void | Promise<void>) => {
    eventHandler = handler
    return () => {
      if (eventHandler === handler) eventHandler = undefined
    }
  },
  onReset: (handler: (reset: AgentEventReset) => void | Promise<void>) => {
    resetHandler = handler
    return () => {
      if (resetHandler === handler) resetHandler = undefined
    }
  },
} as unknown as AgentEventConnection

const mountPanel = (online = true) =>
  mount(RemoteTasksPanel, {
    props: {
      deviceId: 'device',
      deviceName: '工作站 A',
      projectId,
      protocolVersion: 2,
      capabilityVersion: 'tasks.v2',
      online,
      writable: true,
    },
    global: {
      plugins: [createPinia()],
      provide: {
        [remoteRPCKey as symbol]: rpc,
        [agentEventConnectionKey as symbol]: agentEvents,
      },
    },
  })

const buttonWithText = (wrapper: VueWrapper, text: string) => {
  const button = wrapper.findAll('button').find((candidate) => candidate.text().includes(text))
  if (!button) throw new Error(`button not found: ${text}`)
  return button
}

const taskLogEvent = (highWatermark: number): AgentStateEvent => ({
  eventId: 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa',
  sequence: highWatermark + 8,
  highWatermark: highWatermark + 8,
  schemaVersion: 1,
  projectId,
  topic: 'taskLog',
  type: 'task.logs.available',
  aggregateType: 'task',
  aggregateId: runningTask.definition.id,
  operation: 'status',
  revision: highWatermark,
  cursor: { kind: 'task_logs', value: highWatermark },
  data: { highWatermark },
})

const taskLogByteEvent = (highWatermark: number, generation = 1): AgentStateEvent => ({
  eventId: 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb',
  sequence: highWatermark + 8,
  highWatermark: highWatermark + 8,
  schemaVersion: 1,
  projectId,
  topic: 'taskLog',
  type: 'task.logs.available',
  aggregateType: 'task',
  aggregateId: runningTask.definition.id,
  operation: 'status',
  revision: highWatermark,
  cursor: { kind: 'task_log_bytes', value: highWatermark },
  data: { runId, generation, highWatermark },
})

describe('RemoteTasksPanel', () => {
  beforeEach(() => {
    listedTasks = [runningTask]
    eventHandler = undefined
    resetHandler = undefined
    call.mockReset()
    getCapabilities.mockReset()
    getCapabilities.mockResolvedValue({
      protocolMinimum: 1,
      protocolMaximum: 1,
      featureVersions: { tasks: 2 },
      features: { 'tasks.v2': true },
      operatingSystem: 'windows',
      architecture: 'amd64',
      shells: [],
      taskRunners: ['codex'],
      resourceLimits: {},
    })
    downloadTaskLog.mockReset()
    pauseDownloads.mockReset()
    resumeDownloads.mockReset()
    call.mockImplementation(async (method: string, input?: Record<string, unknown>) => {
      if (method === 'task.list') {
        return { items: listedTasks, nextCursor: null, highWatermark: 9 }
      }
      if (method === 'task.logs') {
        return {
          items: [
            {
              taskId: input?.taskId,
              sequence: 1,
              stream: 'stdout',
              content: 'working',
              occurredAt: '2026-08-18T00:00:01Z',
            },
          ],
          ackedThroughSequence: 1,
          highWatermark: 1,
          minimumAvailableSequence: 1,
          hasMore: false,
          resetRequired: false,
        }
      }
      if (method === 'task.runs') {
        return {
          items: [
            {
              id: '88888888-8888-4888-8888-888888888888',
              taskId: input?.taskId,
              status: 'failed',
              attempt: 1,
              createdAt: '2026-08-18T00:00:00Z',
              cliSessionId: 'cli-session-1',
              logAvailable: true,
              logState: 'sealed',
              logGeneration: 1,
              logFormatVersion: 1,
              logSizeBytes: 7,
            },
          ],
          nextCursor: null,
          highWatermark: 1,
        }
      }
      if (method === 'task.update') {
        const definition = input?.definition as (typeof runningTask)['definition']
        return { ...runningTask, definition, revision: 4 }
      }
      return { ...runningTask, status: 'cancelled', revision: 4 }
    })
    vi.stubGlobal(
      'confirm',
      vi.fn(() => true),
    )
  })

  const enableFileTaskLogs = (bulkDownload = true) =>
    getCapabilities.mockResolvedValue({
      protocolMinimum: 1,
      protocolMaximum: 1,
      featureVersions: { tasks: 2, taskLogs: 1 },
      features: {
        'tasks.v2': true,
        'taskLogs.fileSeek': true,
        'taskLogs.bulkDownload': bulkDownload,
      },
      operatingSystem: 'windows',
      architecture: 'amd64',
      shells: [],
      taskRunners: ['codex'],
      resourceLimits: { taskLogSeekBytes: 32768 },
    })

  const seekPage = (
    content: string,
    startOffset: number,
    fileSize: number,
    generation = 1,
    extra: Record<string, unknown> = {},
  ) => ({
    taskId: runningTask.definition.id,
    runId,
    generation,
    formatVersion: 1,
    logState: 'active',
    content,
    startOffset,
    nextOffset: startOffset + new TextEncoder().encode(content).length,
    fileSize,
    eof: startOffset + new TextEncoder().encode(content).length === fileSize,
    hasMoreBefore: startOffset > 0,
    sealed: false,
    cursorAdjusted: false,
    resetRequired: false,
    ...extra,
  })

  it('marks a non-selected task with new logs without reading its body', async () => {
    enableFileTaskLogs()
    listedTasks = [runningTask, queuedTask]
    const wrapper = mountPanel()
    await flushPromises()
    const handler = eventHandler
    if (!handler) throw new Error('task event handler was not registered')
    await handler({
      ...taskLogByteEvent(64),
      aggregateId: queuedTask.definition.id,
      data: {
        runId: '99999999-9999-4999-8999-999999999999',
        generation: 1,
        highWatermark: 64,
      },
    })
    await flushPromises()
    const queuedCard = wrapper
      .findAll('.task-card')
      .find((card) => card.text().includes(queuedTask.definition.title))
    if (!queuedCard) throw new Error('queued task card was not rendered')
    expect(queuedCard.get('.new-log-badge').text()).toBe('新日志')
    expect(
      call.mock.calls.some(
        ([method, input]) =>
          method === 'task.logs' &&
          (input as Record<string, unknown> | undefined)?.taskId === queuedTask.definition.id,
      ),
    ).toBe(false)
    wrapper.unmount()
  })

  it('keeps file seek enabled while hiding download when bulk capability is false', async () => {
    enableFileTaskLogs(false)
    const content = '2026-08-20T00:00:00.000Z [stdout] seek only\n'
    const size = new TextEncoder().encode(content).length
    call.mockImplementation(async (method: string, input?: Record<string, unknown>) => {
      if (method === 'task.list')
        return { items: [runningTask], nextCursor: null, highWatermark: 9 }
      if (method === 'task.runs') {
        return {
          items: [
            {
              id: runId,
              taskId: runningTask.definition.id,
              status: 'running',
              attempt: 1,
              createdAt: '2026-08-18T00:00:00Z',
              logAvailable: true,
              logState: 'active',
              logGeneration: 1,
              logFormatVersion: 1,
              logSizeBytes: size,
            },
          ],
          nextCursor: null,
          highWatermark: 1,
        }
      }
      if (method === 'task.logs' && input?.tailBytes === 32768) return seekPage(content, 0, size)
      throw new Error(`unexpected method ${method}`)
    })

    const wrapper = mountPanel()
    await flushPromises()
    await wrapper.get('.task-card').trigger('click')
    await flushPromises()
    expect(wrapper.text()).toContain('seek only')
    expect(wrapper.text()).not.toContain('下载完整日志')
    expect(downloadTaskLog).not.toHaveBeenCalled()
    wrapper.unmount()
  })

  it('uses run-scoped byte windows, drains byte watermarks, and bounds the rendered log', async () => {
    enableFileTaskLogs()
    const initial = Array.from(
      { length: 520 },
      (_, index) => `2026-08-20T00:00:00.000Z [stdout] file line ${index}\n`,
    ).join('')
    const initialBytes = new TextEncoder().encode(initial).length
    const suffix = '2026-08-20T00:00:01.000Z [stderr] latest byte line\n'
    const suffixBytes = new TextEncoder().encode(suffix).length
    call.mockImplementation(async (method: string, input?: Record<string, unknown>) => {
      if (method === 'task.list')
        return { items: [runningTask], nextCursor: null, highWatermark: 9 }
      if (method === 'task.runs') {
        return {
          items: [
            {
              id: runId,
              taskId: runningTask.definition.id,
              status: 'running',
              attempt: 1,
              createdAt: '2026-08-18T00:00:00Z',
              logAvailable: true,
              logState: 'active',
              logGeneration: 1,
              logFormatVersion: 1,
              logSizeBytes: initialBytes,
            },
          ],
          nextCursor: null,
          highWatermark: 1,
        }
      }
      if (method === 'task.logs' && input?.tailBytes === 32768) {
        return seekPage(initial, 0, initialBytes)
      }
      if (method === 'task.logs' && input?.offset === initialBytes) {
        return seekPage(suffix, initialBytes, initialBytes + suffixBytes)
      }
      throw new Error(`unexpected method ${method}`)
    })

    const wrapper = mountPanel()
    await flushPromises()
    await wrapper.get('.task-card').trigger('click')
    await flushPromises()

    expect(wrapper.text()).not.toContain('file line 0\n')
    expect(wrapper.text()).toContain('file line 519')
    expect(call).toHaveBeenCalledWith(
      'task.logs',
      expect.objectContaining({
        taskId: runningTask.definition.id,
        runId,
        generation: 1,
        tailBytes: 32768,
      }),
      { projectId },
    )
    const handler = eventHandler
    if (!handler) throw new Error('task event handler was not registered')
    await handler(taskLogByteEvent(initialBytes + suffixBytes))
    await flushPromises()
    expect(call).toHaveBeenCalledWith(
      'task.logs',
      expect.objectContaining({ runId, generation: 1, offset: initialBytes }),
      { projectId },
    )
    expect(wrapper.text()).toContain('latest byte line')
    wrapper.unmount()
  })

  it('yields and continues after the per-turn file-log catch-up page budget', async () => {
    enableFileTaskLogs()
    const chunks = Array.from(
      { length: 10 },
      (_, index) => `2026-08-20T00:00:00.000Z [stdout] catchup ${String(index).padStart(2, '0')}\n`,
    )
    const chunkBytes = new TextEncoder().encode(chunks[0]!).length
    const totalBytes = chunks.length * chunkBytes
    call.mockImplementation(async (method: string, input?: Record<string, unknown>) => {
      if (method === 'task.list')
        return { items: [runningTask], nextCursor: null, highWatermark: 9 }
      if (method === 'task.runs') {
        return {
          items: [
            {
              id: runId,
              taskId: runningTask.definition.id,
              status: 'running',
              attempt: 1,
              createdAt: '2026-08-18T00:00:00Z',
              logAvailable: true,
              logState: 'active',
              logGeneration: 1,
              logFormatVersion: 1,
              logSizeBytes: 0,
            },
          ],
          nextCursor: null,
          highWatermark: 1,
        }
      }
      if (method === 'task.logs' && input?.tailBytes === 32768) return seekPage('', 0, 0)
      if (method === 'task.logs' && typeof input?.offset === 'number') {
        const index = input.offset / chunkBytes
        if (!Number.isInteger(index) || index < 0 || index >= chunks.length)
          throw new Error(`unexpected offset ${input.offset}`)
        return seekPage(chunks[index]!, input.offset, totalBytes)
      }
      throw new Error(`unexpected method ${method}`)
    })

    const wrapper = mountPanel()
    await flushPromises()
    await wrapper.get('.task-card').trigger('click')
    await flushPromises()
    const handler = eventHandler
    if (!handler) throw new Error('task event handler was not registered')
    await handler(taskLogByteEvent(totalBytes))
    await flushPromises()
    expect(
      call.mock.calls.filter(
        ([method, input]) =>
          method === 'task.logs' && typeof (input as Record<string, unknown>)?.offset === 'number',
      ),
    ).toHaveLength(10)
    expect(wrapper.text()).toContain('catchup 09')
    wrapper.unmount()
  })

  it('keeps the older side visible when upward paging reaches the bounded window limit', async () => {
    enableFileTaskLogs()
    const older = Array.from(
      { length: 100 },
      (_, index) => `2026-08-19T23:59:59.000Z [stdout] older line ${index}\n`,
    ).join('')
    const initial = Array.from(
      { length: 500 },
      (_, index) => `2026-08-20T00:00:00.000Z [stdout] initial line ${index}\n`,
    ).join('')
    const olderBytes = new TextEncoder().encode(older).length
    const totalBytes = olderBytes + new TextEncoder().encode(initial).length
    call.mockImplementation(async (method: string, input?: Record<string, unknown>) => {
      if (method === 'task.list')
        return { items: [runningTask], nextCursor: null, highWatermark: 9 }
      if (method === 'task.runs') {
        return {
          items: [
            {
              id: runId,
              taskId: runningTask.definition.id,
              status: 'running',
              attempt: 1,
              createdAt: '2026-08-18T00:00:00Z',
              logAvailable: true,
              logState: 'active',
              logGeneration: 1,
              logFormatVersion: 1,
              logSizeBytes: totalBytes,
            },
          ],
          nextCursor: null,
          highWatermark: 1,
        }
      }
      if (method === 'task.logs' && input?.tailBytes === 32768)
        return seekPage(initial, olderBytes, totalBytes)
      if (method === 'task.logs' && input?.beforeOffset === olderBytes)
        return seekPage(older, 0, totalBytes)
      throw new Error(`unexpected method ${method}`)
    })

    const wrapper = mountPanel()
    await flushPromises()
    await wrapper.get('.task-card').trigger('click')
    await flushPromises()
    expect(wrapper.text()).not.toContain('older line 0')
    expect(wrapper.text()).toContain('initial line 499')

    const scroller = wrapper.get('.task-log')
    Object.defineProperty(scroller.element, 'scrollTop', { value: 0, writable: true })
    await scroller.trigger('scroll')
    await flushPromises()

    expect(call).toHaveBeenCalledWith(
      'task.logs',
      expect.objectContaining({ runId, generation: 1, beforeOffset: olderBytes }),
      { projectId },
    )
    expect(wrapper.text()).toContain('older line 0')
    expect(wrapper.text()).not.toContain('initial line 499')
    expect(
      wrapper.get('.file-log-text').text().split('\n').filter(Boolean).length,
    ).toBeLessThanOrEqual(500)
    wrapper.unmount()
  })

  it('resets on generation invalidation and downloads through the prepared task-log source', async () => {
    enableFileTaskLogs()
    let invalidated = false
    const first = '2026-08-20T00:00:00.000Z [stdout] old generation\n'
    const replacement = '2026-08-20T00:00:01.000Z [stdout] new generation\n'
    call.mockImplementation(async (method: string, input?: Record<string, unknown>) => {
      if (method === 'task.list')
        return { items: [runningTask], nextCursor: null, highWatermark: 9 }
      if (method === 'task.runs') {
        return {
          items: [
            {
              id: runId,
              taskId: runningTask.definition.id,
              status: 'running',
              attempt: 1,
              createdAt: '2026-08-18T00:00:00Z',
              logAvailable: true,
              logState: 'active',
              logGeneration: 1,
              logFormatVersion: 1,
              logSizeBytes: 64,
            },
          ],
          nextCursor: null,
          highWatermark: 1,
        }
      }
      if (method === 'task.logs' && !invalidated) {
        const size = new TextEncoder().encode(first).length
        return seekPage(first, 0, size)
      }
      if (method === 'task.logs' && input?.generation === 1) {
        return seekPage('', 0, 0, 2, { resetRequired: true })
      }
      if (method === 'task.logs') {
        const size = new TextEncoder().encode(replacement).length
        return seekPage(replacement, 0, size, 2)
      }
      throw new Error(`unexpected method ${method}`)
    })
    let finishDownload!: (result: { blob: Blob; fileName: string }) => void
    downloadTaskLog.mockImplementation(
      async (
        _taskId: string,
        _runId: string,
        _generation: number,
        onProgress?: (received: number, total: number) => void,
      ) => {
        onProgress?.(32768, 65536)
        return new Promise<{ blob: Blob; fileName: string }>((resolve) => {
          finishDownload = resolve
        })
      },
    )
    vi.stubGlobal('URL', {
      ...URL,
      createObjectURL: vi.fn(() => 'blob:task-log'),
      revokeObjectURL: vi.fn(),
    })
    const click = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => undefined)

    const wrapper = mountPanel()
    await flushPromises()
    await wrapper.get('.task-card').trigger('click')
    await flushPromises()
    invalidated = true
    const handler = eventHandler
    if (!handler) throw new Error('task event handler was not registered')
    await handler({ ...taskLogByteEvent(64, 2), operation: 'invalidate' })
    await flushPromises()
    expect(wrapper.text()).toContain('new generation')
    expect(wrapper.text()).toContain('日志文件已更新')
    await buttonWithText(wrapper, '下载完整日志').trigger('click')
    await flushPromises()
    expect(downloadTaskLog).toHaveBeenCalledWith(
      runningTask.definition.id,
      runId,
      2,
      expect.any(Function),
      { projectId },
    )
    expect(wrapper.text()).toContain('已校验 32768 / 65536 bytes')
    await buttonWithText(wrapper, '暂停').trigger('click')
    expect(pauseDownloads).toHaveBeenCalledOnce()
    expect(wrapper.text()).toContain('已暂停 32768 / 65536 bytes')
    await buttonWithText(wrapper, '继续').trigger('click')
    expect(resumeDownloads).toHaveBeenCalledOnce()
    finishDownload({ blob: new Blob(['verified']), fileName: 'task-run.log' })
    await flushPromises()
    expect(click).toHaveBeenCalled()
    expect(resumeDownloads).toHaveBeenCalledTimes(2)
    click.mockRestore()
    wrapper.unmount()
  })

  it('renders the WenzMark header and exact five status filters with changesRequested only in all', async () => {
    listedTasks = [runningTask, failedTask, acceptanceTask, completedTask, changesTask]
    const wrapper = mountPanel()
    await flushPromises()

    const heading = wrapper.get('.task-title h2')
    expect(heading.text()).toBe('任务中心 · 工作站 A')
    expect(wrapper.get('.task-center').attributes('aria-labelledby')).toBe(heading.attributes('id'))
    expect(wrapper.get('.task-head').attributes('style')).toBeUndefined()
    expect(wrapper.find('button[title="刷新"]').exists()).toBe(false)
    expect(wrapper.findAll('.task-filters button').map((button) => button.text())).toEqual([
      '全部 5',
      '执行队列 1',
      '执行错误 1',
      '待验收 1',
      '已完成 1',
    ])
    expect(wrapper.text()).toContain('Changes requested task')

    await buttonWithText(wrapper, '待验收').trigger('click')
    expect(wrapper.text()).toContain('Acceptance task')
    expect(wrapper.text()).not.toContain('Changes requested task')
    expect(call).toHaveBeenCalledWith('task.list', { limit: 20 }, { projectId })
    wrapper.unmount()
  })

  it('stops automatic task-list aggregation at the cumulative page budget', async () => {
    let pages = 0
    call.mockImplementation(async (method: string) => {
      if (method !== 'task.list') throw new Error(`unexpected method ${method}`)
      pages += 1
      return {
        items: [],
        nextCursor: `page-${pages + 1}`,
        highWatermark: pages,
      }
    })

    const wrapper = mountPanel()
    await flushPromises()

    expect(pages).toBe(16)
    expect(wrapper.get('[role="alert"]').text()).toContain('任务列表页数超过客户端安全上限')
    wrapper.unmount()
  })

  it('rejects an oversized task-run cursor before requesting another page', async () => {
    call.mockImplementation(async (method: string, input?: Record<string, unknown>) => {
      if (method === 'task.list') {
        return { items: [runningTask], nextCursor: null, highWatermark: 9 }
      }
      if (method === 'task.runs') {
        return { items: [], nextCursor: 'x'.repeat(513), highWatermark: 1 }
      }
      if (method === 'task.logs') {
        return {
          items: [],
          ackedThroughSequence: 0,
          highWatermark: 0,
          minimumAvailableSequence: 0,
          hasMore: false,
          resetRequired: false,
          taskId: input?.taskId,
        }
      }
      throw new Error(`unexpected method ${method}`)
    })

    const wrapper = mountPanel()
    await flushPromises()
    await wrapper.get('.task-card').trigger('click')
    await flushPromises()

    expect(call.mock.calls.filter(([method]) => method === 'task.runs')).toHaveLength(1)
    expect(wrapper.get('[role="alert"]').text()).toContain('任务运行历史分页游标超过客户端安全上限')
    wrapper.unmount()
  })

  it('switches between list and task graph and creates from the bottom quick composer', async () => {
    listedTasks = [queuedTask]
    const wrapper = mountPanel()
    await flushPromises()

    await wrapper.get('button[title="任务导图"]').trigger('click')
    expect(wrapper.find('.task-graph').exists()).toBe(true)
    expect(wrapper.text()).toContain('全局任务导图')
    await wrapper.get('button[title="列表视图"]').trigger('click')

    await wrapper.get('.quick-composer textarea').setValue('Implement quick task')
    await wrapper.get('.quick-submit').trigger('submit')
    await flushPromises()
    const createCall = call.mock.calls.find(([method]) => method === 'task.create')
    expect(createCall).toBeTruthy()
    expect(createCall?.[1]).toMatchObject({
      definition: {
        projectId,
        kind: 'codex',
        title: 'Implement quick task',
        execution: {
          relation: 'dependency',
          mode: 'serial',
          runImmediately: true,
          resumeCliSession: false,
        },
      },
    })
    wrapper.unmount()
  })

  it('loads latest device logs in task details and stops the whole queue without confirmation', async () => {
    const wrapper = mountPanel()
    await flushPromises()

    await wrapper.get('.task-card').trigger('click')
    await flushPromises()
    expect(wrapper.find('.detail-modal').exists()).toBe(true)
    expect(wrapper.text()).toContain('working')
    expect(wrapper.findAll('.path-value')).toHaveLength(0)
    expect(call).toHaveBeenCalledWith(
      'task.logs',
      { taskId: runningTask.definition.id, limitBytes: 102400, limitLines: 100 },
      { projectId },
    )

    await wrapper.get('button[title="全部停止"]').trigger('click')
    await flushPromises()
    expect(window.confirm).not.toHaveBeenCalled()
    expect(call).toHaveBeenCalledWith(
      'task.queue.stop',
      { expectedHighWatermark: 9 },
      { projectId },
    )
    wrapper.unmount()
  })

  it('drains a newer task-log watermark that arrives during an in-flight read', async () => {
    const wrapper = mountPanel()
    await flushPromises()
    await wrapper.get('.task-card').trigger('click')
    await flushPromises()

    const fallback = call.getMockImplementation()
    let releaseFirstRead: (() => void) | undefined
    let markFirstReadStarted: (() => void) | undefined
    const firstReadStarted = new Promise<void>((resolve) => {
      markFirstReadStarted = resolve
    })
    const logPage = (sequence: number) => ({
      items: [
        {
          taskId: runningTask.definition.id,
          sequence,
          stream: 'stdout',
          content: `burst ${sequence}`,
          occurredAt: `2026-08-18T00:00:0${sequence}Z`,
        },
      ],
      ackedThroughSequence: sequence,
      highWatermark: sequence,
      minimumAvailableSequence: 1,
      hasMore: false,
      resetRequired: false,
    })
    call.mockImplementation(async (method: string, input?: Record<string, unknown>) => {
      if (method === 'task.logs' && input?.afterSequence === 1) {
        markFirstReadStarted?.()
        return new Promise((resolve) => {
          releaseFirstRead = () => resolve(logPage(2))
        })
      }
      if (method === 'task.logs' && input?.afterSequence === 2) return logPage(3)
      return fallback?.(method, input)
    })

    const handler = eventHandler
    if (!handler) throw new Error('task event handler was not registered')
    const refresh = handler(taskLogEvent(2))
    await firstReadStarted
    const trailing = handler(taskLogEvent(3))
    releaseFirstRead?.()
    await Promise.all([refresh, trailing])
    await flushPromises()

    const incrementalInputs = call.mock.calls
      .filter(([method]) => method === 'task.logs')
      .map(([, input]) => input)
      .filter((input) => input?.afterSequence !== undefined)
    expect(incrementalInputs).toEqual([
      expect.objectContaining({ afterSequence: 1 }),
      expect.objectContaining({ afterSequence: 2 }),
    ])
    expect(wrapper.text()).toContain('burst 3')
    wrapper.unmount()
  })

  it('drains a task-log hint received while the initial tail is loading', async () => {
    const fallback = call.getMockImplementation()
    let releaseInitialRead: (() => void) | undefined
    let markInitialReadStarted: (() => void) | undefined
    const initialReadStarted = new Promise<void>((resolve) => {
      markInitialReadStarted = resolve
    })
    const logPage = (sequence: number) => ({
      items: [
        {
          taskId: runningTask.definition.id,
          sequence,
          stream: 'stdout',
          content: `initial burst ${sequence}`,
          occurredAt: `2026-08-18T00:00:0${sequence}Z`,
        },
      ],
      ackedThroughSequence: sequence,
      highWatermark: sequence,
      minimumAvailableSequence: 1,
      hasMore: false,
      resetRequired: false,
    })
    call.mockImplementation(async (method: string, input?: Record<string, unknown>) => {
      if (method === 'task.logs' && input?.limitLines === 100) {
        markInitialReadStarted?.()
        return new Promise((resolve) => {
          releaseInitialRead = () => resolve(logPage(1))
        })
      }
      if (method === 'task.logs' && input?.afterSequence === 1) return logPage(2)
      return fallback?.(method, input)
    })

    const wrapper = mountPanel()
    await flushPromises()
    await wrapper.get('.task-card').trigger('click')
    await initialReadStarted
    const handler = eventHandler
    if (!handler) throw new Error('task event handler was not registered')
    await handler(taskLogEvent(2))
    releaseInitialRead?.()
    await flushPromises()
    await flushPromises()

    expect(
      call.mock.calls.filter(([method]) => method === 'task.logs').map(([, input]) => input),
    ).toEqual([
      expect.objectContaining({ limitLines: 100 }),
      expect.objectContaining({ afterSequence: 1 }),
    ])
    expect(wrapper.text()).toContain('initial burst 2')
    wrapper.unmount()
  })

  it('keeps the live acknowledgement cursor after loading older logs', async () => {
    const fallback = call.getMockImplementation()
    call.mockImplementation(async (method: string, input?: Record<string, unknown>) => {
      if (method !== 'task.logs') return fallback?.(method, input)
      const sequence = input?.beforeSequence === 99 ? 99 : input?.afterSequence === 100 ? 101 : 100
      return {
        items: [
          {
            taskId: runningTask.definition.id,
            sequence,
            stream: 'stdout',
            content: `cursor ${sequence}`,
            occurredAt: '2026-08-18T00:00:01Z',
          },
        ],
        ackedThroughSequence: sequence,
        highWatermark: sequence === 99 ? 100 : sequence,
        minimumAvailableSequence: 1,
        nextBeforeSequence: sequence - 1,
        hasMore: sequence <= 100,
        resetRequired: false,
      }
    })

    const wrapper = mountPanel()
    await flushPromises()
    await wrapper.get('.task-card').trigger('click')
    await flushPromises()
    await wrapper.get('.task-log').trigger('scroll')
    await flushPromises()

    const handler = eventHandler
    if (!handler) throw new Error('task event handler was not registered')
    await handler(taskLogEvent(101))
    await flushPromises()

    const inputs = call.mock.calls
      .filter(([method]) => method === 'task.logs')
      .map(([, input]) => input)
    expect(inputs).toEqual([
      expect.objectContaining({ limitLines: 100 }),
      expect.objectContaining({ beforeSequence: 99 }),
      expect.objectContaining({ afterSequence: 100 }),
    ])
    expect(wrapper.text()).toContain('cursor 101')
    wrapper.unmount()
  })

  it('starts ready waiting tasks concurrently and keeps a separate stop action', async () => {
    listedTasks = [waitingTask]
    const wrapper = mountPanel()
    await flushPromises()

    const runNow = wrapper.get('button[title="立即执行（切换为并发）"]')
    expect(runNow.attributes('disabled')).toBeUndefined()
    await runNow.trigger('click')
    await flushPromises()
    expect(call).toHaveBeenCalledWith(
      'task.start',
      { taskId: waitingTask.definition.id, expectedRevision: 3 },
      { projectId },
    )

    await wrapper.get('.task-card-actions button[title="停止等待"]').trigger('click')
    await flushPromises()
    expect(call).toHaveBeenCalledWith(
      'task.stop',
      { taskId: waitingTask.definition.id, expectedRevision: 3 },
      { projectId },
    )

    await wrapper.get('.task-card').trigger('click')
    await flushPromises()
    expect(buttonWithText(wrapper, '立即执行（并发）').exists()).toBe(true)
    wrapper.unmount()
  })

  it('does not bypass unfinished prerequisites when starting a waiting task', async () => {
    listedTasks = [
      runningTask,
      makeTask(waitingTask.definition.id, 'Blocked waiting task', 'waiting', {
        definition: {
          execution: {
            ...waitingTask.definition.execution,
            relatedTaskIds: [runningTask.definition.id],
          },
        },
      }),
    ]
    const wrapper = mountPanel()
    await flushPromises()

    const blocked = wrapper.get('button[title="正在等待前置任务完成"]')
    expect(blocked.attributes('disabled')).toBeDefined()
    await blocked.trigger('click')
    expect(call.mock.calls.some(([method]) => method === 'task.start')).toBe(false)
    wrapper.unmount()
  })

  it('edits and schedules a queued task through task.update with revision protection', async () => {
    listedTasks = [queuedTask]
    const wrapper = mountPanel()
    await flushPromises()

    await wrapper.get('button[title="编辑任务"]').trigger('click')
    const editor = wrapper.get('.editor-modal')
    await editor.find('input').setValue('Edited queued task')
    await editor.find('textarea').setValue('Updated prompt')
    await editor.trigger('submit')
    await flushPromises()

    expect(call).toHaveBeenCalledWith(
      'task.update',
      {
        definition: expect.objectContaining({
          id: queuedTask.definition.id,
          title: 'Edited queued task',
          config: expect.objectContaining({ promptText: 'Updated prompt' }),
        }),
        expectedRevision: 3,
      },
      { projectId },
    )

    await wrapper.get('button[title="定时启动"]').trigger('click')
    await wrapper.get('input[type="datetime-local"]').setValue('2099-08-18T16:00')
    await wrapper.get('.small-modal').trigger('submit')
    await flushPromises()
    const scheduledCall = call.mock.calls
      .filter(([method]) => method === 'task.update')
      .find(([, input]) => Boolean(input.definition.execution.scheduledAt))
    expect(scheduledCall?.[1]).toMatchObject({
      definition: {
        id: queuedTask.definition.id,
        execution: { scheduledAt: expect.stringContaining('2099-08-18') },
      },
      expectedRevision: 3,
    })
    wrapper.unmount()
  })

  it('creates child and sibling relationships with WenzMark family metadata', async () => {
    listedTasks = [queuedTask]
    const wrapper = mountPanel()
    await flushPromises()

    await buttonWithText(wrapper, '添加子节点').trigger('click')
    await wrapper.get('.editor-modal textarea').setValue('Child prompt')
    await wrapper.get('.editor-modal').trigger('submit')
    await flushPromises()
    const childCall = call.mock.calls.find(([method]) => method === 'task.create')
    expect(childCall?.[1]).toMatchObject({
      definition: {
        parentTaskId: queuedTask.definition.id,
        rootTaskId: queuedTask.definition.id,
        execution: {
          relation: 'dependency',
          mode: 'serial',
          relatedTaskIds: [queuedTask.definition.id],
        },
      },
    })

    await buttonWithText(wrapper, '添加兄弟节点').trigger('click')
    await wrapper.get('.editor-modal textarea').setValue('Sibling prompt')
    await wrapper.findAll('.editor-modal select').at(-1)?.setValue('parallel')
    await wrapper.get('.editor-modal').trigger('submit')
    await flushPromises()
    const createCalls = call.mock.calls.filter(([method]) => method === 'task.create')
    expect(createCalls.at(-1)?.[1]).toMatchObject({
      definition: {
        execution: {
          relation: 'sibling',
          mode: 'parallel',
          relatedTaskIds: [queuedTask.definition.id],
        },
      },
    })
    wrapper.unmount()
  })

  it('allows changesRequested to rerun with edited prompt/model and resume a CLI session', async () => {
    listedTasks = [changesTask]
    const wrapper = mountPanel()
    await flushPromises()

    await wrapper.get('.round-action').trigger('click')
    await flushPromises()
    expect(wrapper.find('.rerun-modal').exists()).toBe(true)
    await wrapper.get('.rerun-modal textarea').setValue('Address every review item')
    await wrapper
      .get('.rerun-modal input[placeholder="留空使用设备默认模型"]')
      .setValue('gpt-5.6-codex')
    await wrapper.get('.rerun-modal').trigger('submit')
    await flushPromises()

    expect(call).toHaveBeenCalledWith(
      'task.update',
      {
        definition: expect.objectContaining({
          config: expect.objectContaining({
            promptText: 'Address every review item',
            model: 'gpt-5.6-codex',
          }),
        }),
        expectedRevision: 3,
      },
      { projectId },
    )
    expect(call).toHaveBeenCalledWith(
      'task.retry',
      { taskId: changesTask.definition.id, expectedRevision: 4 },
      { projectId },
    )

    await wrapper.get('.round-action').trigger('click')
    await flushPromises()
    await wrapper.get('.rerun-modal input[type="checkbox"]').setValue(true)
    await wrapper.get('.rerun-modal').trigger('submit')
    await flushPromises()
    const resumedUpdate = call.mock.calls
      .filter(([method]) => method === 'task.update')
      .find(([, input]) => input.definition.execution.resumeCliSession === true)
    expect(resumedUpdate?.[1]).toMatchObject({
      definition: {
        execution: { cliSessionId: 'cli-session-1', resumeCliSession: true },
      },
      expectedRevision: 3,
    })
    wrapper.unmount()
  })

  it('appends review feedback as a follow-up and supports acceptance-next actions', async () => {
    listedTasks = [
      acceptanceTask,
      {
        ...acceptanceTask,
        definition: {
          ...acceptanceTask.definition,
          id: queuedTask.definition.id,
          title: 'Next acceptance',
        },
      },
    ]
    const wrapper = mountPanel()
    await flushPromises()

    await wrapper.get('button[title="追加后续任务"]').trigger('click')
    await wrapper.get('.small-modal textarea').setValue('Fix the missing verification')
    await wrapper.get('.small-modal').trigger('submit')
    await flushPromises()
    expect(call).toHaveBeenCalledWith(
      'task.follow-up',
      {
        sourceTaskId: acceptanceTask.definition.id,
        taskId: expect.any(String),
        expectedRevision: 3,
        feedback: 'Fix the missing verification',
      },
      { projectId },
    )

    await wrapper.get('.task-card').trigger('click')
    await flushPromises()
    await buttonWithText(wrapper, '验收并进入下一项').trigger('click')
    await flushPromises()
    expect(call).toHaveBeenCalledWith(
      'task.accept',
      {
        taskId: acceptanceTask.definition.id,
        expectedRevision: 3,
        evidence: '远程验收通过',
      },
      { projectId },
    )
    wrapper.unmount()
  })
})
