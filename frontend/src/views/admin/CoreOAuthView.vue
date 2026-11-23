<template>
  <AppLayout>
    <div class="space-y-6">
      <section class="flex flex-col gap-4 border-b border-gray-200 pb-5 dark:border-dark-700 sm:flex-row sm:items-end sm:justify-between">
        <div>
          <h2 class="text-lg font-semibold text-gray-900 dark:text-white">核心 OAuth 授权</h2>
          <p class="mt-1 max-w-3xl text-sm text-gray-500 dark:text-gray-400">
            Codex 与 Excel 分别授权，由 Sub6 安全保存和刷新。切换仅影响新会话，旧会话和在途请求保持原绑定。
          </p>
        </div>
        <button type="button" class="btn btn-secondary shrink-0" data-action="reload" :disabled="loading || mutationBusy" @click="loadProfiles">刷新状态</button>
      </section>
      <p v-if="loading" role="status" class="text-sm text-gray-500 dark:text-gray-400">正在读取授权状态…</p>
      <p v-if="loadError" role="alert" class="rounded-lg border border-red-200 bg-red-50 p-4 text-sm text-red-700 dark:border-red-900 dark:bg-red-950/30 dark:text-red-300">{{ loadError }} 请点击“刷新状态”重试。</p>
      <p v-else-if="!loading && !capabilities?.available" role="status" class="rounded-lg border border-amber-200 bg-amber-50 p-4 text-sm text-amber-800 dark:border-amber-900 dark:bg-amber-950/30 dark:text-amber-300">核心授权暂不可用，请管理员检查服务端加密与提供方配置后刷新。</p>
      <div class="grid gap-6 lg:grid-cols-2">
        <article v-for="provider in providers" :key="provider.id" :data-provider="provider.id" :aria-busy="busy[provider.id]" class="card space-y-4 border border-gray-200 p-5 dark:border-dark-700">
          <div class="flex flex-wrap items-center justify-between gap-2">
            <h3 class="font-semibold text-gray-900 dark:text-white">{{ provider.name }}</h3>
            <span class="rounded-full bg-gray-100 px-3 py-1 text-xs font-medium text-gray-700 dark:bg-dark-700 dark:text-gray-200">{{ profileStatus(selectedProfile(provider.id)) }}</span>
          </div>
          <p class="text-sm text-gray-500 dark:text-gray-400">{{ provider.description }}</p>
          <div v-if="profilesFor(provider.id).length" class="space-y-1">
            <label :for="`profile-${provider.id}`" class="block text-sm font-medium text-gray-700 dark:text-gray-200">授权档案</label>
            <select :id="`profile-${provider.id}`" v-model="selected[provider.id]" :class="fieldClass" :disabled="loading || mutationBusy" @change="clearTransient(provider.id)">
              <option v-for="item in profilesFor(provider.id)" :key="item.profile_ref" :value="item.profile_ref">{{ item.name }}</option>
              <option value="">新增独立授权档案</option>
            </select>
          </div>
          <div v-if="!selectedProfile(provider.id)" class="space-y-1">
            <label :for="`name-${provider.id}`" class="block text-sm font-medium text-gray-700 dark:text-gray-200">新档案名称</label>
            <input :id="`name-${provider.id}`" v-model="names[provider.id]" type="text" maxlength="100" :class="fieldClass" :disabled="loading || mutationBusy" autocomplete="off">
          </div>
          <dl v-if="selectedProfile(provider.id)" class="space-y-2 text-sm">
            <div v-if="selectedProfile(provider.id)?.email" class="flex flex-wrap justify-between gap-2"><dt class="text-gray-500 dark:text-gray-400">账号</dt><dd class="break-all text-gray-900 dark:text-gray-100">{{ selectedProfile(provider.id)?.email }}</dd></div>
            <div v-if="selectedProfile(provider.id)?.workspace_id" class="flex flex-wrap justify-between gap-2"><dt class="text-gray-500 dark:text-gray-400">工作区</dt><dd class="break-all text-gray-900 dark:text-gray-100">{{ selectedProfile(provider.id)?.workspace_id }}</dd></div>
            <div class="flex flex-wrap justify-between gap-2"><dt class="text-gray-500 dark:text-gray-400">授权有效期</dt><dd class="text-gray-900 dark:text-gray-100">{{ formatTime(selectedProfile(provider.id)?.expires_at) }}</dd></div>
          </dl>
          <p v-if="authorizationFeedback(provider.id)" role="alert" class="rounded-lg border border-amber-200 bg-amber-50 p-4 text-sm text-amber-800 dark:border-amber-900 dark:bg-amber-950/30 dark:text-amber-300">{{ authorizationFeedback(provider.id) }}</p>
          <p v-if="selectedProfile(provider.id)?.status === 'refresh_uncertain'" class="text-sm text-amber-700 dark:text-amber-300">刷新结果未知，已停止重试旧凭据。请重新授权。</p>
          <p v-if="activeAttempt(provider.id) && selectedProfile(provider.id)?.authorized" class="text-sm text-gray-500 dark:text-gray-400">本次重新授权尚未完成，原有效授权保留。</p>
          <div class="flex flex-wrap gap-2">
            <button type="button" class="btn btn-primary" data-action="authorize" :disabled="!providerAvailable(provider.id) || mutationBusy || !!activeAttempt(provider.id) || (!selectedProfile(provider.id) && !names[provider.id].trim())" @click="authorize(provider.id)">{{ selectedProfile(provider.id)?.authorized ? '重新授权' : '开始授权' }}</button>
            <button v-if="selectedProfile(provider.id)?.authorized" type="button" class="btn btn-secondary" data-action="refresh" :disabled="!canRefresh(provider.id)" @click="refresh(provider.id)">刷新授权</button>
          </div>
          <p v-if="!loading && capabilities?.available && !providerAvailable(provider.id)" class="text-sm text-amber-700 dark:text-amber-300">{{ needsReload[provider.id] ? '授权状态尚未确认，请刷新状态后再操作。' : '该提供方暂不可用，请检查配置后刷新状态。' }}</p>
          <div v-if="activeAttempt(provider.id)" class="space-y-3 rounded-lg border border-gray-200 bg-gray-50 p-4 dark:border-dark-600 dark:bg-dark-800">
            <p v-if="attemptExpired(provider.id)" role="status" class="text-sm text-amber-700 dark:text-amber-300">本次授权已过期，请取消后重新发起。</p>
            <p v-else-if="activeAttempt(provider.id)?.status === 'exchanging'" class="text-sm text-gray-600 dark:text-gray-300">授权请求已提交，请先刷新状态核对结果，不要重复提交旧回调。</p>
            <p v-else class="text-sm text-gray-600 dark:text-gray-300">本次授权有效至 {{ formatTime(activeAttempt(provider.id)?.expires_at) }}。请在官方页面登录，将最终地址栏中的完整回调 URL 粘贴到下方。</p>
            <a v-if="authorizationLinks[provider.id] && !attemptExpired(provider.id) && activeAttempt(provider.id)?.status === 'pending'" :href="authorizationLinks[provider.id]" target="_blank" rel="noopener noreferrer" referrerpolicy="no-referrer" class="btn btn-secondary inline-flex" data-action="official-login">打开官方授权页面 ↗</a>
            <p v-else-if="!attemptExpired(provider.id) && activeAttempt(provider.id)?.status === 'pending'" class="text-xs text-gray-500 dark:text-gray-400">若尚未打开官方页面，请取消事务后重新发起。已完成登录的本次回调仍可提交。</p>
            <form class="space-y-3" @submit.prevent="complete(provider.id)">
              <label :for="`callback-${provider.id}`" class="block text-sm font-medium text-gray-700 dark:text-gray-200">本次完整回调 URL</label>
              <textarea :id="`callback-${provider.id}`" v-model="callbackURLs[provider.id]" :class="fieldClass" rows="3" maxlength="16384" autocomplete="off" autocapitalize="off" :spellcheck="false" :disabled="mutationBusy || !providerAvailable(provider.id) || attemptExpired(provider.id) || activeAttempt(provider.id)?.status !== 'pending'" :aria-describedby="`callback-note-${provider.id}`" />
              <p :id="`callback-note-${provider.id}`" class="text-xs text-gray-500 dark:text-gray-400">回调仅用于本次授权，提交后清空。不要粘贴 token、凭据文件或其他事务的回调。</p>
              <div class="flex flex-wrap gap-2">
                <button type="submit" class="btn btn-primary" data-action="complete" :disabled="!canComplete(provider.id)">完成本次授权</button>
                <button type="button" class="btn btn-secondary" data-action="cancel" :disabled="mutationBusy || needsReload[provider.id]" @click="cancel(provider.id)">{{ attemptExpired(provider.id) ? '清理过期事务' : '取消本次事务' }}</button>
              </div>
            </form>
          </div>
          <p v-if="busy[provider.id]" role="status" class="text-sm text-gray-500 dark:text-gray-400">正在处理，请稍候…</p>
          <p v-if="errors[provider.id]" role="alert" class="text-sm text-red-700 dark:text-red-300">{{ errors[provider.id] }}</p>
          <p v-if="notices[provider.id]" role="status" class="text-sm text-green-700 dark:text-green-300">{{ notices[provider.id] }}</p>
        </article>
      </div>
      <section class="card space-y-5 border border-gray-200 p-5 dark:border-dark-700" :aria-busy="switchBusy">
        <div>
          <h3 class="font-semibold text-gray-900 dark:text-white">新会话提供方</h3>
          <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">复用现有受管部署。两份授权均保留，base_url 与 API key 不变；旧会话、在途请求和已有资源保持原绑定。</p>
        </div>
        <form class="space-y-4" @submit.prevent="previewSwitch()">
          <div class="grid gap-4 sm:grid-cols-2">
            <div class="space-y-1">
              <label for="deployment-id" class="block text-sm font-medium text-gray-700 dark:text-gray-200">受管部署 ID</label>
              <input id="deployment-id" v-model="deploymentID" data-field="deployment-id" :class="fieldClass" type="text" maxlength="256" :disabled="switchBusy" autocomplete="off">
              <button type="button" class="text-xs font-medium text-primary-600 hover:underline dark:text-primary-400" data-action="generate-deployment" :disabled="mutationBusy" @click="generateDeploymentID">生成全新部署 ID</button>
            </div>
            <div class="space-y-1">
              <label for="requested-model" class="block text-sm font-medium text-gray-700 dark:text-gray-200">新会话请求模型</label>
              <input id="requested-model" v-model="requestedModel" data-field="requested-model" :class="fieldClass" type="text" maxlength="256" :disabled="switchBusy" autocomplete="off" placeholder="输入实际使用的模型 ID">
            </div>
          </div>
          <fieldset :disabled="switchBusy" class="space-y-2">
            <legend class="text-sm font-medium text-gray-700 dark:text-gray-200">所需能力（至少一项）</legend>
            <div class="flex flex-wrap gap-x-5 gap-y-3">
              <label v-for="item in capabilityChoices" :key="item.id" class="flex items-center gap-2 text-sm text-gray-700 dark:text-gray-200">
                <input v-model="requiredCapabilities" type="checkbox" :value="item.id" class="rounded border-gray-300 text-primary-600 focus:ring-primary-500 dark:border-dark-500">{{ item.label }}
              </label>
            </div>
          </fieldset>
          <button type="submit" class="btn btn-secondary" data-action="preview-switch" :disabled="!validSwitchContext || mutationBusy">{{ switchBusy ? '正在处理…' : '校验可用提供方' }}</button>
        </form>
        <div v-if="switchState" class="space-y-4 border-t border-gray-200 pt-4 dark:border-dark-700">
          <p class="text-sm text-gray-700 dark:text-gray-200">当前新会话：{{ providerName(switchState.selected_provider) }} · 路由版本 {{ switchState.route_revision }}</p>
          <label class="flex items-start gap-2 text-sm text-gray-700 dark:text-gray-200">
            <input v-model="scheduleNewSessions" data-field="schedule-new-sessions" type="checkbox" :disabled="switchBusy || switchState.context_required" class="mt-0.5 rounded border-gray-300 text-primary-600 focus:ring-primary-500 dark:border-dark-500">
            我确认仅为新会话选择提供方；不会恢复人工停调的账号。
          </label>
          <div class="grid gap-3 sm:grid-cols-2">
            <div v-for="provider in providers" :key="provider.id" class="space-y-2 rounded-lg border border-gray-200 p-4 dark:border-dark-600">
              <button type="button" class="btn btn-secondary w-full" :data-switch-provider="provider.id" :disabled="!canSwitch(provider.id)" @click="selectProvider(provider.id)">{{ switchState.selected_provider === provider.id ? '当前：' : '选择 ' }}{{ provider.name }}</button>
              <p class="text-xs text-gray-500 dark:text-gray-400">{{ switchOptionReason(provider.id) }}</p>
            </div>
          </div>
        </div>
        <div class="space-y-4 border-t border-gray-200 pt-4 dark:border-dark-700">
          <button type="button" class="btn btn-secondary" data-action="show-install" :aria-expanded="showInstall" aria-controls="core-runtime-install" :disabled="mutationBusy" @click="toggleInstall">{{ showInstall ? '收起安装步骤' : '首次安装或修复核心运行时' }}</button>
          <form v-if="showInstall" id="core-runtime-install" data-form="install" class="space-y-4" @submit.prevent="installRuntime">
            <div class="space-y-2 rounded-lg bg-gray-50 p-4 text-sm text-gray-600 dark:bg-dark-800 dark:text-gray-300">
              <p>先完成上方两份有效授权，再为同一个 OpenAI 分组创建两个暂停调度的运行账号：</p>
              <ol class="list-decimal space-y-1 pl-5">
                <li>Excel：提供 worker inference base_url 和 API key。</li>
                <li>Codex：不需要填写 OAuth token。</li>
                <li>安装收到 worker 回执后再启用这两个新账号；已有外部受管账号不会被自动接管。</li>
              </ol>
              <p>worker 须配置为仅接收访问凭据的受管模式。安装采用上方当前选中的 Codex 与 Excel 授权档案。</p>
            </div>
            <div class="grid gap-4 sm:grid-cols-2">
              <div class="space-y-1"><label for="install-group" class="block text-sm font-medium text-gray-700 dark:text-gray-200">OpenAI 分组 ID</label><input id="install-group" v-model.number="installGroup" data-field="install-group" :class="fieldClass" type="number" min="1" step="1" :disabled="mutationBusy || !!installBPSAccount"></div>
              <div class="space-y-1"><label for="install-backend" class="block text-sm font-medium text-gray-700 dark:text-gray-200">Worker backend ID</label><input id="install-backend" v-model="installBackend" data-field="install-backend" :class="fieldClass" type="text" maxlength="256" autocomplete="off" :disabled="mutationBusy"></div>
              <div v-if="!installBPSAccount" class="space-y-1"><label for="install-bps-base-url" class="block text-sm font-medium text-gray-700 dark:text-gray-200">Worker inference base URL</label><input id="install-bps-base-url" v-model="installBPSBaseURL" data-field="install-bps-base-url" :class="fieldClass" type="url" maxlength="2048" autocomplete="url" :disabled="mutationBusy"></div>
              <div v-if="!installBPSAccount" class="space-y-1"><label for="install-bps-api-key" class="block text-sm font-medium text-gray-700 dark:text-gray-200">Inference API key</label><input id="install-bps-api-key" v-model="installBPSAPIKey" data-field="install-bps-api-key" :class="fieldClass" type="password" maxlength="4096" autocomplete="new-password" :disabled="mutationBusy"></div>
              <div class="space-y-1"><label for="install-control-url" class="block text-sm font-medium text-gray-700 dark:text-gray-200">Worker 控制根 URL</label><input id="install-control-url" v-model="installControlURL" data-field="install-control-url" :class="fieldClass" type="url" maxlength="2048" autocomplete="off" :disabled="mutationBusy" aria-describedby="install-control-note"></div>
              <div class="space-y-1"><label for="install-control-key" class="block text-sm font-medium text-gray-700 dark:text-gray-200">独立 Control key</label><input id="install-control-key" v-model="installControlKey" data-field="install-control-key" :class="fieldClass" type="password" maxlength="4096" autocomplete="new-password" :disabled="mutationBusy" aria-describedby="install-control-note"></div>
            </div>
            <p id="install-control-note" class="text-xs text-gray-500 dark:text-gray-400">控制 URL 须与 inference 同源；control key 至少 32 字节，且必须独立于 inference 和 Files service key。key 仅用于本次提交，随后清空。首次安装两项必填；已校验的同身份修复可同时留空复用服务端配置。</p>
            <div class="space-y-1">
              <label for="install-provider" class="block text-sm font-medium text-gray-700 dark:text-gray-200">安装后新会话使用</label>
              <select id="install-provider" v-model="installProvider" data-field="install-provider" :class="fieldClass" :disabled="mutationBusy"><option value="" disabled>请选择提供方</option><option value="codex">Codex</option><option value="bps_excel">Excel / BPS</option></select>
            </div>
            <div class="flex flex-wrap items-center gap-2">
              <button v-if="!installCodexAccount" type="button" class="btn btn-secondary" data-action="create-runtime-shells" :disabled="!canCreateRuntimeShells" @click="createRuntimeShells">{{ installBPSAccount ? '重试创建 Codex 账号' : '创建并绑定两个暂停账号' }}</button>
              <span v-if="installBPSAccount && installCodexAccount" role="status" class="text-sm text-gray-600 dark:text-gray-300">已创建 Excel #{{ installBPSAccount }} 与 Codex #{{ installCodexAccount }}</span>
              <span v-else-if="installBPSAccount" role="status" class="text-sm text-gray-600 dark:text-gray-300">已创建 Excel #{{ installBPSAccount }}</span>
            </div>
            <p class="text-xs text-gray-500 dark:text-gray-400">首次安装使用路由版本 0；已有部署会校验当前版本。部署路由 CAS 失败时，暂停账号只会保留运行时标记，不能参与新会话调度。</p>
            <label class="flex items-start gap-2 text-sm text-gray-700 dark:text-gray-200"><input v-model="installConfirmed" data-field="install-confirm" type="checkbox" :disabled="mutationBusy" class="mt-0.5 rounded border-gray-300 text-primary-600 focus:ring-primary-500 dark:border-dark-500">我已核对两份授权、分组和全新空账号，明确为新会话启用所选提供方。</label>
            <button type="submit" class="btn btn-primary" data-action="install-runtime" :disabled="!canInstallRuntime">{{ switchBusy ? '正在安装…' : '安装核心运行时' }}</button>
          </form>
        </div>
        <p v-if="switchError" role="alert" class="text-sm text-red-700 dark:text-red-300">{{ switchError }}</p>
        <p v-if="switchNotice" role="status" class="text-sm text-green-700 dark:text-green-300">{{ switchNotice }}</p>
      </section>
    </div>
    <TotpStepUpDialog :controller="stepUp" />
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import { useRoute } from 'vue-router'
import AppLayout from '@/components/layout/AppLayout.vue'
import TotpStepUpDialog from '@/components/auth/TotpStepUpDialog.vue'
import coreOAuthAPI from '@/api/admin/core-oauth'
import type { CoreOAuthCapabilities, CoreOAuthProfile, CoreOAuthProvider, DeploymentCapability, ManagedSwitchState } from '@/api/admin/core-oauth'
import { isStepUpCancelled, useStepUp } from '@/composables/useStepUp'

