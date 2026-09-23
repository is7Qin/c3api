// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// staleShare 边界单测（spec A27）：Node 内置运行器 `node --test`，零新依赖。
// 五种情形各一用例 + 三个边界例（stale == total → 100.0；真值 0.05% → 0.1 可见；
// 真值 0.04% → 0.0 隐藏）。
import { describe, it } from 'node:test'
import assert from 'node:assert/strict'
import { staleShare } from './stale-share.ts'

describe('staleShare', () => {
  it('total_chains == 0 → 不渲染且无除零', () => {
    assert.equal(staleShare(5, 0), null)
  })
  it('stale_chains == 0 → 不渲染', () => {
    assert.equal(staleShare(0, 100), null)
  })
  it('正常占比 → 一位小数', () => {
    assert.equal(staleShare(123, 1000), '12.3')
  })
  it('真值舍入后为 0.0 → 不渲染', () => {
    assert.equal(staleShare(4, 10000), null)
  })
  it('响应未就绪/字段缺失 → 不渲染（不产 NaN）', () => {
    assert.equal(staleShare(undefined, 100), null)
    assert.equal(staleShare(5, undefined), null)
    assert.equal(staleShare(undefined, undefined), null)
  })
  it('stale == total → 100.0', () => {
    assert.equal(staleShare(7, 7), '100.0')
  })
  it('真值 0.05% → 四舍五入 0.1 → 渲染', () => {
    assert.equal(staleShare(5, 10000), '0.1')
  })
  it('真值 0.04% → 0.0 → 隐藏', () => {
    assert.equal(staleShare(4, 10000), null)
  })
})
