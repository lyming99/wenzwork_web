import { mount } from '@vue/test-utils'
import { createPinia } from 'pinia'
import { createMemoryHistory, createRouter } from 'vue-router'
import { describe, expect, it } from 'vitest'

import AccountLayout from './AccountLayout.vue'

describe('AccountLayout remote manager entry', () => {
  it('keeps the remote device catalog inside account management', async () => {
    const router = createRouter({
      history: createMemoryHistory(),
      routes: [
        { path: '/account', name: 'account', component: { template: '<div />' } },
        { path: '/account/remote', name: 'account-remote', component: { template: '<div />' } },
      ],
    })
    await router.push('/account')
    await router.isReady()
    const wrapper = mount(AccountLayout, {
      global: {
        plugins: [createPinia(), router],
        stubs: { RouterView: true },
      },
    })

    expect(wrapper.get('a[href="/account/remote"]').text()).toContain('远程设备')
  })
})
