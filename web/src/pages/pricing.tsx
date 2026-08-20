// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

import { useEffect, useState } from 'react'
import type { ReactNode } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { motion } from 'framer-motion'
import { Coins, Filter, Pencil, Plus, RefreshCw, Trash2 } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { api } from '@/App'
import { ApiUnauthorized } from '@/lib/api/client'
import { ListToolbar } from '@/components/list-toolbar'
import { PagePagination } from '@/components/page-pagination'
import { SortableHeader, type SortOrder } from '@/components/sortable-header'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { Skeleton } from '@/components/ui/skeleton'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { toast } from '@/components/ui/toast'
import { cn } from '@/lib/utils'
import { formatDateTime, formatPricePerMillion } from '@/components/fmt'
import type { components } from '@/lib/api/schema'

type Pricing = components['schemas']['Pricing']
type PricingUpsert = components['schemas']['PricingUpsert']
type PricingSource = components['schemas']['PricingSource']
type ImagePrice = components['schemas']['ImagePrice']
type ImagePriceUpsert = components['schemas']['ImagePriceUpsert']
type FunctionPrice = components['schemas']['FunctionPrice']
type FunctionPriceUpsert = components['schemas']['FunctionPriceUpsert']

// 页面三 Tab：文本价格 / 图片价格 / 按次价格，各自独立分页/source/provider 筛选/model 搜索。
type TabKey = 'text' | 'image' | 'function'

const SOURCES: PricingSource[] = ['litellm', 'manual']

// 厂商枚举（与 openapi components/schemas/Provider 同源，服务前端下拉框）。
// litellm_provider 动态——新厂商出现时扩此处与 openapi；DB 筛选为自由字符串
// 等值，不限于本枚举（服务端不受限）。
type Provider = components['schemas']['Provider']
const PROVIDERS: Provider[] = [
  'openai', 'anthropic', 'azure', 'vertex_ai', 'bedrock', 'deepseek', 'mistral',
  'cohere', 'xai', 'openrouter', 'groq', 'together_ai', 'fireworks_ai',
  'replicate', 'huggingface', 'moonshot', 'zhipu', 'baidu', 'alibaba', 'meta',
  'nvidia', 'cerebras', 'perplexity',
]

// 可选价格字段（USD/1M tokens 正常值，API 边界换算；表单留空 = 提交时省略 → 落库 NULL）。
// 类型唯一来源 schema.d.ts：PricingUpsert 键名为小写下划线（与 Go 响应大写字段区分）。
type OptKey = Exclude<keyof PricingUpsert, 'prompt_price_per_million' | 'completion_price_per_million'>
const OPT_KEYS: OptKey[] = [
  'cache_read_price_per_million',
  'cache_creation_price_per_million',
  'priority_prompt_price_per_million',
  'priority_completion_price_per_million',
  'priority_cache_read_price_per_million',
  'priority_cache_creation_price_per_million',
  'flex_prompt_price_per_million',
  'flex_completion_price_per_million',
  'flex_cache_read_price_per_million',
  'flex_cache_creation_price_per_million',
  'above_threshold',
  'above_prompt_price_per_million',
  'above_completion_price_per_million',
  'above_cache_read_price_per_million',
  'above_cache_creation_price_per_million',
  'above_priority_prompt_price_per_million',
  'above_priority_completion_price_per_million',
  'above_priority_cache_read_price_per_million',
  'above_priority_cache_creation_price_per_million',
  'above_flex_prompt_price_per_million',
  'above_flex_completion_price_per_million',
  'above_flex_cache_read_price_per_million',
  'above_flex_cache_creation_price_per_million',
  'fast_multiplier',
]

// 回显映射：Pricing 响应字段（Go 大写）→ PricingUpsert 提交键（小写下划线）。
const RESP_TO_KEY: { r: keyof Pricing; k: OptKey }[] = [
  { r: 'CacheReadPricePerMillion', k: 'cache_read_price_per_million' },
  { r: 'CacheCreationPricePerMillion', k: 'cache_creation_price_per_million' },
  { r: 'PriorityPromptPricePerMillion', k: 'priority_prompt_price_per_million' },
  { r: 'PriorityCompletionPricePerMillion', k: 'priority_completion_price_per_million' },
  { r: 'PriorityCacheReadPricePerMillion', k: 'priority_cache_read_price_per_million' },
  { r: 'PriorityCacheCreationPricePerMillion', k: 'priority_cache_creation_price_per_million' },
  { r: 'FlexPromptPricePerMillion', k: 'flex_prompt_price_per_million' },
  { r: 'FlexCompletionPricePerMillion', k: 'flex_completion_price_per_million' },
  { r: 'FlexCacheReadPricePerMillion', k: 'flex_cache_read_price_per_million' },
  { r: 'FlexCacheCreationPricePerMillion', k: 'flex_cache_creation_price_per_million' },
  { r: 'AboveThreshold', k: 'above_threshold' },
  { r: 'FastMultiplier', k: 'fast_multiplier' },
  { r: 'AbovePromptPricePerMillion', k: 'above_prompt_price_per_million' },
  { r: 'AboveCompletionPricePerMillion', k: 'above_completion_price_per_million' },
  { r: 'AboveCacheReadPricePerMillion', k: 'above_cache_read_price_per_million' },
  { r: 'AboveCacheCreationPricePerMillion', k: 'above_cache_creation_price_per_million' },
  { r: 'AbovePriorityPromptPricePerMillion', k: 'above_priority_prompt_price_per_million' },
  { r: 'AbovePriorityCompletionPricePerMillion', k: 'above_priority_completion_price_per_million' },
  { r: 'AbovePriorityCacheReadPricePerMillion', k: 'above_priority_cache_read_price_per_million' },
  { r: 'AbovePriorityCacheCreationPricePerMillion', k: 'above_priority_cache_creation_price_per_million' },
  { r: 'AboveFlexPromptPricePerMillion', k: 'above_flex_prompt_price_per_million' },
  { r: 'AboveFlexCompletionPricePerMillion', k: 'above_flex_completion_price_per_million' },
  { r: 'AboveFlexCacheReadPricePerMillion', k: 'above_flex_cache_read_price_per_million' },
  { r: 'AboveFlexCacheCreationPricePerMillion', k: 'above_flex_cache_creation_price_per_million' },
]

// 表单分组（仅渲染用）：label 取 pricing 作用域 tKey；键与 OPT_KEYS 一一对应。
interface OptField { key: OptKey; tKey: string }
const CACHE_FIELDS: OptField[] = [
  { key: 'cache_read_price_per_million', tKey: 'cacheReadLabel' },
  { key: 'cache_creation_price_per_million', tKey: 'cacheWriteLabel' },
]
const PRIORITY_FIELDS: OptField[] = [
  { key: 'priority_prompt_price_per_million', tKey: 'promptLabel' },
  { key: 'priority_completion_price_per_million', tKey: 'completionLabel' },
  { key: 'priority_cache_read_price_per_million', tKey: 'cacheReadLabel' },
  { key: 'priority_cache_creation_price_per_million', tKey: 'cacheWriteLabel' },
]
const FLEX_FIELDS: OptField[] = [
  { key: 'flex_prompt_price_per_million', tKey: 'promptLabel' },
  { key: 'flex_completion_price_per_million', tKey: 'completionLabel' },
  { key: 'flex_cache_read_price_per_million', tKey: 'cacheReadLabel' },
  { key: 'flex_cache_creation_price_per_million', tKey: 'cacheWriteLabel' },
]
const ABOVE_FIELDS: OptField[] = [
  { key: 'above_prompt_price_per_million', tKey: 'promptLabel' },
  { key: 'above_completion_price_per_million', tKey: 'completionLabel' },
  { key: 'above_cache_read_price_per_million', tKey: 'cacheReadLabel' },
  { key: 'above_cache_creation_price_per_million', tKey: 'cacheWriteLabel' },
]
const ABOVE_PRIORITY_FIELDS: OptField[] = [
  { key: 'above_priority_prompt_price_per_million', tKey: 'promptLabel' },
  { key: 'above_priority_completion_price_per_million', tKey: 'completionLabel' },
  { key: 'above_priority_cache_read_price_per_million', tKey: 'cacheReadLabel' },
  { key: 'above_priority_cache_creation_price_per_million', tKey: 'cacheWriteLabel' },
]
const ABOVE_FLEX_FIELDS: OptField[] = [
  { key: 'above_flex_prompt_price_per_million', tKey: 'promptLabel' },
  { key: 'above_flex_completion_price_per_million', tKey: 'completionLabel' },
  { key: 'above_flex_cache_read_price_per_million', tKey: 'cacheReadLabel' },
  { key: 'above_flex_cache_creation_price_per_million', tKey: 'cacheWriteLabel' },
]

