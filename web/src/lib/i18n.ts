// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

import i18n from 'i18next'
import { initReactI18next } from 'react-i18next'
import type { ParseKeys, ParseKeysWithoutVariables } from 'i18next'
import zh from '@/locales/zh'
import en from '@/locales/en'

export const LANG_KEY = 'c3api_lang'
export type AppLang = 'zh-CN' | 'en'

// 资源与默认命名空间在此导出：`as const` 让 i18next 类型系统（见 src/@types/i18next.d.ts）
// 校验 t() 的 key，并在传入 options 时推断插值变量类型。生成的 locales/*.ts 本身即 `as const`。
export const defaultNS = 'translation'
export const resources = {
  'zh-CN': { translation: zh },
  en: { translation: en },
} as const

// 运行时动态翻译 key（拼接 / 外部数据传入）的断言出口。静态字面量 key 直接传给 t 即可
// 获得完整类型校验；仅当字面量在静态期无法确定时才断言为 DynamicKey。
export type DynamicKey = ParseKeys

// 无插值变量的 key。i18next 27 起 t() 不带 options 时只接受这一子集（
// strictInterpolationChecks）：文案里带 {{var}} 的 key 必须把变量传进来，否则运行时会把
// 占位符原样透出给用户。
export type PlainKey = ParseKeysWithoutVariables<'translation'>

// 运行时拼出来的 key（模板字面量 / 外部数据）的断言出口：先按全集断言，再收窄到无变量子集。
// 两步断言是因为模板字面量类型与收窄后的联合类型不构成直接可比关系，单次断言会被 TS 拒绝。
// 用它等于**声明**「这个 key 的文案没有插值变量」；静态字面量 key 不需要它，直接传字面量仍受校验。
export const asPlainKey = (key: string): PlainKey => key as PlainKey

// 语言解析顺序：localStorage c3api_lang → navigator.language（zh 开头 → zh-CN，否则 en）→ 默认 zh-CN。
function detectLang(): AppLang {
  const saved = localStorage.getItem(LANG_KEY)
  if (saved === 'zh-CN' || saved === 'en') return saved
  return (navigator.language ?? '').toLowerCase().startsWith('zh') ? 'zh-CN' : 'en'
}

i18n.use(initReactI18next).init({
  resources,
  defaultNS,
  lng: detectLang(),
  fallbackLng: 'zh-CN',
  interpolation: { escapeValue: false },
  react: { useSuspense: false },
})

// 切换语言并持久化到 localStorage；react-i18next 自动触发全部已挂载组件重渲染。
export function setLang(lng: AppLang) {
  localStorage.setItem(LANG_KEY, lng)
  i18n.changeLanguage(lng)
}

export default i18n
