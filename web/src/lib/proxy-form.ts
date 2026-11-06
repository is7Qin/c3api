// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 代理条目表单的纯逻辑：廉价格式校验 + 请求体三态投影。
//
// 纪律：
// - **校验只为体验**。真判据在后端，且是**两步**：`internal/service/proxy.go` 的
//   `normalizeProxyInput`（name trim 后 1..64 / scheme 小写白名单 / host 形态 /
//   port 1..65535 / 凭据控制字符）与紧随其后的 `checkSupplierEgressHost`
//   （**仅供应商面**的私网/回环/链路本地拒绝；管理员不受此限）。本文件只做**格式**
//   反馈，两步都不替代——前端放过去的东西后端仍会 400。
// - **本文件不做 IP 分类**（C3 已删 `isPrivateEgressHost`）：前端曾经自己解析
//   IPv4/IPv6/mapped 并维护私网段名单，与后端口径漂移（前端去尾点、认 `.localhost`，
//   后端都不认）。私网/回环/链路本地只有一份判据，在后端；前端只留一句固定说明
//   与后端 400 的 message 透出。
// - **未触碰的字段绝不进请求体**。`ProxyConfig` 的三态是「缺席 = 不变 / null = 清空 /
//   有值 = 落值」，故 `buildProxyPayload` 是这条投影的唯一实现，调用点不得自行拼装。
// - 本模块不 import 任何运行时依赖（只有 type import），`node --test` 可直接执行。
import type { components } from '@/lib/api/schema'

export type ProxyScheme = NonNullable<components['schemas']['ProxyConfig']['scheme']>
export type ProxyConfig = components['schemas']['ProxyConfig']
export type ProxyView = components['schemas']['Proxy']
// 批量导入的读模型（spec-proxy-import）：页面只读这两个，不自己拼行结果。
export type ProxyImportResponse = components['schemas']['ProxyImportResponse']
export type ProxyImportRowResult = components['schemas']['ProxyImportRowResult']

// scheme 白名单：小写四值，原样落库（后端不把 socks5h 归一成 socks5）。
export const PROXY_SCHEMES = ['socks5', 'socks5h', 'http', 'https'] as const satisfies readonly ProxyScheme[]
export const PROXY_NAME_MAX = 64
export const PROXY_HOST_MAX = 253
export const PROXY_PORT_MIN = 1
export const PROXY_PORT_MAX = 65535

// 表单草稿：全部字段用字符串承载（Input 的原生形态），提交时由 buildProxyPayload 投影。
// 密码恒不回显（后端任何响应都不含密码），故初始恒为 ''。
export interface ProxyDraft {
  name: string
  scheme: string
  host: string
  port: string
  username: string
  password: string
  enabled: boolean
  remark: string
}

// 三态意图：两个「清除」哨兵是**显式**动作，与「输入框为空」严格区分
// （空输入 = 不变；勾选清除 = 发 null；这是 accounts.tsx 的既有先例）。
export interface ProxyFormIntent {
  clearPassword: boolean
  clearUsername: boolean
}

export const emptyProxyDraft = (): ProxyDraft => ({
  name: '',
  scheme: 'http',
  host: '',
  port: '',
  username: '',
  password: '',
  enabled: true,
  remark: '',
})

// 编辑回显：只回显读模型真有的字段（无密码；无 last_probe_*，那些列不由 PUT 写）。
export function proxyDraftOf(p: ProxyView): ProxyDraft {
  return {
    name: p.name,
    scheme: p.scheme,
    host: p.host,
    port: String(p.port),
    username: p.username ?? '',
    password: '',
    enabled: p.enabled,
    remark: p.remark ?? '',
  }
}

// IPv6 方括号归一：`[::1]` → `::1`（读模型与写面都以无括号形态为准）。
// 只在「整体被一对括号包住」时剥一层，避免破坏畸形输入的可读性。
export function normalizeProxyHost(raw: string): string {
  const s = raw.trim()
  if (s.length >= 2 && s.startsWith('[') && s.endsWith(']')) return s.slice(1, -1).trim()
  return s
}

// 展示形态：IPv6 必须加回方括号（否则 `scheme://::1:8080` 无法阅读）。
export function proxyEndpoint(p: { scheme: string; host: string; port: number }): string {
  const host = p.host.includes(':') ? `[${p.host}]` : p.host
  return `${p.scheme}://${host}:${p.port}`
}

// Select 选项标签（accounts 页「出口代理」与管理面列表共用同一拼法）。
export function proxyOptionLabel(p: { name: string; scheme: string; host: string; port: number }): string {
  return `${p.name} (${proxyEndpoint(p)})`
}

