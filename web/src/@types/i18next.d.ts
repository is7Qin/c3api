// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// i18next 类型增广（官方推荐范式）：把 i18n.ts 导出的 `resources as const` 与 `defaultNS`
// 接入 CustomTypeOptions，使 t() 的 key 得到校验、插值变量在传入 options 时获得类型推断。
// 前置：tsconfig 已开 `strict`（或 `strictNullChecks`）。
import 'i18next'
import { resources, defaultNS } from '@/lib/i18n'

declare module 'i18next' {
  interface CustomTypeOptions {
    defaultNS: typeof defaultNS
    resources: typeof resources['en']
  }
}
