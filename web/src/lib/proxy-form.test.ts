// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 代理表单纯逻辑的门（spec proxy-pool §9.4「前端纯逻辑」）：格式校验矩阵 + 三态投影。
// **私网/回环/链路本地判据不在这里**（C3：前端不再维护第二份 IP 分类器，判据只有后端
// `normalizeProxyInput` 一份）；这里也不留"从后端源码 grep 私网段"的影子断言。
// 项目没有 jsdom/渲染测试，所以「页面有没有按这些规则接线」由 proxy-page.test.ts 的
// 源码 tripwire 守。
import { describe, it } from 'node:test'
import assert from 'node:assert/strict'
import {
  PROXY_HOST_MAX,
  PROXY_NAME_MAX,
  PROXY_SCHEMES,
  buildProxyPayload,
  parseProxyImportLines,
  emptyProxyDraft,
  normalizeProxyHost,
  proxyDraftOf,
  proxyEndpoint,
  validateProxyForm,
  type ProxyDraft,
  type ProxyScheme,
  type ProxyView,
} from './proxy-form.ts'

const draft = (patch: Partial<ProxyDraft> = {}): ProxyDraft => ({
  ...emptyProxyDraft(),
  name: 'hk-1',
  scheme: 'socks5',
  host: 'proxy.example.com',
  port: '1080',
  ...patch,
})

// 读模型最小替身（后端回显里没有密码，只有 has_password）。
const view = (patch: Partial<ProxyView> = {}): ProxyView => ({
  id: 3,
  name: 'hk-1',
  scheme: 'socks5',
  host: 'proxy.example.com',
  port: 1080,
  username: 'u',
  has_password: true,
  enabled: true,
  owner_uid: null,
  remark: 'r',
  last_probe_at: null,
  last_probe_ok: false,
  last_probe_error: null,
  created_at: '2026-01-01T00:00:00Z',
  updated_at: '2026-01-01T00:00:00Z',
  ...patch,
})

const NO_INTENT = { clearPassword: false, clearUsername: false }

describe('validateProxyForm（与后端 normalizeProxyInput 同口径）', () => {
  it('合法输入 → 无错', () => {
    assert.deepEqual(validateProxyForm(draft()), {})
    for (const scheme of PROXY_SCHEMES) {
      assert.deepEqual(validateProxyForm(draft({ scheme })), {}, `${scheme} 应放行`)
    }
  })

  it('name：trim 后 1..64', () => {
    assert.equal(validateProxyForm(draft({ name: '   ' })).name, 'required')
    assert.equal(validateProxyForm(draft({ name: '' })).name, 'required')
    assert.deepEqual(validateProxyForm(draft({ name: 'x'.repeat(PROXY_NAME_MAX) })), {})
    assert.equal(validateProxyForm(draft({ name: 'x'.repeat(PROXY_NAME_MAX + 1) })).name, 'tooLong')
    // trim 后仍合法：两端空白不算长度
    assert.deepEqual(validateProxyForm(draft({ name: `  ${'x'.repeat(PROXY_NAME_MAX)}  ` })), {})
  })

  it('scheme：四值白名单，大小写不敏感（后端小写后比对）', () => {
    assert.deepEqual(validateProxyForm(draft({ scheme: 'SOCKS5H' })), {})
    assert.equal(validateProxyForm(draft({ scheme: 'socks4' })).scheme, 'scheme')
    assert.equal(validateProxyForm(draft({ scheme: '' })).scheme, 'scheme')
  })

  it('host：非空、无 / @ 空白与控制字符、≤253、方括号归一', () => {
    assert.equal(validateProxyForm(draft({ host: '   ' })).host, 'required')
    assert.equal(validateProxyForm(draft({ host: '[::1]' })).host, undefined)
    assert.equal(validateProxyForm(draft({ host: 'a/b' })).host, 'hostChars')
    assert.equal(validateProxyForm(draft({ host: 'a@b' })).host, 'hostChars')
    assert.equal(validateProxyForm(draft({ host: 'a b' })).host, 'hostChars')
    assert.equal(validateProxyForm(draft({ host: 'a\tb' })).host, 'hostChars')
    // 253 通过、254 超长
    assert.equal(validateProxyForm(draft({ host: 'a'.repeat(PROXY_HOST_MAX) })).host, undefined)
    assert.equal(validateProxyForm(draft({ host: 'a'.repeat(PROXY_HOST_MAX + 1) })).host, 'tooLong')
    // 方括号不算长度：253 的 IPv6 带括号仍通过
    assert.equal(validateProxyForm(draft({ host: `[${'a'.repeat(PROXY_HOST_MAX)}]` })).host, undefined)
  })

  it('port：整数 1..65535', () => {
    assert.equal(validateProxyForm(draft({ port: '' })).port, 'required')
    assert.equal(validateProxyForm(draft({ port: '8.5' })).port, 'portInteger')
    assert.equal(validateProxyForm(draft({ port: 'abc' })).port, 'portInteger')
    assert.equal(validateProxyForm(draft({ port: '0' })).port, 'portRange')
    assert.equal(validateProxyForm(draft({ port: '65536' })).port, 'portRange')
    assert.equal(validateProxyForm(draft({ port: '1' })).port, undefined)
    assert.equal(validateProxyForm(draft({ port: '65535' })).port, undefined)
  })

  it('normalizeProxyHost 只剥一层括号并 trim', () => {
    assert.equal(normalizeProxyHost('  [::1]  '), '::1')
    assert.equal(normalizeProxyHost('proxy.example.com'), 'proxy.example.com')
    assert.equal(normalizeProxyHost('[a]b]'), 'a]b') // 只按首尾判断，畸形串交给后端
  })
})

