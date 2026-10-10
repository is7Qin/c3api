// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 翻译资源哨兵：动态拼接 / 运行时 key（静态抽取器不可见）无法被 `check:i18n` 漂移门禁保护。
// 本用例守住两类回归：① en/zh 叶子键集必须一致；② 关键动态 key 必须存在（防误删）。
import { describe, it } from 'node:test'
import assert from 'node:assert/strict'
import en from './en.ts'
import zh from './zh.ts'

type Dict = Record<string, unknown>

function leafKeys(obj: Dict, prefix = '', out: string[] = []): string[] {
  for (const [k, v] of Object.entries(obj)) {
    const path = prefix ? `${prefix}.${k}` : k
    if (v !== null && typeof v === 'object') leafKeys(v as Dict, path, out)
    else out.push(path)
  }
  return out
}

// 代表性子集：这些 key 由运行时代码拼接 / 传入，抽取器不可见。
const DYNAMIC_SENTINELS = [
  'nav.overview',
  'nav.ops',
  'user.nav.adminSection',
  'user.nav.userSection',
  'ops.stats.dirty',
  'supplierAdmin.act.confirmFailed',
]

describe('locales 资源完整性', () => {
  const enKeys = new Set(leafKeys(en))
  const zhKeys = new Set(leafKeys(zh))

  it('en/zh 叶子键集一致（无单侧漂移）', () => {
    const missingInZh = [...enKeys].filter(k => !zhKeys.has(k))
    const missingInEn = [...zhKeys].filter(k => !enKeys.has(k))
    assert.deepEqual(missingInZh, [], `zh 缺失: ${missingInZh.join(', ')}`)
    assert.deepEqual(missingInEn, [], `en 缺失: ${missingInEn.join(', ')}`)
  })

  it('关键动态 key 存在（防误删）', () => {
    for (const k of DYNAMIC_SENTINELS) {
      assert.ok(enKeys.has(k), `en 缺少动态 key: ${k}`)
      assert.ok(zhKeys.has(k), `zh 缺少动态 key: ${k}`)
    }
  })
})
