// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 账号「冷却中」的唯一判据：会自愈的两档（open / retry_after）。
//
// 徽章与「恢复」按钮共用它：两处各写一遍 `state === 'open' || ...` 迟早漂移——
// 一处加了新档位、另一处漏掉，就会表现成「有琥珀徽章却没有恢复入口」。
// probing（探针接管中）/ ready（空闲）都不算冷却：前者已经在自愈流程里，
// 后者本来就没被限制。
import type { components } from '@/lib/api/schema'

export type AccountHealthView = components['schemas']['AccountHealth']

export function isCooling(h?: AccountHealthView | null): boolean {
  return h?.state === 'open' || h?.state === 'retry_after'
}
