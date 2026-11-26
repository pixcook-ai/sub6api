<template>
  <div class="space-y-4">
    <div
      class="rounded-lg border border-blue-300 bg-white/80 p-4 dark:border-blue-600 dark:bg-gray-800/80"
    >
      <p class="mb-3 text-sm text-blue-700 dark:text-blue-300">
        {{ t('admin.accounts.oauth.openai.batchLoginDesc') }}
      </p>

      <template v-if="!job">
        <label class="mb-2 block text-sm font-semibold text-gray-700 dark:text-gray-300">
          {{ t('admin.accounts.oauth.openai.batchLoginInputLabel') }}
        </label>
        <textarea
          v-model="text"
          rows="6"
          class="input w-full resize-y font-mono text-sm"
          :placeholder="t('admin.accounts.oauth.openai.batchLoginPlaceholder')"
          spellcheck="false"
          autocomplete="off"
          autocapitalize="off"
          :disabled="starting"
        ></textarea>
        <p class="mt-1 text-xs text-blue-600 dark:text-blue-400">
          {{ t('admin.accounts.oauth.openai.batchLoginHint') }}
        </p>

        <fieldset class="mt-4 border-0 p-0">
          <legend class="mb-2 text-sm font-semibold text-gray-700 dark:text-gray-300">
            {{ t('admin.accounts.oauth.openai.batchLoginTargets') }}
          </legend>
          <div class="flex flex-wrap gap-4">
            <label class="flex cursor-pointer items-center gap-2">
              <input v-model="targets" type="checkbox" value="bps" class="rounded text-blue-600" />
              <span class="text-sm">{{ t('admin.accounts.oauth.openai.batchLoginTargetBps') }}</span>
            </label>
            <label class="flex cursor-pointer items-center gap-2">
              <input v-model="targets" type="checkbox" value="codex" class="rounded text-blue-600" />
              <span class="text-sm">{{ t('admin.accounts.oauth.openai.batchLoginTargetCodex') }}</span>
            </label>
          </div>
        </fieldset>

        <div v-if="targets.includes('bps')" class="mt-4">
          <label class="mb-1 block text-sm font-semibold text-gray-700 dark:text-gray-300">
            {{ t('admin.accounts.oauth.openai.batchLoginBpsGroups') }}
          </label>
          <GroupSelector v-model="bpsGroupIds" :groups="bpsGroups" platform="openai_bps" />
        </div>
        <div v-if="targets.includes('codex')" class="mt-4">
          <label class="mb-1 block text-sm font-semibold text-gray-700 dark:text-gray-300">
            {{ t('admin.accounts.oauth.openai.batchLoginCodexGroups') }}
          </label>
          <GroupSelector v-model="codexGroupIds" :groups="codexGroups" platform="openai" />
        </div>

        <label class="mt-4 flex cursor-pointer items-start gap-2">
          <input v-model="saveCredentials" type="checkbox" class="mt-0.5 rounded text-blue-600" />
          <span class="text-sm">
            {{ t('admin.accounts.oauth.openai.batchLoginSaveCredentials') }}
            <span class="block text-xs text-blue-600 dark:text-blue-400">{{ t('admin.accounts.oauth.openai.batchLoginSaveCredentialsHint') }}</span>
          </span>
        </label>
        <p v-if="errorText" role="alert" class="mt-3 text-sm text-red-600 dark:text-red-400">
          {{ errorText }}
        </p>
        <button
          type="button"
          class="btn btn-primary mt-4 w-full"
          :disabled="starting || !text.trim() || !targets.length"
          @click="start"
        >
          {{ t('admin.accounts.oauth.openai.batchLoginStart') }}
        </button>
      </template>

      <template v-else>
        <p class="mb-3 text-sm font-medium text-gray-700 dark:text-gray-300" aria-live="polite">
          <template v-if="job.status === 'running'">{{ t('admin.accounts.oauth.openai.batchLoginRunning') }}</template>
          <template v-else-if="job.status === 'canceled'">{{ t('admin.accounts.oauth.openai.batchLoginCanceled') }}</template>
          <template v-else>{{ summaryText }}</template>
        </p>
        <ul class="max-h-72 space-y-1 overflow-y-auto text-sm" aria-live="polite">
          <li v-for="item in job.items" :key="item.email + item.target" class="flex justify-between gap-4">
            <span class="truncate">{{ item.email }} · {{ targetLabel(item.target) }}</span>
            <span :class="statusClass(item.status)">
              {{ t(`admin.accounts.oauth.openai.batchLoginItemStatus.${item.status}`) }}<template v-if="item.code"> · {{ codeLabel(item.code) }}</template>
            </span>
          </li>
        </ul>
        <p v-if="errorText" role="alert" class="mt-3 text-sm text-red-600 dark:text-red-400">
          {{ errorText }}
        </p>
        <div class="mt-4 flex gap-3">
          <button v-if="job.status === 'running'" type="button" class="btn btn-secondary" @click="cancel">
            {{ t('admin.accounts.oauth.openai.batchLoginCancel') }}
          </button>
          <button v-else type="button" class="btn btn-secondary" @click="reset">
            {{ t('common.back') }}
          </button>
        </div>
      </template>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import GroupSelector from '@/components/common/GroupSelector.vue'
import { adminAPI } from '@/api/admin'
import {
  BatchLoginParseFailure,
  cancelBatchLogin,
  getBatchLogin,
  isBatchLoginJobFinished,
  parseBatchLoginText,
  startBatchLogin
} from '@/api/admin/openaiBatchLogin'
import type { BatchLoginItemStatus, BatchLoginJob, BatchLoginTarget } from '@/api/admin/openaiBatchLogin'
import type { AdminGroup } from '@/types'