const providers = [
  { id: 'codex', name: 'Codex', description: '使用 Codex 官方授权，与 Excel 授权相互独立。' },
  { id: 'bps_excel', name: 'Excel / BPS', description: '使用 Excel 官方加载项授权协议，无需安装或运行 toSub2。' },
] as const
const capabilityChoices: { id: DeploymentCapability; label: string }[] = [
  { id: 'responses', label: 'Responses' }, { id: 'chat_completions', label: 'Chat Completions' },
  { id: 'embeddings', label: 'Embeddings' }, { id: 'alpha_search', label: '搜索' },
  { id: 'live', label: '实时交互' }, { id: 'bps_files', label: 'BPS 文件' },
  { id: 'bps_local_responses', label: 'BPS 本地 Responses' },
]
const fieldClass = 'w-full rounded-lg border border-gray-300 bg-white px-3 py-2 text-sm text-gray-900 focus:border-primary-500 focus:outline-none focus:ring-2 focus:ring-primary-500/20 disabled:cursor-not-allowed disabled:opacity-60 dark:border-dark-600 dark:bg-dark-800 dark:text-gray-100'
const authorizationUncertainMessage = '授权结果尚未确认，请先刷新状态核对。请勿重复提交旧回调；如需重试，请重新发起授权，并使用新事务的回调 URL。'
const stepUp = useStepUp()
const route = useRoute()
let requestedProfileRef = typeof route.query.profile_ref === 'string' ? route.query.profile_ref : ''
const loading = ref(true)
const loadError = ref('')
const capabilities = ref<CoreOAuthCapabilities | null>(null)
const profiles = ref<CoreOAuthProfile[]>([])
const selected = reactive<Record<CoreOAuthProvider, string>>({ codex: '', bps_excel: '' })
const names = reactive({ codex: 'Codex 核心授权', bps_excel: 'Excel 核心授权' })
const busy = reactive({ codex: false, bps_excel: false })
const errors = reactive({ codex: '', bps_excel: '' })
const notices = reactive({ codex: '', bps_excel: '' })
const callbackURLs = reactive({ codex: '', bps_excel: '' })
const authorizationLinks = reactive({ codex: '', bps_excel: '' })
const needsReload = reactive({ codex: false, bps_excel: false })
const now = ref(Date.now())
const anyBusy = computed(() => busy.codex || busy.bps_excel)
const mutationBusy = computed(() => anyBusy.value || switchBusy.value)
let alive = true
let expiryTimer: ReturnType<typeof setInterval> | undefined

