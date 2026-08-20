// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

import type { ReactNode } from 'react'
import { useTranslation } from 'react-i18next'
import { Search, X } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'

// 列表页通用工具栏：名称搜索 + 资源专属筛选。
// 全部受控（props 驱动），children 由页面注入资源专属筛选（status 多选、template 下拉等）。
// 排序已由表格列头（SortableHeader）承担，工具栏不再提供排序控件。
// 容器与表格卡片同为 border bg-card 分层；搜索/筛选/按钮统一 h-9。
export function ListToolbar({
  name,
  onNameChange,
  placeholder,
  children,
}: {
  name: string
  onNameChange: (name: string) => void
  placeholder?: string // 默认 list.search（"搜索名称"）；按资源覆盖（如邮箱搜索）
  children?: ReactNode
}) {
  const { t } = useTranslation()
  return (
    <div className="flex flex-wrap items-center gap-2 rounded-[14px] border border-[rgba(19,45,83,0.26)] bg-[color:var(--glass-card-light)] p-3 shadow-[inset_0_1px_0_rgba(255,255,255,0.5),0_10px_36px_rgba(19,45,83,0.16)] backdrop-blur-[var(--glass-blur)] dark:border-[rgba(148,180,220,0.32)] dark:bg-[color:var(--glass-card-dark)] dark:shadow-[inset_0_1px_0_rgba(255,255,255,0.07),0_10px_36px_rgba(2,6,14,0.5)]">
      <div className="relative w-48">
        <Search className="pointer-events-none absolute top-1/2 left-2.5 size-4 -translate-y-1/2 text-muted-foreground" />
        <Input
          className="h-9 pl-8"
          placeholder={placeholder ?? t('list.search')}
          value={name}
          onChange={e => onNameChange(e.target.value)}
        />
      </div>
      {name !== '' && (
        <Button variant="ghost" size="lg" onClick={() => onNameChange('')}>
          <X />
          {t('list.reset')}
        </Button>
      )}
      {children}
    </div>
  )
}
