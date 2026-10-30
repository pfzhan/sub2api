<template>
  <div class="rounded-lg border border-blue-200 bg-blue-50 p-4 dark:border-blue-700 dark:bg-blue-900/30">
    <div class="flex items-start gap-4">
      <div class="flex h-10 w-10 flex-shrink-0 items-center justify-center rounded-lg bg-blue-500">
        <Icon name="link" size="md" class="text-white" />
      </div>
      <div class="flex-1 space-y-4">
        <h4 class="font-semibold text-blue-900 dark:text-blue-200">
          {{ t('admin.accounts.oauth.kiro.title') }}
        </h4>

        <div class="flex flex-wrap gap-4">
          <label v-for="option in methodOptions" :key="option.value" class="flex cursor-pointer items-center gap-2">
            <input
              v-model="method"
              type="radio"
              :value="option.value"
              class="text-blue-600 focus:ring-blue-500"
              :disabled="busy"
            />
            <span class="text-sm text-blue-900 dark:text-blue-200">{{ option.label }}</span>
          </label>
        </div>

        <div v-if="!isSocial">
          <label class="input-label">{{ t('admin.accounts.oauth.kiro.region') }}</label>
          <input
            v-model="region"
            type="text"
            class="input"
            placeholder="us-east-1"
            :disabled="busy"
          />
          <p class="input-hint">{{ t('admin.accounts.oauth.kiro.regionHint') }}</p>
        </div>

        <template v-if="method === 'identity_center'">
          <div>
            <label class="input-label">{{ t('admin.accounts.oauth.kiro.startUrl') }}</label>
            <input
              v-model="startUrl"
              type="url"
              class="input"
              placeholder="https://my-org.awsapps.com/start"
              :disabled="busy || !!device"
            />
            <p class="input-hint">{{ t('admin.accounts.oauth.kiro.startUrlHint') }}</p>
          </div>
          <div>
            <label class="input-label">{{ t('admin.accounts.oauth.kiro.ssoRegion') }}</label>
            <input
              v-model="ssoRegion"
              type="text"
              class="input"
              :placeholder="region || 'us-east-1'"
              :disabled="busy || !!device"
            />
            <p class="input-hint">{{ t('admin.accounts.oauth.kiro.ssoRegionHint') }}</p>
          </div>
        </template>

        <template v-if="method === 'import'">
          <p class="text-sm text-blue-800 dark:text-blue-300">
            {{ t('admin.accounts.oauth.kiro.importDesc') }}
          </p>
          <textarea
            v-model="rawCredentials"
            rows="6"
            class="input w-full resize-y font-mono text-xs"
            :placeholder="t('admin.accounts.oauth.kiro.importPlaceholder')"
            :disabled="busy"
          ></textarea>
          <button
            type="button"
            class="btn btn-primary"
            :disabled="busy || !rawCredentials.trim()"
            @click="handleImport"
          >
            {{ busy ? t('admin.accounts.oauth.kiro.validating') : t('admin.accounts.oauth.kiro.validateAndCreate') }}
          </button>
        </template>

        <template v-else-if="isSocial">
          <p class="text-sm text-blue-800 dark:text-blue-300">
            {{ t('admin.accounts.oauth.kiro.socialDesc') }}
          </p>
          <div v-if="!social">
            <button type="button" class="btn btn-primary" :disabled="busy" @click="handleStartSocial">
              {{ t('admin.accounts.oauth.kiro.startLogin') }}
            </button>
          </div>
          <div
            v-else
            class="space-y-3 rounded-lg border border-blue-300 bg-white/80 p-4 dark:border-blue-600 dark:bg-gray-800/80"
          >
            <div>
              <p class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.accounts.oauth.kiro.loginPage') }}</p>
              <a
                :href="social.auth_url"
                target="_blank"
                rel="noopener noreferrer"
                class="break-all text-sm text-blue-600 underline hover:text-blue-700 dark:text-blue-400"
              >
                {{ social.auth_url }}
              </a>
            </div>
            <div>
              <label class="input-label">{{ t('admin.accounts.oauth.kiro.callbackUrl') }}</label>
              <textarea
                v-model="callbackUrl"
                rows="3"
                class="input w-full resize-y font-mono text-xs"
                :placeholder="`${social.redirect_uri}?code=...&state=...`"
                :disabled="busy"
              ></textarea>
              <p class="input-hint">{{ t('admin.accounts.oauth.kiro.callbackUrlHint') }}</p>
            </div>
            <div class="flex items-center justify-between gap-3">
              <button
                type="button"
                class="btn btn-primary"
                :disabled="busy || !callbackUrl.trim()"
                @click="handleExchangeSocial"
              >
                {{ busy ? t('admin.accounts.oauth.kiro.validating') : t('admin.accounts.oauth.kiro.completeLogin') }}
              </button>
              <button type="button" class="btn btn-secondary btn-sm" :disabled="busy" @click="resetSocial">
                {{ t('common.cancel') }}
              </button>
            </div>
          </div>
        </template>

        <template v-else>
          <p class="text-sm text-blue-800 dark:text-blue-300">
            {{ method === 'identity_center' ? t('admin.accounts.oauth.kiro.identityCenterDesc') : t('admin.accounts.oauth.kiro.builderIdDesc') }}
          </p>
          <div v-if="!device">
            <button
              type="button"
              class="btn btn-primary"
              :disabled="busy || (method === 'identity_center' && !startUrl.trim())"
              @click="handleStartDevice"
            >
              {{ t('admin.accounts.oauth.kiro.startLogin') }}
            </button>
          </div>
          <div
            v-else
            class="space-y-3 rounded-lg border border-blue-300 bg-white/80 p-4 dark:border-blue-600 dark:bg-gray-800/80"
          >
            <div>
              <p class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.accounts.oauth.kiro.userCode') }}</p>
              <div class="mt-1 flex items-center gap-2">
                <span class="font-mono text-2xl font-semibold tracking-widest text-gray-900 dark:text-white">
                  {{ device.user_code }}
                </span>
                <button
                  type="button"
                  class="btn btn-secondary btn-sm"
                  @click="copyToClipboard(device.user_code)"
                >
                  {{ copied ? t('common.copied') : t('common.copy') }}
                </button>
              </div>
            </div>
            <div>
              <p class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.accounts.oauth.kiro.openLink') }}</p>
              <a
                :href="verificationUrl"
                target="_blank"
                rel="noopener noreferrer"
                class="break-all text-sm text-blue-600 underline hover:text-blue-700 dark:text-blue-400"
              >
                {{ verificationUrl }}
              </a>
            </div>
            <div class="flex items-center justify-between gap-3">
              <span class="flex items-center gap-2 text-sm text-gray-600 dark:text-gray-300">
                <Icon name="refresh" size="sm" class="animate-spin" />
                {{ t('admin.accounts.oauth.kiro.waiting') }}
              </span>
              <button type="button" class="btn btn-secondary btn-sm" @click="resetDevice">
                {{ t('common.cancel') }}
              </button>
            </div>
          </div>
        </template>

        <div
          v-if="error"
          class="rounded-lg border border-red-200 bg-red-50 p-3 text-sm text-red-700 dark:border-red-700 dark:bg-red-900/30 dark:text-red-300"
        >
          {{ error }}
        </div>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import Icon from '@/components/icons/Icon.vue'
