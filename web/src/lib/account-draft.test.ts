// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 账号表单的三态模型（absent / value / clear）回归。
//
// PATCH 契约按字段类别定义状态机：缺席 = 不变、null = 清空、有值 = 落值（集合：[] = 清空）。
// 旧表单用「快照 + 与回显比较」表达「没表态」，于是「还没加载出来」被当成「用户清空」——
// c4b3e711（分组）与 0de4d56f（授予）是同一件事的两次事故。这里把投影钉成表格：
// 每个字段 × {缺席, 有值, 清空} 各一条断言，漏接线由 FIELD_KINDS 的键集合对齐兜底。
import { describe, it } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import {
  FIELD_KINDS,
  assertCreate,
  draftToPatch,
  defaults,
  fromAccountView,
  mergeView,
  withField,
  withoutField,
} from './account-draft.ts'

const ctx = { tpl: 'nonCodex', echoProxyId: null, isSupplierScope: false }

// 逐字段三态：[字段, 有值草稿, patch 期望值, 清空草稿, 清空期望]。
// clear.want = 'absent'（非法瞬时值，投影不发）/ 'throws'（投影拒绝）/ 具体值。
const rows = [
  { key: 'name', draft: { name: '  acc-1  ' }, want: 'acc-1', clear: { draft: { name: '' }, want: 'absent' } },
  { key: 'template_id', draft: { template_id: '7' }, want: 7, clear: { draft: { template_id: '' }, want: 'absent' } },
  { key: 'base_url', draft: { base_url: ' https://up.example ' }, want: 'https://up.example', clear: { draft: { base_url: null }, want: null } },
  { key: 'upstream_key', draft: { upstream_key: 'sk-x' }, want: 'sk-x', clear: { draft: { upstream_key: '' }, want: 'absent' } },
  { key: 'max_concurrency', draft: { max_concurrency: '5' }, want: 5, clear: { draft: { max_concurrency: '' }, want: 'absent' } },
  { key: 'group_ids', draft: { group_ids: [3, 4] }, want: [3, 4], clear: { draft: { group_ids: [] }, want: [] } },
  { key: 'cost_multiplier', patchKey: 'upstream_cost_multiplier', draft: { cost_multiplier: '2.5' }, want: 2.5, clear: { draft: { cost_multiplier: '' }, want: 'throws' } },
  { key: 'cache_domain', draft: { cache_domain: ' a.example.com ' }, want: 'a.example.com', clear: { draft: { cache_domain: null }, want: null } },
  { key: 'supplier_user_id', draft: { supplier_user_id: 9 }, want: 9, clear: { draft: { supplier_user_id: null }, want: null } },
  // proxy 两种取值都看回显：绑定 = 有值，直连 = 解绑可见的那条（回显 null 时仍不发）
  { key: 'proxy', patchKey: 'proxy_id', draft: { proxy: { kind: 'bind', id: 7 } }, want: 7, ctx: { ...ctx, echoProxyId: 3 }, clear: { draft: { proxy: { kind: 'direct' } }, want: null, ctx: { ...ctx, echoProxyId: 5 } } },
]