// —— 校验：字段级错误码（页面负责翻译；测试断言错误码本身）——
export type ProxyFieldError =
  | 'required' // 空
  | 'tooLong' // 超长
  | 'scheme' // 不在白名单
  | 'hostChars' // 含 / @ 空白 或控制字符
  | 'portInteger' // 不是整数
  | 'portRange' // 越界 1..65535
export type ProxyField = 'name' | 'scheme' | 'host' | 'port'
export type ProxyFormErrors = Partial<Record<ProxyField, ProxyFieldError>>

// 控制字符与空白：按码位判定（不在正则里写控制字符字面量，否则 lint 报 no-control-regex）。
// 覆盖 C0 控制字符、空格与 DEL，再加 Unicode 空白（\s）。
const WHITESPACE = /\s/
function hasControlOrSpace(s: string): boolean {
  for (const ch of s) {
    const code = ch.codePointAt(0) ?? 0
    if (code <= 0x20 || code === 0x7f || WHITESPACE.test(ch)) return true
  }
  return false
}

// 与后端同序：name → scheme → host → port（先报先得，页面按字段展示）。
export function validateProxyForm(draft: ProxyDraft): ProxyFormErrors {
  const errs: ProxyFormErrors = {}

  const name = draft.name.trim()
  if (name === '') errs.name = 'required'
  else if (name.length > PROXY_NAME_MAX) errs.name = 'tooLong'

  const scheme = draft.scheme.trim().toLowerCase()
  if (!(PROXY_SCHEMES as readonly string[]).includes(scheme)) errs.scheme = 'scheme'

  const host = normalizeProxyHost(draft.host)
  if (host === '') errs.host = 'required'
  else if (host.length > PROXY_HOST_MAX) errs.host = 'tooLong'
  else if (host.includes('/') || host.includes('@') || hasControlOrSpace(host)) errs.host = 'hostChars'

  const portRaw = draft.port.trim()
  if (portRaw === '') errs.port = 'required'
  else if (!/^\d+$/.test(portRaw)) errs.port = 'portInteger'
  else {
    const n = Number(portRaw)
    if (n < PROXY_PORT_MIN || n > PROXY_PORT_MAX) errs.port = 'portRange'
  }

  return errs
}

export const proxyFormValid = (draft: ProxyDraft): boolean => Object.keys(validateProxyForm(draft)).length === 0

// —— 请求体投影（三态的唯一实现）——
// initial = null ⇒ 创建（只发非空值，其余取后端缺省）。
// initial 非 null ⇒ 更新：只发**变化**项，未触碰的键一律缺席。
export function buildProxyPayload(initial: ProxyView | null, draft: ProxyDraft, intent: ProxyFormIntent): ProxyConfig {
  const payload: ProxyConfig = {
    name: draft.name.trim(),
    scheme: draft.scheme.trim().toLowerCase() as ProxyScheme,
    host: normalizeProxyHost(draft.host),
    port: Number(draft.port.trim()),
  }

  // username：未改 ⇒ 缺席；显式清除 ⇒ null；有值且与回显不同 ⇒ 落值。
  // 空输入永不作为 "" 发送（契约：可空标量 "" = 400，清空只有 null 一个拼法）。
  const username = draft.username.trim()
  if (intent.clearUsername) payload.username = null
  else if (username !== '' && username !== (initial?.username ?? '')) payload.username = username

  // password：后端从不回显 ⇒ 「未改」的唯一判据是输入框为空且未勾清除。
  // 勾了清除即使有输入也以清除为准（UI 会同时禁用输入框，此处是兜底）。
  if (intent.clearPassword) payload.password = null
  else if (draft.password !== '') payload.password = draft.password

  // remark：读模型恒回显（可能为 ''）⇒ 未改 ⇒ 缺席；清空 ⇒ null；有值 ⇒ 落值。
  const remark = draft.remark.trim()
  if (initial) {
    if (draft.remark !== (initial.remark ?? '')) payload.remark = remark === '' ? null : remark
  } else if (remark !== '') {
    payload.remark = remark
  }

  // enabled：只有**显式切换**才出现。创建态缺省即 true（后端缺省），故只在 want false 时带。
  const enabledChanged = initial ? draft.enabled !== initial.enabled : draft.enabled !== true
  if (enabledChanged) payload.enabled = draft.enabled

  return payload
}

