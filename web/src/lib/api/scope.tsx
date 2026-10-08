// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 作用域上下文：把「当前页面处于哪个面 + 用哪套 API base」从模块常量提升为**单一
// 类型化对象**，使账号管理页（@/pages/accounts）在管理面（/api/admin）与供应商面
// （/api/user/supplier）之间**复用同一组件**——只切作用域对象，不复制页面、不建
// 平行字段清单（spec 2026-10-09 §6.1/§10）。
//
// 单一事实源：客户端与 kind 同取自一个对象，组装路由时不再需要同时记得设置两处
// （此前 Context 客户端 + 独立 `scope` prop 共同决定）。
//
// 能力接口缩小（报告的「第二步」）：页面消费的是 **Pick<ApiClient, …实际方法…>**
// 的窄接口，而非完整客户端——「这个页面能调用什么」由类型回答，不靠约定。完整
// ApiClient 实现与 URL 不变，不复制 HTTP 客户端，不改鉴权逻辑。
//
// 默认值 = 管理端：未包 Provider 的既有路由（/app/accounts 等）行为逐字不变。
import { createContext, useContext, type ReactNode } from 'react'
import { adminApi, type ApiClient } from './client'

// SurfaceKind 当前页面所属的用户面。
export type SurfaceKind = 'admin' | 'supplier'

// AccountApi 账户管理页实际使用的方法子集（能力接口缩小）。管理面与供应商面复用
// 同一组件，故两面都只需这一子集；其余管理面/供应商面方法在类型上不可见。
export type AccountApi = Pick<
  ApiClient,
  | 'listAccounts'
  | 'createAccount'
  | 'updateAccount'
  | 'deleteAccount'
  | 'deleteAccountsBatch'
  | 'updateAccountsBatch'
  | 'getAccountExt'
  | 'putAccountExt'
  | 'getAccountGroups'
  | 'recoverAccount'
  | 'listAccountsUsage'
  | 'listGroups'
  | 'listTemplates'
  | 'getStatsCapabilities'
  | 'getStatsEntityTrend'
>

// SupplierConsoleApi 供应商控制台（业务页）实际使用的方法子集（独立小接口）。
export type SupplierConsoleApi = Pick<
  ApiClient,
  | 'getSupplierOverview'
  | 'getSupplierEarnings'
  | 'getSupplierChunks'
  | 'getSupplierSettlements'
  | 'applySupplierSettlement'
>

// SurfaceValue Context 内部承载值：完整客户端 + kind（Provider 由组合根以完整
// adminApi/supplierApi 装配）。
interface SurfaceValue {
  kind: SurfaceKind
  api: ApiClient
}

const defaultSurface: SurfaceValue = { kind: 'admin', api: adminApi }

const ApiScopeContext = createContext<SurfaceValue>(defaultSurface)

export function ApiScopeProvider({ value, children }: { value: SurfaceValue; children: ReactNode }) {
  return <ApiScopeContext.Provider value={value}>{children}</ApiScopeContext.Provider>
}

// useAccountSurface 账户页取「当前作用域（kind + 缩小的账户能力接口）」的唯一入口；
// 后端按 JWT 作用域注入归属，前端仅负责把请求打到对应的 base 前缀。
export function useAccountSurface(): { kind: SurfaceKind; api: AccountApi } {
  const s = useContext(ApiScopeContext)
  return { kind: s.kind, api: s.api }
}

// useScopedApi 需要完整客户端能力（或自定义窄接口，如 SupplierConsoleApi）的组件入口。
export function useScopedApi(): ApiClient {
  return useContext(ApiScopeContext).api
}