describe('buildProxyPayload 三态投影（未改 ⇒ 不出键）', () => {
  const initial = view()

  it('原样回显后提交 ⇒ 只有四个必填键，凭据/启用/备注全部缺席', () => {
    const payload = buildProxyPayload(initial, proxyDraftOf(initial), NO_INTENT)
    assert.deepEqual(Object.keys(payload).sort(), ['host', 'name', 'port', 'scheme'])
    assert.ok(!('username' in payload))
    assert.ok(!('password' in payload))
    assert.ok(!('remark' in payload))
    assert.ok(!('enabled' in payload))
  })

  it('显式清除 ⇒ null（不是 ""，也不是缺席）', () => {
    const payload = buildProxyPayload(initial, proxyDraftOf(initial), { clearPassword: true, clearUsername: true })
    assert.equal(payload.password, null)
    assert.equal(payload.username, null)
    assert.ok(Object.prototype.hasOwnProperty.call(payload, 'password'))
    assert.ok(Object.prototype.hasOwnProperty.call(payload, 'username'))
  })

  it('填了新值 ⇒ 落值；清空输入框（未勾清除）⇒ 仍是缺席，绝不发 ""', () => {
    const ok = buildProxyPayload(initial, draft({ username: 'other', password: 'p@ss' }), NO_INTENT)
    assert.equal(ok.username, 'other')
    assert.equal(ok.password, 'p@ss')

    const blank = buildProxyPayload(initial, draft({ username: '', password: '' }), NO_INTENT)
    assert.ok(!('username' in blank))
    assert.ok(!('password' in blank))
  })

  it('清除哨兵优先于输入框内容（UI 会禁用输入，这里是兜底）', () => {
    const payload = buildProxyPayload(initial, draft({ password: 'typed-but-ignored' }), { clearPassword: true, clearUsername: false })
    assert.equal(payload.password, null)
  })

  it('remark：未改 ⇒ 缺席；清空 ⇒ null；有值 ⇒ 落值', () => {
    assert.ok(!('remark' in buildProxyPayload(initial, proxyDraftOf(initial), NO_INTENT)))
    assert.equal(buildProxyPayload(initial, draft({ remark: '' }), NO_INTENT).remark, null)
    assert.equal(buildProxyPayload(initial, draft({ remark: ' 新备注 ' }), NO_INTENT).remark, '新备注')
    // 回显本来就是空串：改成空串不算变化
    const emptyRemark = view({ remark: '' })
    assert.ok(!('remark' in buildProxyPayload(emptyRemark, proxyDraftOf(emptyRemark), NO_INTENT)))
  })

  it('enabled：只在显式切换时出现', () => {
    assert.ok(!('enabled' in buildProxyPayload(initial, proxyDraftOf(initial), NO_INTENT)))
    assert.equal(buildProxyPayload(initial, draft({ enabled: false }), NO_INTENT).enabled, false)
  })

  it('创建态（initial = null）：缺省 enabled 不发；remark 空不发', () => {
    const base = draft()
    assert.ok(!('enabled' in buildProxyPayload(null, base, NO_INTENT)))
    assert.ok(!('remark' in buildProxyPayload(null, base, NO_INTENT)))
    assert.equal(buildProxyPayload(null, draft({ enabled: false }), NO_INTENT).enabled, false)
    assert.equal(buildProxyPayload(null, draft({ remark: 'x' }), NO_INTENT).remark, 'x')
    assert.equal(buildProxyPayload(null, draft({ password: '' }), NO_INTENT).password, undefined)
  })

  it('写入前归一：name/host trim、scheme 小写、port 数字化', () => {
    const payload = buildProxyPayload(null, draft({ name: ' n1 ', scheme: 'SOCKS5H', host: ' [::1] ', port: ' 1080 ' }), NO_INTENT)
    assert.equal(payload.name, 'n1')
    assert.equal(payload.scheme, 'socks5h')
    assert.equal(payload.host, '::1')
    assert.equal(payload.port, 1080)
  })
})

describe('proxyEndpoint', () => {
  it('IPv6 展示必须加回方括号', () => {
    assert.equal(proxyEndpoint({ scheme: 'http', host: '::1', port: 8080 }), 'http://[::1]:8080')
    assert.equal(proxyEndpoint({ scheme: 'socks5', host: 'p.example.com', port: 1080 }), 'socks5://p.example.com:1080')
  })
})

