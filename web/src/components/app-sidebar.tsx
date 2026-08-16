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
          `group flex items-center gap-2 rounded-md px-3 py-2.5 text-sm transition-colors ${isActive ? 'bg-sidebar-accent text-sidebar-accent-foreground' : 'text-sidebar-foreground/60 hover:bg-sidebar-accent hover:text-sidebar-accent-foreground'}`
        }
      >
        <Icon className="h-4 w-4 transition-transform duration-150 group-hover:scale-110" /> {t(key)}
      </NavLink>
    ))

  return (
    <aside className="w-64 border-r border-sidebar-border bg-sidebar text-sidebar-foreground flex flex-col">
      <div className="p-4 font-semibold text-lg">{t('common.appTitle')}</div>
      {/* 自定义滚动条（scroll-area：自绘 thumb，深色模式不再刺眼） */}
      <ScrollArea className="flex-1">
        <nav className="space-y-1 p-2">
        {navs.map(group => (
          <div key={group.titleKey ?? group.items[0]?.to}>
            {group.titleKey ? (
              <>
                <button
                  type="button"
                  aria-expanded={!collapsed[group.titleKey]}
                  onClick={() => setCollapsed(prev => ({ ...prev, [group.titleKey!]: !prev[group.titleKey!] }))}
                  className="flex w-full items-center justify-between rounded-md px-3 pt-3 pb-1 text-xs font-medium text-sidebar-foreground/40 transition-colors hover:bg-sidebar-accent hover:text-sidebar-accent-foreground"
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
          <DropdownMenuTrigger className="flex w-full items-center gap-2 rounded-lg p-2 hover:bg-sidebar-accent focus:bg-sidebar-accent data-popup-open:bg-sidebar-accent focus:outline-none">
            <span className="flex size-8 shrink-0 items-center justify-center rounded-full bg-sidebar-accent text-sm font-medium text-sidebar-accent-foreground">
              {avatarInitial}
            </span>
            <span className="min-w-0 flex-1 truncate text-left text-sm">{userEmail ?? ''}</span>
            <ChevronsUpDown className="h-4 w-4 shrink-0 text-sidebar-foreground/40" />
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
