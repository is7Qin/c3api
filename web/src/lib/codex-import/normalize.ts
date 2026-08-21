import type { components } from '@/lib/api/schema'

export type CredentialKind = 'codex-oauth' | 'codex-pat'
export type OAuthItem = components['schemas']['CodexOAuthImportItem']
export type PATItem = components['schemas']['CodexPATImportItem']
export type NormalizedRow = { index: number; raw: unknown; item?: OAuthItem | PATItem; error?: string }

const emailRe = /^[^\s@]+@[^\s@]+\.[^\s@]+$/
const str = (v: unknown) => typeof v === 'string' ? v.trim() : v == null ? '' : String(v).trim()

export function normalizeExpired(value: unknown): string | undefined {
  if (value == null || value === '') return undefined
  const date = new Date(str(value))
  if (Number.isNaN(date.getTime())) throw new Error('invalidExpired')
  return date.toISOString()
}

function validateIdentity(email: string, accountId: string) {
  if (!email || !accountId) return '邮箱和账号 ID 为必填项'
  if (!emailRe.test(email) || email.includes('..')) return '邮箱格式无效'
  return undefined
}

export function normalizeRow(raw: unknown, kind: CredentialKind, index: number): NormalizedRow {
  if (!raw || typeof raw !== 'object' || Array.isArray(raw)) return { index, raw, error: '行必须是 JSON 对象' }
  const obj = raw as Record<string, unknown>
  const email = str(obj.email ?? obj.codex_email)
  const accountId = str(obj.account_id ?? obj.codex_account_id)
  const identityError = validateIdentity(email, accountId)
  if (identityError) return { index, raw, error: identityError }
  try {
    if (kind === 'codex-oauth') {
      const token = str(obj.access_token ?? obj.codex_oauth_token)
      const refresh = str(obj.refresh_token ?? obj.codex_oauth_refresh_token)
      if (!token || !refresh) return { index, raw, error: 'OAuth access_token 与 refresh_token 必须成对填写' }
      const item: OAuthItem = { codex_email: email, codex_account_id: accountId, codex_oauth_token: token, codex_oauth_refresh_token: refresh }
      const expired = normalizeExpired(obj.expired ?? obj.codex_oauth_expires_at)
      if (expired) item.codex_oauth_expires_at = expired
      if (typeof obj.weight === 'number' && obj.weight < 0) return { index, raw, error: 'weight 不能小于 0' }
      if (typeof obj.weight === 'number') item.weight = obj.weight
      if (typeof obj.max_concurrency === 'number') item.max_concurrency = obj.max_concurrency
      return { index, raw, item }
    }
    const headers = obj.headers && typeof obj.headers === 'object' ? obj.headers as Record<string, unknown> : undefined
    const auth = str(headers?.authorization)
    const key = (auth || str(obj.access_token ?? obj.codex_pat_key)).replace(/^Bearer\s+/i, '').trim()
    if (!key) return { index, raw, error: 'PAT 凭据不能为空' }
    const item: PATItem = { codex_email: email, codex_account_id: accountId, codex_pat_key: key }
    if (typeof obj.weight === 'number' && obj.weight < 0) return { index, raw, error: 'weight 不能小于 0' }
    if (typeof obj.weight === 'number') item.weight = obj.weight
    if (typeof obj.max_concurrency === 'number') item.max_concurrency = obj.max_concurrency
    return { index, raw, item }
  } catch (e) {
    return { index, raw, error: e instanceof Error && e.message === 'invalidExpired' ? 'expired 时间格式无效' : '行格式无效' }
  }
}
