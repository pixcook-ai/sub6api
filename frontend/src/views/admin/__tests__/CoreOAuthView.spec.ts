import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import CoreOAuthView from '../CoreOAuthView.vue'
import type { CoreOAuthProfile, ManagedSwitchState } from '@/api/admin/core-oauth'

const { api, stepUpRun } = vi.hoisted(() => ({
  api: {
    getCapabilities: vi.fn(), getProfile: vi.fn(), createProfile: vi.fn(), createRuntimeShell: vi.fn(), listProfiles: vi.fn(), startAuthorization: vi.fn(), completeAuthorization: vi.fn(),
    cancelAuthorization: vi.fn(), refreshProfile: vi.fn(), getSwitchOptions: vi.fn(), switchProvider: vi.fn(), installCoreRuntime: vi.fn(),
  },
  stepUpRun: vi.fn((action: () => Promise<unknown>) => action()),
}))
vi.mock('@/api/admin/core-oauth', () => ({ default: api }))
vi.mock('@/composables/useStepUp', () => ({
  useStepUp: () => ({ run: stepUpRun, onCancel: vi.fn() }), isStepUpCancelled: () => false,
}))
enableAutoUnmount(afterEach)

function mountView() {
  return mount(CoreOAuthView, { global: { stubs: {
    AppLayout: { template: '<div><slot /></div>' },
    TotpStepUpDialog: true,
  } } })
}

function profile(overrides: Partial<CoreOAuthProfile> = {}): CoreOAuthProfile {
  return {
    profile_ref: 'codex-profile', provider: 'codex', name: 'Codex 核心授权', status: 'empty',
    authorized: false, needs_authorization: true, subject: '', workspace_id: '', email: '',
    client_id: '', identity_epoch: '', token_revision: 0, expires_at: null, attempt: null,
    last_error_code: '', created_at: '', updated_at: '', ...overrides,
  }
}
const attempt = { attempt_id: 'attempt-current', provider: 'codex' as const, status: 'pending' as const, expires_at: '2099-01-01T00:00:00Z' }
const callbackURL = 'http://localhost:1455/auth/callback?code=private-test-code&state=test-state'
function switchState(overrides: Partial<ManagedSwitchState> = {}): ManagedSwitchState {
  return {
    deployment_id: 'deployment-1', group_id: 1, route_revision: 7, selected_provider: 'codex',
    applies_to: 'new_sessions', context_required: false, requested_model: 'operator-model',
    required_capabilities: ['responses'], options: [
      { provider: 'codex', account_id: 1, profile_ref: 'codex-profile', available: true, reason: '' },
      { provider: 'bps_excel', account_id: 2, profile_ref: 'excel-profile', available: true, reason: '' },
    ], ...overrides,
  }
}
async function previewSwitch(wrapper: ReturnType<typeof mountView>) {
  await wrapper.get('[data-field="deployment-id"]').setValue('deployment-1')
  await wrapper.get('[data-field="requested-model"]').setValue('operator-model')
  await wrapper.get('input[value="responses"]').setValue(true)
  await wrapper.get('form').trigger('submit')
  await flushPromises()
}
function authorizedProfiles() {
  return [
    profile({ status: 'authorized', authorized: true }),
    profile({ provider: 'bps_excel', profile_ref: 'excel-profile', name: 'Excel 授权', status: 'authorized', authorized: true }),
  ]
}
async function prepareInstall(wrapper: ReturnType<typeof mountView>) {
  await wrapper.get('[data-field="deployment-id"]').setValue('deployment-1')
  await wrapper.get('[data-field="requested-model"]').setValue('operator-model')
  await wrapper.get('input[value="responses"]').setValue(true)
  await wrapper.get('[data-action="show-install"]').trigger('click')
  await wrapper.get('[data-field="install-group"]').setValue('1')
  await wrapper.get('[data-field="install-bps-base-url"]').setValue('https://worker.example.invalid/v1')
  await wrapper.get('[data-field="install-bps-api-key"]').setValue('inference-key-test-only')
  await wrapper.get('[data-action="create-runtime-shells"]').trigger('click')
  await flushPromises()
  await wrapper.get('[data-field="install-backend"]').setValue('worker-1')
  await wrapper.get('[data-field="install-control-url"]').setValue('https://worker.example.invalid')
  await wrapper.get('[data-field="install-provider"]').setValue('bps_excel')
  await wrapper.get('[data-field="install-confirm"]').setValue(true)
}

