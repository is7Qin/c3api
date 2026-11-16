// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 控制台内嵌外部页面（spec 2026-11-16 内嵌页面 §7-1）：前端唯一一份规则层。
//
// 这里是**纯函数**：没有 React、没有运行期状态、没有模块级可变值——设置页编辑器与
// 内嵌页路由都只是消费者。原因有两条：
//   1. 本仓没有组件测试设施（只跑 `node --test`），判定点必须能被直接断言；写进组件
//      里就只能靠肉眼 review。
//   2. 解析 / 归一 / 校验 / 序列化与 `internal/domain/custompage.go` 是**同一套规则**
//      的两份实现（客户端就地报错 = UX，服务端校验 = 安全边界，§9-U7）。规则数值由
//      本文件常量区单一承载，Go 侧 `custompage_frontend_gate_test.go` 读本文件逐位
//      核对（§11-T4），改这里必须同步 Go。
import { BarChart3, BookOpen, Code, Globe, LifeBuoy, Link, Sparkles, Ticket, type LucideIcon } from 'lucide-react'

// —— 常量（与 Go 的 domain.CustomPage* 逐位等号；扁平标量字面量，便于跨语言门禁匹配）——
// 顺序即编辑器下拉顺序，不是字母序（Go 的 CustomPageIcons 同样如此）。
export const CUSTOM_PAGE_ICONS = ['link', 'globe', 'book', 'chart', 'lifebuoy', 'ticket', 'code', 'sparkles'] as const
export const CUSTOM_PAGE_VISIBILITIES = ['admin', 'user'] as const
export const CUSTOM_PAGE_MAX_ITEMS = 20
export const CUSTOM_PAGE_MAX_LABEL = 40
export const CUSTOM_PAGE_MAX_URL = 2048
export const CUSTOM_PAGE_MAX_ID = 32
// V9：序列化后的总字节上限（= Go 的 CustomPageMaxTotalBytes）。字段级上限本该够不到它，
// 但 JSON 转义会放大（`\` 翻倍），量的是真正要提交的那串字节。
export const CUSTOM_PAGE_MAX_TOTAL = 65536

// id 正则与 scheme 白名单**导出**成字面量常量：Go 的
// internal/domain/custompage_frontend_gate_test.go（§11-T4/§19-5）读本文件，把它们与
// domain.CustomPageIDPattern / domain.CustomPageURLSchemes 做等号比对。写成内联字面量
// 就没法比对，而这两条一旦两端分叉就是「前端放行、后端 400」或「功能凭空变窄」。
export const CUSTOM_PAGE_ID_PATTERN = /^[a-z0-9][a-z0-9-]*$/
export const CUSTOM_PAGE_URL_SCHEMES = ['http://', 'https://'] as const

// 条目列表的 query key（spec §7.2）。**单点定义**：pages/custom-page.tsx（取数）、
// components/app-shell.tsx（侧栏注入 + 面包屑）、components/custom-pages-editor.tsx
// （保存后定向失效）全部 import 这里。此前两份字面量各自定义，改一处漏一处会让
// "保存后侧栏不刷新"且测试全绿；反向依赖（components/ → pages/）也一并消掉。
export const CUSTOM_PAGES_QUERY_KEY = ['user', 'pages'] as const

export type CustomPageIconKey = (typeof CUSTOM_PAGE_ICONS)[number]
export type CustomPageVisibilityKey = (typeof CUSTOM_PAGE_VISIBILITIES)[number]

// 图标映射：key → Lucide 组件。`Record<CustomPageIconKey, …>` 在类型层就钉住键集，
// 少写/多写一个键都编译失败；运行时键集再由 `custom-pages.test.ts` 断言一次。
export const CUSTOM_PAGE_ICON_COMPONENTS: Record<CustomPageIconKey, LucideIcon> = {
  link: Link,
  globe: Globe,
  book: BookOpen,
  chart: BarChart3,
  lifebuoy: LifeBuoy,
  ticket: Ticket,
  code: Code,
  sparkles: Sparkles,
}

// 未知图标的回退（后端新增图标而旧前端未升级时的前向兼容路径，§8-S8）。
const FALLBACK_ICON: LucideIcon = Link

/** 配置里的一个条目（`settings.custom_pages` 的 JSON 数组元素，键小写、顺序固定）。 */
export interface CustomPageConfig {
  id: string
  label: string
  url: string
  /**
   * 刻意是 `string` 而不是联合类型：库内值可能是被手改出来的未知枚举
   * （`all`/空串）。交给校验报错，而不是让类型系统假装它不存在
   * （§8-S8：未知值在编辑器里报错，不静默改写）。
   */
  visibility: string
  /** 同上：未知图标要能流到 `iconForCustomPage` 的回退分支。 */
  icon: string
  enabled: boolean
}