// 快速倍率（正常值，API 边界已换算）→ 展示：2.0 = ×2.0，0.5 = ×0.5；null = 无倍率。
const formatFastMultiplier = (m: number | null | undefined): string =>
  m == null ? '—' : `×${m.toFixed(1)}`

// 档位列（三挡并列）：P = 优先档输入价（OpenAI 专属）、F = 弹性档输入价（OpenAI 专属）、
// Fast = 快速倍率（Anthropic 专属）。单元格紧凑展示（截断），完整信息放 title。
function TierCell({ p }: { p: Pricing }) {
  const { t } = useTranslation()
  const pp = formatPricePerMillion(p.PriorityPromptPricePerMillion)
  const fp = formatPricePerMillion(p.FlexPromptPricePerMillion)
  const fm = formatFastMultiplier(p.FastMultiplier)
  const text = `P ${pp} / F ${fp} / Fast ${fm}`
  return (
    <TableCell className="text-right">
      <span
        className="block max-w-52 truncate whitespace-nowrap text-xs tabular-nums"
        title={t('pricing.tierCellTitle', { p: pp, f: fp, fast: fm })}
      >
        {text}
      </span>
    </TableCell>
  )
}

// 来源徽章：自动同步灰点 / 手动蓝点（手动价由用户维护）。
function SourceBadge({ source }: { source: PricingSource }) {
  const { t } = useTranslation()
  const manual = source === 'manual'
  return (
    <Badge variant="secondary" className={cn('gap-1.5', manual ? 'text-blue-700 dark:text-blue-400' : 'text-muted-foreground')}>
      <span className={cn('size-1.5 shrink-0 rounded-full', manual ? 'bg-blue-500' : 'bg-muted-foreground/60')} />
      {t(`pricing.source.${source}`)}
    </Badge>
  )
}

// 挡位归属标注：priority/flex = OpenAI 专属，fast = Anthropic 专属，基础/above = 通用。
function OwnershipBadge({ children }: { children: ReactNode }) {
  return (
    <span className="ml-2 rounded-full border border-border px-1.5 py-0.5 text-[10px] font-normal text-muted-foreground">
      {children}
    </span>
  )
}

// 手动设价表单态：所有价格字段均为字符串（'' = 未填/不设置），提交时转换。
interface PriceForm {
  model: string
  prompt: string
  completion: string
  opt: Record<OptKey, string>
}

const emptyForm = (): PriceForm => {
  const opt = {} as Record<OptKey, string>
  for (const k of OPT_KEYS) opt[k] = ''
  return { model: '', prompt: '', completion: '', opt }
}

function toForm(p: Pricing): PriceForm {
  const f = emptyForm()
  f.model = p.Model
  f.prompt = String(p.PromptPricePerMillion)
  f.completion = String(p.CompletionPricePerMillion)
  for (const { r, k } of RESP_TO_KEY) {
    const v = p[r]
    f.opt[k] = v == null ? '' : String(v)
  }
  return f
}

// 提交体：必填 prompt/completion 恒发；可选字段空 = 省略（服务端落库 NULL）。
function toBody(f: PriceForm): PricingUpsert {
  const body: PricingUpsert = {
    prompt_price_per_million: Number(f.prompt),
    completion_price_per_million: Number(f.completion),
  }
  for (const k of OPT_KEYS) {
    const v = f.opt[k]
    if (v !== '') body[k] = Number(v)
  }
  return body
}

// 非负数校验（价格字段通用，支持小数；'' = 未填不校验）。
const isNonNegNum = (v: string) => v === '' || (Number.isFinite(Number(v)) && Number(v) >= 0)

// —— 图片价表单（三分量全可选；至少一个非空，否则 400 语义前端提示） ——
interface ImageForm {
  model: string
  inputToken: string
  outputToken: string
  perImage: string
}
const emptyImageForm = (): ImageForm => ({ model: '', inputToken: '', outputToken: '', perImage: '' })
function toImageForm(p: ImagePrice): ImageForm {
  return {
    model: p.Model,
    inputToken: p.InputImageTokenPricePerMillion == null ? '' : String(p.InputImageTokenPricePerMillion),
    outputToken: p.OutputImageTokenPricePerMillion == null ? '' : String(p.OutputImageTokenPricePerMillion),
    perImage: p.OutputCostPerImage == null ? '' : String(p.OutputCostPerImage),
  }
}
// 提交体（USD 值直接提交，API 边界已换算）：留空的分量省略 → 服务端落库 NULL = 清空该分量。
function toImageBody(f: ImageForm): ImagePriceUpsert {
  const body: ImagePriceUpsert = {}
  if (f.inputToken !== '') body.input_image_token_price_per_million = Number(f.inputToken)
  if (f.outputToken !== '') body.output_image_token_price_per_million = Number(f.outputToken)
  if (f.perImage !== '') body.output_cost_per_image = Number(f.perImage)
  return body
}

// —— 按次价表单（price_per_call 必填 ≥ 0） ——
interface FunctionForm {
  model: string
  pricePerCall: string
}
const emptyFunctionForm = (): FunctionForm => ({ model: '', pricePerCall: '' })
function toFunctionForm(p: FunctionPrice): FunctionForm {
  return { model: p.Model, pricePerCall: p.PricePerCall == null ? '' : String(p.PricePerCall) }
}
const toFunctionBody = (f: FunctionForm): FunctionPriceUpsert => ({ price_per_call: Number(f.pricePerCall) })

// USD 值展示：常规值 4 位小数；极小值（如 per-image 价低至 1e-4 以下）用科学
// 计数，避免显示 0.0000。空值 → —。
const formatUsd = (v: number | null | undefined): string => {
  if (v == null) return '—'
  if (v === 0) return '$0'
  return Math.abs(v) >= 0.0001 ? `$${v.toFixed(4)}` : `$${v.toExponential(2)}`
}