function providerName(provider: CoreOAuthProvider) {
  return provider === 'codex' ? 'Codex' : 'Excel / BPS'
}
function profilesFor(provider: CoreOAuthProvider) {
  return profiles.value.filter((profile) => profile.provider === provider)
}
function selectedProfile(provider: CoreOAuthProvider) {
  return profilesFor(provider).find((profile) => profile.profile_ref === selected[provider])
}
function authorizationFeedback(provider: CoreOAuthProvider) {
  const profile = selectedProfile(provider)
  if (profile?.last_error_code === 'CORE_OAUTH_REGION_UNSUPPORTED') {
    return '官方拒绝了当前网络地区的授权。请选择位于受支持地区的代理，重新发起授权并使用新事务的回调 URL；未选代理时使用服务器直连。'
  }
  if (profile?.last_error_code === 'CORE_OAUTH_PROVIDER_REJECTED') {
    return '官方授权未通过，请重新发起授权，并使用新事务的回调 URL。'
  }
  if (profile?.attempt?.status === 'failed') {
    return '本次授权未完成，请重新发起授权，并使用新事务的回调 URL。'
  }
  if (profile?.attempt?.status === 'exchanging' || (!profile?.authorized && profile?.attempt?.status === 'consumed') || profile?.last_error_code === 'CORE_OAUTH_STORAGE_FAILED') {
    return authorizationUncertainMessage
  }
  return ''
}
function providerAvailable(provider: CoreOAuthProvider) {
  return !loading.value && !loadError.value && !needsReload[provider] && capabilities.value?.available === true &&
    capabilities.value.providers.some((item) => item.provider === provider && item.available)
}
function profileStatus(profile?: CoreOAuthProfile) {
  if (!profile) return '未授权'
  const labels = { empty: '未授权', authorizing: '授权进行中', authorized: '已授权', refreshing: '正在刷新', refresh_uncertain: '刷新结果未知，需重新授权', reauthorization_required: '需重新授权' }
  return labels[profile.status] || '状态未知，请刷新'
}
function formatTime(value?: string | null) {
  const time = value ? new Date(value) : null
  return time && Number.isFinite(time.getTime()) ? time.toLocaleString('zh-CN') : '暂无'
}
function activeAttempt(provider: CoreOAuthProvider) {
  const attempt = selectedProfile(provider)?.attempt
  return attempt && ['pending', 'exchanging', 'expired'].includes(attempt.status) ? attempt : null
}
function attemptExpired(provider: CoreOAuthProvider) {
  const attempt = activeAttempt(provider)
  return !!attempt && (attempt.status === 'expired' || !Number.isFinite(Date.parse(attempt.expires_at)) || Date.parse(attempt.expires_at) <= now.value)
}
function canComplete(provider: CoreOAuthProvider) {
  return providerAvailable(provider) && !mutationBusy.value && activeAttempt(provider)?.status === 'pending' &&
    !attemptExpired(provider) && !!callbackURLs[provider].trim()
}
function canRefresh(provider: CoreOAuthProvider) {
  const profile = selectedProfile(provider)
  return providerAvailable(provider) && !mutationBusy.value && !!profile?.authorized && profile.status === 'authorized' && !activeAttempt(provider)
}
function clearTransient(provider: CoreOAuthProvider) {
  callbackURLs[provider] = ''
  authorizationLinks[provider] = ''
  errors[provider] = ''
  notices[provider] = ''
}
function upsert(profile: CoreOAuthProfile) {
  const index = profiles.value.findIndex((item) => item.profile_ref === profile.profile_ref)
  if (index < 0) profiles.value.push(profile)
  else profiles.value[index] = profile
  selected[profile.provider] = profile.profile_ref
}
function errorCode(error: unknown): string {
  if (!error || typeof error !== 'object') return ''
  const value = error as { code?: unknown; reason?: unknown }
  const markers = [value.code, value.reason].filter((item): item is string => typeof item === 'string')
  return markers.find((item) => /^(CORE_OAUTH_|MANAGED_DEPLOYMENT_|STEP_UP_)/.test(item)) || ''
}
function safeError(error: unknown): string {
  const messages: Record<string, string> = {
    CORE_OAUTH_UNAVAILABLE: '核心授权暂不可用，请检查服务端配置。',
    CORE_OAUTH_INVALID_INPUT: '授权信息无效，请确认粘贴的是本次完整回调 URL。',
    CORE_OAUTH_PROFILE_NOT_FOUND: '授权档案已不存在，请刷新状态。',
    CORE_OAUTH_CONFLICT: '授权状态已变化，请刷新后核对再操作。',
    CORE_OAUTH_BUSY: '该档案正在处理其他操作，请稍候刷新。',
    CORE_OAUTH_ATTEMPT_EXPIRED: '本次授权已过期，请取消后重新发起。',
    CORE_OAUTH_ATTEMPT_FORBIDDEN: '请使用发起本次授权的管理员账号完成或取消事务。',
    CORE_OAUTH_AUTHORIZATION_REQUIRED: '该档案需重新授权，请在官方页面登录。',
    CORE_OAUTH_REFRESH_UNCERTAIN: '刷新结果未知，已停止重试旧凭据。请重新授权。',
    CORE_OAUTH_PROVIDER_REJECTED: '官方授权未通过，请重新发起授权，并使用新事务的回调 URL。',
    CORE_OAUTH_REGION_UNSUPPORTED: '官方拒绝了当前网络地区的授权。请选择位于受支持地区的代理，重新发起授权并使用新事务的回调 URL；未选代理时使用服务器直连。',
    CORE_OAUTH_STORAGE_FAILED: '授权状态暂时无法保存，请稍候刷新确认。',
    CORE_OAUTH_INVALID_RESPONSE: '官方登录地址校验失败，请取消事务并联系管理员。',
    MANAGED_DEPLOYMENT_INVALID: '部署、模型或能力无效，请检查后重新校验。',
    MANAGED_DEPLOYMENT_NOT_FOUND: '受管部署不存在，请检查部署 ID。',
    MANAGED_DEPLOYMENT_CONFLICT: '路由已由其他管理员更新，请重新校验后再选择。',
    MANAGED_DEPLOYMENT_UNAVAILABLE: '目标提供方暂不可用，请重新校验并查看原因。',
    STEP_UP_TOTP_NOT_ENABLED: '此操作需要管理员启用双重验证。',
    STEP_UP_ADMIN_API_KEY_FORBIDDEN: '请使用管理员网页登录会话执行此操作。',
  }
  return messages[errorCode(error)] || '操作未完成，请刷新状态确认后重试。'
}

