<template>
  <section class="card" aria-labelledby="tibo-settings-title" data-testid="tibo-settings">
    <div class="border-b border-gray-100 px-6 py-4 dark:border-dark-700">
      <h2 id="tibo-settings-title" class="text-lg font-semibold text-gray-900 dark:text-white">{{ t('admin.settings.tibo.title') }}</h2>
      <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">{{ t('admin.settings.tibo.description') }}</p>
    </div>
    <div class="space-y-5 p-6" :aria-busy="loading || saving">
      <p v-if="loading" role="status" class="text-sm text-gray-500">{{ t('common.loading') }}</p>
      <template v-else>
        <p v-if="error" role="alert" class="text-sm text-red-600 dark:text-red-400">{{ error }}</p>
        <p v-if="saved" role="status" class="text-sm text-emerald-700 dark:text-emerald-400">{{ t('admin.settings.tibo.saved') }}</p>
        <button v-if="!loaded" type="button" class="btn btn-secondary btn-sm" @click="load">{{ t('admin.settings.tibo.retry') }}</button>
        <fieldset v-else class="space-y-5" :disabled="saving">
          <div class="flex items-center justify-between gap-4">
            <div>
              <label id="tibo-enabled-label" class="font-medium text-gray-900 dark:text-white">{{ t('admin.settings.tibo.enabled') }}</label>
              <p id="tibo-enabled-hint" class="mt-1 text-sm text-gray-500 dark:text-gray-400">{{ t('admin.settings.tibo.enabledHint') }}</p>
            </div>
            <Toggle v-model="form.enabled" aria-labelledby="tibo-enabled-label" aria-describedby="tibo-enabled-hint" data-testid="tibo-enabled" />
          </div>

          <div class="space-y-5 border-t border-gray-100 pt-4 dark:border-dark-700">
            <div class="flex items-center justify-between gap-4">
              <div>
                <label id="tibo-isolation-label" class="font-medium text-gray-900 dark:text-white">{{ t('admin.settings.tibo.pauseOnDegraded') }}</label>
                <p id="tibo-isolation-hint" class="mt-1 text-sm text-gray-500 dark:text-gray-400">{{ t('admin.settings.tibo.pauseHint') }}</p>
              </div>
              <Toggle v-model="form.pause_on_degraded" aria-labelledby="tibo-isolation-label" aria-describedby="tibo-isolation-hint" data-testid="tibo-isolation" />
            </div>
            <div class="flex items-center justify-between gap-4">
              <div>
                <label id="tibo-recovery-label" class="font-medium text-gray-900 dark:text-white">{{ t('admin.settings.tibo.autoResume') }}</label>
                <p id="tibo-recovery-hint" class="mt-1 text-sm text-gray-500 dark:text-gray-400">{{ t('admin.settings.tibo.autoResumeHint') }}</p>
              </div>
              <Toggle v-model="form.auto_resume_on_healthy" aria-labelledby="tibo-recovery-label" aria-describedby="tibo-recovery-hint" data-testid="tibo-auto-resume" />
            </div>
          </div>

          <div class="space-y-4 border-t border-gray-100 pt-4 dark:border-dark-700">
            <div class="flex items-center justify-between gap-4">
              <div>
                <label id="tibo-group-label" class="font-medium text-gray-900 dark:text-white">{{ t('admin.settings.tibo.autoGroup') }}</label>
                <p id="tibo-group-hint" class="mt-1 text-sm text-gray-500 dark:text-gray-400">{{ t('admin.settings.tibo.autoGroupHint') }}</p>
              </div>
              <Toggle v-model="form.auto_group_enabled" aria-labelledby="tibo-group-label" aria-describedby="tibo-group-hint" data-testid="tibo-auto-group" />
            </div>
            <div v-if="form.auto_group_enabled" class="grid gap-4 sm:grid-cols-2">
              <div v-for="field in groupFields" :key="field" class="min-w-0">
                <label :for="'tibo-' + field" class="input-label">{{ t('admin.settings.tibo.' + field) }}</label>
                <select :id="'tibo-' + field" v-model.number="form[field]" class="input w-full" :aria-invalid="!validGroups" :data-testid="field">
                  <option :value="0">{{ t('admin.settings.tibo.chooseGroup') }}</option>
                  <option v-if="form[field] > 0 && !groups.some(group => group.id === form[field])" :value="form[field]" disabled>
                    {{ t('admin.settings.tibo.unavailableGroup', { id: form[field] }) }}
                  </option>
                  <option v-for="group in groups" :key="group.id" :value="group.id">{{ group.name }} (#{{ group.id }})</option>
                </select>
              </div>
              <p v-if="groupError" role="alert" class="text-sm text-red-600 dark:text-red-400 sm:col-span-2">{{ groupError }}</p>
              <p v-if="!validGroups" class="text-sm text-amber-700 dark:text-amber-300 sm:col-span-2">{{ t('admin.settings.tibo.groupValidation') }}</p>
              <button v-if="groupError" type="button" class="btn btn-secondary btn-sm justify-self-start" @click="loadGroups">{{ t('admin.settings.tibo.retryGroups') }}</button>
            </div>
          </div>

          <p class="rounded-lg bg-gray-50 p-3 text-sm text-gray-600 dark:bg-dark-800 dark:text-gray-300">{{ t('admin.settings.tibo.signalCaution') }}</p>
          <div class="flex justify-end border-t border-gray-100 pt-4 dark:border-dark-700">
            <button type="button" class="btn btn-primary btn-sm" :disabled="saving || !canSave" data-testid="tibo-save" @click="save">
              {{ saving ? t('common.saving') : t('common.save') }}
            </button>
          </div>
        </fieldset>
      </template>
    </div>
  </section>
</template>

<script setup lang="ts">
import { computed, onMounted, reactive, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import Toggle from '@/components/common/Toggle.vue'
import { getAll as getGroups } from '@/api/admin/groups'
import { defaultTiboSettings, getTiboSettings, updateTiboSettings } from '@/api/admin/tibo'
import type { AdminGroup } from '@/types'
import { extractApiErrorMessage } from '@/utils/apiError'

const { t } = useI18n()
const form = reactive(defaultTiboSettings())
const loading = ref(true)
const loaded = ref(false)
const saving = ref(false)
const saved = ref(false)
const error = ref('')
const groupError = ref('')
const groups = ref<AdminGroup[]>([])
const groupFields = ['healthy_group_id', 'degraded_group_id'] as const
const validGroups = computed(() => (
  form.healthy_group_id !== form.degraded_group_id &&
  groupFields.every(field => groups.value.some(group => group.id === form[field]))
))
const canSave = computed(() => loaded.value && (!form.auto_group_enabled || validGroups.value))

async function loadGroups() {
  groupError.value = ''
  try {
    groups.value = (await getGroups('openai')).filter(group => group.platform === 'openai' && group.status === 'active')
  } catch (err) {
    groups.value = []
    groupError.value = extractApiErrorMessage(err, t('admin.settings.tibo.groupsLoadFailed'))
  }
}

async function load() {
  loading.value = true
  error.value = ''
  try {
    const [settings] = await Promise.all([getTiboSettings(), loadGroups()])
    Object.assign(form, defaultTiboSettings(), settings)
    loaded.value = true
  } catch (err) {
    loaded.value = false
    error.value = extractApiErrorMessage(err, t('admin.settings.tibo.loadFailed'))
  } finally {
    loading.value = false
  }
}

async function save() {
  if (!canSave.value || saving.value) return
  saving.value = true
  saved.value = false
  error.value = ''
  try {
    await updateTiboSettings({ ...form })
    saved.value = true
  } catch (err) {
    error.value = extractApiErrorMessage(err, t('admin.settings.tibo.saveFailed'))
  } finally {
    saving.value = false
  }
}

watch(form, () => { saved.value = false })

onMounted(load)
</script>
