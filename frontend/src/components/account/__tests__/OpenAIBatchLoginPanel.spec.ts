import { flushPromises, mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { createI18n } from 'vue-i18n'
import zhAccounts from '@/i18n/locales/zh/admin/accounts'
import OpenAIBatchLoginPanel from '../OpenAIBatchLoginPanel.vue'

const api = vi.hoisted(() => ({
  startBatchLogin: vi.fn(),
  getBatchLogin: vi.fn(),
  cancelBatchLogin: vi.fn()
}))

vi.mock('@/api/admin/openaiBatchLogin', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@/api/admin/openaiBatchLogin')>()
  return { ...actual, ...api }
})
vi.mock('@/api/admin', () => ({
  adminAPI: { groups: { getAll: vi.fn().mockResolvedValue([]) } }
}))

const LINE = 'a@example.com----pw-secret----JBSWY3DPEHPK3PXP'

function render(locale = 'en') {
  return mount(OpenAIBatchLoginPanel, {
    props: { proxyId: 5, concurrency: 7, priority: 2 },
    global: {
      plugins: [
        createI18n({
          legacy: false,
          locale,
          missingWarn: false,
          fallbackWarn: false,
          messages: { en: {}, zh: { admin: zhAccounts } }
        })
      ],
      stubs: { GroupSelector: true }
    }
  })
}

beforeEach(() => {
  vi.useFakeTimers()
  api.startBatchLogin.mockReset()
  api.getBatchLogin.mockReset()
  api.cancelBatchLogin.mockReset()
})

afterEach(() => {
  vi.useRealTimers()
})

