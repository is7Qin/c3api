// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 钱包页面模式矩阵（wallet-topup-redeem）的纯逻辑——零 React、零请求。
//
// 三种模式严格对应 spec 的「模式矩阵」三行：
//   · disabled   支付关：只能兑换；充值/渠道/订单的一切都不存在（由渲染分支移除）。
//   · topup      支付开且有渠道：默认充值，兑换是主任务区第二个 Tab，记录区含订单。
//   · noChannels 支付开但零渠道：没有可提交的充值表单、没有订单列表，默认回落兑换，
//                并给出「暂时无法充值」说明 + 去兑换动作。
//
// 这里是「模式矩阵的唯一自动化保险」（本仓库没有 DOM 测试环境），因此可见集合与默认
// Tab 都从这一处导出；页面只负责按集合渲染，不再自行推导。
//
// 运行：`node --test`，见 wallet-mode.test.ts。

// 渠道（GET /api/user/payment/methods）三态：支付关闭时查询被禁用，不会进入 loading。
export type ChannelsState = 'loading' | 'ready' | 'empty'

export type WalletMode = 'disabled' | 'topup' | 'noChannels'

// 主任务 Tab（充值 / 兑换）与记录 Tab（订单 / 兑换记录）是两组独立的 Tabs。
export type MainTab = 'topup' | 'redeem'
export type RecordTab = 'orders' | 'redemptions'
export type WalletTab = MainTab | RecordTab

// `?tab=` 的合法值全集（其余值一律安静回落，不改 URL）。
export const WALLET_TABS: readonly WalletTab[] = ['topup', 'redeem', 'orders', 'redemptions']

export function isMainTab(v: unknown): v is MainTab {
  return v === 'topup' || v === 'redeem'
}

export function isRecordTab(v: unknown): v is RecordTab {
  return v === 'orders' || v === 'redemptions'
}

export function isWalletTab(v: unknown): v is WalletTab {
  return isMainTab(v) || isRecordTab(v)
}

// channels='loading' 归到 topup（字段位渲染 Skeleton），不能当成「零渠道」——否则每次
// 首次进入都会先闪一下「暂时无法充值」。
export function walletMode({ payment, channels }: { payment: boolean; channels: ChannelsState }): WalletMode {
  if (!payment) return 'disabled'
  if (channels === 'empty') return 'noChannels'
  return 'topup'
}

// 当前模式下可用的 Tab。零渠道没有 topup（不可下单，也不给可点的死 Tab）与 orders。
export function visibleTabs(mode: WalletMode): { main: readonly MainTab[]; records: readonly RecordTab[] } {
  if (mode === 'topup') return { main: ['topup', 'redeem'], records: ['orders', 'redemptions'] }
  return { main: ['redeem'], records: ['redemptions'] }
}

// 主任务默认 Tab（无参数时）。
export function defaultTab(mode: WalletMode): MainTab {
  return mode === 'topup' ? 'topup' : 'redeem'
}

// 记录区默认 Tab（无参数时）。
export function defaultRecordTab(mode: WalletMode): RecordTab {
  return mode === 'topup' ? 'orders' : 'redemptions'
}

// 余额卡下方的「有兑换码？去兑换」快捷入口：只要支付开启就保留（充值模式是第二入口，
// 零渠道是唯一出路）；支付关闭时默认已在兑换，再放快捷入口只是噪音。
export function showRedeemShortcut(mode: WalletMode): boolean {
  return mode !== 'disabled'
}

// 两组 Tab 的最终取值。优先级：模式默认 < 本地最后一次点击 < `?tab=`（URL 是权威）。
// - `?order=` 回跳（orderForced）把可用集合的第一项顶上去：支付开时即「充值 + 订单」，
//   让回跳结果不被兑换视图遮住；零渠道时仍只落到兑换。
// - 非法值、或该模式下不可用的值（支付关/零渠道的 topup、零渠道的 orders）安静回落，
//   调用方据此不修改 URL。
export function activeTabs(input: {
  raw: string | null
  picked: WalletTab | null
  mode: WalletMode
  orderForced: boolean
}): { main: MainTab; records: RecordTab } {
  const { raw, picked, mode, orderForced } = input
  const vis = visibleTabs(mode)
  let main = defaultTab(mode)
  let records = defaultRecordTab(mode)
  if (orderForced) {
    main = vis.main[0]
    records = vis.records[0]
  }
  if (picked !== null) {
    if (isMainTab(picked) && vis.main.includes(picked)) main = picked
    if (isRecordTab(picked) && vis.records.includes(picked)) records = picked
  }
  if (raw !== null) {
    if (isMainTab(raw)) main = vis.main.includes(raw) ? raw : defaultTab(mode)
    else if (isRecordTab(raw)) records = vis.records.includes(raw) ? raw : defaultRecordTab(mode)
  }
  return { main, records }
}
