// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

import { useTranslation } from 'react-i18next'
import type { TFunction } from 'i18next'
import { cn } from '@/lib/utils'

// 组选项展示数据（用户面 key 选组：创建下拉/编辑下拉/就地切组 Popover 共用）。
// description 空串/null/undefined = 无描述；multiplier null/undefined = 未知（显示 —）。
export interface GroupChoice {
  id: number
  name: string
  description?: string | null
  multiplier?: number | null
}

// 组价格倍率（正常值，API 边界已换算）→ 展示：null/undefined = 未设置（—）；
// 0 = 免费（t('groups.free')）；其余 ×N（1 = ×1.0，1.5 = ×1.5）。单一来源
// （groups.tsx / users.tsx / keys.tsx 共用，避免多份重复漂移）。
export function formatGroupMultiplier(m: number | null | undefined, t: TFunction): string {
  if (m == null) return '—'
  if (m === 0) return t('groups.free')
  return `×${m.toFixed(1)}`
}

// 组选项展示：名称 + 价格倍率（右）+ 描述次行（muted，空/纯空白描述隐藏）。
// 外层用 span（Select 的 ItemText 亦为 span，避免 div 嵌 span）；flex 行内需
// flex-1 min-w-0（名称可截断、倍率不被挤走）。返回值让 TS 推断（不用 JSX.Element）。
export function GroupOptionMeta({ choice, className }: { choice: GroupChoice; className?: string }) {
  const { t } = useTranslation()
  const desc = choice.description?.trim()
  return (
    <span className={cn('flex flex-1 min-w-0 flex-col gap-0.5 text-left', className)}>
      <span className="flex items-center justify-between gap-2">
        <span className="min-w-0 truncate">{choice.name}</span>
        <span className="shrink-0 text-xs tabular-nums text-muted-foreground">{formatGroupMultiplier(choice.multiplier, t)}</span>
      </span>
      {desc && <span className="truncate text-xs text-muted-foreground" title={desc}>{desc}</span>}
    </span>
  )
}
