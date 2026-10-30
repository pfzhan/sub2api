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
          <label class="flex cursor-pointer items-center gap-2">
            <input
              v-model="method"
              type="radio"
              value="import"
              class="text-blue-600 focus:ring-blue-500"
              :disabled="busy"
            />
            <span class="text-sm text-blue-900 dark:text-blue-200">
              {{ t('admin.accounts.oauth.kiro.methodImport') }}
            </span>
          </label>
          <label class="flex cursor-pointer items-center gap-2">
            <input
              v-model="method"
              type="radio"
              value="builder_id"
              class="text-blue-600 focus:ring-blue-500"
              :disabled="busy"
            />
            <span class="text-sm text-blue-900 dark:text-blue-200">
              {{ t('admin.accounts.oauth.kiro.methodBuilderId') }}
            </span>
          </label>
        </div>

        <div>
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

        <template v-else>
          <p class="text-sm text-blue-800 dark:text-blue-300">
            {{ t('admin.accounts.oauth.kiro.builderIdDesc') }}
          </p>
          <div v-if="!device">
            <button type="button" class="btn btn-primary" :disabled="busy" @click="handleStartLogin">
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
import kiroAPI, { type KiroDeviceAuthResponse } from '@/api/admin/kiro'
import { extractApiErrorMessage } from '@/utils/apiError'

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

const method = ref<'import' | 'builder_id'>('import')
const region = ref('us-east-1')
const rawCredentials = ref('')
const loading = ref(false)
const error = ref('')
const device = ref<KiroDeviceAuthResponse | null>(null)

let pollTimer: ReturnType<typeof setTimeout> | null = null
// 每次发起或取消登录时递增，用于丢弃过期的轮询结果
let pollGeneration = 0

const busy = computed(() => loading.value || !!props.creating)
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

const handleStartLogin = async () => {
  stopPolling()
  loading.value = true
  error.value = ''
  try {
    const result = await kiroAPI.startDeviceAuth({
      region: region.value.trim() || undefined,
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

watch(method, () => {
  resetDevice()
  error.value = ''
})

onBeforeUnmount(stopPolling)
</script>
