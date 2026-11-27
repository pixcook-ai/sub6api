<template>
  <section class="space-y-3 border-t border-gray-200 pt-4 dark:border-dark-600" aria-labelledby="tibo-account-title">
    <h3 id="tibo-account-title" class="input-label mb-0">{{ t('admin.accounts.openai.codexTiboRoutes') }}</h3>
    <p class="text-xs text-gray-500 dark:text-gray-400">{{ t(account.platform === 'openai_bps' ? 'admin.accounts.openai.bpsTiboRouteDesc' : 'admin.accounts.openai.codexTiboRouteDesc') }}</p>
    <div v-if="account.extra?.openai_tibo_quality_isolated === true" class="space-y-2 rounded-lg border border-red-200 bg-red-50 p-3 dark:border-red-900 dark:bg-red-900/20" data-testid="tibo-isolation-panel">
      <p class="text-sm font-medium text-red-700 dark:text-red-300">{{ t('admin.accounts.openai.tiboQualityIsolated') }}</p>
      <p class="text-xs text-red-700 dark:text-red-300">{{ t('admin.accounts.openai.tiboIsolationHint') }}</p>
      <p v-if="!account.schedulable" class="text-xs text-red-700 dark:text-red-300">{{ t('admin.accounts.openai.tiboManualPauseRetained') }}</p>
      <button type="button" class="btn btn-secondary btn-sm" :disabled="clearing" data-testid="tibo-clear-isolation" @click="clearIsolation">
        {{ clearing ? t('common.loading') : t('admin.accounts.openai.tiboClearIsolation') }}
      </button>
      <p v-if="clearError" role="alert" class="text-sm text-red-700 dark:text-red-300">{{ clearError }}</p>
    </div>
    <div v-if="routes.length" class="space-y-3" data-testid="tibo-routes">
      <div v-for="route in routes" :key="route.model + ':' + route.route" class="space-y-1">
        <TiboRouteSpark :model="route.model" :verdict="route.verdict" :samples="route.recent_samples" :compact="false" />
        <p class="text-xs text-gray-600 dark:text-gray-300">{{ t('admin.accounts.openai.codexTiboRouteSummary', { route: route.route, verdict: t('admin.accounts.openai.codexTiboRouteVerdict.' + route.verdict) }) }}</p>
        <p v-if="route.last_sample" class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.accounts.openai.tiboLatestSample', { sample: t('admin.accounts.openai.codexTiboRouteVerdict.' + route.last_sample) }) }}</p>
        <p v-if="route.checked_at" class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.accounts.openai.codexTiboRouteCheckedAt', { time: formatDateTime(new Date(route.checked_at)) }) }}</p>
        <p v-if="route.next_probe_at" class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.accounts.openai.codexTiboRouteNextProbeAt', { time: formatDateTime(new Date(route.next_probe_at)) }) }}</p>
      </div>
    </div>
    <p v-else class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.accounts.openai.tiboNoProbeYet') }}</p>
    <p class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.accounts.openai.tiboUnknownHint') }}</p>
  </section>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import type { Account } from '@/types'
import { clearTiboIsolation } from '@/api/admin/tibo'
import { extractApiErrorMessage } from '@/utils/apiError'
import { formatDateTime } from '@/utils/format'
import TiboRouteSpark from './TiboRouteSpark.vue'

const props = defineProps<{ account: Account }>()
const emit = defineEmits<{ updated: [account: Account] }>()
const { t } = useI18n()
const clearing = ref(false)
const clearError = ref('')
const routes = computed(() => (props.account.tibo_account_statuses ?? []).flatMap(ticket =>
  (ticket.tibo_routes ?? []).map(route => ({ ...route, model: ticket.model }))
))

watch(() => props.account.id, () => { clearError.value = '' })

async function clearIsolation() {
  if (clearing.value) return
  const accountId = props.account.id
  clearing.value = true
  clearError.value = ''
  try {
    const account = await clearTiboIsolation(accountId)
    if (props.account.id === accountId) emit('updated', account)
  } catch (error) {
    if (props.account.id === accountId) clearError.value = extractApiErrorMessage(error, t('admin.accounts.openai.tiboClearFailed'))
  } finally {
    clearing.value = false
  }
}
</script>
