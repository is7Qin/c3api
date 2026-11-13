// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 支付网关相关 UI 的可见性判定——菜单与面包屑的唯一来源。
// 开关取自 useFeatures()（GET /api/user/features 的 payment，即运行时设置
// payment.enabled）；调用方传入的 paymentEnabled 已是 fail-closed 结论（加载中 /
// 请求失败 = false），本文件不再区分三态。开启时结果与开关引入前逐项一致（同引用）。
//
// 纯函数（无 React、无请求）：node --test 直接跑，见 payment-visibility.test.ts。

// 支付网关的三张页面：管理面支付订单、管理面支付账户、用户面钱包（充值 + 我的订单）。
// 菜单项与路由共用同一组字面量。
export const PAYMENT_PATHS = ['/app/orders', '/app/payment-accounts', '/user/wallet'] as const

// 支付页面路径：按段匹配，/app/ordersx、/user/wallet-x 这类前缀相同的路径不算。
export function isPaymentPath(pathname: string): boolean {
  return PAYMENT_PATHS.some((p) => pathname === p || pathname.startsWith(`${p}/`))
}

// 菜单（管理组与用户组共用）：关闭时去掉支付页面，其余保序；开启时原样返回同一数组。
export function paymentNavFor<T extends { to: string }>(items: readonly T[], paymentEnabled: boolean): readonly T[] {
  if (paymentEnabled) return items
  return items.filter((n) => !isPaymentPath(n.to))
}
