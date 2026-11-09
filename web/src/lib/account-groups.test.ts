// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 账号分组的「没表态」语义（回归：编辑对话框里分组被清空）。
//
// 分组回显不随账号列表返回，对话框打开后才拉 GET /accounts/{id}/groups。
// 一旦把「回显还没到」当成「用户清空」，PATCH 里的 `group_ids: []` 就会覆盖
// 账号已有分组——线上表现是「点开编辑、没看到分组、一保存分组就没了」。
// 现在回显进**基线**（视图 = 基线 ⊕ 稀疏草稿）：草稿里没有 group_ids 键 = 没表态，
// 写入侧（draftToPatch）只把草稿里出现的键变成请求体，基线永远不参与「写」。
import { describe, it } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'

// 分组投影的唯一实现是 account-draft.ts 的 draftToPatch（逐字段三态表覆盖）：
// 曾经的 buildGroupIdsField 已删除——两份实现里只有一份在生产路径上，是这次事故的同款形状。

// 源码形状：回显只读不拷贝。React Query 的 structural sharing 会在内容不变时
// 复用同一引用，于是「把回显灌进表单的 effect」在二次打开时永远不跑——
// 线上表现是「分组取到了但一个都没勾上」。这类回归没有渲染测试能接住，只能钉形状。
const page = readFileSync(new URL('../pages/accounts.tsx', import.meta.url), 'utf8')

describe('accounts.tsx：回显不许被 structural sharing 吃掉', () => {
  it('回显进基线（fromAccountView(editing, 回显)），展示派生自 view', () => {
    assert.match(page, /fromAccountView\(editing, groupsEcho\.data\?\.group_ids \?\? \[\]\)/)
    assert.match(page, /value=\{view\.group_ids\}/)
  })

  it('不再把回显回填进草稿', () => {
    assert.equal(/group_ids:\s*\[\.\.\.\(?groupsEcho\.data/.test(page), false)
  })

  it('写入走 update(\'group_ids\')；投影（缺席 = 不发键，[] = 清空）由三态表覆盖', () => {
    assert.match(page, /\bupdate\('group_ids',\s*v\)/)
  })
})
