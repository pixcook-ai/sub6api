import { flushPromises, mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { createI18n } from 'vue-i18n'
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

function render() {
  return mount(OpenAIBatchLoginPanel, {
    props: { proxyId: 5, concurrency: 7, priority: 2 },
    global: {
      plugins: [
        createI18n({
          legacy: false,
          locale: 'en',
          missingWarn: false,
          fallbackWarn: false,
          messages: { en: {} }
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
})