import { useClipboard } from '@/composables/useClipboard'
import kiroAPI, { type KiroDeviceAuthResponse, type KiroSocialAuthResponse } from '@/api/admin/kiro'
import { extractApiErrorCode, extractApiErrorMessage } from '@/utils/apiError'

type KiroLoginMethod = 'import' | 'builder_id' | 'identity_center' | 'google' | 'github'

const props = defineProps<{
  proxyId: number | null
  // 父组件创建账号期间为 true
  creating?: boolean
}>()

const emit = defineEmits<{
  authorized: [credentials: Record<string, unknown>]
}>()

const { t } = useI18n()
const { copied, copyToClipboard } = useClipboard()

const method = ref<KiroLoginMethod>('import')
const region = ref('us-east-1')
const ssoRegion = ref('')
const startUrl = ref('')
const rawCredentials = ref('')
const callbackUrl = ref('')
const loading = ref(false)
const error = ref('')
const device = ref<KiroDeviceAuthResponse | null>(null)
const social = ref<KiroSocialAuthResponse | null>(null)

let pollTimer: ReturnType<typeof setTimeout> | null = null
// 每次发起或取消登录时递增，用于丢弃过期的轮询结果
let pollGeneration = 0

const methodOptions = computed<{ value: KiroLoginMethod; label: string }[]>(() => [
  { value: 'import', label: t('admin.accounts.oauth.kiro.methodImport') },
  { value: 'builder_id', label: t('admin.accounts.oauth.kiro.methodBuilderId') },
  { value: 'identity_center', label: t('admin.accounts.oauth.kiro.methodIdentityCenter') },
  { value: 'google', label: t('admin.accounts.oauth.kiro.methodGoogle') },
  { value: 'github', label: t('admin.accounts.oauth.kiro.methodGithub') }
])

