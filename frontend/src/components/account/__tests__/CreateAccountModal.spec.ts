import { defineComponent } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

const {
  createAccountMock,
  probeUpstreamBillingMock,
  syncUpstreamModelsMock,
  showWarningMock,
  importCodexSessionMock,
  createOpenAICodexPATMock,
  generateAuthUrlMock,
  exchangeCodeMock,
  authIsSimpleMode,
} = vi.hoisted(() => ({
  createAccountMock: vi.fn(),
  probeUpstreamBillingMock: vi.fn(),
  syncUpstreamModelsMock: vi.fn(),
  showWarningMock: vi.fn(),
  importCodexSessionMock: vi.fn(),
  createOpenAICodexPATMock: vi.fn(),
  generateAuthUrlMock: vi.fn(),
  exchangeCodeMock: vi.fn(),
  authIsSimpleMode: { value: true },
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({
    showError: vi.fn(),
    showSuccess: vi.fn(),
    showWarning: showWarningMock,
  }),
}))

vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({
    get isSimpleMode() {
      return authIsSimpleMode.value
    },
  }),
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    accounts: {
      create: createAccountMock,
      probeUpstreamBilling: probeUpstreamBillingMock,
      syncUpstreamModels: syncUpstreamModelsMock,
      checkMixedChannelRisk: vi.fn().mockResolvedValue({ has_risk: false }),
      importCodexSession: importCodexSessionMock,
      createOpenAICodexPAT: createOpenAICodexPATMock,
      generateAuthUrl: generateAuthUrlMock,
      exchangeCode: exchangeCodeMock,
    },
    proxies: { getProxyAccounts: vi.fn().mockResolvedValue([]), testProxy: vi.fn().mockResolvedValue({ success: true, message: 'ok' }) },
    settings: {
      getWebSearchEmulationConfig: vi.fn().mockResolvedValue({ enabled: false, providers: [] }),
      getSettings: vi.fn().mockResolvedValue({}),
    },
    tlsFingerprintProfiles: {
      list: vi.fn().mockResolvedValue([]),
    },
    groups: {
      getAll: vi.fn().mockResolvedValue([]),
    },
  },
}))

vi.mock('@/api/admin/accounts', () => ({
  getAntigravityDefaultModelMapping: vi.fn().mockResolvedValue([]),
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({ t: (key: string) => key }),
  }
})

import CreateAccountModal from '../CreateAccountModal.vue'
import OAuthAuthorizationFlow from '../OAuthAuthorizationFlow.vue'

const BaseDialogStub = defineComponent({
  name: 'BaseDialog',
  props: { show: { type: Boolean, default: false } },
  template: '<div v-if="show"><slot /><slot name="footer" /></div>',
})

const OAuthAuthorizationFlowStub = defineComponent({
  name: 'OAuthAuthorizationFlow',
  props: {
    showManualOption: Boolean,
    showCodexSessionImportOption: Boolean,
    showAgentIdentityOption: Boolean,
    showCodexPatOption: Boolean,
    initialInputMethod: String,
    authUrl: String,
    sessionId: String,
    error: String,
  },
  data: () => ({ inputMethod: 'manual', authCode: '', oauthState: '', refreshToken: '' }),
  emits: ['import-codex-session', 'import-codex-pat', 'generate-url', 'update:input-method'],
  methods: {
    reset() {
      this.inputMethod = 'manual'
      this.authCode = ''
      this.oauthState = ''
      this.refreshToken = ''
    },
  },
  template: `
    <div>
      <p v-if="error">{{ error }}</p>
      <slot v-if="inputMethod === 'manual'" name="manual-options" />
      <button data-testid="import-codex-session" @click="$emit('import-codex-session', 'session-json')">session</button>
      <button data-testid="import-codex-pat" @click="$emit('import-codex-pat', 'pat-token')">pat</button>
    </div>
  `,
})

const GroupSelectorStub = defineComponent({
  name: 'GroupSelector',
  props: {
    modelValue: {
      type: Array,
      default: () => [],
    },
  },
  emits: ['update:modelValue'],
  template: `
    <button
      type="button"
      data-testid="select-pricing-groups"
      @click="$emit('update:modelValue', [1, 2])"
    >
      groups
    </button>
  `,
})

const ModelWhitelistSelectorStub = defineComponent({
  name: 'ModelWhitelistSelector',
  props: {
    modelValue: {
      type: Array,
      default: () => [],
    },
    platform: String,
    syncCredentials: Object,
  },
  emits: ['update:modelValue', 'upstream-synced'],
  template: `<button
    type="button"
    data-testid="model-whitelist-selector"
    @click="$emit('update:modelValue', ['public-glm']); $emit('upstream-synced')"
  >models</button>`,
})

function mountModal(groups: any[] = [], useRealOAuthFlow = false, proxies: any[] = []) {
  return mount(CreateAccountModal, {
    props: { show: true, proxies, groups },
    global: {
      stubs: {
        BaseDialog: BaseDialogStub,
        OAuthAuthorizationFlow: useRealOAuthFlow ? false : OAuthAuthorizationFlowStub,
        ConfirmDialog: true,
        Select: true,
        Icon: true,
        PlatformIcon: true,
        ProxySelector: true,
        ProxyAdBanner: true,
        GroupSelector: GroupSelectorStub,
        ModelWhitelistSelector: ModelWhitelistSelectorStub,
        QuotaLimitCard: true,
      },
    },
  })
}

async function selectButtonByText(wrapper: ReturnType<typeof mountModal>, text: string) {
  const button = wrapper.findAll('button').find((candidate) => candidate.text().includes(text))
  expect(button).toBeDefined()
  await button?.trigger('click')
}

