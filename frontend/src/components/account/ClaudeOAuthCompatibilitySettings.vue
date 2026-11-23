<template>
  <section class="rounded-lg border border-gray-200 p-4 dark:border-dark-600">
    <h3 class="input-label">{{ t(`${prefix}.title`) }}</h3>
    <p class="text-sm text-gray-700 dark:text-gray-300" data-testid="claude-compatibility-mode">{{ t(`${prefix}.transparentMode`) }}</p>
    <p class="input-hint">{{ t(`${prefix}.hint`) }}</p>
    <template v-if="accountId">
      <div class="mt-4 flex items-center justify-between gap-3">
        <h4 class="text-sm font-medium text-gray-900 dark:text-white">{{ t(`${prefix}.latest`) }}</h4>
        <button type="button" class="btn btn-secondary btn-sm" data-testid="claude-compatibility-refresh" :disabled="loading" @click="refresh">{{ t(loading ? 'common.loading' : 'common.refresh') }}</button>
      </div>
      <p class="mt-1 text-xs text-gray-500 dark:text-gray-400">{{ t(`${prefix}.cacheHint`) }}</p>
      <p v-if="error" role="alert" class="mt-2 text-sm text-red-600 dark:text-red-400">{{ t(`${prefix}.loadFailed`) }}</p>
      <p v-else-if="!loading && !observation" class="mt-2 text-sm text-gray-500" data-testid="claude-compatibility-empty">{{ t(`${prefix}.empty`) }}</p>
      <div v-if="observation" class="mt-3 text-xs" data-testid="claude-compatibility-observation">
        <dl class="grid grid-cols-2 gap-x-4 gap-y-2">
          <dt>{{ t(`${prefix}.observedAt`) }}</dt><dd>{{ observation.observed_at }}</dd>
          <dt>{{ t(`${prefix}.endpoint`) }}</dt><dd>{{ observation.endpoint }}</dd>
          <dt>{{ t(`${prefix}.clientVersion`) }}</dt><dd>{{ observation.client_version || '—' }}</dd>
          <dt>{{ t(`${prefix}.observedMode`) }}</dt><dd>{{ observation.mode }}</dd>
          <dt>{{ t(`${prefix}.blocks`) }}</dt><dd>{{ observation.incoming_attribution_blocks }} → {{ observation.outgoing_attribution_blocks }}</dd>
          <dt>cc_gateway / cch</dt><dd>{{ yesNo(observation.cc_gateway_present) }} / {{ yesNo(observation.cch_present) }}</dd>
          <dt>{{ t(`${prefix}.unknownFields`) }}</dt><dd>{{ observation.unknown_attribution_fields }}</dd>
          <dt>wiggly</dt><dd>{{ headerState(observation.wiggly_present, observation.wiggly_forwarded) }}</dd>
          <dt>additional-protection</dt><dd>{{ headerState(observation.additional_protection_present, observation.additional_protection_forwarded) }}</dd>
          <dt>{{ t(`${prefix}.bodyChanged`) }}</dt><dd>{{ yesNo(observation.body_changed) }}</dd>
          <dt>{{ t(`${prefix}.metadataChanged`) }}</dt><dd>{{ yesNo(observation.metadata_changed) }}</dd>
          <dt>{{ t(`${prefix}.betaChanged`) }}</dt><dd>{{ yesNo(observation.beta_changed) }}</dd>
        </dl>
        <p class="mt-3 text-amber-700 dark:text-amber-400">{{ t(`${prefix}.unverified`) }}</p>
        <ul v-if="observation.reasons?.length" class="mt-2 list-disc space-y-1 pl-4">
          <li v-for="reason in observation.reasons" :key="reason">{{ reasonLabel(reason) }}</li>
        </ul>
      </div>
    </template>
  </section>
</template>

<script setup lang="ts">
import { onUnmounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { adminAPI } from '@/api/admin'
import type { ClaudeOAuthCompatibilityObservation } from '@/api/admin/accounts'

const props = withDefaults(defineProps<{ accountId: number; active?: boolean }>(), { active: true })
const { t } = useI18n()
const prefix = 'admin.accounts.claudeCompatibility'
const loading = ref(false)
const error = ref(false)
const observation = ref<ClaudeOAuthCompatibilityObservation | null>(null)
let requestVersion = 0

const yesNo = (value: boolean) => t(`${prefix}.${value ? 'yes' : 'no'}`)
function headerState(present: boolean, forwarded: boolean) {
  return t(`${prefix}.${!present ? 'absent' : forwarded ? 'forwarded' : 'notForwarded'}`)
}
function reasonLabel(reason: string) {
  const known: Record<string, string> = {
    attribution_synthesized: 'reasonSynthesized',
    wiggly_dropped_by_allowlist: 'reasonAllowlist',
    opaque_metadata_unverified: 'reasonOpaqueUnverified',
    noncanonical_attribution: 'reasonNoncanonical',
    unknown_attribution_fields: 'reasonUnknownFields'
  }
  return t(`${prefix}.${known[reason] || 'reasonUnknown'}`)
}
async function refresh() {
  const version = ++requestVersion
  observation.value = null
  error.value = false
  if (!props.accountId || !props.active) { loading.value = false; return }
  loading.value = true
  try {
    const result = await adminAPI.accounts.getClaudeOAuthCompatibility(props.accountId)
    if (version === requestVersion) observation.value = result.observation
  } catch {
    if (version === requestVersion) error.value = true
  } finally {
    if (version === requestVersion) loading.value = false
  }
}
watch(() => [props.accountId, props.active], refresh, { immediate: true })
onUnmounted(() => { requestVersion++ })
</script>
