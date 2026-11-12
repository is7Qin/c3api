// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 本二进制内建协议的 config 字段名（供前端镜像门禁自检使用）。
// ⚠️ 这份清单由 Go 侧测试（internal/paymentregistry/frontend_fields_test.go）与
//    epay.ConfigField* 常量 + 各协议 ConfigSchema() 做**集合等号**核对——改后端字段
//    必须同步这里，否则 Go 门禁红。**顺序在这里没有语义**：表单顺序来自元数据里
//    各协议的 config_schema.fields（§18-2）。
export const PAYMENT_CONFIG_FIELDS = ['base_url', 'pid', 'private_key', 'public_key'] as const
export const PAYMENT_PROVIDER_NAMES = ['epay_v2'] as const
