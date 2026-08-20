// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

import { useState } from 'react'
import { NavLink, useNavigate } from 'react-router-dom'
import { ChevronDown, ChevronsUpDown, CircleUser, LogOut, type LucideIcon } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { userAuth } from '@/lib/auth'
import { cn } from '@/lib/utils'
import { ScrollArea } from '@/components/ui/scroll-area'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'

interface NavItem {
  to: string
  key: string
  icon: LucideIcon
  end?: boolean
}

interface NavGroup {
  titleKey?: string
  items: NavItem[]
}

// 管理端（/app）与用户端（/user）共享的侧边栏：品牌 + 可折叠分组 + 底部用户卡。
// 分组标题（有 titleKey 时）为可点击按钮，默认展开、点击收起；用户卡展示 email，菜单内退出登录。
export default function AppSidebar({ navs, userEmail }: { navs: NavGroup[]; userEmail?: string }) {
  const navTo = useNavigate()
  const { t } = useTranslation()
  // titleKey -> 是否折叠；不在集合内即默认展开
  const [collapsed, setCollapsed] = useState<Record<string, boolean>>({})

  const logout = () => {
    userAuth.clear()
    navTo('/user/login')
  }
  const avatarInitial = userEmail ? userEmail.charAt(0).toUpperCase() : ''

  const renderItems = (items: NavItem[]) =>
    items.map(({ to, key, icon: Icon, end }) => (
      <NavLink
        key={to}
        to={to}
        end={end}
        className={({ isActive }) =>
          `group flex min-h-10 items-center gap-2.5 rounded-[10px] px-3 text-sm font-medium transition-all duration-200 ${isActive ? 'bg-foreground text-background shadow-sm' : 'text-muted-foreground hover:bg-accent hover:text-accent-foreground'}`
        }
      >
        <Icon className="h-4 w-4 transition-transform duration-200 group-hover:scale-105" /> {t(key)}
      </NavLink>
    ))

  return (
    <aside data-od-id="app-sidebar" className="hidden w-[264px] shrink-0 border-r border-sidebar-border bg-sidebar text-sidebar-foreground md:flex md:flex-col">
      <div data-od-id="app-sidebar-brand" className="flex h-20 items-center gap-2 px-5">
        <span className="flex size-7 items-center justify-center rounded-[9px] bg-foreground text-[11px] font-semibold text-background">GP</span>
        <span className="text-[17px] font-semibold tracking-tight" style={{ fontFamily: '"SF Pro Display", "Helvetica Neue", Helvetica, sans-serif' }}>{t('common.appTitle')}</span>
      </div>
      {/* 自定义滚动条（scroll-area：自绘 thumb，深色模式不再刺眼） */}
      <ScrollArea className="flex-1">
        <nav data-od-id="app-sidebar-navigation" className="space-y-3 px-3 pb-3">
        {navs.map(group => (
          <div key={group.titleKey ?? group.items[0]?.to}>
            {group.titleKey ? (
              <>
                <button
                  type="button"
                  aria-expanded={!collapsed[group.titleKey]}
                  onClick={() => setCollapsed(prev => ({ ...prev, [group.titleKey!]: !prev[group.titleKey!] }))}
                  className="flex h-8 w-full items-center justify-between rounded-[8px] px-3 text-[11px] font-semibold tracking-wide text-muted-foreground transition-colors hover:bg-accent hover:text-accent-foreground"
                >
                  {t(group.titleKey)}
                  <ChevronDown
                    className={cn('h-3.5 w-3.5 transition-transform duration-200', collapsed[group.titleKey] ? '' : 'rotate-180')}
                  />
                </button>
                {!collapsed[group.titleKey] && renderItems(group.items)}
              </>
            ) : (
              renderItems(group.items)
            )}
          </div>
        ))}
        </nav>
      </ScrollArea>
      <div className="border-t border-sidebar-border p-3">
        <DropdownMenu>
          <DropdownMenuTrigger data-od-id="app-sidebar-account-menu" className="flex w-full items-center gap-2 rounded-[12px] p-2 transition-colors hover:bg-accent focus:bg-accent data-popup-open:bg-accent focus:outline-none">
            <span className="flex size-8 shrink-0 items-center justify-center rounded-full bg-foreground text-sm font-medium text-background">
              {avatarInitial}
            </span>
            <span className="min-w-0 flex-1 truncate text-left text-sm text-sidebar-foreground">{userEmail ?? ''}</span>
            <ChevronsUpDown className="h-4 w-4 shrink-0 text-muted-foreground" />
          </DropdownMenuTrigger>
          <DropdownMenuContent align="end" side="right" sideOffset={4} className="min-w-48">
            <div className="flex items-center gap-2 px-1.5 py-1.5 text-sm">
              <span className="flex size-8 shrink-0 items-center justify-center rounded-full bg-sidebar-accent text-sm font-medium text-sidebar-accent-foreground">
                {avatarInitial}
              </span>
              <span className="min-w-0 flex-1 truncate text-xs text-muted-foreground">{userEmail ?? ''}</span>
            </div>
            <DropdownMenuSeparator />
            {/* 个人中心入口在底部用户卡内（用户裁决 2026-08-15——不放侧边栏导航）；
                登出独立成组（分隔线隔开——参考 ui 仓库 nav-user 形态） */}
            <DropdownMenuItem onClick={() => navTo('/user/profile')}>
              <CircleUser /> {t('user.nav.profile')}
            </DropdownMenuItem>
            <DropdownMenuSeparator />
            <DropdownMenuItem variant="destructive" onClick={logout}>
              <LogOut /> {t('common.logout')}
            </DropdownMenuItem>
          </DropdownMenuContent>
        </DropdownMenu>
      </div>
    </aside>
  )
}
