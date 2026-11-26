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

/** codex 号池类型：ext 的 codex_* 列组归属（PAT/OAuth 回显等）。 */
export function isCodexCredentialType(credentialType: string): boolean {
  return credentialType === 'codex-oauth' || credentialType === 'codex-pat'
}

/**
 * 出站传输（`codex_responses_transport`）对哪些类型有意义：Responses 原生
 * provider —— codex oauth/pat 与 BPS。字段名是历史命名；语义是「Responses
 * 出站传输」，BPS 复用它（HTTP 客户端 + ws 上游仍以 SSE 回客户端）。
 */
export function supportsResponsesTransport(credentialType: string): boolean {
  return isCodexCredentialType(credentialType) || credentialType === 'bps'
}

/** 降级确认令牌（与 service 的字面量契约一致：`?confirm=transport-downgrade`）。 */
export const TRANSPORT_DOWNGRADE_CONFIRM = 'transport-downgrade'

/** 单个格式层的请求修饰（wire 形状；与 openapi TemplateFormatPatch 兼容）。 */
export type TemplateFormatPatch = {
  headers?: Record<string, string>
  /** gjson path → 任意 JSON 值。 */
  set?: Record<string, unknown>
  delete?: string[]
}

/** 出站请求修饰（request_patch，custom 专用；与 openapi TemplateRequestPatch 兼容）。
 *
 * 值可为密钥、可含 {api_key} 占位符；GET 原样回显（不替换/不掩码）。全列替换语义：
 * 缺省/null = 清空，所以回显链路漏一环就会把已有配置清掉。 */
export type TemplateRequestPatch = {
  headers?: Record<string, string>
  formats?: Record<string, TemplateFormatPatch>
}

/** GET /templates/{id}/ext 的响应形态；null = 没有 ext 行。 */
export type TemplateExtEcho = {
  upstream_paths?: Record<string, string> | null
  strip_image_tools?: boolean | null
  /** ""（默认=http） / "http" / "ws"；后端恒回显（含 ""）。 */
  codex_responses_transport?: string | null
  request_patch?: TemplateRequestPatch | null
} | null

// —— 请求修饰编辑器的**行**状态（表单用；set 值是 JSON 文本）——
//
// 行状态与 wire 形状分开：表单需要"空行可增删"的中间态，wire 只需要最终值。
// 三态继续成立：undefined = 没读到（不灌表单）；null/无 ext 行 = 空表；有值 = 行。
export type PatchHeaderRow = { name: string; value: string }
export type PatchSetRow = { path: string; value: string }
export type FormatPatchForm = { headers: PatchHeaderRow[]; set: PatchSetRow[]; delete: string[] }
export type RequestPatchForm = {
  headers: PatchHeaderRow[]
  /** 键 ∈ CUSTOM_PATH_FIELDS；缺键 = 该格式层未配置。 */
  formats: Record<string, FormatPatchForm>
}

/** 空请求修饰（新建/无 ext 行/清空后的表单形态）。 */
export function emptyRequestPatchForm(): RequestPatchForm {
  return { headers: [], formats: {} }
}

/**
 * wire 请求修饰 → 行编辑器状态（回显映射的唯一入口）。
 *
 * **深拷贝**：header/formats/set/delete 全部新建，不与 echo 缓存共享引用——否则
 * 表单编辑会写回 react-query 缓存，下一次回显 effect 看到的是"已被改过的旧值"。
 * 三态语义：undefined（没读到）由调用方拦住，不在本函数处理；null = 无 ext 行。
 */
export function requestPatchToForm(patch: TemplateRequestPatch | null | undefined): RequestPatchForm {
  const out = emptyRequestPatchForm()
  if (!patch) return out
  for (const [name, value] of Object.entries(patch.headers ?? {})) {
    out.headers.push({ name, value })
  }
  for (const [key, fp] of Object.entries(patch.formats ?? {})) {
    const layer: FormatPatchForm = { headers: [], set: [], delete: [] }
    for (const [name, value] of Object.entries(fp.headers ?? {})) layer.headers.push({ name, value })
    for (const [path, value] of Object.entries(fp.set ?? {})) {
      layer.set.push({ path, value: JSON.stringify(value) ?? 'null' })
    }
    layer.delete = [...(fp.delete ?? [])]
    out.formats[key] = layer
  }
  return out
}

/** wire 请求修饰是否为空（空 = 未配置 = 上层省略字段 ⇒ 清空）。 */
export function isEmptyRequestPatch(patch: TemplateRequestPatch | null | undefined): boolean {
  if (!patch) return true
  return Object.keys(patch.headers ?? {}).length === 0 && Object.keys(patch.formats ?? {}).length === 0
}

/** requestPatchFromForm 的结果：patch 非空才需要上送；invalid = 有 set 行的 JSON 不合法
 * （或值为空）。客户端只做 JSON 语法检查，语义/合法性仍由服务端权威判定。 */
export type RequestPatchFromFormResult = { patch?: TemplateRequestPatch; invalid: boolean }

