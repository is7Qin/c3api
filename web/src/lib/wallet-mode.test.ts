// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 钱包模式矩阵的自动化保险（spec 2026-11-15-wallet-topup-redeem「模式矩阵」+「门禁」）：
// 三模式全量断言默认 Tab、可见 Tab 集合、快捷入口可见性；再锁 `?tab` 的归约规则
// （合法值 / 非法值 / 该模式下不可用的值 / `?order=` 回跳的强制上下文）。
// 运行：`node --test`（Node 内置运行器原生跑 TS，零新依赖）。
import { describe, it } from 'node:test'
import assert from 'node:assert/strict'
import {
  WALLET_TABS,
  activeTabs,
  defaultRecordTab,
  defaultTab,
  isMainTab,
  isRecordTab,
  isWalletTab,
  showRedeemShortcut,
  visibleTabs,
  walletMode,
  type ChannelsState,
  type WalletMode,
} from './wallet-mode.ts'

const ALL_CHANNELS: ChannelsState[] = ['loading', 'ready', 'empty']

// spec「模式矩阵」三行的逐格期望：这是唯一自动化防线，值必须逐字对齐表格。
const MATRIX: Record<
  WalletMode,
  {
    main: string[]
    records: string[]
    defaultMain: string
    defaultRecords: string
    shortcut: boolean
    firstScreenTopup: boolean
    orderList: boolean
  }
> = {
  // 支付关：默认兑换，首屏就是兑换表单，订单列表完全不渲染。
  disabled: {
    main: ['redeem'],
    records: ['redemptions'],
    defaultMain: 'redeem',
    defaultRecords: 'redemptions',
    shortcut: false,
    firstScreenTopup: false,
    orderList: false,
  },
  // 支付开：四 Tab，默认充值，订单在记录区。
  topup: {
    main: ['topup', 'redeem'],
    records: ['orders', 'redemptions'],
    defaultMain: 'topup',
    defaultRecords: 'orders',
    shortcut: true,
    firstScreenTopup: true,
    orderList: true,
  },
  // 支付开但零渠道：没有可提交的充值表单、没有订单列表，默认回落兑换，快捷入口保留。
  noChannels: {
    main: ['redeem'],
    records: ['redemptions'],
    defaultMain: 'redeem',
    defaultRecords: 'redemptions',
    shortcut: true,
    firstScreenTopup: false,
    orderList: false,
  },
}

describe('walletMode（模式矩阵的行选择）', () => {
  it('支付关 ⇒ disabled（与渠道状态无关：支付关闭时渠道查询根本不发）', () => {
    for (const channels of ALL_CHANNELS) {
      assert.equal(walletMode({ payment: false, channels }), 'disabled', channels)
    }
  })

  it('支付开 + loading/ready ⇒ topup（加载中先给字段 Skeleton，不得当成零渠道）', () => {
    assert.equal(walletMode({ payment: true, channels: 'loading' }), 'topup')
    assert.equal(walletMode({ payment: true, channels: 'ready' }), 'topup')
  })

  it('支付开 + empty ⇒ noChannels（说明卡 + 去兑换，无可提交表单）', () => {
    assert.equal(walletMode({ payment: true, channels: 'empty' }), 'noChannels')
  })
})

describe('visibleTabs / defaultTab / 快捷入口（三模式全量）', () => {
  for (const mode of ['disabled', 'topup', 'noChannels'] as const) {
    it(`${mode}：可用集合、默认 Tab、快捷入口、充值位与订单列表`, () => {
      const want = MATRIX[mode]
      const vis = visibleTabs(mode)
      assert.deepEqual([...vis.main], want.main, `${mode} 主任务 Tab`)
      assert.deepEqual([...vis.records], want.records, `${mode} 记录 Tab`)
      assert.equal(defaultTab(mode), want.defaultMain, `${mode} 默认主任务 Tab`)
      assert.equal(defaultRecordTab(mode), want.defaultRecords, `${mode} 默认记录 Tab`)
      assert.equal(showRedeemShortcut(mode), want.shortcut, `${mode} 快捷入口`)
      assert.equal(vis.main.includes('topup'), want.firstScreenTopup, `${mode} 充值位`)
      assert.equal(vis.records.includes('orders'), want.orderList, `${mode} 订单列表`)
    })
  }

  it('支付关闭绝不出现充值 / 订单（渲染分支的依据）', () => {
    const vis = visibleTabs('disabled')
    assert.equal(vis.main.includes('topup'), false)
    assert.equal(vis.records.includes('orders'), false)
    assert.equal(defaultTab('disabled'), 'redeem')
  })

  it('零渠道没有 topup / orders（不可下单，不给可点的死 Tab）', () => {
    const vis = visibleTabs('noChannels')
    assert.equal(vis.main.includes('topup'), false)
    assert.equal(vis.records.includes('orders'), false)
  })
})

