// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 作用域上下文：把「当前页面用哪套 API base」从模块常量提升为可注入值，
// 使账号管理页（@/pages/accounts）在管理面（/api/admin）与供应商面
// （/api/user/supplier）之间**复用同一组件**——只切 BaseURL，不复制页面、
// 不建平行字段清单（spec 2026-10-09 §6.1/§10）。
//
// 默认值 = 管理端实例：未包 Provider 的既有路由（/app/accounts 等）行为逐字不变。
import { createContext, useContext, type ReactNode } from 'react'
import { adminApi, type ApiClient } from './client'

const ApiScopeContext = createContext<ApiClient>(adminApi)

export function ApiScopeProvider({ client, children }: { client: ApiClient; children: ReactNode }) {
  return <ApiScopeContext.Provider value={client}>{children}</ApiScopeContext.Provider>
}

// 页面组件取「当前作用域客户端」的唯一入口；后端按 JWT 作用域注入归属，
// 前端仅负责把请求打到对应的 base 前缀。
export function useScopedApi(): ApiClient {
  return useContext(ApiScopeContext)
}
