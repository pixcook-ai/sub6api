import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createPinia } from 'pinia'
import { createI18n } from 'vue-i18n'
import OpenAIBPSAccountFields from '../OpenAIBPSAccountFields.vue'
import { bpsCredentials, newBPSAccountDraft } from '@/utils/openaiBps'
import ModelWhitelistSelector from '../ModelWhitelistSelector.vue'

const { getAvailableModels, showInfo } = vi.hoisted(() => ({
  getAvailableModels: vi.fn(),
  showInfo: vi.fn()
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    accounts: {
      getAvailableModels
    }
  }
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showInfo })
}))

function render(editing = false) {
  const draft = newBPSAccountDraft({ access_token: 'must-never-render', chatgpt_account_id: 'workspace-1' })
  return { draft, wrapper: mount(OpenAIBPSAccountFields, {
    props: { modelValue: draft, editing },
    global: { plugins: [createPinia(), createI18n({ legacy: false, locale: 'en', missingWarn: false, fallbackWarn: false, messages: { en: {} } })] }
  }) }
}
describe('OpenAI BPS account fields', () => {
  beforeEach(() => {
    getAvailableModels.mockReset()
    showInfo.mockReset()
  })

  it('never loads a saved token into the edit form and preserves it on a blank edit', () => {
    const { draft, wrapper } = render(true)
    const token = wrapper.get<HTMLInputElement>('#bps-access-token')
    expect(token.attributes('type')).toBe('password')
    expect(token.element.value).toBe('')
    expect(token.attributes('required')).toBeUndefined()
    expect(wrapper.html()).not.toContain('must-never-render')
    expect(bpsCredentials(draft)).not.toHaveProperty('access_token')
    expect(bpsCredentials(draft).chatgpt_account_id).toBe('workspace-1')
  })
  it('requires a token on creation and submits replacement credentials and model aliases', async () => {
    const { draft, wrapper } = render()
    expect(wrapper.get('#bps-access-token').attributes('required')).toBeDefined()
    await wrapper.get('#bps-access-token').setValue('  replacement  ')
    await wrapper.get('#bps-account-id').setValue(' override ')
    draft.allowedModels = []
    draft.modelMappings = [{ from: 'my-astra', to: 'gpt-6-astra' }]
    expect(bpsCredentials(draft)).toEqual({ access_token: 'replacement', chatgpt_account_id: 'override', model_mapping: { 'my-astra': 'gpt-6-astra' } })
  })

  it('starts with the two candidate models and preserves administrator mappings', () => {
    expect(Object.keys(bpsCredentials(newBPSAccountDraft()).model_mapping as object)).toEqual(['gpt-6-astra', 'gpt-5.6-sol'])
    const unrestrictedDraft = newBPSAccountDraft({ chatgpt_account_id: 'legacy-workspace' })
    expect(unrestrictedDraft.allowedModels).toEqual([])
    expect(unrestrictedDraft.modelMappings).toEqual([])
    expect(bpsCredentials(unrestrictedDraft).model_mapping).toEqual({})

    const draft = newBPSAccountDraft({ model_mapping: { 'gpt-6-astra': 'gpt-6-astra', alias: 'gpt-5.6-sol' } })
    expect(draft.allowedModels).toEqual(['gpt-6-astra'])
    expect(draft.modelMappings).toEqual([{ from: 'alias', to: 'gpt-5.6-sol' }])
    draft.allowedModels = []
    draft.modelMappings = []
    expect(bpsCredentials(draft).model_mapping).toEqual({})
  })

  it('passes whitelist and alias state separately to the selector and supports adding aliases', async () => {
    const draft = newBPSAccountDraft({
      model_mapping: { 'gpt-6-astra': 'gpt-6-astra', alias: 'gpt-5.6-sol' }
    })
    const wrapper = mount(OpenAIBPSAccountFields, {
      props: { modelValue: draft, editing: true },
      global: {
        plugins: [
          createPinia(),
          createI18n({ legacy: false, locale: 'en', missingWarn: false, fallbackWarn: false, messages: { en: {} } })
        ]
      }
    })
    const selector = wrapper.findComponent(ModelWhitelistSelector)

    expect(selector.props('modelValue')).toEqual(['gpt-6-astra'])
    expect(selector.props('modelMappings')).toEqual([{ from: 'alias', to: 'gpt-5.6-sol' }])

    await wrapper.get('[data-testid=\"add-model-mapping\"]').trigger('click')
    expect(draft.modelMappings).toEqual([
      { from: 'alias', to: 'gpt-5.6-sol' },
      { from: '', to: '' }
    ])
  })

  it('keeps a whitelist rule until a same-name mapping has a target', async () => {
    const draft = newBPSAccountDraft({
      model_mapping: { 'gpt-6-astra': 'gpt-6-astra' }
    })
    draft.modelMappings.push({ from: '', to: '' })
    const wrapper = mount(OpenAIBPSAccountFields, {
      props: { modelValue: draft, editing: true },
      global: {
        plugins: [
          createPinia(),
          createI18n({ legacy: false, locale: 'en', missingWarn: false, fallbackWarn: false, messages: { en: {} } })
        ]
      }
    })

    await wrapper.get('[data-testid="bps-model-mapping-source"]').setValue('gpt-6-astra')
    expect(bpsCredentials(draft).model_mapping).toEqual({ 'gpt-6-astra': 'gpt-6-astra' })

    await wrapper.get('[data-testid="bps-model-mapping-target"]').setValue('gpt-5.6-sol')
    expect(bpsCredentials(draft).model_mapping).toEqual({ 'gpt-6-astra': 'gpt-5.6-sol' })
  })

  it('uses only the account upstream catalog, including when it is empty', async () => {
    getAvailableModels.mockResolvedValue([])
    const draft = newBPSAccountDraft({ model_mapping: { 'gpt-6-astra': 'gpt-6-astra' } })
    const wrapper = mount(OpenAIBPSAccountFields, {
      props: { modelValue: draft, editing: true, accountId: 42 },
      global: {
        plugins: [
          createPinia(),
          createI18n({ legacy: false, locale: 'en', missingWarn: false, fallbackWarn: false, messages: { en: {} } })
        ]
      }
    })

    await flushPromises()

    const selector = wrapper.findComponent(ModelWhitelistSelector)
    expect(getAvailableModels).toHaveBeenCalledWith(42, 'upstream')
    expect(selector.props('candidateModels')).toEqual([])
    await selector.get('div.cursor-pointer').trigger('click')
    expect(selector.findAll('[data-testid=\"model-option\"]')).toHaveLength(0)
  })

  it('ignores an upstream catalog response after the account changes', async () => {
    let resolveFirst!: (value: { id: string }[]) => void
    let resolveSecond!: (value: { id: string }[]) => void
    const firstResult = new Promise<{ id: string }[]>(resolve => { resolveFirst = resolve })
    const secondResult = new Promise<{ id: string }[]>(resolve => { resolveSecond = resolve })
    getAvailableModels.mockImplementation((accountId: number) => accountId === 1 ? firstResult : secondResult)
    const draft = newBPSAccountDraft()
    const wrapper = mount(OpenAIBPSAccountFields, {
      props: { modelValue: draft, editing: true, accountId: 1 },
      global: {
        plugins: [
          createPinia(),
          createI18n({ legacy: false, locale: 'en', missingWarn: false, fallbackWarn: false, messages: { en: {} } })
        ]
      }
    })

    await wrapper.setProps({ accountId: 2 })
    resolveSecond([{ id: 'current-model' }])
    await flushPromises()
    resolveFirst([{ id: 'stale-model' }])
    await flushPromises()

    expect(wrapper.findComponent(ModelWhitelistSelector).props('candidateModels')).toEqual(['current-model'])
  })

  it('clears the previous account catalog while the next account is loading', async () => {
    let resolveSecond!: (value: { id: string }[]) => void
    getAvailableModels.mockImplementation((accountId: number) => accountId === 1
      ? Promise.resolve([{ id: 'previous-model' }])
      : new Promise<{ id: string }[]>(resolve => { resolveSecond = resolve }))
    const draft = newBPSAccountDraft()
    const wrapper = mount(OpenAIBPSAccountFields, {
      props: { modelValue: draft, editing: true, accountId: 1 },
      global: {
        plugins: [
          createPinia(),
          createI18n({ legacy: false, locale: 'en', missingWarn: false, fallbackWarn: false, messages: { en: {} } })
        ]
      }
    })
    await flushPromises()
    expect(wrapper.findComponent(ModelWhitelistSelector).props('candidateModels')).toEqual(['previous-model'])

    await wrapper.setProps({ accountId: 2 })
    expect(wrapper.findComponent(ModelWhitelistSelector).props('candidateModels')).toEqual([])

    resolveSecond([{ id: 'current-model' }])
    await flushPromises()
    expect(wrapper.findComponent(ModelWhitelistSelector).props('candidateModels')).toEqual(['current-model'])
  })
})