const props = withDefaults(
  defineProps<{
    proxyId?: number | null
    concurrency?: number
    priority?: number
  }>(),
  { proxyId: null, concurrency: 10, priority: 1 }
)
const emit = defineEmits<{ created: []; busy: [value: boolean] }>()

const { t } = useI18n()
const POLL_INTERVAL_MS = 2000

const text = ref('')
const targets = ref<BatchLoginTarget[]>(['bps'])
const bpsGroupIds = ref<number[]>([])
const codexGroupIds = ref<number[]>([])
const saveCredentials = ref(false)
const bpsGroups = ref<AdminGroup[]>([])
const codexGroups = ref<AdminGroup[]>([])
const job = ref<BatchLoginJob | null>(null)
const starting = ref(false)
const errorText = ref('')
let pollTimer: ReturnType<typeof setTimeout> | null = null
let disposed = false
let notifiedCreated = false

const summaryText = computed(() => {
  const items = job.value?.items ?? []
  const count = (status: BatchLoginItemStatus) => items.filter((i) => i.status === status).length
  return t('admin.accounts.oauth.openai.batchLoginDone', {
    created: count('created'),
    skipped: count('skipped'),
    failed: count('failed')
  })
})

function targetLabel(target: BatchLoginTarget): string {
  return t(
    target === 'bps'
      ? 'admin.accounts.oauth.openai.batchLoginTargetBps'
      : 'admin.accounts.oauth.openai.batchLoginTargetCodex'
  )
}

function codeLabel(code: string): string {
  const transportFailures = ['proxy_error', 'tls_error', 'risk_control']
  const transportFailure = transportFailures.find((failure) => code.endsWith(`_${failure}`))
  if (transportFailure) {
    const stage = code.slice(0, -(transportFailure.length + 1))
    const stageKey = `admin.accounts.oauth.openai.batchLoginFailureStages.${stage}`
    const stageLabel = t(stageKey)
    if (stageLabel !== stageKey) {
      return `${stageLabel}: ${t(`admin.accounts.oauth.openai.batchLoginItemCodes.${transportFailure}`)}`
    }
  }
  const key = `admin.accounts.oauth.openai.batchLoginItemCodes.${code}`
  const label = t(key)
  return label === key ? code : label
}

function statusClass(status: BatchLoginItemStatus): string {
  if (status === 'created') return 'text-green-600 dark:text-green-400'
  if (status === 'failed') return 'text-red-600 dark:text-red-400'
  return 'text-gray-500 dark:text-gray-400'
}

async function loadGroups() {
  try {
    const [bps, codex] = await Promise.all([
      adminAPI.groups.getAll('openai_bps'),
      adminAPI.groups.getAll('openai')
    ])
    if (disposed) return
    bpsGroups.value = bps
    codexGroups.value = codex
  } catch {
    // Group selection is optional; the backend falls back to platform defaults.
  }
}

function stopPolling() {
  if (pollTimer) clearTimeout(pollTimer)
  pollTimer = null
}

function schedulePoll(id: string) {
  stopPolling()
  pollTimer = setTimeout(() => void poll(id), POLL_INTERVAL_MS)
}

async function poll(id: string) {
  if (disposed) return
  try {
    const next = await getBatchLogin(id)
    if (disposed) return
    job.value = next
    errorText.value = ''
    if (isBatchLoginJobFinished(next)) {
      emit('busy', false)
      if (!notifiedCreated && next.items.some((i) => i.status === 'created')) {
        notifiedCreated = true
        emit('created')
      }
      return
    }
  } catch {
    if (disposed) return
    errorText.value = t('admin.accounts.oauth.openai.batchLoginPollFailed')
  }
  schedulePoll(id)
}

async function start() {
  if (starting.value) return
  errorText.value = ''
  let rows
  try {
    rows = parseBatchLoginText(text.value)
  } catch (error) {
    const code = error instanceof BatchLoginParseFailure ? error.code : 'invalid_row'
    errorText.value = t(`admin.accounts.oauth.openai.batchLoginErrors.${code}`)
    return
  }
  starting.value = true
  emit('busy', true)
  try {
    const started = await startBatchLogin({
      text: rows.map((r) => `${r.email}----${r.password}----${r.totp}`).join('\n'),
      targets: targets.value,
      proxy_id: props.proxyId,
      concurrency: props.concurrency,
      priority: props.priority,
      bps_group_ids: targets.value.includes('bps') ? bpsGroupIds.value : [],
      codex_group_ids: targets.value.includes('codex') ? codexGroupIds.value : [],
      save_credentials: saveCredentials.value
    })
    // Secrets must not linger in the UI once the server owns them.
    text.value = ''
    notifiedCreated = false
    job.value = started
    schedulePoll(started.id)
  } catch {
    errorText.value = t('admin.accounts.oauth.openai.batchLoginStartFailed')
    emit('busy', false)
  } finally {
    starting.value = false
  }
}

async function cancel() {
  if (!job.value) return
  try {
    await cancelBatchLogin(job.value.id)
  } catch {
    // The next poll reports the real state.
  }
}

function reset() {
  stopPolling()
  job.value = null
  errorText.value = ''
  emit('busy', false)
}

onMounted(() => void loadGroups())
onBeforeUnmount(() => {
  disposed = true
  stopPolling()
  text.value = ''
})
</script>
