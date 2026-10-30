/**
 * Admin Kiro API endpoints
 * Handles Kiro credential import and AWS Builder ID device login for administrators
 */

import { apiClient } from '../client'

// 描述登录身份的凭据字段。重新授权时没有返回的字段要清空，否则旧登录的
// profile_arn、client_id 等会与新 token 合并在一起。
export const KIRO_IDENTITY_CREDENTIAL_KEYS = [
  'access_token',
  'refresh_token',
  'expires_at',
  'auth_method',
  'provider',
  'client_id',
  'client_secret',
  'client_secret_expires_at',
  'region',
  'sso_region',
  'profile_arn',
  'start_url'
] as const

// 补齐缺失的身份字段为空串，用于按字段合并的重新授权接口
export function withClearedKiroIdentity(credentials: Record<string, unknown>): Record<string, unknown> {
  const out: Record<string, unknown> = {}
  for (const key of KIRO_IDENTITY_CREDENTIAL_KEYS) {
    out[key] = ''
  }
  return { ...out, ...credentials }
}

export interface KiroImportRequest {
  credentials: string
  region?: string
  proxy_id?: number | null
}

export interface KiroCredentialsResponse {
  credentials: Record<string, unknown>
}

export interface KiroDeviceAuthRequest {
  region?: string
  proxy_id?: number | null
}

export interface KiroDeviceAuthResponse {
  session_id: string
  user_code: string
  verification_uri: string
  verification_uri_complete: string
  expires_in: number
  interval: number
}

export interface KiroDevicePollResponse {
  status: 'pending' | 'success'
  interval?: number
  credentials?: Record<string, unknown>
}

export async function importCredentials(payload: KiroImportRequest): Promise<KiroCredentialsResponse> {
  const { data } = await apiClient.post<KiroCredentialsResponse>('/admin/kiro/oauth/import', payload)
  return data
}

export async function startDeviceAuth(payload: KiroDeviceAuthRequest): Promise<KiroDeviceAuthResponse> {
  const { data } = await apiClient.post<KiroDeviceAuthResponse>('/admin/kiro/oauth/device-auth', payload)
  return data
}

export async function pollDeviceAuth(sessionId: string): Promise<KiroDevicePollResponse> {
  const { data } = await apiClient.post<KiroDevicePollResponse>('/admin/kiro/oauth/device-poll', {
    session_id: sessionId
  })
  return data
}

export default { importCredentials, startDeviceAuth, pollDeviceAuth }
