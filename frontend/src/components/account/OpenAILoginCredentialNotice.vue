<template>
  <div
    v-if="saved"
    class="rounded-lg border border-blue-200 bg-blue-50 p-3 text-sm dark:border-blue-700 dark:bg-blue-900/30"
    data-testid="login-credential-notice"
  >
    <p class="text-blue-800 dark:text-blue-200">
      {{ t('admin.accounts.oauth.openai.loginCredentialSaved') }}
    </p>
    <button
      type="button"
      class="btn btn-secondary mt-2"
      :disabled="busy"
      data-testid="login-credential-delete"
      @click="remove"
    >
      {{ t('admin.accounts.oauth.openai.loginCredentialDelete') }}
    </button>
  </div>
</template>

<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useAppStore } from '@/stores/app'
import { deleteLoginCredential, getLoginCredentialStatus } from '@/api/admin/openaiBatchLogin'

const props = defineProps<{ accountId: number }>()
const { t } = useI18n()
const appStore = useAppStore()
const saved = ref(false)
const busy = ref(false)

onMounted(async () => {
  try {
    saved.value = (await getLoginCredentialStatus(props.accountId)).saved
  } catch {
    // Status is informational; hide the notice when it cannot be read.
    saved.value = false
  }
})

async function remove() {
  if (busy.value) return
  busy.value = true
  try {
    await deleteLoginCredential(props.accountId)
    saved.value = false
    appStore.showSuccess(t('admin.accounts.oauth.openai.loginCredentialDeleted'))
  } catch {
    appStore.showError(t('admin.accounts.oauth.openai.loginCredentialDeleteFailed'))
  } finally {
    busy.value = false
  }
}
</script>
