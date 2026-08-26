import { mount } from '@vue/test-utils'
import { defineComponent, h } from 'vue'
import { describe, expect, it } from 'vitest'

import RemoteSidePanel from './RemoteSidePanel.vue'

const FilesStub = defineComponent({
  name: 'RemoteFilesPanel',
  setup: () => () => h('div', { class: 'files-stub' }, '文件视图'),
})

const TasksStub = defineComponent({
  name: 'RemoteTasksPanel',
  setup: () => () => h('div', { class: 'tasks-stub' }, '任务视图'),
})

const mountPanel = () =>
  mount(RemoteSidePanel, {
    props: {
      deviceId: 'device-1',
      deviceName: '研发工作站',
      projectId: 'project-1',
      protocolVersion: 1,
      capabilityVersion: 'capability-1',
      online: true,
      writable: true,
      filesAvailable: true,
      tasksAvailable: true,
    },
    global: {
      stubs: {
        RemoteFilesPanel: FilesStub,
        RemoteTasksPanel: TasksStub,
      },
    },
  })

const buttonWithText = (wrapper: ReturnType<typeof mountPanel>, text: string) => {
  const button = wrapper.findAll('button').find((candidate) => candidate.text().includes(text))
  if (!button) throw new Error(`找不到按钮：${text}`)
  return button
}

describe('RemoteSidePanel', () => {
  it('retains inactive tab state and supports desktop-style tab closing actions', async () => {
    const wrapper = mountPanel()

    await buttonWithText(wrapper, '阅读文件').trigger('click')
    await buttonWithText(wrapper, '任务管理').trigger('click')

    expect(wrapper.findAll('.files-stub')).toHaveLength(1)
    expect(wrapper.findAll('.tasks-stub')).toHaveLength(1)
    expect(wrapper.findAll('.side-tab')).toHaveLength(2)

    await wrapper.findAll('.side-tab')[0]!.trigger('contextmenu', { clientX: 40, clientY: 50 })
    expect(wrapper.get('.side-tab-context-menu').text()).toContain('关闭右侧')
    await buttonWithText(wrapper, '关闭右侧').trigger('click')

    expect(wrapper.findAll('.side-tab')).toHaveLength(1)
    expect(wrapper.find('.tasks-stub').exists()).toBe(false)

    await wrapper.setProps({ projectId: 'project-2' })
    expect(wrapper.findAll('.side-tab')).toHaveLength(0)
    expect(wrapper.text()).toContain('选择要在右侧打开的视图')

    wrapper.unmount()
  })

  it('matches the desktop quick-link set', async () => {
    const wrapper = mountPanel()
    await buttonWithText(wrapper, '浏览网页').trigger('click')

    expect(wrapper.text()).toContain('pub.dev')
    expect(wrapper.text()).toContain('Dart')
    wrapper.unmount()
  })
})
