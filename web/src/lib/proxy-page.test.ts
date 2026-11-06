// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 接线 tripwire（spec proxy-pool §9.4）：本项目没有 jsdom / 渲染测试，纯函数覆盖不到
// 「导航/路由/请求路径/账号页投影有没有按这些规则接线」。这些用例直接读源码或用纯函数
// 断言——它们不是行为测试，只守最容易静默回归的四处：
//   ① 两个导航项与两条路由存在；
//   ② 代理页调的相对路径与 schema.d.ts 的操作 id 对应得上（两套 path 互不重叠）；
//   ③ 账号编辑的 payload 投影对「未触碰」不产生 proxy_id（防线：整表单覆盖会静默
//      解绑管理员绑的、而读面不可见的平台代理）；
//   ④ codex 类型账号不再置灰（v7 已覆盖 codex 面）。
import { describe, it } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import {
  PROXY_DIRECT_VALUE,
  PROXY_UNCHANGED_VALUE,
  buildAccountPatch,
  buildBatchProxyFields,
  echoMayHideBinding,
  parseBatchProxyValue,
  proxySelectItems,
  proxySelectValue,
  proxySelectionOf,
} from './account-proxy.ts'

const read = (rel: string) => readFileSync(new URL(rel, import.meta.url), 'utf8')
const shell = read('../components/app-shell.tsx')
const app = read('../App.tsx')
const client = read('../lib/api/client.ts')
const schema = read('../lib/api/schema.d.ts')
const accounts = read('../pages/accounts.tsx')
const proxyPage = read('../components/proxy-pool-page.tsx')
const proxyForm = read('../lib/proxy-form.ts')
const enLocale = read('../locales/en.ts')
const zhLocale = read('../locales/zh.ts')

// 取 `const NAME ... = [ ... ]` 的数组字面量主体（导航项 source-text 断言的锚点）。
function navBlock(src: string, name: string): string {
  const start = src.indexOf(`const ${name}`)
  assert.notEqual(start, -1, `找不到 ${name}`)
  const end = src.indexOf('\n]', start)
  assert.notEqual(end, -1, `${name} 的数组未闭合`)
  return src.slice(start, end)
}

describe('导航与路由', () => {
  it('adminNav 含 /app/proxies（面包屑复用同一数组）', () => {
    const nav = navBlock(shell, 'adminNav')
    assert.match(nav, /to: '\/app\/proxies', key: 'nav\.proxies'/)
  })

  it('supplierNav 含 /user/supplier/proxies', () => {
    const nav = navBlock(shell, 'supplierNav')
    assert.match(nav, /to: '\/user\/supplier\/proxies', key: 'supplier\.nav\.proxies'/)
  })

  it('App.tsx 挂了两条路由，且供应商面那条在 SupplierScope 之下（base 才切得对）', () => {
    // 管理面：/app 的 children 里
    const adminStart = app.indexOf("path: 'app'")
    const supplierStart = app.indexOf("path: 'supplier'")
    assert.ok(adminStart !== -1 && supplierStart !== -1)
    const adminRoutes = app.slice(adminStart, supplierStart)
    assert.match(adminRoutes, /path: 'proxies', element: <Proxies \/>/)

    // 供应商面：SupplierScope 内（作用域 = supplierApi ⇒ /api/user/supplier/proxies）
    const scopeStart = app.indexOf('<SupplierScope />')
    assert.notEqual(scopeStart, -1)
    const scopeRoutes = app.slice(scopeStart, app.indexOf('</ApiScopeProvider>'))
    assert.match(scopeRoutes, /path: 'proxies', element: <SupplierProxies \/>/)
    assert.match(app, /import SupplierProxies from '@\/pages\/supplier\/proxies'/)
  })
})

