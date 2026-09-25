// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 错误位旁的「服务端实际查的窗口」提示（spec §4.4(c)/§5）。
//
// 服务端在 400 体里回显**判定后的生效窗口**与机读原因：归一化会把两端向后取整
// 到整点（窗口微移 ≤1h）、闸门拒绝判在生效窗口上——用户只有看到这两个时刻才知道
// "我请求的那段"和"服务器实际查的那段"差在哪里。文案与 reason 词表都是服务端给的
// （`reason` 取值集合的唯一来源是 spec §4.4(d) 的映射表），前端不推断、不重算。
//
// 非统计错误（无 effective_from/effective_to）不渲染任何东西。
import { useTranslation } from 'react-i18next'
import { ApiError } from '@/lib/api/client'
import { formatDateTime } from '@/components/fmt'

export function StatsWindowNotice({ error }: { error: unknown }) {
  const { t } = useTranslation()
  if (!(error instanceof ApiError)) return null
  if (!error.effectiveFrom && !error.effectiveTo) return null
  return (
    <p className="mt-1 text-xs text-muted-foreground">
      {t('stats.window.effective', {
        from: error.effectiveFrom ? formatDateTime(error.effectiveFrom) : '—',
        to: error.effectiveTo ? formatDateTime(error.effectiveTo) : '—',
      })}
      {error.reason ? ` · ${t(`stats.window.reason.${error.reason}`)}` : ''}
    </p>
  )
}