async function submitApiKeyAccount(
  platform: 'openai' | 'anthropic',
  enableLongContextBilling = false,
  disableUpstreamBillingProbe = false
) {
  const wrapper = mountModal()
  await selectButtonByText(wrapper, platform === 'openai' ? 'OpenAI' : 'admin.accounts.claudeConsole')
  if (platform === 'openai') {
    await selectButtonByText(wrapper, 'API Key')
  }
  await wrapper.get('form#create-account-form input[type="text"]').setValue(`${platform} account`)
  await wrapper.get('form#create-account-form input[type="password"]').setValue('test-api-key')
  if (enableLongContextBilling) {
    await wrapper.get('[data-testid="openai-long-context-billing-toggle"]').trigger('click')
  }
  if (disableUpstreamBillingProbe) {
    await wrapper.get('[data-testid="upstream-billing-auto-probe"]').trigger('click')
  }
  await wrapper.get('form#create-account-form').trigger('submit.prevent')
  await flushPromises()
  return wrapper
}

async function openCodexImportStep(toggleClicks = 0) {
  const wrapper = mountModal()
  await selectButtonByText(wrapper, 'OpenAI')
  for (let click = 0; click < toggleClicks; click += 1) {
    await wrapper.get('[data-testid="openai-long-context-billing-toggle"]').trigger('click')
  }
  await wrapper.get('form#create-account-form input[type="text"]').setValue('Codex import')
  await wrapper.get('form#create-account-form').trigger('submit.prevent')
  return wrapper
}

