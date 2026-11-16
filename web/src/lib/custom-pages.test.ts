// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 内嵌页面的前端规则层（spec 2026-11-16 内嵌页面 §11-T3 / §11-T9 的设置页段）。
//
// 这一层是**客户端校验**，不是安全边界（服务端 internal/domain/custompage.go 才是）。
// 它值得被钉住的理由有两条：①设置页能就地报错（否则管理员先提交再吃 400）；②iframe
// 地址拼装是纯函数，能断言到"哪个 query 被覆盖、哪个被保留、token 缺失时旧值是否被删"
// 这种粒度——这些恰好是凭据泄漏的实际形状。
//
// 判据逐条对齐 Go 侧（同顺序、同口径：字节长度用 TextEncoder、label 用码点数）。
import { describe, it } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'

import {
  CUSTOM_PAGE_ICONS,
  CUSTOM_PAGE_ICON_COMPONENTS,
  CUSTOM_PAGE_ID_PATTERN,
  CUSTOM_PAGE_MAX_ID,
  CUSTOM_PAGE_MAX_ITEMS,
  CUSTOM_PAGE_MAX_LABEL,
  CUSTOM_PAGE_MAX_TOTAL,
  CUSTOM_PAGE_MAX_URL,
  CUSTOM_PAGE_URL_SCHEMES,
  CUSTOM_PAGE_VISIBILITIES,
  EMBEDDED_RESERVED_PARAMS,
  buildEmbeddedUrl,
  iconForCustomPage,
  normalizeCustomPages,
  pagesForConsole,
  resolveTheme,
  serializeCustomPages,
  tryParseCustomPages,
  validateCustomPages,
  type CustomPageConfig,
} from './custom-pages.ts'

// ok 一条合法条目；用例按需覆盖单字段。
const ok = (over: Partial<CustomPageConfig> = {}): CustomPageConfig => ({
  id: 'docs',
  label: '文档',
  url: 'https://docs.example.com/guide',
  visibility: 'user',
  icon: 'link',
  enabled: true,
  ...over,
})

const codeOf = (pages: readonly CustomPageConfig[]): string | null => validateCustomPages(pages)?.code ?? null
const bytes = (s: string): number => new TextEncoder().encode(s).length

describe('tryParseCustomPages：宽松解析（空形态 = 未配置）', () => {
  it('"" / "[]" / "null" 都解析为空列表且不报错', () => {
    for (const raw of ['', '   ', '[]', ' null ', 'null']) {
      const { pages, error } = tryParseCustomPages(raw)
      assert.deepEqual(pages, [], `raw=${JSON.stringify(raw)}`)
      assert.equal(error, null, `raw=${JSON.stringify(raw)} 不得报错`)
    }
  })

  it('enabled 缺省 = true；显式 false 保留（否则少写一个字段页面就消失）', () => {
    const implicit = tryParseCustomPages('[{"id":"t","label":"L","url":"https://t.example.com"}]')
    assert.equal(implicit.pages[0].enabled, true)
    const explicit = tryParseCustomPages('[{"id":"t","label":"L","url":"https://t.example.com","enabled":false}]')
    assert.equal(explicit.pages[0].enabled, false)
    // JSON null 与"未写"同义：Go 的 `*bool` 把 null 反序列化成 nil（不报错），按缺省 true
    // 处理。若这里拒掉就是前端比后端严。
    const nulled = tryParseCustomPages('[{"id":"t","label":"L","url":"https://t.example.com","enabled":null}]')
    assert.equal(nulled.error, null)
    assert.equal(nulled.pages[0].enabled, true)
  })

  it('enabled 非布尔（"true"/1）→ enabledInvalid（Go 由 encoding/json 直接拒，§4.2）', () => {
    for (const raw of [
      '[{"id":"t","enabled":"true"}]',
      '[{"id":"t","enabled":1}]',
      '[{"id":"t","enabled":0}]',
      '[{"id":"t","enabled":{}}]',
    ]) {
      const { pages, error } = tryParseCustomPages(raw)
      assert.equal(error, 'enabledInvalid', raw)
      assert.deepEqual(pages, [], `${raw} 不得半解析出条目`)
    }
  })

  it('非法 JSON → invalidJson；非数组 → notArray（返回 code，不抛异常）', () => {
    assert.equal(tryParseCustomPages('{oops').error, 'invalidJson')
    assert.deepEqual(tryParseCustomPages('{oops').pages, [])
    for (const raw of ['{"id":"a"}', '"str"', '123', 'true']) {
      assert.equal(tryParseCustomPages(raw).error, 'notArray', raw)
    }
  })

  it('非对象元素 → invalidElement（不再静默丢弃：丢弃等于保存时顺手删一条配置）', () => {
    for (const raw of [
      '[null]',
      '[1,{"id":"t","label":"L","url":"https://t.example.com"}]',
      '[["nested"],{"id":"t"}]',
      '["str"]',
    ]) {
      const { pages, error } = tryParseCustomPages(raw)
      assert.equal(error, 'invalidElement', raw)
      assert.deepEqual(pages, [], `${raw} 不得半解析出条目`)
    }
  })

  it('未知字段忽略、缺字段补空串（取值判定留给 validate）', () => {
    const { pages, error } = tryParseCustomPages(
      '[{"id":"docs","label":"文档","url":"https://d.example.com","icon_svg":"<svg/>","sort_order":2}]',
    )
    assert.equal(error, null)
    assert.equal(pages.length, 1)
    assert.equal(pages[0].visibility, '')
    assert.equal(pages[0].icon, '')
    assert.equal(pages[0].enabled, true)
  })
})

