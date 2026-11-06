// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 代理条目表单的纯逻辑：校验 + 请求体三态投影 + 私网出口判据。
//
// 纪律：
// - **校验只为体验**。真判据在后端 `internal/service/proxy.go` 的 `normalizeProxyInput`
//   （name trim 后 1..64 / scheme 小写白名单 / host 形态 / port 1..65535）；本文件与之
//   同口径，但不替代它——前端放过去的东西后端仍会 400。
// - **未触碰的字段绝不进请求体**。`ProxyConfig` 的三态是「缺席 = 不变 / null = 清空 /
//   有值 = 落值」，故 `buildProxyPayload` 是这条投影的唯一实现，调用点不得自行拼装。
// - 本模块不 import 任何运行时依赖（只有 type import），`node --test` 可直接执行。
import type { components } from '@/lib/api/schema'

export type ProxyScheme = NonNullable<components['schemas']['ProxyConfig']['scheme']>
export type ProxyConfig = components['schemas']['ProxyConfig']
export type ProxyView = components['schemas']['Proxy']

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

// —— 私网/回环/链路本地/元数据地址判据 ——
// 与后端 §3.6 同一份名单（10/8、172.16/12、192.168/16、127/8、169.254/16、100.64/10、
// 0/8、::1、fc00::/7、fe80::/10、IPv4-mapped、localhost、.local）。
// **只用于供应商面提示**：供应商自建代理拒私网是后端 400 的事，前端不得据此自行决定。
export function isPrivateEgressHost(raw: string): boolean {
  let host = normalizeProxyHost(raw).toLowerCase()
  if (host === '') return false
  // 完全限定名的尾点不影响解析结果
  if (host.endsWith('.')) host = host.slice(0, -1)
  if (host === '') return false

  if (host === 'localhost' || host.endsWith('.localhost')) return true
  if (host.endsWith('.local')) return true

  if (host.includes(':')) return isPrivateIPv6(host)

  const v4 = parseIPv4(host)
  return v4 ? isPrivateIPv4(v4) : false
}

function parseIPv4(host: string): [number, number, number, number] | null {
  const parts = host.split('.')
  if (parts.length !== 4) return null
  const octets: number[] = []
  for (const part of parts) {
    if (!/^\d{1,3}$/.test(part)) return null
    const n = Number(part)
    if (n > 255) return null
    octets.push(n)
  }
  return [octets[0], octets[1], octets[2], octets[3]]
}

function isPrivateIPv4([a, b]: [number, number, number, number]): boolean {
  if (a === 0) return true // 0/8（含 0.0.0.0）
  if (a === 10) return true // 10/8
  if (a === 127) return true // 127/8（回环）
  if (a === 169 && b === 254) return true // 169.254/16（链路本地 / 云元数据）
  if (a === 172 && b >= 16 && b <= 31) return true // 172.16/12（172.32 起是公网）
  if (a === 192 && b === 168) return true // 192.168/16
  if (a === 100 && b >= 64 && b <= 127) return true // 100.64/10（CGNAT）
  return false
}

function isPrivateIPv6(host: string): boolean {
  // 未指定地址：等价于 IPv4 的 0/8
  if (host === '::' || host === '0:0:0:0:0:0:0:0') return true
  // 回环：::1（含展开形态）
  if (host === '::1' || host === '0:0:0:0:0:0:0:1') return true
  // IPv4-mapped：::ffff:a.b.c.d → 按内嵌 IPv4 判（点分形态；十六进制形态视为非私网，后端仍会拦）
  const mapped = host.match(/^(?:0:0:0:0:0:ffff:|::ffff:)(\d{1,3}(?:\.\d{1,3}){3})$/)
  if (mapped) {
    const v4 = parseIPv4(mapped[1])
    return v4 ? isPrivateIPv4(v4) : false
  }
  const first = host.split(':')[0]
  if (first === '') return false
  const hextet = Number.parseInt(first, 16)
  if (Number.isNaN(hextet)) return false
  if ((hextet & 0xfe00) === 0xfc00) return true // fc00::/7（唯一本地地址）
  if ((hextet & 0xffc0) === 0xfe80) return true // fe80::/10（链路本地）
  return false
}