// 校验码值清单。文案在 i18n（`settings.customPages.issue.<code>`），本层只给 code，
// 这样同一份判定既能被测试枚举，也能按语言渲染。
export type CustomPageIssueCode =
  | 'tooMany'
  | 'tooLarge'
  | 'idRequired'
  | 'idTooLong'
  | 'idPattern'
  | 'duplicateId'
  | 'labelRequired'
  | 'labelTooLong'
  | 'urlRequired'
  | 'urlTooLong'
  | 'urlScheme'
  | 'urlCredentials'
  | 'urlControlChar'
  | 'visibilityInvalid'
  | 'iconInvalid'

export type CustomPageIssueField = 'items' | 'id' | 'label' | 'url' | 'visibility' | 'icon'

export interface CustomPageIssue {
  /** 条目下标；列表级问题（条目数/总字节超限）没有具体条目，固定 -1。 */
  index: number
  field: CustomPageIssueField
  code: CustomPageIssueCode
}

/**
 * 解析层错误码（与 issue 分开：结构都坏了就只能清空重来，逐行报错没有意义）。
 * - invalidJson / notArray：JSON 语法错 / 顶层不是数组。
 * - invalidElement：数组里混进了非对象元素（`null`/字符串/数字/数组）。此前是静默丢弃，
 *   保存后"顺手删掉一条配置"——现在与 Go 同步：Go 的 encoding/json 直接报错。
 * - enabledInvalid：`enabled` 不是 JSON 布尔（如 `"true"`/`1`）。Go 由 encoding/json
 *   直接拒（§4.2），前端若当 true 处理就是比 Go 宽。
 */
export type ParseErrorCode = 'invalidJson' | 'notArray' | 'invalidElement' | 'enabledInvalid'

export interface ParseCustomPagesResult {
  pages: CustomPageConfig[]
  /** null = 解析成功（含"未配置"的空形态）。 */
  error: ParseErrorCode | null
}

const isRecord = (v: unknown): v is Record<string, unknown> => typeof v === 'object' && v !== null && !Array.isArray(v)

const asString = (v: unknown): string => (typeof v === 'string' ? v : '')

/** 字节长度：URL/ID 上限按**字节**判，与 Go 的 `len(string)` 同口径（非 UTF-16 码元）。 */
const byteLength = (s: string): number => new TextEncoder().encode(s).length

/**
 * 解析设置原值（宽松解析 + 严格结构）：
 * - `""` / `"[]"` / `"null"` → 空列表且不报错（D11：null 只可能来自手改 DB）。
 * - 语法错 → `invalidJson`；能解析但不是数组 → `notArray`。
 * - 数组里混进非对象元素 → `invalidElement`（**不再静默丢弃**：丢弃等于保存时顺手删掉
 *   一条配置，而 Go 的 encoding/json 对这种输入直接报错）。
 * - `enabled` 不是 JSON 布尔 → `enabledInvalid`（Go 同样在解析阶段就拒，§4.2）。
 *   缺失或 `null` 都按缺省 true 处理——Go 的 `*bool` 把 JSON null 当"未写"，这里同理。
 *
 * 字段值本身（id/label/url/visibility/icon）不做类型校验：非字符串按空串处理，交给
 * validateCustomPages 统一报错——错误定位只有一处，两端对"先报哪个问题"才不会分叉。
 */
export function tryParseCustomPages(raw: string): ParseCustomPagesResult {
  const trimmed = (raw ?? '').trim()
  if (trimmed === '' || trimmed === 'null') return { pages: [], error: null }
  let parsed: unknown
  try {
    parsed = JSON.parse(trimmed)
  } catch {
    return { pages: [], error: 'invalidJson' }
  }
  if (!Array.isArray(parsed)) return { pages: [], error: 'notArray' }
  // 元素级结构问题先整体判掉再映射：半解析出来的列表会让编辑器显示"少了一条"的假象。
  if (!parsed.every(isRecord)) return { pages: [], error: 'invalidElement' }
  if (parsed.some(item => item.enabled !== undefined && item.enabled !== null && typeof item.enabled !== 'boolean')) {
    return { pages: [], error: 'enabledInvalid' }
  }
  const pages = parsed.map(item => ({
    id: asString(item.id),
    label: asString(item.label),
    url: asString(item.url),
    visibility: asString(item.visibility),
    icon: asString(item.icon),
    // 缺省 = true：少写一个字段不能让页面凭空消失（D11）；只有显式 false 才是停用。
    // 非布尔值走上面那条 enabledInvalid——**没有**"字符串 true 也当 true"的宽容。
    enabled: item.enabled !== false,
  }))
  return { pages, error: null }
}