async function loadProfiles() {
  if (mutationBusy.value) return
  loading.value = true
  loadError.value = ''
  try {
    const [available, result] = await Promise.all([coreOAuthAPI.getCapabilities(), coreOAuthAPI.listProfiles()])
    if (!alive) return
    capabilities.value = available
    profiles.value = result
    for (const provider of providers) {
      clearTransient(provider.id)
      needsReload[provider.id] = false
      if (!selectedProfile(provider.id)) selected[provider.id] = profilesFor(provider.id)[0]?.profile_ref || ''
    }
    const requestedProfile = result.find((profile) => profile.profile_ref === requestedProfileRef)
    if (requestedProfile) {
      selected[requestedProfile.provider] = requestedProfile.profile_ref
      requestedProfileRef = ''
      showInstall.value = true
    }
  } catch (error) {
    if (alive) { capabilities.value = null; loadError.value = safeError(error) }
  } finally {
    if (alive) loading.value = false
  }
}
async function syncProfileAfterError(provider: CoreOAuthProvider, profileRef: string) {
  if (!alive) return
  needsReload[provider] = true
  try {
    const result = await coreOAuthAPI.getProfile(profileRef)
    if (!alive || result.provider !== provider || result.profile_ref !== profileRef) return
    upsert(result)
    needsReload[provider] = false
  } catch {
    // The mutation may have reached the server. Require a fresh safe state before another action.
  }
}
function officialURL(value: string, provider: CoreOAuthProvider) {
  try {
    const url = new URL(value)
    const path = provider === 'codex' ? '/oauth/authorize' : '/api/accounts/authorize'
    return url.origin === 'https://auth.openai.com' && !url.username && !url.password && !url.hash && url.pathname === path ? url.href : ''
  } catch { return '' }
}
async function authorize(provider: CoreOAuthProvider) {
  if (!providerAvailable(provider) || mutationBusy.value || activeAttempt(provider)) return
  if (!selectedProfile(provider) && !names[provider].trim()) return
  busy[provider] = true
  clearTransient(provider)
  try {
    await stepUp.run(async () => {
      let profile = selectedProfile(provider)
      if (!profile) {
        profile = await coreOAuthAPI.createProfile({ provider, name: names[provider].trim() })
        if (!alive) return
        if (profile.provider !== provider) throw { code: 'CORE_OAUTH_INVALID_RESPONSE' }
        upsert(profile)
      }
      const result = await coreOAuthAPI.startAuthorization(profile.profile_ref)
      if (!alive) return
      if (result.profile.provider !== provider || result.profile.profile_ref !== profile.profile_ref || result.profile.attempt?.attempt_id !== result.attempt_id) throw { code: 'CORE_OAUTH_INVALID_RESPONSE' }
      upsert(result.profile)
      const url = officialURL(result.authorization_url, provider)
      if (!url) throw { code: 'CORE_OAUTH_INVALID_RESPONSE' }
      authorizationLinks[provider] = url
    })
  } catch (error) {
    if (alive && !isStepUpCancelled(error)) {
      errors[provider] = safeError(error)
      const profile = selectedProfile(provider)
      if (profile) await syncProfileAfterError(provider, profile.profile_ref)
      else needsReload[provider] = true
    }
  } finally { if (alive) busy[provider] = false }
}
async function complete(provider: CoreOAuthProvider) {
  const profile = selectedProfile(provider)
  const attempt = activeAttempt(provider)
  if (!profile || !attempt || !canComplete(provider)) return
  const callbackURL = callbackURLs[provider].trim()
  callbackURLs[provider] = ''
  busy[provider] = true
  errors[provider] = ''
  notices[provider] = ''
  try {
    const result = await stepUp.run(() => coreOAuthAPI.completeAuthorization(profile.profile_ref, { attempt_id: attempt.attempt_id, callback_url: callbackURL }))
    if (!alive) return
    upsert(result)
    authorizationLinks[provider] = ''
    if (!authorizationFeedback(provider) && result.authorized && result.status === 'authorized' && (!result.attempt || result.attempt.status === 'consumed')) {
      notices[provider] = '已完成本次授权。'
    } else if (!authorizationFeedback(provider)) {
      errors[provider] = authorizationUncertainMessage
      needsReload[provider] = true
    }
  } catch (error) {
    if (alive && !isStepUpCancelled(error)) {
      errors[provider] = errorCode(error) ? safeError(error) : authorizationUncertainMessage
      await syncProfileAfterError(provider, profile.profile_ref)
    }
  } finally { if (alive) { callbackURLs[provider] = ''; busy[provider] = false } }
}
async function cancel(provider: CoreOAuthProvider) {
  const profile = selectedProfile(provider)
  const attempt = activeAttempt(provider)
  if (!profile || !attempt || mutationBusy.value || needsReload[provider]) return
  busy[provider] = true
  clearTransient(provider)
  try {
    const result = await stepUp.run(() => coreOAuthAPI.cancelAuthorization(profile.profile_ref, attempt.attempt_id))
    if (!alive) return
    upsert(result)
    notices[provider] = '本次授权事务已取消。'
  } catch (error) {
    if (alive && !isStepUpCancelled(error)) {
      errors[provider] = safeError(error)
      await syncProfileAfterError(provider, profile.profile_ref)
    }
  } finally { if (alive) busy[provider] = false }
}
async function refresh(provider: CoreOAuthProvider) {
  const profile = selectedProfile(provider)
  if (!profile || !canRefresh(provider)) return
  busy[provider] = true
  clearTransient(provider)
  try {
    const result = await stepUp.run(() => coreOAuthAPI.refreshProfile(profile.profile_ref, profile.token_revision))
    if (!alive) return
    upsert(result)
    notices[provider] = '授权已刷新。'
  } catch (error) {
    if (alive && !isStepUpCancelled(error)) {
      errors[provider] = safeError(error)
      await syncProfileAfterError(provider, profile.profile_ref)
    }
  } finally { if (alive) busy[provider] = false }
}