describe('normalizeCustomPages：只 trim label、补缺省、幂等', () => {
  it('trim label，但 url / id 原样保留（不 trim：裁剪会把含控制字符的危险值洗白）', () => {
    const [page] = normalizeCustomPages([
      ok({ id: ' a ', label: ' 文档 ', url: ' https://d.example.com/x\ny ' }),
    ])
    assert.equal(page.label, '文档')
    assert.equal(page.id, ' a ', 'id 不 trim（带空白即 idPattern 拒绝）')
    assert.equal(page.url, ' https://d.example.com/x\ny ', 'url 不 trim（否则换行被裁掉就查不出控制字符）')
  })

  it('空 visibility / icon 补 user / link；非法枚举原样留给校验拒绝', () => {
    const [blank] = normalizeCustomPages([ok({ visibility: '', icon: '' })])
    assert.equal(blank.visibility, 'user')
    assert.equal(blank.icon, 'link')
    const [wrong] = normalizeCustomPages([ok({ visibility: 'all', icon: 'rocket' })])
    assert.equal(wrong.visibility, 'all', '写错枚举必须报错，不能被纠正成 user')
    assert.equal(wrong.icon, 'rocket')
  })

  it('幂等：归一化结果再归一化不变；且不改入参', () => {
    const input = [ok({ label: ' 甲 ', visibility: '', icon: '' })]
    const once = normalizeCustomPages(input)
    assert.deepEqual(normalizeCustomPages(once), once)
    assert.equal(input[0].label, ' 甲 ', '入参不得被就地修改')
    assert.equal(input[0].visibility, '')
  })
})

