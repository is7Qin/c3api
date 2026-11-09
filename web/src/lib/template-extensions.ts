// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 模板类型化扩展（template_ext）的前端纯逻辑：custom 的逐格式上游路径键集合、
// codex 的**出站传输**（与入站 `openai-responses-ws` 同名不同义）、留空不上送、
// 「必须先读回既有配置才能保存」的门禁、两处写面共用的 body 组装，以及 409
// 降级确认的决策。抽成纯函数是为了能被 node --test 直接覆盖——页面级测试不在
// 本项目的能力范围内，而门禁/组装判断错了会静默清空配置或静默跳过 409 门。

/**
 * custom 类型的逐格式路径键（与后端 service / openapi 键集合一致）。
 *
 * 输入框里的值会**原样**拼到模板 base_url 之后（不做任何补全：写 chat/completions
 * 就打 /chat/completions，写 v1/chat/completions 就打 /v1/chat/completions）。
 * placeholder 只是「留空时该格式默认端点」的示例，方便直接照抄；它不是格式要求，
 * 也不限制前缀。
 */
export const CUSTOM_PATH_FIELDS: { key: string; placeholder: string }[] = [
  { key: 'openai-chat', placeholder: 'v1/chat/completions' },
  { key: 'openai-responses', placeholder: 'v1/responses' },
  { key: 'openai-responses-ws', placeholder: 'v1/responses' },
  { key: 'anthropic', placeholder: 'v1/messages' },
  { key: 'openai-search', placeholder: 'v1/alpha/search' },
  { key: 'openai-images.generations', placeholder: 'v1/images/generations' },
  { key: 'openai-images.edits', placeholder: 'v1/images/edits' },
]

/** codex 号池类型：出站传输只对它们有意义（其余类型提交非空值 → 400）。 */
export function isCodexCredentialType(credentialType: string): boolean {
  return credentialType === 'codex-oauth' || credentialType === 'codex-pat'
}

/** 降级确认令牌（与 service 的字面量契约一致：`?confirm=transport-downgrade`）。 */
export const TRANSPORT_DOWNGRADE_CONFIRM = 'transport-downgrade'

/** GET /templates/{id}/ext 的响应形态；null = 没有 ext 行。 */
export type TemplateExtEcho = {
  upstream_paths?: Record<string, string> | null
  strip_image_tools?: boolean | null
  /** ""（默认=http） / "http" / "ws"；后端恒回显（含 ""）。 */
  codex_responses_transport?: string | null
} | null

/** 写面 body 的字段来源：两处写面（主表单链式写 + 扩展对话框）共用。
 *
 * `strip_image_tools` 保留三态：对话框能表达"未配置"（null），主表单只有开/关。 */
export type ExtWriteFields = {
  strip_image_tools: boolean | null
  upstream_paths: Record<string, string>
  /** "" 是合法值（未配置 = 默认 http）——不做归一/折叠。 */
  codex_responses_transport: string
}

/** 表单里与 ext 对应的字段（表单没有"未配置"这一态）。 */
export type ExtFormFields = ExtWriteFields & { strip_image_tools: boolean }

/** ext 写面 body（与 `components['schemas']['TemplateExt']` 结构兼容）。
 *
 * 本文件不 import schema 类型：`node --test` 直接跑 TS（零依赖、零路径别名），
 * 调用点再用 `const body: TemplateExt = extWriteBody(...)` 把契约钉住。 */
export type TemplateExtWriteBody = {
  credential_type: 'responses-special' | 'codex-oauth' | 'codex-pat' | 'custom'
  strip_image_tools: boolean | null
  codex_responses_transport?: string
  upstream_paths?: Record<string, string>
}

/**
 * ext 回显 → 表单字段。
 *
 * 三种输入必须区分开（合并它们会把两种相反的处境当成一回事）：
 * - `undefined` = **没读取到**（查询未成功/进行中/失败）→ 返回 null，调用方既不得
 *   清空表单，也不得认为已同步（否则一次保存就把已有路径写成 NULL）；
 * - `null` = 读取成功但**没有 ext 行**（如只用 API 建过的 custom 模板）→ 空字段，
 *   这确实就是「未配置」，可以标记已同步（否则这类模板会永远存不进去）；
 * - 对象 = 正常回显。
 */
