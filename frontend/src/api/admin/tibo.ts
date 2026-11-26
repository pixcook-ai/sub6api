import { apiClient } from '../client'
import type { Account } from '@/types'

export interface TiboAutomationSettings {
  enabled: boolean
  auto_group_enabled: boolean
  healthy_group_id: number
  degraded_group_id: number
  pause_on_degraded: boolean
  auto_resume_on_healthy: boolean
}

export const defaultTiboSettings = (): TiboAutomationSettings => ({
  enabled: false,
  auto_group_enabled: false,
  healthy_group_id: 0,
  degraded_group_id: 0,
  pause_on_degraded: true,
  auto_resume_on_healthy: false
})

export async function getTiboSettings(): Promise<TiboAutomationSettings> {
  const { data } = await apiClient.get<TiboAutomationSettings>('/admin/tibo/settings')
  return data
}

export async function updateTiboSettings(settings: TiboAutomationSettings): Promise<TiboAutomationSettings> {
  const { data } = await apiClient.put<TiboAutomationSettings>('/admin/tibo/settings', settings)
  return data
}

export async function clearTiboIsolation(accountId: number): Promise<Account> {
  await apiClient.post('/admin/accounts/' + accountId + '/tibo/clear-isolation')
  const { data } = await apiClient.get<Account>('/admin/accounts/' + accountId)
  return data
}
