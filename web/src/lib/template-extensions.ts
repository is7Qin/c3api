// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// custom 模板的逐格式上游路径：与后端约定共享的纯逻辑（键集合、留空不上送、
// 「必须先读回既有配置才能保存」的门禁）。抽成纯函数是为了能被 node --test
// 直接覆盖——页面级测试不在本项目的能力范围内，而门禁判断错了会静默清空配置。

/** custom 类型的逐格式路径键（与后端 service / openapi 键集合一致）。 */
export const CUSTOM_PATH_FIELDS: { key: string; placeholder: string }[] = [
  { key: 'openai-chat', placeholder: 'chat/completions' },
  { key: 'openai-responses', placeholder: 'responses' },
  { key: 'openai-responses-ws', placeholder: 'v1/responses' },
  { key: 'anthropic', placeholder: 'v1/messages' },
  { key: 'openai-search', placeholder: 'alpha/search' },
  { key: 'openai-images.generations', placeholder: 'images/generations' },
  { key: 'openai-images.edits', placeholder: 'images/edits' },
]

/** GET /templates/{id}/ext 的响应形态；null = 没有 ext 行。 */
export type TemplateExtEcho = {
  upstream_paths?: Record<string, string> | null
  strip_image_tools?: boolean | null
} | null

/** 表单里与 ext 对应的两个字段。 */
export type ExtFormFields = {
  strip_image_tools: boolean
  upstream_paths: Record<string, string>
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
  if (ext === null) return { strip_image_tools: false, upstream_paths: {} }
  return {
    strip_image_tools: ext.strip_image_tools === true,
    // 未配置的键不出现：输入框留空 = 该格式走默认端点
    upstream_paths: { ...(ext.upstream_paths ?? {}) },
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
 * 保存前是否必须拦住：custom 模板且表单还没同步过该模板的既有路径。
 *
 * 判据是「syncedFor === editingId」而不是查询状态：关闭再打开同一模板时缓存
 * 数据引用不变、effect 不重跑，而表单已被重置成空表——只看 isLoading 会把
 * 「被重置的空表单」当成已回显，一次保存就把全部路径写成 NULL。
 */
export function pathsEchoBlocked(
  credentialType: string,
  editingId: number | null,
  syncedFor: number | null,
): boolean {
  if (editingId === null) return false // 新建：没有既有配置可清空
  if (credentialType !== 'custom') return false // 其余类型不写路径
  return syncedFor !== editingId
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
