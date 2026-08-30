// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

import { useEffect, useMemo, useRef, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { motion } from 'framer-motion'
import { Boxes, Pencil, Plus, Settings2, Trash2, X } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { api } from '@/App'
import { ApiError, ApiUnauthorized } from '@/lib/api/client'
import type { components } from '@/lib/api/schema'
import { BatchBar } from '@/components/batch-bar'
import { commaList, formatDateTime, truncate } from '@/components/fmt'
import { ListToolbar } from '@/components/list-toolbar'
import { Pagination } from '@/components/pagination'
import { SortableHeader, type SortOrder } from '@/components/sortable-header'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { Checkbox } from '@/components/ui/checkbox'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { Skeleton } from '@/components/ui/skeleton'
import { Switch } from '@/components/ui/switch'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { toast } from '@/components/ui/toast'

type Template = components['schemas']['Template']
type TemplateCreate = components['schemas']['TemplateCreate']
type TemplatePatch = components['schemas']['TemplatePatch']
type TemplateExt = components['schemas']['TemplateExt']
// 模板格式联合 = API SupportedFormats 元素类型（W1 扩展后为 4 值含 openai-responses-ws；
// 不用 RequestFormat 别名——usage 侧枚举保持 3 值，模板格式与其已分化）。
type TemplateFormat = components['schemas']['Template']['SupportedFormats'][number]
type TemplateCredentialType = NonNullable<Template['CredentialType']>

const FORMAT_LABELS: Record<TemplateFormat, string> = {
  'openai-chat': 'OpenAI Chat',
  'openai-responses': 'OpenAI Responses',
  'openai-responses-ws': 'OpenAI Responses (WS)',
  'openai-images': 'OpenAI Images',
  'openai-search': 'OpenAI Search (/v1/alpha/search)',
  anthropic: 'Anthropic',
}
const FORMATS = Object.keys(FORMAT_LABELS) as TemplateFormat[]

// 凭据类型（模板级：一个模板 = 一种号池，账号继承）
const CREDENTIAL_TYPES: TemplateCredentialType[] = ['api_key', 'responses-special', 'codex-oauth', 'codex-pat']
// 生态三类型：走 SDK 类型化端点；模板格式联动限制为 resp / resp-ws / images /
// search（service 校验前置——与 validateTemplate 白名单一致）
const ECO_CREDENTIAL_TYPES: TemplateCredentialType[] = ['responses-special', 'codex-oauth', 'codex-pat']
const ECO_FORMATS: TemplateFormat[] = ['openai-responses', 'openai-responses-ws', 'openai-images', 'openai-search']
const isCodexCredential = (v: string) => v === 'codex-oauth' || v === 'codex-pat'
const CREDENTIAL_BADGE_STYLES: Partial<Record<TemplateCredentialType, string>> = {
  'responses-special': 'bg-violet-500/10 text-violet-600 dark:bg-violet-400/10 dark:text-violet-400',
  'codex-oauth': 'bg-sky-500/10 text-sky-600 dark:bg-sky-400/10 dark:text-sky-400',
  'codex-pat': 'bg-amber-500/10 text-amber-600 dark:bg-amber-400/10 dark:text-amber-400',
}

type MappingMode = components['schemas']['ModelMappingEntry']['mode']
type BatchMappingMode = 'unchanged' | 'replace'
type ModelMappingEntry = components['schemas']['ModelMappingEntry']

function isMappingMode(v: string): v is MappingMode {
  return v === 'explicit' || v === 'implicit'
}
function isBatchMappingMode(v: string): v is BatchMappingMode {
  return v === 'unchanged' || v === 'replace'
}
function isModelMappingEntry(v: unknown): v is ModelMappingEntry {
  if (!v || typeof v !== 'object') return false
  const o = v as Record<string, unknown>
  return typeof o['mapped_model'] === 'string' && typeof o['mode'] === 'string' && isMappingMode(o['mode'] as string)
}

// —— 多格式表单状态（supported_formats/format_models；default_format/model_formats 已废弃） ——
interface FormatRow {
  format: TemplateFormat
  modelsText: string
}
interface MappingRow {
  alias: string
  mapped_model: string
  mode: MappingMode
}
interface FormState {
  name: string
  base_url: string
  credential_type: string // '' = 未指定（创建时后端兜底 api_key）
  supported_formats: TemplateFormat[]
  modelsText: string
  format_models: FormatRow[]
  model_mapping: MappingRow[]
  // 模板级图像 tool 剥离（生态类型；false = 未配置 = 关闭——保存时链式写 ext）
  strip_image_tools: boolean
}

const emptyForm = (): FormState => ({
  name: '',
  base_url: '',
  credential_type: '',
  supported_formats: [],
  modelsText: '',
  format_models: [],
  model_mapping: [],
  strip_image_tools: false,
})

function toForm(t: Template): FormState {
  const ct = t.CredentialType ?? ''
  return {
    name: t.Name ?? '',
    base_url: isCodexCredential(ct) ? '' : (t.BaseURL ?? ''),
    credential_type: ct,
    supported_formats: [...(t.SupportedFormats ?? [])],
    modelsText: (t.Models ?? []).join(', '),
    format_models: Object.entries(t.FormatModels ?? {}).map(([format, models]) => ({
      format: format as TemplateFormat,
      modelsText: (models ?? []).join(', '),
    })),
    model_mapping: Object.entries(t.ModelMapping ?? {}).map(([alias, entry]) => ({
      alias,
      mapped_model: entry.mapped_model,
      mode: entry.mode,
    })),
    // 编辑回显经 GET /templates/{id}/ext 拉取填充（见 dialog 挂载 effect）
    strip_image_tools: false,
  }
}

const splitList = (s: string) => s.split(',').map(x => x.trim()).filter(Boolean)

function formatModelsOf(f: FormState): Record<string, string[]> {
  const out: Record<string, string[]> = {}
  for (const r of f.format_models) {
    const models = splitList(r.modelsText)
    if (r.format && models.length) out[r.format] = models
  }
  return out
}

function mappingOf(rows: MappingRow[]): Record<string, ModelMappingEntry> {
  const out: Record<string, ModelMappingEntry> = {}
  for (const r of rows) {
    const alias = r.alias.trim()
    const target = r.mapped_model.trim()
    out[alias] = { mapped_model: target, mode: r.mode }
  }
  return out
}

function validateMappingRows(rows: MappingRow[], tr: (k: string, o?: Record<string, unknown>) => string): string | null {
  const seen = new Set<string>()
  for (const r of rows) {
    const aliasTrim = r.alias.trim()
    const targetTrim = r.mapped_model.trim()
    if (!aliasTrim || !targetTrim) return tr('templates.modelMappingValidation.incomplete')
    if (r.alias !== aliasTrim || r.mapped_model !== targetTrim) return tr('templates.modelMappingValidation.whitespace')
    if (seen.has(aliasTrim)) return tr('templates.modelMappingValidation.duplicate', { alias: aliasTrim })
    seen.add(aliasTrim)
    if (r.mode !== 'explicit' && r.mode !== 'implicit') return tr('templates.modelMappingValidation.incomplete')
  }
  return null
}

function toBody(f: FormState): TemplateCreate {
  const format_models = formatModelsOf(f)
  const model_mapping = mappingOf(f.model_mapping)
  const ct = (f.credential_type || 'api_key') as string
  return {
    name: f.name.trim(),
    base_url: isCodexCredential(ct) ? '' : f.base_url.trim(),
    credential_type: ct as TemplateCreate['credential_type'],
    supported_formats: f.supported_formats,
    models: splitList(f.modelsText),
    format_models: Object.keys(format_models).length ? format_models : undefined,
    model_mapping,
  }
}

// 批量更新：仅包含已填写的字段（TemplatePatch 子集）；model_mapping 由 batchMappingMode 控制
function toPatch(f: FormState, batchMappingMode: BatchMappingMode): TemplatePatch {
  const patch: TemplatePatch = {}
  if (f.name.trim()) patch.name = f.name.trim()
  if (f.base_url.trim()) patch.base_url = f.base_url.trim()
  if (f.supported_formats.length) patch.supported_formats = f.supported_formats
  const models = splitList(f.modelsText)
  if (models.length) patch.models = models
  const format_models = formatModelsOf(f)
  if (Object.keys(format_models).length) patch.format_models = format_models
  if (batchMappingMode === 'replace') {
    patch.model_mapping = mappingOf(f.model_mapping)
  }
  return patch
}

function FormatBadge({ format }: { format?: string }) {
  return <Badge variant="outline">{format ? (FORMAT_LABELS[format as TemplateFormat] ?? format) : '—'}</Badge>
}

// 凭据类型徽章：api_key 灰显；生态三类型彩色（号池走 SDK 类型化端点）
function CredentialTypeBadge({ credentialType }: { credentialType?: string }) {
  const { t } = useTranslation()
  const ct = (credentialType ?? 'api_key') as TemplateCredentialType
  const style = CREDENTIAL_BADGE_STYLES[ct]
  return style
    ? <Badge className={style}>{t(`templates.credentialType.${ct}`)}</Badge>
    : <Badge variant="outline">{t(`templates.credentialType.${ct}`)}</Badge>
}

// —— 表单区（创建/编辑与批量更新共用；batch 模式下所有字段可选） ——
function FormFields({
  form,
  setForm,
  error,
  batch = false,
  batchCodex = false,
  batchMappingMode = 'unchanged',
  setBatchMappingMode,
}: {
  form: FormState
  setForm: (updater: (f: FormState) => FormState) => void
  error?: string | null
  batch?: boolean // 批量更新隐藏凭据类型（TemplatePatch 不支持类型变更，评审 M-2）
  batchCodex?: boolean
  batchMappingMode?: BatchMappingMode
  setBatchMappingMode?: (v: BatchMappingMode) => void
}) {
  const { t } = useTranslation()

  const toggleFormat = (f: TemplateFormat) => {
    setForm(prev => {
      const on = prev.supported_formats.includes(f)
      const supported_formats = on ? prev.supported_formats.filter(x => x !== f) : [...prev.supported_formats, f]
      // 取消勾选的格式同步移除其 format_models 行（后端要求 key ∈ supported_formats）
      const format_models = on ? prev.format_models.filter(r => r.format !== f) : prev.format_models
      return { ...prev, supported_formats, format_models }
    })
  }
  const setFormatRow = (i: number, patch: Partial<FormatRow>) =>
    setForm(f => ({ ...f, format_models: f.format_models.map((r, j) => (j === i ? { ...r, ...patch } : r)) }))
  const removeFormatRow = (i: number) =>
    setForm(f => ({ ...f, format_models: f.format_models.filter((_, j) => j !== i) }))
  const addFormatRow = () =>
    setForm(f => ({
      ...f,
      format_models: [...f.format_models, { format: f.supported_formats[0] ?? 'openai-chat', modelsText: '' }],
    }))
  const setMappingRow = (i: number, patch: Partial<MappingRow>) =>
    setForm(f => ({ ...f, model_mapping: f.model_mapping.map((r, j) => (j === i ? { ...r, ...patch } : r)) }))
  const removeMappingRow = (i: number) =>
    setForm(f => ({ ...f, model_mapping: f.model_mapping.filter((_, j) => j !== i) }))
  const addMappingRow = () => setForm(f => ({ ...f, model_mapping: [...f.model_mapping, { alias: '', mapped_model: '', mode: 'explicit' }] }))

  // 格式行下拉选项 = 已选 supported_formats
  const formatOptions: Record<string, string> = {}
  for (const f of form.supported_formats) formatOptions[f] = FORMAT_LABELS[f]

  return (
    <div className="space-y-3">
      <div className="space-y-1.5">
        <Label htmlFor="tpl-name">{t('templates.nameLabel')}</Label>
        <Input
          id="tpl-name"
          value={form.name}
          placeholder={t('templates.namePlaceholder')}
          onChange={e => setForm(f => ({ ...f, name: e.target.value }))}
        />
      </div>
      {batch ? (
        batchCodex ? (
          <p className="text-xs text-muted-foreground">{t('templates.batchBaseUrlCodexDisabled')}</p>
        ) : (
          <div className="space-y-1.5">
            <Label htmlFor="tpl-base">BaseURL</Label>
            <Input
              id="tpl-base"
              value={form.base_url}
              placeholder="https://api.openai.com"
              onChange={e => setForm(f => ({ ...f, base_url: e.target.value }))}
            />
            <p className="text-xs text-muted-foreground">{t('templates.baseUrlOptional')}</p>
          </div>
        )
      ) : isCodexCredential(form.credential_type) ? (
        <p className="text-xs text-muted-foreground">{t('templates.baseUrlCodexHidden')}</p>
      ) : (
        <div className="space-y-1.5">
          <Label htmlFor="tpl-base">BaseURL</Label>
          <Input
            id="tpl-base"
            value={form.base_url}
            placeholder="https://api.openai.com"
            onChange={e => setForm(f => ({ ...f, base_url: e.target.value }))}
          />
          <p className="text-xs text-muted-foreground">{t('templates.baseUrlOptional')}</p>
        </div>
      )}

      {/* credential_type：一个模板 = 一种号池（非 api_key 类型走 SDK 内置端点，base_url 忽略） */}
      {!batch && (
        <>
        <div className="space-y-1.5">
          <Label htmlFor="tpl-cred-type">{t('templates.credentialTypeLabel')}</Label>
          <Select
            items={Object.fromEntries(CREDENTIAL_TYPES.map(ct => [ct, t(`templates.credentialType.${ct}`)]))}
            value={form.credential_type || 'api_key'}
            onValueChange={v => setForm(f => {
              // 生态三类型：supported_formats 联动限制为 resp / resp-ws（service 校验前置）
              const isCodex = isCodexCredential(v)
              if (!ECO_CREDENTIAL_TYPES.includes(v as TemplateCredentialType)) return { ...f, credential_type: v, base_url: isCodex ? '' : f.base_url }
              const supported_formats = f.supported_formats.filter(x => ECO_FORMATS.includes(x))
              const format_models = f.format_models.filter(r => ECO_FORMATS.includes(r.format))
              return {
                ...f,
                credential_type: v,
                base_url: isCodex ? '' : f.base_url,
                supported_formats: supported_formats.length ? supported_formats : [...ECO_FORMATS],
                format_models,
              }
            })}
          >
            <SelectTrigger id="tpl-cred-type" className="w-48"><SelectValue /></SelectTrigger>
            <SelectContent>
              {CREDENTIAL_TYPES.map(ct => (
                <SelectItem key={ct} value={ct} label={t(`templates.credentialType.${ct}`)}>{t(`templates.credentialType.${ct}`)}</SelectItem>
              ))}
            </SelectContent>
          </Select>
          {ECO_CREDENTIAL_TYPES.includes(form.credential_type as TemplateCredentialType) && (
            <p className="text-xs text-muted-foreground">{t('templates.ecoFormatHint')}</p>
          )}
        </div>
        {/* 模板级图像 tool 剥离（生态类型；与行操作「扩展配置」弹窗同一数据源——
            此处表单内直配，保存时链式写 ext。开关两态：false = 未配置 = 关闭，
            与 null 语义等价（后端 nullable 仅区分「未配置」历史）） */}
        {ECO_CREDENTIAL_TYPES.includes(form.credential_type as TemplateCredentialType) && (
          <div className="flex items-center justify-between gap-2">
            <div className="space-y-0.5">
              <Label>{t('templates.ext.stripImageTools')}</Label>
              <p className="text-xs text-muted-foreground">{t('templates.ext.stripImageToolsHint')}</p>
            </div>
            <Switch
              checked={form.strip_image_tools === true}
              onCheckedChange={c => setForm(f => ({ ...f, strip_image_tools: c }))}
              aria-label={t('templates.ext.stripImageTools')}
            />
          </div>
        )}
        </>
      )}

      {/* supported_formats：chips 多选（非空校验）；生态类型只显示白名单格式（resp / resp-ws / images / search） */}
      <div className="space-y-1.5">
        <Label>{t('templates.supportedFormatsLabel')}</Label>
        <div className="flex flex-wrap gap-1.5">
          {(ECO_CREDENTIAL_TYPES.includes(form.credential_type as TemplateCredentialType) ? ECO_FORMATS : FORMATS).map(f => {
            const on = form.supported_formats.includes(f)
            return (
              <Button
                key={f}
                type="button"
                size="sm"
                variant={on ? 'default' : 'outline'}
                aria-pressed={on}
                className={on ? 'rounded-full' : 'rounded-full border-muted-foreground/30'}
                onClick={() => toggleFormat(f)}
              >
                {FORMAT_LABELS[f]}
              </Button>
            )
          })}
        </div>
        {error && <p className="text-sm text-destructive">{error}</p>}
      </div>

      <div className="space-y-1.5">
        <Label htmlFor="tpl-models">{t('templates.modelsLabel')}</Label>
        <Input
          id="tpl-models"
          value={form.modelsText}
          placeholder="gpt-4o, gpt-4o-mini"
          onChange={e => setForm(f => ({ ...f, modelsText: e.target.value }))}
        />
      </div>

      {/* format_models 动态行：格式 → 模型列表（逗号分隔）；未配置的格式支持全部模型 */}
      <div className="space-y-1.5">
        <Label>{t('templates.formatModelsLabel')}</Label>
        <p className="text-xs text-muted-foreground">{t('templates.formatModelsHint')}</p>
        <div className="space-y-1.5">
          {form.format_models.map((row, i) => (
            <div key={i} className="flex items-center gap-1.5">
              <Select
                items={formatOptions}
                value={row.format}
                onValueChange={v => setFormatRow(i, { format: v as TemplateFormat })}
              >
                <SelectTrigger className="w-40"><SelectValue /></SelectTrigger>
                <SelectContent>
                  {form.supported_formats.map(f => (
                    <SelectItem key={f} value={f} label={FORMAT_LABELS[f]}>{FORMAT_LABELS[f]}</SelectItem>
                  ))}
                </SelectContent>
              </Select>
              <Input
                className="flex-1"
                placeholder={t('templates.modelsListPlaceholder')}
                value={row.modelsText}
                onChange={e => setFormatRow(i, { modelsText: e.target.value })}
              />
              <Button variant="ghost" size="icon-sm" title={t('templates.deleteRow')} onClick={() => removeFormatRow(i)}>
                <X />
              </Button>
            </div>
          ))}
          <Button variant="outline" size="sm" disabled={form.supported_formats.length === 0} onClick={addFormatRow}>
            <Plus /> {t('templates.addFormatRow')}
          </Button>
        </div>
      </div>

      {/* model_mapping 动态行：alias → mapped_model + mode */}
      <div className="space-y-1.5">
        <div className="flex flex-wrap items-center justify-between gap-2">
          <Label>{t('templates.modelMappingLabel')}</Label>
          {batch && setBatchMappingMode && (
            <div className="flex flex-wrap items-center gap-1.5">
              <Label className="text-xs font-normal text-muted-foreground">{t('templates.modelMappingBatchModeLabel')}</Label>
              <Select value={batchMappingMode} onValueChange={v => { if (isBatchMappingMode(v)) setBatchMappingMode(v) }}>
                <SelectTrigger className="h-7 w-32"><SelectValue /></SelectTrigger>
                <SelectContent>
                  <SelectItem value="unchanged" label={t('templates.modelMappingBatchMode.unchanged')}>{t('templates.modelMappingBatchMode.unchanged')}</SelectItem>
                  <SelectItem value="replace" label={t('templates.modelMappingBatchMode.replace')}>{t('templates.modelMappingBatchMode.replace')}</SelectItem>
                </SelectContent>
              </Select>
            </div>
          )}
        </div>
        {batch && batchMappingMode === 'unchanged' ? (
          <p className="text-xs text-muted-foreground">{t('templates.modelMappingBatchHint')}</p>
        ) : (
          <>
            <p className="text-xs text-muted-foreground">{batch ? t('templates.modelMappingBatchReplaceHint') : t('templates.modelMappingHint')}</p>
            <div className="space-y-1.5">
              {form.model_mapping.map((row, i) => (
                <div key={i} className="flex flex-wrap items-center gap-1.5">
                  <Input
                    className="flex-1 min-w-[110px] basis-[110px]"
                    placeholder={t('templates.clientModelPlaceholder')}
                    value={row.alias}
                    onChange={e => setMappingRow(i, { alias: e.target.value })}
                    aria-label={t('templates.clientModelPlaceholder')}
                  />
                  <span className="shrink-0 text-muted-foreground">→</span>
                  <Input
                    className="flex-1 min-w-[110px] basis-[110px]"
                    placeholder={t('templates.upstreamModelPlaceholder')}
                    value={row.mapped_model}
                    onChange={e => setMappingRow(i, { mapped_model: e.target.value })}
                    aria-label={t('templates.upstreamModelPlaceholder')}
                  />
                  <Select value={row.mode} onValueChange={v => { if (isMappingMode(v)) setMappingRow(i, { mode: v }) }}>
                    <SelectTrigger className="w-28 shrink-0" aria-label={t('templates.modelMappingModeLabel')}><SelectValue /></SelectTrigger>
                    <SelectContent>
                      <SelectItem value="explicit" label={t('templates.modelMappingMode.explicit')}>{t('templates.modelMappingMode.explicit')}</SelectItem>
                      <SelectItem value="implicit" label={t('templates.modelMappingMode.implicit')}>{t('templates.modelMappingMode.implicit')}</SelectItem>
                    </SelectContent>
                  </Select>
                  <Button variant="ghost" size="icon-sm" title={t('templates.deleteRow')} aria-label={t('templates.deleteRow')} onClick={() => removeMappingRow(i)}>
                    <X />
                  </Button>
                </div>
              ))}
              <Button variant="outline" size="sm" onClick={addMappingRow}>
                <Plus /> {t('templates.addMapping')}
              </Button>
            </div>
          </>
        )}
      </div>
    </div>
  )
}

export default function Templates() {
  const { t: tr } = useTranslation()
  const qc = useQueryClient()

  // —— 列表状态：分页/筛选/排序 ——
  const [offset, setOffset] = useState(0)
  const [limit, setLimit] = useState(20)
  const [name, setName] = useState('')
  const [activeSort, setActiveSort] = useState<string | null>(null) // null = 无主动排序（默认 id desc）
  const [order, setOrder] = useState<SortOrder>('desc')
  const [debouncedName, setDebouncedName] = useState('')
  const [selected, setSelected] = useState<number[]>([])

  // 搜索输入防抖 300ms
  useEffect(() => {
    const timer = setTimeout(() => setDebouncedName(name), 300)
    return () => clearTimeout(timer)
  }, [name])

  const { data, isLoading, isError, error } = useQuery({
    queryKey: ['templates', { limit, offset, name: debouncedName, sort: activeSort ?? 'id', order }],
    queryFn: () => api.listTemplates({ limit, offset, name: debouncedName || undefined, sort: activeSort ?? 'id', order }),
  })
  const rows = useMemo(() => data?.rows ?? [], [data?.rows])

  // 行数据变化后清理已不存在的勾选
  useEffect(() => {
    const ids = new Set(rows.map(r => r.ID))
    setSelected(s => {
      const filtered = s.filter(id => ids.has(id))
      if (filtered.length === s.length && filtered.every((v, i) => v === s[i])) return s
      return filtered
    })
  }, [rows])

  // 筛选/排序/翻页变化 → 重置 offset 并清空选择
  const onNameChange = (v: string) => { setName(v); setOffset(0); setSelected([]) }
  // 列头三态：新列 → 降序；同列降序 → 升序；同列升序 → 取消（回默认 id desc）
  const onColumnToggle = (col: string) => {
    setOffset(0)
    setSelected([])
    if (activeSort !== col) {
      setActiveSort(col)
      setOrder('desc')
    } else if (order === 'desc') {
      setOrder('asc')
    } else {
      setActiveSort(null)
      setOrder('desc')
    }
  }
  const onOffsetChange = (o: number) => { setOffset(o); setSelected([]) }
  // 每页条数变化 → 重置 offset 并清空选择。
  const onLimitChange = (l: number) => { setLimit(l); setOffset(0); setSelected([]) }

  // —— 创建/编辑对话框 ——
  const [dialogOpen, setDialogOpen] = useState(false)
  const [editing, setEditing] = useState<Template | null>(null)
  const [form, setForm] = useState<FormState>(emptyForm())
  const [validationMsg, setValidationMsg] = useState<string | null>(null)
  // 批量更新时若选中任一 codex 模板则禁用 BaseURL（混合/全 Codex 均不发送已知无效请求；api_key/responses-special 保持可编辑；stale 行 fail-closed）
  const isBatchCodex = useMemo(() => {
    if (selected.length === 0) return false
    return selected.some(id => {
      const t = rows.find(r => r.ID === id)
      if (!t) return true
      return isCodexCredential(t.CredentialType as string)
    })
  }, [selected, rows])
  useEffect(() => {
    if (isBatchCodex && form.base_url !== '') {
      setForm(f => ({ ...f, base_url: '' }))
    }
  }, [isBatchCodex, form.base_url])

  // —— 批量更新对话框 ——
  const [batchOpen, setBatchOpen] = useState(false)
  const [batchMappingMode, setBatchMappingMode] = useState<BatchMappingMode>('unchanged')
  const batchResolve = useRef<((r: 'cancelled' | 'submitted') => void) | null>(null)
  // —— 删除确认 ——
  const [deleting, setDeleting] = useState<Template | null>(null)

  // —— 扩展配置（生态三类型；GET 404 = 无 ext 行 → 空表单；strip_image_tools 三态：未配置/开/关） ——
  const [extTarget, setExtTarget] = useState<Template | null>(null)
  const [extStrip, setExtStrip] = useState<boolean | null>(null)
  const extQ = useQuery({
    queryKey: ['template-ext', extTarget?.ID],
    queryFn: async () => {
      try {
        return await api.getTemplateExt(extTarget!.ID)
      } catch (e) {
        if (e instanceof ApiError && e.status === 404) return null // 无 ext 行 = 空表单（null 是合法数据，undefined 会触发 react-query 数据未定义错误）
        throw e
      }
    },
    enabled: !!extTarget,
  })
  const openExt = (t: Template) => {
    setExtTarget(t)
    setExtStrip(null)
  }
  useEffect(() => {
    if (!extTarget || extQ.isLoading || extQ.data === null) return
    const target = extQ.data?.strip_image_tools ?? null
    setExtStrip(prev => (prev === target ? prev : target))
  }, [extTarget?.ID, extQ.isLoading, extQ.data])
  const extSave = useMutation({
    mutationFn: () => {
      const tpl = extTarget!
      const body: TemplateExt = {
        template_id: tpl.ID,
        credential_type: tpl.CredentialType as TemplateExt['credential_type'],
        strip_image_tools: extStrip,
      }
      return api.putTemplateExt(tpl.ID, body)
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['templates'] })
      setExtTarget(null)
      toast.add({ title: tr('templates.ext.saveSuccess'), type: 'success' })
    },
  })

  const openCreate = () => {
    setEditing(null)
    setForm(emptyForm())
    setValidationMsg(null)
    setDialogOpen(true)
  }
  const openEdit = (t: Template) => {
    setEditing(t)
    setForm(toForm(t))
    setValidationMsg(null)
    setDialogOpen(true)
  }
  // 编辑回显：生态类型模板拉 ext 填充 strip_image_tools（404 = 无 ext 行 → 未配置）
  const templateExtEcho = useQuery({
    queryKey: ['template-ext-echo', editing?.ID],
    queryFn: async () => {
      try {
        return await api.getTemplateExt(editing!.ID)
      } catch (e) {
        if (e instanceof ApiError && e.status === 404) return null // 无 ext 行 = 未配置
        throw e
      }
    },
    enabled: !!editing && dialogOpen && ECO_CREDENTIAL_TYPES.includes(editing?.CredentialType as TemplateCredentialType),
  })
  useEffect(() => {
    if (!editing || templateExtEcho.isLoading) return
    const target = templateExtEcho.data?.strip_image_tools ?? false
    setForm(f => (f.strip_image_tools === target ? f : { ...f, strip_image_tools: target }))
  }, [editing?.ID, templateExtEcho.isLoading, templateExtEcho.data])
  // BatchBar 的 onUpdate 返回 promise：对话框关闭（提交成功/取消）时 resolve。
  const closeBatchUpdate = (r: 'cancelled' | 'submitted' = 'cancelled') => {
    setBatchOpen(false)
    setValidationMsg(null)
    batchResolve.current?.(r)
    batchResolve.current = null
  }
  const openBatchUpdate = () => {
    setForm(emptyForm())
    setValidationMsg(null)
    setBatchMappingMode('unchanged')
    setBatchOpen(true)
    return new Promise<'cancelled' | 'submitted'>(resolve => {
      batchResolve.current = resolve
    })
  }

  // 生态类型保存链式写 ext（strip_image_tools 三态）；api_key 类型无 ext 行
  const save = useMutation({
    mutationFn: async (f: FormState) => {
      const ct = (f.credential_type || 'api_key') as TemplateCredentialType
      let id = editing?.ID
      if (!editing) {
        id = (await api.createTemplate(toBody(f))).ID
      } else {
        await api.updateTemplate(editing.ID, toBody(f))
      }
      if (id && ECO_CREDENTIAL_TYPES.includes(ct)) {
        const body: TemplateExt = {
          template_id: id,
          credential_type: ct as TemplateExt['credential_type'],
          strip_image_tools: f.strip_image_tools,
        }
        await api.putTemplateExt(id, body)
      }
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['templates'] })
      setDialogOpen(false)
    },
  })
  const remove = useMutation({
    mutationFn: (id: number) => api.deleteTemplate(id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['templates'] })
      setDeleting(null)
      // 删除的是当前页最后一行时回退一页
      if (rows.length === 1 && offset > 0) setOffset(offset - limit)
    },
  })
  const batchDelete = useMutation({
    mutationFn: (ids: number[]) => api.deleteTemplatesBatch(ids),
    onSuccess: (_res, ids) => {
      qc.invalidateQueries({ queryKey: ['templates'] })
      setSelected([])
      // 当前页被删空时回到最后有效页
      const after = (data?.total ?? 0) - ids.length
      if (offset > 0 && offset >= after) setOffset(Math.max(0, after - (after % limit)))
    },
  })
  const batchUpdate = useMutation({
    mutationFn: ({ ids, fields }: { ids: number[]; fields: TemplatePatch }) => api.updateTemplatesBatch(ids, fields),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['templates'] })
      closeBatchUpdate('submitted')
      setSelected([])
    },
    // onError 不 resolve：对话框保持打开就地展示错误，取消时由 closeBatchUpdate resolve
  })

  const submit = () => {
    setValidationMsg(null)
    if (!form.name.trim() || form.supported_formats.length === 0) {
      setValidationMsg(tr('templates.formRequired'))
      return
    }
    const mappingErr = validateMappingRows(form.model_mapping, tr)
    if (mappingErr) {
      setValidationMsg(mappingErr)
      return
    }
    save.mutate(form)
  }

  const submitBatch = () => {
    setValidationMsg(null)
    if (batchMappingMode === 'replace') {
      const mappingErr = validateMappingRows(form.model_mapping, tr)
      if (mappingErr) {
        setValidationMsg(mappingErr)
        return
      }
    }
    const fields = toPatch(form, batchMappingMode)
    if (isBatchCodex) delete (fields as Record<string, unknown>).base_url
    if (Object.keys(fields).length === 0) {
      setValidationMsg(tr('templates.batchUpdateEmpty'))
      return
    }
    batchUpdate.mutate({ ids: selected, fields })
  }

  const errMsg = (e: unknown) => (e instanceof ApiUnauthorized ? null : (e as Error)?.message)

  const toggleRow = (id: number, on: boolean) => setSelected(s => (on ? [...s, id] : s.filter(x => x !== id)))
  const allChecked = rows.length > 0 && rows.every(r => selected.includes(r.ID))
  const someChecked = rows.some(r => selected.includes(r.ID)) && !allChecked

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-semibold tracking-tight">{tr('templates.title')}</h1>
          <p className="text-sm text-muted-foreground">{tr('templates.subtitle')}</p>
        </div>
        <Button onClick={openCreate}><Plus /> {tr('templates.new')}</Button>
      </div>

      <ListToolbar
        name={name}
        onNameChange={onNameChange}
      />

      <BatchBar
        selected={selected}
        onClear={() => setSelected([])}
        onDelete={() => batchDelete.mutate(selected)}
        onUpdate={openBatchUpdate}
      />
      {batchDelete.isError && errMsg(batchDelete.error) && (
        <p className="text-sm text-destructive">{errMsg(batchDelete.error)}</p>
      )}
      {batchUpdate.isError && errMsg(batchUpdate.error) && (
        <p className="text-sm text-destructive">{errMsg(batchUpdate.error)}</p>
      )}

      {isError ? (
        <p className="text-sm text-destructive">{tr('common.loadFailed', { message: (error as Error).message })}</p>
      ) : isLoading ? (
        <div className="space-y-2">
          {Array.from({ length: 4 }).map((_, i) => <Skeleton key={i} className="h-12" />)}
        </div>
      ) : rows.length === 0 ? (
        <motion.div initial={{ opacity: 0, y: 12 }} animate={{ opacity: 1, y: 0 }} transition={{ duration: 0.25 }}>
          <Card className="flex flex-col items-center gap-2 py-12 text-muted-foreground">
            <Boxes className="size-10" />
            <p className="font-medium">{debouncedName ? tr('templates.noResultsTitle') : tr('templates.emptyTitle')}</p>
            <p className="text-sm">{debouncedName ? tr('templates.noResultsDesc') : tr('templates.emptyDesc')}</p>
            {debouncedName ? (
              <Button className="mt-2" variant="outline" onClick={() => onNameChange('')}>{tr('list.reset')}</Button>
            ) : (
              <Button className="mt-2" onClick={openCreate}><Plus /> {tr('templates.new')}</Button>
            )}
          </Card>
        </motion.div>
      ) : (
        <>
          <div className="overflow-hidden rounded-lg">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead className="w-10">
                    <Checkbox
                      checked={allChecked}
                      indeterminate={someChecked}
                      onCheckedChange={(c: boolean) => setSelected(c ? rows.map(r => r.ID) : [])}
                      aria-label={tr('list.selected', { count: rows.length })}
                    />
                  </TableHead>
                  <SortableHeader field="id" label="ID" active={activeSort === 'id'} order={order} onToggle={onColumnToggle} />
                  <SortableHeader field="name" label={tr('templates.table.name')} active={activeSort === 'name'} order={order} onToggle={onColumnToggle} />
                  <SortableHeader field="base_url" label="BaseURL" active={activeSort === 'base_url'} order={order} onToggle={onColumnToggle} />
                  <TableHead>{tr('templates.table.credentialType')}</TableHead>
                  <TableHead>{tr('templates.table.supportedFormats')}</TableHead>
                  <TableHead>{tr('templates.table.models')}</TableHead>
                  <TableHead>{tr('templates.table.formatModels')}</TableHead>
                  <TableHead>{tr('templates.table.modelMapping')}</TableHead>
                  <SortableHeader field="created_at" label={tr('templates.table.createdAt')} active={activeSort === 'created_at'} order={order} onToggle={onColumnToggle} />
                  <TableHead className="text-right">{tr('templates.table.actions')}</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody className="[&_td]:py-3">
                {rows.map(t => {
                  const models = commaList(t.Models)
                  const formats = Object.entries(t.FormatModels ?? {})
                  const mappings = Object.entries(t.ModelMapping ?? {})
                  return (
                    <TableRow key={t.ID} data-state={selected.includes(t.ID) ? 'selected' : undefined}>
                      <TableCell>
                        <Checkbox
                          checked={selected.includes(t.ID)}
                          onCheckedChange={(c: boolean) => toggleRow(t.ID, c)}
                          aria-label={t.Name ?? String(t.ID)}
                        />
                      </TableCell>
                      <TableCell className="tabular-nums">{t.ID}</TableCell>
                      <TableCell className="max-w-36 truncate" title={t.Name}>{t.Name}</TableCell>
                      <TableCell className="max-w-52 truncate font-mono text-xs" title={t.BaseURL}>{t.BaseURL}</TableCell>
                      <TableCell>
                        <CredentialTypeBadge credentialType={t.CredentialType} />
                      </TableCell>
                      <TableCell>
                        <div className="flex max-w-44 flex-wrap gap-1">
                          {(t.SupportedFormats ?? []).map(f => <FormatBadge key={f} format={f} />)}
                        </div>
                      </TableCell>
                      <TableCell className="max-w-40 truncate" title={models.full || undefined}>{models.text}</TableCell>
                      <TableCell>
                        {formats.length === 0 ? '—' : (
                          <div className="flex max-w-56 flex-wrap gap-1">
                            {formats.slice(0, 3).map(([f, ms]) => {
                              const label = FORMAT_LABELS[f as TemplateFormat] ?? f
                              const list = (ms ?? []).join(', ')
                              return (
                                <Badge key={f} variant="outline" className="font-mono text-xs" title={`${label}: ${list}`}>
                                  {truncate(label, 10)}:{truncate(list, 12)}
                                </Badge>
                              )
                            })}
                            {formats.length > 3 && <Badge variant="outline">+{formats.length - 3}</Badge>}
                          </div>
                        )}
                      </TableCell>
                      <TableCell>
                        {mappings.length === 0 ? '—' : (
                          <div className="flex max-w-64 flex-wrap gap-1">
                            {mappings.slice(0, 3).map(([k, raw]) => {
                              if (!isModelMappingEntry(raw)) return null
                              const e = raw
                              return (
                                <Badge key={k} variant="outline" className="font-mono text-xs" title={`${k} → ${e.mapped_model} (${e.mode})`}>
                                  {truncate(k, 10)}→{truncate(e.mapped_model, 10)}
                                  <span className="ml-1 rounded bg-muted px-1 text-[10px] font-sans">{tr(`templates.modelMappingMode.${e.mode}`)}</span>
                                </Badge>
                              )
                            })}
                            {mappings.length > 3 && <Badge variant="outline">+{mappings.length - 3}</Badge>}
                          </div>
                        )}
                      </TableCell>
                      <TableCell className="text-xs text-muted-foreground">{formatDateTime(t.CreatedAt)}</TableCell>
                      <TableCell className="text-right">
                        <div className="flex justify-end gap-1">
                          {ECO_CREDENTIAL_TYPES.includes(t.CredentialType as TemplateCredentialType) && (
                            <Button variant="ghost" size="icon-sm" title={tr('templates.ext.button')} aria-label={tr('templates.ext.button')} onClick={() => openExt(t)}>
                              <Settings2 />
                            </Button>
                          )}
                          <Button variant="ghost" size="icon-sm" title={tr('common.edit')} aria-label={tr('common.edit')} onClick={() => openEdit(t)}>
                            <Pencil />
                          </Button>
                          <Button
                            variant="ghost"
                            size="icon-sm"
                            className="text-destructive"
                            title={tr('common.delete')}
                            aria-label={tr('common.delete')}
                            onClick={() => setDeleting(t)}
                          >
                            <Trash2 />
                          </Button>
                        </div>
                      </TableCell>
                    </TableRow>
                  )
                })}
              </TableBody>
            </Table>
          </div>
          <Pagination total={data?.total ?? 0} limit={limit} offset={offset} onOffsetChange={onOffsetChange} onLimitChange={onLimitChange} />
        </>
      )}

      {/* —— 创建/编辑对话框 —— */}
      <Dialog open={dialogOpen} onOpenChange={o => { setDialogOpen(o); if (!o) setValidationMsg(null) }}>
        <DialogContent className="sm:max-w-lg">
          <DialogHeader>
            <DialogTitle>{editing ? tr('templates.editTitle', { id: editing.ID }) : tr('templates.newTitle')}</DialogTitle>
            <DialogDescription>{tr('templates.dialogDesc')}</DialogDescription>
          </DialogHeader>
          <div className="space-y-3">
            <FormFields form={form} setForm={setForm} error={validationMsg} />
            {save.isError && errMsg(save.error) && (
              <p className="text-sm text-destructive">{errMsg(save.error)}</p>
            )}
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setDialogOpen(false)}>{tr('common.cancel')}</Button>
            <Button onClick={submit} disabled={save.isPending}>
              {save.isPending ? tr('common.saving') : editing ? tr('common.saveChanges') : tr('common.create')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* —— 批量更新对话框 —— */}
      <Dialog open={batchOpen} onOpenChange={o => { if (!o) closeBatchUpdate() }}>
        <DialogContent className="sm:max-w-lg">
          <DialogHeader>
            <DialogTitle>{tr('templates.batchUpdateTitle', { count: selected.length })}</DialogTitle>
            <DialogDescription>{tr('templates.batchUpdateDesc')}</DialogDescription>
          </DialogHeader>
          <div className="space-y-3">
            <FormFields form={form} setForm={setForm} batch batchCodex={isBatchCodex} batchMappingMode={batchMappingMode} setBatchMappingMode={setBatchMappingMode} />
            {validationMsg && <p className="text-sm text-destructive">{validationMsg}</p>}
            {batchUpdate.isError && errMsg(batchUpdate.error) && (
              <p className="text-sm text-destructive">{errMsg(batchUpdate.error)}</p>
            )}
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => closeBatchUpdate()}>{tr('common.cancel')}</Button>
            <Button onClick={submitBatch} disabled={batchUpdate.isPending}>
              {batchUpdate.isPending ? tr('common.saving') : tr('common.saveChanges')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* —— 扩展配置（生态三类型模板；strip_image_tools 三态：未配置/开/关） —— */}
      <Dialog open={!!extTarget} onOpenChange={o => { if (!o && !extSave.isPending) setExtTarget(null) }}>
        <DialogContent className="sm:max-w-sm">
          <DialogHeader>
            <DialogTitle>{tr('templates.ext.title', { id: extTarget?.ID })}</DialogTitle>
            <DialogDescription>{tr('templates.ext.desc')}</DialogDescription>
          </DialogHeader>
          <div className="space-y-3">
            <div className="space-y-1.5">
              <Label>{tr('templates.ext.credentialType')}</Label>
              {extTarget && <CredentialTypeBadge credentialType={extTarget.CredentialType} />}
            </div>
            <div className="flex items-center justify-between gap-2">
              <div className="space-y-0.5">
                <Label>{tr('templates.ext.stripImageTools')}</Label>
                <p className="text-xs text-muted-foreground">{tr('templates.ext.stripImageToolsHint')}</p>
              </div>
              <Switch
                checked={extStrip === true}
                disabled={extQ.isLoading || extSave.isPending}
                onCheckedChange={c => setExtStrip(c)}
                aria-label={tr('templates.ext.stripImageTools')}
              />
            </div>
            {extQ.isError && (
              <p className="text-sm text-destructive">{tr('templates.ext.loadFailed', { message: (extQ.error as Error).message })}</p>
            )}
            {extSave.isError && errMsg(extSave.error) && (
              <p className="text-sm text-destructive">{errMsg(extSave.error)}</p>
            )}
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setExtTarget(null)} disabled={extSave.isPending}>{tr('common.cancel')}</Button>
            <Button onClick={() => extSave.mutate()} disabled={extSave.isPending || extQ.isLoading}>
              {extSave.isPending ? tr('common.saving') : tr('common.save')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* —— 删除确认 —— */}
      <Dialog open={!!deleting} onOpenChange={o => { if (!o && !remove.isPending) setDeleting(null) }}>
        <DialogContent className="sm:max-w-sm">
          <DialogHeader>
            <DialogTitle>{tr('templates.deleteTitle')}</DialogTitle>
            <DialogDescription>
              {tr('templates.deleteDesc', { name: deleting?.Name })}
            </DialogDescription>
          </DialogHeader>
          {remove.isError && errMsg(remove.error) && (
            <p className="text-sm text-destructive">{errMsg(remove.error)}</p>
          )}
          <DialogFooter>
            <Button variant="outline" onClick={() => setDeleting(null)} disabled={remove.isPending}>{tr('common.cancel')}</Button>
            <Button variant="destructive" onClick={() => deleting && remove.mutate(deleting.ID)} disabled={remove.isPending}>
              {remove.isPending ? tr('common.deleting') : tr('common.confirmDelete')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
}
