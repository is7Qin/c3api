// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 薄 fetch 封装：token 注入、401 归一化、类型化返回（schema.d.ts 生成）。
// 响应字段为 Go 大写风格（ID/Name/...），前端按此使用，不做 camelCase 转换。
import type { components } from './schema.d.ts'
import { userAuth } from '@/lib/auth'

// 类实现（brief 原为 type 别名，但 throw new ApiError(...) 需要运行时值）
export class ApiError extends Error {
  status: number
  constructor(status: number, message: string) {
    super(message)
    this.status = status
    this.name = 'ApiError'
  }
}

// —— 列表查询参数（三页通用 + 专属）——
export interface ListParams {
  limit?: number
  offset?: number
  name?: string
  sort?: string
  order?: 'asc' | 'desc'
}
export type TemplateListParams = ListParams
export interface AccountListParams extends ListParams {
  status?: string
  template_id?: number
}
export type GroupListParams = ListParams

// —— 日志/统计查询参数（usage/err 游标分页；from/to 必填）——
export interface UsageLogParams {
  limit?: number
  cursor?: number
  group_id?: number
  account_id?: number
  user_id?: number
  key_id?: number
  model?: string
  format?: string
  error_type?: string
  from: string
  to: string
}
export interface ErrLogParams extends UsageLogParams {
  status_code?: number
}
// 用户端无 user_id 过滤（服务端强制本人），也无 account_id（用户级契约不含该参数）
export type MyUsageLogParams = Omit<UsageLogParams, 'user_id' | 'account_id'>
export type MyErrLogParams = Omit<ErrLogParams, 'user_id' | 'account_id'>
export interface UserStatParams {
  from: string
  to: string
  granularity?: 'hour' | 'day'
  model?: string
}

// 过滤 undefined/null/空串，返回 '' 或 '?k=v&...' 查询串。
export function toQuery(p?: object): string {
  if (!p) return ''
  const qs = new URLSearchParams()
  for (const [k, v] of Object.entries(p)) {
    if (v === undefined || v === null || v === '') continue
    qs.set(k, String(v))
  }
  const s = qs.toString()
  return s ? `?${s}` : ''
}

// base：管理端 '/api/admin' / 用户端 '/api/user'；token 统一由 userAuth 注入（一套登录态）。
// 架构约束：所有 API 统一收口于 /api/*，前端 SPA 与 API 无前缀重叠。
export class ApiClient {
  private base: string
  private getToken: () => string | null
  constructor(getToken: () => string | null, base: string = '/api/admin') {
    this.getToken = getToken
    this.base = base
  }