// —— 批量导入：粘贴文本 → 请求行（spec-proxy-import §5）——
//
// 语法：`[scheme://][user[:pass]@]host[:port]`
// - 忽略空行与以 `#` 开头的**整行**注释（不支持行尾注释：密码里可以有 '#'，猜边界就是猜错）。
// - 省略 scheme ⇒ 用调用方给的默认协议（页面上是一个显式下拉，不猜）。
// - authority 里**最后一个** '@' 是凭据分隔符（密码可含 '@'）；userinfo 里**第一个** ':'
//   分用户名与密码（密码可含 ':'）；`u:@host` ⇒ 无密码（后端允许"有用户名无密码"）。
// - host/port 从右侧切：`[::1]:1080` 靠方括号给 IPv6 定界；其余取**最后一个** ':'。
//   **裸 IPv6 必须写方括号**——`::1:1080` 无法判断哪个冒号是端口分隔符，报 badHost。
// - name 由端点派生（`scheme://host:port`，与列表展示同一拼法 proxyEndpoint），超 64 字符
//   报 nameTooLong 而**不静默截断**：截断会造出难以察觉的重名。
//
// 边界（C3 的教训）：本函数只判"能否构成请求"（结构），**不判地址值是否合法**——端口范围
// 越界必然 400，属结构约束；私网/回环拒绝与 host 字符集仍只有后端一份判据。
export const PROXY_IMPORT_MAX_ROWS = 100

export type ProxyImportParseError =
  | 'needPort' // 没有端口段
  | 'badPort' // 端口不是 1..65535 的十进制数
  | 'emptyHost' // host 为空
  | 'badHost' // 方括号不配对，或裸 IPv6（必须写方括号）
  | 'badScheme' // scheme 不在白名单
  | 'nameTooLong' // 派生 name 超过 64 字符
  | 'emptyUsername' // 写了 `@` 但用户名是空的

export interface ProxyImportLine {
  /** 源文本里的行号（1 基）：空行与注释行不占位，但行号仍按原文计。 */
  line: number
  raw: string
  config?: ProxyConfig
  error?: ProxyImportParseError
}

const SCHEME_PREFIX = /^([A-Za-z][A-Za-z0-9+.-]*):\/\//

/** 把粘贴文本解析成逐行结果：成功的行带 config（可直接进请求体），失败的带错误码。 */
export function parseProxyImportLines(text: string, defaultScheme: ProxyScheme): ProxyImportLine[] {
  const out: ProxyImportLine[] = []
  const lines = text.split('\n')
  for (let i = 0; i < lines.length; i++) {
    const raw = lines[i].trim()
    if (raw === '' || raw.startsWith('#')) continue
    out.push({ line: i + 1, raw, ...parseProxyImportLine(raw, defaultScheme) })
  }
  return out
}

function parseProxyImportLine(
  raw: string,
  defaultScheme: ProxyScheme,
): { config?: ProxyConfig; error?: ProxyImportParseError } {
  let scheme: ProxyScheme = defaultScheme
  let rest = raw
  const m = SCHEME_PREFIX.exec(raw)
  if (m) {
    const candidate = m[1].toLowerCase()
    if (!(PROXY_SCHEMES as readonly string[]).includes(candidate)) return { error: 'badScheme' }
    scheme = candidate as ProxyScheme
    rest = raw.slice(m[0].length)
  }

  // 凭据：host 里不可能出现 '@'（后端拒绝），故最后一个 '@' 一定是凭据分隔符。
  let username = ''
  let password: string | undefined
  let hostport = rest
  const at = rest.lastIndexOf('@')
  if (at >= 0) {
    const userinfo = rest.slice(0, at)
    hostport = rest.slice(at + 1)
    const colon = userinfo.indexOf(':')
    username = colon < 0 ? userinfo : userinfo.slice(0, colon)
    if (colon >= 0) password = userinfo.slice(colon + 1)
    if (username === '') return { error: 'emptyUsername' }
  }

  let host = ''
  let portRaw = ''
  if (hostport.startsWith('[')) {
    const close = hostport.indexOf(']')
    if (close < 0) return { error: 'badHost' }
    host = hostport.slice(1, close)
    const tail = hostport.slice(close + 1)
    if (!tail.startsWith(':')) return { error: 'needPort' }
    portRaw = tail.slice(1)
  } else {
    const colon = hostport.lastIndexOf(':')
    if (colon < 0) return { error: 'needPort' }
    host = hostport.slice(0, colon)
    portRaw = hostport.slice(colon + 1)
    if (host.includes(':')) return { error: 'badHost' } // 裸 IPv6：端口边界不可判
  }
  if (host === '') return { error: 'emptyHost' }
  if (!/^\d+$/.test(portRaw)) return { error: 'badPort' }
  const port = Number(portRaw)
  if (port < PROXY_PORT_MIN || port > PROXY_PORT_MAX) return { error: 'badPort' }

  const name = proxyEndpoint({ scheme, host, port })
  if ([...name].length > PROXY_NAME_MAX) return { error: 'nameTooLong' }

  const config: ProxyConfig = { name, scheme, host, port }
  if (username !== '') config.username = username
  if (password !== undefined && password !== '') config.password = password
  return { config }
}
