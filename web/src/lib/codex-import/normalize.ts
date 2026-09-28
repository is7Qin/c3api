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

function validateIdentity(email: string, accountId: string, kind: CredentialKind) {
  if (!email) return '邮箱为必填项'
  // PAT 没有 JWT，空账号 ID 交给后端 whoami 补全；OAuth 只能从 access_token 离线解析。
  if (!accountId && kind !== 'codex-pat') return '账号 ID 为必填项，且无法从 access_token 解析'
  if (!emailRe.test(email) || email.includes('..')) return '邮箱格式无效'
  return undefined
}

// accountIdFromToken 从 ChatGPT access_token 的 payload 读取 chatgpt_account_id。
// 只解码、不验签；格式不合法或 claim 缺失时返回空字符串。
function accountIdFromToken(token: string) {
  const payload = token.split('.')[1]
  if (!payload) return ''
  try {
    const padded = payload.replace(/-/g, '+').replace(/_/g, '/').padEnd(Math.ceil(payload.length / 4) * 4, '=')
    const claims = JSON.parse(atob(padded)) as { 'https://api.openai.com/auth'?: { chatgpt_account_id?: unknown } }
    return str(claims['https://api.openai.com/auth']?.chatgpt_account_id)
  } catch {
    return ''
  }
}

export function normalizeRow(raw: unknown, kind: CredentialKind, index: number): NormalizedRow {
  if (!raw || typeof raw !== 'object' || Array.isArray(raw)) return { index, raw, error: '行必须是 JSON 对象' }
  const obj = raw as Record<string, unknown>
  const email = str(obj.email ?? obj.codex_email)
  const token = str(obj.access_token ?? obj.codex_oauth_token)
  const accountId = str(obj.account_id ?? obj.codex_account_id) || (kind === 'codex-oauth' ? accountIdFromToken(token) : '')
  const identityError = validateIdentity(email, accountId, kind)
  if (identityError) return { index, raw, error: identityError }
  try {
    if (kind === 'codex-oauth') {
      const refresh = str(obj.refresh_token ?? obj.codex_oauth_refresh_token)
      if (!token || !refresh) return { index, raw, error: 'OAuth access_token 与 refresh_token 必须成对填写' }
      const item: OAuthItem = { codex_email: email, codex_account_id: accountId, codex_oauth_token: token, codex_oauth_refresh_token: refresh }
      const expired = normalizeExpired(obj.expired ?? obj.codex_oauth_expires_at)
      if (expired) item.codex_oauth_expires_at = expired
      if (typeof obj.max_concurrency === 'number') item.max_concurrency = obj.max_concurrency
      return { index, raw, item }
    }
    const headers = obj.headers && typeof obj.headers === 'object' ? obj.headers as Record<string, unknown> : undefined
    const auth = str(headers?.authorization)
    const key = (auth || str(obj.access_token ?? obj.codex_pat_key)).replace(/^Bearer\s+/i, '').trim()
    if (!key) return { index, raw, error: 'PAT 凭据不能为空' }
    const item: PATItem = { codex_email: email, codex_account_id: accountId, codex_pat_key: key }
    if (typeof obj.max_concurrency === 'number') item.max_concurrency = obj.max_concurrency
    return { index, raw, item }
  } catch (e) {
    return { index, raw, error: e instanceof Error && e.message === 'invalidExpired' ? 'expired 时间格式无效' : '行格式无效' }
  }
}