// —— 批量导入的粘贴语法（spec-proxy-import §5）——
//
// 这段只判"能否构成请求"（结构）：不判地址值是否合法（私网/回环、host 字符集仍只有后端
// 一份判据）。name 由端点派生，与 proxyEndpoint 同拼法。
describe('parseProxyImportLines', () => {
  const parse1 = (line: string, scheme: ProxyScheme = 'http') => {
    const rows = parseProxyImportLines(line, scheme)
    assert.equal(rows.length, 1, `应当只解析出一行：${line}`)
    return rows[0]
  }

  it('完整 URI：scheme/host/port 各自落位，name 按端点派生', () => {
    const row = parse1('socks5://10.1.1.1:1080')
    assert.deepEqual(row.config, { name: 'socks5://10.1.1.1:1080', scheme: 'socks5', host: '10.1.1.1', port: 1080 })
    assert.equal(row.line, 1)
    assert.equal(row.raw, 'socks5://10.1.1.1:1080')
  })

  it('省略 scheme ⇒ 用调用方给的默认协议（不猜）', () => {
    assert.equal(parse1('10.1.1.1:1080', 'socks5h').config?.scheme, 'socks5h')
    assert.equal(parse1('10.1.1.1:1080', 'socks5h').config?.name, 'socks5h://10.1.1.1:1080')
  })

  it('scheme 大小写归一；不在白名单 ⇒ badScheme', () => {
    assert.equal(parse1('SOCKS5://h.example.com:1080').config?.scheme, 'socks5')
    assert.equal(parse1('socks4://h.example.com:1080').error, 'badScheme')
  })

  it('凭据：最后一个 @ 分隔，userinfo 的第一个 : 分用户名/密码', () => {
    const row = parse1('socks5://u:p@ss@w0rd@10.1.1.1:1080')
    assert.equal(row.config?.username, 'u')
    assert.equal(row.config?.password, 'p@ss@w0rd', '密码里的 @ 必须原样保留')
    assert.equal(row.config?.host, '10.1.1.1')
  })

  it('密码可含冒号；`u:` 视为无密码', () => {
    assert.equal(parse1('socks5://u:p:q@h.example.com:1080').config?.password, 'p:q')
    const row = parse1('socks5://u:@h.example.com:1080')
    assert.equal(row.config?.username, 'u')
    assert.ok(!('password' in (row.config ?? {})), '空密码不发（后端允许有用户名无密码）')
  })

  it('只有 @ 没有用户名 ⇒ emptyUsername', () => {
    assert.equal(parse1('socks5://:p@h.example.com:1080').error, 'emptyUsername')
  })

  it('IPv6 必须写方括号；裸 IPv6 报 badHost（端口边界不可判）', () => {
    assert.equal(parse1('http://[2001:db8::1]:8080').config?.host, '2001:db8::1')
    assert.equal(parse1('http://[2001:db8::1]:8080').config?.name, 'http://[2001:db8::1]:8080')
    assert.equal(parse1('http://::1:8080').error, 'badHost')
    assert.equal(parse1('http://[2001:db8::1:8080').error, 'badHost')
  })

  it('缺端口 / 端口不是数字 / 越界 / 缺主机', () => {
    assert.equal(parse1('http://h.example.com').error, 'needPort')
    assert.equal(parse1('http://h.example.com:abc').error, 'badPort')
    assert.equal(parse1('http://h.example.com:0').error, 'badPort')
    assert.equal(parse1('http://h.example.com:65536').error, 'badPort')
    assert.equal(parse1('http://:8080').error, 'emptyHost')
  })

  it('派生 name 超过 64 字符 ⇒ nameTooLong（不静默截断）', () => {
    const host = 'a'.repeat(PROXY_NAME_MAX)
    assert.equal(parse1(`http://${host}:80`).error, 'nameTooLong')
  })

  it('空行与 # 整行注释跳过，但行号按原文计', () => {
    const rows = parseProxyImportLines(['# 这是注释', '', 'http://a.example.com:8080', '   ', 'b.example.com:1080'].join('\n'), 'socks5')
    assert.equal(rows.length, 2)
    assert.equal(rows[0].line, 3, '注释与空行占原文行号，但不进结果')
    assert.equal(rows[1].line, 5)
    assert.equal(rows[1].config?.scheme, 'socks5')
  })

  it('行尾不解析注释（密码里可以有 #）', () => {
    assert.equal(parse1('http://u:p#1@h.example.com:8080').config?.password, 'p#1')
  })

  it('单个非法行不影响其余行：结果逐行给出', () => {
    const rows = parseProxyImportLines('http://ok.example.com:8080\nnot-a-proxy', 'http')
    assert.equal(rows.length, 2)
    assert.ok(rows[0].config)
    assert.equal(rows[1].error, 'needPort')
  })
})