describe('validateCustomPages：每个 code + index 与 Go 逐条对齐', () => {
  it('合法形态通过（含上限边界、大写 scheme、query + fragment、停用项）', () => {
    const atLimit = ok({
      id: 'a'.repeat(CUSTOM_PAGE_MAX_ID),
      label: '字'.repeat(CUSTOM_PAGE_MAX_LABEL),
      url: `https://d.example.com/${'a'.repeat(CUSTOM_PAGE_MAX_URL - 'https://d.example.com/'.length)}`,
    })
    assert.equal(bytes(atLimit.id), CUSTOM_PAGE_MAX_ID)
    assert.equal(bytes(atLimit.url), CUSTOM_PAGE_MAX_URL)
    assert.equal([...atLimit.label].length, CUSTOM_PAGE_MAX_LABEL)
    for (const page of [
      ok(),
      atLimit,
      ok({ url: 'http://192.168.1.10:8080/grafana' }),
      ok({ url: 'HTTPS://docs.example.com/' }),
      ok({ url: 'https://d.example.com/p?x=1&y=2#sec-3' }),
      // 合法百分号转义（两位十六进制）必须放行：只有"不是 %XX"才算 urlScheme。
      ok({ url: 'https://d.example.com/%E4%B8%AD' }),
      ok({ visibility: 'admin' }),
      ok({ enabled: false }),
      ok({ label: ' 带空格的标签 ' }), // 校验内部先 trim 再判，与 Go 同序
    ]) {
      assert.equal(validateCustomPages([page]), null, JSON.stringify(page))
    }
  })

  it('每类问题给出 code + 条目下标', () => {
    const cases: Array<[string, CustomPageConfig, string, number]> = [
      ['id 为空', ok({ id: '' }), 'idRequired', 0],
      ['id 超长（33 字节，形状本身合法）', ok({ id: 'a'.repeat(CUSTOM_PAGE_MAX_ID + 1) }), 'idTooLong', 0],
      ['id 大写', ok({ id: 'Docs' }), 'idPattern', 0],
      ['id 前导连字符', ok({ id: '-docs' }), 'idPattern', 0],
      ['id 含空格', ok({ id: 'a b' }), 'idPattern', 0],
      ['label 纯空格', ok({ label: '   ' }), 'labelRequired', 0],
      ['label 41 rune', ok({ label: '字'.repeat(CUSTOM_PAGE_MAX_LABEL + 1) }), 'labelTooLong', 0],
      ['url 为空', ok({ url: '' }), 'urlRequired', 0],
      ['url 超长', ok({ url: `https://d.example.com/${'a'.repeat(CUSTOM_PAGE_MAX_URL)}` }), 'urlTooLong', 0],
      ['javascript:', ok({ url: 'javascript:alert(1)' }), 'urlScheme', 0],
      ['data:', ok({ url: 'data:text/html,<b>x</b>' }), 'urlScheme', 0],
      ['file:', ok({ url: 'file:///etc/passwd' }), 'urlScheme', 0],
      ['相对路径', ok({ url: '/inner' }), 'urlScheme', 0],
      ['userinfo', ok({ url: 'https://u:p@d.example.com/' }), 'urlCredentials', 0],
      // 空 userinfo（§19-4-1）：Go 的 url.User 非 nil → 拒；WHATWG 的 username/password
      // 都是空串，只靠它们判不出来，故按 authority 段是否含 `@` 判。
      ['空 userinfo', ok({ url: 'https://@d.example.com/' }), 'urlCredentials', 0],
      ['空 userinfo 带端口', ok({ url: 'http://@d.example.com:8080/x' }), 'urlCredentials', 0],
      // 非法百分号转义（§19-4-2）：Go 的 url.Parse 报错，WHATWG `new URL` 放行。
      ['非法百分号转义', ok({ url: 'https://d.example.com/%zz' }), 'urlScheme', 0],
      ['截断的百分号转义', ok({ url: 'https://d.example.com/100%' }), 'urlScheme', 0],
      ['控制字符（换行）', ok({ url: 'https://d.example.com/\nx' }), 'urlControlChar', 0],
      ['visibility 未知', ok({ visibility: 'all' }), 'visibilityInvalid', 0],
      ['visibility 空串（校验不补缺省）', ok({ visibility: '' }), 'visibilityInvalid', 0],
      ['icon 未知', ok({ icon: 'rocket' }), 'iconInvalid', 0],
      ['icon 空串（校验不补缺省）', ok({ icon: '' }), 'iconInvalid', 0],
    ]
    for (const [name, page, code, index] of cases) {
      const issue = validateCustomPages([page])
      assert.equal(issue?.code, code, name)
      assert.equal(issue?.index, index, name)
    }
  })

  it('url 判定顺序与 Go 一致：控制字符先于长度（换行不会被"太长"掩盖）', () => {
    const page = ok({ url: `https://d.example.com/${'a'.repeat(CUSTOM_PAGE_MAX_URL)}\nx` })
    assert.equal(codeOf([page]), 'urlControlChar')
  })

  it('重复 id → duplicateId 指向后出现的那条（管理员改它即可）', () => {
    const issue = validateCustomPages([ok(), ok({ label: '另一条' })])
    assert.equal(issue?.code, 'duplicateId')
    assert.equal(issue?.index, 1)
    assert.equal(issue?.field, 'id')
  })

  it('超量 → tooMany 是列表级问题：index = -1、field = items', () => {
    const many = Array.from({ length: CUSTOM_PAGE_MAX_ITEMS + 1 }, (_, i) => ok({ id: `p${i}` }))
    const issue = validateCustomPages(many)
    assert.equal(issue?.code, 'tooMany')
    assert.equal(issue?.index, -1)
    assert.equal(issue?.field, 'items')
    assert.equal(codeOf(many.slice(0, CUSTOM_PAGE_MAX_ITEMS)), null, '恰好 20 条必须通过')
  })

  it('V9 序列化总字节超限 → tooLarge 也是列表级（index = -1、field = items）', () => {
    // 单字段都在上限内，但 JSON 转义会把 `\` 翻倍：20 条 × ~2 KiB 的路径 → ~80 KiB。
    // 这正是 V9 存在的理由（字段级上限够不到总量，但转义放大能到）。
    const path = `https://d.example.com/${'\\'.repeat(2000)}`
    const pages = Array.from({ length: CUSTOM_PAGE_MAX_ITEMS }, (_, i) => ok({ id: `p${i}`, url: path }))
    assert.ok(
      bytes(serializeCustomPages(pages)) > CUSTOM_PAGE_MAX_TOTAL,
      '前置条件：这组输入序列化后必须确实超过总上限（否则断言在测别的东西）',
    )
    const issue = validateCustomPages(pages)
    assert.equal(issue?.code, 'tooLarge')
    assert.equal(issue?.index, -1)
    assert.equal(issue?.field, 'items')
  })

  it('下标指向真正有问题的那一条（前面的条目合法）', () => {
    const issue = validateCustomPages([ok({ id: 'a' }), ok({ id: 'b' }), ok({ id: 'c', icon: 'nope' })])
    assert.equal(issue?.index, 2)
    assert.equal(issue?.code, 'iconInvalid')
  })
})

