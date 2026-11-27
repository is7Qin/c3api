// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 接线 tripwire（spec proxy-pool §9.4）：本项目没有 jsdom / 渲染测试，纯函数覆盖不到
// 「导航/路由/请求路径/账号页投影有没有按这些规则接线」。这些用例直接读源码或用纯函数
// 断言——它们不是行为测试，只守最容易静默回归的四处：
//   ① 两个导航项与两条路由存在；
//   ② 代理页调的相对路径与 schema.d.ts 的操作 id 对应得上（两套 path 互不重叠）；
//   ③ 账号编辑的 payload 投影（模块 draftToPatch）对「未触碰」不产生 proxy_id（防线：整表单覆盖会静默
//      解绑管理员绑的、而读面不可见的平台代理）；
//   ④ codex 类型账号不再置灰（v7 已覆盖 codex 面）；
//   ⑤ 批量探测：并发上限只有一个来源、批量路径只在全部结束后刷一次列表（M10/M11）。
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
const draftModule = read('./account-draft.ts')
const concurrencySrc = read('./concurrency.ts')
const proxyPage = read('../components/proxy-pool-page.tsx')
const proxyForm = read('../lib/proxy-form.ts')
const enLocale = read('../locales/en.ts')
const zhLocale = read('../locales/zh.ts')
const importDialog = read('../components/codex-import/import-dialog.tsx')
const loginDialog = read('../components/codex-import/login-dialog.tsx')

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
  it('proxy 投影在 draftToPatch 内，且与归属作用域守卫平级（供应商面也要能绑自己的代理）', () => {
    // 投影已收敛到模块：页面只交草稿 + 上下文（回显 id / 作用域），不再自己组合补丁。
    assert.match(accounts, /draftToPatch\(draft, \{[\s\S]{0,200}?echoProxyId: editing\?\.proxy_id \?\? null,[\s\S]{0,80}?isSupplierScope,/)
    // proxy 赋值必须与 supplier_user_id 的守卫**平级**（同为函数体顶层语句）：
    // 写进那个分支就会让供应商永远绑不上自己的代理。缩进是最廉价的结构判据。
    const at = draftModule.indexOf('export function draftToPatch(')
    const end = draftModule.indexOf('export function assertCreate(')
    assert.ok(at !== -1 && end > at, '找不到 draftToPatch')
    const body = draftModule.slice(at, end)
    assert.match(body, /^ {2}if \(d\.supplier_user_id !== undefined && !ctx\.isSupplierScope\)/m)
    // 批量注入的 proxyFields 优先（批量「直连」必须发 null，不能用单条的 echo 规则），
    // 未注入时才走单条的 buildAccountPatch；两者都必须是函数体顶层语句。
    assert.match(body, /^ {2}if \(ctx\.proxyFields !== undefined\) Object\.assign\(p, ctx\.proxyFields\)/m)
    assert.match(body, /^ {2}else if \(d\.proxy !== undefined\) Object\.assign\(p, buildAccountPatch\(ctx\.echoProxyId, d\.proxy\)\)/m)
    // 先后关系：归属守卫在前、proxy 在后（不是嵌套）
    assert.ok(body.indexOf('d.supplier_user_id') < body.indexOf('buildAccountPatch('), 'proxy 投影必须在归属守卫之后执行')
  })

  it('批量编辑默认「不变」（草稿缺键 ⇒ 哨兵），投影走 buildBatchProxyFields', () => {
    // 三态收敛到稀疏草稿：proxy 缺席 = 不变（Select 显示哨兵），显式选择才落键。
    assert.match(accounts, /batchDraft\.proxy === undefined \? PROXY_UNCHANGED_VALUE : proxySelectValue\(batchDraft\.proxy\)/)
    assert.match(accounts, /buildBatchProxyFields\(batchDraft\.proxy \?\? null\)/)
    // 旧快照表单已删：任何 batchForm 引用都说明又长回了第二份真值。
    assert.equal(/\bbatchForm\b/.test(accounts), false)
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
    assert.match(proxyPage, /\{showOwner && <TableHead className="w-28">/)
  })
})

describe('地址判据只在后端（C3：删掉前端第二套 IP 分类器）', () => {
  // 扫的是**剥掉部分注释后的源码文本**（不是语法树）：下面剥块注释与**整行**行注释
  // （注释里出现「已删 isPrivateEgressHost」是好事，不该让门禁误报）。行尾的行注释
  // （`x = 1 // 备注`）与字符串字面量仍留在文本里会被扫到——所以私网段/特殊名不要写在
  // 行尾注释或字符串里。这是一条廉价的文本门禁，不是解析器。
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

// 批量导入（spec-proxy-import）的接线：解析、路径、文案分流各自**只有一处**实现。
describe('批量导入接线（spec-proxy-import）', () => {
  it('页面把请求交给 api.importProxies，不自己拼路径（切 base 即切作用域）', () => {
    assert.match(proxyPage, /api\.importProxies\(/)
    assert.ok(!proxyPage.includes('/proxies/import'), '页面不得自己拼路径')
    assert.match(client, /importProxies = \(rows:[\s\S]{0,120}?'\/proxies\/import'/)
  })

  it('粘贴文本的拆分只发生在 lib/proxy-form（页面不自己 split/正则）', () => {
    assert.match(proxyPage, /parseProxyImportLines\(/)
    assert.ok(!proxyPage.includes(".split('\\n')"), '文本拆分只在 lib/proxy-form 一处')
  })

  it('派生 name 复用 proxyEndpoint（与列表展示同一拼法，不另写一份）', () => {
    // 基名只有一个拼法；批次内序号是**第二遍**（assignImportNames）的事，页面不参与。
    assert.match(proxyForm, /const base = proxyEndpoint\(\{ scheme, host, port \}\)/)
    assert.match(proxyForm, /return assignImportNames\(out\)/)
  })

  it('失败行按 reason 分流：conflict 用本地化文案，其余透出后端原文', () => {
    assert.match(proxyPage, /r\.reason === 'conflict' \? t\('proxies\.nameConflict'\) : \(r\.error \?\?/)
  })

  it('上限与空态都挡住提交（不在服务端才发现超限）', () => {
    assert.match(proxyPage, /importable\.length > PROXY_IMPORT_MAX_ROWS/)
    assert.match(proxyPage, /disabled=\{importMut\.isPending \|\| importable\.length === 0 \|\| overCap\}/)
  })

  it('结果表用提交时的行快照标回原文行号（响应只有请求下标）', () => {
    assert.match(proxyPage, /const at = submitted\[r\.index\]/)
    assert.match(proxyPage, /setSubmitted\(importable\.map/)
  })

  it('i18n 两侧同批：导入文案键集在 en/zh 一致', () => {
    const keys = (src: string) => {
      const start = src.indexOf('"importTitle"')
      assert.notEqual(start, -1, '找不到 importTitle')
      const end = src.indexOf('"importErrors"', start)
      assert.notEqual(end, -1, '找不到 importErrors')
      return [...src.slice(start, end).matchAll(/"([A-Za-z]+)":/g)].map(m => m[1]).sort()
    }
    assert.deepEqual(keys(enLocale), keys(zhLocale))
    // 解析错误码也必须两侧齐全（页面按 DynamicKey 拼，缺一条就是运行时露出原始 key）。
    const errKeys = (src: string) => {
      const start = src.indexOf('"importErrors"')
      const end = src.indexOf('}', start)
      return [...src.slice(start, end).matchAll(/"([A-Za-z]+)":/g)].map(m => m[1]).sort()
    }
    assert.deepEqual(errKeys(enLocale), errKeys(zhLocale))
    assert.deepEqual(errKeys(enLocale), [
      'badHost', 'badPort', 'badScheme', 'emptyHost', 'emptyUsername', 'importErrors', 'nameTooLong', 'needPort',
    ])
  })
})

// 探测列是 last_probe_ip / last_probe_error 的**唯一渲染点**（spec-probe §3.3 三态矩阵）。
// 这里守两条最容易静默回归的接线：出口 IP 有没有被读出来，错误文本有没有又被 ok 门控起来。
describe('探测列：出口 IP 与错误文本（spec-probe §3.3）', () => {
  it('last_probe_ip 非空 ⇒ 行内显示「出口 IP」，mono + tabular-nums 且可选中', () => {
    assert.match(proxyPage, /\{p\.last_probe_ip && \(/)
    assert.match(proxyPage, /t\('proxies\.probeEgressIP'\)/)
    assert.match(proxyPage, /className="font-mono tabular-nums">\{p\.last_probe_ip\}/)
    assert.match(proxyPage, /select-text/, '出口 IP 要可复制')
  })

  it('last_probe_error 与 last_probe_ok 解耦：ok=true 时同样显示（否则取 IP 失败的原因不可见）', () => {
    // 旧形状把错误文本包在 `!p.last_probe_ok && …` 里 ⇒ ok=true + error≠'' 的三态行永远看不到原因。
    assert.ok(
      !/!\s*p\.last_probe_ok\s*&&\s*p\.last_probe_error/.test(proxyPage),
      '错误文本不得再由 last_probe_ok 门控',
    )
    assert.match(proxyPage, /\{p\.last_probe_error && \(/)
    // 既有展示口径不变：truncate 80 + title 全文。
    assert.match(proxyPage, /title=\{p\.last_probe_error\}>\{truncate\(p\.last_probe_error, 80\)\}/)
  })

  it('i18n 标签两侧同键同批（zh「出口 IP」/ en「Egress IP」）', () => {
    assert.match(zhLocale, /"probeEgressIP": "出口 IP"/)
    assert.match(enLocale, /"probeEgressIP": "Egress IP"/)
  })
})

// 批量探测（spec batch-probe §1）：前端有界并发扇出。这里守三条最容易静默回归的接线——
// 并发上限只有一个来源、批量路径只在全部结束后刷一次列表、逐条完成即回填。
describe('批量探测接线（spec batch-probe §1）', () => {
  // 跑批函数的函数体：以 `const runBatchProbe = async () => {` 起，到其**两空格缩进的收尾 `}`**
  // 为止（体内没有更浅的闭合行）。按结构锚点切片，不按行号。
  const batchRunner = (() => {
    const at = proxyPage.indexOf('const runBatchProbe = async () => {')
    assert.notEqual(at, -1, '找不到批量探测的跑批函数')
    const end = proxyPage.indexOf('\n  }\n', at)
    assert.notEqual(end, -1, '跑批函数的收尾花括号找不到（缩进变了要同步改本锚点）')
    return proxyPage.slice(at, end)
  })()

  // 单条探测的包装（页面侧上限 + 失败转 outcome）：同样的结构锚点切片。
  const probeOnce = (() => {
    const at = proxyPage.indexOf('const probeOnce = async (id: number): Promise<ProbeOutcome> => {')
    assert.notEqual(at, -1, '找不到 probeOnce')
    const end = proxyPage.indexOf('\n  }\n', at)
    assert.notEqual(end, -1, 'probeOnce 的收尾花括号找不到')
    return proxyPage.slice(at, end)
  })()

  it('★ 并发常量只定义一处：页面从 lib/proxy-form 取用，绝不写字面量（M10 靶子）', () => {
    // 定义点唯一：三份源码里 `PROBE_CONCURRENCY = <数字>` 只出现一次。
    const defs = (proxyForm + proxyPage + concurrencySrc).match(/PROBE_CONCURRENCY\s*=\s*\d/g) ?? []
    assert.equal(defs.length, 1, 'PROBE_CONCURRENCY 只能有一个定义点')
    assert.match(proxyForm, /export const PROBE_CONCURRENCY = 4/)
    // 使用点必须传常量（改常量 1 / 100 时 mapWithConcurrency 单测的峰值断言会红）。
    assert.match(batchRunner, /mapWithConcurrency\(ids, PROBE_CONCURRENCY,/)
    assert.ok(!/mapWithConcurrency\([^)]*,\s*\d+\s*,/.test(batchRunner), 'limit 必须是常量，不是字面量')
  })

  it('按钮：无选中或 running 时禁用，文案在「批量探测 (N)」与「探测中 x/N」之间切换', () => {
    const at = proxyPage.indexOf('data-od-id="proxy-batch-probe"')
    assert.notEqual(at, -1, '找不到批量探测按钮')
    const btn = proxyPage.slice(proxyPage.lastIndexOf('<Button', at), proxyPage.indexOf('</Button>', at))
    assert.match(btn, /disabled=\{running \|\| selectedIds\.length === 0\}/)
    assert.match(btn, /t\('proxies\.batchProbeRunning', \{ done: /)
    assert.match(btn, /t\('proxies\.batchProbe', \{ count: selectedIds\.length \}\)/)
  })

  it('★ 只在全部结束后 invalidate 一次（M11 靶子：改成每条都刷必红）', () => {
    assert.equal(batchRunner.match(/invalidate\(\)/g)?.length, 1, '批量路径只能有一处 invalidate')
    assert.ok(
      batchRunner.indexOf('await mapWithConcurrency') < batchRunner.indexOf('invalidate()'),
      '刷新必须在全部探测之后（逐条刷 = 每条都重载列表）',
    )
    // 单条 probe mutation 的 onSuccess 就是「每条都刷」——批量路径不得复用它。
    assert.ok(!/probe\.mutate/.test(batchRunner), '批量路径不得复用单条 probe mutation')
  })

  it('逐条完成即回填（单条失败也进结果表），不是等全部结束才显示', () => {
    assert.match(batchRunner, /setLive\(prev => applyOutcome\(prev, id, outcome\)\)/)
    // 请求失败在 probeOnce 内转成 outcome：既不 reject（否则整批提前收尾），也不丢行。
    assert.match(probeOnce, /catch \(e\) \{[\s\S]{0,160}?kind: 'requestFailed'/)
    assert.match(probeOnce, /return \{ kind: 'row', row, at: row\.last_probe_at/, '200 结论同样落成 outcome')
    // 渲染：本轮结论（含第四态）经 probeCell 交给 ProbeCell；占位只给「在跑、还没结论」的行。
    assert.match(
      proxyPage,
      /const probeCell = \(p: ProxyView\) => \{[\s\S]{0,220}?<ProbeCell p=\{probeView\(p, outcome\)\} live=\{outcome\} pending=\{running && inBatch\(p\.id\) && outcome === undefined\} \/>/,
    )
    // 归属判定走批次快照：跑批期间 rows 变化不改变「谁在跑、结果给谁」。
    assert.match(
      proxyPage,
      /const liveFor = \(id: number\): ProbeOutcome \| undefined => \(running && !inBatch\(id\) \? undefined : live\.get\(id\)\)/,
    )
  })

  it('★ 批次快照固定归属：跑批期间 rows 变化不得让在跑的行丢占位/丢结果（P1-3）', () => {
    assert.match(proxyPage, /const \[batchIds, setBatchIds\] = useState<number\[\] \| null>\(null\)/)
    assert.match(proxyPage, /const inBatch = \(id: number\) => batchSet !== null && batchSet\.has\(id\)/)
    assert.match(batchRunner, /setBatchIds\(ids\)/, '开始时把选中集固定成批次快照')
    assert.match(batchRunner, /setBatchIds\(null\)/, '结束时清快照')
    // 占位/归属都不得回退到每帧从 rows 派生的 selectedSet（那正是 P1-3 的回归形状）。
    assert.ok(!/pending=\{running && selectedSet\.has/.test(proxyPage), '占位判定不得回退到 selectedSet')
    assert.ok(!/withLiveProbe/.test(proxyPage), '结果归属不得绕过批次快照（旧的 withLiveProbe 已删除）')
  })

  it('★ 刷新失败时保留已拿到的结论：按 query error 判定，不看 promise（P1-2）', () => {
    // invalidateQueries 在 refetch 失败时**仍然 resolve** ⇒ 判据只能是 query 的 error 状态。
    assert.match(batchRunner, /const refreshError = qc\.getQueryState\(\['proxies', kind\]\)\?\.error/)
    assert.match(
      batchRunner,
      /if \(refreshError == null\) \{[\s\S]{0,260}?pruneForRound\(prev, myRun, runRef\.current\)/,
      '只有刷新成功才丢成功结论',
    )
    assert.ok(!/\.then\(\(\) => setLive\(/.test(batchRunner), '不得只按 promise 收尾（失败也 resolve）')
    // 刷新失败要让用户知道（复用既有 loadFailed 文案，不新造键）。
    assert.match(batchRunner, /t\('common\.loadFailed', \{ message:/)
  })

  it('★ 轮次令牌：上一轮的收尾不得清掉新一轮的结果（B）', () => {
    assert.match(proxyPage, /const runRef = useRef\(0\)/)
    assert.match(batchRunner, /const myRun = runRef\.current \+ 1\n    runRef\.current = myRun/)
    // 跨轮不生效的判定被搬进纯函数（probe-live.test.ts 有专项用例），这里守调用点。
    assert.match(batchRunner, /pruneForRound\(prev, myRun, runRef\.current\)/)
  })

  it('★ 服务端重新决定这一行 ⇒ 本地结论作废（A：单条探测 / 编辑 / 删除三处）', () => {
    // 单条探测成功：服务端已写回更新的 last_probe_*。
    assert.match(proxyPage, /onSuccess: \(_row, id\) => \{[\s\S]{0,220}?setLive\(prev => dropIds\(prev, \[id\]\)\)/)
    // 编辑保存成功：被编辑的那一行。
    assert.match(proxyPage, /if \(editing\) setLive\(prev => dropIds\(prev, \[editing\.id\]\)\)/)
    // 删除成功：整行消失。
    assert.match(proxyPage, /onSuccess: \(_res, id\) => \{[\s\S]{0,260}?setLive\(prev => dropIds\(prev, \[id\]\)\)/)
    // 刷新后服务端更晚的结论由 pickLive 的 at 比较兜住（差异语义的用例在 probe-live.test.ts）。
    assert.match(proxyPage, /pickLive\(p, liveFor\(p\.id\)\)/)
  })

  it('★ requestFailed 是第四态：不写探测列、单独分支渲染（C）', () => {
    assert.match(proxyPage, /live\?\.kind === 'requestFailed'/, '第四态必须有独立分支')
    const at = proxyPage.indexOf("live?.kind === 'requestFailed'")
    const branch = proxyPage.slice(at, proxyPage.indexOf('if (p.last_probe_at == null)', at))
    assert.ok(!/last_probe_ok/.test(branch), '第四态不得读/写 ok 列（显示「可达/不可达」就是伪造结论）')
    assert.match(branch, /t\('proxies\.batchProbeRequestFailed'/)
    // 合并只由 probeView 做（requestFailed 原样返回列表快照，见 probe-live.test.ts）。
    assert.match(proxyPage, /p=\{probeView\(p, outcome\)\}/)
  })

  it('★ 删除后不留幽灵：live 与选中集一起清；跑批期间禁止改/删（D）', () => {
    assert.match(
      proxyPage,
      /setLive\(prev => dropIds\(prev, \[id\]\)\)\n      setSelected\(s => s\.filter\(x => x !== id\)\)/,
      '删除成功要同时清掉本地结论与选中项',
    )
    assert.match(proxyPage, /title=\{t\('common\.edit'\)\} onClick=\{\(\) => openEdit\(p\)\} disabled=\{running\}/)
    assert.match(proxyPage, /title=\{t\('common\.delete'\)\} onClick=\{\(\) => setDeleting\(p\)\} disabled=\{running\}/)
  })

  it('★ 单条调用有页面侧上限：超时常量与并发上限同处、只一处（E）', () => {
    const defs = (proxyForm + proxyPage).match(/PROBE_CALL_TIMEOUT_MS\s*=\s*[\d_]+/g) ?? []
    assert.equal(defs.length, 1, 'PROBE_CALL_TIMEOUT_MS 只能有一个定义点')
    assert.match(proxyForm, /export const PROBE_CALL_TIMEOUT_MS = 30_000/)
    assert.match(probeOnce, /withTimeout\(\n?\s*api\.probeProxy\(id\),\n?\s*PROBE_CALL_TIMEOUT_MS,/)
    assert.match(probeOnce, /t\('proxies\.batchProbeTimedOut', \{ seconds: PROBE_CALL_TIMEOUT_MS \/ 1000 \}\)/)
    // 超时也是 requestFailed 结论（否则一条悬挂调用会让 running 永久为真）。
    assert.match(probeOnce, /kind: 'requestFailed'/)
  })

  it('汇总 toast 分三档 + 请求失败（§3.3：ok 且有错误 ≠ 成功）', () => {
    assert.match(batchRunner, /if \(outcome\.kind === 'requestFailed'\) requestFailed\+\+/)
    assert.match(batchRunner, /else if \(!outcome\.row\.last_probe_ok\) unreachable\+\+/)
    assert.match(batchRunner, /else if \(outcome\.row\.last_probe_error\) egressFailed\+\+/, 'TCP 通但取 IP 失败要单列')
    assert.match(batchRunner, /t\('proxies\.batchProbeOutcomes', \{ reachable, unreachable, egressFailed, requestFailed \}\)/)
    assert.match(batchRunner, /type: reachable === ids\.length \? 'success' : 'error'/)
    assert.ok(!proxyPage.includes("'proxies.batchProbeSummary'"), '旧的双档汇总键必须删净')
    for (const locale of [enLocale, zhLocale]) {
      assert.ok(locale.includes('"batchProbeOutcomes"'), '新旧键两侧都要有 batchProbeOutcomes')
      assert.ok(!locale.includes('"batchProbeSummary"'), '旧的双档汇总键必须删净')
    }
  })

  it('多选列照 accounts 页做法：表头全选 + 行内 checkbox + data-state 高亮', () => {
    assert.match(proxyPage, /const \[selected, setSelected\] = useState<number\[\]>\(\[\]\)/)
    assert.match(proxyPage, /indeterminate=\{someChecked && !allChecked\}/)
    assert.match(proxyPage, /const toggleRow = \(id: number\) => setSelected/)
    assert.match(proxyPage, /data-state=\{selectedSet\.has\(p\.id\) \? 'selected' : undefined\}/)
  })

  it('选中集跑完不清空（失败的常常要重试）', () => {
    assert.ok(!/setSelected\(\[\]\)/.test(batchRunner), '批量路径不得清空选中集')
  })
})

// 三条**创建**路径（新建 / 导入 / 设备码登录）都能在创建时选出口代理。
// 新建是纯前端缺口（proxy_id 本来就是 AccountCreate 的字段），导入与登录是 wire 缺口。
describe('账号创建三条路径的代理接线（源码形状）', () => {
  it('新建对话框不再把代理选择器藏在编辑态后面', () => {
    // 判据取**正向**：在代理块窗口里必须能改草稿的 proxy，且标签之前不得有
    // `{editing && (` 之类的门。只否定一种旧写法会被 `{editing ? (…) : null}` /
    // `<Field hidden={!editing}>` 绕过去；注释先剥掉，免得注释里的旧形状假红。
    const code = accounts.replace(/\/\*[\s\S]*?\*\//g, '').replace(/^\s*\/\/.*$/gm, '')
    const at = code.indexOf("t('accounts.proxy.label')")
    assert.ok(at > 0, '找不到代理块标签')
    const before = code.slice(Math.max(0, at - 320), at)
    assert.ok(!/\{editing && \(/.test(before), '代理块不得再被 editing 包裹（创建态也要能选）')
    assert.ok(!/hidden=\{!editing\}/.test(before), '代理块不得用 hidden 藏起来')
    const block = code.slice(at, code.indexOf('</div>', at))
    assert.match(block, /update\('proxy', parseProxySelectValue\(v\)\)/, '创建态必须能写进草稿')
    // 创建态用自己的提示文案（「不动这一项就保持原绑定」在建号时无对象）。
    assert.match(code, /editing \? t\('accounts\.proxy\.hint'\) : t\('accounts\.proxy\.createHint'\)/)
    // 不可见绑定提示仍只在编辑态出现（创建态回显恒为 null）。
    assert.match(code, /\{editing && echoMayHideBinding\(kind, editing\.proxy_id\) && \(/)
  })

  it('代理候选查询在两个导入/登录对话框打开时也会拉取，且候选是同一个 query', () => {
    assert.match(accounts, /enabled: dialogOpen \|\| batchUpdateOpen \|\| importOpen \|\| loginOpen/)
    // 候选以 prop 下发（不在对话框里另开一个 query key——那会抄第二份作用域逻辑）。
    const handed = accounts.match(/proxies=\{proxiesQ\.data\?\.rows \?\? \[\]\}/g) ?? []
    assert.equal(handed.length, 2, '登录与导入两个对话框都要拿到同一批候选')
    assert.equal(/useQuery/.test(importDialog), false, '导入对话框不自建 query')
    assert.equal(/useQuery/.test(loginDialog), false, '登录对话框不自建 query')
  })

  it('导入的 proxy_id 进的是 body 级配置（直连 = 不发键）', () => {
    assert.match(importDialog, /proxy_id\?: number/)
    assert.match(importDialog, /if \(proxy\.kind === 'bind'\) cfg\.proxy_id = proxy\.id/)
    assert.match(importDialog, /setCfgProxy\(PROXY_DIRECT_VALUE\)/, '重开对话框必须回到直连')
  })

  it('登录完成体带 proxy_id（直连 = 不发键）', () => {
    assert.match(loginDialog, /proxy_id: proxy\.kind === 'bind' \? proxy\.id : undefined/)
    assert.match(loginDialog, /setProxyId\(PROXY_DIRECT_VALUE\)/, 'reset 必须回到直连')
  })

  it('创建态提示文案两侧都有', () => {
    for (const locale of [enLocale, zhLocale]) {
      assert.ok(locale.includes('"createHint"'), 'accounts.proxy.createHint 必须两侧同时存在')
    }
  })
})