/**
 * 归一化：trim **label**、补缺省 `visibility=user` / `icon=link`。幂等，且**不改入参**。
 *
 * url 与 id 刻意不 trim（与 Go 的 NormalizeCustomPages 一致）：带空白的 URL/ID 是配置
 * 错误，而静默裁剪会把「含控制字符」这种危险值洗白（`https://h/\nx` trim 后看着正常）；
 * 交给校验报 `urlControlChar`/`idPattern` 更好。非法枚举（`all`）也原样留给校验拒绝——
 * 「写错枚举」不能被"纠正"成 user。
 */
export function normalizeCustomPages(pages: readonly CustomPageConfig[]): CustomPageConfig[] {
  return pages.map(p => ({
    id: p.id,
    label: p.label.trim(),
    url: p.url,
    visibility: p.visibility === '' ? 'user' : p.visibility,
    icon: p.icon === '' ? 'link' : p.icon,
    enabled: p.enabled,
  }))
}

/**
 * 校验：返回**第一个**问题（null = 通过）。顺序逐条对齐 Go 的 ValidateCustomPages
 * （V2 → 每条的 id/label/url/visibility/icon → V4 唯一性），这样两端对「同一条非法配置
 * 先报哪个问题」也一致，排障时不必猜是哪一端的顺序不同。
 */
export function validateCustomPages(pages: readonly CustomPageConfig[]): CustomPageIssue | null {
  // V2：条目数上限（列表级，没有具体条目）。
  if (pages.length > CUSTOM_PAGE_MAX_ITEMS) return { index: -1, field: 'items', code: 'tooMany' }
  const seen = new Set<string>()
  for (let i = 0; i < pages.length; i++) {
    const page = pages[i]
    // V3：id 非空 → 字节上限 → 形状。空串单独一类，管理面才能提示"请填写 id"。
    if (page.id === '') return { index: i, field: 'id', code: 'idRequired' }
    if (byteLength(page.id) > CUSTOM_PAGE_MAX_ID) return { index: i, field: 'id', code: 'idTooLong' }
    if (!CUSTOM_PAGE_ID_PATTERN.test(page.id)) return { index: i, field: 'id', code: 'idPattern' }
    // V5：label **trim 后**非空且 ≤40 rune。rune 数 = 码点数（`[...s].length`），
    // 与 Go 的 utf8.RuneCountInString 同口径（不是 UTF-16 length）。
    const label = page.label.trim()
    if (label === '') return { index: i, field: 'label', code: 'labelRequired' }
    if ([...label].length > CUSTOM_PAGE_MAX_LABEL) return { index: i, field: 'label', code: 'labelTooLong' }
    // V6：url 必填 + 控制字符 + 长度 + scheme/host/userinfo。
    if (page.url === '') return { index: i, field: 'url', code: 'urlRequired' }
    const urlCode = validateCustomPageUrl(page.url)
    if (urlCode) return { index: i, field: 'url', code: urlCode }
    // V7 / V8：枚举值域。空串不在这里补缺省（缺省是 normalize 的职责），直接拒。
    if (!(CUSTOM_PAGE_VISIBILITIES as readonly string[]).includes(page.visibility)) {
      return { index: i, field: 'visibility', code: 'visibilityInvalid' }
    }
    if (!(CUSTOM_PAGE_ICONS as readonly string[]).includes(page.icon)) {
      return { index: i, field: 'icon', code: 'iconInvalid' }
    }
    // V4：全表唯一。报**后**出现的那条（管理员改它即可），与 Go 的下标口径一致。
    if (seen.has(page.id)) return { index: i, field: 'id', code: 'duplicateId' }
    seen.add(page.id)
  }
  // V9：序列化后总字节上限（列表级，与 V2 同形）。放在逐条规则之后，与 Go 的
  // ValidateCustomPages 顺序一致（先条目问题，再总量）。量的是**提交给后端的那串**，
  // 所以直接复用 serializeCustomPages——不另写一份估算。
  if (byteLength(serializeCustomPages(pages)) > CUSTOM_PAGE_MAX_TOTAL) {
    return { index: -1, field: 'items', code: 'tooLarge' }
  }
  return null
}