describe('serializeCustomPages：键序固定 + 字节幂等', () => {
  it('逐字节规范形（键序 id,label,url,visibility,icon,enabled，与 Go 同形）', () => {
    assert.equal(
      serializeCustomPages([ok()]),
      '[{"id":"docs","label":"文档","url":"https://docs.example.com/guide","visibility":"user","icon":"link","enabled":true}]',
    )
  })

  it('空列表 → "[]"（不是 "" 也不是 "null"）', () => {
    assert.equal(serializeCustomPages([]), '[]')
  })

  it('往返幂等：serialize → parse → normalize → serialize 字节不变', () => {
    const pages = normalizeCustomPages([
      ok({ id: 'docs' }),
      ok({ id: 'ops', label: '工单', visibility: 'admin', icon: 'ticket', enabled: false }),
    ])
    const once = serializeCustomPages(pages)
    const twice = serializeCustomPages(normalizeCustomPages(tryParseCustomPages(once).pages))
    assert.equal(twice, once)
  })

  it('不转义 < > &（Go 侧 SetEscapeHTML(false)，否则库内字节与设置页所见不一致）', () => {
    const json = serializeCustomPages([ok({ url: 'https://d.example.com/p?a=1&b=2<c' })])
    assert.match(json, /a=1&b=2<c/)
    assert.doesNotMatch(json, /\\u0026|\\u003c/)
  })
})

