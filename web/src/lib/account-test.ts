// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 账号探测候选纯逻辑：与 domain.Template.FormatSupports/Serves 的资格口径一致。
import type { components } from './api/schema.d.ts'

export type ProbeTemplate = Pick<components['schemas']['Template'], 'SupportedFormats' | 'Models' | 'FormatModels' | 'ModelMapping'>
export type ProbeFormat = components['schemas']['AccountTestRequest']['format']

export const PROBE_FORMATS = ['openai-chat', 'openai-responses', 'openai-responses-ws', 'anthropic'] as const satisfies readonly ProbeFormat[]

export function probeFormats(template: ProbeTemplate | null | undefined): ProbeFormat[] {
  if (!template) return []
  return PROBE_FORMATS.filter(format => template.SupportedFormats.includes(format))
}

// FormatSupports：格式不受支持时恒 false；格式模型表已配置（含空表）时仅表内模型合格。
export function formatSupports(template: ProbeTemplate, format: ProbeFormat, model: string): boolean {
  if (!template.SupportedFormats.includes(format)) return false
  const formatModels = template.FormatModels ?? {}
  if (Object.hasOwn(formatModels, format)) return formatModels[format].includes(model)
  return true
}

export function hasWildcardModelSpace(template: ProbeTemplate): boolean {
  // 与后端 domain.Template.HasModelSpace 完全一致：任一容器非空（含“只含空列表”
  // 的 FormatModels map）即已配置模型空间，不再是通配。
  return (template.Models?.length ?? 0) === 0
    && Object.keys(template.FormatModels ?? {}).length === 0
    && Object.keys(template.ModelMapping ?? {}).length === 0
}

export function probeModelsFor(template: ProbeTemplate, format: ProbeFormat): { models: string[]; wildcard: boolean } {
  const formatModels = template.FormatModels ?? {}
  let candidates: string[]
  if (Object.hasOwn(formatModels, format)) {
    candidates = [...formatModels[format]]
  } else {
    candidates = [
      ...(template.Models ?? []),
      ...Object.values(formatModels).flat(),
      ...Object.keys(template.ModelMapping ?? {}),
    ]
  }
  const models = [...new Set(candidates.filter(model => formatSupports(template, format, model)))].sort()
  return { models, wildcard: hasWildcardModelSpace(template) }
}
