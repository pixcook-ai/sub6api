import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import TiboSettingsPanel from '../TiboSettingsPanel.vue'

const { getSettings, updateSettings, getGroups } = vi.hoisted(() => ({
  getSettings: vi.fn(), updateSettings: vi.fn(), getGroups: vi.fn()
}))

vi.mock('@/api/admin/tibo', async importOriginal => ({
  ...(await importOriginal<typeof import('@/api/admin/tibo')>()),
  getTiboSettings: getSettings,
  updateTiboSettings: updateSettings
}))
vi.mock('@/api/admin/groups', () => ({ getAll: getGroups }))
vi.mock('vue-i18n', async importOriginal => ({ ...(await importOriginal<typeof import('vue-i18n')>()), useI18n: () => ({ t: (key: string) => key }) }))

const settings = () => ({
  enabled: false, auto_group_enabled: false, healthy_group_id: 0,
  degraded_group_id: 0, pause_on_degraded: true, auto_resume_on_healthy: false
})

beforeEach(() => {
  vi.resetAllMocks()
  getSettings.mockResolvedValue(settings())
  updateSettings.mockResolvedValue(settings())
  getGroups.mockResolvedValue([
    { id: 14, name: 'Renamed healthy', platform: 'openai', status: 'active' },
    { id: 27, name: 'Custom fallback', platform: 'openai', status: 'active' },
    { id: 33, name: 'Inactive', platform: 'openai', status: 'inactive' },
    { id: 44, name: 'Other platform', platform: 'anthropic', status: 'active' }
  ])
})

describe('Tibo settings', () => {
  it('saves probes and isolation with grouping off and no group IDs', async () => {
    const wrapper = mount(TiboSettingsPanel)
    await flushPromises()
    expect(wrapper.find('select').exists()).toBe(false)
    await wrapper.get('[data-testid="tibo-enabled"]').trigger('click')
    await wrapper.get('[data-testid="tibo-save"]').trigger('click')
    await flushPromises()
    expect(updateSettings).toHaveBeenCalledWith({ ...settings(), enabled: true })
    expect(wrapper.find('[role="status"]').text()).toBe('admin.settings.tibo.saved')
    expect(getGroups).toHaveBeenCalledWith('openai')
    wrapper.unmount()
  })

  it('selects groups by numeric ID and blocks missing or duplicate choices', async () => {
    const wrapper = mount(TiboSettingsPanel)
    await flushPromises()
    await wrapper.get('[data-testid="tibo-auto-group"]').trigger('click')
    const save = wrapper.get<HTMLButtonElement>('[data-testid="tibo-save"]')
    expect(save.element.disabled).toBe(true)
    const healthy = wrapper.get('[data-testid="healthy_group_id"]')
    const degraded = wrapper.get('[data-testid="degraded_group_id"]')
    expect(healthy.text()).toContain('Renamed healthy')
    expect(healthy.text()).not.toContain('Inactive')
    expect(healthy.text()).not.toContain('Other platform')
    await healthy.setValue('14')
    await degraded.setValue('14')
    expect(save.element.disabled).toBe(true)
    await degraded.setValue('27')
    expect(save.element.disabled).toBe(false)
    await save.trigger('click')
    await flushPromises()
    expect(updateSettings).toHaveBeenCalledWith({ ...settings(), auto_group_enabled: true, healthy_group_id: 14, degraded_group_id: 27 })
    wrapper.unmount()
  })

  it('does not replace an unavailable configured group with the first available group', async () => {
    getSettings.mockResolvedValue({ ...settings(), auto_group_enabled: true, healthy_group_id: 999, degraded_group_id: 27 })
    const wrapper = mount(TiboSettingsPanel)
    await flushPromises()
    expect(wrapper.get<HTMLSelectElement>('[data-testid="healthy_group_id"]').element.value).toBe('999')
    expect(wrapper.get<HTMLButtonElement>('[data-testid="tibo-save"]').element.disabled).toBe(true)
    await wrapper.get('[data-testid="tibo-auto-group"]').trigger('click')
    expect(wrapper.get<HTMLButtonElement>('[data-testid="tibo-save"]').element.disabled).toBe(false)
    wrapper.unmount()
  })

  it('does not allow overwriting unknown settings after a failed load', async () => {
    getSettings.mockRejectedValue(new Error('settings unavailable'))
    const wrapper = mount(TiboSettingsPanel)
    await flushPromises()
    expect(wrapper.find('[data-testid="tibo-save"]').exists()).toBe(false)
    expect(wrapper.get('[role="alert"]').text()).toContain('settings unavailable')
    expect(updateSettings).not.toHaveBeenCalled()
    wrapper.unmount()
  })

  it('still permits observation-only configuration if the optional group list fails', async () => {
    getGroups.mockRejectedValue(new Error('groups unavailable'))
    const wrapper = mount(TiboSettingsPanel)
    await flushPromises()
    expect(wrapper.get<HTMLButtonElement>('[data-testid="tibo-save"]').element.disabled).toBe(false)
    await wrapper.get('[data-testid="tibo-auto-group"]').trigger('click')
    expect(wrapper.get<HTMLButtonElement>('[data-testid="tibo-save"]').element.disabled).toBe(true)
    expect(wrapper.text()).toContain('groups unavailable')
    wrapper.unmount()
  })

  it('shows a save failure without reporting success', async () => {
    updateSettings.mockRejectedValue(new Error('write failed'))
    const wrapper = mount(TiboSettingsPanel)
    await flushPromises()
    await wrapper.get('[data-testid="tibo-save"]').trigger('click')
    await flushPromises()
    expect(wrapper.get('[role="alert"]').text()).toContain('write failed')
    expect(wrapper.find('[role="status"]').exists()).toBe(false)
    wrapper.unmount()
  })
})
