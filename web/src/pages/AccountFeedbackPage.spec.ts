import { createHead } from '@unhead/vue/client'
import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import { createMyFeedback, listMyFeedback } from '@/api/feedback'
import { useAuthStore } from '@/stores/auth'

import AccountFeedbackPage from './AccountFeedbackPage.vue'

vi.mock('@/api/feedback', () => ({
  listMyFeedback: vi.fn(),
  createMyFeedback: vi.fn(),
}))

describe('AccountFeedbackPage', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    setActivePinia(createPinia())
    useAuthStore().user = {
      id: 'user-1',
      email: 'member@example.test',
      displayName: 'Member',
      status: 'active',
      emailVerifiedAt: '2026-07-21T00:00:00Z',
      roles: ['user'],
    }
    vi.mocked(listMyFeedback).mockResolvedValue([])
  })

  it('submits feedback through the API and shows it in member history', async () => {
    vi.mocked(createMyFeedback).mockResolvedValue({
      id: 'feedback-1',
      category: 'bug',
      subject: '导出失败',
      content: '点击导出后没有响应。',
      contactEmail: 'member@example.test',
      status: 'pending',
      adminReply: '',
      resolvedAt: null,
      createdAt: '2026-07-21T00:00:00Z',
      updatedAt: '2026-07-21T00:00:00Z',
    })
    const wrapper = mount(AccountFeedbackPage, { global: { plugins: [createHead()] } })
    await flushPromises()
    await wrapper.get('#feedback-category').setValue('bug')
    await wrapper.get('#feedback-subject').setValue('导出失败')
    await wrapper.get('#feedback-content').setValue('点击导出后没有响应。')
    await wrapper.get('form.feedback-form').trigger('submit')
    await flushPromises()

    expect(createMyFeedback).toHaveBeenCalledWith({
      category: 'bug',
      subject: '导出失败',
      content: '点击导出后没有响应。',
      contactEmail: 'member@example.test',
    })
    expect(wrapper.text()).toContain('反馈已提交')
    expect(wrapper.text()).toContain('待处理')
  })
})
