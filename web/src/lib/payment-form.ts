// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 支付账户表单的**决策层**（spec 支付网关 v2 §18-3 / §13 P52）：纯函数，无 React。
//
// 为什么单独一层：协议元数据有三态，「表单能不能打开 / 能不能提交」是安全判定而不是
// 渲染细节。N49 的事故形状正是：`useState(() => initialConfig(...))` 在 fields 还没到
// 的那一刻求值，把 config 初始化成一串空串 → "每个字段都算改过" → 保存时把所有键提交成
// 空串；空串 ≠ `***`，不会被服务端当成"保持原值"，**密钥就没了**。
// 所以三态判定必须是一个能被 `node --test` 直接钉住的纯函数，页面只消费它的输出
// （openable / submittable / fields / config），不得在组件里重新推导。
//
// 输入里刻意**没有**「account → schema」的推导：loading/failed 态根本没有 schema，
// 把 fields 放在参数位上（而不是从 account 里挖）让"拿不到"这件事写在类型上。

import type { components } from '@/lib/api/schema'

/** 协议元数据的三态（§18-3）：ready 之外都不允许渲染可提交的配置表单。 */
export type MetaState = 'loading' | 'failed' | 'ready'

/** 服务端下发的 config 字段元数据（§18-2 / §4 ConfigField；`kind` 故意是 string 而非
 *  闭合 union——协议新增 kind 时旧前端必须还能编译，向前兼容不以编译失败为代价）。 */
// 这三个形状**不在这里声明**：它们就是 openapi 生成物（`gen:api` → `lib/api/schema.d.ts`）。
// 在这里另写一份等于给自己造第二个真相源——协议加字段时前端不会报错，只会静默少渲染一个框。
export type ConfigField = components['schemas']['ProviderConfigField']
export type PaymentProviderInfo = components['schemas']['ProviderMetadata']
export type PaymentProviderList = components['schemas']['ProviderMetadataList']

/** 账户读面里表单真正需要的部分：`config` 的 secret 字段已被服务端掩码成 `***`。 */
export type AccountLike = { config?: Record<string, unknown> | null }

/** secret 字段的掩码；原值回传 = 保持原值（§10-3）。 */
export const SECRET_MASK = '***'

export type FormState = {
  /** 能否打开配置表单（loading / failed → false） */
  openable: boolean
  /** 能否提交配置（loading / failed → false） */
  submittable: boolean
  /** ready 时来自 schema（保持协议声明顺序，**不排序**） */
  fields: ConfigField[]
  /** 表单初值；secret 字段回落到 `***`（新建时全为空串） */
  config: Record<string, string>
}

// 每次都给新对象：调用方拿到的 state 不允许被共享/改写。
const closed = (): FormState => ({ openable: false, submittable: false, fields: [], config: {} })

/**
 * 元数据三态 × 账户 → 表单状态（P52 的唯一决策落点）。
 *
 * - `loading` / `failed`：一律关死。**绝不能**返回一个"看起来可提交的空配置"——那正是
 *   密钥被清空的根源（§18-3 / N49）：空串不会被 `***` 合并当成"保持原值"。
 * - `ready`：fields = schema（原样、不排序），config 按字段名从账户取值，secret 回落 `***`。
 * - `account === null`（新建）：fields/config 来自 schema 的空串初值（secret 也要用户手填）。
 *
 * @param fields 选中协议的 `config_schema.fields`（元数据 ready 时才有；缺省空表只用于
 *   loading/failed 这类不关心字段的调用点）。
 */
export function resolveFormState(
  metaState: MetaState,
  account: AccountLike | null,
  fields: readonly ConfigField[] = [],
): FormState {
  if (metaState !== 'ready') return closed()
  const config: Record<string, string> = {}
  for (const f of fields) {
    if (account === null) {
      config[f.name] = ''
      continue
    }
    const raw = account.config?.[f.name]
    const value = typeof raw === 'string' ? raw : ''
    // secret 回落 `***`：键缺席或空串都按"保持原值"提交，绝不会退化成"清空"。
    config[f.name] = f.secret === true && value === '' ? SECRET_MASK : value
  }
  return { openable: true, submittable: true, fields: [...fields], config }
}

