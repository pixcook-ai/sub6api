import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { mount, type VueWrapper } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { nextTick } from 'vue'
import type { AdminComplianceStatus } from '@/api/admin/compliance'

const { driverMock, acceptMock } = vi.hoisted(() => ({
  driverMock: vi.fn(),
  acceptMock: vi.fn()
}))

vi.mock('driver.js', () => ({ driver: driverMock }))
vi.mock('@/api/admin/compliance', () => ({
  default: { getStatus: vi.fn(), accept: acceptMock }
}))
vi.mock('@/components/layout/AppSidebar.vue', () => ({ default: { template: '<aside />' } }))
vi.mock('@/components/layout/AppHeader.vue', () => ({ default: { template: '<header />' } }))
vi.mock('vue-i18n', async () => ({
  ...await vi.importActual<typeof import('vue-i18n')>('vue-i18n'),
  useI18n: () => ({ t: (key: string) => key })
}))

import AppLayout from '../AppLayout.vue'
import { useAuthStore } from '@/stores/auth'
import { useAdminComplianceStore } from '@/stores/adminCompliance'
import { useOnboardingStore } from '@/stores/onboarding'

function complianceStatus(required: boolean): AdminComplianceStatus {
  return {
    required,
    version: 'test-v1',
    document_path_zh: 'test.zh.md',
    document_path_en: 'test.en.md',
    document_url_zh: '/test.zh.md',
    document_url_en: '/test.en.md',
    ack_phrase_zh: 'test agreement',
    ack_phrase_en: 'test agreement'
  }
}

describe('AppLayout onboarding compliance ordering', () => {
  let wrapper: VueWrapper | undefined

  beforeEach(() => {
    vi.useFakeTimers()
    localStorage.clear()
    setActivePinia(createPinia())
    const auth = useAuthStore()
    auth.$patch({ user: { id: 42, role: 'admin' } })
    let active = false
    driverMock.mockReset().mockImplementation((options) => ({
      drive: vi.fn(() => { active = true }),
      destroy: vi.fn(() => {
        active = false
        options.onDestroyed()
      }),
      isActive: () => active
    }))
    acceptMock.mockReset().mockResolvedValue(complianceStatus(false))
  })

  afterEach(() => {
    useOnboardingStore().getDriverInstance()?.destroy()
    wrapper?.unmount()
    wrapper = undefined
    vi.clearAllTimers()
    vi.useRealTimers()
  })

  it('waits for compliance status and acknowledgement before starting the first tour', async () => {
    const compliance = useAdminComplianceStore()
    wrapper = mount(AppLayout)

    await vi.advanceTimersByTimeAsync(1500)
    expect(driverMock).not.toHaveBeenCalled()

    compliance.requireAcknowledgement(complianceStatus(true))
    await nextTick()
    await vi.advanceTimersByTimeAsync(1500)
    expect(driverMock).not.toHaveBeenCalled()

    acceptMock.mockRejectedValueOnce(new Error('accept failed'))
    await expect(compliance.accept('test agreement')).rejects.toThrow('accept failed')
    await vi.advanceTimersByTimeAsync(1500)
    expect(driverMock).not.toHaveBeenCalled()

    await compliance.accept('test agreement')
    await nextTick()
    await vi.advanceTimersByTimeAsync(1000)
    expect(driverMock).toHaveBeenCalledTimes(1)
  })

  it('preserves the completed tour marker when compliance acknowledgement is required again', async () => {
    const key = 'admin_guide_42_admin_v4_interactive'
    localStorage.setItem(key, 'true')
    const compliance = useAdminComplianceStore()
    compliance.requireAcknowledgement(complianceStatus(true))
    wrapper = mount(AppLayout)

    await compliance.accept('test agreement')
    await vi.advanceTimersByTimeAsync(1500)

    expect(driverMock).not.toHaveBeenCalled()
    expect(localStorage.getItem(key)).toBe('true')
  })

  it('cancels an automatic start if compliance becomes required during the delay', async () => {
    const compliance = useAdminComplianceStore()
    compliance.$patch({ initialized: true, status: complianceStatus(false) })
    wrapper = mount(AppLayout)
    await vi.advanceTimersByTimeAsync(500)

    compliance.requireAcknowledgement(complianceStatus(true))
    await nextTick()
    await vi.advanceTimersByTimeAsync(1500)
    expect(driverMock).not.toHaveBeenCalled()

    await compliance.accept('test agreement')
    await vi.advanceTimersByTimeAsync(1000)
    expect(driverMock).toHaveBeenCalledTimes(1)
  })

  it('keeps manual replay behind compliance without clearing an existing completion marker', async () => {
    const key = 'admin_guide_42_admin_v4_interactive'
    localStorage.setItem(key, 'true')
    const compliance = useAdminComplianceStore()
    compliance.requireAcknowledgement(complianceStatus(true))
    wrapper = mount(AppLayout)

    useOnboardingStore().replay()
    await nextTick()
    expect(driverMock).not.toHaveBeenCalled()
    expect(localStorage.getItem(key)).toBe('true')

    await compliance.accept('test agreement')
    useOnboardingStore().replay()
    await nextTick()
    expect(driverMock).toHaveBeenCalledTimes(1)
    expect(localStorage.getItem(key)).toBeNull()
  })

  it('does not schedule a tour after the layout is unmounted', async () => {
    const compliance = useAdminComplianceStore()
    compliance.$patch({ initialized: true, status: complianceStatus(false) })
    wrapper = mount(AppLayout)
    await vi.advanceTimersByTimeAsync(500)
    wrapper.unmount()
    wrapper = undefined

    await vi.advanceTimersByTimeAsync(1500)
    expect(driverMock).not.toHaveBeenCalled()
  })

  it('does not automatically start the administrator tour for an ordinary user', async () => {
    useAuthStore().$patch({ user: { id: 43, role: 'user' } })
    wrapper = mount(AppLayout)

    await vi.advanceTimersByTimeAsync(1500)
    expect(driverMock).not.toHaveBeenCalled()
  })
})
