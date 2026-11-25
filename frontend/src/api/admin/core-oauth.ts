import { apiClient } from '../client'

export type CoreOAuthProvider = 'codex' | 'bps_excel'
export type CoreOAuthStatus =
  | 'empty'
  | 'authorizing'
  | 'authorized'
  | 'refreshing'
  | 'refresh_uncertain'
  | 'reauthorization_required'

export interface CoreOAuthAttempt {
  attempt_id: string
  provider: CoreOAuthProvider
  status: 'pending' | 'exchanging' | 'consumed' | 'canceled' | 'expired' | 'failed'
  expires_at: string
}

/** Safe server projection. Provider credentials never belong in frontend state. */
export interface CoreOAuthProfile {
  profile_ref: string
  provider: CoreOAuthProvider
  name: string
  status: CoreOAuthStatus
  authorized: boolean
  needs_authorization: boolean
  subject: string
  workspace_id: string
  email: string
  client_id: string
  identity_epoch: string
  token_revision: number
  expires_at: string | null
  attempt: CoreOAuthAttempt | null
  last_error_code: string
  created_at: string
  updated_at: string
}

export interface CoreOAuthCapabilities {
  available: boolean
  reason?: string
  providers: { provider: CoreOAuthProvider; available: boolean; requires_callback_url: true }[]
}

export interface CoreOAuthAuthorization {
  profile: CoreOAuthProfile
  attempt_id: string
  authorization_url: string
  redirect_uri: string
  expires_at: string
}

export const deploymentCapabilities = [
  'responses', 'chat_completions', 'embeddings', 'alpha_search', 'live', 'bps_files', 'bps_local_responses',
] as const
export type DeploymentCapability = typeof deploymentCapabilities[number]

export interface ManagedSwitchContext {
  requested_model: string
  required_capabilities: DeploymentCapability[]
}

export interface ManagedSwitchOption {
  provider: CoreOAuthProvider
  account_id: number
  profile_ref: string
  available: boolean
  reason: string
}

export interface ManagedSwitchState {
  deployment_id: string
  group_id: number
  route_revision: number
  selected_provider: CoreOAuthProvider
  applies_to: 'new_sessions'
  context_required: boolean
  requested_model: string
  required_capabilities: DeploymentCapability[]
  options: ManagedSwitchOption[]
}

export interface ManagedSwitchInput extends ManagedSwitchContext {
  expected_route_revision: number
  selected_provider: CoreOAuthProvider
  schedule_new_sessions: true
}

export interface CoreRuntimeInstallInput extends ManagedSwitchInput {
  group_id: number
  bps_profile_ref: string
  codex_profile_ref: string
  bps_account_id: number
  codex_account_id: number
  bps_backend_id: string
  bps_control_url: string
  bps_control_key: string
  bps_files_service_key: string
}

export interface CoreRuntimeShellInput {
  provider: CoreOAuthProvider
  name?: string
  group_id: number
  base_url?: string
  api_key?: string
}

export interface CoreRuntimeShell {
  id: number
  name: string
  platform: string
  type: string
  schedulable: boolean
  status: string
}

export interface CoreRuntimeShellSummary extends CoreRuntimeShell {
  provider: CoreOAuthProvider
}

const base = '/admin/core-oauth'
const profilePath = (ref: string) => `${base}/profiles/${encodeURIComponent(ref)}`
const deploymentPath = (id: string) => `/admin/managed-deployments/${encodeURIComponent(id)}`

export async function getCapabilities(): Promise<CoreOAuthCapabilities> {
  const { data } = await apiClient.get<CoreOAuthCapabilities>(`${base}/capabilities`)
  return data
}

export async function listProfiles(): Promise<CoreOAuthProfile[]> {
  const { data } = await apiClient.get<{ profiles: CoreOAuthProfile[] }>(`${base}/profiles`)
  return data.profiles
}

export async function getProfile(ref: string): Promise<CoreOAuthProfile> {
  const { data } = await apiClient.get<CoreOAuthProfile>(profilePath(ref))
  return data
}

export async function createProfile(input: { name: string; provider: CoreOAuthProvider; proxy_id?: number }): Promise<CoreOAuthProfile> {
  const { data } = await apiClient.post<CoreOAuthProfile>(`${base}/profiles`, input)
  return data
}

export async function createRuntimeShell(input: CoreRuntimeShellInput): Promise<CoreRuntimeShell> {
  const { data } = await apiClient.post<CoreRuntimeShell>(`${base}/runtime-shells`, input)
  return data
}

export async function listRuntimeShells(groupID: number): Promise<CoreRuntimeShellSummary[]> {
  const { data } = await apiClient.get<{ accounts: CoreRuntimeShellSummary[] }>(`${base}/runtime-shells`, { params: { group_id: groupID } })
  return data.accounts
}

export async function startAuthorization(ref: string): Promise<CoreOAuthAuthorization> {
  const { data } = await apiClient.post<CoreOAuthAuthorization>(`${profilePath(ref)}/authorize`, {})
  return data
}

export async function completeAuthorization(ref: string, input: { attempt_id: string; callback_url: string }): Promise<CoreOAuthProfile> {
  const { data } = await apiClient.post<CoreOAuthProfile>(`${profilePath(ref)}/complete`, input)
  return data
}

export async function cancelAuthorization(ref: string, attemptID: string): Promise<CoreOAuthProfile> {
  const { data } = await apiClient.post<CoreOAuthProfile>(`${profilePath(ref)}/cancel`, { attempt_id: attemptID })
  return data
}

export async function refreshProfile(ref: string, expectedRevision: number): Promise<CoreOAuthProfile> {
  const { data } = await apiClient.post<CoreOAuthProfile>(`${profilePath(ref)}/refresh`, { expected_revision: expectedRevision })
  return data
}

export async function getSwitchOptions(id: string, context: ManagedSwitchContext): Promise<ManagedSwitchState> {
  const { data } = await apiClient.get<ManagedSwitchState>(`${deploymentPath(id)}/switch-options`, {
    params: {
      requested_model: context.requested_model,
      required_capabilities: context.required_capabilities.join(','),
    },
  })
  return data
}

export async function switchProvider(id: string, input: ManagedSwitchInput): Promise<ManagedSwitchState> {
  const { data } = await apiClient.post<ManagedSwitchState>(`${deploymentPath(id)}/switch`, input)
  return data
}

export async function installCoreRuntime(id: string, input: CoreRuntimeInstallInput): Promise<ManagedSwitchState> {
  const { data } = await apiClient.post<ManagedSwitchState>(`${deploymentPath(id)}/core-runtime`, input)
  return data
}

export default {
  getCapabilities, listProfiles, getProfile, createProfile, createRuntimeShell, listRuntimeShells, startAuthorization, completeAuthorization,
  cancelAuthorization, refreshProfile, getSwitchOptions, switchProvider, installCoreRuntime,
}
