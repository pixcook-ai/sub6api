<template>
  <section class="space-y-4 rounded-lg border border-gray-200 p-4 dark:border-dark-600">
    <h3 class="font-medium">OpenAI BPS · Access Token</h3>
    <div>
      <label for="bps-access-token" class="input-label">Access Token</label>
      <input id="bps-access-token" v-model="draft.token" type="password" autocomplete="new-password" class="input" :required="!editing" :placeholder="editing ? t('admin.accounts.bps.keepToken') : 'access_token'" />
      <p class="input-hint">{{ t('admin.accounts.bps.tokenHint') }}</p>
    </div>
    <div>
      <label for="bps-account-id" class="input-label">ChatGPT Account ID</label>
      <input id="bps-account-id" v-model="draft.accountId" class="input" :placeholder="t('admin.accounts.bps.accountHint')" />
      <p class="input-hint">{{ t('admin.accounts.bps.accountHint') }}</p>
    </div>
    <BPSCredentialStatus v-if="editing" :state="savedState" />
    <p v-if="editing && draft.token.trim()" class="input-hint">{{ t('admin.accounts.bps.newTokenPending') }}</p>
    <div class="space-y-3">
      <h4 class="input-label">{{ t('admin.accounts.modelWhitelist') }}</h4>
      <ModelWhitelistSelector
        v-model="draft.allowedModels"
        platform="openai"
        :candidate-models="whitelistCandidates"
        :model-mappings="draft.modelMappings"
      />
      <p class="input-hint">
        <span v-if="draft.allowedModels.length === 0 && draft.modelMappings.length === 0">
          {{ t('admin.accounts.supportsAllModels') }}
        </span>
        <span v-else>{{ t('admin.accounts.bps.modelsHint') }}</span>
      </p>
      <h4 class="input-label">{{ t('admin.accounts.modelMapping') }}</h4>
      <div v-if="draft.modelMappings.length > 0" class="space-y-2">
        <div
          v-for="(mapping, index) in draft.modelMappings"
          :key="index"
          class="flex items-center gap-2"
        >
          <input
            v-model="mapping.from"
            type="text"
            class="input min-w-0 flex-1"
            :aria-label="t('admin.accounts.bps.clientModel')"
            :placeholder="t('admin.accounts.bps.clientModel')"
            data-testid="bps-model-mapping-source"
            @change="handleMappingSourceChange(index)"
          />
          <span aria-hidden="true" class="text-gray-400">→</span>
          <input
            v-model="mapping.to"
            type="text"
            class="input min-w-0 flex-1"
            :aria-label="t('admin.accounts.bps.upstreamModel')"
            :placeholder="t('admin.accounts.bps.upstreamModel')"
            data-testid="bps-model-mapping-target"
          />
          <button
            type="button"
            class="rounded-lg p-2 text-red-500 transition-colors hover:bg-red-50 hover:text-red-600 dark:hover:bg-red-900/20"
            :aria-label="t('common.delete')"
            data-testid="remove-bps-model-mapping"
            @click="draft.modelMappings.splice(index, 1)"
          >
            <svg class="h-4 w-4" fill="none" viewBox="0 0 24 24" stroke="currentColor">
              <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M19 7l-.867 12.142A2 2 0 0116.138 21H7.862a2 2 0 01-1.995-1.858L5 7m5 4v6m4-6v6m1-10V4a1 1 0 00-1-1h-4a1 1 0 00-1 1v3M4 7h16" />
            </svg>
          </button>
        </div>
      </div>
      <button
        type="button"
        class="w-full rounded-lg border-2 border-dashed border-gray-300 px-4 py-2 text-sm text-gray-600 transition-colors hover:border-gray-400 hover:text-gray-700 dark:border-dark-500 dark:text-gray-400 dark:hover:border-dark-400 dark:hover:text-gray-300"
        data-testid="add-model-mapping"
        @click="draft.modelMappings.push({ from: '', to: '' })"
      >
        {{ t('admin.accounts.addMapping') }}
      </button>
    </div>
  </section>
</template>
<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { adminAPI } from '@/api/admin'
import { useAppStore } from '@/stores/app'
import ModelWhitelistSelector from './ModelWhitelistSelector.vue'
import { BPS_DEFAULT_MODELS, type BPSAccountDraft, type BPSCredentialState } from '@/utils/openaiBps'
import { useBPSCredentialState } from '@/composables/useBPSCredentialState'
import BPSCredentialStatus from './BPSCredentialStatus.vue'
const props = defineProps<{ accountId?: number; editing?: boolean; expiresAt?: string; credentialState?: BPSCredentialState; accountStatus?: string; schedulable?: boolean }>()
const { state: savedState } = useBPSCredentialState(() => ({ platform: 'openai_bps', credentials: { expires_at: props.expiresAt }, bps_credential_state: props.credentialState, status: props.accountStatus, schedulable: props.schedulable }))
const appStore = useAppStore()
const draft = defineModel<BPSAccountDraft>({ required: true })
const { t } = useI18n()
// Models the BPS upstream says this account may use; empty until loaded or when unavailable.
const upstreamModels = ref<string[]>([])
let modelsRequestVersion = 0
const loadUpstreamModels = async (accountId = props.accountId) => {
  const requestVersion = ++modelsRequestVersion
  upstreamModels.value = []
  if (!accountId) {
    return
  }
  try {
    const models = await adminAPI.accounts.getAvailableModels(accountId, 'upstream')
    if (requestVersion !== modelsRequestVersion) return
    upstreamModels.value = models.map(model => model.id).filter(Boolean)
  } catch {
    if (requestVersion === modelsRequestVersion) upstreamModels.value = []
  }
}
onMounted(() => void loadUpstreamModels())
watch(() => props.accountId, accountId => void loadUpstreamModels(accountId))
onUnmounted(() => {
  modelsRequestVersion += 1
})
const whitelistCandidates = computed(() => {
  const candidates = props.accountId ? upstreamModels.value : [...BPS_DEFAULT_MODELS]
  const mappedSources = new Set(draft.value.modelMappings.map(mapping => mapping.from.trim()).filter(Boolean))
  return candidates.filter(model => !mappedSources.has(model))
})
const handleMappingSourceChange = (index: number) => {
  const mapping = draft.value.modelMappings[index]
  if (!mapping) return

  const source = mapping.from.trim()
  if (!source) return

  const duplicate = draft.value.modelMappings.find((other, otherIndex) =>
    otherIndex !== index && other.from.trim() === source
  )
  if (duplicate) {
    appStore.showInfo(t('admin.accounts.modelExists'))
    mapping.from = ''
  }
}
</script>
