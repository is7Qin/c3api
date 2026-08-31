// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

import type { TFunction } from 'i18next'
import { useTranslation } from 'react-i18next'
import { Badge } from '@/components/ui/badge'
import { cn } from '@/lib/utils'

// BadgeStatus 徽章状态词表：账号运行时健康分布（dashboard 四态，源自调度器
// 快照——持久 status 列已删除）与 user/key 生命周期（active/disabled）共用。
export type BadgeStatus = 'active' | 'unhealthy' | '429' | 'disabled'

// 状态徽章：小圆点携带状态色 + 文字同色（均带 dark: 变体），chip 用 secondary 底随主题自适应。
const STATUS_META: Record<BadgeStatus, { dot: string; text: string }> = {
  active: { dot: 'bg-emerald-500', text: 'text-emerald-700 dark:text-emerald-400' },
  unhealthy: { dot: 'bg-amber-500', text: 'text-amber-700 dark:text-amber-400' },
  '429': { dot: 'bg-orange-500', text: 'text-orange-600 dark:text-orange-400' },
  disabled: { dot: 'bg-muted-foreground/60', text: 'text-muted-foreground' },
}

export function statusLabel(status: BadgeStatus | undefined, t: TFunction): string {
  return t(`status.${status ?? 'disabled'}`)
}

export function StatusBadge({ status, className }: { status?: BadgeStatus; className?: string }) {
  const { t } = useTranslation()
  const meta = STATUS_META[status ?? 'disabled']
  return (
    <Badge variant="secondary" className={cn('gap-1.5', meta.text, className)}>
      <span className={cn('size-1.5 shrink-0 rounded-full', meta.dot)} />
      {statusLabel(status, t)}
    </Badge>
  )
}
