// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

import { MutationCache, QueryCache, QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { Outlet, RouterProvider, createBrowserRouter, Navigate } from 'react-router-dom'
import { ApiUnauthorized, adminApi, supplierApi } from '@/lib/api/client'
import { ApiScopeProvider } from '@/lib/api/scope'
import { ThemeProvider } from '@/components/theme-provider'
import { userAuth } from '@/lib/auth'
import { Toaster } from '@/components/ui/toast'
import { FluidCanvas } from '@/components/fluid-canvas'
import Home from '@/pages/home'
import AppShell from '@/components/app-shell'
import UserLogin from '@/pages/user/login'
import UserRegister from '@/pages/user/register'
import ForgotPassword from '@/pages/user/forgot-password'
import UserOverview from '@/pages/user/overview'
import UserKeys from '@/pages/user/keys'
import UserManagementKeys from '@/pages/user/management-keys'
import UserLogs from '@/pages/user/logs'
import UserStats from '@/pages/user/stats'
import UserRedemptions from '@/pages/user/redemptions'
import UserProfile from '@/pages/user/profile'
import Forbidden from '@/pages/forbidden'
import NotFound from '@/pages/not-found'
import Dashboard from '@/pages/dashboard'
import Templates from '@/pages/templates'
import Accounts from '@/pages/accounts'
import Users from '@/pages/users'
import Groups from '@/pages/groups'
import Logs from '@/pages/logs'
import Stats from '@/pages/stats'
import Rules from '@/pages/rules'
import RedemptionCodes from '@/pages/redemption-codes'
import PricingPage from '@/pages/pricing'
import SettingsPage from '@/pages/settings'
import Ops from '@/pages/ops'
import SupplierConsole from '@/pages/supplier/console'
import SupplierAdmin from '@/pages/supplier-admin'

// 唯一登录态 userAuth：管理端 api 与用户端 userApi 同源取 token，
// platform_admin 的 JWT 同样通过 /admin 后端鉴权（middleware 已支持）。
// api = 管理端实例（/api/admin），实例定义下沉到 client.ts（adminApi），
// 以便供应商面（supplierApi）与作用域上下文共享同一批方法而不产生循环依赖。
export const api = adminApi

const router = createBrowserRouter([
  { path: '/', element: <Home /> },
  { path: '/user/login', element: <UserLogin /> },
  { path: '/user/register', element: <UserRegister /> },
  { path: '/user/forgot-password', element: <ForgotPassword /> },
  // /app 与 /user 共用单一 AppShell：路由切换只换 Outlet，侧边栏/顶栏不重挂
  {
    path: '/',
    element: <AppShell />,
    children: [
      {
        path: 'app',
        element: <RequireAdmin />,
        children: [
          { index: true, element: <Navigate to="/app/dashboard" replace /> },
          { path: 'dashboard', element: <Dashboard /> },
          { path: 'templates', element: <Templates /> },
          { path: 'accounts', element: <Accounts /> },
          { path: 'users', element: <Users /> },
          { path: 'groups', element: <Groups /> },
          { path: 'logs', element: <Logs /> },
          { path: 'stats', element: <Stats /> },
          { path: 'rules', element: <Rules /> },
          { path: 'redemption-codes', element: <RedemptionCodes /> },
          { path: 'pricing', element: <PricingPage /> },
          { path: 'settings', element: <SettingsPage /> },
          { path: 'ops', element: <Ops /> },
          { path: 'supplier', element: <SupplierAdmin /> },
        ],
      },
      {
        path: 'user',
        children: [
          { index: true, element: <UserOverview /> },
          { path: 'profile', element: <UserProfile /> },
          { path: 'keys', element: <UserKeys /> },
          { path: 'management-keys', element: <UserManagementKeys /> },
          { path: 'logs', element: <UserLogs /> },
          { path: 'stats', element: <UserStats /> },
          { path: 'redemptions', element: <UserRedemptions /> },
          // 供应商控制台（spec 2026-10-09 §6.1）：门控 RequireSupplier（supplier | platform_admin）。
          { path: 'supplier', element: <RequireSupplier />, children: [
            // 整个供应商域注入 supplierApi（base /api/user/supplier）作用域。
            { element: <SupplierScope />, children: [
              { index: true, element: <SupplierConsole /> },
              // 账号管理页**复用同一 Accounts 组件**（作用域由 Provider 单一对象决定，
              // 不再传 scope prop）。
              { path: 'accounts', element: <Accounts /> },
            ] },
          ] },
        ],
      },
    ],
  },
  // 无匹配路径兜底：必须置于最后，避免吞掉 /user 与 /app 子路由
  { path: '*', element: <NotFound /> },
])

// 管理端路由守卫：未登录一律跳登录页；已登录但角色不足渲染 403 界面。
// 安全默认：token 存在但 role 缺失（旧会话残留、localStorage 被手动清理）一律视为无权限。
// 后端鉴权仍在（非 platform_admin JWT → 401 → handleAuthError 清 token 跳 /user/login），此守卫只是前端第一层拦截。
function RequireAdmin() {
  if (!userAuth.getToken()) return <Navigate to="/user/login" replace />
  if (userAuth.getRole() !== 'platform_admin') return <Forbidden />
  return <Outlet />
}

// 供应商面门控（spec 2026-10-09 §2.6 可达集 = {supplier, platform_admin}）：
// 后端 RequireRole 仍兜底（非可达角色 → 401/403），此处仅前端第一层拦截。
// platform_admin 亦可进入（查看/管理自己名下账号与收益；非供应商管理员看到空态，无害）。
function RequireSupplier() {
  if (!userAuth.getToken()) return <Navigate to="/user/login" replace />
  const role = userAuth.getRole()
  if (role !== 'supplier' && role !== 'platform_admin') return <Forbidden />
  return <Outlet />
}

// 供应商作用域注入：整个供应商域（控制台 + 账号页）的作用域对象为
// { kind: 'supplier', api: supplierApi }（base /api/user/supplier）。账号页管理面/
// supplier 复用同一组件，kind 与客户端同取自**一个对象**。
function SupplierScope() {
  return (
    <ApiScopeProvider value={{ kind: 'supplier', api: supplierApi }}>
      <Outlet />
    </ApiScopeProvider>
  )
}

// 401 全局拦截：任何 query/mutation 收到
// ApiUnauthorized（client.ts 对 401 响应的归一化）→ 清 token + 跳 /user/login。
// 页面无需各自 onError 兜底；QueryCache/MutationCache 的 onError 在 React Query
// v5 中对所有活跃观测者/变更统一触发（queries.retry: 0 保证每个请求只报一次）。
const handleAuthError = (err: unknown) => {
  if (err instanceof ApiUnauthorized) {
    userAuth.clear()
    router.navigate('/user/login')
  }
}

const qc = new QueryClient({
  queryCache: new QueryCache({ onError: handleAuthError }),
  mutationCache: new MutationCache({ onError: handleAuthError }),
  defaultOptions: {
    queries: { retry: 0, refetchOnWindowFocus: false },
    mutations: { retry: 0 },
  },
})

export default function App() {
  return (
    <ThemeProvider defaultTheme="system" storageKey="vite-ui-theme">
      <div data-glass-ambient aria-hidden="true">
        <FluidCanvas />
      </div>
      <QueryClientProvider client={qc}>
        <RouterProvider router={router} />
      </QueryClientProvider>
      <Toaster />
    </ThemeProvider>
  )
}
