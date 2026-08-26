import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import { remoteRPCKey, type RemoteRPCClient } from '@/remote/rpcTypes'

import RemoteProjectsPanel from './RemoteProjectsPanel.vue'

const mocks = vi.hoisted(() => ({
  listRemoteProjects: vi.fn(),
  syncRemoteProjects: vi.fn(),
}))

vi.mock('@/api/remote', () => ({
  listRemoteProjects: mocks.listRemoteProjects,
  syncRemoteProjects: mocks.syncRemoteProjects,
}))

vi.mock('@/api/auth', () => ({
  problemMessage: (error: unknown, fallback: string) =>
    error instanceof Error && error.message ? error.message : fallback,
}))

const project = {
  id: '11111111-1111-4111-8111-111111111111',
  displayName: 'Disposable project',
  revision: 7,
  capabilities: ['files'],
  state: 'available',
  observedAt: '2026-08-19T08:00:00Z',
}

const mountPanel = (call: RemoteRPCClient['call']) =>
  mount(RemoteProjectsPanel, {
    props: {
      deviceId: 'device-1',
      canSync: true,
      canDelete: true,
      selectedProjectId: project.id,
    },
    global: {
      provide: {
        [remoteRPCKey as symbol]: { call } as unknown as RemoteRPCClient,
      },
    },
  })

describe('RemoteProjectsPanel', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    mocks.listRemoteProjects.mockResolvedValue({
      items: [project],
      nextCursor: null,
      stale: false,
      deviceOnline: true,
      observedAt: '2026-08-19T08:00:00Z',
    })
    mocks.syncRemoteProjects.mockResolvedValue({ id: 'sync-1', status: 'queued' })
    Object.defineProperty(window, 'confirm', {
      configurable: true,
      value: vi.fn(() => true),
    })
  })

  it('confirms a revision-guarded record deletion and keeps the directory untouched', async () => {
    const call = vi.fn(async () => ({
      removed: true,
      projectId: project.id,
      state: 'removed',
      revision: 8,
    })) as RemoteRPCClient['call']
    const wrapper = mountPanel(call)
    await flushPromises()

    await wrapper.get('.project-delete').trigger('click')
    await flushPromises()

    expect(window.confirm).toHaveBeenCalledWith(
      expect.stringContaining('文件夹和 Git 仓库不会被删除'),
    )
    expect(window.confirm).toHaveBeenCalledWith(expect.stringContaining('这些数据会继续保留'))
    expect(window.confirm).toHaveBeenCalledWith(
      expect.stringContaining('正在生成的 AI 对话或未结束的任务'),
    )
    expect(call).toHaveBeenCalledWith('project.remove', {
      projectId: project.id,
      expectedRevision: project.revision,
    })
    expect(mocks.syncRemoteProjects).toHaveBeenCalledWith('device-1')
    expect(wrapper.text()).not.toContain(project.displayName)
    expect(wrapper.text()).toContain('设备文件夹、Git 仓库及已有对话/任务数据均未删除')
  })

  it('explains that unfinished tasks must end first', async () => {
    const call = vi.fn(async () => {
      throw new Error('设备拒绝了远程操作（PROJECT_HAS_TASKS）。')
    }) as RemoteRPCClient['call']
    const wrapper = mountPanel(call)
    await flushPromises()

    await wrapper.get('.project-delete').trigger('click')
    await flushPromises()

    expect(wrapper.text()).toContain('请先结束或取消后再删除项目记录')
    expect(wrapper.text()).toContain(project.displayName)
    expect(mocks.syncRemoteProjects).not.toHaveBeenCalled()
  })

  it('keeps the project visible when the Agent returns an invalid tombstone', async () => {
    const call = vi.fn(async () => ({
      removed: true,
      projectId: project.id,
      state: 'removed',
      revision: project.revision,
    })) as RemoteRPCClient['call']
    const wrapper = mountPanel(call)
    await flushPromises()

    await wrapper.get('.project-delete').trigger('click')
    await flushPromises()

    expect(wrapper.text()).toContain('设备返回了无效的项目删除结果')
    expect(wrapper.text()).toContain(project.displayName)
    expect(mocks.syncRemoteProjects).not.toHaveBeenCalled()
  })

  it('shows a newer same-ID registration after the delete tombstone', async () => {
    vi.useFakeTimers()
    try {
      const call = vi.fn(async () => ({
        removed: true,
        projectId: project.id,
        state: 'removed',
        revision: 8,
      })) as RemoteRPCClient['call']
      const wrapper = mountPanel(call)
      await flushPromises()
      await wrapper.get('.project-delete').trigger('click')
      await flushPromises()
      expect(wrapper.text()).not.toContain(project.displayName)

      mocks.listRemoteProjects.mockResolvedValue({
        items: [{ ...project, displayName: 'Restored project', revision: 9 }],
        nextCursor: null,
        stale: false,
        deviceOnline: true,
        observedAt: '2026-08-23T03:00:00Z',
      })
      await wrapper.get('.remote-panel-heading button').trigger('click')
      await flushPromises()
      await vi.advanceTimersByTimeAsync(1200)
      await flushPromises()

      expect(wrapper.text()).toContain('Restored project')
    } finally {
      vi.useRealTimers()
    }
  })

  it('keeps an old deleted projection hidden when it appears on a later page', async () => {
    vi.useFakeTimers()
    try {
      const call = vi.fn(async () => ({
        removed: true,
        projectId: project.id,
        state: 'removed',
        revision: 8,
      })) as RemoteRPCClient['call']
      const wrapper = mountPanel(call)
      await flushPromises()
      await wrapper.get('.project-delete').trigger('click')
      await flushPromises()

      const otherProject = {
        ...project,
        id: '22222222-2222-4222-8222-222222222222',
        displayName: 'First-page project',
      }
      mocks.listRemoteProjects
        .mockResolvedValueOnce({
          items: [otherProject],
          nextCursor: 'next-page',
          stale: false,
          deviceOnline: true,
          observedAt: '2026-08-23T03:00:00Z',
        })
        .mockResolvedValueOnce({
          items: [project],
          nextCursor: null,
          stale: true,
          deviceOnline: true,
          observedAt: '2026-08-19T08:00:00Z',
        })
      await wrapper.get('.remote-panel-heading button').trigger('click')
      await flushPromises()
      await vi.advanceTimersByTimeAsync(1200)
      await flushPromises()
      await wrapper.get('.remote-load-more').trigger('click')
      await flushPromises()

      expect(wrapper.text()).toContain(otherProject.displayName)
      expect(wrapper.text()).not.toContain(project.displayName)
    } finally {
      vi.useRealTimers()
    }
  })
})
