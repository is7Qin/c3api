// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 冷却判据 + 恢复按钮接线的回归。
//
// 背景：冷却（health.state = open/retry_after）原先只有琥珀徽章、没有人工入口，
// 运维在上游恢复后只能等 Sync 过期或去 Redis 手工删键。现在 recover 按钮对
// 「终态失效」与「冷却中」同时开放；徽章与按钮必须共用 isCooling，否则会出现
// 「有徽章没按钮」的漂移。
import { describe, it } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { isCooling } from './account-health.ts'

describe('isCooling：冷却判据', () => {
  it('open / retry_after ⇒ true（会自愈的两档）', () => {
    assert.equal(isCooling({ state: 'open' }), true)
    assert.equal(isCooling({ state: 'retry_after' }), true)
  })

  it('probing / ready / 缺失 ⇒ false', () => {
    assert.equal(isCooling({ state: 'probing' }), false)
    assert.equal(isCooling({ state: 'ready' }), false)
    assert.equal(isCooling({ state: '' }), false)
    assert.equal(isCooling(undefined), false)
    assert.equal(isCooling(null), false)
  })
})

// 源码形状（本仓无 jsdom）：钉住「徽章与按钮共用判据」和按钮条件本身。
const page = readFileSync(new URL('../pages/accounts.tsx', import.meta.url), 'utf8')

describe('accounts.tsx：冷却账号的恢复入口', () => {
  it('徽章读共享判据（不再内联 state 比较）', () => {
    assert.match(page, /const cooling = isCooling\(a\.health\)/)
    assert.equal(/a\.health\?\.state === 'open'/.test(page), false)
  })

  it('恢复按钮对 failed_at 或冷却同时开放', () => {
    assert.match(page, /\{\(a\.FailedAt \|\| isCooling\(a\.health\)\) && \(/)
  })

  it('旧注释「仅 failed_at 置位时出现」已删除，恢复语义注释覆盖冷却', () => {
    assert.equal(/仅 failed_at 置位时出现/.test(page), false)
    assert.match(page, /冷却的默认[\s\S]{0,40}路径是到期自愈/)
  })
})
