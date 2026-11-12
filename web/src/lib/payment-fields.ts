// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 本二进制内建协议的 config 字段名（供前端门禁与回归测试使用）。
// ⚠️ 这份清单由 Go 侧测试（cmd/server 或 internal/paymentregistry）与
//    epay.ConfigField* 常量做**等号**核对——改后端字段必须同步这里，否则 Go 门禁红。
export const PAYMENT_CONFIG_FIELDS = ['base_url', 'pid', 'private_key', 'public_key'] as const
export const PAYMENT_PROVIDER_NAMES = ['epay_v2'] as const