describe('draftToPatch：逐字段三态', () => {
  it('草稿为空 ⇒ 空补丁（一个键都不带）', () => {
    assert.deepEqual(draftToPatch({}, ctx), {})
  })

  it('表格覆盖 FIELD_KINDS 的每个字段（加字段必须补用例）', () => {
    assert.deepEqual(rows.map(r => r.key).sort(), Object.keys(FIELD_KINDS).sort())
  })

  for (const row of rows) {
    const patchKey = row.patchKey ?? row.key
    it(`${row.key}：缺席 ⇒ 不发键`, () => {
      assert.equal(Object.hasOwn(draftToPatch({}, row.ctx ?? ctx), patchKey), false)
    })
    it(`${row.key}：有值 ⇒ 发 ${JSON.stringify(row.want)}`, () => {
      const patch = draftToPatch(row.draft, row.ctx ?? ctx)
      assert.equal(Object.hasOwn(patch, patchKey), true)
      assert.deepEqual(patch[patchKey], row.want)
    })
    it(`${row.key}：${row.clear.want === 'throws' ? '非法空串 ⇒ 投影拒绝' : `清空 ⇒ ${JSON.stringify(row.clear.want)}`}`, () => {
      const run = () => draftToPatch(row.clear.draft, row.clear.ctx ?? ctx)
      if (row.clear.want === 'throws') {
        assert.throws(run)
        return
      }
      if (row.clear.want === 'absent') {
        assert.equal(Object.hasOwn(run(), patchKey), false)
        return
      }
      assert.deepEqual(run()[patchKey], row.clear.want)
    })
  }

  it('enabled（批量专属，单条无控件）单独一条', () => {
    assert.equal(Object.hasOwn(draftToPatch({}, ctx), 'enabled'), false)
    assert.deepEqual(draftToPatch({ enabled: false }, ctx), { enabled: false })
  })
})

describe('draftToPatch：codex / 模板上下文四分', () => {
  it("tpl: 'codex' ⇒ 恒发 base_url: null（模板可能随本次 PATCH 切换）", () => {
    assert.deepEqual(draftToPatch({ name: 'a' }, { ...ctx, tpl: 'codex' }), { name: 'a', base_url: null })
  })

  it("tpl: 'codex' ⇒ 草稿里的 base_url 被 null 覆盖（不是跳过）", () => {
    assert.deepEqual(draftToPatch({ base_url: 'https://x.example' }, { ...ctx, tpl: 'codex' }), { base_url: null })
  })

  it("tpl: 'nonCodex' ⇒ 按草稿：缺席不发 / null 清空 / 有值 trim", () => {
    assert.deepEqual(draftToPatch({}, { ...ctx, tpl: 'nonCodex' }), {})
    assert.deepEqual(draftToPatch({ base_url: null }, { ...ctx, tpl: 'nonCodex' }), { base_url: null })
    assert.deepEqual(draftToPatch({ base_url: ' a.example/v1 ' }, { ...ctx, tpl: 'nonCodex' }), { base_url: 'a.example/v1' })
  })

  // 批量混合/未解析 ⇒ 不发 BaseURL：宁可拒绝也不误清（非 codex 行不能被写 null）
  for (const tpl of ['mixed', 'unresolved']) {
    it(`tpl: '${tpl}' ⇒ 不含 base_url`, () => {
      assert.equal(Object.hasOwn(draftToPatch({ base_url: 'https://x.example' }, { ...ctx, tpl }), 'base_url'), false)
    })
  }
})

describe('draftToPatch：作用域与代理', () => {
  it('供应商作用域 ⇒ 永不编码 supplier_user_id（管理面照常分配）', () => {
    assert.equal(Object.hasOwn(draftToPatch({ supplier_user_id: 9 }, { ...ctx, isSupplierScope: true }), 'supplier_user_id'), false)
  })

  it('回显 null + 直连 ⇒ 不出现 proxy_id（看不见的绑定不得被覆盖成 null）', () => {
    assert.deepEqual(draftToPatch({ proxy: { kind: 'direct' } }, { ...ctx, echoProxyId: null }), {})
  })

  it('绑定 ⇒ proxy_id = id', () => {
    assert.deepEqual(draftToPatch({ proxy: { kind: 'bind', id: 7 } }, { ...ctx, echoProxyId: null }), { proxy_id: 7 })
  })
})

describe('assertCreate：创建必填', () => {
  it('空草稿 ⇒ 抛错（name/template_id 必填）', () => {
    assert.throws(() => assertCreate({}))
  })

  it('只有空白 name ⇒ 抛错', () => {
    assert.throws(() => assertCreate({ name: '   ', template_id: '1' }))
  })

  it('name + template_id ⇒ 通过，且投影只含这两键之外的显式字段', () => {
    const draft = { name: 'a', template_id: '1' }
    assert.doesNotThrow(() => assertCreate(draft))
    assert.deepEqual(draftToPatch(draft, ctx), { name: 'a', template_id: 1 })
  })
})

