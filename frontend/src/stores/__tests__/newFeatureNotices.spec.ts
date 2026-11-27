import { createPinia, setActivePinia } from 'pinia'
import { beforeEach, describe, expect, it } from 'vitest'
import { NEW_FEATURE_NOTICES } from '@/constants/newFeatureNotices'
import { useNewFeatureNoticeStore } from '../newFeatureNotices'

describe('new feature notice preferences', () => {
  beforeEach(() => {
    localStorage.clear()
    setActivePinia(createPinia())
  })

  it('persists dismissed notice IDs per admin account', () => {
    const store = useNewFeatureNoticeStore()
    expect(store.isDismissed(41, 'protocol-switches')).toBe(false)

    store.dismiss(41, 'protocol-switches')

    expect(store.isDismissed(41, 'protocol-switches')).toBe(true)
    expect(store.isDismissed(42, 'protocol-switches')).toBe(false)
    expect(localStorage.getItem('sub2api:new-feature-notices:v1:41')).toBe('["protocol-switches"]')

    setActivePinia(createPinia())
    expect(useNewFeatureNoticeStore().isDismissed(41, 'protocol-switches')).toBe(true)
  })

  it('ignores malformed stored preferences and does not overwrite other notices', () => {
    localStorage.setItem('sub2api:new-feature-notices:v1:41', '{bad json')
    const store = useNewFeatureNoticeStore()

    expect(store.isDismissed(41, 'protocol-switches')).toBe(false)
    store.dismiss(41, 'protocol-switches')
    store.dismiss(41, 'other-feature')

    expect(JSON.parse(localStorage.getItem('sub2api:new-feature-notices:v1:41') ?? '[]')).toEqual([
      'protocol-switches',
      'other-feature',
    ])
  })
  it('announces unified account routing independently and preserves dismissals across reloads', () => {
    const store = useNewFeatureNoticeStore()
    store.dismiss(41, NEW_FEATURE_NOTICES.bpsAccountMultiProxy)
    expect(store.isDismissed(41, NEW_FEATURE_NOTICES.openaiAccountMultiProxy)).toBe(false)
    store.dismiss(41, NEW_FEATURE_NOTICES.openaiAccountMultiProxy)
    expect(store.isDismissed(41, NEW_FEATURE_NOTICES.openaiAccountMultiProxy)).toBe(true)
    setActivePinia(createPinia())
    const reloaded = useNewFeatureNoticeStore()
    expect(reloaded.isDismissed(41, NEW_FEATURE_NOTICES.bpsAccountMultiProxy)).toBe(true)
    expect(reloaded.isDismissed(41, NEW_FEATURE_NOTICES.openaiAccountMultiProxy)).toBe(true)
    expect(reloaded.isDismissed(41, 'future-key-risk-feature')).toBe(false)
    expect(reloaded.isDismissed(42, NEW_FEATURE_NOTICES.openaiAccountMultiProxy)).toBe(false)
  })
})