describe('?tab 契约的合法值', () => {
  it('恰好是 topup|redeem|orders|redemptions（不扩展）', () => {
    assert.deepEqual([...WALLET_TABS], ['topup', 'redeem', 'orders', 'redemptions'])
  })

  it('分组判定：前两者主任务，后两者记录区', () => {
    assert.equal(isMainTab('topup'), true)
    assert.equal(isMainTab('redeem'), true)
    assert.equal(isMainTab('orders'), false)
    assert.equal(isRecordTab('orders'), true)
    assert.equal(isRecordTab('redemptions'), true)
    assert.equal(isWalletTab('topup'), true)
    assert.equal(isWalletTab('redemptions'), true)
    for (const bad of ['', 'Topup', 'wallet', 'order', 'topup ', null, undefined, 1]) {
      assert.equal(isWalletTab(bad), false, String(bad))
    }
  })
})

describe('activeTabs（?tab 归一 + ?order 优先）', () => {
  const tabs = (raw: string | null, picked: Parameters<typeof activeTabs>[0]['picked'], mode: WalletMode, orderForced = false) =>
    activeTabs({ raw, picked, mode, orderForced })

  it('无参数 ⇒ 模式默认（充值开：topup/orders；其余：redeem/redemptions）', () => {
    assert.deepEqual(tabs(null, null, 'topup'), { main: 'topup', records: 'orders' })
    assert.deepEqual(tabs(null, null, 'noChannels'), { main: 'redeem', records: 'redemptions' })
    assert.deepEqual(tabs(null, null, 'disabled'), { main: 'redeem', records: 'redemptions' })
  })

  it('合法且可用 ⇒ 原样采用（两组各自生效，另一组仍是默认）', () => {
    assert.deepEqual(tabs('redeem', null, 'topup'), { main: 'redeem', records: 'orders' })
    assert.deepEqual(tabs('orders', null, 'topup'), { main: 'topup', records: 'orders' })
    assert.deepEqual(tabs('redemptions', null, 'topup'), { main: 'topup', records: 'redemptions' })
    assert.deepEqual(tabs('redeem', null, 'disabled'), { main: 'redeem', records: 'redemptions' })
  })

  it('非法值 ⇒ 安静回落模式默认（不产生"要写回 URL"的信号）', () => {
    assert.deepEqual(tabs('foo', null, 'topup'), { main: 'topup', records: 'orders' })
    assert.deepEqual(tabs('', null, 'topup'), { main: 'topup', records: 'orders' })
    assert.deepEqual(tabs('Topup', null, 'topup'), { main: 'topup', records: 'orders' })
  })

  it('该模式下不可用的值 ⇒ 回落到该组默认（topup 在支付关 / 零渠道，orders 在零渠道）', () => {
    assert.deepEqual(tabs('topup', null, 'disabled'), { main: 'redeem', records: 'redemptions' })
    assert.deepEqual(tabs('topup', null, 'noChannels'), { main: 'redeem', records: 'redemptions' })
    assert.deepEqual(tabs('orders', null, 'noChannels'), { main: 'redeem', records: 'redemptions' })
  })

  it('本地最后一次点击只在 URL 未指向该组时保留（切换 Tab 不丢另一组选择）', () => {
    // 记录区点过 redemptions，之后点主任务 redeem ⇒ 记录区保持 redemptions
    assert.deepEqual(tabs('redeem', 'redemptions', 'topup'), { main: 'redeem', records: 'redemptions' })
    // URL 指向记录组时，主任务组保留本地点击
    assert.deepEqual(tabs('redemptions', 'redeem', 'topup'), { main: 'redeem', records: 'redemptions' })
    // 不可用的本地点击不生效
    assert.deepEqual(tabs(null, 'topup', 'noChannels'), { main: 'redeem', records: 'redemptions' })
  })

  it('?order= 回跳：强制充值/订单上下文（可用时），且优先于模式默认', () => {
    assert.deepEqual(tabs(null, null, 'topup', true), { main: 'topup', records: 'orders' })
    // 显式 ?tab=redeem 仍可切主任务视图（等待卡在 Tabs 之上渲染，不会被遮住）
    assert.deepEqual(tabs('redeem', null, 'topup', true), { main: 'redeem', records: 'orders' })
    // 零渠道没有订单 Tab 可落，但上下文本身由调用方渲染说明卡
    assert.deepEqual(tabs(null, null, 'noChannels', true), { main: 'redeem', records: 'redemptions' })
  })
})