describe('pagesForConsole：过滤但保序、不改入参', () => {
  const pages = [
    { ID: 'z', Visibility: 'user', Enabled: true },
    { ID: 'a', Visibility: 'user', Enabled: true },
    { ID: 'm', Visibility: 'admin', Enabled: true },
    { ID: 'off', Visibility: 'user', Enabled: false },
  ]

  it('用户台：只有 visibility=user 且启用的项，顺序 = 配置顺序', () => {
    assert.deepEqual(pagesForConsole(pages, 'user').map(p => p.ID), ['z', 'a'])
  })

  it('管理台：只有 visibility=admin 的项', () => {
    assert.deepEqual(pagesForConsole(pages, 'admin').map(p => p.ID), ['m'])
  })

  it('Enabled 缺省视为启用（服务端不下发该字段，D10）', () => {
    assert.deepEqual(pagesForConsole([{ ID: 'x', Visibility: 'user' }], 'user').map(p => p.ID), ['x'])
  })

  it('不改入参（不就地 sort/filter：缓存数组的顺序必须原样）', () => {
    const before = pages.map(p => p.ID)
    const picked = pagesForConsole(pages, 'user')
    assert.notEqual(picked, pages, '必须返回新数组')
    assert.deepEqual(pages.map(p => p.ID), before)
  })
})

describe('buildEmbeddedUrl：只覆盖 7 个保留参数', () => {
  const params = {
    userId: 42,
    token: 'jwt-token',
    theme: 'dark' as const,
    lang: 'zh-CN',
    srcHost: 'https://console.example.com',
    srcUrl: 'https://console.example.com/app/custom/docs',
  }

  it('7 个保留参数齐备且各只出现一次（ui_mode 固定 embedded）', () => {
    const out = new URL(buildEmbeddedUrl('https://target.example.com/path', params))
    assert.equal(out.searchParams.get('user_id'), '42')
    assert.equal(out.searchParams.get('token'), 'jwt-token')
    assert.equal(out.searchParams.get('theme'), 'dark')
    assert.equal(out.searchParams.get('lang'), 'zh-CN')
    assert.equal(out.searchParams.get('ui_mode'), 'embedded')
    assert.equal(out.searchParams.get('src_host'), 'https://console.example.com')
    assert.equal(out.searchParams.get('src_url'), 'https://console.example.com/app/custom/docs')
    assert.equal(EMBEDDED_RESERVED_PARAMS.length, 7)
    for (const key of EMBEDDED_RESERVED_PARAMS) {
      assert.equal(out.searchParams.getAll(key).length, 1, `${key} 不得重复`)
    }
  })

  it('第三方 query 原样保留，同名保留参数被覆盖', () => {
    const out = new URL(
      buildEmbeddedUrl('https://t.example.com/p?token=old&theme=light&foo=1&bar=2#frag', params),
    )
    assert.equal(out.searchParams.get('token'), 'jwt-token', '保留参数必须被覆盖')
    assert.equal(out.searchParams.get('theme'), 'dark')
    assert.equal(out.searchParams.get('foo'), '1', '第三方参数不得被抹掉')
    assert.equal(out.searchParams.get('bar'), '2')
  })

  it('base 里重复的同名保留参数折叠为一条，第三方重复参数原样保留', () => {
    const out = new URL(buildEmbeddedUrl('https://t.example.com/p?token=a&token=b&foo=1&foo=2', params))
    assert.equal(out.searchParams.getAll('token').length, 1)
    assert.equal(out.searchParams.get('token'), 'jwt-token')
    assert.deepEqual(out.searchParams.getAll('foo'), ['1', '2'])
  })

  it('没有 token / user_id 时删除 base 里的旧值（不给第三方留陈旧凭据）', () => {
    const out = new URL(
      buildEmbeddedUrl('https://t.example.com/p?token=old&user_id=7&foo=1', {
        ...params,
        token: null,
        userId: null,
      }),
    )
    assert.equal(out.searchParams.get('token'), null)
    assert.equal(out.searchParams.get('user_id'), null)
    assert.equal(out.searchParams.get('foo'), '1', '第三方参数不受影响')
  })

  it('fragment 保留且参数插在它前面', () => {
    const out = buildEmbeddedUrl('https://t.example.com/p#sec', params)
    assert.match(out, /^https:\/\/t\.example\.com\/p\?/)
    assert.ok(out.endsWith('#sec'), out)
  })

  it('token 含 + / = 时按 URL 编码往返无损', () => {
    const token = 'a+b/c=='
    const out = new URL(buildEmbeddedUrl('https://t.example.com/p', { ...params, token }))
    assert.equal(out.searchParams.get('token'), token)
  })

  it('非法或非 http(s) 的 base 原样返回（拼装层不改写配置）', () => {
    for (const base of ['', 'not a url', 'javascript:alert(1)', 'file:///etc/passwd', '/relative']) {
      assert.equal(buildEmbeddedUrl(base, params), base)
    }
  })
})

