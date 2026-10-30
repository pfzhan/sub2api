import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import KiroAuthPanel from '../KiroAuthPanel.vue'
import { withClearedKiroIdentity } from '@/api/admin/kiro'

const kiroMocks = vi.hoisted(() => ({
  importCredentials: vi.fn(),
  startDeviceAuth: vi.fn(),
  pollDeviceAuth: vi.fn(),
  startSocialAuth: vi.fn(),
  exchangeSocialAuth: vi.fn()
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
    kiroMocks.startSocialAuth.mockReset()
    kiroMocks.exchangeSocialAuth.mockReset()
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

    expect(kiroMocks.startDeviceAuth).toHaveBeenCalledWith({
      region: 'us-east-1',
      sso_region: undefined,
      start_url: undefined,
      proxy_id: 3
    })
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

describe('KiroAuthPanel identity center and social login', () => {
  beforeEach(() => {
    vi.useFakeTimers()
    vi.spyOn(window, 'open').mockReturnValue(null)
    Object.values(kiroMocks).forEach((mock) => mock.mockReset())
  })

  afterEach(() => {
    vi.useRealTimers()
    vi.restoreAllMocks()
  })

  it('sends the start URL and SSO region for IAM Identity Center', async () => {
    kiroMocks.startDeviceAuth.mockResolvedValue(deviceAuth)
    const wrapper = mountPanel()

    await wrapper.get('input[value="identity_center"]').setValue(true)
    const startButton = wrapper.get('button.btn-primary')
    expect(startButton.attributes('disabled')).toBeDefined()

    await wrapper.get('input[type="url"]').setValue(' https://my-org.awsapps.com/start ')
    const textInputs = wrapper.findAll('input[type="text"]')
    await textInputs[1].setValue('eu-west-1')
    await wrapper.get('button.btn-primary').trigger('click')
    await flushPromises()

    expect(kiroMocks.startDeviceAuth).toHaveBeenCalledWith({
      region: 'us-east-1',
      sso_region: 'eu-west-1',
      start_url: 'https://my-org.awsapps.com/start',
      proxy_id: 3
    })
    expect(wrapper.text()).toContain('ABCD-EFGH')
  })

  it('exchanges the pasted callback URL for GitHub login', async () => {
    kiroMocks.startSocialAuth.mockResolvedValue({
      session_id: 'social-1',
      auth_url: 'https://prod.us-east-1.auth.desktop.kiro.dev/login?idp=Github',
      redirect_uri: 'kiro://kiro.kiroAgent/authenticate-success',
      expires_in: 600
    })
    kiroMocks.exchangeSocialAuth
      .mockRejectedValueOnce({ reason: 'KIRO_INVALID_CALLBACK', message: 'invalid kiro callback: state mismatch' })
      .mockResolvedValueOnce({ credentials: { access_token: 'at', auth_method: 'social' } })
    const wrapper = mountPanel()

    await wrapper.get('input[value="github"]').setValue(true)
    expect(wrapper.find('input[placeholder="us-east-1"]').exists()).toBe(false)
    await wrapper.get('button.btn-primary').trigger('click')
    await flushPromises()

    expect(kiroMocks.startSocialAuth).toHaveBeenCalledWith({ provider: 'github', proxy_id: 3 })
    expect(window.open).toHaveBeenCalledWith(
      'https://prod.us-east-1.auth.desktop.kiro.dev/login?idp=Github',
      '_blank',
      'noopener,noreferrer'
    )

    await wrapper.get('textarea').setValue('kiro://kiro.kiroAgent/authenticate-success?code=c&state=bad')
    await wrapper.get('button.btn-primary').trigger('click')
    await flushPromises()
    expect(wrapper.text()).toContain('state mismatch')
    expect(wrapper.find('textarea').exists()).toBe(true)

    await wrapper.get('textarea').setValue('kiro://kiro.kiroAgent/authenticate-success?code=c&state=ok')
    await wrapper.get('button.btn-primary').trigger('click')
    await flushPromises()
    expect(kiroMocks.exchangeSocialAuth).toHaveBeenLastCalledWith(
      'social-1',
      'kiro://kiro.kiroAgent/authenticate-success?code=c&state=ok'
    )
    expect(wrapper.emitted('authorized')).toEqual([[{ access_token: 'at', auth_method: 'social' }]])
  })

  it('restarts social login when the session is no longer usable', async () => {
    kiroMocks.startSocialAuth.mockResolvedValue({
      session_id: 'social-1',
      auth_url: 'https://prod.us-east-1.auth.desktop.kiro.dev/login?idp=Google',
      redirect_uri: 'kiro://kiro.kiroAgent/authenticate-success',
      expires_in: 600
    })
    kiroMocks.exchangeSocialAuth.mockRejectedValue({ reason: 'KIRO_SOCIAL_TOKEN_FAILED', message: 'kiro social login failed' })
    const wrapper = mountPanel()

    await wrapper.get('input[value="google"]').setValue(true)
    await wrapper.get('button.btn-primary').trigger('click')
    await flushPromises()
    await wrapper.get('textarea').setValue('kiro://kiro.kiroAgent/authenticate-success?code=c&state=s')
    await wrapper.get('button.btn-primary').trigger('click')
    await flushPromises()

    expect(wrapper.text()).toContain('kiro social login failed')
    expect(wrapper.find('textarea').exists()).toBe(false)
    expect(wrapper.text()).toContain('admin.accounts.oauth.kiro.startLogin')
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