describe('CreateAccountModal OpenAI long-context billing', () => {
  beforeEach(() => {
    authIsSimpleMode.value = true
    createAccountMock.mockReset().mockResolvedValue({ id: 42, platform: 'openai', type: 'apikey' })
    probeUpstreamBillingMock.mockReset().mockResolvedValue({})
    syncUpstreamModelsMock.mockReset().mockResolvedValue({ models: [], metadata: {} })
    showWarningMock.mockReset()
    importCodexSessionMock.mockReset().mockResolvedValue({
      created: 1,
      updated: 0,
      skipped: 0,
      failed: 0,
      errors: [],
      warnings: [],
    })
    createOpenAICodexPATMock.mockReset().mockResolvedValue({})
    generateAuthUrlMock.mockReset().mockImplementation(async (_endpoint, config) => ({
      auth_url: 'https://auth.example.com/authorize?state=test-state', session_id: 'test-session',
      ...(config.proxy_mode === 'request' ? { proxy_mode: 'request', proxy_id: config.proxy_ids?.[0] } : {}),
    }))
    exchangeCodeMock.mockReset().mockResolvedValue({ access_token: 'at', refresh_token: 'rt', expires_at: 1900000000 })
  })

  afterEach(() => vi.useRealTimers())

  const requestProxies = [
    { id: 11, name: 'Proxy A', protocol: 'http', host: 'a.example.test', port: 8080, status: 'active', expires_at: null },
    { id: 22, name: 'Proxy B', protocol: 'http', host: 'b.example.test', port: 8080, status: 'active', expires_at: null },
  ]
  async function requestMode(wrapper: ReturnType<typeof mountModal>, ids = [11, 22]) {
    await wrapper.get('[data-testid="openai-proxy-mode-request"]').setValue()
    await wrapper.get('[data-testid="bps-proxy-trigger"]').trigger('click')
    for (const id of ids) await wrapper.get(`[data-testid="bps-proxy-${id}"]`).setValue(true)
  }
  async function requestAuthorization() {
    const wrapper = mountModal([], false, requestProxies)
    await selectButtonByText(wrapper, 'OpenAI')
    await wrapper.getComponent({ name: 'ProxySelector' }).vm.$emit('update:modelValue', 45)
    await wrapper.get('form#create-account-form input[type="text"]').setValue('Request account')
    await requestMode(wrapper)
    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    const flow = wrapper.getComponent(OAuthAuthorizationFlowStub)
    flow.vm.$emit('generate-url')
    await flushPromises()
    return { wrapper, flow }
  }

  it('creates request routing from a selected group and retains the previous single proxy', async () => {
    const { wrapper, flow } = await requestAuthorization()
    expect(generateAuthUrlMock).toHaveBeenLastCalledWith('/admin/openai/generate-auth-url', {
      proxy_mode: 'request', proxy_ids: [11, 22],
    })
    await flow.setData({ authCode: 'authorization-code' })
    await selectButtonByText(wrapper, 'admin.accounts.oauth.completeAuth')
    await flushPromises()
    expect(exchangeCodeMock).toHaveBeenLastCalledWith('/admin/openai/exchange-code', {
      session_id: 'test-session', code: 'authorization-code', state: 'test-state', proxy_mode: 'request', proxy_ids: [11, 22],
    })
    expect(createAccountMock).toHaveBeenCalledWith(expect.objectContaining({
      proxy_id: 45, extra: expect.objectContaining({ openai_proxy_mode: 'request', openai_proxy_ids: [11, 22] }),
    }))
    wrapper.unmount()
  })

  it.each(['expired', 'endpoint', 'credentials', 'inactive', 'removed'])('requires fresh authorization when selected egress is %s', async change => {
    const { wrapper, flow } = await requestAuthorization()
    const changes: Record<string, object> = {
      expired: { expires_at: '2000-01-01T00:00:00Z' }, endpoint: { host: 'changed.example.test' },
      credentials: { username: 'changed' }, inactive: { status: 'inactive' }, removed: {},
    }
    await wrapper.setProps({ proxies: change === 'removed' ? [requestProxies[1]] : [{ ...requestProxies[0], ...changes[change] }, requestProxies[1]] })
    await flow.setData({ authCode: 'authorization-code' })
    await selectButtonByText(wrapper, 'admin.accounts.oauth.completeAuth')
    expect(exchangeCodeMock).not.toHaveBeenCalled()
    expect(createAccountMock).not.toHaveBeenCalled()
    expect(wrapper.text()).toContain('admin.accounts.openai.proxyAuthorizationChanged')
    wrapper.unmount()
  })

  it('blocks an empty request group and catalog failures before starting authorization', async () => {
    const wrapper = mountModal([], false, requestProxies)
    await selectButtonByText(wrapper, 'OpenAI')
    await wrapper.get('form#create-account-form input[type="text"]').setValue('Request account')
    await wrapper.get('[data-testid="openai-proxy-mode-request"]').setValue()
    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    expect(wrapper.findComponent(OAuthAuthorizationFlowStub).exists()).toBe(false)
    await requestMode(wrapper, [11])
    await wrapper.setProps({ proxiesLoadError: true })
    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    expect(wrapper.findComponent(OAuthAuthorizationFlowStub).exists()).toBe(false)
    expect(generateAuthUrlMock).not.toHaveBeenCalled()
    await wrapper.get('[data-testid="bps-proxy-catalog-retry"]').trigger('click')
    expect(wrapper.emitted('retry-proxies')).toHaveLength(1)
    wrapper.unmount()
  })

  it.each(['pat', 'agent_identity'])('blocks unsupported %s imports in request mode', async method => {
    const { wrapper, flow } = await requestAuthorization()
    if (method === 'pat') flow.vm.$emit('import-codex-pat', 'pat-token')
    else flow.vm.$emit('import-codex-session', JSON.stringify({ auth_mode: 'agentIdentity', agent_identity: {} }))
    await flushPromises()
    expect(createOpenAICodexPATMock).not.toHaveBeenCalled()
    expect(importCodexSessionMock).not.toHaveBeenCalled()
    expect(wrapper.text()).toContain('admin.accounts.openai.proxyUnsupportedImport')
    wrapper.unmount()
  })

  it('applies request groups to the independent BPS platform without OpenAI-only extra settings', async () => {
    const wrapper = mountModal([], false, requestProxies)
    await selectButtonByText(wrapper, 'OpenAI BPS')
    await wrapper.get('form#create-account-form input[type="text"]').setValue('BPS group')
    await wrapper.get('#bps-access-token').setValue('private-test-token')
    await wrapper.get('#bps-account-id').setValue('workspace')
    await requestMode(wrapper)
    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    await flushPromises()
    wrapper.getComponent('[data-testid="bps-risk-confirmation"]').vm.$emit('confirm')
    await flushPromises()
    expect(createAccountMock).toHaveBeenCalledWith(expect.objectContaining({
      platform: 'openai_bps', extra: { openai_proxy_mode: 'request', openai_proxy_ids: [11, 22] },
    }))
    wrapper.unmount()
  })

  it('sets month and year expiry presets without submitting the account form', async () => {
    vi.useFakeTimers({ toFake: ['Date'] })
    vi.setSystemTime(new Date('2026-01-31T12:34:00'))
    const wrapper = mountModal()
    await selectButtonByText(wrapper, 'OpenAI')
    await selectButtonByText(wrapper, 'API Key')
    await wrapper.get('form#create-account-form input[type="text"]').setValue('expiry account')
    await wrapper.get('form#create-account-form input[type="password"]').setValue('test-api-key')
    const input = wrapper.get<HTMLInputElement>('input[type="datetime-local"]')

    for (const [label, expected] of [
      ['payment.oneMonth', '2026-02-28T12:34'],
      ['payment.oneYear', '2027-01-31T12:34'],
    ]) {
      const button = wrapper.findAll('button').find((candidate) => candidate.text() === label)!
      expect(button.attributes('type')).toBe('button')
      await button.trigger('click')
      expect(input.element.value).toBe(expected)
      expect(createAccountMock).not.toHaveBeenCalled()
    }

    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    await flushPromises()
    expect(createAccountMock.mock.calls[0]?.[0]?.expires_at).toBe(new Date('2027-01-31T12:34:00').getTime() / 1000)
    wrapper.unmount()
  })

  it('allows a manually entered expiry to override a preset before account creation', async () => {
    const wrapper = mountModal()
    await selectButtonByText(wrapper, 'OpenAI')
    await selectButtonByText(wrapper, 'API Key')
    await wrapper.get('form#create-account-form input[type="text"]').setValue('custom expiry account')
    await wrapper.get('form#create-account-form input[type="password"]').setValue('test-api-key')
    await selectButtonByText(wrapper, 'payment.oneMonth')
    await wrapper.get('input[type="datetime-local"]').setValue('2030-04-15T09:20')

    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    await flushPromises()
    expect(createAccountMock.mock.calls[0]?.[0]?.expires_at).toBe(new Date('2030-04-15T09:20:00').getTime() / 1000)
    wrapper.unmount()
  })

  it('hides only the redundant account toggle when every selected group enables tier pricing', async () => {
    authIsSimpleMode.value = false
    const wrapper = mountModal([
      { id: 1, long_context_pricing_enabled: true },
      { id: 2, long_context_pricing_enabled: true },
    ])

    await selectButtonByText(wrapper, 'OpenAI')
    await wrapper.get('[data-testid="select-pricing-groups"]').trigger('click')

    expect(wrapper.find('[data-testid="openai-long-context-billing-toggle"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="create-openai-ws-mode"]').exists()).toBe(true)
  })

  it('keeps the account toggle when any selected group disables tier pricing', async () => {
    authIsSimpleMode.value = false
    const wrapper = mountModal([
      { id: 1, long_context_pricing_enabled: true },
      { id: 2, long_context_pricing_enabled: false },
    ])

    await selectButtonByText(wrapper, 'OpenAI')
    await wrapper.get('[data-testid="select-pricing-groups"]').trigger('click')

    expect(wrapper.find('[data-testid="openai-long-context-billing-toggle"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="create-openai-ws-mode"]').exists()).toBe(true)
  })

  it('sends false explicitly for normal OpenAI account creation by default', async () => {
    await submitApiKeyAccount('openai')

    expect(createAccountMock).toHaveBeenCalledTimes(1)
    expect(createAccountMock.mock.calls[0]?.[0]?.extra?.openai_long_context_billing_enabled).toBe(false)
  })

  it('omits the upstream request id header from extra when left empty', async () => {
    await submitApiKeyAccount('openai')

    expect(createAccountMock).toHaveBeenCalledTimes(1)
    expect(createAccountMock.mock.calls[0]?.[0]?.extra).not.toHaveProperty('upstream_request_id_header')
  })

  it('sends the trimmed upstream request id header in extra when filled', async () => {
    const wrapper = mountModal()
    await selectButtonByText(wrapper, 'OpenAI')
    await selectButtonByText(wrapper, 'API Key')
    await wrapper.get('form#create-account-form input[type="text"]').setValue('openai account')
    await wrapper.get('form#create-account-form input[type="password"]').setValue('test-api-key')
    await wrapper.get('[data-testid="upstream-request-id-header"]').setValue('  X-Oneapi-Request-Id  ')
    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    await flushPromises()

    expect(createAccountMock).toHaveBeenCalledTimes(1)
    expect(createAccountMock.mock.calls[0]?.[0]?.extra?.upstream_request_id_header).toBe('X-Oneapi-Request-Id')
  })

  it('omits images_url_to_b64_json from extra by default', async () => {
    await submitApiKeyAccount('openai')

    expect(createAccountMock).toHaveBeenCalledTimes(1)
    expect(createAccountMock.mock.calls[0]?.[0]?.extra).not.toHaveProperty('images_url_to_b64_json')
  })

  it('sends images_url_to_b64_json in extra when the toggle is enabled', async () => {
    const wrapper = mountModal()
    await selectButtonByText(wrapper, 'OpenAI')
    await selectButtonByText(wrapper, 'API Key')
    await wrapper.get('form#create-account-form input[type="text"]').setValue('openai account')
    await wrapper.get('form#create-account-form input[type="password"]').setValue('test-api-key')
    await wrapper.get('[data-testid="openai-images-url-to-b64-json-toggle"]').trigger('click')
    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    await flushPromises()

    expect(createAccountMock).toHaveBeenCalledTimes(1)
    expect(createAccountMock.mock.calls[0]?.[0]?.extra?.images_url_to_b64_json).toBe(true)
  })

  it('persists upstream model metadata after creating an account from preview', async () => {
    const wrapper = mountModal()
    await selectButtonByText(wrapper, 'OpenAI')
    await selectButtonByText(wrapper, 'API Key')
    await wrapper.get('form#create-account-form input[type="text"]').setValue('OpenCode account')
    await wrapper.get('form#create-account-form input[type="password"]').setValue('test-api-key')
    await wrapper.get('[data-testid="model-whitelist-selector"]').trigger('click')
    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    await flushPromises()

    expect(createAccountMock).toHaveBeenCalledOnce()
    expect(syncUpstreamModelsMock).toHaveBeenCalledWith(42)
  })

  it('includes the current concrete model mapping in preview credentials', async () => {
    const wrapper = mountModal()
    await selectButtonByText(wrapper, 'OpenAI')
    await selectButtonByText(wrapper, 'API Key')
    await wrapper.get('form#create-account-form input[type="password"]').setValue('test-api-key')
    await wrapper.get('[data-testid="model-whitelist-selector"]').trigger('click')
    await flushPromises()

    expect(wrapper.getComponent(ModelWhitelistSelectorStub).props('syncCredentials')).toMatchObject({
      model_mapping: { 'public-glm': 'public-glm' }
    })
  })

  it('runs formal capability sync after creating an account with explicit mappings', async () => {
    const wrapper = mountModal()
    await selectButtonByText(wrapper, 'OpenAI')
    await selectButtonByText(wrapper, 'API Key')
    await wrapper.get('form#create-account-form input[type="text"]').setValue('Mapped account')
    await wrapper.get('form#create-account-form input[type="password"]').setValue('test-api-key')
    await selectButtonByText(wrapper, 'admin.accounts.modelMapping')
    await selectButtonByText(wrapper, 'admin.accounts.addMapping')
    await wrapper.get('input[placeholder="admin.accounts.requestModel"]').setValue('public-glm')
    await wrapper.get('input[placeholder="admin.accounts.actualModel"]').setValue('glm-5.3')
    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    await flushPromises()

    expect(createAccountMock.mock.calls[0]?.[0]?.credentials?.model_mapping).toEqual({
      'public-glm': 'glm-5.3'
    })
    expect(syncUpstreamModelsMock).toHaveBeenCalledWith(42)
  })

  it('warns when post-create capability metadata remains incomplete', async () => {
    syncUpstreamModelsMock.mockResolvedValue({
      models: ['x-preview-f-free'],
      warnings: [{ code: 'upstream_model_metadata_incomplete', message: 'metadata incomplete' }],
    })
    const wrapper = mountModal()
    await selectButtonByText(wrapper, 'OpenAI')
    await selectButtonByText(wrapper, 'API Key')
    await wrapper.get('form#create-account-form input[type="text"]').setValue('OpenCode account')
    await wrapper.get('form#create-account-form input[type="password"]').setValue('test-api-key')
    await wrapper.get('[data-testid="model-whitelist-selector"]').trigger('click')
    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    await flushPromises()

    expect(showWarningMock).toHaveBeenCalledWith(
      'admin.accounts.syncUpstreamModelsMetadataIncomplete'
    )
  })

  // namespace 摊平是仅 OAuth 的兼容开关：API Key 走 chat completions 回退桥时由桥自行摊平
  it('shows the Codex namespace flatten toggle only for OpenAI OAuth accounts', async () => {
    const wrapper = mountModal()
    await selectButtonByText(wrapper, 'OpenAI')

    expect(wrapper.find('[data-testid="create-openai-flatten-namespaces-toggle"]').exists()).toBe(
      true
    )

    await selectButtonByText(wrapper, 'API Key')
    expect(wrapper.find('[data-testid="create-openai-flatten-namespaces-toggle"]').exists()).toBe(
      false
    )
  })

  it('enables upstream billing probes by default for new OpenAI API key accounts', async () => {
    await submitApiKeyAccount('openai')

    expect(createAccountMock.mock.calls[0]?.[0]?.upstream_billing_probe_enabled).toBe(true)
  })

  it('waits for the initial upstream billing probe before refreshing the account list', async () => {
    let resolveProbe: (() => void) | undefined
    probeUpstreamBillingMock.mockImplementationOnce(
      () => new Promise<void>((resolve) => {
        resolveProbe = resolve
      })
    )

    const wrapper = await submitApiKeyAccount('openai')

    expect(probeUpstreamBillingMock).toHaveBeenCalledWith(42)
    expect(wrapper.emitted('created')).toBeUndefined()

    resolveProbe?.()
    await flushPromises()

    expect(wrapper.emitted('created')).toHaveLength(1)
  })

  it('sends an explicit disabled state when the create toggle is turned off', async () => {
    await submitApiKeyAccount('openai', false, true)

    expect(createAccountMock.mock.calls[0]?.[0]?.upstream_billing_probe_enabled).toBe(false)
    expect(probeUpstreamBillingMock).not.toHaveBeenCalled()
  })

  it('submits OpenCode Zen default protocol rules with adaptive endpoints', async () => {
    const wrapper = mountModal()
    await selectButtonByText(wrapper, 'OpenCode')
    await wrapper.get('form#create-account-form input[type="text"]').setValue('oc')
    await wrapper.get('form#create-account-form input[type="password"]').setValue('sk-opencode-zen')

    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    await flushPromises()

    expect(createAccountMock).toHaveBeenCalledTimes(1)
    expect(createAccountMock.mock.calls[0]?.[0]?.credentials).toMatchObject({
      account_mode: 'zen',
      api_protocol: 'adaptive',
      base_url: 'https://opencode.ai/zen/v1',
      api_base_urls: {
        chat_completions: 'https://opencode.ai/zen/v1',
        anthropic: 'https://opencode.ai/zen',
        responses: 'https://opencode.ai/zen/v1'
      },
      protocol_rules: [
        { pattern: 'grok-*', protocol: 'responses' },
        { pattern: 'gpt-*', protocol: 'responses' },
        { pattern: 'muse-spark-*', protocol: 'responses' },
        { pattern: 'claude-*', protocol: 'anthropic' },
        { pattern: 'qwen*', protocol: 'anthropic' }
      ]
    })
  })

  it('submits OpenCode GO endpoints after switching account type', async () => {
    const wrapper = mountModal()
    await selectButtonByText(wrapper, 'OpenCode')
    await selectButtonByText(wrapper, 'admin.accounts.opencodeGo.accountMode.go')
    await wrapper.get('form#create-account-form input[type="text"]').setValue('oc-go')
    await wrapper.get('form#create-account-form input[type="password"]').setValue('sk-opencode-go')

    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    await flushPromises()

    expect(createAccountMock).toHaveBeenCalledTimes(1)
    expect(createAccountMock.mock.calls[0]?.[0]?.credentials).toMatchObject({
      account_mode: 'go',
      api_protocol: 'adaptive',
      base_url: 'https://opencode.ai/zen/go/v1',
      api_base_urls: {
        chat_completions: 'https://opencode.ai/zen/go/v1',
        anthropic: 'https://opencode.ai/zen/go',
        responses: 'https://opencode.ai/zen/go/v1'
      },
      protocol_rules: [
        { pattern: 'grok-*', protocol: 'responses' },
        { pattern: 'gpt-*', protocol: 'responses' },
        { pattern: 'muse-spark-*', protocol: 'responses' },
        { pattern: 'minimax-*', protocol: 'anthropic' },
        { pattern: 'qwen*', protocol: 'anthropic' }
      ]
    })
  })

  it('submits adaptive Kimi protocol endpoints', async () => {
    const wrapper = mountModal()
    await selectButtonByText(wrapper, 'Kimi')
    await wrapper.get('form#create-account-form input[type="text"]').setValue('Kimi adaptive')
    await wrapper.get('form#create-account-form input[type="password"]').setValue('sk-kimi')

    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    await flushPromises()

    expect(createAccountMock).toHaveBeenCalledTimes(1)
    expect(createAccountMock.mock.calls[0]?.[0]?.credentials).toMatchObject({
      account_mode: 'payg',
      api_protocol: 'adaptive',
      base_url: 'https://api.moonshot.cn/v1',
      api_base_urls: {
        chat_completions: 'https://api.moonshot.cn/v1',
        anthropic: 'https://api.moonshot.cn/anthropic',
        responses: 'https://api.moonshot.cn/v1'
      }
    })
  })

  it('submits adaptive Kimi Coding Plan Responses endpoint', async () => {
    const wrapper = mountModal()
    await selectButtonByText(wrapper, 'Kimi')
    await selectButtonByText(wrapper, 'admin.accounts.cnProviders.accountMode.coding')
    await wrapper.get('form#create-account-form input[type="text"]').setValue('Kimi coding')
    await wrapper.get('form#create-account-form input[type="password"]').setValue('sk-kimi-coding')

    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    await flushPromises()

    expect(createAccountMock).toHaveBeenCalledTimes(1)
    expect(createAccountMock.mock.calls[0]?.[0]?.credentials).toMatchObject({
      account_mode: 'coding',
      api_protocol: 'adaptive',
      base_url: 'https://api.kimi.com/coding/v1',
      api_base_urls: {
        chat_completions: 'https://api.kimi.com/coding/v1',
        anthropic: 'https://api.kimi.com/coding',
        responses: 'https://api.kimi.com/coding/v1'
      }
    })
  })

  it('submits adaptive MiniMax protocol endpoints', async () => {
    const wrapper = mountModal()
    await selectButtonByText(wrapper, 'MiniMax')
    await wrapper.get('form#create-account-form input[type="text"]').setValue('MiniMax adaptive')
    await wrapper.get('form#create-account-form input[type="password"]').setValue('sk-minimax')

    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    await flushPromises()

    expect(createAccountMock).toHaveBeenCalledTimes(1)
    expect(createAccountMock.mock.calls[0]?.[0]?.credentials).toMatchObject({
      account_mode: 'payg',
      api_protocol: 'adaptive',
      base_url: 'https://api.minimaxi.com/v1',
      api_base_urls: {
        chat_completions: 'https://api.minimaxi.com/v1',
        anthropic: 'https://api.minimaxi.com/anthropic',
        responses: 'https://api.minimaxi.com/v1'
      }
    })
  })

  it('uses the edited adaptive Chat endpoint when previewing upstream models', async () => {
    const wrapper = mountModal()
    await selectButtonByText(wrapper, 'Kimi')
    await wrapper
      .get('[data-testid="cn-adaptive-base-url-chat_completions"]')
      .setValue('https://relay.example.com/v1')
    await wrapper.get('form#create-account-form input[type="password"]').setValue('sk-relay')

    expect(wrapper.getComponent(ModelWhitelistSelectorStub).props('syncCredentials')).toMatchObject({
      platform: 'kimi',
      type: 'apikey',
      base_url: 'https://relay.example.com/v1',
      api_key: 'sk-relay'
    })
  })

  it('exposes Agent Identity in the OpenAI authorization methods', async () => {
    const wrapper = mountModal()
    await selectButtonByText(wrapper, 'OpenAI')
    await wrapper.get('form#create-account-form input[type="text"]').setValue('OpenAI account')
    await wrapper.get('form#create-account-form').trigger('submit.prevent')

    const flow = wrapper.getComponent(OAuthAuthorizationFlowStub)
    expect(flow.props('showManualOption')).toBe(true)
    expect(flow.props('showCodexSessionImportOption')).toBe(true)
    expect(flow.props('showAgentIdentityOption')).toBe(true)
    expect(flow.props('showCodexPatOption')).toBe(true)
    expect(flow.props('initialInputMethod')).toBe('manual')
  })

  it('keeps the manual OAuth channel selection out of step one', async () => {
    const wrapper = mountModal()
    await selectButtonByText(wrapper, 'OpenAI')

    expect(wrapper.find('[data-testid="openai-excel-oauth"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="openai-oauth-channel"]').exists()).toBe(false)
  })

  it('shows the Codex and Excel channel choices for manual authorization', async () => {
    const wrapper = await openCodexImportStep()
    const channel = wrapper.get('[data-testid="openai-oauth-channel"]')
    const radios = channel.findAll<HTMLInputElement>('input[type="radio"]')

    expect(channel.text()).toContain('Codex OAuth')
    expect(channel.text()).toContain('Excel / BPS OAuth')
    expect(radios).toHaveLength(2)
    expect(radios[0]?.element.checked).toBe(true)
    expect(radios[1]?.element.checked).toBe(false)

    const flow = wrapper.getComponent(OAuthAuthorizationFlowStub)
    flow.vm.inputMethod = 'batch_login'
    await flushPromises()

    expect(wrapper.find('[data-testid="openai-oauth-channel"]').exists()).toBe(false)
  })

  it('uses the Excel OAuth client when generating the selected manual channel URL', async () => {
    const wrapper = await openCodexImportStep()
    const radios = wrapper.get('[data-testid="openai-oauth-channel"]').findAll('input[type="radio"]')
    await radios[1]?.setValue(true)

    const flow = wrapper.getComponent(OAuthAuthorizationFlowStub)
    flow.vm.$emit('generate-url')
    await flushPromises()

    expect(generateAuthUrlMock).toHaveBeenCalledWith('/admin/openai/generate-auth-url', {
      oauth_client: 'excel',
    })
    expect(flow.props('authUrl')).toBe('https://auth.example.com/authorize?state=test-state')
    expect(flow.props('sessionId')).toBe('test-session')
  })

  it('clears the actual manual callback when changing channels and keeps batch targets independent', async () => {
    const wrapper = mountModal([], true)
    await selectButtonByText(wrapper, 'OpenAI')
    await wrapper.get('form#create-account-form input[type="text"]').setValue('OpenAI account')
    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    const flow = wrapper.getComponent(OAuthAuthorizationFlow)
    const channel = wrapper.get('[data-testid="openai-oauth-channel"]')

    flow.vm.$emit('generate-url')
    await flushPromises()
    expect(generateAuthUrlMock).toHaveBeenLastCalledWith('/admin/openai/generate-auth-url', {})
    await flow.get('textarea').setValue('http://localhost/callback?code=old-code&state=old-state')
    await flushPromises()
    expect(flow.vm.authCode).toBe('old-code')
    expect(flow.vm.oauthState).toBe('old-state')

    await channel.findAll('input[type="radio"]')[1]?.setValue(true)
    await flushPromises()
    expect(flow.props('authUrl')).toBe('')
    expect(flow.props('sessionId')).toBe('')
    expect(flow.vm.authCode).toBe('')
    expect(flow.vm.oauthState).toBe('')

    await flow.get('input[type="radio"][value="batch_login"]').setValue(true)
    await flushPromises()
    expect(wrapper.find('[data-testid="openai-oauth-channel"]').exists()).toBe(false)
    expect(flow.get<HTMLInputElement>('input[type="radio"][value="batch_login"]').element.checked).toBe(true)
    expect(flow.get<HTMLInputElement>('input[type="checkbox"][value="bps"]').element.checked).toBe(true)
    expect(flow.get<HTMLInputElement>('input[type="checkbox"][value="codex"]').element.checked).toBe(false)
    await flow.get('input[type="checkbox"][value="bps"]').setValue(false)
    await flow.get('input[type="checkbox"][value="codex"]').setValue(true)
    expect(flow.get<HTMLInputElement>('input[type="radio"][value="batch_login"]').element.checked).toBe(true)

    await flow.get('input[type="radio"][value="manual"]').setValue(true)
    await flushPromises()
    expect(wrapper.get('[data-testid="openai-oauth-channel"]').findAll<HTMLInputElement>('input[type="radio"]')[0]?.element.checked).toBe(true)
    flow.vm.$emit('generate-url')
    await flushPromises()
    expect(generateAuthUrlMock).toHaveBeenLastCalledWith('/admin/openai/generate-auth-url', {})
    wrapper.unmount()
  })

  it('preserves the selected refresh-token method when leaving the Excel manual channel', async () => {
    const wrapper = await openCodexImportStep()
    const radios = wrapper.get('[data-testid="openai-oauth-channel"]').findAll('input[type="radio"]')
    await radios[1]?.setValue(true)
    const flow = wrapper.getComponent(OAuthAuthorizationFlowStub)
    const reset = vi.spyOn(flow.vm, 'reset')

    flow.vm.inputMethod = 'refresh_token'
    flow.vm.$emit('update:input-method', 'refresh_token')
    await flushPromises()

    expect(flow.vm.inputMethod).toBe('refresh_token')
    expect(reset).not.toHaveBeenCalled()
    expect(wrapper.find('[data-testid="openai-oauth-channel"]').exists()).toBe(false)
  })

  it('clears the generated session, child credentials, and pricing groups when changing channels', async () => {
    authIsSimpleMode.value = false
    const wrapper = await openCodexImportStep()
    const radios = wrapper.get('[data-testid="openai-oauth-channel"]').findAll('input[type="radio"]')
    await radios[1]?.setValue(true)
    await wrapper.get('[data-testid="select-pricing-groups"]').trigger('click')

    const flow = wrapper.getComponent(OAuthAuthorizationFlowStub)
    flow.vm.$emit('generate-url')
    await flushPromises()
    flow.vm.authCode = 'old-code'
    flow.vm.oauthState = 'old-state'
    flow.vm.refreshToken = 'old-refresh-token'
    const reset = vi.spyOn(flow.vm, 'reset')
    expect(wrapper.getComponent(GroupSelectorStub).props('modelValue')).toEqual([1, 2])

    await radios[0]?.setValue(true)
    await flushPromises()

    expect(flow.props('authUrl')).toBe('')
    expect(flow.props('sessionId')).toBe('')
    expect(reset).toHaveBeenCalledOnce()
    expect(flow.vm.authCode).toBe('')
    expect(flow.vm.oauthState).toBe('')
    expect(flow.vm.refreshToken).toBe('')

    await radios[1]?.setValue(true)
    expect(wrapper.getComponent(GroupSelectorStub).props('modelValue')).toEqual([])
  })

  it.each([
    ['camelCase', { authMode: 'agentIdentity', agentIdentity: { agentRuntimeId: 'runtime' } }],
    ['nested identity without auth_mode', { agent_identity: { agent_runtime_id: 'runtime' } }],
  ])('accepts backend-compatible %s Agent Identity imports', async (_name, content) => {
    const wrapper = await openCodexImportStep()
    const flow = wrapper.getComponent(OAuthAuthorizationFlowStub)
    flow.vm.inputMethod = 'agent_identity'

    flow.vm.$emit('import-codex-session', JSON.stringify(content))
    await flushPromises()

    expect(importCodexSessionMock).toHaveBeenCalledTimes(1)
  })

  it('sends true explicitly when OpenAI long-context billing is enabled', async () => {
    await submitApiKeyAccount('openai', true)

    expect(createAccountMock).toHaveBeenCalledTimes(1)
    expect(createAccountMock.mock.calls[0]?.[0]?.extra?.openai_long_context_billing_enabled).toBe(true)
  })

  it('omits the OpenAI setting for non-OpenAI account creation', async () => {
    await submitApiKeyAccount('anthropic')

    expect(createAccountMock).toHaveBeenCalledTimes(1)
    expect(createAccountMock.mock.calls[0]?.[0]?.extra?.openai_long_context_billing_enabled).toBeUndefined()
    // 上游倍率探测已放宽到全部 API-key 平台：非 OpenAI 平台与 OpenAI 一致，默认开启。
    expect(createAccountMock.mock.calls[0]?.[0]?.upstream_billing_probe_enabled).toBe(true)
  })

  it('sends an explicit disabled state when the non-OpenAI create toggle is turned off', async () => {
    await submitApiKeyAccount('anthropic', false, true)

    expect(createAccountMock.mock.calls[0]?.[0]?.upstream_billing_probe_enabled).toBe(false)
  })

  it('antigravity upstream 创建默认携带上游倍率探测开关', async () => {
    // antigravity upstream 走独立创建 helper，
    // 也必须与其余 API-key 平台一样默认开启探测并传递开关。
    const wrapper = mountModal()
    await selectButtonByText(wrapper, 'Antigravity')
    await selectButtonByText(wrapper, 'admin.accounts.types.antigravityApikey')
    await wrapper.get('form#create-account-form input[type="text"]').setValue('antigravity relay')
    const baseInput = wrapper
      .findAll('input')
      .find((candidate) => candidate.attributes('placeholder') === 'https://cloudcode-pa.googleapis.com')
    expect(baseInput).toBeDefined()
    await baseInput?.setValue('https://relay.example')
    await wrapper.get('form#create-account-form input[type="password"]').setValue('sk-upstream')
    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    await flushPromises()

    expect(createAccountMock).toHaveBeenCalledTimes(1)
    const payload = createAccountMock.mock.calls[0]?.[0]
    expect(payload?.platform).toBe('antigravity')
    expect(payload?.type).toBe('apikey')
    expect(payload?.upstream_billing_probe_enabled).toBe(true)
    // 创建成功后前端立即发起一次首探（与其他 apikey 平台一致）。
    expect(probeUpstreamBillingMock).toHaveBeenCalledWith(42)
  })

  it('leaves Codex session import billing ownership to the backend', async () => {
    const wrapper = await openCodexImportStep()
    await wrapper.get('[data-testid="import-codex-session"]').trigger('click')
    await flushPromises()

    expect(importCodexSessionMock).toHaveBeenCalledTimes(1)
    expect(importCodexSessionMock.mock.calls[0]?.[0]?.extra?.openai_long_context_billing_enabled).toBeUndefined()
  })

  it('leaves Codex PAT import billing ownership to the backend', async () => {
    const wrapper = await openCodexImportStep()
    await wrapper.get('[data-testid="import-codex-pat"]').trigger('click')
    await flushPromises()

    expect(createOpenAICodexPATMock).toHaveBeenCalledTimes(1)
    expect(createOpenAICodexPATMock.mock.calls[0]?.[0]?.extra?.openai_long_context_billing_enabled).toBeUndefined()
  })

  it('sends explicit true for Codex session import after the toggle is enabled', async () => {
    const wrapper = await openCodexImportStep(1)
    await wrapper.get('[data-testid="import-codex-session"]').trigger('click')
    await flushPromises()

    expect(importCodexSessionMock.mock.calls[0]?.[0]?.extra?.openai_long_context_billing_enabled).toBe(true)
  })

  it('sends explicit false for Codex session import after the toggle is changed back', async () => {
    const wrapper = await openCodexImportStep(2)
    await wrapper.get('[data-testid="import-codex-session"]').trigger('click')
    await flushPromises()

    expect(importCodexSessionMock.mock.calls[0]?.[0]?.extra?.openai_long_context_billing_enabled).toBe(false)
  })

  it('sends explicit true for Codex PAT import after the toggle is enabled', async () => {
    const wrapper = await openCodexImportStep(1)
    await wrapper.get('[data-testid="import-codex-pat"]').trigger('click')
    await flushPromises()

    expect(createOpenAICodexPATMock.mock.calls[0]?.[0]?.extra?.openai_long_context_billing_enabled).toBe(true)
  })

  it('sends explicit false for Codex PAT import after the toggle is changed back', async () => {
    const wrapper = await openCodexImportStep(2)
    await wrapper.get('[data-testid="import-codex-pat"]').trigger('click')
    await flushPromises()

    expect(createOpenAICodexPATMock.mock.calls[0]?.[0]?.extra?.openai_long_context_billing_enabled).toBe(false)
  })
})

