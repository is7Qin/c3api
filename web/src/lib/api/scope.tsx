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
// 默认值 = 管理端：未包 Provider 的既有路由（/app/accounts 等）行为逐字不变。
import { createContext, useContext, type ReactNode } from 'react'
import { adminApi, type ApiClient } from './client'

// SurfaceKind 当前页面所属的用户面。
export type SurfaceKind = 'admin' | 'supplier'

// AccountSurface 作用域对象：客户端与 kind 的结构化绑定。
export interface AccountSurface {
  kind: SurfaceKind
  api: ApiClient
}

const defaultSurface: AccountSurface = { kind: 'admin', api: adminApi }

const ApiScopeContext = createContext<AccountSurface>(defaultSurface)

export function ApiScopeProvider({ value, children }: { value: AccountSurface; children: ReactNode }) {
  return <ApiScopeContext.Provider value={value}>{children}</ApiScopeContext.Provider>
}

// useAccountSurface 页面组件取「当前作用域（kind + 客户端）」的唯一入口；后端按 JWT
// 作用域注入归属，前端仅负责把请求打到对应的 base 前缀。
export function useAccountSurface(): AccountSurface {
  return useContext(ApiScopeContext)
}

// useScopedApi 仅需客户端、不关心 kind 的组件（导入对话框 / 供应商控制台）的便捷入口。
export function useScopedApi(): ApiClient {
  return useContext(ApiScopeContext).api
}