export default function PricingPage() {
  const { t } = useTranslation()
  const qc = useQueryClient()

  // —— 页面三 Tab（文本/图片/按次，各自独立状态） ——
  const [tab, setTab] = useState<TabKey>('text')

  // —— 文本价列表：page/page_size 1-based 分页（PagePagination 范式）+ source/provider 筛选 + model 模糊搜索 ——
  const [page, setPage] = useState(1)
  const [pageSize, setPageSize] = useState(20)
  const [model, setModel] = useState('')
  const [sourceFilter, setSourceFilter] = useState<'all' | PricingSource>('all')
  const [providerFilter, setProviderFilter] = useState<'all' | Provider>('all')
  const [activeSort, setActiveSort] = useState<string | null>(null) // null = 默认 model asc
  const [order, setOrder] = useState<SortOrder>('desc')
  const sort = activeSort ?? 'model'
  const ord = activeSort ? order : 'asc'

  const { data: textData, isLoading: textLoading, isError: textIsError, error: textError } = useQuery({
    queryKey: ['pricing', { page, page_size: pageSize, source: sourceFilter, provider: providerFilter, model, sort, order: ord }],
    queryFn: () =>
      api.listPricing({
        page,
        page_size: pageSize,
        source: sourceFilter === 'all' ? undefined : sourceFilter,
        provider: providerFilter === 'all' ? undefined : providerFilter,
        model: model || undefined,
        sort,
        order: ord,
      }),
  })
  const textRows = textData?.rows ?? []

  // 末页死胡同守卫：非首页的当前页数据被清空（筛选把末页清空）时回退到第 1 页。
  useEffect(() => {
    if (!textLoading && !textIsError && textRows.length === 0 && page > 1) setPage(1)
  }, [textLoading, textIsError, textRows.length, page])

  const resetPage = () => setPage(1)
  const changePageSize = (s: number) => { setPageSize(s); resetPage() }
  const changeModel = (v: string) => { setModel(v); resetPage() }
  const changeSource = (v: string) => { setSourceFilter(v as 'all' | PricingSource); resetPage() }
  const changeProvider = (v: string) => { setProviderFilter(v as 'all' | Provider); resetPage() }
  // 列头三态：新列 → 降序；同列降序 → 升序；同列升序 → 取消（回默认 model asc）。
  const onColumnToggle = (col: string) => {
    resetPage()
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
  const hasFilters = model !== '' || sourceFilter !== 'all' || providerFilter !== 'all'
  const clearFilters = () => { setModel(''); setSourceFilter('all'); setProviderFilter('all'); resetPage() }

  // —— 图片价列表（同形独立状态） ——
  const [imgPage, setImgPage] = useState(1)
  const [imgPageSize, setImgPageSize] = useState(20)
  const [imgModel, setImgModel] = useState('')
  const [imgSource, setImgSource] = useState<'all' | PricingSource>('all')
  const [imgProvider, setImgProvider] = useState<'all' | Provider>('all')
  const [imgActiveSort, setImgActiveSort] = useState<string | null>(null)
  const [imgOrder, setImgOrder] = useState<SortOrder>('desc')
  const imgSort = imgActiveSort ?? 'model'
  const imgOrd = imgActiveSort ? imgOrder : 'asc'

  const { data: imgData, isLoading: imgLoading, isError: imgIsError, error: imgError } = useQuery({
    queryKey: ['image-pricing', { page: imgPage, page_size: imgPageSize, source: imgSource, provider: imgProvider, model: imgModel, sort: imgSort, order: imgOrd }],
    queryFn: () =>
      api.getImagePrices({
        page: imgPage,
        page_size: imgPageSize,
        source: imgSource === 'all' ? undefined : imgSource,
        provider: imgProvider === 'all' ? undefined : imgProvider,
        model: imgModel || undefined,
        sort: imgSort,
        order: imgOrd,
      }),
  })
  const imgRows = imgData?.rows ?? []

  useEffect(() => {
    if (!imgLoading && !imgIsError && imgRows.length === 0 && imgPage > 1) setImgPage(1)
  }, [imgLoading, imgIsError, imgRows.length, imgPage])

  const imgReset = () => setImgPage(1)
  const imgSetPageSize = (s: number) => { setImgPageSize(s); imgReset() }
  const imgSetModel = (v: string) => { setImgModel(v); imgReset() }
  const imgSetSource = (v: string) => { setImgSource(v as 'all' | PricingSource); imgReset() }
  const imgSetProvider = (v: string) => { setImgProvider(v as 'all' | Provider); imgReset() }
  const imgToggleSort = (col: string) => {
    imgReset()
    if (imgActiveSort !== col) {
      setImgActiveSort(col)
      setImgOrder('desc')
    } else if (imgOrder === 'desc') {
      setImgOrder('asc')
    } else {
      setImgActiveSort(null)
      setImgOrder('desc')
    }
  }
  const imgHasFilters = imgModel !== '' || imgSource !== 'all' || imgProvider !== 'all'
  const imgClearFilters = () => { setImgModel(''); setImgSource('all'); setImgProvider('all'); imgReset() }

  // —— 按次价列表（同形独立状态） ——
  const [fnPage, setFnPage] = useState(1)
  const [fnPageSize, setFnPageSize] = useState(20)
  const [fnModel, setFnModel] = useState('')
  const [fnSource, setFnSource] = useState<'all' | PricingSource>('all')
  const [fnProvider, setFnProvider] = useState<'all' | Provider>('all')
  const [fnActiveSort, setFnActiveSort] = useState<string | null>(null)
  const [fnOrder, setFnOrder] = useState<SortOrder>('desc')
  const fnSort = fnActiveSort ?? 'model'
  const fnOrd = fnActiveSort ? fnOrder : 'asc'

  const { data: fnData, isLoading: fnLoading, isError: fnIsError, error: fnError } = useQuery({
    queryKey: ['function-pricing', { page: fnPage, page_size: fnPageSize, source: fnSource, provider: fnProvider, model: fnModel, sort: fnSort, order: fnOrd }],
    queryFn: () =>
      api.getFunctionPrices({
        page: fnPage,
        page_size: fnPageSize,
        source: fnSource === 'all' ? undefined : fnSource,
        provider: fnProvider === 'all' ? undefined : fnProvider,
        model: fnModel || undefined,
        sort: fnSort,
        order: fnOrd,
      }),
  })
  const fnRows = fnData?.rows ?? []

  useEffect(() => {
    if (!fnLoading && !fnIsError && fnRows.length === 0 && fnPage > 1) setFnPage(1)
  }, [fnLoading, fnIsError, fnRows.length, fnPage])

  const fnReset = () => setFnPage(1)
  const fnSetPageSize = (s: number) => { setFnPageSize(s); fnReset() }
  const fnSetModel = (v: string) => { setFnModel(v); fnReset() }
  const fnSetSource = (v: string) => { setFnSource(v as 'all' | PricingSource); fnReset() }
  const fnSetProvider = (v: string) => { setFnProvider(v as 'all' | Provider); fnReset() }
  const fnToggleSort = (col: string) => {
    fnReset()
    if (fnActiveSort !== col) {
      setFnActiveSort(col)
      setFnOrder('desc')
    } else if (fnOrder === 'desc') {
      setFnOrder('asc')
    } else {
      setFnActiveSort(null)
      setFnOrder('desc')
    }
  }
  const fnHasFilters = fnModel !== '' || fnSource !== 'all' || fnProvider !== 'all'
  const fnClearFilters = () => { setFnModel(''); setFnSource('all'); setFnProvider('all'); fnReset() }

  // —— 手动触发价格同步（三线统计合一：文本/图片/按次；成功后展示并刷新三个列表） ——
  const sync = useMutation({
    mutationFn: () => api.syncPricing(),
    onSuccess: res => {
      toast.add({
        title: (
          <>
            <span className="block">{t('pricing.syncDone', { rows: res.rows, skipped: res.skipped, updated: res.updated })}</span>
            <span className="block">{t('pricing.syncDoneImage', { image_rows: res.image_rows ?? 0, image_updated: res.image_updated ?? 0 })}</span>
            <span className="block">{t('pricing.syncDoneFunction', { function_rows: res.function_rows ?? 0, function_updated: res.function_updated ?? 0 })}</span>
          </>
        ),
        type: 'success',
      })
      qc.invalidateQueries({ queryKey: ['pricing'] })
      qc.invalidateQueries({ queryKey: ['image-pricing'] })
      qc.invalidateQueries({ queryKey: ['function-pricing'] })
    },
    onError: (e: Error) => toast.add({ title: e.message, type: 'error' }),
  })

  // —— 文本价：手动设价（新建/编辑复用）：编辑 litellm 行 = 保存后接管为手动价 ——
  const [dialogOpen, setDialogOpen] = useState(false)
  const [editing, setEditing] = useState<Pricing | null>(null)
  const [form, setForm] = useState<PriceForm>(emptyForm())
  const [formErr, setFormErr] = useState<string | null>(null)
  const [activeTab, setActiveTab] = useState('base')

  const openCreate = () => {
    if (tab === 'image') openImageCreate()
    else if (tab === 'function') openFunctionCreate()
    else {
      setEditing(null)
      setForm(emptyForm())
      setFormErr(null)
      setActiveTab('base')
      setDialogOpen(true)
    }
  }
  const openEdit = (p: Pricing) => {
    setEditing(p)
    setForm(toForm(p))
    setFormErr(null)
    setActiveTab('base')
    setDialogOpen(true)
  }
  const setOpt = (k: OptKey, v: string) => {
    setForm(f => ({ ...f, opt: { ...f.opt, [k]: v } }))
    setFormErr(null)
  }

  const save = useMutation({
    mutationFn: (f: PriceForm) => api.upsertPricing(editing ? editing.Model : f.model.trim(), toBody(f)),
    onSuccess: () => {
      toast.add({ title: t('pricing.saved'), type: 'success' })
      qc.invalidateQueries({ queryKey: ['pricing'] })
      setDialogOpen(false)
    },
  })
  const submit = () => {
    const fm = form
    const fast = fm.opt.fast_multiplier
    const fastNum = fast === '' ? null : Number(fast)
    const valid =
      (editing || fm.model.trim() !== '') &&
      isNonNegNum(fm.prompt) && Number(fm.prompt) >= 0 && fm.prompt !== '' &&
      isNonNegNum(fm.completion) && fm.completion !== '' &&
      (fastNum === null || (Number.isFinite(fastNum) && fastNum > 0 && fastNum <= 10)) &&
      OPT_KEYS.every(k => k === 'fast_multiplier' || isNonNegNum(fm.opt[k]))
    if (!valid) {
      setFormErr(t('pricing.formInvalid'))
      return
    }
    save.mutate(fm)
  }

  // —— 文本价：删除手动价（自动同步维护的行 → 按钮禁用 + 服务端 409 兜底展示） ——
  const [deleting, setDeleting] = useState<Pricing | null>(null)
  const del = useMutation({
    mutationFn: (p: Pricing) => api.deletePricing(p.Model),
    onSuccess: () => {
      toast.add({ title: t('pricing.deleted'), type: 'success' })
      qc.invalidateQueries({ queryKey: ['pricing'] })
      setDeleting(null)
    },
  })

  // —— 图片价：手动设价 ——
  const [imgDialogOpen, setImgDialogOpen] = useState(false)
  const [imgEditing, setImgEditing] = useState<ImagePrice | null>(null)
  const [imgForm, setImgForm] = useState<ImageForm>(emptyImageForm())
  const [imgFormErr, setImgFormErr] = useState<string | null>(null)

  const openImageCreate = () => {
    setImgEditing(null)
    setImgForm(emptyImageForm())
    setImgFormErr(null)
    setImgDialogOpen(true)
  }
  const openImageEdit = (p: ImagePrice) => {
    setImgEditing(p)
    setImgForm(toImageForm(p))
    setImgFormErr(null)
    setImgDialogOpen(true)
  }
  const setImg = (k: keyof ImageForm, v: string) => {
    setImgForm(f => ({ ...f, [k]: v }))
    setImgFormErr(null)
  }

  const imgSave = useMutation({
    mutationFn: (f: ImageForm) => api.putImagePrice(imgEditing ? imgEditing.Model : f.model.trim(), toImageBody(f)),
    onSuccess: () => {
      toast.add({ title: t('pricing.saved'), type: 'success' })
      qc.invalidateQueries({ queryKey: ['image-pricing'] })
      setImgDialogOpen(false)
    },
  })
  // 三分量至少一个非空（400 语义前端提示）；各分量须为非负数；新建时模型必填。
  const imgSubmit = () => {
    const fm = imgForm
    const valid =
      (imgEditing || fm.model.trim() !== '') &&
      (fm.inputToken !== '' || fm.outputToken !== '' || fm.perImage !== '') &&
      isNonNegNum(fm.inputToken) && isNonNegNum(fm.outputToken) && isNonNegNum(fm.perImage)
    if (!valid) {
      setImgFormErr(t('pricing.image.formInvalid'))
      return
    }
    imgSave.mutate(fm)
  }

  // —— 图片价：删除 ——
  const [imgDeleting, setImgDeleting] = useState<ImagePrice | null>(null)
  const imgDel = useMutation({
    mutationFn: (p: ImagePrice) => api.deleteImagePrice(p.Model),
    onSuccess: () => {
      toast.add({ title: t('pricing.deleted'), type: 'success' })
      qc.invalidateQueries({ queryKey: ['image-pricing'] })
      setImgDeleting(null)
    },
  })

  // —— 按次价：手动设价（price_per_call 必填 ≥ 0） ——
  const [fnDialogOpen, setFnDialogOpen] = useState(false)
  const [fnEditing, setFnEditing] = useState<FunctionPrice | null>(null)
  const [fnForm, setFnForm] = useState<FunctionForm>(emptyFunctionForm())
  const [fnFormErr, setFnFormErr] = useState<string | null>(null)

  const openFunctionCreate = () => {
    setFnEditing(null)
    setFnForm(emptyFunctionForm())
    setFnFormErr(null)
    setFnDialogOpen(true)
  }
  const openFunctionEdit = (p: FunctionPrice) => {
    setFnEditing(p)
    setFnForm(toFunctionForm(p))
    setFnFormErr(null)
    setFnDialogOpen(true)
  }
  const setFn = (k: keyof FunctionForm, v: string) => {
    setFnForm(f => ({ ...f, [k]: v }))
    setFnFormErr(null)
  }

  const fnSave = useMutation({
    mutationFn: (f: FunctionForm) => api.putFunctionPrice(fnEditing ? fnEditing.Model : f.model.trim(), toFunctionBody(f)),
    onSuccess: () => {
      toast.add({ title: t('pricing.saved'), type: 'success' })
      qc.invalidateQueries({ queryKey: ['function-pricing'] })
      setFnDialogOpen(false)
    },
  })
  const fnSubmit = () => {
    const fm = fnForm
    const v = Number(fm.pricePerCall)
    const valid = (fnEditing || fm.model.trim() !== '') && fm.pricePerCall !== '' && Number.isFinite(v) && v >= 0
    if (!valid) {
      setFnFormErr(t('pricing.function.formInvalid'))
      return
    }
    fnSave.mutate(fm)
  }

  // —— 按次价：删除 ——
  const [fnDeleting, setFnDeleting] = useState<FunctionPrice | null>(null)
  const fnDel = useMutation({
    mutationFn: (p: FunctionPrice) => api.deleteFunctionPrice(p.Model),
    onSuccess: () => {
      toast.add({ title: t('pricing.deleted'), type: 'success' })
      qc.invalidateQueries({ queryKey: ['function-pricing'] })
      setFnDeleting(null)
    },
  })

  const errMsg = (e: unknown) => (e instanceof ApiUnauthorized ? null : (e as Error)?.message)
  const sourceItems = Object.fromEntries([['all', t('pricing.all')], ...SOURCES.map(s => [s, t(`pricing.source.${s}`)])])
  const providerItems = Object.fromEntries([['all', t('pricing.providerAll')], ...PROVIDERS.map(p => [p, p])])
  // 删除按钮：litellm 行禁用 + title 提示（服务端 409 语义前置拦截）。
  const delDisabledTitle = (source: PricingSource) =>
    source === 'litellm' ? t('pricing.deleteLitellmHint') : t('pricing.deleteTitle')

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-semibold tracking-tight">{t('pricing.title')}</h1>
          <p className="text-sm text-muted-foreground">{t('pricing.subtitle')}</p>
        </div>
        <div className="flex items-center gap-2">
          <Button variant="outline" onClick={() => sync.mutate()} disabled={sync.isPending}>
            <RefreshCw className={sync.isPending ? 'animate-spin' : ''} />
            {sync.isPending ? t('pricing.syncing') : t('pricing.sync')}
          </Button>
          <Button onClick={openCreate}>
            <Plus />
            {tab === 'image' ? t('pricing.image.new') : tab === 'function' ? t('pricing.function.new') : t('pricing.new')}
          </Button>
        </div>
      </div>

      <Tabs value={tab} onValueChange={v => v && setTab(v as TabKey)}>
        <TabsList className="w-full">
          <TabsTrigger value="text" className="flex-1">{t('pricing.tabs.text')}</TabsTrigger>
          <TabsTrigger value="image" className="flex-1">{t('pricing.tabs.image')}</TabsTrigger>
          <TabsTrigger value="function" className="flex-1">{t('pricing.tabs.function')}</TabsTrigger>
        </TabsList>

        {/* —— Tab 1：文本价格（USD/1M tokens） —— */}
        <TabsContent value="text" className="space-y-6 pt-4">
          <ListToolbar name={model} onNameChange={changeModel} placeholder={t('pricing.searchModel')}>
            <Select items={sourceItems} value={sourceFilter} onValueChange={changeSource}>
              <SelectTrigger size="default" className="w-40" aria-label={t('pricing.all')}>
                <SelectValue placeholder={t('pricing.all')} />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="all" label={t('pricing.all')}>{t('pricing.all')}</SelectItem>
                {SOURCES.map(s => <SelectItem key={s} value={s} label={t(`pricing.source.${s}`)}>{t(`pricing.source.${s}`)}</SelectItem>)}
              </SelectContent>
            </Select>
            <Select items={providerItems} value={providerFilter} onValueChange={changeProvider}>
              <SelectTrigger size="default" className="w-44" aria-label={t('pricing.providerAll')}>
                <SelectValue placeholder={t('pricing.providerAll')} />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="all" label={t('pricing.providerAll')}>{t('pricing.providerAll')}</SelectItem>
                {PROVIDERS.map(p => <SelectItem key={p} value={p} label={p}>{p}</SelectItem>)}
              </SelectContent>
            </Select>
            {(sourceFilter !== 'all' || providerFilter !== 'all') && (
              <Button variant="ghost" size="lg" onClick={clearFilters}><Filter /> {t('list.reset')}</Button>
            )}
          </ListToolbar>

          {textIsError ? (
            <p className="text-sm text-destructive">{t('common.loadFailed', { message: (textError as Error).message })}</p>
          ) : textLoading ? (
            <div className="space-y-2">
              {Array.from({ length: 4 }).map((_, i) => <Skeleton key={i} className="h-12" />)}
            </div>
          ) : textRows.length === 0 ? (
            <motion.div initial={{ opacity: 0, y: 12 }} animate={{ opacity: 1, y: 0 }} transition={{ duration: 0.25 }}>
              <Card className="flex flex-col items-center gap-2 py-12 text-muted-foreground">
                <Coins className="size-10" />
                <p className="font-medium">{hasFilters ? t('pricing.filterEmpty') : t('pricing.emptyTitle')}</p>
                {!hasFilters && <p className="text-sm">{t('pricing.emptyDesc')}</p>}
                {hasFilters ? (
                  <Button className="mt-2" variant="outline" onClick={clearFilters}><Filter /> {t('list.reset')}</Button>
                ) : (
                  <Button className="mt-2" onClick={openCreate}><Plus /> {t('pricing.new')}</Button>
                )}
              </Card>
            </motion.div>
          ) : (
            <>
              <div className="overflow-hidden rounded-lg">
                <Table>
                  <TableHeader>
                    <TableRow>
                      <SortableHeader field="model" label={t('pricing.table.model')} active={activeSort === 'model'} order={order} onToggle={onColumnToggle} />
                      <TableHead className="text-right">{t('pricing.table.prompt')}</TableHead>
                      <TableHead className="text-right">{t('pricing.table.completion')}</TableHead>
                      <TableHead className="text-right">{t('pricing.table.cacheRead')}</TableHead>
                      <TableHead className="text-right">{t('pricing.table.cacheWrite')}</TableHead>
                      <TableHead className="text-right" title={t('pricing.table.tierTitle')}>{t('pricing.table.tier')}</TableHead>
                      <TableHead className="text-right">{t('pricing.table.aboveThreshold')}</TableHead>
                      <TableHead>{t('pricing.table.source')}</TableHead>
                      <TableHead>{t('pricing.table.provider')}</TableHead>
                      <SortableHeader field="updated_at" label={t('pricing.table.updatedAt')} active={activeSort === 'updated_at'} order={order} onToggle={onColumnToggle} />
                      <TableHead className="text-right">{t('pricing.table.actions')}</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody className="[&_td]:py-3">
                    {textRows.map(p => (
                      <TableRow key={p.Model}>
                        <TableCell className="max-w-48 truncate font-mono text-sm" title={p.Model}>{p.Model}</TableCell>
                        <TableCell className="text-right tabular-nums">{formatPricePerMillion(p.PromptPricePerMillion)}</TableCell>
                        <TableCell className="text-right tabular-nums">{formatPricePerMillion(p.CompletionPricePerMillion)}</TableCell>
                        <TableCell className="text-right tabular-nums">{formatPricePerMillion(p.CacheReadPricePerMillion)}</TableCell>
                        <TableCell className="text-right tabular-nums">{formatPricePerMillion(p.CacheCreationPricePerMillion)}</TableCell>
                        <TierCell p={p} />
                        <TableCell className="text-right tabular-nums">{p.AboveThreshold == null ? '—' : t('pricing.table.aboveThresholdValue', { value: p.AboveThreshold })}</TableCell>
                        <TableCell><SourceBadge source={p.Source} /></TableCell>
                        <TableCell className="max-w-32 truncate" title={p.Provider ?? undefined}>{p.Provider || '—'}</TableCell>
                        <TableCell className="text-xs text-muted-foreground whitespace-nowrap">{formatDateTime(p.UpdatedAt)}</TableCell>
                        <TableCell className="text-right">
                          <div className="flex justify-end gap-1">
                            <Button variant="ghost" size="icon-sm" title={t('common.edit')} onClick={() => openEdit(p)}><Pencil /></Button>
                            <Button
                              variant="ghost"
                              size="icon-sm"
                              className="text-destructive"
                              title={delDisabledTitle(p.Source)}
                              onClick={() => setDeleting(p)}
                              disabled={p.Source === 'litellm' || del.isPending}
                            >
                              <Trash2 />
                            </Button>
                          </div>
                        </TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              </div>
              <PagePagination total={textData?.total ?? 0} pageSize={pageSize} page={page} onPageChange={setPage} onPageSizeChange={changePageSize} />
            </>
          )}
        </TabsContent>

        {/* —— Tab 2：图片价格（USD/1M image tokens + USD/张，per-million 口径同文本价） —— */}
        <TabsContent value="image" className="space-y-6 pt-4">
          <ListToolbar name={imgModel} onNameChange={imgSetModel} placeholder={t('pricing.searchModel')}>
            <Select items={sourceItems} value={imgSource} onValueChange={imgSetSource}>
              <SelectTrigger size="default" className="w-40" aria-label={t('pricing.all')}>
                <SelectValue placeholder={t('pricing.all')} />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="all" label={t('pricing.all')}>{t('pricing.all')}</SelectItem>
                {SOURCES.map(s => <SelectItem key={s} value={s} label={t(`pricing.source.${s}`)}>{t(`pricing.source.${s}`)}</SelectItem>)}
              </SelectContent>
            </Select>
            <Select items={providerItems} value={imgProvider} onValueChange={imgSetProvider}>
              <SelectTrigger size="default" className="w-44" aria-label={t('pricing.providerAll')}>
                <SelectValue placeholder={t('pricing.providerAll')} />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="all" label={t('pricing.providerAll')}>{t('pricing.providerAll')}</SelectItem>
                {PROVIDERS.map(p => <SelectItem key={p} value={p} label={p}>{p}</SelectItem>)}
              </SelectContent>
            </Select>
            {(imgSource !== 'all' || imgProvider !== 'all') && (
              <Button variant="ghost" size="lg" onClick={imgClearFilters}><Filter /> {t('list.reset')}</Button>
            )}
          </ListToolbar>

          {imgIsError ? (
            <p className="text-sm text-destructive">{t('common.loadFailed', { message: (imgError as Error).message })}</p>
          ) : imgLoading ? (
            <div className="space-y-2">
              {Array.from({ length: 4 }).map((_, i) => <Skeleton key={i} className="h-12" />)}
            </div>
          ) : imgRows.length === 0 ? (
            <motion.div initial={{ opacity: 0, y: 12 }} animate={{ opacity: 1, y: 0 }} transition={{ duration: 0.25 }}>
              <Card className="flex flex-col items-center gap-2 py-12 text-muted-foreground">
                <Coins className="size-10" />
                <p className="font-medium">{imgHasFilters ? t('pricing.image.filterEmpty') : t('pricing.image.emptyTitle')}</p>
                {!imgHasFilters && <p className="text-sm">{t('pricing.image.emptyDesc')}</p>}
                {imgHasFilters ? (
                  <Button className="mt-2" variant="outline" onClick={imgClearFilters}><Filter /> {t('list.reset')}</Button>
                ) : (
                  <Button className="mt-2" onClick={openImageCreate}><Plus /> {t('pricing.image.new')}</Button>
                )}
              </Card>
            </motion.div>
          ) : (
            <>
              <div className="overflow-hidden rounded-lg">
                <Table>
                  <TableHeader>
                    <TableRow>
                      <SortableHeader field="model" label={t('pricing.table.model')} active={imgActiveSort === 'model'} order={imgOrder} onToggle={imgToggleSort} />
                      <TableHead className="text-right" title="USD/1M image tokens">{t('pricing.image.table.inputToken')}</TableHead>
                      <TableHead className="text-right" title="USD/1M image tokens">{t('pricing.image.table.outputToken')}</TableHead>
                      <TableHead className="text-right" title="USD/张">{t('pricing.image.table.perImage')}</TableHead>
                      <TableHead>{t('pricing.table.source')}</TableHead>
                      <TableHead>{t('pricing.table.provider')}</TableHead>
                      <SortableHeader field="updated_at" label={t('pricing.table.updatedAt')} active={imgActiveSort === 'updated_at'} order={imgOrder} onToggle={imgToggleSort} />
                      <TableHead className="text-right">{t('pricing.table.actions')}</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody className="[&_td]:py-3">
                    {imgRows.map(p => (
                      <TableRow key={p.Model}>
                        <TableCell className="max-w-48 truncate font-mono text-sm" title={p.Model}>{p.Model}</TableCell>
                        <TableCell className="text-right tabular-nums">{formatUsd(p.InputImageTokenPricePerMillion)}</TableCell>
                        <TableCell className="text-right tabular-nums">{formatUsd(p.OutputImageTokenPricePerMillion)}</TableCell>
                        <TableCell className="text-right tabular-nums">{formatUsd(p.OutputCostPerImage)}</TableCell>
                        <TableCell><SourceBadge source={p.Source} /></TableCell>
                        <TableCell className="max-w-32 truncate" title={p.Provider ?? undefined}>{p.Provider || '—'}</TableCell>
                        <TableCell className="text-xs text-muted-foreground whitespace-nowrap">{formatDateTime(p.UpdatedAt)}</TableCell>
                        <TableCell className="text-right">
                          <div className="flex justify-end gap-1">
                            <Button variant="ghost" size="icon-sm" title={t('common.edit')} onClick={() => openImageEdit(p)}><Pencil /></Button>
                            <Button
                              variant="ghost"
                              size="icon-sm"
                              className="text-destructive"
                              title={delDisabledTitle(p.Source)}
                              onClick={() => setImgDeleting(p)}
                              disabled={p.Source === 'litellm' || imgDel.isPending}
                            >
                              <Trash2 />
                            </Button>
                          </div>
                        </TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              </div>
              <PagePagination total={imgData?.total ?? 0} pageSize={imgPageSize} page={imgPage} onPageChange={setImgPage} onPageSizeChange={imgSetPageSize} />
            </>
          )}
        </TabsContent>

        {/* —— Tab 3：按次价格（USD/次） —— */}
        <TabsContent value="function" className="space-y-6 pt-4">
          <ListToolbar name={fnModel} onNameChange={fnSetModel} placeholder={t('pricing.searchModel')}>
            <Select items={sourceItems} value={fnSource} onValueChange={fnSetSource}>
              <SelectTrigger size="default" className="w-40" aria-label={t('pricing.all')}>
                <SelectValue placeholder={t('pricing.all')} />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="all" label={t('pricing.all')}>{t('pricing.all')}</SelectItem>
                {SOURCES.map(s => <SelectItem key={s} value={s} label={t(`pricing.source.${s}`)}>{t(`pricing.source.${s}`)}</SelectItem>)}
              </SelectContent>
            </Select>
            <Select items={providerItems} value={fnProvider} onValueChange={fnSetProvider}>
              <SelectTrigger size="default" className="w-44" aria-label={t('pricing.providerAll')}>
                <SelectValue placeholder={t('pricing.providerAll')} />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="all" label={t('pricing.providerAll')}>{t('pricing.providerAll')}</SelectItem>
                {PROVIDERS.map(p => <SelectItem key={p} value={p} label={p}>{p}</SelectItem>)}
              </SelectContent>
            </Select>
            {(fnSource !== 'all' || fnProvider !== 'all') && (
              <Button variant="ghost" size="lg" onClick={fnClearFilters}><Filter /> {t('list.reset')}</Button>
            )}
          </ListToolbar>

          {fnIsError ? (
            <p className="text-sm text-destructive">{t('common.loadFailed', { message: (fnError as Error).message })}</p>
          ) : fnLoading ? (
            <div className="space-y-2">
              {Array.from({ length: 4 }).map((_, i) => <Skeleton key={i} className="h-12" />)}
            </div>
          ) : fnRows.length === 0 ? (
            <motion.div initial={{ opacity: 0, y: 12 }} animate={{ opacity: 1, y: 0 }} transition={{ duration: 0.25 }}>
              <Card className="flex flex-col items-center gap-2 py-12 text-muted-foreground">
                <Coins className="size-10" />
                <p className="font-medium">{fnHasFilters ? t('pricing.function.filterEmpty') : t('pricing.function.emptyTitle')}</p>
                {!fnHasFilters && <p className="text-sm">{t('pricing.function.emptyDesc')}</p>}
                {fnHasFilters ? (
                  <Button className="mt-2" variant="outline" onClick={fnClearFilters}><Filter /> {t('list.reset')}</Button>
                ) : (
                  <Button className="mt-2" onClick={openFunctionCreate}><Plus /> {t('pricing.function.new')}</Button>
                )}
              </Card>
            </motion.div>
          ) : (
            <>
              <div className="overflow-hidden rounded-lg">
                <Table>
                  <TableHeader>
                    <TableRow>
                      <SortableHeader field="model" label={t('pricing.table.model')} active={fnActiveSort === 'model'} order={fnOrder} onToggle={fnToggleSort} />
                      <TableHead className="text-right" title="USD/次">{t('pricing.function.table.price')}</TableHead>
                      <TableHead>{t('pricing.table.source')}</TableHead>
                      <TableHead>{t('pricing.table.provider')}</TableHead>
                      <SortableHeader field="updated_at" label={t('pricing.table.updatedAt')} active={fnActiveSort === 'updated_at'} order={fnOrder} onToggle={fnToggleSort} />
                      <TableHead className="text-right">{t('pricing.table.actions')}</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody className="[&_td]:py-3">
                    {fnRows.map(p => (
                      <TableRow key={p.Model}>
                        <TableCell className="max-w-48 truncate font-mono text-sm" title={p.Model}>{p.Model}</TableCell>
                        <TableCell className="text-right tabular-nums">{formatUsd(p.PricePerCall)}</TableCell>
                        <TableCell><SourceBadge source={p.Source} /></TableCell>
                        <TableCell className="max-w-32 truncate" title={p.Provider ?? undefined}>{p.Provider || '—'}</TableCell>
                        <TableCell className="text-xs text-muted-foreground whitespace-nowrap">{formatDateTime(p.UpdatedAt)}</TableCell>
                        <TableCell className="text-right">
                          <div className="flex justify-end gap-1">
                            <Button variant="ghost" size="icon-sm" title={t('common.edit')} onClick={() => openFunctionEdit(p)}><Pencil /></Button>
                            <Button
                              variant="ghost"
                              size="icon-sm"
                              className="text-destructive"
                              title={delDisabledTitle(p.Source)}
                              onClick={() => setFnDeleting(p)}
                              disabled={p.Source === 'litellm' || fnDel.isPending}
                            >
                              <Trash2 />
                            </Button>
                          </div>
                        </TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              </div>
              <PagePagination total={fnData?.total ?? 0} pageSize={fnPageSize} page={fnPage} onPageChange={setFnPage} onPageSizeChange={fnSetPageSize} />
            </>
          )}
        </TabsContent>
      </Tabs>

      {/* —— 文本价手动设价对话框（新建/编辑复用；Tabs 分组防撑爆） —— */}
      <Dialog open={dialogOpen} onOpenChange={o => { if (!o && !save.isPending) setDialogOpen(false) }}>
        <DialogContent className="sm:max-w-2xl">
          <DialogHeader>
            <DialogTitle>{editing ? t('pricing.editTitle', { model: editing.Model }) : t('pricing.newTitle')}</DialogTitle>
            <DialogDescription>{t('pricing.dialogDesc')}</DialogDescription>
          </DialogHeader>
          <Tabs value={activeTab} onValueChange={v => v && setActiveTab(v)}>
            <TabsList className="w-full">
              <TabsTrigger value="base" className="flex-1">{t('pricing.tabBase')}</TabsTrigger>
              <TabsTrigger value="cache" className="flex-1">{t('pricing.tabCache')}</TabsTrigger>
              <TabsTrigger value="tier" className="flex-1">{t('pricing.tabTier')}</TabsTrigger>
              <TabsTrigger value="segment" className="flex-1">{t('pricing.tabSegment')}</TabsTrigger>
            </TabsList>

            {/* 基础（通用） */}
            <TabsContent value="base" className="space-y-3 pt-3">
              <p className="flex items-center text-xs text-muted-foreground">
                {t('pricing.baseOwnership')}<OwnershipBadge>{t('pricing.ownership.generic')}</OwnershipBadge>
              </p>
              <div className="space-y-1.5">
                <Label htmlFor="pf-model">{t('pricing.modelLabel')} <span className="text-destructive">*</span></Label>
                <Input
                  id="pf-model"
                  value={form.model}
                  placeholder={t('pricing.modelPlaceholder')}
                  onChange={e => { setForm(f => ({ ...f, model: e.target.value })); setFormErr(null) }}
                  disabled={!!editing}
                />
                {editing?.Source === 'litellm' && (
                  <p className="text-xs text-muted-foreground">{t('pricing.takeoverHint')}</p>
                )}
              </div>
              <div className="grid grid-cols-2 gap-3">
                <div className="space-y-1.5">
                  <Label htmlFor="pf-prompt">{t('pricing.promptLabel')} <span className="text-destructive">*</span></Label>
                  <Input id="pf-prompt" type="number" min={0} value={form.prompt} onChange={e => { setForm(f => ({ ...f, prompt: e.target.value })); setFormErr(null) }} />
                </div>
                <div className="space-y-1.5">
                  <Label htmlFor="pf-completion">{t('pricing.completionLabel')} <span className="text-destructive">*</span></Label>
                  <Input id="pf-completion" type="number" min={0} value={form.completion} onChange={e => { setForm(f => ({ ...f, completion: e.target.value })); setFormErr(null) }} />
                </div>
              </div>
            </TabsContent>

            {/* 缓存 */}
            <TabsContent value="cache" className="space-y-3 pt-3">
              <p className="text-xs text-muted-foreground">{t('pricing.cacheHint')}</p>
              <div className="grid grid-cols-2 gap-3">
                {CACHE_FIELDS.map(f => (
                  <div key={f.key} className="space-y-1.5">
                    <Label htmlFor={`pf-${f.key}`}>{t(`pricing.${f.tKey}`)}</Label>
                    <Input id={`pf-${f.key}`} type="number" min={0} value={form.opt[f.key]} onChange={e => setOpt(f.key, e.target.value)} />
                  </div>
                ))}
              </div>
            </TabsContent>

            {/* 档位：优先档 + 弹性档 + 快速档（三挡并列，各带归属标注） */}
            <TabsContent value="tier" className="space-y-4 pt-3">
              <p className="text-xs text-muted-foreground">{t('pricing.tierHint')}</p>
              <div className="space-y-2">
                <p className="flex items-center text-sm font-medium">
                  {t('pricing.priorityGroup')}<OwnershipBadge>{t('pricing.ownership.openai')}</OwnershipBadge>
                </p>
                <div className="grid grid-cols-2 gap-3">
                  {PRIORITY_FIELDS.map(f => (
                    <div key={f.key} className="space-y-1.5">
                      <Label htmlFor={`pf-${f.key}`}>{t(`pricing.${f.tKey}`)}</Label>
                      <Input id={`pf-${f.key}`} type="number" min={0} value={form.opt[f.key]} onChange={e => setOpt(f.key, e.target.value)} />
                    </div>
                  ))}
                </div>
              </div>
              <div className="space-y-2">
                <p className="flex items-center text-sm font-medium">
                  {t('pricing.flexGroup')}<OwnershipBadge>{t('pricing.ownership.openai')}</OwnershipBadge>
                </p>
                <div className="grid grid-cols-2 gap-3">
                  {FLEX_FIELDS.map(f => (
                    <div key={f.key} className="space-y-1.5">
                      <Label htmlFor={`pf-${f.key}`}>{t(`pricing.${f.tKey}`)}</Label>
                      <Input id={`pf-${f.key}`} type="number" min={0} value={form.opt[f.key]} onChange={e => setOpt(f.key, e.target.value)} />
                    </div>
                  ))}
                </div>
              </div>
              <div className="space-y-2">
                <p className="flex items-center text-sm font-medium">
                  {t('pricing.fastGroup')}<OwnershipBadge>{t('pricing.ownership.anthropic')}</OwnershipBadge>
                </p>
                <div className="space-y-1.5">
                  <Label htmlFor="pf-fast_multiplier">{t('pricing.fastMultiplierLabel')}</Label>
                  <Input id="pf-fast_multiplier" type="number" min={0} max={10} step={0.1} value={form.opt.fast_multiplier} onChange={e => setOpt('fast_multiplier', e.target.value)} />
                  <p className="text-xs text-muted-foreground">{t('pricing.fastMultiplierHint')}</p>
                </div>
              </div>
            </TabsContent>

            {/* 分段：阈值 + 基础/优先档/弹性档三组分段价 */}
            <TabsContent value="segment" className="space-y-4 pt-3">
              <div className="space-y-1.5">
                <Label htmlFor="pf-above_threshold">{t('pricing.segmentThresholdLabel')}</Label>
                <Input id="pf-above_threshold" type="number" min={0} value={form.opt.above_threshold} onChange={e => setOpt('above_threshold', e.target.value)} />
                <p className="text-xs text-muted-foreground">{t('pricing.segmentThresholdHint')}</p>
              </div>
              <div className="space-y-2">
                <p className="flex items-center text-sm font-medium">
                  {t('pricing.segmentBaseGroup')}<OwnershipBadge>{t('pricing.ownership.generic')}</OwnershipBadge>
                </p>
                <div className="grid grid-cols-2 gap-3">
                  {ABOVE_FIELDS.map(f => (
                    <div key={f.key} className="space-y-1.5">
                      <Label htmlFor={`pf-${f.key}`}>{t(`pricing.${f.tKey}`)}</Label>
                      <Input id={`pf-${f.key}`} type="number" min={0} value={form.opt[f.key]} onChange={e => setOpt(f.key, e.target.value)} />
                    </div>
                  ))}
                </div>
              </div>
              <div className="space-y-2">
                <p className="flex items-center text-sm font-medium">
                  {t('pricing.segmentPriorityGroup')}<OwnershipBadge>{t('pricing.ownership.openai')}</OwnershipBadge>
                </p>
                <div className="grid grid-cols-2 gap-3">
                  {ABOVE_PRIORITY_FIELDS.map(f => (
                    <div key={f.key} className="space-y-1.5">
                      <Label htmlFor={`pf-${f.key}`}>{t(`pricing.${f.tKey}`)}</Label>
                      <Input id={`pf-${f.key}`} type="number" min={0} value={form.opt[f.key]} onChange={e => setOpt(f.key, e.target.value)} />
                    </div>
                  ))}
                </div>
              </div>
              <div className="space-y-2">
                <p className="flex items-center text-sm font-medium">
                  {t('pricing.segmentFlexGroup')}<OwnershipBadge>{t('pricing.ownership.openai')}</OwnershipBadge>
                </p>
                <div className="grid grid-cols-2 gap-3">
                  {ABOVE_FLEX_FIELDS.map(f => (
                    <div key={f.key} className="space-y-1.5">
                      <Label htmlFor={`pf-${f.key}`}>{t(`pricing.${f.tKey}`)}</Label>
                      <Input id={`pf-${f.key}`} type="number" min={0} value={form.opt[f.key]} onChange={e => setOpt(f.key, e.target.value)} />
                    </div>
                  ))}
                </div>
              </div>
              <p className="text-xs text-muted-foreground">{t('pricing.segmentHint')}</p>
            </TabsContent>
          </Tabs>
          {formErr && <p className="text-sm text-destructive">{formErr}</p>}
          {save.isError && errMsg(save.error) && (
            <p className="text-sm text-destructive">{errMsg(save.error)}</p>
          )}
          <DialogFooter>
            <Button variant="outline" onClick={() => setDialogOpen(false)} disabled={save.isPending}>{t('common.cancel')}</Button>
            <Button onClick={submit} disabled={save.isPending}>
              {save.isPending ? t('common.saving') : t('common.save')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* —— 图片价手动设价对话框（三分量全可选；至少一个非空） —— */}
      <Dialog open={imgDialogOpen} onOpenChange={o => { if (!o && !imgSave.isPending) setImgDialogOpen(false) }}>
        <DialogContent className="sm:max-w-md">
          <DialogHeader>
            <DialogTitle>{imgEditing ? t('pricing.image.editTitle', { model: imgEditing.Model }) : t('pricing.image.newTitle')}</DialogTitle>
            <DialogDescription>{t('pricing.image.dialogDesc')}</DialogDescription>
          </DialogHeader>
          <div className="space-y-3">
            <div className="space-y-1.5">
              <Label htmlFor="im-model">{t('pricing.modelLabel')} <span className="text-destructive">*</span></Label>
              <Input
                id="im-model"
                value={imgForm.model}
                placeholder={t('pricing.modelPlaceholder')}
                onChange={e => setImg('model', e.target.value)}
                disabled={!!imgEditing}
              />
              {imgEditing?.Source === 'litellm' && (
                <p className="text-xs text-muted-foreground">{t('pricing.takeoverHint')}</p>
              )}
            </div>
            <div className="grid grid-cols-1 gap-3">
              <div className="space-y-1.5">
                <Label htmlFor="im-input">{t('pricing.image.inputTokenLabel')}</Label>
                <Input id="im-input" type="number" min={0} step="any" value={imgForm.inputToken} onChange={e => setImg('inputToken', e.target.value)} />
              </div>
              <div className="space-y-1.5">
                <Label htmlFor="im-output">{t('pricing.image.outputTokenLabel')}</Label>
                <Input id="im-output" type="number" min={0} step="any" value={imgForm.outputToken} onChange={e => setImg('outputToken', e.target.value)} />
              </div>
              <div className="space-y-1.5">
                <Label htmlFor="im-per-image">{t('pricing.image.perImageLabel')}</Label>
                <Input id="im-per-image" type="number" min={0} step="any" value={imgForm.perImage} onChange={e => setImg('perImage', e.target.value)} />
              </div>
            </div>
          </div>
          {imgFormErr && <p className="text-sm text-destructive">{imgFormErr}</p>}
          {imgSave.isError && errMsg(imgSave.error) && (
            <p className="text-sm text-destructive">{errMsg(imgSave.error)}</p>
          )}
          <DialogFooter>
            <Button variant="outline" onClick={() => setImgDialogOpen(false)} disabled={imgSave.isPending}>{t('common.cancel')}</Button>
            <Button onClick={imgSubmit} disabled={imgSave.isPending}>
              {imgSave.isPending ? t('common.saving') : t('common.save')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* —— 按次价手动设价对话框（price_per_call 必填 ≥ 0） —— */}
      <Dialog open={fnDialogOpen} onOpenChange={o => { if (!o && !fnSave.isPending) setFnDialogOpen(false) }}>
        <DialogContent className="sm:max-w-md">
          <DialogHeader>
            <DialogTitle>{fnEditing ? t('pricing.function.editTitle', { model: fnEditing.Model }) : t('pricing.function.newTitle')}</DialogTitle>
            <DialogDescription>{t('pricing.function.dialogDesc')}</DialogDescription>
          </DialogHeader>
          <div className="space-y-3">
            <div className="space-y-1.5">
              <Label htmlFor="fn-model">{t('pricing.modelLabel')} <span className="text-destructive">*</span></Label>
              <Input
                id="fn-model"
                value={fnForm.model}
                placeholder={t('pricing.modelPlaceholder')}
                onChange={e => setFn('model', e.target.value)}
                disabled={!!fnEditing}
              />
              {fnEditing?.Source === 'litellm' && (
                <p className="text-xs text-muted-foreground">{t('pricing.takeoverHint')}</p>
              )}
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="fn-price">{t('pricing.function.priceLabel')} <span className="text-destructive">*</span></Label>
              <Input id="fn-price" type="number" min={0} step="any" value={fnForm.pricePerCall} onChange={e => setFn('pricePerCall', e.target.value)} />
            </div>
          </div>
          {fnFormErr && <p className="text-sm text-destructive">{fnFormErr}</p>}
          {fnSave.isError && errMsg(fnSave.error) && (
            <p className="text-sm text-destructive">{errMsg(fnSave.error)}</p>
          )}
          <DialogFooter>
            <Button variant="outline" onClick={() => setFnDialogOpen(false)} disabled={fnSave.isPending}>{t('common.cancel')}</Button>
            <Button onClick={fnSubmit} disabled={fnSave.isPending}>
              {fnSave.isPending ? t('common.saving') : t('common.save')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* —— 文本价删除确认（litellm 行按钮已禁用；服务端 409 兜底就地展示） —— */}
      <Dialog open={!!deleting} onOpenChange={o => { if (!o && !del.isPending) setDeleting(null) }}>
        <DialogContent className="sm:max-w-sm">
          <DialogHeader>
            <DialogTitle>{t('pricing.deleteTitle')}</DialogTitle>
            <DialogDescription>{t('pricing.deleteDesc', { model: deleting?.Model })}</DialogDescription>
          </DialogHeader>
          {del.isError && errMsg(del.error) && (
            <p className="text-sm text-destructive">{errMsg(del.error)}</p>
          )}
          <DialogFooter>
            <Button variant="outline" onClick={() => setDeleting(null)} disabled={del.isPending}>{t('common.cancel')}</Button>
            <Button variant="destructive" onClick={() => deleting && del.mutate(deleting)} disabled={del.isPending}>
              {del.isPending ? t('common.deleting') : t('pricing.deleteConfirm')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* —— 图片价删除确认 —— */}
      <Dialog open={!!imgDeleting} onOpenChange={o => { if (!o && !imgDel.isPending) setImgDeleting(null) }}>
        <DialogContent className="sm:max-w-sm">
          <DialogHeader>
            <DialogTitle>{t('pricing.deleteTitle')}</DialogTitle>
            <DialogDescription>{t('pricing.deleteDesc', { model: imgDeleting?.Model })}</DialogDescription>
          </DialogHeader>
          {imgDel.isError && errMsg(imgDel.error) && (
            <p className="text-sm text-destructive">{errMsg(imgDel.error)}</p>
          )}
          <DialogFooter>
            <Button variant="outline" onClick={() => setImgDeleting(null)} disabled={imgDel.isPending}>{t('common.cancel')}</Button>
            <Button variant="destructive" onClick={() => imgDeleting && imgDel.mutate(imgDeleting)} disabled={imgDel.isPending}>
              {imgDel.isPending ? t('common.deleting') : t('pricing.deleteConfirm')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* —— 按次价删除确认 —— */}
      <Dialog open={!!fnDeleting} onOpenChange={o => { if (!o && !fnDel.isPending) setFnDeleting(null) }}>
        <DialogContent className="sm:max-w-sm">
          <DialogHeader>
            <DialogTitle>{t('pricing.deleteTitle')}</DialogTitle>
            <DialogDescription>{t('pricing.deleteDesc', { model: fnDeleting?.Model })}</DialogDescription>
          </DialogHeader>
          {fnDel.isError && errMsg(fnDel.error) && (
            <p className="text-sm text-destructive">{errMsg(fnDel.error)}</p>
          )}
          <DialogFooter>
            <Button variant="outline" onClick={() => setFnDeleting(null)} disabled={fnDel.isPending}>{t('common.cancel')}</Button>
            <Button variant="destructive" onClick={() => fnDeleting && fnDel.mutate(fnDeleting)} disabled={fnDel.isPending}>
              {fnDel.isPending ? t('common.deleting') : t('pricing.deleteConfirm')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
}
