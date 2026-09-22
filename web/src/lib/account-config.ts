// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 账号生命周期配置（倍率/缓存域）的共享校验：镜像后端 service.validateAccountPatch /
// service.validateCacheDomain——后端仍是唯一权威，这里只为 UI 即时反馈与按钮禁用。
// 创建/编辑、批量、codex 导入三个写面共用同一套边界，避免各自漂移。

// 成本倍率正常值（1 = ×1，0 = 免费，上限 ×10；bp 精度 = 4 位小数，normalToMult ×10000）。
// 非法/越界 → null（调用方据此禁用提交，fail-closed）。
export const parseMultiplier = (s: string): number | null => {
  if (!s.trim()) return null
  const v = Number(s)
  if (!Number.isFinite(v) || v < 0 || v > 10) return null
  return Math.round(v * 10000) / 10000
}

// 缓存域校验：labels 1–63（a–z/0–9/-，首尾非连字符）、点分、总长 ≤253；
// 空 = 账号私有域（清空走 null，不走空串）。
export const CACHE_DOMAIN_RE = /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$/
export const validCacheDomain = (s: string): boolean => s.length > 0 && s.length <= 253 && CACHE_DOMAIN_RE.test(s)
