// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 账号探测候选纯逻辑：与 domain.Template.FormatSupports/Serves 的资格口径一致。
import type { components } from './api/schema.d.ts'

export type ProbeTemplate = Pick<components['schemas']['Template'], 'SupportedFormats' | 'Models' | 'FormatModels' | 'ModelMapping'>
export type ProbeFormat = components['schemas']['AccountTestRequest']['format']
export type ProbeImageMode = NonNullable<components['schemas']['AccountTestRequest']['image_mode']>

// UI 目标 ≠ wire format：images 的两个 mode 在界面上是两个独立目标，但都必须
// 映射为 `{format:"openai-images", image_mode:"generations"|"edits"}`。
export type ProbeTarget =
  | 'openai-chat'
  | 'openai-responses'
  | 'openai-responses-ws'
  | 'anthropic'
  | 'openai-images.generations'
  | 'openai-images.edits'
  | 'openai-search'

const PROBE_TARGETS = [
  { target: 'openai-chat', format: 'openai-chat' },
  { target: 'openai-responses', format: 'openai-responses' },
  { target: 'openai-responses-ws', format: 'openai-responses-ws' },
  { target: 'anthropic', format: 'anthropic' },
  { target: 'openai-images.generations', format: 'openai-images' },
  { target: 'openai-images.edits', format: 'openai-images' },
  { target: 'openai-search', format: 'openai-search' },
] as const satisfies readonly { target: ProbeTarget; format: ProbeFormat }[]

export function probeTargetFormat(target: ProbeTarget): ProbeFormat {
  switch (target) {
    case 'openai-images.generations':
    case 'openai-images.edits':
      return 'openai-images'
    default:
      return target
  }
}

export function probeTargetImageMode(target: ProbeTarget): ProbeImageMode | undefined {
  switch (target) {
    case 'openai-images.generations':
      return 'generations'
    case 'openai-images.edits':
      return 'edits'
    default:
      return undefined
  }
}

// 与后端 domain.ProbeMaxInputLen / ProbeMaxImageSourceBytes 同值。前端只做
// 提示与提交前拦截；最终判定仍在 service（UTF-8 字节，不是 JS 字符数）。
export const PROBE_MAX_INPUT_BYTES = 8192
export const PROBE_MAX_IMAGE_URL_BYTES = 6 * 1024 * 1024

export function probeUTF8Bytes(value: string): number {
  return new TextEncoder().encode(value).length
}

export function probeInputByteLength(raw: string): number {
  return probeUTF8Bytes(raw.trim())
}

export function probeImageURLByteLength(raw: string): number {
  return probeUTF8Bytes(raw.trim())
}

// 缺省/显式空串都表示“不发送 input”，由后端按格式取默认提示词。非空值
// 先 trim（与 service 同口径）。
export function probeInputValue(raw: string): string | undefined {
  const value = raw.trim()
  return value === '' ? undefined : value
}

// 与 service.validProbeImageURL 的 wire 形态一致：http(s) 绝对 URL，或
// data:image/<mime>;base64,<data>。字节上限由 probeImageURLByteLength 单独判定。
export function probeImageURLShapeValid(raw: string): boolean {
  const value = raw.trim()
  if (value === '') return false
  const lower = value.toLowerCase()
  if (lower.startsWith('data:')) {
    if (!lower.startsWith('data:image/')) return false
    const comma = value.indexOf(',')
    if (comma < 0) return false
    const meta = value.slice(0, comma).slice('data:'.length).toLowerCase()
    return meta.endsWith(';base64') && meta.length - ';base64'.length > 'image/'.length
  }
  try {
    const url = new URL(value)
    return (url.protocol === 'http:' || url.protocol === 'https:') && url.host !== ''
  } catch {
    return false
  }
}

// 请求体只携带真正有值的可选字段：input 缺省/null 才是后端默认值；显式空串
// 会被 service 判 400。images 的目标必须落成 `{format, image_mode}`，edits 再
// 附加 image_url；generations 绝不发送 image_url。
export function buildProbeRequest(args: {
  target: ProbeTarget
  model: string
  input: string
  imageURL: string
}): components['schemas']['AccountTestRequest'] {
  const imageMode = probeTargetImageMode(args.target)
  const body: components['schemas']['AccountTestRequest'] = {
    model: args.model.trim(),
    format: probeTargetFormat(args.target),
  }
  const input = probeInputValue(args.input)
  if (input !== undefined) body.input = input
  if (imageMode !== undefined) {
    body.image_mode = imageMode
    if (imageMode === 'edits') {
      const imageURL = args.imageURL.trim()
      if (imageURL !== '') body.image_url = imageURL
    }
  }
  return body
}

export function probeTargets(template: ProbeTemplate | null | undefined): ProbeTarget[] {
  if (!template) return []
  return PROBE_TARGETS
    .filter(item => template.SupportedFormats.includes(item.format))
    .map(item => item.target)
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

// 通配模板允许自由输入；受控 Combobox 的 value 必须跟随输入本身（而不是
// “输入是否在候选内”）。否则关闭弹层/清除受控值时 null 会把已输入的模型抹掉，
// 结果面板也随之失效。空串仍映射为 null（未选择）。
export function probeComboboxValue(model: string): string | null {
  return model || null
}