/**
 * 行编辑器状态 → wire 请求修饰（写面的唯一转换点）。
 *
 * - 空头名/空 path 行忽略（表单留的空行不该变成配置）；
 * - header 值允许留空（= 删除该头，语义有效）；
 * - set 值必须是合法 JSON 文本：空/非法 ⇒ invalid=true 且该行不上送（调用方必须
 *   拦住保存并提示，而不是静默丢弃——静默丢弃＝运营以为写进去了）；
 * - 全空 ⇒ patch 为 undefined（上层省略字段 = 清空，与 upstream_paths 同语义）。
 */
export function requestPatchFromForm(form: RequestPatchForm): RequestPatchFromFormResult {
  let invalid = false
  const headers: Record<string, string> = {}
  for (const row of form.headers) {
    const name = row.name.trim()
    if (!name) continue
    headers[name] = row.value
  }
  const formats: Record<string, TemplateFormatPatch> = {}
  for (const [key, layer] of Object.entries(form.formats)) {
    const out: TemplateFormatPatch = {}
    const layerHeaders: Record<string, string> = {}
    for (const row of layer.headers) {
      const name = row.name.trim()
      if (!name) continue
      layerHeaders[name] = row.value
    }
    if (Object.keys(layerHeaders).length) out.headers = layerHeaders
    const set: Record<string, unknown> = {}
    for (const row of layer.set) {
      const path = row.path.trim()
      if (!path) continue
      const raw = row.value.trim()
      if (!raw) {
        invalid = true
        continue
      }
      try {
        set[path] = JSON.parse(raw)
      } catch {
        invalid = true
      }
    }
    if (Object.keys(set).length) out.set = set
    const del = layer.delete.map(s => s.trim()).filter(Boolean)
    if (del.length) out.delete = del
    if (out.headers || out.set || out.delete) formats[key] = out
  }
  const patch: TemplateRequestPatch = {}
  if (Object.keys(headers).length) patch.headers = headers
  if (Object.keys(formats).length) patch.formats = formats
  return { patch: isEmptyRequestPatch(patch) ? undefined : patch, invalid }
}

/**
 * 找出对象/数组里所有**超出 JS 安全整数范围**的整数值路径（评审 momus I-1 护栏）。
 *
 * 为什么需要：GET /templates/{id}/ext 经 fetch 的 JSON.parse 读成 JS number 时，
 * 绝对值超过 2^53-1 的整数**已经被四舍五入**；表单再 JSON.stringify 保存就是一次
 * 静默改写（9007199254740993 → 9007199254740992）。服务端 admin API 用端点级
 * UseNumber 精确保存，所以唯一安全修法是把这类值交给 admin API，web 端发现即
 * **阻止保存**（见 templates.tsx 的两处写面门禁）。
 *
 * 判据：typeof number && Number.isInteger(n) && !Number.isSafeInteger(n)。字符串里
 * 的数字、小数都不报（JSON.parse 不会改写它们——只有 numeric 整数值会丢精度）。
 * 路径形如 `formats.openai-chat.set.big`、`formats.anthropic.set.nested.n`、
 * `arr[0].id`；prefix 为空时路径就是顶层键名（根值是数字时回落 `<root>`）。
 */
export function findUnsafeIntegerPaths(value: unknown, prefix?: string): string[] {
  const out: string[] = []
  const walk = (v: unknown, path: string): void => {
    if (typeof v === 'number') {
      if (Number.isInteger(v) && !Number.isSafeInteger(v)) out.push(path || '<root>')
      return
    }
    if (Array.isArray(v)) {
      v.forEach((item, i) => walk(item, path ? `${path}[${i}]` : `[${i}]`))
      return
    }
    if (v && typeof v === 'object') {
      for (const [key, child] of Object.entries(v as Record<string, unknown>)) {
        walk(child, path ? `${path}.${key}` : key)
      }
    }
  }
  walk(value, prefix ?? '')
  return out
}

/**
 * 行编辑器状态 → 不安全整数路径（保存前护栏；与 requestPatchFromForm 同一份
 * 「set 值是 JSON 文本」语义）。
 *
 * 以**表单当前内容**重算而不是沿用回显时刻的检测结果：用户把某行改成安全范围或
 * 删掉该行之后必须立刻放行。空 path/空值与非法的 set 行在这里跳过（前者不是配置，
 * 后者由 requestPatchFromForm 的 invalid 分支拦住并提示）。
 */
export function requestPatchUnsafeIntegerPathsFromForm(form: RequestPatchForm): string[] {
  const out: string[] = []
  for (const [key, layer] of Object.entries(form.formats)) {
    for (const row of layer.set) {
      const path = row.path.trim()
      const raw = row.value.trim()
      if (!path || !raw) continue
      let parsed: unknown
      try {
        parsed = JSON.parse(raw)
      } catch {
        continue
      }
      out.push(...findUnsafeIntegerPaths(parsed, `formats.${key}.set.${path}`))
    }
  }
  return out
}