/**
 * URL 判定（与 Go 的 validateCustomPageURL 同判据、同顺序）：
 * 控制字符 → 字节上限 → 字面 http(s):// → 非法百分号转义 → 可解析 + 有 host →
 * authority 含 `@`（userinfo）→ WHATWG 解析出的 username/password。
 *
 * 顺序有意义：`javascript:`/`data:`/`file:` 与相对路径都归到 `urlScheme`，但如果先做
 * `new URL()` 再判协议，像 `http:/x` 这种会被浏览器"补全"成合法地址，与 Go 的字面前缀
 * 判定分叉——所以先钉字面前缀。
 *
 * **与 Go 有意的差异（端口）**：`https://h:70000/` 在 Go 的 `url.Parse` 里是合法地址
 * （它只校验端口是数字），而 WHATWG `new URL()` 会直接抛错。这里保留 TS 更严的行为
 * （把浏览器根本连不上的地址挡在提交前）；不是漏同步，别为"对齐"把这条放宽。
 */
function validateCustomPageUrl(raw: string): CustomPageIssueCode | null {
  // 控制字符（C0 / DEL / C1）会打断 iframe src 与请求行；与 Go 的 unicode.IsControl 同集合。
  // 不 trim、不清洗：裁剪会把危险值洗白（见 normalizeCustomPages 的注释）。
  for (const ch of raw) {
    const cp = ch.codePointAt(0) ?? 0
    if (cp < 0x20 || (cp >= 0x7f && cp <= 0x9f)) return 'urlControlChar'
  }
  if (byteLength(raw) > CUSTOM_PAGE_MAX_URL) return 'urlTooLong'
  // scheme 大小写不敏感（HTTP:// 合法），但必须字面 http(s)://（白名单常量与 Go 逐位等号）。
  const lowered = raw.toLowerCase()
  if (!CUSTOM_PAGE_URL_SCHEMES.some(scheme => lowered.startsWith(scheme))) return 'urlScheme'
  // 非法百分号转义：Go 的 url.Parse 报错（`invalid URL escape "%zz"`），而 WHATWG
  // `new URL()` 会原样保留路径里的 `%zz` 不抛错——两边会分叉，故显式判一次，归 urlScheme。
  if (/%(?![0-9A-Fa-f]{2})/.test(raw)) return 'urlScheme'
  let parsed: URL
  try {
    parsed = new URL(raw)
  } catch {
    return 'urlScheme'
  }
  if (parsed.host === '') return 'urlScheme'
  // userinfo：先按 authority 段是否含 `@` 判（覆盖 `https://@host/` 这种**空** userinfo——
  // Go 的 url.User 非 nil 会拒，而 WHATWG 的 username/password 都是空串、下面那条判不出来）。
  // authority = scheme:// 之后到第一个 `/?#` 之前的那一段。
  const authority = raw.slice(lowered.indexOf('://') + 3).split(/[/?#]/, 1)[0]
  if (authority.includes('@')) return 'urlCredentials'
  // 凭据不能进配置：条目对目标角色可见，`user:pass@` 会随条目泄露；iframe 里的凭据
  // 还会绕开账号体系（§8-S2 同理），直接拒。
  if (parsed.username !== '' || parsed.password !== '') return 'urlCredentials'
  return null
}

/**
 * 序列化为提交串：键序固定 `id,label,url,visibility,icon,enabled`，空 → `"[]"`。
 *
 * 与 Go 的 MarshalCustomPages **同键序、同样不转义 HTML**（Go 侧 `SetEscapeHTML(false)`，
 * JSON.stringify 默认保留 `& < >`），所以常规配置往返后字节一致。但别说成"逐字节同形"：
 * 两边的转义表并不完全一致（Go 会把 U+2028/U+2029 转成 `\u2028`/`\u2029`，也会把非法
 * UTF-8 替换成 U+FFFD；`JSON.stringify` 保留前者、把孤立代理项写成 `\udXXX`）。**权威
 * 形态是服务端归一化后的那串**（UpdateSetting 回全量设置），本函数只负责提交；逐字节
 * 相等只对"归一化后的合法配置"成立，别拿它当跨语言的幂等证据。
 *
 * 幂等是契约的一部分：同一份配置重复保存必须得到同一条字符串，否则每次保存都产生一条
 * "看起来变了"的 diff（§11-T5 的前提）。
 */
export function serializeCustomPages(pages: readonly CustomPageConfig[]): string {
  return JSON.stringify(
    pages.map(p => ({
      id: p.id,
      label: p.label,
      url: p.url,
      visibility: p.visibility,
      icon: p.icon,
      enabled: p.enabled,
    })),
  )
}

/** `GET /api/user/custom-pages` 下发条目的结构性最小面（服务端不下发 `Enabled`，§4.4 D10）。 */
export interface ConsolePageLike {
  Visibility: string
  Enabled?: boolean
}

/**
 * 取某个控制台要显示的条目：`enabled` 是硬前提（服务端已过滤，这里再兜一次手改 DB /
 * 旧缓存），再按 `visibility` 过滤。
 *
 * **不排序**（顺序即配置顺序 = 侧边栏顺序）、**不改入参**（返回新数组；缓存里的数组
 * 被就地 sort/reverse 会让下一次渲染顺序莫名变化）。
 */
export function pagesForConsole<T extends ConsolePageLike>(pages: readonly T[], scope: CustomPageVisibilityKey): T[] {
  return pages.filter(p => p.Enabled !== false && p.Visibility === scope)
}

/**
 * 图标映射：未知 key 回退 `link`（不抛错、不留空白）。
 *
 * 前向兼容：后端新增图标而旧前端未升级时，条目仍然能渲染（§8-S8）——这比让整块侧栏
 * 崩掉或渲染成空盒子都划算。
 */
export function iconForCustomPage(icon: string): LucideIcon {
  return (CUSTOM_PAGE_ICON_COMPONENTS as Record<string, LucideIcon | undefined>)[icon] ?? FALLBACK_ICON
}

// —— iframe 地址拼装 ——

/**
 * 保留参数：这 7 个名字归我们所有。有值则覆盖（`set` 同时清掉同名重复项，结果里只出现
 * 一次），无值则**删除**——把上一轮的 token 留在 URL 上等于把陈旧凭据交给第三方（§8-S1）。
 */
export const EMBEDDED_RESERVED_PARAMS = ['user_id', 'token', 'theme', 'lang', 'ui_mode', 'src_host', 'src_url'] as const

/** 内嵌模式标记：目标站点据此收起自己的导航壳。 */
export const EMBEDDED_UI_MODE = 'embedded'

export interface BuildEmbeddedUrlParams {
  userId?: number | string | null
  token?: string | null
  theme: 'light' | 'dark'
  lang: string
  srcHost?: string
  srcUrl?: string
}

/**
 * 拼装 iframe 地址：只覆盖上面 7 个保留参数，**第三方 query 原样保留**（目标站点自己的
 * `?tab=x` 之类不能被我们抹掉）；fragment 保留（浏览器自己会把 query 放在它前面）。
 *
 * 非法 / 非 http(s) 的 baseUrl **原样返回**：拼装层不是校验层，静默改写用户配置比渲染
 * 一句"地址非法"更糟（§8-S3 的渲染侧兜底由调用方决定怎么显示）。
 */
export function buildEmbeddedUrl(baseUrl: string, params: BuildEmbeddedUrlParams): string {
  if (!baseUrl) return baseUrl
  let url: URL
  try {
    url = new URL(baseUrl)
  } catch {
    return baseUrl
  }
  if (url.protocol !== 'http:' && url.protocol !== 'https:') return baseUrl

  const put = (key: (typeof EMBEDDED_RESERVED_PARAMS)[number], value: string | null | undefined) => {
    if (value === null || value === undefined || value === '') {
      url.searchParams.delete(key)
      return
    }
    url.searchParams.set(key, value)
  }

  put('user_id', params.userId === undefined || params.userId === null || params.userId === '' ? null : String(params.userId))
  put('token', params.token)
  put('theme', params.theme)
  put('lang', params.lang)
  // 固定值：内嵌模式是"这是本控制台的 iframe"的唯一信号，不接受调用方覆盖。
  put('ui_mode', EMBEDDED_UI_MODE)
  put('src_host', params.srcHost)
  put('src_url', params.srcUrl)
  return url.toString()
}

/**
 * 当前主题：与 ThemeProvider 一致（`dark` 类挂在 `<html>` 上）。
 *
 * `document` 缺失时返回 `light`：本文件会被 `node --test` 直接 import（无 DOM），
 * 让一个纯函数在测试环境抛 ReferenceError 只会逼出"测试里给 document 打桩"的噪音。
 */
export function resolveTheme(): 'light' | 'dark' {
  if (typeof document === 'undefined') return 'light'
  return document.documentElement.classList.contains('dark') ? 'dark' : 'light'
}
