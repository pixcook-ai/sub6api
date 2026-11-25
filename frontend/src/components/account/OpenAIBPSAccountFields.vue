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
    <div>
      <label class="input-label">{{ t('admin.accounts.modelMapping') }}</label>
      <ModelWhitelistSelector v-model="selectedModels" platform="openai" />
      <p class="input-hint">{{ t('admin.accounts.bps.modelsHint') }}</p>
    </div>
  </section>
</template>
<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import ModelWhitelistSelector from './ModelWhitelistSelector.vue'
import type { BPSAccountDraft, BPSCredentialState } from '@/utils/openaiBps'
import { useBPSCredentialState } from '@/composables/useBPSCredentialState'
import BPSCredentialStatus from './BPSCredentialStatus.vue'
const props = defineProps<{ editing?: boolean; expiresAt?: string; credentialState?: BPSCredentialState; accountStatus?: string; schedulable?: boolean }>()
const { state: savedState } = useBPSCredentialState(() => ({ platform: 'openai_bps', credentials: { expires_at: props.expiresAt }, bps_credential_state: props.credentialState, status: props.accountStatus, schedulable: props.schedulable }))
const draft = defineModel<BPSAccountDraft>({ required: true })
const { t } = useI18n()
// Same selector as the reference repo; each chosen model maps to itself unless a mapping already exists.
const selectedModels = computed<string[]>({
  get: () => draft.value.models.map(row => row.from).filter(Boolean),
  set: (value) => {
    draft.value.models = value.map(from => ({ from, to: draft.value.models.find(row => row.from === from)?.to || from }))
  }
})
</script>
