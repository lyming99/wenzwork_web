import { createHead } from '@unhead/vue/client'
import { mount } from '@vue/test-utils'
import { createPinia } from 'pinia'
import { createMemoryHistory, createRouter } from 'vue-router'
import { afterEach, describe, expect, it } from 'vitest'

import { REMOTE_MANAGER_WINDOW_NAME } from '@/utils/remoteManagerWindow'

import RemoteAppLayout from './RemoteAppLayout.vue'

describe('RemoteAppLayout', () => {
  afterEach(() => (window.name = ''))

  it('claims the unique app window name and renders the standalone workspace shell', async () => {
    const router = createRouter({
      history: createMemoryHistory(),
      routes: [{ path: '/remote', name: 'remote-app', component: { template: '<div />' } }],
    })
    await router.push('/remote')
    await router.isReady()
    const wrapper = mount(RemoteAppLayout, {
      global: {
        plugins: [createPinia(), router, createHead()],
        stubs: { RouterView: true },
      },
    })

    expect(window.name).toBe(REMOTE_MANAGER_WINDOW_NAME)
    expect(wrapper.find('.remote-app-shell').exists()).toBe(true)
  })
})
