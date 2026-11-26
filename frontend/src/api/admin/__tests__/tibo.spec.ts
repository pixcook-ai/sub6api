import { beforeEach, describe, expect, it, vi } from 'vitest'
import { clearTiboIsolation, defaultTiboSettings, getTiboSettings, updateTiboSettings } from '../tibo'

const { get, put, post } = vi.hoisted(() => ({ get: vi.fn(), put: vi.fn(), post: vi.fn() }))
vi.mock('@/api/client', () => ({ apiClient: { get, put, post } }))

beforeEach(() => vi.resetAllMocks())

describe('Tibo admin API', () => {
  it('uses the separate admin settings endpoint with optional group IDs', async () => {
    const settings = defaultTiboSettings()
    get.mockResolvedValue({ data: settings })
    put.mockResolvedValue({ data: settings })
    expect(await getTiboSettings()).toEqual(settings)
    expect(await updateTiboSettings(settings)).toEqual(settings)
    expect(get).toHaveBeenCalledWith('/admin/tibo/settings')
    expect(put).toHaveBeenCalledWith('/admin/tibo/settings', settings)
    expect(settings.healthy_group_id).toBe(0)
    expect(settings.degraded_group_id).toBe(0)
  })

  it('clears only quality isolation and reads fresh account state without toggling scheduling', async () => {
    const account = { id: 17, schedulable: false, extra: { openai_tibo_quality_isolated: false } }
    post.mockResolvedValue({ data: { success: true } })
    get.mockResolvedValue({ data: account })
    expect(await clearTiboIsolation(17)).toEqual(account)
    expect(post).toHaveBeenCalledWith('/admin/accounts/17/tibo/clear-isolation')
    expect(get).toHaveBeenCalledWith('/admin/accounts/17')
    expect(put).not.toHaveBeenCalled()
  })
})
