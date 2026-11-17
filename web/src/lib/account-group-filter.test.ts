// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 账号页「分组筛选」（spec 2026-11-17 §2）：选项纯函数 + 页面源码 tripwire。
//
// 为什么既有纯函数用例又有源码断言：筛选条件一旦漏进 query key 或漏清 offset，
// 表现是「筛选看着生效、翻页后串味」——没有渲染测试能接住（本项目无 jsdom/RTL），
// 只能在源码形状上钉住三件事：group_id 进了 listAccounts 参数、切筛选清 offset、
// group_id 进了 query key。
import { describe, it } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'

import { GROUP_FILTER_ALL, GROUP_FILTER_UNGROUPED, groupFilterOptions, groupFilterValue } from './account-group-filter.ts'

const page = readFileSync(new URL('../pages/accounts.tsx', import.meta.url), 'utf8')

describe('groupFilterOptions：三态选项构造', () => {
  it('空分组列表 ⇒ 仍有两项（全部 / 未分组），不因空列表隐藏控件', () => {
    const opts = groupFilterOptions([])
    assert.deepEqual(opts.map(o => o.value), [GROUP_FILTER_ALL, GROUP_FILTER_UNGROUPED])
    assert.deepEqual(opts.map(o => o.labelKey), ['accounts.groupFilter.all', 'accounts.groupFilter.ungrouped'])
  })

  it('有分组 ⇒ 顺序与分组列表一致（分组名进 name，不进 locale）', () => {
    const groups = [{ ID: 7, Name: 'alpha' }, { ID: 3, Name: 'beta' }, { ID: 11, Name: 'gamma' }]
    const opts = groupFilterOptions(groups)
    assert.deepEqual(opts.map(o => o.value), ['all', 'ungrouped', '7', '3', '11'])
    assert.deepEqual(opts.slice(2).map(o => o.name), ['alpha', 'beta', 'gamma'])
    assert.equal(opts[2].labelKey, undefined, '分组项不用 i18n 键')
  })

  it('「未分组」恒在「全部」之后、具体分组之前（任何输入长度都成立）', () => {
    for (const groups of [[], [{ ID: 1, Name: 'a' }], [{ ID: 1, Name: 'a' }, { ID: 2, Name: 'b' }]]) {
      const values = groupFilterOptions(groups).map(o => o.value)
      const all = values.indexOf(GROUP_FILTER_ALL)
      const ungrouped = values.indexOf(GROUP_FILTER_UNGROUPED)
      assert.equal(all, 0)
      assert.equal(ungrouped, all + 1)
      for (const v of values.slice(ungrouped + 1)) {
        assert.match(v, /^\d+$/, '具体分组项只能是数字 id 串')
      }
    }
  })
})

describe('groupFilterValue：sentinel ⇄ wire（service 端三态）', () => {
  it('全部 ⇒ undefined（缺席 = 不过滤）、未分组 ⇒ 0、分组 ⇒ 该 id', () => {
    assert.equal(groupFilterValue(GROUP_FILTER_ALL), undefined)
    assert.equal(groupFilterValue(GROUP_FILTER_UNGROUPED), 0)
    assert.equal(groupFilterValue('7'), 7)
  })

  it('未知/非法值按不过滤处理（不把坏 sentinel 变成空列表或负数）', () => {
    assert.equal(groupFilterValue(''), undefined)
    assert.equal(groupFilterValue('nope'), undefined)
    assert.equal(groupFilterValue('-1'), undefined)
    assert.equal(groupFilterValue('1.5'), undefined)
  })
})

describe('accounts.tsx tripwire：筛选必须参与查询与分页', () => {
  it('group_id 进了 listAccounts 参数（不是前端过滤）', () => {
    assert.match(page, /groupFilterValue\(groupIdFilter\)/, '页面用纯函数换算三态')
    // `[^}]*` 限定在**同一个参数对象**内：否则「把 group_id 从 listAccounts 挪到别处」
    // 也能被跨行匹配放过（本文件开头那段说明的同一个坑）。
    assert.match(page, /listAccounts\(\{[^}]*group_id: groupFilterValue\(groupIdFilter\)/)
  })

  it('切筛选清 offset（与搜索/模板筛选同一条纪律）', () => {
    // `[^}]*` 同样限定在**同一个函数体**内：`resetPage()` 必须在这条语句里，
    // 不能靠后面那行 changeEnabled 的 resetPage 顶包。
    assert.match(page, /const changeGroupFilter = \(v: string\) => \{[^}]*resetPage\(\)/)
  })

  it('group_id 进了 query key（筛选条件必须参与缓存键）', () => {
    assert.match(page, /queryKey: \[\s*'accounts',\s*\{[^}]*group_id: groupFilterValue\(groupIdFilter\)/)
  })

  it('选项数据只来自既有 groups 查询（不新增第二次分组请求），i18n 键与纯函数同源', () => {
    assert.match(page, /groupFilterOptions\(groups\)/)
    assert.match(page, /accounts\.groupFilter\.label/)
    // 基线：页面里 `api.listGroups(` 只出现**一次**（`['groups']` 那份 query），
    // 分组筛选复用它的结果。此断言会在「给筛选控件另开一次查询」时变红
    // （旧版本的 `groupIdFilter ... api.listGroups` 反向正则恒为 false，是恒真的）。
    const listGroupsCalls = page.match(/api\.listGroups\(/g) ?? []
    assert.equal(listGroupsCalls.length, 1, '分组筛选不得新增独立的分组查询（复用 groupsQ）')
    assert.match(page, /const groups = groupsQ\.data\?\.rows \?\? \[\]/, '选项派生值仍来自 groupsQ')
  })
})

// toQuery 是「未分组 ⇒ group_id=0」的最后一道闸（spec 2026-11-17 §2）：把 0 当空值
// 吞掉的写法（`if (!v) continue` / `v ? … : ''` / `v || …`）会让「未分组」静默变成
// 「不过滤」——后端收到缺席 ⇒ 返回全量，两态都是 200，只有形状断言能挡。
const client = readFileSync(new URL('./api/client.ts', import.meta.url), 'utf8')

// toQueryBody 只取 toQuery 函数体（避免被 `if (!p) return ''` 这种合法守卫误伤）。
const toQueryBody = (() => {
  const start = client.indexOf('export function toQuery')
  assert.notEqual(start, -1, 'client.ts 必须有 toQuery')
  const end = client.indexOf('\n}', start)
  assert.notEqual(end, -1, 'toQuery 函数体必须可定位')
  return client.slice(start, end)
})()

describe('client.ts toQuery：0 不是空值', () => {
  it('只跳 undefined / null / 空串（0 必须进查询串）', () => {
    assert.match(toQueryBody, /v === undefined \|\| v === null \|\| v === ''/, '必须逐值列举“空值”，不能按真值判断')
  })

  it('不出现任何 falsy 过滤（if (!v) / v ? / v ||）', () => {
    assert.equal(/if\s*\(\s*!\s*v\s*\)/.test(toQueryBody), false, '`if (!v) continue` 会把 group_id=0 吞掉')
    assert.equal(/\bv\s*\?/.test(toQueryBody), false, '三元按真值判断同样吞 0')
    assert.equal(/\bv\s*\|\|/.test(toQueryBody), false, '`v || …` 同样吞 0')
  })
})
