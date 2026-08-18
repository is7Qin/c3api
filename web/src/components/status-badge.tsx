// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

import type { TFunction } from 'i18next'
import { useTranslation } from 'react-i18next'
import type { components } from '@/lib/api/schema'
import { Badge } from '@/components/ui/badge'
import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/ui/tooltip'
import { formatDateTime } from '@/components/fmt'
import { cn } from '@/lib/utils'

export type AccountStatus = components['schemas']['AccountStatus']

// 状态徽章：小圆点携带状态色 + 文字同色（均带 dark: 变体），chip 用 secondary 底随主题自适应。
const STATUS_META: Record<AccountStatus, { dot: string; text: string }> = {
  active: { dot: 'bg-emerald-500', text: 'text-emerald-700 dark:text-emerald-400' },
  unhealthy: { dot: 'bg-amber-500', text: 'text-amber-700 dark:text-amber-400' },
  '429': { dot: 'bg-orange-500', text: 'text-orange-600 dark:text-orange-400' },
  disabled: { dot: 'bg-muted-foreground/60', text: 'text-muted-foreground' },
}

export function statusLabel(status: AccountStatus | undefined, t: TFunction): string {
  return t(`status.${status ?? 'disabled'}`)
}

export function StatusBadge({ status, className }: { status?: AccountStatus; className?: string }) {
  const { t } = useTranslation()
  const meta = STATUS_META[status ?? 'disabled']
  return (
    <Badge variant="secondary" className={cn('gap-1.5', meta.text, className)}>
      <span className={cn('size-1.5 shrink-0 rounded-full', meta.dot)} />
      {statusLabel(status, t)}
    </Badge>
  )
}

// 冷却徽章（A-4，2026-08-19）：CooldownUntil 未过期 → "冷却中"（status=active
// 也标出——status 与冷却解耦展示：C-M2 语义下 ok 规则把 unhealthy/429 拉回
// active 而冷却保留，解决"active 却恒 429"的展示盲区）。过期/无冷却 → 不渲染。
// 样式同 StatusBadge 模式（secondary 底 + 圆点 + 文字同色，dark: 变体）。
export function CooldownBadge({ cooldownUntil, className }: { cooldownUntil?: string | null; className?: string }) {
  const { t } = useTranslation()
  if (!cooldownUntil) return null
  const until = new Date(cooldownUntil)
  if (Number.isNaN(until.getTime()) || until.getTime() <= Date.now()) return null
  return (
    <Tooltip>
      <TooltipTrigger
        render={
          <Badge variant="secondary" className={cn('gap-1.5 text-muted-foreground', className)}>
            <span className="size-1.5 shrink-0 rounded-full bg-muted-foreground/60" />
            {t('cooldown.active')}
          </Badge>
        }
      />
      <TooltipContent>{t('cooldown.until', { time: formatDateTime(cooldownUntil) })}</TooltipContent>
    </Tooltip>
  )
}
