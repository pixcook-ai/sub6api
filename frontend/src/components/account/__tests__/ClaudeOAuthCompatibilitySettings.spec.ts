import { describe, expect, it, vi, beforeEach } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import type { ClaudeOAuthCompatibilityObservation } from '@/api/admin/accounts'

const { getObservation } = vi.hoisted(() => ({ getObservation: vi.fn() }))
vi.mock('@/api/admin', () => ({ adminAPI: { accounts: { getClaudeOAuthCompatibility: getObservation } } }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))
import ClaudeOAuthCompatibilitySettings from '../ClaudeOAuthCompatibilitySettings.vue'

const observation: ClaudeOAuthCompatibilityObservation = {
  observed_at: '2026-11-23T00:00:00Z', endpoint: 'messages', mode: 'transparent', client_version: '2.1.330',
  incoming_attribution_blocks: 1, outgoing_attribution_blocks: 1, cc_gateway_present: true, cch_present: true,
  unknown_attribution_fields: 1, wiggly_present: true, wiggly_forwarded: false,
  additional_protection_present: false, additional_protection_forwarded: false, body_changed: true,
  metadata_changed: false, beta_changed: false, integrity_status: 'unverified', reasons: ['opaque_metadata_unverified', 'wiggly_dropped_by_allowlist']
}

describe('Claude OAuth compatibility settings', () => {
  beforeEach(() => getObservation.mockReset())

  it('is read-only and exposes no attribution rewriting controls', async () => {
    getObservation.mockResolvedValue({ account_id: 1, attribution_mode: 'transparent', observation: null })
    const wrapper = mount(ClaudeOAuthCompatibilitySettings, { props: { accountId: 1 } })
    await flushPromises()
    expect(wrapper.find('select').exists()).toBe(false)
    expect(wrapper.find('input').exists()).toBe(false)
    expect(wrapper.get('[data-testid="claude-compatibility-mode"]').text()).toContain('claudeCompatibility.transparentMode')
    expect(wrapper.emitted('update:modelValue')).toBeUndefined()
    expect(getObservation).toHaveBeenCalledWith(1)
  })

  it('shows an explicit empty state without treating it as a pass', async () => {
    getObservation.mockResolvedValue({ account_id: 1, attribution_mode: 'transparent', observation: null })
    const wrapper = mount(ClaudeOAuthCompatibilitySettings, { props: { accountId: 1 } })
    await flushPromises()
    expect(getObservation).toHaveBeenCalledWith(1)
    expect(wrapper.get('[data-testid="claude-compatibility-empty"]').text()).toContain('claudeCompatibility.empty')
    expect(wrapper.find('[data-testid="claude-compatibility-observation"]').exists()).toBe(false)
  })

  it('refreshes safe facts without exposing an attribution policy control', async () => {
    getObservation.mockResolvedValueOnce({ account_id: 1, attribution_mode: 'transparent', observation: null })
      .mockResolvedValueOnce({ account_id: 1, attribution_mode: 'transparent', observation })
    const wrapper = mount(ClaudeOAuthCompatibilitySettings, { props: { accountId: 1 } })
    await flushPromises()
    await wrapper.get('[data-testid="claude-compatibility-refresh"]').trigger('click')
    await flushPromises()
    const result = wrapper.get('[data-testid="claude-compatibility-observation"]')
    expect(result.text()).toContain('2.1.330')
    expect(result.text()).toContain('1 → 1')
    expect(result.text()).toContain('claudeCompatibility.notForwarded')
    expect(result.text()).toContain('claudeCompatibility.unverified')
    expect(wrapper.emitted('update:modelValue')).toBeUndefined()
  })

  it('does not display server error payloads and permits retry', async () => {
    getObservation.mockRejectedValueOnce(new Error('private-token-and-device-id'))
      .mockResolvedValueOnce({ account_id: 1, attribution_mode: 'transparent', observation: null })
    const wrapper = mount(ClaudeOAuthCompatibilitySettings, { props: { accountId: 1 } })
    await flushPromises()
    expect(wrapper.get('[role="alert"]').text()).toContain('claudeCompatibility.loadFailed')
    expect(wrapper.text()).not.toContain('private-token')
    await wrapper.get('[data-testid="claude-compatibility-refresh"]').trigger('click')
    await flushPromises()
    expect(wrapper.find('[role="alert"]').exists()).toBe(false)
  })

  it.each([
    ['attribution_synthesized', 'reasonSynthesized'],
    ['wiggly_dropped_by_allowlist', 'reasonAllowlist'],
    ['opaque_metadata_unverified', 'reasonOpaqueUnverified'],
    ['noncanonical_attribution', 'reasonNoncanonical'],
    ['unknown_attribution_fields', 'reasonUnknownFields']
  ])('renders the observed service reason %s with its specific message', async (reason, translation) => {
    getObservation.mockResolvedValue({
      account_id: 1, attribution_mode: 'transparent',
      observation: { ...observation, incoming_attribution_blocks: reason === 'attribution_synthesized' ? 0 : 1, reasons: [reason] }
    })
    const wrapper = mount(ClaudeOAuthCompatibilitySettings, { props: { accountId: 1 } })
    await flushPromises()
    expect(wrapper.get('ul').text()).toBe('admin.accounts.claudeCompatibility.' + translation)
  })

  it('discards stale responses after switching accounts or closing', async () => {
    let finishOld!: (value: unknown) => void
    getObservation.mockImplementationOnce(() => new Promise((resolve) => { finishOld = resolve }))
      .mockResolvedValueOnce({ account_id: 2, attribution_mode: 'transparent', observation: null })
    const wrapper = mount(ClaudeOAuthCompatibilitySettings, { props: { accountId: 1 } })
    await wrapper.setProps({ accountId: 2 })
    await flushPromises()
    finishOld({ account_id: 1, attribution_mode: 'transparent', observation })
    await flushPromises()
    expect(wrapper.find('[data-testid="claude-compatibility-observation"]').exists()).toBe(false)
    await wrapper.setProps({ active: false })
    expect(getObservation).toHaveBeenCalledTimes(2)
    wrapper.unmount()
  })
})