describe('OpenAIBatchLoginPanel', () => {
  it.each([
    { targets: ['codex'] },
    { targets: ['bps', 'codex'] }
  ])('sends the independently selected batch targets $targets and their own groups', async ({ targets: selectedTargets }) => {
    api.startBatchLogin.mockResolvedValue({ id: 'targets', status: 'running', items: [] })
    const wrapper = render('zh')
    if (!selectedTargets.includes('bps')) {
      await wrapper.get('input[type="checkbox"][value="bps"]').setValue(false)
    }
    await wrapper.get('input[type="checkbox"][value="codex"]').setValue(true)
    for (const selector of wrapper.findAllComponents({ name: 'GroupSelector' })) {
      selector.vm.$emit('update:modelValue', selector.props('platform') === 'openai_bps' ? [10] : [20])
    }
    await wrapper.get('textarea').setValue(LINE)
    await wrapper.get('button.btn-primary').trigger('click')
    await flushPromises()

    expect(api.startBatchLogin).toHaveBeenCalledWith(expect.objectContaining({
      targets: selectedTargets,
      bps_group_ids: selectedTargets.includes('bps') ? [10] : [],
      codex_group_ids: [20]
    }))
    wrapper.unmount()
  })

  it.each([
    ['403', '上游拒绝访问或要求安全验证'],
    ['429', '请求过于频繁'],
    ['503', '上游服务暂时不可用'],
    ['418', '']
  ])('shows HTTP %s with the failed login step', async (status, description) => {
    api.startBatchLogin.mockResolvedValue({
      id: 'http-error', status: 'running', items: []
    })
    api.getBatchLogin.mockResolvedValue({
      id: 'http-error', status: 'done',
      items: [{ email: 'a@example.com', target: 'codex', status: 'failed', stage: 'password', code: `codex_oauth_http_${status}` }]
    })
    const wrapper = render('zh')
    await wrapper.get('textarea').setValue(LINE)
    await wrapper.get('button.btn-primary').trigger('click')
    await flushPromises()
    await vi.advanceTimersByTimeAsync(1000)

    expect(wrapper.get('li').text()).toContain('验证密码失败')
    expect(wrapper.get('li').text()).toContain(`HTTP ${status}`)
    if (description) expect(wrapper.get('li').text()).toContain(description)
    expect(wrapper.get('li').text()).not.toContain('codex_oauth_http_')
    wrapper.unmount()
  })

  it('updates each running step from polling and displays the final status after completion', async () => {
    const item = { email: 'a@example.com', target: 'codex' }
    api.startBatchLogin.mockResolvedValue({
      id: 'steps', status: 'running', items: [{ ...item, status: 'running', stage: 'proxy_check' }]
    })
    api.getBatchLogin
      .mockResolvedValueOnce({
        id: 'steps', status: 'running', items: [{ ...item, status: 'running', stage: 'password' }]
      })
      .mockResolvedValueOnce({
        id: 'steps', status: 'running', items: [{ ...item, status: 'running', stage: 'totp' }]
      })
      .mockResolvedValueOnce({
        id: 'steps', status: 'done', items: [{ ...item, status: 'created', stage: 'token_exchange' }]
      })
    const wrapper = render('zh')
    await wrapper.get('textarea').setValue(LINE)
    await wrapper.get('button.btn-primary').trigger('click')
    await flushPromises()

    expect(wrapper.get('li').text()).toContain('检测代理…')
    await vi.advanceTimersByTimeAsync(1000)
    expect(wrapper.get('li').text()).toContain('验证密码…')
    await vi.advanceTimersByTimeAsync(1000)
    expect(wrapper.get('li').text()).toContain('验证 2FA…')
    await vi.advanceTimersByTimeAsync(1000)
    expect(wrapper.get('li').text()).toContain('已创建')
    expect(wrapper.get('li').text()).not.toContain('交换令牌')
    wrapper.unmount()
  })

  it('falls back to the running status for missing and unknown progress stages', async () => {
    const item = { email: 'a@example.com', target: 'bps', status: 'running' }
    api.startBatchLogin.mockResolvedValue({ id: 'legacy', status: 'running', items: [item] })
    api.getBatchLogin.mockResolvedValue({
      id: 'legacy', status: 'running', items: [{ ...item, stage: 'unrecognized-stage' }]
    })
    const wrapper = render('zh')
    await wrapper.get('textarea').setValue(LINE)
    await wrapper.get('button.btn-primary').trigger('click')
    await flushPromises()

    expect(wrapper.get('li').text()).toContain('授权中')
    await vi.advanceTimersByTimeAsync(1000)
    expect(wrapper.get('li').text()).toContain('授权中')
    expect(wrapper.get('li').text()).not.toContain('unrecognized-stage')
    wrapper.unmount()
  })

  it('does not call the API for a malformed batch and never echoes the input', async () => {
    const wrapper = render()
    await wrapper.get('textarea').setValue('a@example.com----only-two-fields')
    await wrapper.get('button.btn-primary').trigger('click')
    await flushPromises()
    expect(api.startBatchLogin).not.toHaveBeenCalled()
    expect(wrapper.html()).not.toContain('only-two-fields</')
    expect(wrapper.find('[role="alert"]').exists()).toBe(true)
  })

  it('sends parsed rows, clears the textarea and emits created when the job finishes', async () => {
    api.startBatchLogin.mockResolvedValue({
      id: 'job1',
      status: 'running',
      items: [{ email: 'a@example.com', target: 'bps', status: 'pending' }]
    })
    api.getBatchLogin.mockResolvedValue({
      id: 'job1',
      status: 'done',
      items: [{ email: 'a@example.com', target: 'bps', status: 'created', account_id: 9 }]
    })
    const wrapper = render()
    await wrapper.get('textarea').setValue(LINE)
    await wrapper.get('button.btn-primary').trigger('click')
    await flushPromises()

    expect(api.startBatchLogin).toHaveBeenCalledTimes(1)
    const request = api.startBatchLogin.mock.calls[0][0]
    expect(request).toMatchObject({
      text: LINE,
      targets: ['bps'],
      proxy_id: 5,
      concurrency: 7,
      priority: 2
    })
    // The secrets are no longer in the DOM once the server owns them.
    expect(wrapper.find('textarea').exists()).toBe(false)
    expect(wrapper.html()).not.toContain('pw-secret')
    expect(wrapper.emitted('busy')?.[0]).toEqual([true])

    await vi.advanceTimersByTimeAsync(2000)
    await flushPromises()
    expect(api.getBatchLogin).toHaveBeenCalledWith('job1')
    expect(wrapper.emitted('created')).toHaveLength(1)
    expect(wrapper.emitted('busy')?.at(-1)).toEqual([false])
  })

  it('stops polling after unmount', async () => {
    api.startBatchLogin.mockResolvedValue({ id: 'job2', status: 'running', items: [] })
    api.getBatchLogin.mockResolvedValue({ id: 'job2', status: 'running', items: [] })
    const wrapper = render()
    await wrapper.get('textarea').setValue(LINE)
    await wrapper.get('button.btn-primary').trigger('click')
    await flushPromises()

    await vi.advanceTimersByTimeAsync(2000)
    const callsBefore = api.getBatchLogin.mock.calls.length
    wrapper.unmount()
    await vi.advanceTimersByTimeAsync(10000)
    expect(api.getBatchLogin.mock.calls.length).toBe(callsBefore)
  })

  it('describes email verification as an incomplete automated login', async () => {
    const item = { email: 'a@example.com', target: 'bps' }
    api.startBatchLogin.mockResolvedValue({
      id: 'job3',
      status: 'running',
      items: [{ ...item, status: 'pending' }]
    })
    api.getBatchLogin.mockResolvedValue({
      id: 'job3',
      status: 'done',
      items: [{ ...item, status: 'failed', stage: 'password', code: 'email_otp_required' }]
    })
    const wrapper = render('zh')
    await wrapper.get('textarea').setValue(LINE)
    await wrapper.get('button.btn-primary').trigger('click')
    await flushPromises()

    await vi.advanceTimersByTimeAsync(2000)
    expect(wrapper.get('li').text()).toContain('自动登录停在邮箱验证码步骤，授权尚未完成')
    expect(wrapper.get('li').text()).not.toContain('验证密码…')
  })

  it('shows the Codex stage and risk-control category for a failed preflight', async () => {
    const item = { email: 'a@example.com', target: 'codex' }
    api.startBatchLogin.mockResolvedValue({
      id: 'job4',
      status: 'running',
      items: [{ ...item, status: 'pending' }]
    })
    api.getBatchLogin.mockResolvedValue({
      id: 'job4',
      status: 'done',
      items: [{ ...item, status: 'failed', code: 'codex_chatgpt_preflight_risk_control' }]
    })
    const wrapper = render('zh')
    await wrapper.get('textarea').setValue(LINE)
    await wrapper.get('button.btn-primary').trigger('click')
    await flushPromises()

    await vi.advanceTimersByTimeAsync(2000)
    expect(wrapper.get('li').text()).toContain('Codex ChatGPT 代理预检: 命中安全校验')
  })
})