const busy = computed(() => loading.value || !!props.creating)
const isSocial = computed(() => method.value === 'google' || method.value === 'github')
const verificationUrl = computed(
  () => device.value?.verification_uri_complete || device.value?.verification_uri || ''
)

const stopPolling = () => {
  pollGeneration++
  if (pollTimer) {
    clearTimeout(pollTimer)
    pollTimer = null
  }
}

const resetDevice = () => {
  stopPolling()
  device.value = null
}

const resetSocial = () => {
  social.value = null
  callbackUrl.value = ''
}

const handleImport = async () => {
  if (!rawCredentials.value.trim()) return
  loading.value = true
  error.value = ''
  try {
    const result = await kiroAPI.importCredentials({
      credentials: rawCredentials.value.trim(),
      region: region.value.trim() || undefined,
      proxy_id: props.proxyId
    })
    emit('authorized', result.credentials)
  } catch (err) {
    error.value = extractApiErrorMessage(err, t('admin.accounts.oauth.kiro.importFailed'))
  } finally {
    loading.value = false
  }
}

const schedulePoll = (sessionId: string, intervalSeconds: number, generation: number) => {
  pollTimer = setTimeout(() => {
    void poll(sessionId, generation)
  }, Math.max(intervalSeconds, 1) * 1000)
}

const poll = async (sessionId: string, generation: number) => {
  try {
    const result = await kiroAPI.pollDeviceAuth(sessionId)
    if (generation !== pollGeneration) return
    if (result.status === 'success' && result.credentials) {
      device.value = null
      emit('authorized', result.credentials)
      return
    }
    schedulePoll(sessionId, result.interval || device.value?.interval || 5, generation)
  } catch (err) {
    if (generation !== pollGeneration) return
    device.value = null
    error.value = extractApiErrorMessage(err, t('admin.accounts.oauth.kiro.loginFailed'))
  }
}

const handleStartDevice = async () => {
  stopPolling()
  loading.value = true
  error.value = ''
  const identityCenter = method.value === 'identity_center'
  try {
    const result = await kiroAPI.startDeviceAuth({
      region: region.value.trim() || undefined,
      sso_region: identityCenter ? ssoRegion.value.trim() || undefined : undefined,
      start_url: identityCenter ? startUrl.value.trim() : undefined,
      proxy_id: props.proxyId
    })
    device.value = result
    window.open(result.verification_uri_complete || result.verification_uri, '_blank', 'noopener,noreferrer')
    schedulePoll(result.session_id, result.interval || 5, pollGeneration)
  } catch (err) {
    error.value = extractApiErrorMessage(err, t('admin.accounts.oauth.kiro.loginFailed'))
  } finally {
    loading.value = false
  }
}

const handleStartSocial = async () => {
  if (method.value !== 'google' && method.value !== 'github') return
  loading.value = true
  error.value = ''
  try {
    const result = await kiroAPI.startSocialAuth({ provider: method.value, proxy_id: props.proxyId })
    social.value = result
    window.open(result.auth_url, '_blank', 'noopener,noreferrer')
  } catch (err) {
    error.value = extractApiErrorMessage(err, t('admin.accounts.oauth.kiro.loginFailed'))
  } finally {
    loading.value = false
  }
}

const handleExchangeSocial = async () => {
  if (!social.value || !callbackUrl.value.trim()) return
  loading.value = true
  error.value = ''
  try {
    const result = await kiroAPI.exchangeSocialAuth(social.value.session_id, callbackUrl.value.trim())
    resetSocial()
    emit('authorized', result.credentials)
  } catch (err) {
    // 回调地址填错时可以重新粘贴；其他错误下登录会话已失效，需要重新开始。
    if (extractApiErrorCode(err) !== 'KIRO_INVALID_CALLBACK') {
      resetSocial()
    }
    error.value = extractApiErrorMessage(err, t('admin.accounts.oauth.kiro.loginFailed'))
  } finally {
    loading.value = false
  }
}

watch(method, () => {
  resetDevice()
  resetSocial()
  error.value = ''
})

onBeforeUnmount(stopPolling)
</script>
