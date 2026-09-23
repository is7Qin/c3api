// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 旧代际占比徽标纯函数：服务端两整数 stale_chains / total_chains → 一位小数占比字符串。
// 框架无关（Node 内置运行器 `node --test` 单测，零新依赖），禁止引入 React/i18n。
// 可见性规则（唯一规则，无特例）：四舍五入到一位小数后 > 0 即见，否则隐藏（null）——
// stale_chains == 0（占比 0）与 total_chains == 0（占比无定义，不除零）皆被同一规则吞没，
// 且徽标永远不可能渲染自相矛盾的 `0.0%`。响应未就绪/字段缺失（undefined）同样返回 null。
export function staleShare(
  staleChains: number | null | undefined,
  totalChains: number | null | undefined,
): string | null {
  if (typeof staleChains !== 'number' || typeof totalChains !== 'number') return null
  if (!Number.isFinite(staleChains) || !Number.isFinite(totalChains)) return null
  if (totalChains <= 0) return null
  if (staleChains <= 0) return null
  const rounded = Math.round((staleChains / totalChains) * 1000) / 10
  if (rounded <= 0) return null
  return rounded.toFixed(1)
}