describe('withField / withoutField：纯写入原语', () => {
  it('withField 不改原对象', () => {
    const before = { name: 'a' }
    const after = withField(before, 'template_id', '2')
    assert.deepEqual(before, { name: 'a' })
    assert.deepEqual(after, { name: 'a', template_id: '2' })
  })

  it('withField 接受 null（清空编码能过类型，undefined 不能）', () => {
    assert.deepEqual(withField({}, 'base_url', null), { base_url: null })
  })

  it('withoutField 真删键（不是置 undefined）', () => {
    const after = withoutField({ name: 'a', base_url: null }, 'base_url')
    assert.equal(Object.hasOwn(after, 'base_url'), false)
    assert.deepEqual(after, { name: 'a' })
  })

  it('withoutField 不改原对象', () => {
    const before = { name: 'a', cache_domain: null }
    withoutField(before, 'cache_domain')
    assert.deepEqual(before, { name: 'a', cache_domain: null })
  })
})

describe('mergeView：基线 ⊕ 草稿', () => {
  it('undefined 不覆盖基线（洞不算表态）', () => {
    const base = defaults()
    assert.deepEqual(mergeView(base, { name: undefined }), base)
  })

  it('null 覆盖基线（展示层据此 ?? \'\'）', () => {
    const base = { ...defaults(), base_url: 'https://x.example', cache_domain: 'a.example' }
    const view = mergeView(base, { base_url: null, cache_domain: null })
    assert.equal(view.base_url, null)
    assert.equal(view.cache_domain, null)
  })

  it('不改基线对象', () => {
    const base = defaults()
    mergeView(base, { name: 'b' })
    assert.equal(base.name, '')
  })
})

describe('fromAccountView：回显 → 基线', () => {
  const view = {
    ID: 1,
    Name: 'acc-1',
    TemplateID: 2,
    Template: { CredentialType: 'api_key' },
    BaseURL: 'https://up.example',
    UpstreamKey: 'sk-x',
    MaxConcurrency: 4,
    UpstreamCostMultiplier: 2.5,
    CacheDomain: 'a.example',
    supplier_user_id: 7,
    proxy_id: 3,
  }

  it('可空字段保留契约的 null（不是 \'\'）；分组来自回显参数', () => {
    const base = fromAccountView({ ID: 1, Name: 'x', TemplateID: 2 }, [5, 6])
    assert.equal(base.base_url, null)
    assert.equal(base.cache_domain, null)
    assert.equal(base.supplier_user_id, null)
    assert.deepEqual(base.group_ids, [5, 6])
  })

  it('有值字段照抄回显；proxy 由 proxy_id 派生', () => {
    const base = fromAccountView(view, [5])
    assert.equal(base.name, 'acc-1')
    assert.equal(base.template_id, '2')
    assert.equal(base.base_url, 'https://up.example')
    assert.equal(base.upstream_key, 'sk-x')
    assert.equal(base.max_concurrency, '4')
    assert.equal(base.cost_multiplier, '2.5')
    assert.equal(base.cache_domain, 'a.example')
    assert.equal(base.supplier_user_id, 7)
    assert.deepEqual(base.proxy, { kind: 'bind', id: 3 })
  })

  it('codex 模板的 base_url 恒为 null（SDK 默认端点，非空禁用）', () => {
    const base = fromAccountView({ ...view, Template: { CredentialType: 'codex-oauth' } }, [])
    assert.equal(base.base_url, null)
  })

  it('不改调用方的分组数组（拷贝语义）', () => {
    const ids = [5]
    const base = fromAccountView(view, ids)
    ids.push(6)
    assert.deepEqual(base.group_ids, [5])
  })
})

