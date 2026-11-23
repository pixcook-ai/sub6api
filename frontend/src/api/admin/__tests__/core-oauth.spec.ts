import { beforeEach, describe, expect, it, vi } from 'vitest'
import coreOAuthAPI from '../core-oauth'

const { get, post } = vi.hoisted(() => ({ get: vi.fn(), post: vi.fn() }))
vi.mock('../../client', () => ({ apiClient: { get, post } }))

describe('核心 OAuth API 安全协议', () => {
  beforeEach(() => {
    vi.resetAllMocks()
    get.mockResolvedValue({ data: {} })
    post.mockResolvedValue({ data: {} })
  })

  it('读取能力与 profiles envelope，不转发管理凭据', async () => {
    const capabilities = { available: false, providers: [] }
    get.mockResolvedValueOnce({ data: capabilities }).mockResolvedValueOnce({ data: { profiles: [{ profile_ref: 'profile-1' }] } })
    expect(await coreOAuthAPI.getCapabilities()).toEqual(capabilities)
    expect(await coreOAuthAPI.listProfiles()).toEqual([{ profile_ref: 'profile-1' }])
    expect(get.mock.calls).toEqual([['/admin/core-oauth/capabilities'], ['/admin/core-oauth/profiles']])
  })

  it('按编码后的 profile ref 读取状态', async () => {
    const profile = { profile_ref: 'profile/id' }
    get.mockResolvedValue({ data: profile })
    expect(await coreOAuthAPI.getProfile('profile/id')).toEqual(profile)
    expect(get).toHaveBeenCalledWith('/admin/core-oauth/profiles/profile%2Fid')
  })

  it('新建独立 provider 档案，再启动服务器持有的 PKCE 事务', async () => {
    const profile = { profile_ref: 'profile-1', provider: 'bps_excel' }
    post.mockResolvedValueOnce({ data: profile }).mockResolvedValueOnce({ data: { attempt_id: 'attempt-1' } })
    expect(await coreOAuthAPI.createProfile({ name: 'Excel 授权', provider: 'bps_excel' })).toEqual(profile)
    expect(await coreOAuthAPI.startAuthorization('profile-1')).toEqual({ attempt_id: 'attempt-1' })
    expect(post.mock.calls).toEqual([
      ['/admin/core-oauth/profiles', { name: 'Excel 授权', provider: 'bps_excel' }],
      ['/admin/core-oauth/profiles/profile-1/authorize', {}],
    ])
  })

  it('仅提交本次 attempt_id 和完整 callback_url', async () => {
    const callback = 'https://bps.openai.com/basispoints/extension/360590d7-f8f9-4d88-bf75-0edfe0a4b9f3/auth/callback?code=fixture&state=fixture'
    await coreOAuthAPI.completeAuthorization('profile-1', { attempt_id: 'attempt-1', callback_url: callback })
    expect(post).toHaveBeenCalledWith('/admin/core-oauth/profiles/profile-1/complete', { attempt_id: 'attempt-1', callback_url: callback })
  })

  it('取消携带 attempt，刷新携带所见 revision', async () => {
    await coreOAuthAPI.cancelAuthorization('profile-1', 'attempt-1')
    await coreOAuthAPI.refreshProfile('profile-1', 23)
    expect(post.mock.calls).toEqual([
      ['/admin/core-oauth/profiles/profile-1/cancel', { attempt_id: 'attempt-1' }],
      ['/admin/core-oauth/profiles/profile-1/refresh', { expected_revision: 23 }],
    ])
  })

  it('预检采用显式模型和逗号分隔能力，不发明默认值', async () => {
    await coreOAuthAPI.getSwitchOptions('deployment/id', { requested_model: 'operator-model', required_capabilities: ['responses', 'bps_files'] })
    expect(get).toHaveBeenCalledWith('/admin/managed-deployments/deployment%2Fid/switch-options', {
      params: { requested_model: 'operator-model', required_capabilities: 'responses,bps_files' },
    })
  })

  it('切换只提交 CAS 与明确新会话调度意图', async () => {
    await coreOAuthAPI.switchProvider('deployment-1', {
      expected_route_revision: 4, selected_provider: 'codex', requested_model: 'operator-model',
      required_capabilities: ['responses'], schedule_new_sessions: true,
    })
    expect(post).toHaveBeenCalledWith('/admin/managed-deployments/deployment-1/switch', {
      expected_route_revision: 4, selected_provider: 'codex', requested_model: 'operator-model',
      required_capabilities: ['responses'], schedule_new_sessions: true,
    })
  })

  it('显式安装核心运行时，不转发 provider refresh token', async () => {
    const input = {
      group_id: 1, bps_profile_ref: 'excel-profile', codex_profile_ref: 'codex-profile',
      bps_account_id: 11, codex_account_id: 12, bps_backend_id: 'worker-1',
      bps_control_url: 'https://worker.example.invalid', bps_control_key: 'fixture-independent-control-key-1234567890',
      expected_route_revision: 0, selected_provider: 'bps_excel' as const, requested_model: 'operator-model',
      required_capabilities: ['responses' as const], schedule_new_sessions: true as const,
    }
    await coreOAuthAPI.installCoreRuntime('deployment-1', input)
    expect(post).toHaveBeenCalledWith('/admin/managed-deployments/deployment-1/core-runtime', input)
  })
})
