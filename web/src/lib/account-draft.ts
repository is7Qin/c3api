// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 账号表单的「稀疏草稿」模型：键缺席 = 用户没表态；清空只有 null（标量）/ []（集合）。
// 为什么：PATCH 契约本身就是三态（缺席=不变、null=清空、有值=落值），
// 而表单快照把「还没加载出来」和「用户清空」都表示成空值——线上因此清空过两次数据。
import type { components } from '@/lib/api/schema'
import { buildAccountPatch, proxyDirect, proxySelectionOf, type ProxySelection } from './account-proxy.ts'
import { parseMultiplier } from './account-config.ts'

export type AccountConfigPatch = components['schemas']['AccountConfigPatch']
export type AccountView = components['schemas']['AccountView']

// 回显基线：可直接展示的全部字段（可空字段保留契约的 null，展示时再 ?? ''）
export interface AccountValues {
  name: string
  template_id: string
  base_url: string | null
  upstream_key: string
  max_concurrency: string
  group_ids: number[]
  cost_multiplier: string
  cache_domain: string | null
  supplier_user_id: number | null
  proxy: ProxySelection
}

export type AccountDraft = Partial<AccountValues>
export type BatchDraft = AccountDraft & { enabled?: boolean }

// 字段类别：加字段漏登记 → tsc 失败（本文件在 src 内，受 tsconfig.app 检查）
export const FIELD_KINDS = {
  name: 'scalar',
  template_id: 'scalar',
  base_url: 'nullable',
  upstream_key: 'scalar',
  max_concurrency: 'scalar',
  group_ids: 'collection',
  cost_multiplier: 'scalar',
  cache_domain: 'nullable',
  supplier_user_id: 'nullable',
  proxy: 'choice',
} satisfies Record<keyof AccountValues, 'scalar' | 'nullable' | 'collection' | 'choice'>

// 纯写入原语（页面只经 update/clear 调用；update 不接受 undefined = 从源头不打洞）
export function withField<K extends keyof BatchDraft>(d: BatchDraft, k: K, v: Exclude<BatchDraft[K], undefined>): BatchDraft {
  return { ...d, [k]: v }
}
export function withoutField(d: BatchDraft, k: keyof BatchDraft): BatchDraft {
  const n = { ...d }
  delete n[k]
  return n
}

// 派生视图：基线 ⊕ 草稿（滤掉 undefined 洞）
export function mergeView(base: AccountValues, draft: AccountDraft): AccountValues {
  const out: AccountValues = { ...base }
  for (const [k, v] of Object.entries(draft)) {
    if (v !== undefined) (out as unknown as Record<string, unknown>)[k] = v
  }
  return out
}

// 创建态基线：全空。max_concurrency 刻意留空——缺席即「取后端配置缺省」，
// 预填数字会把它变成一次显式写入（旧表单的 8 就是这么来的）。
export function defaults(): AccountValues {
  return {
    name: '',
    template_id: '',
    base_url: null,
    upstream_key: '',
    max_concurrency: '',
    group_ids: [],
    cost_multiplier: '1',
    cache_domain: null,
    supplier_user_id: null,
    proxy: proxyDirect(),
  }
}

// 编辑态基线：AccountView（列表行）+ 分组回显（GET /accounts/{id}/groups，不随列表返回）。
// 可空字段回落契约的 null（而不是 ''）——'' 只表示「用户清空了输入框」，基线里没有它。
export function fromAccountView(a: AccountView, groupIds: number[]): AccountValues {
  const ct = a.Template?.CredentialType
  const codex = ct === 'codex-oauth' || ct === 'codex-pat'
  return {
    name: a.Name ?? '',
    template_id: String(a.TemplateID ?? ''),
    // Codex 不可设置 BaseURL（SDK 默认端点）
    base_url: codex ? null : (a.BaseURL ?? null),
    upstream_key: a.UpstreamKey ?? '',
    max_concurrency: String(a.MaxConcurrency ?? 8),
    group_ids: [...groupIds],
    cost_multiplier: String(a.UpstreamCostMultiplier ?? 1),
    cache_domain: a.CacheDomain ?? null,
    supplier_user_id: a.supplier_user_id ?? null,
    // proxy_id 非 null ⇒ 该代理对当前调用方可见；null ⇒ 直连**或**绑着一条对调用方
    // 不可见的代理——两种情形在 UI 上无法区分，故投影侧只认草稿（buildAccountPatch）。
    proxy: proxySelectionOf(a.proxy_id),
  }
}

// codex 上下文四分：mixed / unresolved 下 BaseURL 一律不发（批量无法按行分流）。
export type TplCtx = 'codex' | 'nonCodex' | 'mixed' | 'unresolved'

// 唯一投影：草稿 → AccountConfigPatch（创建与编辑共用）
export function draftToPatch(
  d: BatchDraft,
  ctx: {
    tpl: TplCtx
    echoProxyId: number | null
    isSupplierScope: boolean
    /** 批量：调用方已用 buildBatchProxyFields 算好；提供时优先于草稿+echo 的投影 */
    proxyFields?: Pick<AccountConfigPatch, 'proxy_id'>
  },
): AccountConfigPatch {
  const p: AccountConfigPatch = {}
  // 不可空标量的 '' 是「用户清空了必填框」的非法瞬时值（校验层拦），投影里不发。
  if (d.name !== undefined && d.name.trim() !== '') p.name = d.name.trim()
  if (d.template_id !== undefined && d.template_id !== '') p.template_id = Number(d.template_id)
  // base_url：codex 恒 null（模板可能随本次 PATCH 切换）；mixed/unresolved 一律不发
  if (ctx.tpl === 'codex') p.base_url = null
  else if (ctx.tpl === 'nonCodex' && d.base_url !== undefined) {
    if (d.base_url === null) p.base_url = null
    else {
      // 纯空白 = 用户把输入框清空的另一种写法（控件只把精确 '' 映射成 null）。
      // 线上 '' 是 400（handler.nullableScalar），故按「未表态」处理——
      // 清空只有一个拼法：null。
      const u = d.base_url.trim()
      if (u !== '') p.base_url = u
    }
  }
  if (d.upstream_key !== undefined && d.upstream_key !== '') p.upstream_key = d.upstream_key
  if (d.max_concurrency !== undefined && d.max_concurrency !== '') p.max_concurrency = Number(d.max_concurrency)
  if (d.group_ids !== undefined) p.group_ids = [...d.group_ids]
  if (d.cost_multiplier !== undefined) {
    const m = parseMultiplier(d.cost_multiplier)
    if (m === null) throw new Error('invalid multiplier') // 校验层本应拦截
    p.upstream_cost_multiplier = m
  }
  if (d.cache_domain !== undefined) {
    if (d.cache_domain === null) p.cache_domain = null
    else {
      const v = d.cache_domain.trim()
      if (v !== '') p.cache_domain = v // 纯空白同上：不发 ''（400）
    }
  }
  if (d.supplier_user_id !== undefined && !ctx.isSupplierScope) p.supplier_user_id = d.supplier_user_id
  if (ctx.proxyFields !== undefined) Object.assign(p, ctx.proxyFields)
  else if (d.proxy !== undefined) Object.assign(p, buildAccountPatch(ctx.echoProxyId, d.proxy))
  if (d.enabled !== undefined) p.enabled = d.enabled
  return p
}

export function assertCreate(d: AccountDraft): void {
  if (!d.name || !d.name.trim()) throw new Error('name is required')
  if (!d.template_id) throw new Error('template_id is required')
}