// —— 源码形状（剥注释后断言）——
// 这些接线错误没有渲染测试能接住（本仓无 jsdom），只能钉源码：
// 「忘记接某个字段」「又长出第二个写入入口」「openEdit 忘了重置草稿」都会静默写坏数据。
function stripComments(src: string): string {
  let out = ''
  let i = 0
  while (i < src.length) {
    const c = src[i]
    const c2 = src[i + 1]
    if (c === '/' && c2 === '/') {
      while (i < src.length && src[i] !== '\n') i++
      continue
    }
    if (c === '/' && c2 === '*') {
      i += 2
      while (i < src.length && !(src[i] === '*' && src[i + 1] === '/')) i++
      i += 2
      continue
    }
    if (c === '"' || c === "'" || c === '`') {
      const q = c
      out += c
      i++
      while (i < src.length) {
        if (src[i] === '\\') {
          out += src[i] + (src[i + 1] ?? '')
          i += 2
          continue
        }
        out += src[i]
        if (src[i] === q) {
          i++
          break
        }
        i++
      }
      continue
    }
    out += c
    i++
  }
  return out
}

// 取 `const <name> = … => {` 的函数体（花括号配平）。注释已剥除，字符串里的括号成对。
function fnBody(src: string, decl: string): string {
  const at = src.indexOf(decl)
  assert.notEqual(at, -1, `找不到 ${decl}`)
  const start = src.indexOf('{', at)
  let depth = 0
  for (let i = start; i < src.length; i++) {
    if (src[i] === '{') depth++
    else if (src[i] === '}') {
      depth--
      if (depth === 0) return src.slice(start + 1, i)
    }
  }
  return ''
}

const page = stripComments(readFileSync(new URL('../pages/accounts.tsx', import.meta.url), 'utf8'))

describe('accounts.tsx：接线形状', () => {
  it('每个字段都有 update(\'<key>\', …) 写入点（enabled 不在页内）', () => {
    for (const key of Object.keys(FIELD_KINDS)) {
      if (key === 'enabled') continue
      assert.match(page, new RegExp(`\\bupdate\\s*\\(\\s*'${key}'\\s*,`), `${key} 没有写入点`)
    }
  })

  it('禁止变量键调用（键必须字面量，否则漏字段查不出来）', () => {
    assert.equal(/\bupdate\s*\(\s*(?!')/.test(page), false)
  })

  it('setDraft( 只有 3 处（update/clear/reset 三个封装）', () => {
    assert.equal(page.match(/setDraft\(/g)?.length, 3)
  })

  it('openCreate / openEdit 各自 reset() 一次（否则上一个账号的增量串到下一个）', () => {
    assert.equal(fnBody(page, 'const openCreate').match(/reset\(\)/g)?.length, 1)
    assert.equal(fnBody(page, 'const openEdit').match(/reset\(\)/g)?.length, 1)
  })

  it('旧的快照模型符号归零（form. / setForm( / clearOwner）', () => {
    assert.equal(/\bform\./.test(page), false)
    assert.equal(/setForm\(/.test(page), false)
    assert.equal(/clearOwner/.test(page), false)
  })

  it('投影只有 draftToPatch（创建与编辑共用）', () => {
    assert.match(page, /\bdraftToPatch\(draft,/)
    assert.equal(/toBody\(/.test(page), false)
  })

  it('分组回显进基线、不回填草稿；写入经 update(\'group_ids\')', () => {
    assert.match(page, /fromAccountView\(editing, groupsEcho\.data\?\.group_ids \?\? \[\]\)/)
    assert.equal(/group_ids:\s*\[\.\.\.\(?groupsEcho\.data/.test(page), false)
    assert.match(page, /\bupdate\('group_ids',\s*v\)/)
  })

  it('分组控件保留回显门禁（回显未到 ⇒ 禁用，防只勾几个就覆盖全部授予）', () => {
    assert.match(page, /<GroupMultiSelect\b[\s\S]{0,300}?disabled=\{!!editing && !groupsLoaded\}/)
  })

  // 批量面（Part B）仍持有自己的 clear* 布尔与本表单无关；清空 max_concurrency 是非法瞬时值。
  it('最大并发清空的判据读草稿（不在视图上做比较）', () => {
    assert.match(page, /draft\.max_concurrency === ''/)
  })
})
