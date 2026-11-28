import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import type { Account } from '@/types'
import TiboAccountHealthPanel from '../TiboAccountHealthPanel.vue'

const { clearIsolation } = vi.hoisted(() => ({ clearIsolation: vi.fn() }))
vi.mock('@/api/admin/tibo', () => ({ clearTiboIsolation: clearIsolation }))
vi.mock('vue-i18n', async importOriginal => ({ ...(await importOriginal<typeof import('vue-i18n')>()), useI18n: () => ({ t: (key: string) => key }) }))

const account = (overrides: Partial<Account> = {}): Account => ({
  id: 17, name: 'Paused account', platform: 'openai', type: 'oauth',
  proxy_id: null, concurrency: 1, priority: 1, status: 'active', error_message: null,
  last_used_at: null, expires_at: null, auto_pause_on_expired: true,
  created_at: '2026-11-20T00:00:00Z', updated_at: '2026-11-20T00:00:00Z',
  schedulable: false, rate_limited_at: null, rate_limit_reset_at: null,
  overload_until: null, temp_unschedulable_until: null, temp_unschedulable_reason: null,
  session_window_start: null, session_window_end: null, session_window_status: null,
  ...overrides
})

beforeEach(() => vi.resetAllMocks())

describe('account Tibo health', () => {
  it.each(['openai', 'openai_bps'] as const)('reads back %s after clearing isolation and preserves a manual scheduling pause', async (platform) => {
    const current = account({ platform, extra: { openai_tibo_quality_isolated: true } })
    const refreshed = account({ platform, extra: { openai_tibo_quality_isolated: false } })
    clearIsolation.mockResolvedValue(refreshed)
    const wrapper = mount(TiboAccountHealthPanel, { props: { account: current } })
    expect(wrapper.text()).toContain('admin.accounts.openai.tiboManualPauseRetained')
    await wrapper.get('[data-testid="tibo-clear-isolation"]').trigger('click')
    await flushPromises()
    expect(clearIsolation).toHaveBeenCalledWith(17)
    expect(wrapper.emitted('updated')?.[0]).toEqual([refreshed])
    expect(refreshed.schedulable).toBe(false)
    wrapper.unmount()
  })

  it('shows the BPS transport description and independent probe history', () => {
    const wrapper = mount(TiboAccountHealthPanel, { props: { account: account({
      platform: 'openai_bps',
      tibo_account_statuses: [{ model: 'gpt-6-astra', tibo_routes: [{ route: 'http', verdict: 'healthy', probes_hour: 1, flips_hour: 0, recent_samples: [{ verdict: 'healthy' }] }] }]
    }) } })
    expect(wrapper.text()).toContain('admin.accounts.openai.bpsTiboRouteDesc')
    expect(wrapper.text()).not.toContain('admin.accounts.openai.codexTiboRouteDesc')
    expect(wrapper.findAll('[data-testid="tibo-dot"]')).toHaveLength(24)
    expect(wrapper.findAll('[data-verdict="healthy"]')).toHaveLength(1)
    wrapper.unmount()
  })

  it('does not offer an action to clear accounts that are not isolated', () => {
    const wrapper = mount(TiboAccountHealthPanel, { props: { account: account() } })
    expect(wrapper.find('[data-testid="tibo-clear-isolation"]').exists()).toBe(false)
    expect(wrapper.text()).toContain('admin.accounts.openai.tiboNoProbeYet')
    wrapper.unmount()
  })

  it('updates definitive history while the account remains manually paused', async () => {
    const current = account({ tibo_account_statuses: [{ model: 'gpt-6-astra', tibo_routes: [{ route: 'http', verdict: 'degraded', probes_hour: 1, flips_hour: 1, recent_samples: [{ verdict: 'degraded' }] }] }] })
    const wrapper = mount(TiboAccountHealthPanel, { props: { account: current } })
    expect(wrapper.findAll('[data-testid="tibo-dot"]')).toHaveLength(24)
    expect(wrapper.findAll('[data-verdict="degraded"]')).toHaveLength(1)
    await wrapper.setProps({ account: account({ tibo_account_statuses: [{ model: 'gpt-6-astra', tibo_routes: [{ route: 'http', verdict: 'healthy', probes_hour: 2, flips_hour: 2, recent_samples: [{ verdict: 'degraded' }, { verdict: 'healthy' }] }] }] }) })
    expect(wrapper.findAll('[data-testid="tibo-dot"]')).toHaveLength(24)
    expect(wrapper.findAll('[data-verdict="healthy"]')).toHaveLength(1)
    expect(wrapper.props('account').schedulable).toBe(false)
    wrapper.unmount()
  })

  it('does not emit a previous account response after the edited account changes', async () => {
    let finish!: (account: Account) => void
    clearIsolation.mockReturnValue(new Promise<Account>(resolve => { finish = resolve }))
    const wrapper = mount(TiboAccountHealthPanel, { props: { account: account({ extra: { openai_tibo_quality_isolated: true } }) } })
    await wrapper.get('[data-testid="tibo-clear-isolation"]').trigger('click')
    await wrapper.setProps({ account: account({ id: 22 }) })
    finish(account())
    await flushPromises()
    expect(wrapper.emitted('updated')).toBeUndefined()
    wrapper.unmount()
  })
})
