import { flushPromises, mount } from '@vue/test-utils'
import { createMemoryHistory, createRouter } from 'vue-router'
import { describe, expect, it, vi } from 'vitest'

import PublicLayout from './PublicLayout.vue'

const bootstrapMock = vi.hoisted(() => vi.fn().mockResolvedValue(undefined))

vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({
    isAuthenticated: false,
    bootstrap: bootstrapMock,
  }),
}))

describe('PublicLayout mobile navigation', () => {
  it('navigates before closing the expanded menu', async () => {
    const router = createRouter({
      history: createMemoryHistory(),
      routes: [
        { path: '/', component: { template: '<p>首页内容</p>' } },
        { path: '/help', component: { template: '<p>帮助内容</p>' } },
        { path: '/download', component: { template: '<p>下载内容</p>' } },
        { path: '/pricing', component: { template: '<p>价格内容</p>' } },
        { path: '/login', component: { template: '<p>登录内容</p>' } },
      ],
    })
    await router.push('/')
    await router.isReady()

    const wrapper = mount(PublicLayout, {
      global: {
        plugins: [router],
        stubs: { BrandLogo: true },
      },
    })

    await wrapper.get('.menu-button').trigger('click')
    expect(wrapper.get('#primary-navigation').classes()).toContain('open')

    await wrapper.get('.primary-nav a[href="/pricing"]').trigger('click')
    await flushPromises()

    expect(router.currentRoute.value.fullPath).toBe('/pricing')
    expect(wrapper.get('#primary-navigation').classes()).not.toContain('open')
    expect(wrapper.get('#main-content').text()).toContain('价格内容')
    expect(wrapper.get('.footer-contact').text()).toContain('QQ 交流群：1026582431')
    expect(wrapper.get('.product-menu-panel a[href="https://wenzflow.com"]').text()).toContain(
      'WenzFlow',
    )
    expect(wrapper.get('.product-menu-panel a[href="https://wenzmark.cn"]').text()).toContain(
      'WenzMark',
    )
    expect(wrapper.get('.product-menu-panel a[href="https://work.wenzflow.com"]').text()).toContain(
      'WenzWork',
    )
    expect(wrapper.get('.site-footer a[href="https://github.com/lyming99/wenzwork"]').text()).toBe(
      'GitHub 开源',
    )
  })
})