describe('请求路径 ↔ schema 操作 id', () => {
  it('client.ts 只发相对路径，两个 base 分别落在管理面/供应商面', () => {
    assert.match(client, /listProxies = \(\) => this\.request<[^>]+>\('\/proxies'\)/)
    assert.match(client, /`\/proxies\/\$\{id\}`/)
    assert.match(client, /`\/proxies\/\$\{id\}\/probe`/)
    // base 常量：管理面 /api/admin、供应商面 /api/user/supplier
    assert.match(client, /adminApi = new ApiClient\(userAuth\.getToken, '\/api\/admin'\)/)
    assert.match(client, /supplierApi = new ApiClient\(userAuth\.getToken, '\/api\/user\/supplier'\)/)
  })

  it('schema.d.ts 里两套 path 与十个操作 id 齐备（管理面 admin tag / 供应商面 supplier tag）', () => {
    for (const path of ['"/proxies"', '"/proxies/{id}"', '"/proxies/{id}/probe"',
      '"/api/user/supplier/proxies"', '"/api/user/supplier/proxies/{id}"', '"/api/user/supplier/proxies/{id}/probe"']) {
      assert.ok(schema.includes(path), `schema 缺 path ${path}`)
    }
    for (const op of ['GetProxies', 'PostProxies', 'GetProxiesId', 'PutProxiesId', 'DeleteProxiesId', 'PostProxiesIdProbe',
      'GetSupplierProxies', 'PostSupplierProxies', 'GetSupplierProxiesId', 'PutSupplierProxiesId', 'DeleteSupplierProxiesId', 'PostSupplierProxiesIdProbe']) {
      assert.ok(schema.includes(`operations["${op}"]`), `schema 缺操作 ${op}`)
    }
  })

  it('代理页不自己拼 URL（一律走 api.*），且探测不按失败处理', () => {
    assert.match(proxyPage, /api\.listProxies\(\)/)
    assert.match(proxyPage, /api\.probeProxy\(/)
    assert.match(proxyPage, /api\.createProxy\(|api\.updateProxy\(/)
    assert.match(proxyPage, /api\.deleteProxy\(/)
    assert.ok(!/fetch\(/.test(proxyPage), '不要在页面里裸 fetch')
  })
})

describe('账号出口代理的三态投影（安全防线）', () => {
  it('★ 回显为 null 且用户未触碰 ⇒ payload 不含 proxy_id 键', () => {
    // 这是「整表单覆盖会静默解绑管理员配置」的钉子：供应商面账号可能被管理员绑了
    // 平台代理，而读面回显 null（不可见）。把回显当成「要解绑」= 静默破坏管理配置。
    for (const initial of [null, undefined]) {
      const patch = buildAccountPatch(initial, { kind: 'direct' })
      assert.deepEqual(patch, {})
      assert.ok(!('proxy_id' in patch), '回显 null 时绝不能出现 proxy_id 键')
    }
  })

  it('回显为 null 时选直连也不发 null（无法区分「真未绑定」与「看不见的绑定」）', () => {
    // 用户只是重新点了一下「直连」（闭合态本来就是直连）不构成解绑意图。
    assert.ok(!('proxy_id' in buildAccountPatch(null, proxySelectionOf(null))))
  })

  it('绑定：变了才发 id；没变不发', () => {
    assert.deepEqual(buildAccountPatch(null, { kind: 'bind', id: 7 }), { proxy_id: 7 })
    assert.deepEqual(buildAccountPatch(3, { kind: 'bind', id: 7 }), { proxy_id: 7 })
    assert.deepEqual(buildAccountPatch(7, { kind: 'bind', id: 7 }), {})
    // 非法 id 当作未触碰（fail-closed，不发明绑定）
    assert.deepEqual(buildAccountPatch(null, { kind: 'bind', id: 0 }), {})
  })

  it('解绑：只有回显非 null（代理对调用方可见）才发 null', () => {
    assert.deepEqual(buildAccountPatch(7, { kind: 'direct' }), { proxy_id: null })
  })

  it('批量三态：不变 ⇒ 不发键；直连 ⇒ null；具体代理 ⇒ id', () => {
    assert.deepEqual(buildBatchProxyFields(null), {})
    assert.deepEqual(buildBatchProxyFields({ kind: 'direct' }), { proxy_id: null })
    assert.deepEqual(buildBatchProxyFields({ kind: 'bind', id: 9 }), { proxy_id: 9 })

    assert.equal(parseBatchProxyValue(PROXY_UNCHANGED_VALUE), null)
    assert.deepEqual(parseBatchProxyValue(PROXY_DIRECT_VALUE), { kind: 'direct' })
    assert.deepEqual(parseBatchProxyValue('9'), { kind: 'bind', id: 9 })
  })

  it('Select 哨兵与 label 同源（闭合态与弹出列表显示一致）', () => {
    const items = proxySelectItems(
      [{ id: 4, name: 'hk', scheme: 'socks5', host: 'p.example.com', port: 1080 }],
      { directLabel: 'Direct', unchangedLabel: 'Unchanged' },
    )
    assert.equal(items[PROXY_DIRECT_VALUE], 'Direct')
    assert.equal(items[PROXY_UNCHANGED_VALUE], 'Unchanged')
    assert.equal(items['4'], 'hk (socks5://p.example.com:1080)')
    assert.equal(proxySelectValue(proxySelectionOf(4)), '4')
    assert.equal(proxySelectValue(proxySelectionOf(null)), PROXY_DIRECT_VALUE)
  })

  it('停用代理在选项上可见（绑定停用代理 ⇒ 账号不被调度，不能只靠后端报错解释）', () => {
    const items = proxySelectItems(
      [
        { id: 4, name: 'on', scheme: 'socks5', host: 'p1.example.com', port: 1080, enabled: true },
        { id: 5, name: 'off', scheme: 'socks5', host: 'p2.example.com', port: 1080, enabled: false },
      ],
      { disabledSuffix: ' · disabled' },
    )
    assert.equal(items['4'], 'on (socks5://p1.example.com:1080)')
    assert.equal(items['5'], 'off (socks5://p2.example.com:1080) · disabled')
  })

  it('回显可能藏绑定的提示只在供应商面 + 回显 null 时出现', () => {
    assert.equal(echoMayHideBinding('supplier', null), true)
    assert.equal(echoMayHideBinding('supplier', 4), false)
    assert.equal(echoMayHideBinding('admin', null), false)
  })
})

describe('accounts 页面接线（源码形状）', () => {
  it('编辑保存路径走 buildAccountPatch，且落在归属分支之外（供应商面也要能绑自己的代理）', () => {
    assert.match(accounts, /buildAccountPatch\(editing\.proxy_id, parseProxySelectValue\(f\.proxy\)\)/)
    // 归属三态只在管理面（supplier_user_id 恒为 JWT 本人）；proxy 必须在其 `}` 之后、
    // PATCH 之前——写进那个分支就会让供应商永远绑不上自己的代理。
    const ordered = /if \(!isSupplierScope\) \{[\s\S]*?\n      \}\n[\s\S]{0,600}?Object\.assign\(patch, buildAccountPatch\(editing\.proxy_id[\s\S]{0,200}?await api\.updateAccount\(id, patch, editing\.LifecycleRevision\)/
    assert.match(accounts, ordered, 'proxy 三态必须在归属分支之后、PATCH 之前执行')
  })

  it('批量编辑默认「不变」（哨兵），投影走 buildBatchProxyFields', () => {
    assert.match(accounts, /proxy: PROXY_UNCHANGED_VALUE,/)
    assert.match(accounts, /buildBatchProxyFields\(parseBatchProxyValue\(batchForm\.proxy\)\)/)
  })

  it('单条编辑的出口代理 Select 不按凭据类型置灰（codex 面 v7 已覆盖）', () => {
    const anchor = accounts.indexOf('accounts.proxy.label')
    assert.notEqual(anchor, -1, '找不到出口代理的 Label')
    const block = accounts.slice(anchor, accounts.indexOf('</Select>', anchor))
    assert.ok(!/disabled=/.test(block), '出口代理选择器不得有 disabled（含 codex 置灰）')
    assert.ok(!/isCodex/.test(block), '出口代理选择器不得按 codex 类型分流')
    // 候选列表带缓存键 + 作用域，避免两面互相污染
    assert.match(accounts, /queryKey: \['proxies', kind\]/)
  })

  it('代理池页面用的作用域来自 useAccountSurface（不硬编码 admin）', () => {
    assert.match(proxyPage, /useAccountSurface\(\)/)
    assert.match(proxyPage, /queryKey: \['proxies', kind\]/)
    // 供应商面差异：无归属列、无「分配给账号」动作
    assert.match(proxyPage, /\{!isSupplier && <TableHead className="w-28">/)
  })
})

describe('地址判据只在后端（C3：删掉前端第二套 IP 分类器）', () => {
  // 只查**代码**：下面剥掉块注释与**整行**行注释（注释里出现「已删 isPrivateEgressHost」
  // 是好事，不该让门禁误报）。注意行尾的行注释（`x = 1 // 备注`）仍留在代码串里会被
  // 扫到——所以私网段/特殊名不要写进行尾注释。
  const proxyFormCode = proxyForm.replace(/\/\*[\s\S]*?\*\//g, '').replace(/^\s*\/\/.*$/gm, '')

  it('proxy-form.ts 不再导出私网判定，也不再解析 IPv4/IPv6', () => {
    assert.ok(!/isPrivateEgressHost|parseIPv4|isPrivateIP/.test(proxyFormCode), '前端不得再维护 IP 分类器')
    // 私网段名单是"第二份事实"的形状特征：出现在前端**代码**里就说明有人把它加回来了。
    for (const hint of ['192.168', '172.16', '10.0.0', 'fc00::', 'fe80::', '.localhost', 'fe00']) {
      assert.ok(!proxyFormCode.includes(hint), `proxy-form.ts 不得出现私网段/特殊名 ${hint}`)
    }
  })

  it('供应商面的提示是固定说明（不随 host 输入变化），失败时透出后端 message', () => {
    // 固定说明：键与 JSX 都只有一处，且不挂在任何"私网判定"状态上。
    assert.match(proxyPage, /\{t\('proxies\.hostPolicyHint'\)\}/)
    assert.ok(!/privateHost/.test(proxyPage), '页面不得再有 privateHost 提示状态')
    // 400（内网/回环拒绝）没有字段级错误清单 ⇒ 展示后端 message。
    assert.match(proxyPage, /errMsg\(save\.error\)/)
  })

  it('i18n 两侧同批：hostPolicyHint 存在、旧的两条私网提示已删净', () => {
    for (const locale of [enLocale, zhLocale]) {
      assert.ok(locale.includes('"hostPolicyHint"'), '两侧都要有 hostPolicyHint')
      assert.ok(!locale.includes('"privateHostHint"'), '旧的 privateHostHint 必须删净')
      assert.ok(!locale.includes('"privateHostRejected"'), '旧的 privateHostRejected 必须删净')
    }
  })
})