const deploymentID = ref('')
const requestedModel = ref('')
const requiredCapabilities = ref<DeploymentCapability[]>([])
const scheduleNewSessions = ref(false)
const switchBusy = ref(false)
const switchState = ref<ManagedSwitchState | null>(null)
const switchError = ref('')
const switchNotice = ref('')
const previewKey = ref('')
const showInstall = ref(false)
const installGroup = ref<number | ''>('')
const installBPSBaseURL = ref('')
const installBPSAPIKey = ref('')
const installBPSAccount = ref<number | ''>('')
const installCodexAccount = ref<number | ''>('')
const installBackend = ref('')
const installControlURL = ref('')
const installControlKey = ref('')
const installProvider = ref<CoreOAuthProvider | ''>('')
const installConfirmed = ref(false)
const contextKey = computed(() => JSON.stringify([deploymentID.value.trim(), requestedModel.value.trim(), [...requiredCapabilities.value].sort()]))
const validSwitchContext = computed(() => !!deploymentID.value.trim() && !!requestedModel.value.trim() && requiredCapabilities.value.length > 0)
watch(contextKey, () => {
  switchState.value = null
  previewKey.value = ''
  scheduleNewSessions.value = false
  switchError.value = ''
  switchNotice.value = ''
  installConfirmed.value = false
})
function switchContext() {
  return { requested_model: requestedModel.value.trim(), required_capabilities: [...requiredCapabilities.value] }
}
async function previewSwitch(preserveError = false) {
  if (!validSwitchContext.value || mutationBusy.value) return
  switchBusy.value = true
  if (!preserveError) switchError.value = ''
  switchNotice.value = ''
  scheduleNewSessions.value = false
  switchState.value = null
  const key = contextKey.value
  try {
    const result = await coreOAuthAPI.getSwitchOptions(deploymentID.value.trim(), switchContext())
    if (!alive || key !== contextKey.value) return
    switchState.value = result
    previewKey.value = key
  } catch (error) {
    if (alive) switchError.value = safeError(error)
  } finally { if (alive) switchBusy.value = false }
}
function canSwitch(provider: CoreOAuthProvider) {
  const state = switchState.value
  return !!state && !mutationBusy.value && !state.context_required && scheduleNewSessions.value &&
    previewKey.value === contextKey.value && state.applies_to === 'new_sessions' && Number.isSafeInteger(state.route_revision) && state.route_revision > 0 &&
    state.options.some((option) => option.provider === provider && option.available) && state.selected_provider !== provider
}
function switchOptionReason(provider: CoreOAuthProvider) {
  const option = switchState.value?.options.find((item) => item.provider === provider)
  if (!option) return '目标授权未就绪，请先完成该提供方授权。'
  if (option.available) return '已通过当前模型、能力和调度策略校验。'
  const reasons: Record<string, string> = {
    request_context_required: '请明确输入模型并选择至少一项能力后重新校验。',
    authorization_unavailable: '目标授权不可用，请先完成授权或刷新状态。',
    identity_mismatch: '授权主体不匹配，无法切换。', workspace_mismatch: '工作区不匹配，无法切换。',
    scheduling_paused: '账号已被人工停调，请在账号管理中核对。', group_mismatch: '目标账号与部署分组不一致。',
    model_not_supported: '目标提供方不支持所请求的模型。', capability_mismatch: '目标提供方不满足所需能力。',
    policy_restricted: '当前调度策略不允许使用此提供方。',
  }
  return reasons[option.reason] || '目标提供方暂不可用，请联系管理员。'
}
async function selectProvider(provider: CoreOAuthProvider) {
  const state = switchState.value
  if (!state || !canSwitch(provider)) return
  switchBusy.value = true
  switchError.value = ''
  switchNotice.value = ''
  let conflict = false
  try {
    const result = await stepUp.run(() => coreOAuthAPI.switchProvider(deploymentID.value.trim(), {
      ...switchContext(), expected_route_revision: state.route_revision, selected_provider: provider, schedule_new_sessions: true,
    }))
    if (!alive) return
    switchState.value = result
    switchNotice.value = `新会话已选择 ${providerName(result.selected_provider)}；旧会话与在途请求保持原绑定。`
  } catch (error) {
    if (alive && !isStepUpCancelled(error)) {
      switchError.value = safeError(error)
      conflict = errorCode(error) === 'MANAGED_DEPLOYMENT_CONFLICT'
      switchState.value = null
    }
  } finally { if (alive) { switchBusy.value = false; scheduleNewSessions.value = false } }
  // Re-read on conflict, but require a new explicit operator action before writing.
  if (alive && conflict) await previewSwitch(true)
}

