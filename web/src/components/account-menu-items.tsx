// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 账户菜单条目（spec 2026-10-09 §4.1）：个人中心 / 管理 API key / 登出，由桌面侧边栏
// 底部用户卡与移动端 header 下拉**同一来源**渲染（单入口，桌面与移动端皆可达）。
import { useNavigate } from 'react-router-dom'
import { CircleUser, KeyRound, LogOut } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { userAuth } from '@/lib/auth'
import { DropdownMenuItem, DropdownMenuSeparator } from '@/components/ui/dropdown-menu'

export function AccountMenuItems() {
  const navTo = useNavigate()
  const { t } = useTranslation()
  const logout = () => { userAuth.clear(); navTo('/user/login') }
  return (
    <>
      <DropdownMenuItem onClick={() => navTo('/user/profile')}>
        <CircleUser /> {t('user.nav.profile')}
      </DropdownMenuItem>
      <DropdownMenuItem onClick={() => navTo('/user/management-keys')}>
        <KeyRound /> {t('user.nav.managementKeys')}
      </DropdownMenuItem>
      <DropdownMenuSeparator />
      <DropdownMenuItem variant="destructive" onClick={logout}>
        <LogOut /> {t('common.logout')}
      </DropdownMenuItem>
    </>
  )
}