/** 控件类型：渲染层只需要认这三种（url/text 同形，差别只在 kind 的提示语义）。 */
export type ControlKind = 'password' | 'multiline' | 'text'

const KNOWN_KINDS: readonly string[] = ['text', 'password', 'multiline', 'url']

/** `kind` 是否是本前端认识的取值。 */
export function isKnownKind(kind: string): boolean {
  return KNOWN_KINDS.includes(kind)
}

/**
 * kind → 控件。`secret` 是秘密字段的**唯一**声明口（§18-2），所以未知 kind 必须
 * secret 感知降级（N43）：`secret:true` 的未知 kind 一律用 password 控件——秘密字段
 * 永不因为"前端不认识这个 kind"落成明文输入框。
 */
export function controlFor(field: ConfigField): ControlKind {
  switch (field.kind) {
    case 'password':
      return 'password'
    case 'multiline':
      return 'multiline'
    case 'url':
    case 'text':
      return 'text'
    default:
      return field.secret === true ? 'password' : 'text'
  }
}

/**
 * 未知 kind 的降级文案（§18-7：降级必须**可观测**，不能静默换控件）。null = 无需告警。
 * 含 provider 名与 kind 值，便于运维把一个 warning 直接对回协议实现。
 */
export function unknownKindWarning(provider: string, field: ConfigField): string | null {
  if (isKnownKind(field.kind)) return null
  return `[payment] provider "${provider}" declares unknown config field kind "${field.kind}" (field "${field.name}"); degrading to "${controlFor(field)}" control`
}

// 同一条 (provider, kind) 只告警一次：React 会重复渲染，运维不需要同一行刷屏。
const warned = new Set<string>()

/**
 * 渲染时调用：把未知 kind 的降级打到 `console.warn`，返回本次新发出的文案
 * （返回值供测试钉住"含 provider 名与 kind"）。
 */
export function warnUnknownKinds(provider: string, fields: readonly ConfigField[]): string[] {
  const emitted: string[] = []
  for (const field of fields) {
    const msg = unknownKindWarning(provider, field)
    if (msg === null) continue
    const key = `${provider}\u0000${field.kind}`
    if (warned.has(key)) continue
    warned.add(key)
    console.warn(msg)
    emitted.push(msg)
  }
  return emitted
}

/**
 * 字段标签优先级（§18-3）：i18n `paymentAccounts.field.<name>` → 协议自述 `label`（非空）
 * → 原始 `name`。服务端不做本地化，`label` 只是 `sk`/`pk` 这类不可读字段名的英文兜底。
 *
 * `translate` 命中时返回译文；i18next 缺 key 时原样返回 key（据此判定"没命中"）。
 */
export function fieldLabel(field: ConfigField, translate: (key: string) => string): string {
  const key = `paymentAccounts.field.${field.name}`
  const hit = translate(key)
  if (typeof hit === 'string' && hit !== '' && hit !== key) return hit
  const label = field.label?.trim()
  if (label) return label
  return field.name
}

/** react-query 查询态 → 元数据三态（pending 优先：首次加载失败时两者可能同时为真）。 */
export function metaStateOf(q: { isPending: boolean; isError: boolean }): MetaState {
  if (q.isPending) return 'loading'
  if (q.isError) return 'failed'
  return 'ready'
}

/**
 * 按 name 取协议元数据。元数据的唯一来源是协议实现（§18-2），前端不保留任何镜像，
 * 所以"未注册/元数据不可用"表现为 null —— 调用方据此关掉表单而不是回退到硬编码。
 */
export function providerOf(
  providers: readonly PaymentProviderInfo[],
  name: string | null | undefined,
): PaymentProviderInfo | null {
  if (!name) return null
  return providers.find(p => p.name === name) ?? null
}
