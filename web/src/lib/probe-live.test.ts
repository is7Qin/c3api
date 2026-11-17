// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// probe-live 的规则断言：轮次归属（跨轮不删）、服务端更新后的自愈、第四态不写探测列、
// 以及页面侧超时。全是纯函数，`node --test` 直接跑（无 sleep 计时依赖：超时用极小 ms，
// 且只断言「会不会拒绝」，不测耗时）。
import { describe, it } from 'node:test'
import assert from 'node:assert/strict'
import {
  applyOutcome,
  dropIds,
  pickLive,
  probeView,
  pruneForRound,
  withTimeout,
  type ProbeOutcome,
} from './probe-live.ts'
import type { ProxyView } from './proxy-form.ts'

function row(over: Partial<ProxyView> = {}): ProxyView {
  return {
    id: 1,
    name: 'p',
    scheme: 'http',
    host: 'p.example.com',
    port: 1080,
    has_password: false,
    enabled: true,
    remark: '',
    last_probe_ok: false,
    ...over,
  }
}

const rowOutcome = (at: string, over: Partial<ProxyView> = {}): ProbeOutcome => ({
  kind: 'row',
  row: row({ last_probe_at: at, last_probe_ok: true, last_probe_ip: '203.0.113.7', ...over }),
  at,
})
const failedOutcome = (at: string): ProbeOutcome => ({ kind: 'requestFailed', message: 'boom', at })

describe('probe-live（批量探测的行内即时结果）', () => {
  it('applyOutcome：不可变回填（旧 Map 保持不变）', () => {
    const live = applyOutcome(new Map(), 1, rowOutcome('2026-11-17T10:00:00Z'))
    const next = applyOutcome(live, 2, failedOutcome('2026-11-17T10:00:01Z'))
    assert.equal(live.size, 1, '旧 Map 不得被就地修改')
    assert.deepEqual([...next.keys()], [1, 2])
  })

  it('dropIds：删掉本地结论；没有命中时返回同一引用（不触发渲染）', () => {
    const live = applyOutcome(new Map(), 7, failedOutcome('2026-11-17T10:00:00Z'))
    const after = dropIds(live, [7])
    assert.equal(after.has(7), false)
    assert.equal(live.has(7), true, '入参不可变')
    assert.equal(dropIds(live, [99]), live, '未命中必须原样返回')
  })

  it('★ 跨轮不动：上一轮的收尾不得删掉新一轮已回填的条目（B）', () => {
    const live = applyOutcome(applyOutcome(new Map(), 1, rowOutcome('2026-11-17T10:00:00Z')), 2, failedOutcome('2026-11-17T10:00:01Z'))
    // 上一轮（round=1）还在等刷新时，新一轮（round=2）已经在跑
    assert.equal(pruneForRound(live, 1, 2), live, '轮次不同必须原样返回')
    assert.equal(pruneForRound(live, 2, 2).has(1), false, '同轮才过滤成功结论')
  })

  it('同轮收尾只留 requestFailed（成功结论由刷新后的服务端事实取代）', () => {
    const live = applyOutcome(applyOutcome(new Map(), 1, rowOutcome('2026-11-17T10:00:00Z')), 2, failedOutcome('2026-11-17T10:00:01Z'))
    const pruned = pruneForRound(live, 3, 3)
    assert.equal(pruned.has(1), false)
    assert.equal(pruned.has(2), true, 'requestFailed 服务端没写过，必须留着')
  })

  it('★ 自愈：服务端已有更晚的探测结论 ⇒ 本地结论作废（A）', () => {
    const local = failedOutcome('2026-11-17T10:00:00Z')
    // 刷新后这一行已被服务端重新探测过（last_probe_at 更晚）
    assert.equal(pickLive(row({ last_probe_at: '2026-11-17T10:00:09Z' }), local), undefined)
    // 没有更晚的结论 ⇒ 保留本地
    assert.equal(pickLive(row({ last_probe_at: '2026-11-17T09:59:59Z' }), local), local)
    assert.equal(pickLive(row({ last_probe_at: null }), local), local, '从未探测 ⇒ 本地结论仍然更可信')
    // 时间不可解析时保留（宁可显示刚拿到的结果，也不假装「从未探测」）
    assert.equal(pickLive(row({ last_probe_at: 'not-a-time' }), local), local)
  })

  it('★ requestFailed 不写探测列：合并后仍是列表快照（C）', () => {
    const snapshot = row({ last_probe_at: '2026-11-17T09:00:00Z', last_probe_ok: true, last_probe_ip: '198.51.100.1' })
    const merged = probeView(snapshot, failedOutcome('2026-11-17T10:00:00Z'))
    assert.equal(merged, snapshot, '请求失败不得覆盖列表快照')
    assert.equal(merged.last_probe_ok, true)
    assert.equal(merged.last_probe_ip, '198.51.100.1')
  })

  it('row 结论整行覆盖列表快照（服务端最新事实）', () => {
    const snapshot = row({ last_probe_at: null })
    const fresh = row({ id: 1, last_probe_at: '2026-11-17T10:00:00Z', last_probe_ok: false, last_probe_error: 'connect timeout' })
    const merged = probeView(snapshot, { kind: 'row', row: fresh, at: '2026-11-17T10:00:00Z' })
    assert.equal(merged.last_probe_ok, false)
    assert.equal(merged.last_probe_error, 'connect timeout')
  })

  it('withTimeout：到点拒绝 / 正常透传 / 原错误照旧（E）', async () => {
    await assert.rejects(
      () => withTimeout(new Promise<number>(() => {}), 5, 'timed out after 30s'),
      /timed out after 30s/,
    )
    assert.equal(await withTimeout(Promise.resolve(7), 50, 'x'), 7)
    await assert.rejects(() => withTimeout(Promise.reject(new Error('boom')), 50, 'x'), /boom/)
  })
})
