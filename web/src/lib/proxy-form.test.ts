// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 代理表单纯逻辑的门（spec proxy-pool §9.4「前端纯逻辑」）：校验矩阵 + 三态投影 +
// 私网判据。项目没有 jsdom/渲染测试，所以「页面有没有按这些规则接线」由
// proxy-page.test.ts 的源码 tripwire 守。
import { describe, it } from 'node:test'
import assert from 'node:assert/strict'
import {
  PROXY_HOST_MAX,
  PROXY_NAME_MAX,
  PROXY_SCHEMES,
  buildProxyPayload,
  emptyProxyDraft,
  isPrivateEgressHost,
  normalizeProxyHost,
  proxyDraftOf,
  proxyEndpoint,
  validateProxyForm,
  type ProxyDraft,
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

describe('isPrivateEgressHost（只用于供应商面提示，判据仍在后端）', () => {
  const priv = (host: string, want: boolean) =>
    assert.equal(isPrivateEgressHost(host), want, `${host} 应为 ${want ? '私网' : '公网'}`)

  it('IPv4 私网段逐个命中', () => {
    priv('10.0.0.1', true)
    priv('10.255.255.255', true)
    priv('172.16.0.1', true)
    priv('172.31.255.255', true)
    priv('192.168.1.1', true)
    priv('127.0.0.1', true)
    priv('169.254.169.254', true) // 云元数据
    priv('100.64.0.1', true) // CGNAT
    priv('100.127.255.255', true)
    priv('0.0.0.0', true)
  })

  it('相邻公网边界不误伤（172.31 vs 172.32、100.127 vs 100.128）', () => {
    priv('172.32.0.1', false)
    priv('172.15.255.255', false)
    priv('100.128.0.1', false)
    priv('100.63.255.255', false)
    priv('11.0.0.1', false)
    priv('192.169.0.1', false)
    priv('8.8.8.8', false)
  })

  it('名字与后缀：localhost / .localhost / .local', () => {
    priv('localhost', true)
    priv('LOCALHOST', true)
    priv('api.localhost', true)
    priv('printer.local', true)
    priv('example.com', false)
    priv('local.example.com', false) // 只认 .local 后缀（local.example.com 结尾不是 .local）
  })

  it('IPv6：::1、fc00::/7、fe80::/10、IPv4-mapped', () => {
    priv('::1', true)
    priv('[::1]', true)
    priv('::', true) // 未指定地址（等价 0/8）
    priv('fc00::1', true)
    priv('fd12:3456::1', true)
    priv('fe80::1', true)
    priv('febf::1', true)
    priv('fec0::1', false) // site-local 不在名单内（且已废弃）
    priv('2001:db8::1', false)
    priv('::ffff:127.0.0.1', true)
    priv('::ffff:10.1.2.3', true)
    priv('::ffff:8.8.8.8', false)
  })

  it('畸形输入不误判为私网（判据在后端）', () => {
    priv('', false)
    priv('10.0.0', false)
    priv('10.0.0.256', false)
    priv('not an ip', false) // 含空格：不是 IP 也不是特殊名
  })

  it('尾点 FQDN 归一后仍命中', () => {
    priv('localhost.', true)
    priv('printer.local.', true)
    priv('example.com.', false)
  })
})

describe('proxyEndpoint', () => {
  it('IPv6 展示必须加回方括号', () => {
    assert.equal(proxyEndpoint({ scheme: 'http', host: '::1', port: 8080 }), 'http://[::1]:8080')
    assert.equal(proxyEndpoint({ scheme: 'socks5', host: 'p.example.com', port: 1080 }), 'socks5://p.example.com:1080')
  })
})
