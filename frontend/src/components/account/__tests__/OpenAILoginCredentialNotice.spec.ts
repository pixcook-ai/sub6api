import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createPinia, setActivePinia } from 'pinia'
import { createI18n } from 'vue-i18n'
import OpenAILoginCredentialNotice from '../OpenAILoginCredentialNotice.vue'

const api = vi.hoisted(() => ({
  getLoginCredentialStatus: vi.fn(),
  deleteLoginCredential: vi.fn()
}))
vi.mock('@/api/admin/openaiBatchLogin', () => api)

function render() {
  return mount(OpenAILoginCredentialNotice, {
    props: { accountId: 7 },
    global: {
      plugins: [
        createI18n({ legacy: false, locale: 'en', missingWarn: false, fallbackWarn: false, messages: { en: {} } })
      ]
    }
  })
}

describe('OpenAILoginCredentialNotice', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    api.getLoginCredentialStatus.mockReset()
    api.deleteLoginCredential.mockReset()
  })

  it('renders nothing when no credential is stored', async () => {
    api.getLoginCredentialStatus.mockResolvedValue({ saved: false })
    const wrapper = render()
    await flushPromises()
    expect(wrapper.find('[data-testid="login-credential-notice"]').exists()).toBe(false)
    expect(api.getLoginCredentialStatus).toHaveBeenCalledWith(7)
  })

  it('hides the notice when the status request fails', async () => {
    api.getLoginCredentialStatus.mockRejectedValue(new Error('x'))
    const wrapper = render()
    await flushPromises()
    expect(wrapper.find('[data-testid="login-credential-notice"]').exists()).toBe(false)
  })

  it('shows the notice and deletes the stored credential', async () => {
    api.getLoginCredentialStatus.mockResolvedValue({ saved: true })
    api.deleteLoginCredential.mockResolvedValue(undefined)
    const wrapper = render()
    await flushPromises()
    expect(wrapper.find('[data-testid="login-credential-notice"]').exists()).toBe(true)
    await wrapper.get('[data-testid="login-credential-delete"]').trigger('click')
    await flushPromises()
    expect(api.deleteLoginCredential).toHaveBeenCalledWith(7)
    expect(wrapper.find('[data-testid="login-credential-notice"]').exists()).toBe(false)
  })
})