/**
 * 保存前护栏的**唯一执行点**（评审 momus r2 Important：执行点此前没有源码 tripwire，
 * 只删掉某一处 await/throw 不会被任何用例发现）。
 *
 * 输入是「检测结果」而不是表单/wire 本身：两个写面的检测入口不同（主表单按行状态
 * 重算 requestPatchUnsafeIntegerPathsFromForm，扩展对话框按回显 wire 值
 * findUnsafeIntegerPaths），但"命中即中止一切写"这条**执行语义**必须只有一份。
 * 非空 ⇒ `throw`（不是 console.warn、不是禁用按钮：mutationFn 抛出即任何 PUT 都不会
 * 发出）；空 ⇒ 放行。
 *
 * 消息由调用方传入：本文件是 `node --test` 直跑的零依赖纯逻辑，i18n 在页面层。
 */
export function assertRequestPatchSafe(unsafePaths: string[], message: (joined: string) => string): void {
  if (unsafePaths.length === 0) return
  throw new Error(message(unsafePaths.join(', ')))
}

/** 写面 body 的字段来源：两处写面（主表单链式写 + 扩展对话框）共用。
 *
 * `strip_image_tools` 保留三态：对话框能表达"未配置"（null），主表单只有开/关。 */
export type ExtWriteFields = {
  strip_image_tools: boolean | null
  upstream_paths: Record<string, string>
  /** "" 是合法值（未配置 = 默认 http）——不做归一/折叠。 */
  codex_responses_transport: string
  /** wire 形状（custom 且非空才会进 body；其余类型省略 = 清空）。 */
  request_patch: TemplateRequestPatch
}

/** 表单里与 ext 对应的字段（表单没有"未配置"这一态；request_patch 是行编辑器状态）。 */
export type ExtFormFields = Omit<ExtWriteFields, 'strip_image_tools' | 'request_patch'> & {
  strip_image_tools: boolean
  request_patch: RequestPatchForm
}

/** ext 写面 body（与 `components['schemas']['TemplateExt']` 结构兼容）。
 *
 * 本文件不 import schema 类型：`node --test` 直接跑 TS（零依赖、零路径别名），
 * 调用点再用 `const body: TemplateExt = extWriteBody(...)` 把契约钉住。 */
export type TemplateExtWriteBody = {
  credential_type: 'responses-special' | 'codex-oauth' | 'codex-pat' | 'custom' | 'bps'
  strip_image_tools: boolean | null
  codex_responses_transport?: string
  upstream_paths?: Record<string, string>
  request_patch?: TemplateRequestPatch
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
  if (ext === null) {
    return {
      strip_image_tools: false,
      upstream_paths: {},
      codex_responses_transport: '',
      request_patch: emptyRequestPatchForm(),
    }
  }
  return {
    strip_image_tools: ext.strip_image_tools === true,
    // 未配置的键不出现：输入框留空 = 该格式走默认端点
    upstream_paths: { ...(ext.upstream_paths ?? {}) },
    // "" 本身有意义（默认面）：只把 null/undefined 兜成 ""，其余原样。
    codex_responses_transport: ext.codex_responses_transport ?? '',
    // request_patch 深拷贝（不共享 echo 缓存引用）；没有 ext 行 = 空表。
    request_patch: requestPatchToForm(ext.request_patch ?? null),
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
 * 覆盖 `custom`（upstream_paths）**与** Responses 原生 provider（codex/bps 的
 * codex_responses_transport）：
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
  if (credentialType !== 'custom' && !supportsResponsesTransport(credentialType)) return false // 其余类型不写 ext 字段
  return syncedFor !== editingId
}

/**
 * ext 写面 body 的**唯一**组装点（两处写面共用）：全列替换，三字段一起发。
 *
 * - `upstream_paths` 只有 custom 发：其余类型提交非空 map → 400；
 * - `request_patch` 同样只有 custom 发（非空时），其余类型省略 = 清空；
 * - `codex_responses_transport` 只有 Responses 原生 provider（codex/bps）发：其余
 *   类型下 `"http"`/`"ws"` 永远不生效（→ 400），而陈旧行残留值是否"恰好等于库里的
 *   原值"（幂等例外）在 web 侧无法判定，所以不猜——省略即清空；旧值确实是 `ws` 时
 *   由服务端 409 门接住，用户确认后再降级。
 */
export function extWriteBody(f: ExtWriteFields, credentialType: string): TemplateExtWriteBody {
  const body: TemplateExtWriteBody = {
    credential_type: credentialType as TemplateExtWriteBody['credential_type'],
    strip_image_tools: f.strip_image_tools,
  }
  if (credentialType === 'custom') {
    body.upstream_paths = nonEmptyPaths(f.upstream_paths)
    // request_patch 只有 custom 发：其余类型提交非空 ⇒ 400；省略即清空（全列替换）。
    if (!isEmptyRequestPatch(f.request_patch)) body.request_patch = f.request_patch
  }
  if (supportsResponsesTransport(credentialType)) body.codex_responses_transport = f.codex_responses_transport
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
