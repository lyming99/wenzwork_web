import { flushPromises, mount } from '@vue/test-utils'
import { createMemoryHistory, createRouter } from 'vue-router'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import { listPublishedHelpDocuments } from '@/api/help'

import HelpDocumentTree from './HelpDocumentTree.vue'

vi.mock('@/api/help', () => ({
  listPublishedHelpDocuments: vi.fn(),
}))

const listMock = vi.mocked(listPublishedHelpDocuments)

describe('HelpDocumentTree', () => {
  beforeEach(() => listMock.mockReset())

  it('keeps grouped local documents available and highlights the current article', async () => {
    listMock.mockResolvedValue([])
    const router = createRouter({
      history: createMemoryHistory(),
      routes: [{ path: '/help/:slug?', component: { template: '<div />' } }],
    })
    await router.push('/help/getting-started')
    await router.isReady()

    const wrapper = mount(HelpDocumentTree, {
      props: { activeSlug: 'getting-started' },
      global: { plugins: [router] },
    })
    await flushPromises()

    expect(wrapper.text()).toContain('快速开始')
    expect(wrapper.text()).toContain('账户与会员')
    expect(wrapper.get('[aria-current="page"]').text()).toContain('WenzWork 快速开始')
    expect(wrapper.findAll('.help-tree-link').length).toBeGreaterThanOrEqual(9)

    await wrapper.get('input[type="search"]').setValue('兑换码')
    expect(wrapper.findAll('.help-tree-link')).toHaveLength(1)
    expect(wrapper.get('.help-tree-link').text()).toContain('使用会员兑换码')
  })

  it('merges published managed documents into the category tree', async () => {
    listMock.mockResolvedValue([
      {
        slug: 'managed-release-notes',
        title: '发布说明',
        description: 'Managed document',
        category: '产品动态',
        sortOrder: 5,
        updatedAt: '2026-07-21T00:00:00Z',
        searchText: '发布说明 managed document 产品动态',
      },
    ])
    const router = createRouter({
      history: createMemoryHistory(),
      routes: [{ path: '/help/:slug?', component: { template: '<div />' } }],
    })
    const wrapper = mount(HelpDocumentTree, { global: { plugins: [router] } })
    await flushPromises()

    expect(wrapper.text()).toContain('产品动态')
    expect(wrapper.get('a[href="/help/managed-release-notes"]').text()).toContain('发布说明')
  })
})