function toggleInstall() {
  showInstall.value = !showInstall.value
  installControlKey.value = ''
  installConfirmed.value = false
}
function generateDeploymentID() {
  try {
    deploymentID.value = `deployment_${crypto.randomUUID()}`
  } catch {
    switchError.value = '当前浏览器无法生成部署 ID，请使用支持安全随机 UUID 的浏览器。'
  }
}
function positiveID(value: number | '') {
  return typeof value === 'number' && Number.isSafeInteger(value) && value > 0
}
function validControlURL(value: string) {
  try {
    const url = new URL(value)
    return ['https:', 'http:'].includes(url.protocol) && !url.username && !url.password && !url.search && !url.hash
  } catch { return false }
}
const canCreateRuntimeShells = computed(() => !mutationBusy.value && positiveID(installGroup.value) && !installCodexAccount.value &&
  (!!installBPSAccount.value || (!!installBPSBaseURL.value.trim() && !!installBPSAPIKey.value.trim())))
async function createRuntimeShells() {
  if (!canCreateRuntimeShells.value) return
  const groupID = Number(installGroup.value)
  switchBusy.value = true
  switchError.value = ''
  switchNotice.value = ''
  try {
    if (!installBPSAccount.value) {
      const excel = await stepUp.run(() => coreOAuthAPI.createRuntimeShell({
        provider: 'bps_excel', group_id: groupID, base_url: installBPSBaseURL.value.trim(), api_key: installBPSAPIKey.value,
      }))
      installBPSAPIKey.value = ''
      installBPSBaseURL.value = ''
      installBPSAccount.value = excel.id
    }
    const codex = await stepUp.run(() => coreOAuthAPI.createRuntimeShell({ provider: 'codex', group_id: groupID }))
    installCodexAccount.value = codex.id
    switchNotice.value = '两个运行账号已创建并保持暂停调度。'
  } catch (error) {
    if (!isStepUpCancelled(error)) switchError.value = safeError(error)
  } finally {
    switchBusy.value = false
    installBPSAPIKey.value = ''
  }
}
const installRevision = computed(() => previewKey.value === contextKey.value ? switchState.value?.route_revision ?? 0 : 0)
const canInstallRuntime = computed(() => {
  const controlURL = installControlURL.value.trim()
  const hasControl = !!controlURL || !!installControlKey.value
  const validControl = hasControl
    ? validControlURL(controlURL) && new TextEncoder().encode(installControlKey.value).length >= 32
    : installRevision.value > 0
  return !mutationBusy.value && !loading.value && !loadError.value && validSwitchContext.value && installConfirmed.value &&
    Number.isSafeInteger(installRevision.value) && installRevision.value >= 0 &&
    providerAvailable('codex') && providerAvailable('bps_excel') && !!selectedProfile('codex')?.authorized && !!selectedProfile('bps_excel')?.authorized &&
    positiveID(installGroup.value) && positiveID(installBPSAccount.value) && positiveID(installCodexAccount.value) &&
    installBPSAccount.value !== installCodexAccount.value && !!installBackend.value.trim() && !!installProvider.value && validControl
})
async function installRuntime() {
  const codex = selectedProfile('codex')
  const excel = selectedProfile('bps_excel')
  const provider = installProvider.value
  if (!canInstallRuntime.value || !codex || !excel || !provider) return
  const controlKey = installControlKey.value
  installControlKey.value = ''
  const input = {
    ...switchContext(), group_id: Number(installGroup.value),
    bps_profile_ref: excel.profile_ref, codex_profile_ref: codex.profile_ref,
    bps_account_id: Number(installBPSAccount.value), codex_account_id: Number(installCodexAccount.value),
    bps_backend_id: installBackend.value.trim(), bps_control_url: installControlURL.value.trim(), bps_control_key: controlKey,
    expected_route_revision: installRevision.value, selected_provider: provider, schedule_new_sessions: true as const,
  }
  switchBusy.value = true
  switchError.value = ''
  switchNotice.value = ''
  scheduleNewSessions.value = false
  let conflict = false
  try {
    const result = await stepUp.run(() => coreOAuthAPI.installCoreRuntime(deploymentID.value.trim(), input))
    if (!alive) return
    switchState.value = result
    previewKey.value = contextKey.value
    switchNotice.value = `核心运行时已安装，新会话已选择 ${providerName(result.selected_provider)}。旧会话保持原绑定。`
  } catch (error) {
    if (alive && !isStepUpCancelled(error)) {
      switchError.value = safeError(error)
      conflict = errorCode(error) === 'MANAGED_DEPLOYMENT_CONFLICT'
      switchState.value = null
    }
  } finally {
    if (alive) { switchBusy.value = false; installConfirmed.value = false; installControlKey.value = '' }
  }
  if (alive && conflict) await previewSwitch(true)
}

onMounted(() => {
  void loadProfiles()
  expiryTimer = setInterval(() => { now.value = Date.now() }, 1000)
})
onBeforeUnmount(() => {
  alive = false
  stepUp.onCancel()
  installControlKey.value = ''
  clearInterval(expiryTimer)
  for (const provider of providers) clearTransient(provider.id)
})
</script>