describe('图标映射与常量（跨语言门禁的运行时那半）', () => {
  it('CUSTOM_PAGE_ICONS 非空、无重复、顺序与 Go 逐位一致', () => {
    assert.ok(CUSTOM_PAGE_ICONS.length > 0)
    assert.equal(new Set(CUSTOM_PAGE_ICONS).size, CUSTOM_PAGE_ICONS.length)
    assert.deepEqual([...CUSTOM_PAGE_ICONS], ['link', 'globe', 'book', 'chart', 'lifebuoy', 'ticket', 'code', 'sparkles'])
  })

  it('CUSTOM_PAGE_VISIBILITIES 与 Go 常量集合一致', () => {
    assert.deepEqual([...CUSTOM_PAGE_VISIBILITIES], ['admin', 'user'])
  })

  it('上限常量与 Go 同值（含 V9 总量）', () => {
    assert.deepEqual(
      [CUSTOM_PAGE_MAX_ITEMS, CUSTOM_PAGE_MAX_LABEL, CUSTOM_PAGE_MAX_URL, CUSTOM_PAGE_MAX_ID, CUSTOM_PAGE_MAX_TOTAL],
      [20, 40, 2048, 32, 64 << 10],
    )
  })

  it('导出 id 正则与 scheme 白名单（Go 门禁读这两个字面量做等号比对）', () => {
    assert.equal(CUSTOM_PAGE_ID_PATTERN.source, '^[a-z0-9][a-z0-9-]*$')
    assert.deepEqual([...CUSTOM_PAGE_URL_SCHEMES], ['http://', 'https://'])
  })

  it('CUSTOM_PAGE_ICON_COMPONENTS 键集与清单完全一致（不多不少）', () => {
    assert.deepEqual(Object.keys(CUSTOM_PAGE_ICON_COMPONENTS).sort(), [...CUSTOM_PAGE_ICONS].sort())
  })

  it('已知 key 命中自身，未知 key 回退 link（不抛错、不留空白）', () => {
    assert.equal(iconForCustomPage('ticket'), CUSTOM_PAGE_ICON_COMPONENTS.ticket)
    assert.equal(iconForCustomPage('future-icon'), CUSTOM_PAGE_ICON_COMPONENTS.link)
    assert.equal(iconForCustomPage(''), CUSTOM_PAGE_ICON_COMPONENTS.link)
  })
})

describe('resolveTheme：读 <html> 上的 dark 类', () => {
  const stubDocument = (dark: boolean) => {
    Object.defineProperty(globalThis, 'document', {
      value: { documentElement: { classList: { contains: (cls: string) => dark && cls === 'dark' } } },
      configurable: true,
    })
  }

  it('dark 类在则 dark，否则 light', () => {
    stubDocument(true)
    assert.equal(resolveTheme(), 'dark')
    stubDocument(false)
    assert.equal(resolveTheme(), 'light')
  })

  it('没有 document 时返回 light（本文件被 node --test 直接 import）', () => {
    Reflect.deleteProperty(globalThis, 'document')
    assert.equal(resolveTheme(), 'light')
  })
})

