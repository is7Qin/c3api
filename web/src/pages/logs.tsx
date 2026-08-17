// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

import { useEffect, useMemo, useRef, useState } from 'react'
import { keepPreviousData, useQuery, useQueryClient } from '@tanstack/react-query'
import { ArrowDown, ArrowUp, FileText, RotateCcw, SlidersHorizontal } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { api } from '@/App'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { Combobox, ComboboxContent, ComboboxEmpty, ComboboxInput, ComboboxItem, ComboboxList } from '@/components/ui/combobox'
import { DateRangePicker } from '@/components/date-range-picker'
import { DropdownMenu, DropdownMenuCheckboxItem, DropdownMenuContent, DropdownMenuGroup, DropdownMenuLabel, DropdownMenuTrigger } from '@/components/ui/dropdown-menu'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { LogPagination } from '@/components/log-pagination'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { ScrollArea } from '@/components/ui/scroll-area'
import { Skeleton } from '@/components/ui/skeleton'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { Tabs, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/ui/tooltip'
import { useCursorLogs } from '@/components/use-cursor-logs'
import { defaultLogRange, formatCost, formatDateTime, toRFC3339 } from '@/components/fmt'
import { useDebounced } from '@/lib/use-debounced'
import { cn } from '@/lib/utils'
import type { ErrLogParams, UsageLogParams } from '@/lib/api/client'
import type { components } from '@/lib/api/schema'

type ErrorType = components['schemas']['ErrorType']
type RequestFormat = components['schemas']['RequestFormat']
type UsageLog = components['schemas']['UsageLog']
type ErrLog = components['schemas']['ErrLog']

// 错误类型全值域（err_logs 完整错误面：拒绝 + 异常双轨）。
const ERROR_TYPES: ErrorType[] = ['none', '429', '4xx', '5xx', 'network', 'auth', 'no_account', 'abort', 'billing']
// usage_logs 放行面只有 none/abort 两种错误类型。
const USAGE_ERROR_TYPES: ErrorType[] = ['none', 'abort']

// brief Step 1 色板：none 绿 / 4xx 黄 / 5xx、network、abort 红 / 429 橙 / auth、no_account 灰 / billing 紫。
const ERROR_META: Record<ErrorType, string> = {
  none: 'bg-emerald-500/10 text-emerald-600 dark:bg-emerald-400/10 dark:text-emerald-400',
  '4xx': 'bg-yellow-500/10 text-yellow-600 dark:bg-yellow-400/10 dark:text-yellow-400',
  '5xx': 'bg-red-500/10 text-red-600 dark:bg-red-400/10 dark:text-red-400',
  network: 'bg-red-500/10 text-red-600 dark:bg-red-400/10 dark:text-red-400',
  abort: 'bg-red-500/10 text-red-600 dark:bg-red-400/10 dark:text-red-400',
  '429': 'bg-orange-500/10 text-orange-600 dark:bg-orange-400/10 dark:text-orange-400',
  auth: 'bg-muted text-muted-foreground',
  no_account: 'bg-muted text-muted-foreground',
  billing: 'bg-violet-500/10 text-violet-600 dark:bg-violet-400/10 dark:text-violet-400',
}

function ErrorTypeBadge({ type }: { type?: ErrorType }) {
  const { t } = useTranslation()
  if (!type) return <span className="text-xs text-muted-foreground">—</span>
  return <Badge className={ERROR_META[type]}>{t(`errorType.${type}`)}</Badge>
}

// 厂商/协议专有名词不翻译（与 templates.tsx 保持一致）。
const FORMAT_LABELS: Record<RequestFormat, string> = {
  'openai-chat': 'OpenAI Chat',
  'openai-responses': 'OpenAI Responses',
  'openai-responses-ws': 'OpenAI Responses (WS)',
  'openai-images': 'OpenAI Images',
  'openai-search': 'OpenAI Search',
  anthropic: 'Anthropic',
}

// 表头样式（sub2api 配方）：uppercase 小字 + sticky（评审 Minor-1：必须位于
// 纵向滚动容器内——纯 overflow-x 容器中 sticky top 不生效）。
function Th({ className, ...props }: React.ComponentProps<typeof TableHead>) {
  return (
    <TableHead
      className={cn(
        'sticky top-0 z-10 bg-background text-xs uppercase tracking-wider text-muted-foreground',
        className
      )}
      {...props}
    />
  )
}

// 延迟健康色（仅色点着色，阈值应用于 TTFT）：<1s 绿 / <5s 黄 / <15s 橙 / 以上红。
function latencyColor(ms: number): string {
  if (ms < 1000) return 'bg-emerald-500'
  if (ms < 5000) return 'bg-amber-500'
  if (ms < 15000) return 'bg-orange-500'
  return 'bg-red-500'
}

// 时长格式化：≥1000ms 用 s（保留 1 位小数），否则 ms。
const fmtDuration = (ms: number): string => (ms >= 1000 ? `${(ms / 1000).toFixed(1)}s` : `${ms}ms`)

// 单价格式化：每 M token 毫分 → USD/M（≥0.01 四位小数，否则六位，去尾零）。
const fmtPricePerM = (millis: number): string => {
  const usd = millis / 100_000
  const s = (usd >= 0.01 ? usd.toFixed(4) : usd.toFixed(6)).replace(/\.?0+$/, '')
  return `$${s}/M`
}

// 行内 token 紧凑格式：K/M/B 分级（1 位小数去尾零），<1000 原始值。
// 阈值留四舍五入余量（999.95K+ 升 M、999.95M+ 升 B），避免 999999 → "1000K"
// 的进位断裂；只有 K 时大数值（1000K/1234567K）会撑破单元格被裁。
// 大卡内保持千分位原始值（toLocaleString，不改）。
const fmtTokens = (n: number): string => {
  const trim = (v: number) => `${v.toFixed(1).replace(/\.0$/, '')}`
  if (n >= 999.95e6) return `${trim(n / 1e9)}B`
  if (n >= 999.95e3) return `${trim(n / 1e6)}M`
  if (n >= 1e3) return `${trim(n / 1e3)}K`
  return String(n)
}

// base-ui Select 不接受空串值，用哨兵表示「全部」。
const ERROR_ALL = '__all__'
const FORMAT_ALL = '__all__'

// 可隐藏列（时间/请求 ID 始终可见，参考 sub2api 使用明细的列设置模式）；
// BillingTier/AboveHit/Overdraft 已并入 Tokens 悬停窗（不再独立列）。
// 隐藏选择持久化到 localStorage（logs-hidden-columns）。用量/错误两 Tab 列集不同。
const HIDDEN_STORAGE_KEY = 'logs-hidden-columns'
const USAGE_HIDDENABLE_COLS = ['user', 'key', 'group', 'account', 'model', 'format', 'errorType', 'cost', 'latency', 'tokens'] as const
const ERR_HIDDENABLE_COLS = ['user', 'key', 'group', 'account', 'model', 'format', 'statusCode', 'errorType', 'errorMessage', 'latency', 'billingTier'] as const

function loadHiddenCols(): Set<string> {
  try {
    const raw = localStorage.getItem(HIDDEN_STORAGE_KEY)
    if (raw) return new Set(JSON.parse(raw) as string[])
  } catch { /* 损坏数据忽略 */ }
  return new Set()
}

interface LogFilters {
  user_id: string
  key_id: string
  group_id: string
  account_id: string
  model: string
  format: string
  error_type: string
  status_code: string
  from: string
  to: string
}

const emptyFilters = (): LogFilters => ({
  user_id: '', key_id: '', group_id: '', account_id: '', model: '', format: '', error_type: '', status_code: '', ...defaultLogRange(),
})

// —— 筛选候选搜索（user/key/group/account 四筛选器，2026-08-16 改造）——
// 服务端搜索：输入防抖 300ms 后按词查询候选（≤20 条），无全量拉取——候选
// 完整性不随实体总量衰减。user/group/account 用端点既有模糊参数（email/name），
// key 用 /admin/keys 脱敏端点并携带已选 user/group 关联收窄（同名 key 区分度）。
interface FilterOption { id: number; label: string }
interface FilterState { user: string; key: string; group: string; account: string }
// 展开态与搜索词同键但值为布尔（原误复用 FilterState，tsc 全项目失败）。
interface FilterOpenState { user: boolean; key: boolean; group: boolean; account: boolean }

// 搜索型筛选框（base-ui Combobox 受控封装）：value 为字符串 ID（与 filters
// 直存同构）；label 经 itemToStringLabel 查 id→label 缓存展示——候选刷新后
// 已选项仍显示名称，不退化 #id。filter 恒真关本地过滤（搜索语义归服务端，
// 与 ILIKE 模糊一致）；autoComplete="none" 防输入时自动补全干扰。
function FilterCombobox({
  id,
  placeholder,
  value,
  onSelect,
  options,
  loading,
  open,
  onOpenChange,
  onInputChange,
}: {
  id: string
  placeholder: string
  value: string
  onSelect: (v: string) => void
  options: FilterOption[]
  loading: boolean
  open: boolean
  onOpenChange: (open: boolean) => void
  onInputChange: (term: string) => void
}) {
  const { t } = useTranslation()
  // id → label 缓存（候选刷新后已选项仍能显示名称）
  const labelCache = useRef(new Map<string, string>())
  if (options.length) for (const o of options) labelCache.current.set(String(o.id), o.label)
  return (
    <Combobox
      // items 必须与 ComboboxItem.value 同源同类型（base-ui 内部按 items 解析索引，
      // 对象数组 + 字符串 value 会导致选中链路失败）；字符串数组 + 恒真过滤 =
      // 搜索语义归服务端，本地不过滤。
      items={options.map(o => String(o.id))}
      filter={() => true}
      autoComplete="none"
      // 受控 API 是 value/onValueChange（selectedValue/onSelectedValueChange 已被
      // base-ui 1.7 Omit——传错名字会静默退化为非受控，回调永不触发，曾踩坑）
      value={value || null}
      onValueChange={v => onSelect(v ?? '')}
      itemToStringLabel={v => labelCache.current.get(v) ?? v}
      open={open}
      onOpenChange={onOpenChange}
    >
      <ComboboxInput id={id} placeholder={placeholder} showClear onChange={e => onInputChange(e.target.value)} />
      <ComboboxContent>
        {/* base-ui Empty 的 children 由内部 filteredItems 判定（非空 → children=null
            空壳 div）——渲染条件必须与之一致：仅 options 真为空时渲染；loading
            期间 keepPreviousData 保留旧候选（options 非空）→ 不渲染 → 无空壳；
            options 空时 base-ui filteredItems 同空 → children 正常显示 */}
        {options.length === 0 && (
          <ComboboxEmpty>{loading ? t('logs.filter.searching') : t('logs.filter.noMatch')}</ComboboxEmpty>
        )}
        <ComboboxList>
          {options.map(o => (
            <ComboboxItem key={o.id} value={String(o.id)}>
              <span className="min-w-0 truncate">{o.label}</span>
            </ComboboxItem>
          ))}
        </ComboboxList>
      </ComboboxContent>
    </Combobox>
  )
}

export default function Logs() {
  const { t } = useTranslation()
  const [tab, setTab] = useState<'usage' | 'errors'>('usage')
  const [filters, setFilters] = useState<LogFilters>(emptyFilters)
  const [limit, setLimit] = useState(20)

  // 过滤条件 / 每页条数变化 → hook 参数键变化自动重置回第 1 页（游标链自持，调用方只传派生值）。
  const set = (patch: Partial<LogFilters>) => setFilters(f => ({ ...f, ...patch }))
  const changeLimit = (v: number) => setLimit(v)
  // 纯输入防抖字段（model/status_code 逐键输入）：输入即时反映在受控框，查询参数
  // 300ms 合并后再触发（避免每键一次后端查询——与候选搜索同频）。
  const debouncedModel = useDebounced(filters.model, 300)
  const debouncedStatus = useDebounced(filters.status_code, 300)
  // Tab 切换：各自独立游标链（hook 按参数变化重置）；usage 面错误类型值域收窄为
  // none/abort，超出值重置（收窄逻辑属调用方，hook 只收归一后参数）。
  const switchTab = (v: string) => {
    setTab(v as 'usage' | 'errors')
    if (v === 'usage' && filters.error_type && !USAGE_ERROR_TYPES.includes(filters.error_type as ErrorType)) {
      setFilters(f => ({ ...f, error_type: '' }))
    }
  }

  // 参数对象随 filter/limit/tab 派生（游标由 hook 注入）。管理端 7 字段全筛
  // （user/key/group/account 数字输入 + model/format/errorType）+ 错误面 status_code 专属。
  // model/status_code 走防抖值（useDebounced——300ms 合并逐键输入）。
  const { usageParams, errParams } = useMemo(() => {
    const base: UsageLogParams = {
      user_id: filters.user_id ? Number(filters.user_id) : undefined,
      key_id: filters.key_id ? Number(filters.key_id) : undefined,
      group_id: filters.group_id ? Number(filters.group_id) : undefined,
      account_id: filters.account_id ? Number(filters.account_id) : undefined,
      model: debouncedModel || undefined,
      format: filters.format || undefined,
      error_type: filters.error_type || undefined,
      from: toRFC3339(filters.from) ?? '',
      to: toRFC3339(filters.to) ?? '',
      limit,
    }
    return {
      usageParams: base,
      errParams: {
        ...base,
        // Number('e')=NaN 会以 'NaN' 字符串发送 → 服务端 400；isFinite 过滤为 undefined。
        status_code: debouncedStatus && Number.isFinite(Number(debouncedStatus)) ? Number(debouncedStatus) : undefined,
      } satisfies ErrLogParams,
    }
  }, [filters, debouncedModel, debouncedStatus, limit])

  // 游标链分页：替代 useQuery + 自计页号；fetchPage 注入（hook 不感知 API 层）。
  const { page, rows, loadedPages, hasNext, isLoading, isFetching, isError, error, goNext, goPrev, goLatest, goToPage } = useCursorLogs<UsageLog | ErrLog>(
    [tab, usageParams, errParams],
    (cursor: number | null) =>
      tab === 'errors'
        ? api.getErrLogs({ ...errParams, cursor: cursor ?? undefined })
        : api.getUsageLogs({ ...usageParams, cursor: cursor ?? undefined }),
  )

  // —— 名称映射：日志行只存 ID，组/账号列显示名称（未命中回退 #id）——
  // 全量拉取（上限 1000，超出部分仅影响展示回退数字）；5 分钟缓存避免每页刷新重查。
  const { data: groupNameById } = useQuery({
    queryKey: ['groups', { limit: 1000 }],
    queryFn: () => api.listGroups({ limit: 1000 }),
    select: data => new Map(data.rows.map(g => [g.ID, g.Name])),
    staleTime: 5 * 60 * 1000,
  })
  const { data: accountNameById } = useQuery({
    queryKey: ['accounts', { limit: 1000 }],
    queryFn: () => api.listAccounts({ limit: 1000 }),
    select: data => new Map(data.rows.map(a => [a.ID, a.Name])),
    staleTime: 5 * 60 * 1000,
  })
  // 用户列：id → 邮箱（sub2api 使用明细同款，邮箱太长截断 + title 悬停全文）
  const { data: userEmailById } = useQuery({
    queryKey: ['users', { limit: 1000 }],
    queryFn: () => api.listUsers({ limit: 1000 }),
    select: data => new Map(data.rows.map(u => [u.ID, u.Email ?? ''])),
    staleTime: 5 * 60 * 1000,
  })

  // —— 筛选候选（服务端搜索；打开才查，防抖词驱动；key 携带已选 user/group
  // 关联收窄——同名 key 在已选用户/分组范围内仍可区分）——
  const [search, setSearch] = useState<FilterState>({ user: '', key: '', group: '', account: '' })
  const debouncedSearch = useDebounced(search, 300)
  const [filterOpen, setFilterOpen] = useState<FilterOpenState>({ user: false, key: false, group: false, account: false })
  const openFilter = (k: keyof FilterState, open: boolean) => setFilterOpen(p => ({ ...p, [k]: open }))
  const setSearchTerm = (k: keyof FilterState) => (term: string) => setSearch(s => ({ ...s, [k]: term }))
  // 挂载预取空条件候选（20 条）：打开筛选器瞬间即有列表，避免 popup 先空后内容
  // 的闪烁（enabled 打开才查 + 预取缓存命中——queryKey 与打开时一致）
  const qc = useQueryClient()
  useEffect(() => {
    const prefetch = (key: unknown[], fn: () => Promise<unknown>) => void qc.prefetchQuery({ queryKey: key, queryFn: fn, staleTime: 60_000 })
    prefetch(['logs-filter', 'users', { term: '' }], () => api.listUsers({ limit: 20 }))
    prefetch(['logs-filter', 'groups', { term: '' }], () => api.listGroups({ limit: 20 }))
    prefetch(['logs-filter', 'accounts', { term: '' }], () => api.listAccounts({ limit: 20 }))
    prefetch(['logs-filter', 'keys', { term: '', user_id: '', group_id: '' }], () => api.listKeys({ limit: 20 }))
  }, [qc])

  const userCandidates = useQuery({
    queryKey: ['logs-filter', 'users', { term: debouncedSearch.user }],
    queryFn: () => api.listUsers({ email: debouncedSearch.user || undefined, limit: 20 }),
    enabled: filterOpen.user,
    placeholderData: keepPreviousData,
    staleTime: 60_000,
    select: r => r.rows.map(u => ({ id: u.ID, label: u.Email ?? `#${u.ID}` })),
  })
  const groupCandidates = useQuery({
    queryKey: ['logs-filter', 'groups', { term: debouncedSearch.group }],
    queryFn: () => api.listGroups({ name: debouncedSearch.group || undefined, limit: 20 }),
    enabled: filterOpen.group,
    placeholderData: keepPreviousData,
    staleTime: 60_000,
    select: r => r.rows.map(g => ({ id: g.ID, label: g.Name || `#${g.ID}` })),
  })
  const accountCandidates = useQuery({
    queryKey: ['logs-filter', 'accounts', { term: debouncedSearch.account }],
    queryFn: () => api.listAccounts({ name: debouncedSearch.account || undefined, limit: 20 }),
    enabled: filterOpen.account,
    placeholderData: keepPreviousData,
    staleTime: 60_000,
    select: r => r.rows.map(a => ({ id: a.ID, label: a.Name || `#${a.ID}` })),
  })
  // key 候选：已选 user/group 筛选条件下候选限缩（关联收窄；name 搜索同构）
  const keyCandidates = useQuery({
    queryKey: ['logs-filter', 'keys', { term: debouncedSearch.key, user_id: filters.user_id, group_id: filters.group_id }],
    queryFn: () => api.listKeys({
      name: debouncedSearch.key || undefined,
      user_id: filters.user_id ? Number(filters.user_id) : undefined,
      group_id: filters.group_id ? Number(filters.group_id) : undefined,
      limit: 20,
    }),
    enabled: filterOpen.key,
    placeholderData: keepPreviousData,
    staleTime: 60_000,
    select: r => r.rows.map(k => ({ id: k.ID, label: k.Name || `#${k.ID}` })),
  })

  // —— 列可见性（localStorage 持久化）——
  const [hiddenCols, setHiddenCols] = useState<Set<string>>(loadHiddenCols)
  const toggleCol = (key: string) => {
    setHiddenCols(prev => {
      const next = new Set(prev)
      if (next.has(key)) next.delete(key)
      else next.add(key)
      try { localStorage.setItem(HIDDEN_STORAGE_KEY, JSON.stringify([...next])) } catch { /* 忽略 */ }
      return next
    })
  }
  const isColVisible = (key: string) => !hiddenCols.has(key)

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">{t('logs.title')}</h1>
        <p className="text-sm text-muted-foreground">{t('logs.subtitle')}</p>
      </div>

      {/* Tab 切换：用量日志 / 错误日志（两表独立游标与列集） */}
      <Tabs value={tab} onValueChange={v => v && switchTab(v)}>
        <TabsList>
          <TabsTrigger value="usage">{t('logs.tab.usage')}</TabsTrigger>
          <TabsTrigger value="errors">{t('logs.tab.errors')}</TabsTrigger>
        </TabsList>
      </Tabs>

      {/* 过滤栏：分组/账号/模型/错误类型（+错误面状态码）+ 时间范围 */}
      <Card className="p-4">
        <div className="grid grid-cols-2 gap-3 md:grid-cols-4 xl:grid-cols-8">
          <div className="space-y-1.5">
            <Label htmlFor="log-user">{t('logs.filter.userId')}</Label>
            <FilterCombobox
              id="log-user"
              placeholder={t('logs.filter.searchHint')}
              value={filters.user_id}
              onSelect={v => set({ user_id: v })}
              options={userCandidates.data ?? []}
              loading={userCandidates.isFetching}
              open={filterOpen.user}
              onOpenChange={o => openFilter('user', o)}
              onInputChange={setSearchTerm('user')}
            />
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="log-key">{t('logs.filter.keyId')}</Label>
            <FilterCombobox
              id="log-key"
              placeholder={t('logs.filter.searchHint')}
              value={filters.key_id}
              onSelect={v => set({ key_id: v })}
              options={keyCandidates.data ?? []}
              loading={keyCandidates.isFetching}
              open={filterOpen.key}
              onOpenChange={o => openFilter('key', o)}
              onInputChange={setSearchTerm('key')}
            />
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="log-group">{t('logs.filter.groupId')}</Label>
            <FilterCombobox
              id="log-group"
              placeholder={t('logs.filter.searchHint')}
              value={filters.group_id}
              onSelect={v => set({ group_id: v })}
              options={groupCandidates.data ?? []}
              loading={groupCandidates.isFetching}
              open={filterOpen.group}
              onOpenChange={o => openFilter('group', o)}
              onInputChange={setSearchTerm('group')}
            />
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="log-account">{t('logs.filter.accountId')}</Label>
            <FilterCombobox
              id="log-account"
              placeholder={t('logs.filter.searchHint')}
              value={filters.account_id}
              onSelect={v => set({ account_id: v })}
              options={accountCandidates.data ?? []}
              loading={accountCandidates.isFetching}
              open={filterOpen.account}
              onOpenChange={o => openFilter('account', o)}
              onInputChange={setSearchTerm('account')}
            />
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="log-model">{t('logs.filter.model')}</Label>
            <Input id="log-model" placeholder="gpt-4o" value={filters.model} onChange={e => set({ model: e.target.value })} />
          </div>
          <div className="space-y-1.5">
            <Label>{t('logs.filter.format')}</Label>
            <Select
              items={Object.fromEntries([[FORMAT_ALL, t('logs.filter.all')], ...(Object.keys(FORMAT_LABELS) as RequestFormat[]).map(f => [f, FORMAT_LABELS[f]])])}
              value={filters.format || FORMAT_ALL}
              onValueChange={v => set({ format: v === FORMAT_ALL ? '' : v })}
            >
              <SelectTrigger className="w-full"><SelectValue /></SelectTrigger>
              <SelectContent>
                <SelectItem value={FORMAT_ALL} label={t('logs.filter.all')}>{t('logs.filter.all')}</SelectItem>
                {(Object.keys(FORMAT_LABELS) as RequestFormat[]).map(f => <SelectItem key={f} value={f} label={FORMAT_LABELS[f]}>{FORMAT_LABELS[f]}</SelectItem>)}
              </SelectContent>
            </Select>
          </div>
          <div className="space-y-1.5">
            <Label>{t('logs.filter.errorType')}</Label>
            <Select
              items={Object.fromEntries([[ERROR_ALL, t('logs.filter.all')], ...(tab === 'errors' ? ERROR_TYPES : USAGE_ERROR_TYPES).map(et => [et, t(`errorType.${et}`)])])}
              value={filters.error_type || ERROR_ALL}
              onValueChange={v => set({ error_type: v === ERROR_ALL ? '' : v })}
            >
              <SelectTrigger className="w-full"><SelectValue /></SelectTrigger>
              <SelectContent>
                <SelectItem value={ERROR_ALL} label={t('logs.filter.all')}>{t('logs.filter.all')}</SelectItem>
                {(tab === 'errors' ? ERROR_TYPES : USAGE_ERROR_TYPES).map(et => <SelectItem key={et} value={et} label={t(`errorType.${et}`)}>{t(`errorType.${et}`)}</SelectItem>)}
              </SelectContent>
            </Select>
          </div>
          {tab === 'errors' && (
            <div className="space-y-1.5">
              <Label htmlFor="log-status">{t('logs.filter.statusCode')}</Label>
              <Input id="log-status" type="number" min={0} placeholder="429" value={filters.status_code} onChange={e => set({ status_code: e.target.value })} />
            </div>
          )}
          <div className="space-y-1.5">
            <Label>{t('dateRange.label')}</Label>
            <DateRangePicker value={{ from: filters.from, to: filters.to }} onChange={v => set(v)} />
          </div>
          <div className="flex items-end">
            <Button
              variant="outline"
              className="w-full"
              onClick={() => {
                setFilters(emptyFilters())
                // 候选搜索词/展开态一并复位（筛选清空后候选不残留旧词）
                setSearch({ user: '', key: '', group: '', account: '' })
                setFilterOpen({ user: false, key: false, group: false, account: false })
              }}
            >
              <RotateCcw /> {t('logs.filter.reset')}
            </Button>
          </div>
        </div>
      </Card>

      {/* 列设置 + 表格标题（游标分页无 total，标题用当前 Tab 名） */}
      <div className="flex items-center justify-between gap-2">
        <h2 className="text-sm font-medium text-muted-foreground">{t(tab === 'errors' ? 'logs.tab.errors' : 'logs.tab.usage')}</h2>
        <DropdownMenu>
          <DropdownMenuTrigger render={<Button variant="outline" size="sm"><SlidersHorizontal className="size-4" />{t('logs.columnSettings')}</Button>} />
          <DropdownMenuContent align="end" className="max-h-80 w-48">
            <DropdownMenuGroup>
              <DropdownMenuLabel>{t('logs.columnSettings')}</DropdownMenuLabel>
              {(tab === 'errors' ? ERR_HIDDENABLE_COLS : USAGE_HIDDENABLE_COLS).map(key => (
                <DropdownMenuCheckboxItem
                  key={key}
                  checked={isColVisible(key)}
                  onCheckedChange={() => toggleCol(key)}
                >
                  {t(`logs.table.${key}`)}
                </DropdownMenuCheckboxItem>
              ))}
            </DropdownMenuGroup>
          </DropdownMenuContent>
        </DropdownMenu>
      </div>

      {isError ? (
        <p className="text-sm text-destructive">{t('common.loadFailed', { message: (error as Error).message })}</p>
      ) : isLoading ? (
        <Card>
          <div className="space-y-2 p-4">
            {Array.from({ length: 4 }).map((_, i) => <Skeleton key={i} className="h-10" />)}
          </div>
        </Card>
      ) : rows.length === 0 ? (
        <Card className="flex flex-col items-center gap-2 py-12 text-muted-foreground">
          <FileText className="size-10" />
          <p className="font-medium">{t('logs.emptyTitle')}</p>
          <p className="text-sm">{t('logs.emptyDesc')}</p>
        </Card>
      ) : (
        <>
        <Card className="overflow-hidden">
          {/* 纵横向滚动均走 ScrollArea 自绘滚动条（深色模式统一观感） */}
          <ScrollArea className="max-h-[calc(100vh-16rem)]" showHorizontal>
          {/* overflow-x-visible 覆盖 Table 默认 overflow-x-auto（twMerge）：横向
              滚动交由 ScrollArea viewport，避免嵌套滚动条 */}
          <Table containerClassName="overflow-x-visible">
            <TableHeader>
              <TableRow>
                <Th>{t('logs.table.requestId')}</Th>
                <Th>{t('logs.table.createdAt')}</Th>
                {isColVisible('user') && <Th className="text-right">{t('logs.table.user')}</Th>}
                {isColVisible('key') && <Th className="text-right">{t('logs.table.key')}</Th>}
                {isColVisible('group') && <Th className="text-right">{t('logs.table.group')}</Th>}
                {isColVisible('account') && <Th className="text-right">{t('logs.table.account')}</Th>}
                {isColVisible('model') && <Th>{t('logs.table.model')}</Th>}
                {isColVisible('format') && <Th>{t('logs.table.format')}</Th>}
                {tab === 'errors' && isColVisible('statusCode') && <Th className="text-right">{t('logs.table.statusCode')}</Th>}
                {isColVisible('errorType') && <Th>{t('logs.table.errorType')}</Th>}
                {tab === 'errors' && isColVisible('errorMessage') && <Th>{t('logs.table.errorMessage')}</Th>}
                {/* 表头顺序与单元格渲染一致（usage: tokens→cost→latency；errors: latency→billingTier）——
                    Token/费用曾按用户要求移到耗时前，表头漏同步导致错位 */}
                {tab === 'usage' && isColVisible('tokens') && <Th className="text-right">{t('logs.table.tokens')}</Th>}
                {tab === 'usage' && isColVisible('cost') && <Th className="text-right">{t('logs.table.cost')}</Th>}
                {isColVisible('latency') && <Th className="text-right">{t('logs.table.latency')}</Th>}
                {tab === 'errors' && isColVisible('billingTier') && <Th>{t('logs.table.billingTier')}</Th>}
                <Th>{t('logs.table.ip')}</Th>
              </TableRow>
            </TableHeader>
            <TableBody className="[&_td]:py-3">
              {tab === 'usage'
                ? (rows as UsageLog[]).map(l => (
                <TableRow key={l.ID}>
                  <TableCell className="max-w-36">
                    <span className="block truncate font-mono text-xs text-muted-foreground" title={l.RequestID}>{l.RequestID ?? '—'}</span>
                  </TableCell>
                  <TableCell className="text-xs text-muted-foreground whitespace-nowrap">{formatDateTime(l.CreatedAt)}</TableCell>
                  {/* 鉴权归属：用户(邮箱)/Key；组/账号显示名称，未命中回退 #id（0 = 无鉴权） */}
                  {isColVisible('user') && (
                    <TableCell className="text-right">
                      {l.UserID ? (
                        <span className="inline-block max-w-40 truncate align-middle tabular-nums" title={userEmailById?.get(l.UserID)}>
                          {userEmailById?.get(l.UserID) ?? `#${l.UserID}`}
                        </span>
                      ) : '—'}
                    </TableCell>
                  )}
                  {isColVisible('key') && <TableCell className="text-right tabular-nums">{l.KeyID ? `#${l.KeyID}` : '—'}</TableCell>}
                  {isColVisible('group') && (
                    <TableCell className="text-right">
                      {l.GroupID ? <span className="tabular-nums">{groupNameById?.get(l.GroupID) ?? `#${l.GroupID}`}</span> : '—'}
                    </TableCell>
                  )}
                  {isColVisible('account') && (
                    <TableCell className="text-right">
                      {l.AccountID ? <span className="tabular-nums">{accountNameById?.get(l.AccountID) ?? `#${l.AccountID}`}</span> : '—'}
                    </TableCell>
                  )}
                  {/* 模型链式（sub2api 纵向链）：请求模型加粗 + 映射模型缩进灰（有值才显示 ↳）；
                      超长 truncate 隐藏 + title 悬停全文（与用户列邮箱同做法） */}
                  {isColVisible('model') && (
                  <TableCell>
                    <div className="space-y-0.5 text-xs">
                      <div className="max-w-40 truncate font-medium" title={l.Model}>{l.Model ?? '—'}</div>
                      {l.MappedModel && (
                        <div className="max-w-40 truncate pl-3 text-muted-foreground" title={l.MappedModel}>↳{l.MappedModel}</div>
                      )}
                    </div>
                  </TableCell>
                  )}
                  {isColVisible('format') && (
                  <TableCell>
                    {l.Format ? <Badge variant="outline">{FORMAT_LABELS[l.Format]}</Badge> : <span className="text-xs text-muted-foreground">—</span>}
                  </TableCell>
                  )}
                  {isColVisible('errorType') && <TableCell><ErrorTypeBadge type={l.ErrorType} /></TableCell>}
                  {/* token 列：↓输入 ↑输出（muted 单色收敛）+ cache 第二行（无值不显示）+ ⓘ 悬停大卡
                      （tokens 明细 + 档位 BillingTier + 超档/透支徽章） */}
                  {isColVisible('tokens') && (
                  <TableCell className="text-right font-medium tabular-nums">
                    {l.InputTokens || l.OutputTokens || l.CacheReadTokens || l.CacheCreationTokens ? (
                      <span className="inline-flex items-center justify-end gap-1.5">
                        <span className="space-y-0.5 text-xs text-right">
                          <span className="inline-flex items-center gap-2 text-muted-foreground">
                            <span className="inline-flex items-center gap-0.5">
                              <ArrowDown className="size-3" />{fmtTokens(l.InputTokens ?? 0)}
                            </span>
                            <span className="inline-flex items-center gap-0.5">
                              <ArrowUp className="size-3" />{fmtTokens(l.OutputTokens ?? 0)}
                            </span>
                          </span>
                          {l.CacheReadTokens || l.CacheCreationTokens ? (
                            <div className="text-right">
                              {l.CacheReadTokens ? <span className="text-blue-500/70">{t('logs.tokens.read')} {fmtTokens(l.CacheReadTokens)}</span> : null}
                              {l.CacheReadTokens && l.CacheCreationTokens ? <span className="mx-1 text-muted-foreground/40">·</span> : null}
                              {l.CacheCreationTokens ? <span className="text-amber-500/70">{t('logs.tokens.write')} {fmtTokens(l.CacheCreationTokens)}</span> : null}
                            </div>
                          ) : null}
                        </span>
                        {/* delay 0 立即弹出（base-ui 默认 600ms 偏慢）；触发热区 -m-1 p-1 扩大
                            （16px 圆点视觉不变，命中面积 36px） */}
                        <Tooltip>
                          <TooltipTrigger delay={0} render={<span className="inline-flex -m-1 size-4 shrink-0 cursor-help items-center justify-center rounded-full bg-muted p-1 text-muted-foreground text-[10px] leading-none" />}>
                            i
                          </TooltipTrigger>
                          {/* bg-popover 必须配 text-popover-foreground（跟随主题：浅色白底黑字/深色黑底白字）；
                              勿用默认反色 bg-foreground/text-background（浅色黑卡深色白卡，突兀） */}
                          <TooltipContent className="max-w-xs border bg-popover p-0 text-popover-foreground shadow-lg">
                            <div className="space-y-1.5 p-3 text-xs">
                              {/* 单价小字尾注：$0.0025/M（每 M token；null = 未计费路径不显示） */}
                              <div className="flex items-center justify-between gap-6">
                                <span className="text-muted-foreground">{t('logs.tokens.input')}</span>
                                <span className="flex items-baseline gap-2">
                                  <span className="font-medium tabular-nums">{(l.InputTokens ?? 0).toLocaleString()}</span>
                                  {l.PriceInputMillis != null && <span className="text-[11px] tabular-nums text-muted-foreground">{fmtPricePerM(l.PriceInputMillis)}</span>}
                                </span>
                              </div>
                              <div className="flex items-center justify-between gap-6">
                                <span className="text-muted-foreground">{t('logs.tokens.output')}</span>
                                <span className="flex items-baseline gap-2">
                                  <span className="font-medium tabular-nums">{(l.OutputTokens ?? 0).toLocaleString()}</span>
                                  {l.PriceOutputMillis != null && <span className="text-[11px] tabular-nums text-muted-foreground">{fmtPricePerM(l.PriceOutputMillis)}</span>}
                                </span>
                              </div>
                              {l.CacheReadTokens ? (
                                <div className="flex items-center justify-between gap-6">
                                  <span className="text-muted-foreground">{t('logs.tokens.cacheRead')}</span>
                                  <span className="flex items-baseline gap-2">
                                    <span className="font-medium tabular-nums">{l.CacheReadTokens.toLocaleString()}</span>
                                    {l.PriceCacheReadMillis != null && <span className="text-[11px] tabular-nums text-muted-foreground">{fmtPricePerM(l.PriceCacheReadMillis)}</span>}
                                  </span>
                                </div>
                              ) : null}
                              {l.CacheCreationTokens ? (
                                <div className="flex items-center justify-between gap-6">
                                  <span className="text-muted-foreground">{t('logs.tokens.cacheWrite')}</span>
                                  <span className="flex items-baseline gap-2">
                                    <span className="font-medium tabular-nums">{l.CacheCreationTokens.toLocaleString()}</span>
                                    {l.PriceCacheCreationMillis != null && <span className="text-[11px] tabular-nums text-muted-foreground">{fmtPricePerM(l.PriceCacheCreationMillis)}</span>}
                                  </span>
                                </div>
                              ) : null}
                              <div className="flex items-center justify-between gap-6 border-t pt-1.5">
                                <span className="text-muted-foreground">{t('logs.tokens.total')}</span>
                                {/* 动态求和（不依赖 TotalTokens 字段——某行该字段缺失/为 null 时总计仍正确） */}
                                <span className="font-semibold tabular-nums">
                                  {((l.InputTokens ?? 0) + (l.OutputTokens ?? 0) + (l.CacheReadTokens ?? 0) + (l.CacheCreationTokens ?? 0)).toLocaleString()}
                                </span>
                              </div>
                              {/* 计费信息并入：档位 + 超档/透支徽章 */}
                              <div className="flex items-center justify-between gap-6 border-t pt-1.5">
                                <span className="text-muted-foreground">{t('logs.table.billingTier')}</span>
                                {l.BillingTier ? (
                                  <Badge variant="outline">{l.BillingTier}</Badge>
                                ) : (
                                  <span className="text-muted-foreground">—</span>
                                )}
                              </div>
                              {(l.AboveHit || l.Overdraft) && (
                                <div className="flex items-center justify-end gap-1">
                                  {l.AboveHit && <Badge className="bg-sky-500/10 text-sky-600 dark:bg-sky-400/10 dark:text-sky-400">{t('logs.table.aboveHit')}</Badge>}
                                  {l.Overdraft && <Badge className="bg-rose-500/10 text-rose-600 dark:bg-rose-400/10 dark:text-rose-400">{t('logs.table.overdraft')}</Badge>}
                                </div>
                              )}
                            </div>
                          </TooltipContent>
                        </Tooltip>
                      </span>
                    ) : (
                      <span className="text-xs text-muted-foreground">—</span>
                    )}
                  </TableCell>
                  )}
                  {/* 计费：Cost 毫分 → USD（0/空显示 —）；档位/超档/透支已并入 Tokens 悬停窗 */}
                  {isColVisible('cost') && <TableCell className="text-right tabular-nums">{formatCost(l.Cost)}</TableCell>}
                  {/* 耗时列：上行 TTFT（色点按 ttft 着色 + ≥1000ms 用 s）+ 下行总耗时；ttft 无值只显示总耗时 */}
                  {isColVisible('latency') && (
                  <TableCell className="text-right tabular-nums">
                    {l.TTFTMS != null ? (
                      <div className="space-y-0.5 text-right text-xs">
                        <div className="inline-flex items-center justify-end gap-1.5">
                          <span className={cn('size-2 rounded-full', latencyColor(l.TTFTMS))} />
                          <span className="text-muted-foreground">{t('logs.latency.ttft')} {fmtDuration(l.TTFTMS)}</span>
                        </div>
                        <div className="text-muted-foreground/60">{t('logs.latency.total')} {fmtDuration(l.LatencyMS)}</div>
                      </div>
                    ) : l.LatencyMS != null ? (
                      <span className="text-muted-foreground">{fmtDuration(l.LatencyMS)}</span>
                    ) : (
                      <span className="text-xs text-muted-foreground">—</span>
                    )}
                  </TableCell>
                  )}
                  {/* 客户端 IP（CF-Connecting-IP 优先；无则 RemoteAddr）；IPv6 超长 truncate */}
                  <TableCell className="max-w-36">
                    <span className="block truncate font-mono text-xs text-muted-foreground" title={l.ClientIP}>{l.ClientIP ?? '—'}</span>
                  </TableCell>
                </TableRow>
                ))
                : (rows as ErrLog[]).map(l => (
                <TableRow key={l.ID}>
                  <TableCell className="max-w-36">
                    <span className="block truncate font-mono text-xs text-muted-foreground" title={l.RequestID}>{l.RequestID ?? '—'}</span>
                  </TableCell>
                  <TableCell className="text-xs text-muted-foreground whitespace-nowrap">{formatDateTime(l.CreatedAt)}</TableCell>
                  {/* 鉴权归属：用户(邮箱)/Key；组/账号显示名称，未命中回退 #id（0 = 无鉴权） */}
                  {isColVisible('user') && (
                    <TableCell className="text-right">
                      {l.UserID ? (
                        <span className="inline-block max-w-40 truncate align-middle tabular-nums" title={userEmailById?.get(l.UserID)}>
                          {userEmailById?.get(l.UserID) ?? `#${l.UserID}`}
                        </span>
                      ) : '—'}
                    </TableCell>
                  )}
                  {isColVisible('key') && <TableCell className="text-right tabular-nums">{l.KeyID ? `#${l.KeyID}` : '—'}</TableCell>}
                  {isColVisible('group') && (
                    <TableCell className="text-right">
                      {l.GroupID ? <span className="tabular-nums">{groupNameById?.get(l.GroupID) ?? `#${l.GroupID}`}</span> : '—'}
                    </TableCell>
                  )}
                  {isColVisible('account') && (
                    <TableCell className="text-right">
                      {l.AccountID ? <span className="tabular-nums">{accountNameById?.get(l.AccountID) ?? `#${l.AccountID}`}</span> : '—'}
                    </TableCell>
                  )}
                  {/* 错误面模型无映射链（ErrLog 无 MappedModel）：单行 truncate + title 悬停 */}
                  {isColVisible('model') && (
                  <TableCell>
                    <div className="max-w-40 truncate text-xs font-medium" title={l.Model}>{l.Model ?? '—'}</div>
                  </TableCell>
                  )}
                  {isColVisible('format') && (
                  <TableCell>
                    {l.Format ? <Badge variant="outline">{FORMAT_LABELS[l.Format]}</Badge> : <span className="text-xs text-muted-foreground">—</span>}
                  </TableCell>
                  )}
                  {/* 状态码：0 = 连接级错误（无 HTTP 码）显示 — */}
                  {isColVisible('statusCode') && (
                    <TableCell className="text-right tabular-nums">
                      {l.StatusCode ? <Badge variant="outline">{l.StatusCode}</Badge> : <span className="text-xs text-muted-foreground">—</span>}
                    </TableCell>
                  )}
                  {isColVisible('errorType') && <TableCell><ErrorTypeBadge type={l.ErrorType} /></TableCell>}
                  {/* 错误信息：max-w truncate + title 悬停全文（与用户列同做法；域内已截断 500 字符） */}
                  {isColVisible('errorMessage') && (
                    <TableCell className="max-w-72">
                      {l.ErrorMessage ? (
                        <span className="block truncate text-xs text-muted-foreground" title={l.ErrorMessage}>{l.ErrorMessage}</span>
                      ) : (
                        <span className="text-xs text-muted-foreground">—</span>
                      )}
                    </TableCell>
                  )}
                  {/* 耗时：错误面无 TTFT，仅总耗时（健康色点 + 着色数字） */}
                  {isColVisible('latency') && (
                  <TableCell className="text-right tabular-nums">
                    {l.LatencyMS != null ? (
                      <span className="inline-flex items-center justify-end gap-1.5">
                        <span className={cn('size-2 rounded-full', latencyColor(l.LatencyMS))} />
                        <span className="text-xs text-muted-foreground">{fmtDuration(l.LatencyMS)}</span>
                      </span>
                    ) : (
                      <span className="text-xs text-muted-foreground">—</span>
                    )}
                  </TableCell>
                  )}
                  {/* 计费档：service_tier 归一化值；null = 未计费路径 */}
                  {isColVisible('billingTier') && (
                    <TableCell>
                      {l.BillingTier ? <Badge variant="outline">{l.BillingTier}</Badge> : <span className="text-xs text-muted-foreground">—</span>}
                    </TableCell>
                  )}
                  {/* 客户端 IP（CF-Connecting-IP 优先；无则 RemoteAddr）；IPv6 超长 truncate */}
                  <TableCell className="max-w-36">
                    <span className="block truncate font-mono text-xs text-muted-foreground" title={l.ClientIP}>{l.ClientIP ?? '—'}</span>
                  </TableCell>
                </TableRow>
                ))}
            </TableBody>
          </Table>
          </ScrollArea>
        </Card>
        {/* 分页底栏：游标链（无 total/offset）——条数 Select + 页码按钮组 + 跳转 + 翻页/回最新；
            isFetching（翻页/补链中）禁用全部控件，防连点重复请求 */}
        <LogPagination
          ns="logs.pagination"
          page={page}
          loadedPages={loadedPages}
          hasNext={hasNext}
          isFetching={isFetching}
          limit={limit}
          onChangeLimit={changeLimit}
          onGoToPage={goToPage}
          onGoPrev={goPrev}
          onGoNext={goNext}
          onGoLatest={goLatest}
        />
        </>
      )}
    </div>
  )
}