describe('CreateAccountModal OpenAI BPS', () => {
  it('switches from TypeSafe to BPS without an unrelated API-key requirement and opens the saved connection tester', async () => {
    const saved = { id: 42, platform: 'openai_bps', type: 'oauth' }
    createAccountMock.mockReset().mockResolvedValue(saved)
    const wrapper = mountModal()
    await selectButtonByText(wrapper, 'TypeSafe / Jev')
    expect(wrapper.find('input[placeholder="ts-..."]').exists()).toBe(true)
    await selectButtonByText(wrapper, 'OpenAI BPS')
    expect(wrapper.find('input[placeholder="sk-ant-..."]').exists()).toBe(false)
    await wrapper.get('form#create-account-form input[type="text"]').setValue('BPS account')
    await wrapper.get('#bps-access-token').setValue('test-token')
    await wrapper.get('#bps-account-id').setValue('workspace')
    await wrapper.get('[data-testid="bps-save-and-test"]').trigger('click')
    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    await flushPromises()
    expect(createAccountMock).not.toHaveBeenCalled()
    const confirmation = wrapper.getComponent('[data-testid="bps-risk-confirmation"]')
    expect(confirmation.props('show')).toBe(true)
    confirmation.vm.$emit('confirm')
    await flushPromises()
    expect(createAccountMock).toHaveBeenCalledWith(expect.objectContaining({
      platform: 'openai_bps', type: 'oauth', credentials: expect.objectContaining({ access_token: 'test-token', chatgpt_account_id: 'workspace' })
    }))
    expect(wrapper.emitted('test')).toEqual([[saved]])
    wrapper.unmount()
  })
})


