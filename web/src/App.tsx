// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

import { MutationCache, QueryCache, QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { Outlet, RouterProvider, createBrowserRouter, Navigate } from 'react-router-dom'
import { ApiClient, ApiUnauthorized } from '@/lib/api/client'
import { ThemeProvider } from '@/components/theme-provider'
import { userAuth } from '@/lib/auth'
import { Toaster } from '@/components/ui/toast'
import Home from '@/pages/home'
import AppShell from '@/components/app-shell'
import UserLogin from '@/pages/user/login'
import UserRegister from '@/pages/user/register'
import UserOverview from '@/pages/user/overview'
import UserKeys from '@/pages/user/keys'
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

// 唯一登录态 userAuth：管理端 api 与用户端 userApi 同源取 token，
// platform_admin 的 JWT 同样通过 /admin 后端鉴权（middleware 已支持）。
export const api = new ApiClient(userAuth.getToken)

const router = createBrowserRouter([
  { path: '/', element: <Home /> },
  { path: '/user/login', element: <UserLogin /> },
  { path: '/user/register', element: <UserRegister /> },
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
        ],
      },
      {
        path: 'user',
        children: [
          { index: true, element: <UserOverview /> },
          { path: 'profile', element: <UserProfile /> },
          { path: 'keys', element: <UserKeys /> },
          { path: 'logs', element: <UserLogs /> },
          { path: 'stats', element: <UserStats /> },
          { path: 'redemptions', element: <UserRedemptions /> },
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

// 401 全局拦截（Task 2→3 handoff 硬性要求）：任何 query/mutation 收到
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
      <QueryClientProvider client={qc}>
        <RouterProvider router={router} />
      </QueryClientProvider>
      <Toaster />
    </ThemeProvider>
  )
}
