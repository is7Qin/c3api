// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

import { Link, Outlet, useLocation } from 'react-router-dom'
import { motion } from 'framer-motion'
import { LayoutDashboard, Boxes, Users, UserCog, FolderOpen, FileText, BarChart3, ScrollText, Ticket, Coins, Settings, KeyRound, Cpu } from 'lucide-react'
import { useQuery } from '@tanstack/react-query'
import { useTranslation } from 'react-i18next'
import { userApi } from '@/lib/api/client'
import { setLang, type AppLang } from '@/lib/i18n'
import { Button } from '@/components/ui/button'
import { ModeToggle } from '@/components/mode-toggle'
import { cn } from '@/lib/utils'
import {
  Breadcrumb,
  BreadcrumbItem,
  BreadcrumbList,
  BreadcrumbPage,
  BreadcrumbSeparator,
} from '@/components/ui/breadcrumb'
import AppSidebar from '@/components/app-sidebar'
import { ScrollArea } from '@/components/ui/scroll-area'

// /app 与 /user 共用单一 AppShell：路由切换只换 Outlet 内容，
// 侧边栏/顶栏不重挂（消除闪烁）。导航由 me().Role 决定：
// platform_admin 同时看到管理组（/app）与用户组（/user），普通用户仅用户组。

// 用户中心菜单组（个人中心入口在底部用户卡内——用户裁决 2026-08-15，不放导航）
const userNav = [
  { to: '/user', key: 'user.nav.overview', icon: LayoutDashboard, end: true },
  { to: '/user/keys', key: 'user.nav.keys', icon: KeyRound, end: false },
  { to: '/user/logs', key: 'user.nav.logs', icon: FileText, end: false },
  { to: '/user/stats', key: 'user.nav.stats', icon: BarChart3, end: false },
  { to: '/user/redemptions', key: 'user.nav.redemptions', icon: Ticket, end: false },
]

// platform_admin 专属的管理端菜单组（排序 = 功能边界，2026-08-15 用户裁决）：
// 概览独立首位 → 代理配置域（模板/账户/规则——上游资源与转发策略）→ 客户域
// （用户/分组——下游消费方，与账户不直接相邻）→ 观测域（日志/统计）→ 商业域
// （兑换码/计费）→ 系统域（设置/运维）。平铺不拆子分组标题。
const adminNav = [
  { to: '/app/dashboard', key: 'nav.overview', icon: LayoutDashboard },
  { to: '/app/templates', key: 'nav.templates', icon: Boxes },
  { to: '/app/accounts', key: 'nav.accounts', icon: Users },
  { to: '/app/rules', key: 'nav.rules', icon: ScrollText },
  { to: '/app/users', key: 'nav.users', icon: UserCog },
  { to: '/app/groups', key: 'nav.groups', icon: FolderOpen },
  { to: '/app/logs', key: 'nav.logs', icon: FileText },
  { to: '/app/stats', key: 'nav.stats', icon: BarChart3 },
  { to: '/app/redemption-codes', key: 'nav.redemptions', icon: Ticket },
  { to: '/app/pricing', key: 'nav.pricing', icon: Coins },
  { to: '/app/settings', key: 'nav.settings', icon: Settings },
  { to: '/app/ops', key: 'nav.ops', icon: Cpu },
]

const LANGS: { code: AppLang; label: string }[] = [
  { code: 'zh-CN', label: '中' },
  { code: 'en', label: 'EN' },
]

// 路径 → 面包屑两级（分组根 + 当前页）；未知路径返回 null（顶栏不渲染面包屑）。
// 映射直接复用 adminNav/userNav（同一来源，避免漂移）：
// /app/* → 管理台 + 对应页名；/user/* → 用户中心 + 对应页名。
function breadcrumbFor(pathname: string): { root: string; section: string; page: string } | null {
  if (pathname.startsWith('/app')) {
    if (pathname === '/app') return { root: '/app', section: 'user.nav.adminSection', page: 'nav.overview' }
    const item = adminNav.find((n) => n.to === pathname)
    return item ? { root: '/app', section: 'user.nav.adminSection', page: item.key } : null
  }
  if (pathname.startsWith('/user')) {
    const item = userNav.find((n) => n.to === pathname)
    return item ? { root: '/user', section: 'user.nav.userSection', page: item.key } : null
  }
  return null
}

