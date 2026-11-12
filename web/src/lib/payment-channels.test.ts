// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 渠道子表的可操作性（spec 支付网关 v2 §13 P55 / §18-3 / §19-4b）。
//
// 事故形状：协议元数据坏掉（`GET /payment/providers` 失败）时，如果渠道子表跟着
// 一起"只读"，运维就**没法把这个账户停售**——渠道 `enabled` 是该账户能不能卖的
// 唯一开关（§5-1 ④）。一次元数据故障于是变成"必须继续卖"。
//
// 反过来，"新增渠道"确实需要元数据：`pay_type` 的取值域就是 `capabilities`，
// 手打的值服务端必拒（B54/P39）。两侧不对称，所以判据必须显式钉住。
import { describe, it } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import {
  MICRO,
  canAddChannel,
  channelDeleteRequest,
  channelDraftOf,
  channelRowActions,
  channelUpsertRequest,
  newChannelDraft,
  payTypeEditable,
  rateText,
  type ChannelDraft,
  type PaymentChannel,
} from './payment-channels.ts'

const CAPS: readonly string[] = ['alipay', 'wxpay']

// 一条渠道行（字段取 schema 的全部 required 项；判据只看 enabled/rate/限额）。
const channel = (over: Partial<PaymentChannel> = {}): PaymentChannel => ({
  id: 1,
  account_id: 7,
  pay_type: 'alipay',
  enabled: true,
  currency: 'CNY',
  rate_micro: 7_200_000,
  min_usd: 1,
  max_usd: 1000,
  created_at: '2026-11-12T00:00:00Z',
  updated_at: '2026-11-12T00:00:00Z',
  ...over,
})

// 一个"要停用"的草稿：enabled=false，其余保持既有行的值。
const disabling = (): ChannelDraft => ({ ...channelDraftOf(channel()), enabled: false })

describe('canAddChannel：新增渠道需要元数据（P55①）', () => {
  it('loading / failed 下不可新增（没有 capabilities 就没有 pay_type 取值域）', () => {
    for (const state of ['loading', 'failed'] as const) {
      assert.equal(canAddChannel(state, CAPS), false, `${state} 下不得开放新增渠道`)
    }
  })

  it('ready 但 capabilities 为空 → 仍不可新增（协议没声明任何支付方式）', () => {
    assert.equal(canAddChannel('ready', []), false)
  })

  it('ready + 非空 capabilities → 可新增', () => {
    assert.equal(canAddChannel('ready', CAPS), true)
  })
})

describe('channelRowActions：停用/删除与元数据无关（P55②）', () => {
  it('编辑与删除恒可用（函数不接收 metaState ⇒ 结构上不可能依赖它）', () => {
    assert.deepEqual(channelRowActions(), { edit: true, del: true })
  })

  it('三步链路成立：编辑既有行 → 关 enabled → 同一条 upsert（这就是"停用"）', () => {
    const d = disabling()
    assert.equal(d.isNew, false, '既有行是编辑态')
    assert.equal(payTypeEditable(d), false, '寻址键不可改')
    assert.equal(channelUpsertRequest('epay_main', d).body.enabled, false, '停用请求必须发得出去')
  })
})

describe('payTypeEditable：pay_type 是寻址键（P55③）', () => {
  it('既有渠道只读、新建可填', () => {
    assert.equal(payTypeEditable({ isNew: false }), false)
    assert.equal(payTypeEditable({ isNew: true }), true)
  })

  it('channelDraftOf 产出的既有草稿必然是只读的', () => {
    assert.equal(channelDraftOf(channel()).isNew, false)
    assert.equal(payTypeEditable(channelDraftOf(channel())), false)
  })
})

describe('渠道写路径不读元数据（P55④）', () => {
  it('upsert 请求只由 (account key, 草稿) 决定：停用 = enabled:false 的同一路径', () => {
    const req = channelUpsertRequest('epay_main', disabling())
    assert.equal(req.key, 'epay_main')
    assert.equal(req.payType, 'alipay')
    assert.deepEqual(req.body, {
      enabled: false,
      currency: 'CNY',
      rate_micro: 7_200_000,
      min_usd: 1,
      max_usd: 1000,
    })
  })

  it('同一草稿在"元数据可用 / 不可用"下构造出的请求逐字段相同（写路径没有元数据入参）', () => {
    const d = disabling()
    // 元数据状态不在这条路径的任何入参里：两种状态只能产出同一个请求。
    const duringOutage = channelUpsertRequest('epay_main', d)
    const whenHealthy = channelUpsertRequest('epay_main', d)
    assert.deepEqual(duringOutage, whenHealthy)
  })

  it('删除请求只需要 account key 与 pay_type（没有第三条信息）', () => {
    assert.deepEqual(channelDeleteRequest('epay_main', 'alipay'), {
      key: 'epay_main',
      payType: 'alipay',
    })
  })

  it('rate 文本 → rate_micro 是整数换算（7.2 → 7200000，不经 float 传输）', () => {
    const d = { ...channelDraftOf(channel()), rate: '7.2' }
    const micro = channelUpsertRequest('k', d).body.rate_micro
    assert.equal(micro, 7 * MICRO + 200_000)
    assert.ok(Number.isInteger(micro), 'rate_micro 必须是整数')
    assert.equal(rateText(micro), '7.2000', '显示与编辑初值同源（4 位小数粒度）')
  })
})

describe('草稿工厂（P55 的 fixture 本身也要可信）', () => {
  it('newChannelDraft 优先挑未被占用的 pay_type', () => {
    assert.equal(newChannelDraft(CAPS, ['alipay']).payType, 'wxpay')
    assert.equal(newChannelDraft(CAPS, CAPS).payType, 'alipay', '全被占用时回落到第一个')
    assert.equal(newChannelDraft([], []).payType, '', '没有取值域时给空串（此时不可新增）')
    assert.equal(newChannelDraft(CAPS, []).isNew, true)
  })

  it('channelDraftOf 把整数 micro 还原成编辑粒度文本，且保留 enabled', () => {
    const d = channelDraftOf(channel({ enabled: false, rate_micro: 7_187_500 }))
    assert.equal(d.rate, '7.1875')
    assert.equal(d.enabled, false)
    assert.equal(d.min, '1')
    assert.equal(d.max, '1000')
  })
})

describe('页面确实消费决策层（P55⑤：不是装饰）', () => {
  it('payment-accounts.tsx 调用了这四个判定函数', () => {
    const src = readFileSync(
      fileURLToPath(new URL('../pages/payment-accounts.tsx', import.meta.url)),
      'utf8',
    )
    for (const call of [
      'canAddChannel(',
      'channelRowActions(',
      'payTypeEditable(',
      'channelUpsertRequest(',
      'channelDeleteRequest(',
    ]) {
      assert.ok(src.includes(call), `页面必须调用 ${call}（否则 §19-4b 的纯函数只是装饰）`)
    }
  })
})
