// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 供应商相关 UI 的可见性判定——菜单、面包屑、角色选项、运维统计项的唯一来源。
// 开关取自 useFeatures()（GET /api/user/features 的 supplier）；调用方传入的
// supplierEnabled 已是 fail-closed 结论（加载中 / 请求失败 = false），本文件不再
// 区分三态。开启时每个函数的结果与开关引入前逐项一致（同选项、同顺序、同引用）。
//
// 纯函数（无 React、无请求）：node --test 直接跑，见 supplier-visibility.test.ts。
import type { components } from '@/lib/api/schema'

type UserRole = components['schemas']['UserRole']

// 管理面结算工作台路径（管理菜单项与路由共用同一字面量）。
export const ADMIN_SUPPLIER_PATH = '/app/supplier'

// 供应商面路径：管理面结算工作台 + 供应商控制台整棵子树。按段匹配，
// /user/supplierx、/app/supplier-x 这类前缀相同的路径不算。
export function isSupplierPath(pathname: string): boolean {
  return pathname === ADMIN_SUPPLIER_PATH || pathname === '/user/supplier' || pathname.startsWith('/user/supplier/')
}

// 管理菜单：关闭时去掉结算工作台，其余保序；开启时原样返回同一数组（引用不变）。
export function adminNavFor<T extends { to: string }>(items: readonly T[], supplierEnabled: boolean): readonly T[] {
  if (supplierEnabled) return items
  return items.filter((n) => n.to !== ADMIN_SUPPLIER_PATH)
}

// 「供应商」菜单组：开关开启且角色在供应商面可达集（supplier | platform_admin）内。
export function showSupplierNav(role: UserRole | undefined, supplierEnabled: boolean): boolean {
  return supplierEnabled && (role === 'platform_admin' || role === 'supplier')
}

// 用户列表的角色筛选项：关闭时不提供「供应商」。
export function filterableRoles(supplierEnabled: boolean): UserRole[] {
  return supplierEnabled ? ['platform_admin', 'user', 'supplier'] : ['platform_admin', 'user']
}

// 新建/编辑表单的角色选项：关闭时不提供「供应商」，但正在编辑的用户当前就是
// supplier 时保留该项——下拉才能显示当前值，不改角色直接保存也不会改掉它。
export function assignableRoles(supplierEnabled: boolean, currentRole?: UserRole): UserRole[] {
  return supplierEnabled || currentRole === 'supplier' ? ['platform_admin', 'user', 'supplier'] : ['platform_admin', 'user']
}

// 运维 worker 统计项：capacity_blocked 的唯一后果是停供应商新流量，关闭时不展示。
export function isOpsStatVisible(key: string, supplierEnabled: boolean): boolean {
  return supplierEnabled || key !== 'capacity_blocked'
}