// 动态 key（`settings.customPages.issue.<code>`、`icons.<key>`）在类型系统里是透明的，
// 少一条文案不会编译失败——只会在界面上显示成原始 key。这里按 §10 的键清单钉住。
describe('§10 i18n：动态 key 的文案在 zh/en 都要存在', () => {
  // 与 CustomPageIssueCode 同集合（每个码值都由上一个 describe 的用例实际产出过）。
  const ISSUE_CODES = [
    'tooMany',
    'tooLarge',
    'idRequired',
    'idTooLong',
    'idPattern',
    'duplicateId',
    'labelRequired',
    'labelTooLong',
    'urlRequired',
    'urlTooLong',
    'urlScheme',
    'urlCredentials',
    'urlControlChar',
    'visibilityInvalid',
    'iconInvalid',
  ]
  // B3 的 frame / 路由页消费，本批先落 key（§10）。
  const VIEW_KEYS = ['openInNewTab', 'blockedHint', 'dismissHint', 'invalidUrl', 'notFoundTitle', 'notFoundDesc', 'backHome']

  it('issue 码值、图标名与内嵌页文案键在两侧同名同存', () => {
    for (const file of ['../locales/zh.ts', '../locales/en.ts']) {
      const src = read(file)
      for (const code of ISSUE_CODES) assert.ok(src.includes(`"${code}":`), `${file} 缺少 issue.${code}`)
      for (const icon of CUSTOM_PAGE_ICONS) assert.ok(src.includes(`"${icon}":`), `${file} 缺少 icons.${icon}`)
      for (const key of VIEW_KEYS) assert.ok(src.includes(`"${key}":`), `${file} 缺少 customPage.${key}`)
    }
  })
})

// —— §11-T9 的设置页段（源码形状断言：本仓没有组件测试设施，能自动钉住的只有"代码里写了"）——
const read = (rel: string): string => readFileSync(fileURLToPath(new URL(rel, import.meta.url)), 'utf8')

/**
 * stripComments 去掉 JS 注释、**保留字符串与模板字面量内容**。
 *
 * 为什么断言前必须剥注释（§19-5）：M7 那类变异曾在注释里留下 `{t(key)}` 字面量，
 * 于是"不得出现"的断言被一句注释喂成了假绿——注释不是代码，却能让形状断言失效。
 * 反过来也不能粗暴地按 `//` 切：URL 字符串里的 `https://` 会被当成行注释的开头，
 * 把真正的代码整段吞掉，断言又变成反向假绿。所以这里是一个最小的扫描器：只在
 * "不在字符串里"的位置认 `//` 与 `/* *\/`。
 */
const stripComments = (src: string): string => {
  let out = ''
  let quote: '"' | "'" | '`' | null = null
  for (let i = 0; i < src.length; i++) {
    const ch = src[i]
    const next = src[i + 1]
    if (quote) {
      out += ch
      if (ch === '\\') {
        out += next ?? ''
        i++
        continue
      }
      if (ch === quote) quote = null
      continue
    }
    if (ch === '"' || ch === "'" || ch === '`') {
      quote = ch
      out += ch
      continue
    }
    if (ch === '/' && next === '/') {
      while (i < src.length && src[i] !== '\n') i++
      out += '\n'
      continue
    }
    if (ch === '/' && next === '*') {
      i += 2
      while (i < src.length && !(src[i] === '*' && src[i + 1] === '/')) i++
      i++
      continue
    }
    out += ch
  }
  return out
}

