// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 渠道子表的**决策层**（spec 支付网关 v2 §13 P55 / §18-3 / §19-4b）：纯函数，无 React。
//
// 为什么单独一层：渠道的 `enabled` 是该账户"能不能卖"的**唯一**开关（§5-1 ④），
// 而协议元数据可能坏掉（loading / failed）。那时"能不能新增渠道"必须为否（新增要用
// `capabilities` 当 `pay_type` 取值域），但"已有渠道能不能停用 / 删除"**必须仍然为真**
// ——否则一次元数据故障就把资金链路锁死在"继续卖"上。
//
// 这条判据不能写在组件里靠肉眼 review：组件不可渲染（本仓前端测试只跑 `node --test`，
// 没有 jsdom / RTL），所以判定点必须能被纯函数用例直接钉住，页面只消费输出。

import type { components } from '@/lib/api/schema'
import type { MetaState } from '@/lib/payment-form'

export type PaymentChannel = components['schemas']['PaymentChannel']
export type PaymentChannelUpsert = components['schemas']['PaymentChannelUpsert']

/** 汇率定点：`rate_micro = 汇率 × 1e6`（契约面是整数，禁 float 传输）。 */
export const MICRO = 1_000_000

/** 渠道草稿（行内编辑器的状态）。`payType` 在"编辑既有渠道"时不可改——它是 upsert 的寻址键。 */
export type ChannelDraft = {
  payType: string
  enabled: boolean
  currency: string
  rate: string
  min: string
  max: string
  isNew: boolean
}

/**
 * 能不能新增渠道：需要元数据 ready（`capabilities` 才有值）且取值域非空。
 *
 * `failed` / `loading` 下**不可新增**：没有 capabilities 就只能让用户手打 pay_type，
 * 而手打的值服务端必拒（`pay_type ∈ capabilities(provider)`，B54/P39）。
 */
export function canAddChannel(metaState: MetaState, capabilities: readonly string[]): boolean {
  return metaState === 'ready' && capabilities.length > 0
}

/** 已有渠道行的动作可用性。 */
export type ChannelRowActions = {
  /** 打开行内编辑器（改汇率/限额，或把 `enabled` 切掉 = 停用该渠道） */
  edit: boolean
  /** 删除该渠道行（独立的 DELETE，不是 upsert） */
  del: boolean
}

/**
 * 已有渠道行永远可编辑 / 可删除（P55）。
 *
 * 这个函数**刻意不接收** `metaState`：只要它能读到元数据状态，就存在"故障期把它关掉"
 * 的可能，而"停用/删除一条渠道"根本不需要协议元数据（它只需要 account key +
 * pay_type，两者都在账户行上）。把"与元数据无关"写成签名，比写成三个分支的注释更硬。
 */
export function channelRowActions(): ChannelRowActions {
  return { edit: true, del: true }
}

/**
 * `pay_type` 是否可编辑：只有"新建渠道"可改。
 *
 * 既有渠道的 `pay_type` 是 upsert 的寻址键（`(account_id, pay_type)` 唯一）：改了
 * 等于"新建一条 + 留着旧的"，两种金额上限会同时可售。与元数据状态无关。
 */
export function payTypeEditable(draft: Pick<ChannelDraft, 'isNew'>): boolean {
  return draft.isNew
}

/**
 * 新建渠道草稿：优先挑一个"还没被该账户占用"的 pay_type（省一次手改），
 * capabilities 为空时给空串（此时 `canAddChannel` 必为 false，草稿不该被用到）。
 */
export function newChannelDraft(
  capabilities: readonly string[],
  used: readonly string[],
): ChannelDraft {
  const taken = new Set(used)
  const free = capabilities.find(c => !taken.has(c)) ?? capabilities[0] ?? ''
  return { payType: free, enabled: true, currency: 'CNY', rate: '', min: '1', max: '1000', isNew: true }
}

/** 既有渠道行 → 草稿（`rate` 用整数除法渲染，不经 float 往返）。 */
export function channelDraftOf(ch: PaymentChannel): ChannelDraft {
  return {
    payType: ch.pay_type,
    enabled: ch.enabled,
    currency: ch.currency,
    rate: rateText(ch.rate_micro),
    min: String(ch.min_usd),
    max: String(ch.max_usd),
    isNew: false,
  }
}

/**
 * 渠道 upsert 请求（`PUT /payment/accounts/{key}/channels/{pay_type}`）。
 *
 * **停用就是这条路径**：`enabled: false` 的同一个 upsert（没有单独的 enable/disable
 * 端点）。请求体**不读**协议元数据——这正是"元数据坏掉时仍能停一条渠道"的机制。
 */
export function channelUpsertRequest(
  accountKey: string,
  d: ChannelDraft,
): { key: string; payType: string; body: PaymentChannelUpsert } {
  return {
    key: accountKey,
    payType: d.payType,
    body: {
      enabled: d.enabled,
      currency: d.currency,
      // 汇率 ×1e6 落库：契约面是整数（禁 float 传输），在这里一次性换算。
      rate_micro: Math.round(Number(d.rate) * MICRO),
      min_usd: Number(d.min),
      max_usd: Number(d.max),
    },
  }
}

/** 渠道删除请求（`DELETE /payment/accounts/{key}/channels/{pay_type}`，幂等 204）。 */
export function channelDeleteRequest(
  accountKey: string,
  payType: string,
): { key: string; payType: string } {
  return { key: accountKey, payType }
}

/**
 * `rate_micro` → 4 位小数字符串（7200000 → "7.2000"）：整数除法，不经 float 往返。
 * 4 位小数是本页的编辑粒度；显示与编辑初值必须同源，否则一次"没改"的保存会把
 * 汇率按显示精度截断。
 */
export function rateText(micro: number): string {
  const sign = micro < 0 ? '-' : ''
  const abs = Math.abs(micro)
  const frac = String(abs % MICRO).padStart(6, '0').slice(0, 4)
  return `${sign}${Math.trunc(abs / MICRO)}.${frac}`
}
