import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import KiroAuthPanel from '../KiroAuthPanel.vue'
import { withClearedKiroIdentity } from '@/api/admin/kiro'

const kiroMocks = vi.hoisted(() => ({
  importCredentials: vi.fn(),
  startDeviceAuth: vi.fn(),
  pollDeviceAuth: vi.fn()
}))

vi.mock('@/api/admin/kiro', async () => {
  const actual = await vi.importActual<typeof import('@/api/admin/kiro')>('@/api/admin/kiro')
  return { ...actual, default: kiroMocks }
})

vi.mock('@/composables/useClipboard', async () => {
  const { ref } = await vi.importActual<typeof import('vue')>('vue')
  return { useClipboard: () => ({ copied: ref(false), copyToClipboard: vi.fn() }) }
})

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) }
})

const deviceAuth = {
  session_id: 'sess-1',
  user_code: 'ABCD-EFGH',
  verification_uri: 'https://device.sso.us-east-1.amazonaws.com/',
  verification_uri_complete: 'https://device.sso.us-east-1.amazonaws.com/?user_code=ABCD-EFGH',
  expires_in: 600,
  interval: 5
}

function mountPanel() {
  return mount(KiroAuthPanel, { props: { proxyId: 3 } })
}

describe('KiroAuthPanel', () => {
  beforeEach(() => {
    vi.useFakeTimers()
    vi.spyOn(window, 'open').mockReturnValue(null)
    kiroMocks.importCredentials.mockReset()
    kiroMocks.startDeviceAuth.mockReset()
    kiroMocks.pollDeviceAuth.mockReset()
  })

  afterEach(() => {
    vi.useRealTimers()
    vi.restoreAllMocks()
  })

  it('imports credentials and emits the validated result', async () => {
    kiroMocks.importCredentials.mockResolvedValue({ credentials: { access_token: 'at' } })
    const wrapper = mountPanel()

    await wrapper.get('textarea').setValue('  rt-token  ')
    await wrapper.get('button.btn-primary').trigger('click')
    await flushPromises()

    expect(kiroMocks.importCredentials).toHaveBeenCalledWith({
      credentials: 'rt-token',
      region: 'us-east-1',
      proxy_id: 3
    })
    expect(wrapper.emitted('authorized')).toEqual([[{ access_token: 'at' }]])
  })

  it('shows the import error from the API', async () => {
    kiroMocks.importCredentials.mockRejectedValue({ message: 'kiro token refresh failed' })
    const wrapper = mountPanel()

    await wrapper.get('textarea').setValue('bad')
    await wrapper.get('button.btn-primary').trigger('click')
    await flushPromises()

    expect(wrapper.text()).toContain('kiro token refresh failed')
    expect(wrapper.emitted('authorized')).toBeUndefined()
  })

  it('polls Builder ID login at the server interval until it succeeds', async () => {
    kiroMocks.startDeviceAuth.mockResolvedValue(deviceAuth)
    kiroMocks.pollDeviceAuth
      .mockResolvedValueOnce({ status: 'pending', interval: 10 })
      .mockResolvedValueOnce({ status: 'success', credentials: { access_token: 'at' } })
    const wrapper = mountPanel()

    await wrapper.get('input[value="builder_id"]').setValue(true)
    await wrapper.get('button.btn-primary').trigger('click')
    await flushPromises()

    expect(kiroMocks.startDeviceAuth).toHaveBeenCalledWith({ region: 'us-east-1', proxy_id: 3 })
    expect(window.open).toHaveBeenCalledWith(deviceAuth.verification_uri_complete, '_blank', 'noopener,noreferrer')
    expect(wrapper.text()).toContain('ABCD-EFGH')

    await vi.advanceTimersByTimeAsync(5000)
    expect(kiroMocks.pollDeviceAuth).toHaveBeenCalledTimes(1)
    await vi.advanceTimersByTimeAsync(9000)
    expect(kiroMocks.pollDeviceAuth).toHaveBeenCalledTimes(1)
    await vi.advanceTimersByTimeAsync(1000)
    expect(kiroMocks.pollDeviceAuth).toHaveBeenCalledTimes(2)
    await flushPromises()

    expect(wrapper.emitted('authorized')).toEqual([[{ access_token: 'at' }]])
    expect(wrapper.text()).not.toContain('ABCD-EFGH')
  })

  it('stops polling when the login is cancelled', async () => {
    kiroMocks.startDeviceAuth.mockResolvedValue(deviceAuth)
    const wrapper = mountPanel()

    await wrapper.get('input[value="builder_id"]').setValue(true)
    await wrapper.get('button.btn-primary').trigger('click')
    await flushPromises()
    const cancel = wrapper.findAll('button').find((button) => button.text() === 'common.cancel')
    await cancel!.trigger('click')
    await vi.advanceTimersByTimeAsync(30000)

    expect(kiroMocks.pollDeviceAuth).not.toHaveBeenCalled()
  })
})

describe('withClearedKiroIdentity', () => {
  it('clears identity fields missing from the new credentials', () => {
    const merged = withClearedKiroIdentity({ access_token: 'at', refresh_token: 'rt', auth_method: 'social' })
    expect(merged.access_token).toBe('at')
    expect(merged.profile_arn).toBe('')
    expect(merged.client_id).toBe('')
    expect(merged).not.toHaveProperty('model_mapping')
  })
})