describe('设置页把 custom_pages 归入专用 tab（§11-T9）', () => {
  it('GROUPS 含 customPages 分组且声明 custom_pages（否则会落进「其他」被单行 Input 编辑）', () => {
    const src = read('../pages/settings.tsx')
    assert.match(src, /id: 'customPages', keys: \['custom_pages'\]/, 'GROUPS 缺少 { id: \'customPages\', keys: [\'custom_pages\'] }')
    // 该分组必须由 GROUPS 派生出的 GROUPED_KEYS 自动排除，不得另写第二份排除表。
    assert.match(src, /const GROUPED_KEYS = new Set<string>\(GROUPS\.flatMap/)
  })
})

// —— §11-T9 的 frame / 路由页 / 导航段 ——
// 同一套「源码形状」口径：本仓没有组件测试设施，能自动钉住的只有"代码里写了什么"。
// 它证明不了运行期行为（例如 X-Frame-Options 拒绝时 iframe 到底渲染成什么），只保证
// 安全属性与导航取标签的写法没有被后续改动悄悄删掉。
describe('内嵌页组件源码形状（§11-T9）', () => {
  it('frame 逐字保留三条安全属性，且 frame / 路由页都不含 dangerouslySetInnerHTML', () => {
    const frame = read('../components/custom-page-frame.tsx')
    assert.ok(
      frame.includes('referrerPolicy="no-referrer"'),
      'frame 必须设置 referrerPolicy="no-referrer"（§8-S1②：否则含 token 的 URL 会经 Referer 传给第三个域）',
    )
    assert.ok(frame.includes('rel="noopener noreferrer"'), '「在新窗口打开」锚点必须带 rel="noopener noreferrer"')
    assert.ok(frame.includes('allow="fullscreen"'), 'iframe 的能力只开 fullscreen（§8-S4）')
    for (const file of ['../components/custom-page-frame.tsx', '../pages/custom-page.tsx']) {
      assert.ok(!read(file).includes('dangerouslySetInnerHTML'), `${file} 不得使用 dangerouslySetInnerHTML（§8-S4）`)
    }
  })

  it('侧栏与移动端下拉都走 navLabel，且渲染处不再把成员访问的 key 直接交给 t()', () => {
    // 负向正则：任何 `t(x.key)` 都是"把某个对象的 key 当 i18n key"。唯一合法的出现是
    // §7.1 的 navLabel 本体（`'text' in item ? item.text : t(item.key)`——判别联合收窄
    // 之后才允许取 key）。因此按"出现次数"判：定义了 navLabel 的文件允许 1 处，
    // 其他文件 0 处。这样既覆盖 `{t(item.key)}` 这类成员访问写法，又不会把正确实现判红。
    for (const file of ['../components/app-sidebar.tsx', '../components/app-shell.tsx']) {
      const src = read(file)
      assert.ok(src.includes('navLabel('), `${file} 必须用 navLabel 取标签（自定义项的 label 不是 i18n key）`)
      // 先剥注释：注释里的同形字面量不是代码，M7 曾因此让窗口变成假绿（§19-5）。
      const code = stripComments(src)
      const hits = code.match(/t\([A-Za-z_$][\w$]*\.key/g) ?? []
      const allowed = code.includes('export const navLabel') ? 1 : 0
      assert.equal(
        hits.length,
        allowed,
        `${file} 只允许 navLabel 本体出现 t(x.key)，实际 ${hits.length} 处（不得把用户 label 直接交给 t()）`,
      )
    }
  })

  it('内嵌页 query key 单点定义在 lib/custom-pages.ts，三个消费者全部 import', () => {
    // §19-6：此前两份字面量 + components/ 反向依赖 pages/，"改一处漏一处"会让保存后
    // 侧栏不刷新且测试全绿。这里钉住：定义只有一处，消费者只 import。
    assert.match(read('../lib/custom-pages.ts'), /export const CUSTOM_PAGES_QUERY_KEY = \['user', 'pages'\] as const/)
    for (const file of ['../pages/custom-page.tsx', '../components/app-shell.tsx', '../components/custom-pages-editor.tsx']) {
      const src = read(file)
      assert.match(src, /import \{[^}]*CUSTOM_PAGES_QUERY_KEY[^}]*\} from '@\/lib\/custom-pages'/s,
        `${file} 必须从 @/lib/custom-pages import query key（不得本地另写一份）`)
      assert.ok(!src.includes("= ['user', 'pages'] as const"),
        `${file} 不得再本地定义 query key 字面量（单点定义在 lib/custom-pages.ts）`)
    }
    assert.ok(!read('../components/app-shell.tsx').includes("from '@/pages/custom-page'"),
      'components/ 不得反向依赖 pages/（query key 已移到 lib/）')
  })
})