  // init.params 为 toQuery 产出的查询串（'' 或 '?k=v&...'），附加到 path 之后。
  private async request<T>(path: string, init?: RequestInit & { params?: string }): Promise<T> {
    const token = this.getToken()
    const { params, ...rest } = init ?? {}
    const headers: Record<string, string> = { 'Content-Type': 'application/json', ...(rest.headers as Record<string, string> | undefined) }
    if (token) headers['Authorization'] = `Bearer ${token}`
    const qs = params ?? ''
    const url = `${this.base}${path}${qs ? (qs.startsWith('?') ? qs : `?${qs}`) : ''}`
    const res = await fetch(url, { ...rest, headers })
    if (res.status === 401) throw new ApiUnauthorized()
    if (!res.ok) {
      const body = await res.json().catch(() => null)
      throw new ApiError(res.status, (body as { error?: string } | null)?.error ?? `HTTP ${res.status}`)
    }
    // DELETE /rules/{id} 等返回 204 无 body，不能 res.json()
    if (res.status === 204) return undefined as T
    return res.json() as Promise<T>
  }
  // —— 模板 ——
  listTemplates = (p?: TemplateListParams) => this.request<components['schemas']['TemplateListResponse']>('/templates', { params: toQuery(p) })
  createTemplate = (b: components['schemas']['TemplateCreate']) => this.request<components['schemas']['Template']>('/templates', { method: 'POST', body: JSON.stringify(b) })
  updateTemplate = (id: number, b: components['schemas']['TemplateCreate']) => this.request<components['schemas']['Template']>(`/templates/${id}`, { method: 'PUT', body: JSON.stringify(b) })
  deleteTemplate = (id: number) => this.request<components['schemas']['DeletedResponse']>(`/templates/${id}`, { method: 'DELETE' })
  deleteTemplatesBatch = (ids: number[]) => this.request<components['schemas']['BatchDeleteResponse']>('/templates/batch-delete', { method: 'POST', body: JSON.stringify({ ids }) })
  updateTemplatesBatch = (ids: number[], fields: components['schemas']['TemplatePatch']) => this.request<components['schemas']['BatchUpdateResponse']>('/templates/batch-update', { method: 'POST', body: JSON.stringify({ ids, fields }) })
  getTemplateExt = (id: number) => this.request<components['schemas']['TemplateExt']>(`/templates/${id}/ext`)
  putTemplateExt = (id: number, b: components['schemas']['TemplateExt']) => this.request<components['schemas']['TemplateExt']>(`/templates/${id}/ext`, { method: 'PUT', body: JSON.stringify(b) })
  // —— 账号 ——
  listAccounts = (p?: AccountListParams) => this.request<components['schemas']['AccountListResponse']>('/accounts', { params: toQuery(p) })
  createAccount = (b: components['schemas']['AccountCreate']) => this.request<components['schemas']['Account']>('/accounts', { method: 'POST', body: JSON.stringify(b) })
  updateAccount = (id: number, b: components['schemas']['AccountCreate']) => this.request<components['schemas']['Account']>(`/accounts/${id}`, { method: 'PUT', body: JSON.stringify(b) })
  deleteAccount = (id: number) => this.request<components['schemas']['DeletedResponse']>(`/accounts/${id}`, { method: 'DELETE' })
  deleteAccountsBatch = (ids: number[]) => this.request<components['schemas']['BatchDeleteResponse']>('/accounts/batch-delete', { method: 'POST', body: JSON.stringify({ ids }) })
  updateAccountsBatch = (ids: number[], fields: components['schemas']['AccountPatch']) => this.request<components['schemas']['BatchUpdateResponse']>('/accounts/batch-update', { method: 'POST', body: JSON.stringify({ ids, fields }) })
  resetAccountsCooldown = (ids: number[]) => this.request<components['schemas']['BatchResetCooldownResponse']>('/accounts/batch-reset-cooldown', { method: 'POST', body: JSON.stringify({ ids }) })
  // —— 分组 ——
  listGroups = (p?: GroupListParams) => this.request<components['schemas']['GroupListResponse']>('/groups', { params: toQuery(p) })
  createGroup = (b: components['schemas']['GroupCreate']) => this.request<components['schemas']['Group']>('/groups', { method: 'POST', body: JSON.stringify(b) })
  updateGroup = (id: number, b: components['schemas']['GroupCreate']) => this.request<components['schemas']['Group']>(`/groups/${id}`, { method: 'PUT', body: JSON.stringify(b) })
  deleteGroup = (id: number) => this.request<components['schemas']['DeletedResponse']>(`/groups/${id}`, { method: 'DELETE' })
  deleteGroupsBatch = (ids: number[]) => this.request<components['schemas']['BatchDeleteResponse']>('/groups/batch-delete', { method: 'POST', body: JSON.stringify({ ids }) })
  updateGroupsBatch = (ids: number[], fields: components['schemas']['GroupPatch']) => this.request<components['schemas']['BatchUpdateResponse']>('/groups/batch-update', { method: 'POST', body: JSON.stringify({ ids, fields }) })
  getAccountGroups = (id: number) => this.request<components['schemas']['AccountGroupsResponse']>(`/accounts/${id}/groups`)
  getAccountExt = (id: number) => this.request<components['schemas']['AccountExt']>(`/accounts/${id}/ext`)
  putAccountExt = (id: number, b: components['schemas']['AccountExt']) => this.request<components['schemas']['AccountExt']>(`/accounts/${id}/ext`, { method: 'PUT', body: JSON.stringify(b) })
  // —— 账号用量聚合（0e77d2a）：批量 ≤100 条；from/to 缺省 = 当天（UTC 零点 → now）。
  // codex 账号附带上游额度快照（upstream）；api-key/无凭据账号恒 null。
  listAccountsUsage = (accountIds: number[], p?: { from?: string; to?: string }) =>
    this.request<components['schemas']['AccountsUsageResponse']>('/accounts/usage', {
      params: toQuery({ account_ids: accountIds.join(','), ...p }),
    })
  // —— codex 凭据批量导入（Task B；行级失败归 failed——HTTP 恒 200）——
  importCodexOauthAccounts = (b: components['schemas']['CodexOAuthImportBody']) => this.request<components['schemas']['ImportResult']>('/accounts/batch-import-codex-oauth', { method: 'POST', body: JSON.stringify(b) })
  importCodexPatAccounts = (b: components['schemas']['CodexPATImportBody']) => this.request<components['schemas']['ImportResult']>('/accounts/batch-import-codex-pat', { method: 'POST', body: JSON.stringify(b) })
  // —— 规则 ——
  listRules = (p?: { enabled?: boolean }) => this.request<components['schemas']['RuleListResponse']>('/rules', { params: toQuery(p) })
  createRule = (b: components['schemas']['RuleCreate']) => this.request<components['schemas']['Rule']>('/rules', { method: 'POST', body: JSON.stringify(b) })
  updateRule = (id: number, b: components['schemas']['RulePatch']) => this.request<components['schemas']['Rule']>(`/rules/${id}`, { method: 'PUT', body: JSON.stringify(b) })
  deleteRule = (id: number) => this.request<components['schemas']['DeletedResponse']>(`/rules/${id}`, { method: 'DELETE' })
  deleteRulesBatch = (ids: number[]) => this.request<components['schemas']['BatchDeleteResponse']>('/rules/batch-delete', { method: 'POST', body: JSON.stringify({ ids }) })
  // —— 日志 / 统计 ——
  getUsageLogs = (p: UsageLogParams) => this.request<components['schemas']['LogsResponse']>('/usage_logs', { params: toQuery(p) })
  getErrLogs = (p: ErrLogParams) => this.request<components['schemas']['ErrLogsResponse']>('/err_logs', { params: toQuery(p) })
  // —— 统计 v2 ——
  getStatsTrend = (p: { from: string; to: string; granularity: 'hour' | 'day'; group_id?: number; model?: string }) =>
    this.request<components['schemas']['StatTrendPoint'][]>('/stats/trend', { params: toQuery(p) })
  getStatsTop = (p: { from: string; to: string; entity: 'account' | 'user' | 'key'; by: 'cost' | 'requests' | 'tokens'; limit?: number }) =>
    this.request<components['schemas']['StatTopEntry'][]>('/stats/top', { params: toQuery(p) })
  getStatsEntityTrend = (p: { entity: 'account' | 'user' | 'key'; id: number; from: string; to: string; granularity: 'hour' | 'day'; model?: string }) =>
    this.request<components['schemas']['StatTrendPoint'][]>('/stats/entity-trend', { params: toQuery(p) })
  getStatsTTFT = (p: { from: string; to: string; entity?: 'account' | 'user' | 'key'; id?: number; model?: string }) =>
    this.request<components['schemas']['StatTTFTSummary']>('/stats/ttft', { params: toQuery(p) })
  // —— 用户管理 ——
  listUsers = (p?: { limit?: number; offset?: number; email?: string; sort?: string; order?: 'asc' | 'desc' }) => this.request<components['schemas']['UserListResponse']>('/users', { params: toQuery(p) })
  createUser = (b: components['schemas']['UserCreate']) => this.request<components['schemas']['User']>('/users', { method: 'POST', body: JSON.stringify(b) })
  updateUser = (id: number, b: components['schemas']['UserUpdate']) => this.request<components['schemas']['User']>(`/users/${id}`, { method: 'PUT', body: JSON.stringify(b) })
  setGroupAssignments = (id: number, b: components['schemas']['GroupAssignmentsBody']) => this.request<components['schemas']['GroupAssignmentsResponse']>(`/groups/${id}/assignments`, { method: 'PUT', body: JSON.stringify(b) })
  getGroupAssignments = (id: number) => this.request<components['schemas']['GroupAssignmentsResponse']>(`/groups/${id}/assignments`)
  getUserGroups = (id: number) => this.request<components['schemas']['UserGroupsResponse']>(`/users/${id}/groups`)
  setUserGroups = (id: number, b: components['schemas']['UserGroupsBody']) => this.request<components['schemas']['UserGroupsResponse']>(`/users/${id}/groups`, { method: 'PUT', body: JSON.stringify(b) })
  // —— 密钥（管理端脱敏搜索列表——不含 key 明文；/app/logs 筛选候选消费；
  //     user_id/group_id 关联收窄：已选筛选条件下候选限缩到该用户/分组）——
  listKeys = (p?: { limit?: number; offset?: number; name?: string; user_id?: number; group_id?: number; sort?: string; order?: 'asc' | 'desc' }) => this.request<components['schemas']['AdminKeyListResponse']>('/keys', { params: toQuery(p) })
  // —— 设置 ——
  getSettings = () => this.request<components['schemas']['Setting'][]>('/settings')
  updateSetting = (b: components['schemas']['SettingUpdate']) => this.request<components['schemas']['Setting'][]>('/settings', { method: 'PUT', body: JSON.stringify(b) })
  // —— 兑换码 ——
  listRedemptionCodes = (p?: { page?: number; page_size?: number; type?: string; status?: string; sort?: string; order?: 'asc' | 'desc' }) => this.request<components['schemas']['RedemptionCodeListResponse']>('/redemption-codes', { params: toQuery(p) })
  generateRedemptionCodes = (b: components['schemas']['GenerateRequest']) => this.request<components['schemas']['GenerateResponse']>('/redemption-codes', { method: 'POST', body: JSON.stringify(b) })
  deactivateRedemptionCode = (id: number) => this.request<components['schemas']['DeactivateResponse']>(`/redemption-codes/${id}/deactivate`, { method: 'POST' })
  deactivateRedemptionCodesBatch = (ids: number[]) => this.request<components['schemas']['BatchDeactivateResponse']>('/redemption-codes/batch-deactivate', { method: 'POST', body: JSON.stringify({ ids }) })
  getRedemptionCodeUses = (id: number) => this.request<components['schemas']['RedemptionUseListResponse']>(`/redemption-codes/${id}/uses`)
  // —— 定价（统一 prices API：mode token|call|image 覆盖旧三表）——
  syncPricing = () => this.request<components['schemas']['PricingSyncResponse']>('/pricing/sync', { method: 'POST' })
  syncPricingPreview = () => this.request<components['schemas']['PricingSyncPreviewResponse']>('/pricing/sync/preview', { method: 'POST' })
  listPriceEntries = (p?: { page?: number; page_size?: number; mode?: string; source?: string; provider?: string; model?: string; sort?: string; order?: 'asc' | 'desc' }) => this.request<components['schemas']['PriceEntryListResponse']>('/prices', { params: toQuery(p) })
  // 模型 ID 含 "/"（openai/gpt-5.6-sol）——一律查询参数，禁路径拼接
  getPriceEntry = (model: string) => this.request<components['schemas']['PriceEntry']>('/prices/entry', { params: toQuery({ model }) })
  upsertPriceEntry = (model: string, b: components['schemas']['PriceEntryUpsert']) => this.request<components['schemas']['PriceEntry']>('/prices/entry', { method: 'PUT', params: toQuery({ model }), body: JSON.stringify(b) })
  deletePriceEntry = (model: string) => this.request<components['schemas']['DeletedResponse']>('/prices/entry', { method: 'DELETE', params: toQuery({ model }) })
  listPriceVariants = (model: string) => this.request<components['schemas']['PriceVariantListResponse']>('/prices/variants', { params: toQuery({ model }) })
  putPriceVariants = (model: string, b: components['schemas']['PriceVariantListRequest']) => this.request<components['schemas']['PriceVariantListResponse']>('/prices/variants', { method: 'PUT', params: toQuery({ model }), body: JSON.stringify(b) })
  deletePriceVariants = (model: string) => this.request<components['schemas']['DeletedResponse']>('/prices/variants', { method: 'DELETE', params: toQuery({ model }) })
  // —— 管理端总览（/api/admin/overview + /api/admin/users-top；聚合面 30s / 实时面 2s
  // 服务端 TTL 缓存，dashboard 轮询频率下无陈旧感）——
  getOverview = (p?: { days?: number; group_id?: number }) => this.request<components['schemas']['OverviewResponse']>('/overview', { params: toQuery(p) })
  getUsersTop = (p?: { top?: number }) => this.request<components['schemas']['UsersTopResponse']>('/users-top', { params: toQuery(p) })
  // —— 运维观测（/api/admin/ops/workers；管理端专属，契约 ops tag 生成类型）——
  getOpsWorkers = () => this.request<components['schemas']['WorkersResponse']>('/ops/workers')
  // 管理侧临时额度全量分页（/api/admin/temp-balances；/app/users 查看入口消费）
  getAdminTempBalances = (p?: { page?: number; page_size?: number; user_id?: number; sort?: string; order?: 'asc' | 'desc' }) =>
    this.request<components['schemas']['AdminTempBalancesResponse']>('/temp-balances', { params: toQuery(p) })
  // —— 用户端（userApi 专属；token 用 userAuth）——
  register = (b: components['schemas']['UserAuthRegister']) => this.request<components['schemas']['UserAuthResponse']>('/auth/register', { method: 'POST', body: JSON.stringify(b) })
  login = (b: components['schemas']['UserAuthLogin']) => this.request<components['schemas']['UserAuthResponse']>('/auth/login', { method: 'POST', body: JSON.stringify(b) })
  me = () => this.request<components['schemas']['User']>('/auth/me')
  listUserGroups = () => this.request<components['schemas']['Group'][]>('/groups')
  listUserKeys = (p?: ListParams) => this.request<components['schemas']['KeyListResponse']>('/keys', { params: toQuery(p) })
  createUserKey = (b: components['schemas']['KeyCreate']) => this.request<components['schemas']['Key']>('/keys', { method: 'POST', body: JSON.stringify(b) })
  updateUserKey = (id: number, b: components['schemas']['KeyUpdate']) => this.request<components['schemas']['Key']>(`/keys/${id}`, { method: 'PUT', body: JSON.stringify(b) })
  deleteUserKey = (id: number) => this.request<components['schemas']['DeletedResponse']>(`/keys/${id}`, { method: 'DELETE' })
  rotateUserKey = (id: number) => this.request<components['schemas']['Key']>(`/keys/${id}/rotate`, { method: 'POST' })
  getMyUsageLogs = (p: MyUsageLogParams) => this.request<components['schemas']['UserLogsResponse']>('/usage_logs', { params: toQuery(p) })
  getMyErrLogs = (p: MyErrLogParams) => this.request<components['schemas']['UserErrLogsResponse']>('/err_logs', { params: toQuery(p) })
  getMyStats = (p: UserStatParams) => this.request<components['schemas']['StatTrendPoint'][]>('/stats', { params: toQuery(p) })
  getMyStatsTTFT = (p: { from: string; to: string; model?: string }) =>
    this.request<components['schemas']['StatTTFTSummary']>('/stats/ttft', { params: toQuery(p) })
  redeem = (code: string) => this.request<components['schemas']['RedeemResponse']>('/redemptions', { method: 'POST', body: JSON.stringify({ code }) })
  listUserRedemptions = (p?: { page?: number; page_size?: number; sort?: string; order?: 'asc' | 'desc' }) => this.request<components['schemas']['RedemptionRecordListResponse']>('/redemptions', { params: toQuery(p) })
  getTempBalances = () => this.request<components['schemas']['TempBalancesResponse']>('/temp-balances')
  changePassword = (b: components['schemas']['UserAuthChangePassword']) => this.request<components['schemas']['ChangePasswordResponse']>('/auth/change-password', { method: 'POST', body: JSON.stringify(b) })
  registerCode = (b: components['schemas']['RegisterCodeRequest']) => this.request<components['schemas']['SentResponse']>('/auth/register-code', { method: 'POST', body: JSON.stringify(b) })
  forgotPassword = (b: components['schemas']['ForgotPasswordRequest']) => this.request<components['schemas']['SentResponse']>('/auth/forgot-password', { method: 'POST', body: JSON.stringify(b) })
  resetPassword = (b: components['schemas']['ResetPasswordRequest']) => this.request<components['schemas']['ChangePasswordResponse']>('/auth/reset-password', { method: 'POST', body: JSON.stringify(b) })
  // —— 邮件模板（管理端）——
  getMailTemplates = () => this.request<components['schemas']['MailTemplate'][]>('/mail/templates')
  putMailTemplate = (purpose: string, b: components['schemas']['MailTemplateUpdate']) => this.request<components['schemas']['MailTemplate']>(`/mail/templates/${purpose}`, { method: 'PUT', body: JSON.stringify(b) })
}

export class ApiUnauthorized extends Error {
  constructor() { super('unauthorized'); this.name = 'ApiUnauthorized' }
}

// 用户端实例：base '/api/user'，Authorization 走 userAuth（c3api_user_token）。
export const userApi = new ApiClient(userAuth.getToken, '/api/user')
