// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 账号分组的「没表态」语义（回归：编辑对话框里分组被清空）。
//
// 分组回显不随账号列表返回，对话框打开后才拉 GET /accounts/{id}/groups。
// 一旦把「回显还没到」当成「用户清空」，PATCH 里的 `group_ids: []` 就会覆盖
// 账号已有分组——线上表现是「点开编辑、没看到分组、一保存分组就没了」。
// 所以草稿用 number[] | null 表达意图，写入侧只负责把 null 变成「不发键」。
import { describe, it } from 'node:test'
import assert from 'node:assert/strict'
import { buildGroupIdsField } from './account-proxy.ts'

describe('buildGroupIdsField：分组三态', () => {
  it('null（没表态）⇒ 不带 group_ids：PATCH 缺席 = 不变', () => {
    assert.deepEqual(buildGroupIdsField(null), {})
  })

  it('[]（明确清空）⇒ 显式空数组：清空是用户意图', () => {
    assert.deepEqual(buildGroupIdsField([]), { group_ids: [] })
  })

  it('有值 ⇒ 替换语义的全量列表', () => {
    assert.deepEqual(buildGroupIdsField([3, 4]), { group_ids: [3, 4] })
  })

  it('不改写调用方数组（拷贝语义）', () => {
    const draft = [7]
    const field = buildGroupIdsField(draft)
    assert.notEqual(field.group_ids, draft)
    draft.push(8)
    assert.deepEqual(field.group_ids, [7])
  })
})