describe('BPS risk confirmation', () => {
  async function readyBPS() {
    createAccountMock.mockReset().mockResolvedValue({ id: 42, platform: 'openai_bps', type: 'oauth' })
    const wrapper = mountModal()
    expect(wrapper.find('[data-testid="bps-risk-warning"]').exists()).toBe(false)
    await selectButtonByText(wrapper, 'OpenAI BPS')
    await wrapper.get('form#create-account-form input[type="text"]').setValue('BPS risk test')
    await wrapper.get('#bps-access-token').setValue('private-test-token')
    await wrapper.get('#bps-account-id').setValue('workspace')
    return wrapper
  }
  it('shows a warning and requires confirmation for form/Enter submission', async () => {
    const wrapper = await readyBPS()
    expect(wrapper.get('[data-testid="bps-risk-warning"]').attributes('role')).toBe('alert')
    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    await flushPromises()
    expect(createAccountMock).not.toHaveBeenCalled()
    const dialog = wrapper.getComponent('[data-testid="bps-risk-confirmation"]')
    expect(dialog.props('show')).toBe(true)
    expect(dialog.props('message')).toBe('admin.accounts.bps.riskConfirmMessage')
    expect(dialog.props('message')).not.toContain('private-test-token')
    dialog.vm.$emit('confirm')
    dialog.vm.$emit('confirm')
    await flushPromises()
    expect(createAccountMock).toHaveBeenCalledTimes(1)
    expect(wrapper.emitted('test')).toBeUndefined()
    wrapper.unmount()
  })
  it('cancels without creating or losing input and asks again on the next save', async () => {
    const wrapper = await readyBPS()
    await wrapper.get('[data-testid="bps-save-and-test"]').trigger('click')
    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    await flushPromises()
    const dialog = wrapper.getComponent('[data-testid="bps-risk-confirmation"]')
    dialog.vm.$emit('cancel')
    await flushPromises()
    expect(createAccountMock).not.toHaveBeenCalled()
    expect(dialog.props('show')).toBe(false)
    expect((wrapper.get('#bps-access-token').element as HTMLInputElement).value).toBe('private-test-token')
    expect((wrapper.get('#bps-account-id').element as HTMLInputElement).value).toBe('workspace')
    expect(wrapper.emitted('close')).toBeUndefined()
    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    await flushPromises()
    expect(dialog.props('show')).toBe(true)
    dialog.vm.$emit('confirm')
    await flushPromises()
    expect(createAccountMock).toHaveBeenCalledTimes(1)
    expect(wrapper.emitted('test')).toBeUndefined()
    wrapper.unmount()
  })
  it('drops pending confirmation when the parent closes', async () => {
    const wrapper = await readyBPS()
    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    await flushPromises()
    const dialog = wrapper.getComponent('[data-testid="bps-risk-confirmation"]')
    await wrapper.setProps({ show: false })
    dialog.vm.$emit('confirm')
    await flushPromises()
    expect(createAccountMock).not.toHaveBeenCalled()
    expect(dialog.props('show')).toBe(false)
    wrapper.unmount()
  })
})