describe('核心双 OAuth 授权管理', () => {
  beforeEach(() => {
    vi.resetAllMocks()
    stepUpRun.mockImplementation((action: () => Promise<unknown>) => action())
    api.listProfiles.mockResolvedValue([])
    api.getCapabilities.mockResolvedValue({ available: true, providers: [
      { provider: 'codex', available: true, requires_callback_url: true },
      { provider: 'bps_excel', available: true, requires_callback_url: true },
    ] })
    api.createProfile.mockResolvedValue(profile())
    api.createRuntimeShell.mockImplementation(async ({ provider }: { provider: string }) => ({
      id: provider === 'bps_excel' ? 11 : 12,
      name: `${provider} runtime shell`, platform: 'openai', type: provider === 'bps_excel' ? 'apikey' : 'oauth',
      status: 'active', schedulable: false,
    }))
    api.getProfile.mockResolvedValue(profile())
    api.startAuthorization.mockResolvedValue({
      profile: profile({ status: 'authorizing', attempt }), attempt_id: attempt.attempt_id,
      authorization_url: 'https://auth.openai.com/oauth/authorize?state=fixture',
      redirect_uri: 'http://localhost:1455/auth/callback', expires_at: attempt.expires_at,
    })
    api.completeAuthorization.mockResolvedValue(profile({ status: 'authorized', authorized: true, needs_authorization: false, token_revision: 1 }))
    api.cancelAuthorization.mockResolvedValue(profile())
    api.refreshProfile.mockResolvedValue(profile({ status: 'authorized', authorized: true, needs_authorization: false, token_revision: 8 }))
    api.getSwitchOptions.mockResolvedValue(switchState())
    api.switchProvider.mockResolvedValue(switchState({ selected_provider: 'bps_excel', route_revision: 8 }))
    api.installCoreRuntime.mockResolvedValue(switchState({ selected_provider: 'bps_excel', route_revision: 1 }))
  })

  it('提供 Codex 与 Excel 两份独立授权，并明确只影响新会话', async () => {
    const wrapper = mountView()
    await flushPromises()
    expect(wrapper.get('[data-provider="codex"]').text()).toContain('Codex')
    expect(wrapper.get('[data-provider="bps_excel"]').text()).toContain('Excel')
    expect(wrapper.findAll('[data-action="authorize"]')).toHaveLength(2)
    expect(wrapper.text()).toContain('新会话')
    expect(wrapper.find('iframe').exists()).toBe(false)
    expect(wrapper.find('input[type="file"]').exists()).toBe(false)
  })

  it('为所选 provider 创建档案并展示官方授权链接', async () => {
    const wrapper = mountView()
    await flushPromises()
    await wrapper.get('[data-provider="codex"] [data-action="authorize"]').trigger('click')
    await flushPromises()
    expect(api.createProfile).toHaveBeenCalledWith({ provider: 'codex', name: 'Codex 核心授权' })
    expect(api.startAuthorization).toHaveBeenCalledWith('codex-profile')
    expect(stepUpRun).toHaveBeenCalled()
    const link = wrapper.get('[data-provider="codex"] [data-action="official-login"]')
    expect(link.attributes('href')).toBe('https://auth.openai.com/oauth/authorize?state=fixture')
    expect(link.attributes('rel')).toContain('noopener')
    expect(wrapper.get('[data-provider="bps_excel"]').find('textarea').exists()).toBe(false)
  })

  it('将完整回调 URL 与本次事务发送，成功后清除敏感输入', async () => {
    api.listProfiles.mockResolvedValue([profile({ status: 'authorizing', attempt })])
    const wrapper = mountView()
    await flushPromises()
    await wrapper.get('[data-provider="codex"] textarea').setValue(callbackURL)
    await wrapper.get('[data-provider="codex"] form').trigger('submit')
    await flushPromises()
    expect(api.completeAuthorization).toHaveBeenCalledWith('codex-profile', { attempt_id: 'attempt-current', callback_url: callbackURL })
    expect(wrapper.find('textarea').exists()).toBe(false)
    expect(wrapper.text()).toContain('已授权')
    expect(wrapper.html()).not.toContain('private-test-code')
  })

  it('失败后清空回调且仅显示安全错误，不回显上游消息', async () => {
    api.listProfiles.mockResolvedValue([profile({ status: 'authorizing', attempt })])
    api.getProfile.mockResolvedValue(profile({ status: 'authorizing', attempt }))
    api.completeAuthorization.mockRejectedValue({ status: 400, code: 'CORE_OAUTH_PROVIDER_REJECTED', message: 'private-test-code secret-access-token' })
    const wrapper = mountView()
    await flushPromises()
    await wrapper.get('textarea').setValue(callbackURL)
    await wrapper.get('[data-provider="codex"] form').trigger('submit')
    await flushPromises()
    expect((wrapper.get('textarea').element as HTMLTextAreaElement).value).toBe('')
    expect(wrapper.text()).toContain('官方授权未通过')
    expect(wrapper.html()).not.toContain('secret-access-token')
    expect(wrapper.html()).not.toContain('private-test-code')
  })

  it('取消当前事务且不影响另一份档案', async () => {
    api.listProfiles.mockResolvedValue([profile({ status: 'authorizing', attempt }), profile({ profile_ref: 'excel-profile', provider: 'bps_excel', name: 'Excel 核心授权', status: 'authorized', authorized: true })])
    const wrapper = mountView()
    await flushPromises()
    await wrapper.get('textarea').setValue(callbackURL)
    await wrapper.get('[data-action="cancel"]').trigger('click')
    await flushPromises()
    expect(api.cancelAuthorization).toHaveBeenCalledWith('codex-profile', 'attempt-current')
    expect(wrapper.find('textarea').exists()).toBe(false)
    expect(wrapper.get('[data-provider="bps_excel"]').text()).toContain('已授权')
  })

  it('过期事务不能提交，仍可取消后重新发起', async () => {
    api.listProfiles.mockResolvedValue([profile({ status: 'authorizing', attempt: { ...attempt, expires_at: '2000-01-01T00:00:00Z' } })])
    const wrapper = mountView()
    await flushPromises()
    await wrapper.get('textarea').setValue(callbackURL)
    expect(wrapper.get('[data-action="complete"]').attributes('disabled')).toBeDefined()
    expect(wrapper.text()).toContain('已过期')
    expect(api.completeAuthorization).not.toHaveBeenCalled()
  })

  it('使用所选档案 revision 刷新，需重新授权状态有明确提示', async () => {
    api.listProfiles.mockResolvedValue([profile({ status: 'authorized', authorized: true, needs_authorization: false, token_revision: 7 })])
    const wrapper = mountView()
    await flushPromises()
    await wrapper.get('[data-action="refresh"]').trigger('click')
    await flushPromises()
    expect(api.refreshProfile).toHaveBeenCalledWith('codex-profile', 7)
    api.listProfiles.mockResolvedValue([profile({ status: 'reauthorization_required', needs_authorization: true })])
    await wrapper.get('[data-action="reload"]').trigger('click')
    await flushPromises()
    expect(wrapper.get('[data-provider="codex"]').text()).toContain('需重新授权')
  })

  it('拒绝非官方登录地址，能力不可用时不创建档案', async () => {
    api.startAuthorization.mockResolvedValue({
      profile: profile({ status: 'authorizing', attempt }), attempt_id: attempt.attempt_id,
      authorization_url: 'https://evil.example/oauth/authorize', redirect_uri: '', expires_at: attempt.expires_at,
    })
    const wrapper = mountView()
    await flushPromises()
    await wrapper.get('[data-provider="codex"] [data-action="authorize"]').trigger('click')
    await flushPromises()
    expect(wrapper.find('[data-action="official-login"]').exists()).toBe(false)
    expect(wrapper.text()).toContain('官方登录地址校验失败')
    wrapper.unmount()
    api.createProfile.mockClear()
    api.getCapabilities.mockResolvedValue({ available: false, reason: 'private configuration', providers: [] })
    const unavailable = mountView()
    await flushPromises()
    expect(unavailable.get('[data-provider="codex"] [data-action="authorize"]').attributes('disabled')).toBeDefined()
    expect(unavailable.text()).toContain('暂不可用')
    expect(unavailable.text()).not.toContain('private configuration')
    expect(api.createProfile).not.toHaveBeenCalled()
  })

  it('没有显式模型和能力时不能预检或切换', async () => {
    const wrapper = mountView()
    await flushPromises()
    await wrapper.get('[data-field="deployment-id"]').setValue('deployment-1')
    expect(wrapper.get('[data-action="preview-switch"]').attributes('disabled')).toBeDefined()
    expect(api.getSwitchOptions).not.toHaveBeenCalled()
    expect(api.switchProvider).not.toHaveBeenCalled()
  })

  it('预检后明确调度新会话并提交 CAS，不携带绑定或凭据', async () => {
    const wrapper = mountView()
    await flushPromises()
    await previewSwitch(wrapper)
    expect(api.getSwitchOptions).toHaveBeenCalledWith('deployment-1', { requested_model: 'operator-model', required_capabilities: ['responses'] })
    expect(wrapper.get('[data-switch-provider="bps_excel"]').attributes('disabled')).toBeDefined()
    await wrapper.get('[data-field="schedule-new-sessions"]').setValue(true)
    await wrapper.get('[data-switch-provider="bps_excel"]').trigger('click')
    await flushPromises()
    expect(api.switchProvider).toHaveBeenCalledWith('deployment-1', {
      expected_route_revision: 7, selected_provider: 'bps_excel', requested_model: 'operator-model',
      required_capabilities: ['responses'], schedule_new_sessions: true,
    })
    expect(wrapper.text()).toContain('新会话已选择 Excel')
    expect(wrapper.text()).toContain('旧会话')
  })

  it('目标不可用时显示安全原因并拒绝切换', async () => {
    const state = switchState()
    state.options[1] = { ...state.options[1]!, available: false, reason: 'scheduling_paused' }
    api.getSwitchOptions.mockResolvedValue(state)
    const wrapper = mountView()
    await flushPromises()
    await previewSwitch(wrapper)
    await wrapper.get('[data-field="schedule-new-sessions"]').setValue(true)
    expect(wrapper.get('[data-switch-provider="bps_excel"]').attributes('disabled')).toBeDefined()
    expect(wrapper.text()).toContain('已被人工停调')
    expect(api.switchProvider).not.toHaveBeenCalled()
  })

  it('CAS 冲突重读路由但不自动重试切换', async () => {
    api.switchProvider.mockRejectedValue({ status: 409, code: 'CONFLICT', reason: 'MANAGED_DEPLOYMENT_CONFLICT', message: 'private backend details' })
    api.getSwitchOptions.mockResolvedValueOnce(switchState()).mockResolvedValueOnce(switchState({ route_revision: 9 }))
    const wrapper = mountView()
    await flushPromises()
    await previewSwitch(wrapper)
    await wrapper.get('[data-field="schedule-new-sessions"]').setValue(true)
    await wrapper.get('[data-switch-provider="bps_excel"]').trigger('click')
    await flushPromises()
    expect(api.switchProvider).toHaveBeenCalledTimes(1)
    expect(api.getSwitchOptions).toHaveBeenCalledTimes(2)
    expect(wrapper.text()).toContain('其他管理员')
    expect(wrapper.text()).not.toContain('private backend details')
    expect(wrapper.get('[data-switch-provider="bps_excel"]').attributes('disabled')).toBeDefined()
  })

  it('修改预检上下文后旧选项立即失效', async () => {
    const wrapper = mountView()
    await flushPromises()
    await previewSwitch(wrapper)
    await wrapper.get('[data-field="requested-model"]').setValue('another-model')
    expect(wrapper.find('[data-switch-provider]').exists()).toBe(false)
    expect(api.switchProvider).not.toHaveBeenCalled()
  })

  it('刷新失败后重读安全状态，结果未知时不能继续刷新旧凭据', async () => {
    api.listProfiles.mockResolvedValue([profile({ status: 'authorized', authorized: true, needs_authorization: false, token_revision: 7 })])
    api.refreshProfile.mockRejectedValue({ code: 'CORE_OAUTH_REFRESH_UNCERTAIN' })
    api.getProfile.mockResolvedValue(profile({ status: 'refresh_uncertain', needs_authorization: true, token_revision: 7 }))
    const wrapper = mountView()
    await flushPromises()
    await wrapper.get('[data-action="refresh"]').trigger('click')
    await flushPromises()
    expect(api.getProfile).toHaveBeenCalledWith('codex-profile')
    expect(wrapper.find('[data-action="refresh"]').exists()).toBe(false)
    expect(wrapper.text()).toContain('已停止重试旧凭据')
    expect(api.refreshProfile).toHaveBeenCalledTimes(1)
  })

  it('另一提供方操作期间拒绝并发变更，避免共享二次验证事务竞争', async () => {
    let release: ((value: CoreOAuthProfile) => void) | undefined
    api.createProfile.mockReturnValue(new Promise<CoreOAuthProfile>((resolve) => { release = resolve }))
    const wrapper = mountView()
    await flushPromises()
    await wrapper.get('[data-provider="codex"] [data-action="authorize"]').trigger('click')
    const excelButton = wrapper.get('[data-provider="bps_excel"] [data-action="authorize"]')
    expect(excelButton.attributes('disabled')).toBeDefined()
    await excelButton.trigger('click')
    expect(api.createProfile).toHaveBeenCalledTimes(1)
    release?.(profile())
    await flushPromises()
  })

  it('Excel 使用独立协议路径和本次完整官方回调', async () => {
    const excelAttempt = { ...attempt, provider: 'bps_excel' as const, attempt_id: 'excel-attempt' }
    const excel = profile({ provider: 'bps_excel', profile_ref: 'excel-profile', name: 'Excel 核心授权' })
    api.createProfile.mockResolvedValue(excel)
    api.startAuthorization.mockResolvedValue({
      profile: { ...excel, status: 'authorizing', attempt: excelAttempt }, attempt_id: 'excel-attempt',
      authorization_url: 'https://auth.openai.com/api/accounts/authorize?state=excel-fixture',
      redirect_uri: 'https://bps.openai.com/basispoints/extension/360590d7-f8f9-4d88-bf75-0edfe0a4b9f3/auth/callback', expires_at: excelAttempt.expires_at,
    })
    api.completeAuthorization.mockResolvedValue({ ...excel, status: 'authorized', authorized: true, needs_authorization: false })
    const wrapper = mountView()
    await flushPromises()
    await wrapper.get('[data-provider="bps_excel"] [data-action="authorize"]').trigger('click')
    await flushPromises()
    expect(api.createProfile).toHaveBeenCalledWith({ provider: 'bps_excel', name: 'Excel 核心授权' })
    expect(wrapper.get('[data-action="official-login"]').attributes('href')).toContain('/api/accounts/authorize')
    const url = 'https://bps.openai.com/basispoints/extension/360590d7-f8f9-4d88-bf75-0edfe0a4b9f3/auth/callback?code=excel-private-test&state=excel-fixture'
    await wrapper.get('textarea').setValue(url)
    await wrapper.get('[data-provider="bps_excel"] form').trigger('submit')
    await flushPromises()
    expect(api.completeAuthorization).toHaveBeenCalledWith('excel-profile', { attempt_id: 'excel-attempt', callback_url: url })
    expect(wrapper.html()).not.toContain('excel-private-test')
  })

  it('支持反向选择 Codex 且继续使用读取到的 route revision', async () => {
    api.getSwitchOptions.mockResolvedValue(switchState({ selected_provider: 'bps_excel', route_revision: 11 }))
    api.switchProvider.mockResolvedValue(switchState({ selected_provider: 'codex', route_revision: 12 }))
    const wrapper = mountView()
    await flushPromises()
    await previewSwitch(wrapper)
    await wrapper.get('[data-field="schedule-new-sessions"]').setValue(true)
    await wrapper.get('[data-switch-provider="codex"]').trigger('click')
    await flushPromises()
    expect(api.switchProvider).toHaveBeenCalledWith('deployment-1', expect.objectContaining({ selected_provider: 'codex', expected_route_revision: 11 }))
    expect(wrapper.text()).toContain('新会话已选择 Codex')
  })

  it('首次安装需要两份有效授权、显式空账号与独立 control key，并在提交后清空 key', async () => {
    api.listProfiles.mockResolvedValue(authorizedProfiles())
    const wrapper = mountView()
    await flushPromises()
    await prepareInstall(wrapper)
    expect(api.createRuntimeShell).toHaveBeenNthCalledWith(1, { provider: 'bps_excel', group_id: 1, base_url: 'https://worker.example.invalid/v1', api_key: 'inference-key-test-only' })
    expect(api.createRuntimeShell).toHaveBeenNthCalledWith(2, { provider: 'codex', group_id: 1 })
    const key = 'fixture-independent-control-key-1234567890'
    const keyInput = wrapper.get('[data-field="install-control-key"]')
    expect(keyInput.attributes('type')).toBe('password')
    await keyInput.setValue('short')
    await wrapper.get('[data-field="install-provider"]').setValue('bps_excel')
    await wrapper.get('[data-field="install-confirm"]').setValue(true)
    expect(wrapper.get('[data-action="install-runtime"]').attributes('disabled')).toBeDefined()
    await keyInput.setValue(key)
    expect(wrapper.get('[data-action="install-runtime"]').attributes('disabled')).toBeUndefined()
    await wrapper.get('[data-form="install"]').trigger('submit')
    await flushPromises()
    expect(api.installCoreRuntime).toHaveBeenCalledWith('deployment-1', {
      group_id: 1, bps_profile_ref: 'excel-profile', codex_profile_ref: 'codex-profile',
      bps_account_id: 11, codex_account_id: 12, bps_backend_id: 'worker-1',
      bps_control_url: 'https://worker.example.invalid', bps_control_key: key,
      expected_route_revision: 0, selected_provider: 'bps_excel', requested_model: 'operator-model',
      required_capabilities: ['responses'], schedule_new_sessions: true,
    })
    expect((keyInput.element as HTMLInputElement).value).toBe('')
    expect(wrapper.html()).not.toContain(key)
    expect(wrapper.text()).toContain('核心运行时已安装')
  })

  it('首次安装缺少双授权时拒绝操作，不创建账号或隐式接管', async () => {
    const wrapper = mountView()
    await flushPromises()
    await wrapper.get('[data-action="show-install"]').trigger('click')
    expect(wrapper.text()).toContain('两份有效授权')
    expect(wrapper.get('[data-action="install-runtime"]').attributes('disabled')).toBeDefined()
    expect(api.installCoreRuntime).not.toHaveBeenCalled()
  })

  it('创建运行账号失败会清空 inference API key，不回显或继续创建 Codex 账号', async () => {
    api.listProfiles.mockResolvedValue(authorizedProfiles())
    api.createRuntimeShell.mockRejectedValueOnce({ code: 'CORE_OAUTH_INVALID_INPUT', message: 'private inference key' })
    const wrapper = mountView()
    await flushPromises()
    await wrapper.get('[data-action="show-install"]').trigger('click')
    await wrapper.get('[data-field="install-group"]').setValue('1')
    await wrapper.get('[data-field="install-bps-base-url"]').setValue('https://worker.example.invalid/v1')
    await wrapper.get('[data-field="install-bps-api-key"]').setValue('private-inference-key')
    await wrapper.get('[data-action="create-runtime-shells"]').trigger('click')
    await flushPromises()
    expect(api.createRuntimeShell).toHaveBeenCalledTimes(1)
    expect((wrapper.get('[data-field="install-bps-api-key"]').element as HTMLInputElement).value).toBe('')
    expect(wrapper.html()).not.toContain('private-inference-key')
    expect(wrapper.html()).not.toContain('private inference key')
  })

  it('安装失败同样清空 control key，不回显错误中的凭据或自动重试', async () => {
    api.listProfiles.mockResolvedValue(authorizedProfiles())
    const key = 'fixture-independent-control-key-1234567890'
    api.installCoreRuntime.mockRejectedValue({ code: 'MANAGED_DEPLOYMENT_UNAVAILABLE', message: key })
    const wrapper = mountView()
    await flushPromises()
    await prepareInstall(wrapper)
    await wrapper.get('[data-field="install-control-key"]').setValue(key)
    await wrapper.get('[data-form="install"]').trigger('submit')
    await flushPromises()
    expect(api.installCoreRuntime).toHaveBeenCalledTimes(1)
    expect((wrapper.get('[data-field="install-control-key"]').element as HTMLInputElement).value).toBe('')
    expect(wrapper.html()).not.toContain(key)
    expect(wrapper.text()).toContain('目标提供方暂不可用')
    expect(wrapper.get('[data-action="install-runtime"]').attributes('disabled')).toBeDefined()
  })

  it('修复已有运行时使用预检 revision，允许同时留空控制配置以复用服务端配置', async () => {
    api.listProfiles.mockResolvedValue(authorizedProfiles())
    const wrapper = mountView()
    await flushPromises()
    await previewSwitch(wrapper)
    await prepareInstall(wrapper)
    await wrapper.get('[data-field="install-control-url"]').setValue('')
    expect(wrapper.get('[data-action="install-runtime"]').attributes('disabled')).toBeUndefined()
    await wrapper.get('[data-form="install"]').trigger('submit')
    await flushPromises()
    expect(api.installCoreRuntime).toHaveBeenCalledWith('deployment-1', expect.objectContaining({ expected_route_revision: 7, bps_control_url: '', bps_control_key: '' }))
  })
})