// me 未加载（undefined）时先按普通用户渲染用户组，避免侧边栏闪动；
// me 返回后 platform_admin 自动补上管理组。401 由 App.tsx 全局 handleAuthError 统一处理。
export default function AppShell() {
  const location = useLocation()
  const { t, i18n } = useTranslation()
  const lang: AppLang = i18n.resolvedLanguage?.startsWith('zh') ? 'zh-CN' : 'en'
  // 全局 qc 共享（与各页面同键缓存）；staleTime 60s 内路由切换不重复请求
  const { data: me } = useQuery({
    queryKey: ['user', 'me'],
    queryFn: () => userApi.me(),
    staleTime: 60_000,
  })
  const isAdmin = me?.Role === 'platform_admin'
  const navs = isAdmin
    ? [{ titleKey: 'user.nav.adminSection', items: adminNav }, { titleKey: 'user.nav.userSection', items: userNav }]
    : [{ titleKey: 'user.nav.userSection', items: userNav }]
  // 顶栏面包屑（两级）：未知路径返回 null 不渲染
  const crumb = breadcrumbFor(location.pathname)
  return (
    <div className="flex h-screen overflow-hidden">
      {/* h-screen 锁定视口高度：侧边栏 nav 与 main 各自 ScrollArea 独立滚动（自绘
          滚动条，深色模式统一观感）；底部用户卡固定左下角 */}
      <AppSidebar navs={navs} userEmail={me?.Email} />
      {/* 主内容滚动区：自定义滚动条（scroll-area 自绘 thumb，深色模式统一观感）——
          header sticky 依赖滚动容器在其内部 */}
      <main className="flex flex-1 flex-col">
        <ScrollArea className="flex-1">
          <header className="sticky top-0 z-10 flex h-16 shrink-0 items-center gap-2 border-b bg-background px-4 lg:px-6">
            {crumb && (
              <Breadcrumb className="min-w-0 flex-1">
                <BreadcrumbList>
                  <BreadcrumbItem className="hidden md:block">
                    <Link to={crumb.root} className="text-sm transition-colors hover:text-foreground">
                      {t(crumb.section)}
                    </Link>
                  </BreadcrumbItem>
                  <BreadcrumbSeparator className="hidden md:block" />
                  <BreadcrumbItem>
                    <BreadcrumbPage>{t(crumb.page)}</BreadcrumbPage>
                  </BreadcrumbItem>
                </BreadcrumbList>
              </Breadcrumb>
            )}
            <div className="flex shrink-0 items-center justify-end gap-2 lg:gap-3">
              <span className="text-sm text-muted-foreground">{me?.Email ?? ''}</span>
              <ModeToggle />
              <div className="inline-flex items-center gap-1 rounded-md border bg-background p-0.5">
                {LANGS.map(({ code, label }) => (
                  <Button
                    key={code}
                    size="sm"
                    variant="ghost"
                    className={cn('h-8 min-w-10 px-2', lang === code && 'bg-secondary text-secondary-foreground')}
                    onClick={() => setLang(code)}
                  >
                    {label}
                  </Button>
                ))}
              </div>
            </div>
          </header>
          <div className="@container/main flex flex-col gap-2 px-4 lg:px-6">
            <div className="flex flex-col gap-4 py-4 md:gap-6 md:py-6">
              <motion.div key={location.pathname} initial={{ opacity: 0, y: 8 }} animate={{ opacity: 1, y: 0 }} transition={{ duration: 0.2 }}>
                <Outlet />
              </motion.div>
            </div>
          </div>
        </ScrollArea>
      </main>
    </div>
  )
}