export function extFormFields(ext: TemplateExtEcho | undefined): ExtFormFields | null {
  if (ext === undefined) return null
  if (ext === null) return { strip_image_tools: false, upstream_paths: {}, codex_responses_transport: '' }
  return {
    strip_image_tools: ext.strip_image_tools === true,
    // 未配置的键不出现：输入框留空 = 该格式走默认端点
    upstream_paths: { ...(ext.upstream_paths ?? {}) },
    // "" 本身有意义（默认面）：只把 null/undefined 兜成 ""，其余原样。
    codex_responses_transport: ext.codex_responses_transport ?? '',
  }
}

/**
 * 回显 effect 是否该把 ext 结果写回表单。
 *
 * 三个条件必须同时成立：有正在编辑的模板、对话框打开、读取成功。`dialogOpen`
 * 在条件里是有意的：关掉再打开同一模板时缓存数据引用不变、effect 不重跑，
 * 只有把「对话框打开」也当作触发条件，重置后的表单才会被重新灌一次。
 */
export function shouldSyncEcho(opts: {
  editingId: number | null
  dialogOpen: boolean
  extReadOk: boolean
}): boolean {
  return opts.editingId !== null && opts.dialogOpen && opts.extReadOk
}

/**
 * 保存前是否必须拦住：编辑态且表单还没同步过该模板的 ext 回显。
 *
 * 覆盖 `custom`（upstream_paths）**与** codex 两类（codex_responses_transport）：
 * 写面是全列替换，回显没走完就写等于把已有配置清成默认——custom 丢 7 条路径，
 * codex 把 `ws` 清回默认 http（一次「只改个名字」的保存就够）。
 *
 * 判据是「syncedFor === editingId」而不是查询状态：关闭再打开同一模板时缓存
 * 数据引用不变、effect 不重跑，而表单已被重置成空表——只看 isLoading 会把
 * 「被重置的空表单」当成已回显。
 */
export function extEchoBlocked(
  credentialType: string,
  editingId: number | null,
  syncedFor: number | null,
): boolean {
  if (editingId === null) return false // 新建：没有既有配置可清空
  if (credentialType !== 'custom' && !isCodexCredentialType(credentialType)) return false // 其余类型不写 ext 字段
  return syncedFor !== editingId
}

/**
 * ext 写面 body 的**唯一**组装点（两处写面共用）：全列替换，三字段一起发。
 *
 * - `upstream_paths` 只有 custom 发：其余类型提交非空 map → 400；
 * - `codex_responses_transport` 只有 codex 发：非 codex 下 `"http"`/`"ws"` 永远不生效
 *   （→ 400），而陈旧行残留值是否"恰好等于库里的原值"（幂等例外）在 web 侧无法判定，
 *   所以不猜——省略即清空；旧值确实是 `ws` 时由服务端 409 门接住，用户确认后再降级。
 */
export function extWriteBody(f: ExtWriteFields, credentialType: string): TemplateExtWriteBody {
  const body: TemplateExtWriteBody = {
    credential_type: credentialType as TemplateExtWriteBody['credential_type'],
    strip_image_tools: f.strip_image_tools,
  }
  if (credentialType === 'custom') body.upstream_paths = nonEmptyPaths(f.upstream_paths)
  if (isCodexCredentialType(credentialType)) body.codex_responses_transport = f.codex_responses_transport
  return body
}

/**
 * 409 重发时是否带确认令牌：**只有用户确认过才带**。
 *
 * 无条件带 confirm 等于替用户把"实验臂被降级"的确认一路点头掉——服务端 409 门
 * 存在的唯一目的就是让这一次降级必须有人看过（spec §4.3 第 3 步）。
 */
export function extWriteConfirm(confirmed: boolean): { confirm: string } | undefined {
  return confirmed ? { confirm: TRANSPORT_DOWNGRADE_CONFIRM } : undefined
}

/**
 * 保存要打的目标模板 id：正在编辑的模板优先，其次是「模板行已落库、但链式写
 * ext 失败」的 pending id。返回 null 表示需要新建（只有真正的新建才允许 create）。
 *
 * 为什么需要它：模板与 ext 是两次调用，ext 写失败时模板行已经存在；若重试仍按
 * 「editing 为空 ⇒ 新建」判断，就会留下一个孤儿模板并再建一个。
 */
export function saveTargetID(editingId: number | null, pendingId: number | null): number | null {
  return editingId ?? pendingId ?? null
}

/** 空白键不上送（后端 nil = 未配置 = 该格式回落官方默认端点）。 */
export function nonEmptyPaths(rec: Record<string, string>): Record<string, string> | undefined {
  const out: Record<string, string> = {}
  for (const [k, v] of Object.entries(rec)) {
    const t = v.trim()
    if (t) out[k] = t
  }
  return Object.keys(out).length ? out : undefined
}
