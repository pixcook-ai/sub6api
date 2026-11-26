<template>
  <div
    v-if="state"
    :class="compact ? 'inline-flex max-w-full text-xs' : 'max-w-[240px] space-y-1 break-words text-xs'"
    data-testid="bps-credential-status"
    aria-live="polite"
  >
    <HelpTooltip v-if="compact" :content="details" trigger="click" class="!ml-0">
      <template #trigger>
        <button
          type="button"
          :class="failed ? 'badge badge-danger whitespace-nowrap text-xs' : 'whitespace-nowrap text-gray-500 dark:text-gray-400'"
          :title="details"
        >
          {{ t(`admin.accounts.bps.credentialStatusShort.${state.status}`) }}
        </button>
      </template>
    </HelpTooltip>
    <template v-else>
      <span :class="failed ? 'badge badge-danger text-xs' : 'text-gray-500 dark:text-gray-400'">
        {{ t(`admin.accounts.bps.credentialStatus.${state.status}`) }}
      </span>
      <p v-if="showExpiry && state.expires_at" class="text-gray-500 dark:text-gray-400">
        {{ t('admin.accounts.bps.expiresAt') }}: {{ formatDateTime(state.expires_at) }}
      </p>
      <p v-if="failed" class="font-medium text-red-600 dark:text-red-400">{{ t('admin.accounts.bps.replaceToken') }}</p>
      <p v-if="state.error_code" class="text-gray-500 dark:text-gray-400">{{ t('admin.accounts.bps.errorCode') }}: {{ state.error_code }}</p>
      <p v-if="state.requires_manual_resume" class="text-amber-600 dark:text-amber-400">{{ t('admin.accounts.bps.manualResume') }}</p>
    </template>
  </div>
</template>
<script setup lang="ts">
import { computed } from 'vue'
import HelpTooltip from '@/components/common/HelpTooltip.vue'
import { useI18n } from 'vue-i18n'
import { formatDateTime } from '@/utils/format'
import { isBPSCredentialFailure, type BPSCredentialState } from '@/utils/openaiBps'
const props = withDefaults(defineProps<{
  state?: BPSCredentialState | null
  showExpiry?: boolean
  compact?: boolean
}>(), { showExpiry: true, compact: false })
const { t } = useI18n()
const failed = computed(() => isBPSCredentialFailure(props.state))
const details = computed(() => {
  const state = props.state
  if (!state) return ''
  const lines = [t(`admin.accounts.bps.credentialStatus.${state.status}`)]
  if (props.showExpiry && state.expires_at) lines.push(`${t('admin.accounts.bps.expiresAt')}: ${formatDateTime(state.expires_at)}`)
  if (failed.value) lines.push(t('admin.accounts.bps.replaceToken'))
  if (state.error_code) lines.push(`${t('admin.accounts.bps.errorCode')}: ${state.error_code}`)
  if (state.requires_manual_resume) lines.push(t('admin.accounts.bps.manualResume'))
  return lines.join('\n')
})
</script>
