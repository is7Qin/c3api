// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

import { useEffect, useMemo, useRef, useState } from 'react'
import { useMutation, useQueries, useQuery, useQueryClient } from '@tanstack/react-query'
import { motion } from 'framer-motion'
import { Plus, Pencil, Trash2, Users, Ban, CircleCheck, Filter, Settings2, SlidersHorizontal, Upload } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { api } from '@/App'
import { ApiError, ApiUnauthorized } from '@/lib/api/client'
import { BatchBar } from '@/components/batch-bar'
import { ListToolbar } from '@/components/list-toolbar'
import { Pagination } from '@/components/pagination'
import { SortableHeader, type SortOrder } from '@/components/sortable-header'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { Checkbox } from '@/components/ui/checkbox'
import { DateTimePicker } from '@/components/ui/date-picker'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { DropdownMenu, DropdownMenuCheckboxItem, DropdownMenuContent, DropdownMenuGroup, DropdownMenuLabel, DropdownMenuTrigger } from '@/components/ui/dropdown-menu'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { ScrollArea } from '@/components/ui/scroll-area'
import { Popover, PopoverContent, PopoverTrigger } from '@/components/ui/popover'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { Skeleton } from '@/components/ui/skeleton'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/ui/tooltip'
import { toast } from '@/components/ui/toast'
import { StatusBadge, CooldownBadge } from '@/components/status-badge'
import { fmtTokens, formatPercent, toRFC3339, truncate } from '@/components/fmt'
import { cn } from '@/lib/utils'
import type { components } from '@/lib/api/schema'
import { CodexImportDialog } from '@/components/codex-import/import-dialog'

type AccountView = components['schemas']['AccountView']
type AccountCreate = components['schemas']['AccountCreate']
type AccountPatch = components['schemas']['AccountPatch']
type AccountStatus = components['schemas']['AccountStatus']
type AccountExt = components['schemas']['AccountExt']
type Group = components['schemas']['Group']

// codex 生态账号（AccountExt 仅支持 codex-oauth/codex-pat）：凭据走扩展配置，列表上游 key 不承载凭据
const CODE_CREDENTIAL_TYPES: NonNullable<components['schemas']['Template']['CredentialType']>[] = ['codex-oauth', 'codex-pat']
const isCodexTemplate = (a: AccountView) =>
  a.Template?.CredentialType === 'codex-oauth' || a.Template?.CredentialType === 'codex-pat'

// —— 列设置（logs 同款模式）：可隐藏列 + localStorage 持久化；usage 列懒加载——
// 隐藏选择持久化（accounts-hidden-columns）；勾选/ID/操作恒显。
// 视口懒加载块大小 = 批量端点上限（accounts/usage ≤100/次）。
const USAGE_BLOCK_SIZE = 100
const ACCOUNTS_HIDDEN_STORAGE_KEY = 'accounts-hidden-columns'
const ACCOUNTS_HIDDENABLE_COLS = ['name', 'template', 'status', 'weight', 'maxConcurrency', 'curConcurrency', 'errRate', 'errCount', 'lastError', 'usage'] as const

function loadHiddenCols(): Set<string> {
  try {
    const raw = localStorage.getItem(ACCOUNTS_HIDDEN_STORAGE_KEY)
    if (raw) return new Set(JSON.parse(raw) as string[])
  } catch { /* 损坏数据忽略 */ }
  return new Set()
}

function saveHiddenCols(cols: Set<string>) {
  localStorage.setItem(ACCOUNTS_HIDDEN_STORAGE_KEY, JSON.stringify([...cols]))
}

// —— 用量/额度概要列（0e77d2a accounts/usage 批量聚合）——
// A = 乘倍率前原始成本（raw_cost_usd）、U = 计费成本（cost_usd）——gateway 全账号有；
// codex 账号（upstream 非 null）追加消耗百分比条 + reset 剩余时间；upstream_error 显示失败态。
// 金额：≥$0.01 两位小数，更小四位保精度（0.0042 不被抹成 $0.00）；0/null 显示 —。
const fmtUsd = (v?: number | null): string => (v == null || v <= 0 ? '$0.00' : `$${(v >= 0.01 ? v.toFixed(2) : v.toFixed(4))}`)
const pctColor = (p: number): string => (p < 60 ? 'bg-emerald-500' : p < 85 ? 'bg-amber-500' : 'bg-red-500')
// reset 剩余时长紧凑格式：≥1d → "Xd Xh"，≥1h → "Xh Xm"，否则 "Xm"。
const fmtReset = (ms: number): string => {
  const d = Math.floor(ms / 86_400_000)
  const h = Math.floor((ms % 86_400_000) / 3_600_000)
  const m = Math.floor((ms % 3_600_000) / 60_000)
  if (d > 0) return `${d}d ${h}h`
  if (h > 0) return `${h}h ${m}m`
  return `${m}m`
}

// —— 用量明细弹窗（B-2）：预置时间范围（hours ≤72 → 分桶 hour 粒度，否则 day）——
const USAGE_RANGES = [
  { key: '24h', hours: 24 },
  { key: '7d', hours: 168 },
  { key: '30d', hours: 720 },
  { key: '90d', hours: 2160 },
] as const

// 分桶时间列按粒度截断：day → 仅日期（桶边界 08:00 的 UTC 时刻对"一天"无语义）；
// hour → 日期 + 整点（桶起点）。本地时区转换（与 formatDateTime 一致）。
const fmtBucketTime = (iso?: string, g: 'hour' | 'day' = 'day'): string => {
  if (!iso) return '—'
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return '—'
  const pad = (n: number) => String(n).padStart(2, '0')
  const date = `${d.getFullYear()}/${pad(d.getMonth() + 1)}/${pad(d.getDate())}`
  return g === 'hour' ? `${date} ${pad(d.getHours())}:00` : date
}

// 汇总卡片（A 原始成本 / U 计费成本 / 请求数 / 总 tokens），口径与用量单元格一致。
function UsageSummary({ label, value }: { label: string; value: string }) {
  return (
    <div className="rounded-lg border bg-card p-3">
      <p className="text-xs text-muted-foreground">{label}</p>
      <p className="mt-1 text-lg font-semibold tabular-nums">{value}</p>
    </div>
  )
}
function UsageCell({ item }: { item?: components['schemas']['AccountUsageItem'] }) {
  const { t } = useTranslation()
  if (!item) return <span className="text-xs text-muted-foreground">—</span>
  const g = item.gateway
  const up = item.upstream
  const pct = up?.rate_limit?.used_percent
  const resetAt = up?.rate_limit?.reset_at
  // reset 剩余时长（列表 10s refetch 刷新，无需定时器）
  const leftMs = resetAt ? new Date(resetAt).getTime() - Date.now() : null
  return (
    <div className="space-y-1 text-xs">
      <div className="whitespace-nowrap tabular-nums text-muted-foreground">
        A <span className="font-medium text-foreground">{fmtUsd(g?.raw_cost_usd)}</span>
        <span className="mx-1 text-muted-foreground/40">·</span>
        U <span className="font-medium text-foreground">{fmtUsd(g?.cost_usd)}</span>
      </div>
      {up && pct != null && (
        <div className="flex items-center justify-center gap-1.5">
          <div className="h-1.5 w-20 overflow-hidden rounded-full bg-muted">
            <div className={cn('h-full rounded-full', pctColor(pct))} style={{ width: `${Math.min(100, Math.max(0, pct))}%` }} />
          </div>
          <span className="whitespace-nowrap tabular-nums text-muted-foreground">{pct}%</span>
          {resetAt && leftMs != null && leftMs > 0 && (
            <span className="whitespace-nowrap tabular-nums text-muted-foreground">{fmtReset(leftMs)}</span>
          )}
        </div>
      )}
      {resetAt && leftMs != null && leftMs <= 0 && <div className="whitespace-nowrap text-[11px] text-muted-foreground">{t('accounts.usage.resetSoon')}</div>}
      {item.upstream_error && <div className="whitespace-nowrap text-[11px] text-destructive">{t(`accounts.usage.err.${item.upstream_error}`)}</div>}
    </div>
  )
}

// RFC3339（API）→ datetime-local 'YYYY-MM-DDTHH:mm'（本地时区；DateTimePicker 值格式，'' = 未设置）
function toLocalDT(iso: string): string {
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return ''
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`
}
// 批量更新表单里 status/template_id 的「不修改」哨兵值。
type BatchStatus = 'all' | AccountStatus

const STATUSES: AccountStatus[] = ['active', 'unhealthy', '429', 'disabled']

interface FormState {
  name: string
  template_id: string // Select 值统一用字符串，提交时转 number
  base_url: string // 账号级覆盖（'' = 继承模板）
  upstream_key: string
  status: AccountStatus
  weight: string
  max_concurrency: string
  group_ids: number[]
  // codex 凭据（按模板类型分流：codex-oauth → codex_oauth_* 组；codex-pat →
  // codex_pat_key；与 account_ext 字段对应——保存时链式写入扩展配置）
  codex_oauth_token: string
  codex_oauth_refresh_token: string
  codex_oauth_expires_at: string
  codex_pat_key: string
  codex_email: string
}

const emptyForm = (): FormState => ({
  name: '',
  template_id: '',
  base_url: '',
  upstream_key: '',
  status: 'active',
  weight: '0',
  max_concurrency: '8',
  group_ids: [],
  codex_oauth_token: '',
  codex_oauth_refresh_token: '',
  codex_oauth_expires_at: '',
  codex_pat_key: '',
  codex_email: '',
})

function toForm(a: AccountView): FormState {
  return {
    name: a.Name ?? '',
    template_id: String(a.TemplateID ?? ''),
    base_url: a.BaseURL ?? '', // 编辑回显（C3：toAPIAccountView 平铺携带，否则保存静默清空）
    upstream_key: a.UpstreamKey ?? '',
    status: a.Status ?? 'active',
    weight: String(a.Weight ?? 0),
    max_concurrency: String(a.MaxConcurrency ?? 8),
    // 编辑回显不走账号列表（I-1 方案 B）：对话框挂载时经 getAccountGroups
    // 拉取，加载完成前禁用保存（防误发 [] 清空）。codex 凭据经 ext 拉取回显。
    group_ids: [],
    codex_oauth_token: '',
    codex_oauth_refresh_token: '',
    codex_oauth_expires_at: '',
    codex_pat_key: '',
    codex_email: '',
  }
}

// PUT 全量替换：重建 AccountCreate（只带契约字段，不带运行时字段）。
// 编辑态总是发送 group_ids（含空数组 = 清空）；创建态仅已选时发送
// （缺省 = 无分组，语义与 null 一致）。
function toBody(f: FormState, editing: boolean): AccountCreate {
  const body: AccountCreate = {
    name: f.name.trim(),
    template_id: Number(f.template_id),
    // 空串归一 null（create/update 路径 ""/null/缺省统一 = 继承模板）
    base_url: f.base_url.trim() || null,
    upstream_key: f.upstream_key,
    status: f.status,
    weight: f.weight === '' ? 0 : Number(f.weight),
    max_concurrency: f.max_concurrency === '' ? 8 : Number(f.max_concurrency),
  }
  if (editing || f.group_ids.length > 0) body.group_ids = f.group_ids
  return body
}

// 分组多选（替换语义 UI；disabled 用于回显加载中/批量清空勾选时）。
function GroupMultiSelect({ groups, value, onChange, disabled }: {
  groups: Group[]
  value: number[]
  onChange: (v: number[]) => void
  disabled?: boolean
}) {
  const toggle = (id: number) => onChange(value.includes(id) ? value.filter(x => x !== id) : [...value, id])
  return (
    <ScrollArea className={`max-h-48 rounded-lg border p-2 ${disabled ? 'pointer-events-none opacity-50' : ''}`}>
      <div className="space-y-1">
      {groups.map(g => (
        <label key={g.ID} className="flex cursor-pointer items-center gap-2.5 rounded-md px-2 py-1.5 hover:bg-muted">
          <Checkbox checked={value.includes(g.ID!)} onCheckedChange={() => toggle(g.ID!)} />
          <span className="flex-1 truncate text-sm">{g.Name}</span>
          <span className="text-xs text-muted-foreground">#{g.ID}</span>
        </label>
      ))}
      </div>
    </ScrollArea>
  )
}

// 禁用/启用 quick action：取当前对象重建请求体 + status 翻转。
// base_url 从 AccountView 取（C3——缺则 toggle PUT 全量替换静默清空）。
function toggleBody(a: AccountView, next: AccountStatus): AccountCreate {
  return {
    name: a.Name ?? '',
    template_id: a.TemplateID ?? 0,
    base_url: a.BaseURL ?? null,
    upstream_key: a.UpstreamKey ?? '',
    status: next,
    weight: a.Weight ?? 0,
    max_concurrency: a.MaxConcurrency ?? 8,
  }
}

// 批量更新表单：空字段 = 不发送（保持原值）。
interface BatchForm {
  name: string
  upstream_key: string
  base_url: string
  status: BatchStatus
  weight: string
  max_concurrency: string
  template_id: string
  group_ids: string[]
  clearGroups: boolean // 评审 I-2：勾选发送 group_ids: [] 并禁用分组多选
  clearBaseURL: boolean // C1 三态哨兵（对齐 clearGroups 先例）：勾选发送 base_url: "" = 清空；未勾选且输入非空 → 该值；未勾选且空 → 不变
}

const emptyBatchForm = (): BatchForm => ({
  name: '',
  upstream_key: '',
  base_url: '',
  status: 'all',
  weight: '',
  max_concurrency: '',
  template_id: 'all',
  group_ids: [],
  clearGroups: false,
  clearBaseURL: false,
})

export default function Accounts() {
  const { t } = useTranslation()
  const qc = useQueryClient()

  // —— 列表：筛选/分页状态归 queryKey ——
  const [name, setName] = useState('')
  const [activeSort, setActiveSort] = useState<string | null>(null) // null = 无主动排序（默认 id desc）
  const [order, setOrder] = useState<SortOrder>('desc')
  const [offset, setOffset] = useState(0)
  const [limit, setLimit] = useState(20)
  const [statusFilter, setStatusFilter] = useState<AccountStatus[]>([])
  const [templateId, setTemplateId] = useState('all') // 'all' = 全部模板

  const { data, isLoading, isError, error } = useQuery({
    queryKey: [
      'accounts',
      { limit, offset, name, sort: activeSort ?? 'id', order, status: statusFilter.join(','), template_id: templateId === 'all' ? undefined : Number(templateId) },
    ],
    queryFn: () =>
      api.listAccounts({
        limit,
        offset,
        name: name || undefined,
        sort: activeSort ?? 'id',
        order,
        status: statusFilter.length > 0 ? statusFilter.join(',') : undefined,
        template_id: templateId === 'all' ? undefined : Number(templateId),
      }),
    refetchInterval: 10_000,
  })
  const templatesQ = useQuery({ queryKey: ['templates'], queryFn: () => api.listTemplates({ limit: 100 }) })
  const templates = templatesQ.data?.rows ?? []
  const groupsQ = useQuery({ queryKey: ['groups'], queryFn: () => api.listGroups({ limit: 100 }) })
  const groups = groupsQ.data?.rows ?? []
  const rows = data?.rows ?? []

  // 行勾选（跨页保留，筛选/翻页后清空）——
  const [selected, setSelected] = useState<number[]>([])
  const pageIds = rows.map(r => r.ID!)
  const allChecked = rows.length > 0 && pageIds.every(id => selected.includes(id))
  const someChecked = pageIds.some(id => selected.includes(id))
  const toggleRow = (id: number) => setSelected(s => (s.includes(id) ? s.filter(x => x !== id) : [...s, id]))
  const toggleAll = (c: boolean) =>
    setSelected(s => (c ? Array.from(new Set([...s, ...pageIds])) : s.filter(x => !pageIds.includes(x))))

  // —— 列可见性（usage 懒加载：列隐藏 → 查询停用，轮询停）——
  const [hiddenCols, setHiddenCols] = useState<Set<string>>(() => loadHiddenCols())
  const isColVisible = (key: string) => !hiddenCols.has(key)
  const toggleCol = (key: string) =>
    setHiddenCols(prev => {
      const next = new Set(prev)
      if (next.has(key)) next.delete(key)
      else next.add(key)
      saveHiddenCols(next)
      return next
    })

  // —— 用量/额度聚合（0e77d2a）：**视口懒加载**（用户裁决 2026-08-19）——
  // 批量端点上限 100/次 → 块 100；滚动/缩放 rAF 节流 + 二分定位可视行；
  // useQueries 每块独立 key/缓存/轮询——块滚出视口即停轮询（数据留缓存，
  // 滚回从缓存恢复）；usage 列隐藏时 enabled=false 全停。from/to 缺省 = 今日。
  const tableRef = useRef<HTMLTableElement>(null)
  const [visibleRange, setVisibleRange] = useState<[number, number]>([0, 0])
  // IO 观察数据行（表格在 ScrollArea viewport 内滚动——window scroll 监听接不住；
  // IO 尊重 overflow 裁剪链，滚出可视区的行自动报告离开，无需识别滚动容器）。
  useEffect(() => {
    const el = tableRef.current
    if (!el) return
    const trs = Array.from(el.querySelectorAll('tbody tr'))
    if (trs.length === 0) return
    const visible = new Set<number>()
    let raf = 0
    const flush = () => {
      raf = 0
      if (visible.size === 0) { setVisibleRange(prev => prev[0] === -1 ? prev : [-1, -1]); return }
      let first = Infinity, last = -1
      visible.forEach(i => { if (i < first) first = i; if (i > last) last = i })
      setVisibleRange(prev => (prev[0] === first && prev[1] === last) ? prev : [first, last])
    }
    const io = new IntersectionObserver(entries => {
      for (const e of entries) {
        const idx = Number((e.target as HTMLElement).dataset.idx)
        if (e.isIntersecting) visible.add(idx)
        else visible.delete(idx)
      }
      if (!raf) raf = requestAnimationFrame(flush)
    })
    trs.forEach((tr, i) => { (tr as HTMLElement).dataset.idx = String(i); io.observe(tr) })
    return () => { io.disconnect(); if (raf) cancelAnimationFrame(raf) }
  }, [data])

  // 可视行覆盖的账号块（行序 chunk 100；视口外块不请求）
  const visibleBlocks = useMemo(() => {
    const ids = new Set(rows.slice(Math.max(0, visibleRange[0]), visibleRange[1] + 1).map(r => r.ID!))
    const blocks: typeof rows[] = []
    for (let i = 0; i < rows.length; i += USAGE_BLOCK_SIZE) {
      const b = rows.slice(i, i + USAGE_BLOCK_SIZE)
      if (b.some(r => ids.has(r.ID!))) blocks.push(b)
    }
    return blocks
  }, [rows, visibleRange])

  const usageQs = useQueries({
    queries: visibleBlocks.map(b => ({
      queryKey: ['accounts-usage-block', b.map(r => r.ID).join(',')],
      queryFn: () => api.listAccountsUsage(b.map(r => r.ID!)),
      enabled: isColVisible('usage') && pageIds.length > 0,
      // staleTime = 轮询周期：切回页面/滚回视口时缓存新鲜（<10s）零请求直显，
      // 避免 staleTime=0 默认的切回突刺 refetch；过期后按轮询节奏刷新。
      staleTime: 10_000,
      refetchInterval: 10_000,
    })),
  })
  const usageById = useMemo(() => {
    const m = new Map<number, components['schemas']['AccountUsageItem']>()
    usageQs.forEach(q => q.data?.items?.forEach(i => { if (i.account_id != null) m.set(i.account_id, i) }))
    return m
  }, [usageQs])
  // 单元格所属块首载 pending → Skeleton 蒙版（刷新保留旧数据不清空）
  const blockLoadingById = useMemo(() => {
    const m = new Map<number, boolean>()
    visibleBlocks.forEach((b, idx) => { for (const r of b) m.set(r.ID!, usageQs[idx]?.isPending ?? false) })
    return m
  }, [visibleBlocks, usageQs])

  // —— 用量明细弹窗（B-2）：三查询并行——汇总 = usage_logs 实时全窗（无聚合延迟，
  // A/U 准确、含尾窗）；分桶 = stats-agg 离线聚合（watermark 滞后 Lag，末桶为进行中的
  // 部分桶）→ 末桶被尾窗补行 [末桶起点, now) 原位替代（无双计无缺口）。
  // from/to 每次渲染重算但查询 key 不含时间戳——渲染期不重取；弹窗打开（enabled
  // 翻转触发 refetch）/切换范围（key 变化）时 queryFn 拿到当前时刻，滚动/轮询零请求。
  const [usageDetail, setUsageDetail] = useState<AccountView | null>(null)
  const [rangeKey, setRangeKey] = useState<string>('7d')
  const range = USAGE_RANGES.find(r => r.key === rangeKey) ?? USAGE_RANGES[1]
  // 分桶粒度（≤72h → hour，否则 day）——分桶表时间列按粒度截断（day 只显示日期）
  const granularity: 'hour' | 'day' = range.hours <= 72 ? 'hour' : 'day'
  const from = new Date(Date.now() - range.hours * 3600_000).toISOString()
  const to = new Date().toISOString()
  const detailQ = useQuery({
    queryKey: ['account-usage-detail', usageDetail?.ID, rangeKey],
    queryFn: () => api.listAccountsUsage([usageDetail!.ID!], { from, to }),
    enabled: !!usageDetail,
  })
  const statsQ = useQuery({
    queryKey: ['account-stats-detail', usageDetail?.ID, rangeKey],
    queryFn: () => api.getStatsEntityTrend({ entity: 'account', id: usageDetail!.ID!, from, to, granularity }),
    enabled: !!usageDetail,
  })
  // 统计桶按 BucketTime 升序（spec 钉死）：后端 day 合并按 map 迭代返回无序
  //（实测 17/18/19/16 乱序）——末桶判定/slice(0,-1) 依赖升序，必须显式排序。
  const statsBuckets = useMemo(
    () => [...(statsQ.data ?? [])].sort((a, b) => Date.parse(a.BucketTime ?? '') - Date.parse(b.BucketTime ?? '')),
    [statsQ.data],
  )
  // 切分点不假设「上一整点」——取 stats 升序数组末桶 BucketTime 为真实边界
  //（watermark 可停在任意整点）；stats 空数组 → 不查尾窗、分桶表无补行。
  const tailFrom = statsBuckets.length > 0 ? statsBuckets[statsBuckets.length - 1].BucketTime : undefined
  const tailQ = useQuery({
    queryKey: ['account-stats-tail', usageDetail?.ID, rangeKey, tailFrom],
    queryFn: () => api.listAccountsUsage([usageDetail!.ID!], { from: tailFrom!, to }),
    enabled: !!usageDetail && !!tailFrom,
  })
  // 尾窗补行：末桶被尾窗行原位替代（slice(0,-1)）；tailQ 失败 → 恢复完整 buckets
  //（末桶保留，防当前小时整行空缺）+ 错误行提示（复审 O 级裁决）；加载中/空 → 完整
  // buckets。isError 时置空 tailItem——错误态残留的旧缓存尾行会误隐藏末桶（实测）。
  const tailItem = tailQ.isError ? undefined : tailQ.data?.items?.[0]
  const tailFailed = tailQ.isError && statsBuckets.length > 0
  const shownBuckets = tailItem ? statsBuckets.slice(0, -1) : statsBuckets

  // rows 变化清理已不存在的勾选（M2，templates 同款思路）：refetchInterval/操作
  // 刷新后把已删除的行移出 selected。账号页跨页勾选是既有语义（翻页不清空），
  // 故仅同视图（offset 未变）时清理到当前页可见 ID，翻页时跳过。
  const pageOffsetRef = useRef(offset)
  useEffect(() => {
    const pageChanged = pageOffsetRef.current !== offset
    pageOffsetRef.current = offset
    if (pageChanged) return
    const ids = new Set(rows.map(r => r.ID!))
    setSelected(s => s.filter(id => ids.has(id)))
  }, [rows, offset])

  // 筛选/翻页变化 → 回第一页 + 清勾选。
  const resetPage = () => {
    setOffset(0)
    setSelected([])
  }
  // 每页条数变化 → 重置 offset 并清勾选。
  const changeLimit = (l: number) => { setLimit(l); setOffset(0); setSelected([]) }
  const changeName = (v: string) => { setName(v); resetPage() }
  // 列头三态：新列 → 降序；同列降序 → 升序；同列升序 → 取消（回默认 id desc）
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
  const toggleStatusFilter = (s: AccountStatus) => {
    setStatusFilter(cur => (cur.includes(s) ? cur.filter(x => x !== s) : [...cur, s]))
    resetPage()
  }
  const changeTemplate = (v: string) => { setTemplateId(v); resetPage() }
  const hasFilters = name !== '' || statusFilter.length > 0 || templateId !== 'all'
  const clearFilters = () => {
    setName('')
    setStatusFilter([])
    setTemplateId('all')
    resetPage()
  }

  // —— 批量删除/更新 ——
  const batchDelete = useMutation({
    mutationFn: (ids: number[]) => api.deleteAccountsBatch(ids),
    onSuccess: (_res, ids) => {
      qc.invalidateQueries({ queryKey: ['accounts'] })
      setSelected([])
      // 当前页被删空时回到最后有效页（templates 同款守卫，不再一律回第 1 页）
      const after = (data?.total ?? 0) - ids.length
      if (offset > 0 && offset >= after) setOffset(Math.max(0, after - (after % limit)))
    },
  })
  const batchUpdate = useMutation({
    mutationFn: (p: { ids: number[]; fields: AccountPatch }) => api.updateAccountsBatch(p.ids, p.fields),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['accounts'] })
      setSelected([])
      closeBatchUpdate('submitted')
    },
  })
  const batchResetCooldown = useMutation({
    mutationFn: (ids: number[]) => api.resetAccountsCooldown(ids),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['accounts'] })
      setSelected([])
    },
  })
  // BatchBar 的 onUpdate 返回 promise：对话框关闭（提交成功/取消）时 resolve。
  const [batchUpdateOpen, setBatchUpdateOpen] = useState(false)
  const [batchForm, setBatchForm] = useState<BatchForm>(emptyBatchForm())
  const [batchFormErr, setBatchFormErr] = useState<string | null>(null)
  const batchResolve = useRef<((r: 'cancelled' | 'submitted') => void) | null>(null)
  const closeBatchUpdate = (r: 'cancelled' | 'submitted' = 'cancelled') => {
    setBatchUpdateOpen(false)
    batchResolve.current?.(r)
    batchResolve.current = null
  }
  const openBatchUpdate = () => {
    setBatchForm(emptyBatchForm())
    setBatchFormErr(null)
    setBatchUpdateOpen(true)
  }
  const submitBatchUpdate = () => {
    const fields: AccountPatch = {}
    if (batchForm.name.trim()) fields.name = batchForm.name.trim()
    if (batchForm.upstream_key) fields.upstream_key = batchForm.upstream_key
    // base_url 批量三态（C1）：勾选清空 → "" = 清空（回继承模板）；
    // 未勾选且输入非空 → 落值；未勾选且空 → 不变（不发送）
    if (batchForm.clearBaseURL) fields.base_url = ''
    else if (batchForm.base_url.trim()) fields.base_url = batchForm.base_url.trim()
    if (batchForm.status !== 'all') fields.status = batchForm.status
    if (batchForm.weight !== '') fields.weight = Number(batchForm.weight)
    if (batchForm.max_concurrency !== '') fields.max_concurrency = Number(batchForm.max_concurrency)
    if (batchForm.template_id !== 'all') fields.template_id = Number(batchForm.template_id)
    if (batchForm.clearGroups) fields.group_ids = []
    else if (batchForm.group_ids.length > 0) fields.group_ids = batchForm.group_ids.map(Number)
    if (Object.keys(fields).length === 0) {
      setBatchFormErr(t('accounts.batchUpdateEmpty'))
      return
    }
    batchUpdate.mutate({ ids: selected, fields })
  }

  // —— 单行 创建/编辑/删除 ——
  const [dialogOpen, setDialogOpen] = useState(false)
  const [editing, setEditing] = useState<AccountView | null>(null)
  const [form, setForm] = useState<FormState>(emptyForm())
  const [deleting, setDeleting] = useState<AccountView | null>(null)
  const [importOpen, setImportOpen] = useState(false)

  // 编辑回显（评审 I-1 方案 B）：对话框挂载时拉取当前分组；数据未到前禁用
  // 保存与分组多选（防未加载完提交误发 [] 清空）。
  const groupsEcho = useQuery({
    queryKey: ['account-groups', editing?.ID],
    queryFn: () => api.getAccountGroups(editing!.ID!),
    enabled: !!editing && dialogOpen,
  })
  const groupsLoaded = !editing || (groupsEcho.data !== undefined && !groupsEcho.isError)
  useEffect(() => {
    if (groupsEcho.data) {
      setForm(f => ({ ...f, group_ids: [...(groupsEcho.data!.group_ids ?? [])] }))
    }
  }, [groupsEcho.data])

  // codex 账号编辑回显：对话框挂载时拉 ext 凭据（404 = 无 ext 行 → 空表单）；
  // 与「扩展配置」弹窗同一数据源，此处仅填充表单。
  const extEcho = useQuery({
    queryKey: ['account-ext-echo', editing?.ID],
    queryFn: async () => {
      try {
        return await api.getAccountExt(editing!.ID!)
      } catch (e) {
        if (e instanceof ApiError && e.status === 404) return null // 无 ext 行 = 空表单
        throw e
      }
    },
    enabled: dialogOpen && !!editing && isCodexTemplate(editing),
  })
  useEffect(() => {
    const d = extEcho.data
    if (editing && !extEcho.isLoading && d) {
      setForm(f => ({
        ...f,
        codex_oauth_token: d.codex_oauth_token ?? '',
        codex_oauth_refresh_token: d.codex_oauth_refresh_token ?? '',
        codex_oauth_expires_at: d.codex_oauth_expires_at ? toLocalDT(d.codex_oauth_expires_at) : '',
        codex_pat_key: d.codex_pat_key ?? '',
        codex_email: d.codex_email ?? '',
      }))
    }
  }, [editing, extEcho.isLoading, extEcho.data])

  const openCreate = () => {
    setEditing(null)
    setForm(emptyForm())
    setDialogOpen(true)
  }
  const openEdit = (a: AccountView) => {
    setEditing(a)
    setForm(toForm(a))
    setDialogOpen(true)
  }

  // 表单直填凭据：codex 类型账号本体（upstream_key 可空）+ 链式 PUT account_ext
  // （codex_oauth_* 列组 / codex_pat_key 按模板类型分流；与「扩展配置」弹窗共用写路径）。
  const save = useMutation({
    mutationFn: async (f: FormState) => {
      const ct = selTemplate?.CredentialType
      const id = editing?.ID ?? (await api.createAccount(toBody(f, false))).ID
      if (editing) await api.updateAccount(id!, toBody(f, true))
      if (id && (ct === 'codex-oauth' || ct === 'codex-pat')) {
        const cur = extEcho.data
        const extBody: AccountExt = {
          account_id: id,
          credential_type: ct,
          codex_email: (f.codex_email?.trim() ?? cur?.codex_email ?? null) as string | null | undefined,
          ...(ct === 'codex-oauth'
            ? {
                codex_oauth_token: f.codex_oauth_token.trim() || null,
                codex_oauth_refresh_token: f.codex_oauth_refresh_token.trim() || null,
                codex_oauth_expires_at: toRFC3339(f.codex_oauth_expires_at) ?? null,
                codex_pat_key: null,
              }
            : {
                codex_pat_key: f.codex_pat_key.trim() || null,
                codex_oauth_token: null,
                codex_oauth_refresh_token: null,
                codex_oauth_expires_at: null,
              }),
          // 身份四元组只读：回显原对象防清空（首次写入由 service 自动生成）
          ...(cur?.codex_identity ? { codex_identity: cur.codex_identity } : {}),
        }
        await api.putAccountExt(id, extBody)
      }
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['accounts'] })
      setDialogOpen(false)
      toast.add({ title: t('accounts.saveSuccess'), type: 'success' })
    },
  })
  const toggle = useMutation({
    mutationFn: (a: AccountView) =>
      api.updateAccount(a.ID!, toggleBody(a, a.Status === 'disabled' ? 'active' : 'disabled')),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['accounts'] }),
  })
  const remove = useMutation({
    mutationFn: (id: number) => api.deleteAccount(id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['accounts'] })
      setDeleting(null)
      // 删除的是当前页最后一行时回退一页（templates 同款「最后有效页」守卫）
      if (rows.length === 1 && offset > 0) setOffset(offset - limit)
    },
  })

  const submit = () => {
    // 凭据必填性按模板类型分流：api_key/responses-special → upstream_key；
    // codex-oauth → codex_oauth_token；codex-pat → codex_pat_key（后端同步校验）。
    if (!form.name.trim() || !form.template_id) return
    const ct = selTemplate?.CredentialType
    if (ct === 'codex-oauth' && !form.codex_oauth_token.trim()) return
    if (ct === 'codex-pat' && !form.codex_pat_key.trim()) return
    if (ct !== 'codex-oauth' && ct !== 'codex-pat' && !form.upstream_key) return
    save.mutate(form)
  }

  // —— 扩展配置（codex-oauth/codex-pat 模板；GET 404 = 无 ext 行 → 空表单，credential_type 预填模板类型） ——
  const [extTarget, setExtTarget] = useState<AccountView | null>(null)
  const [extForm, setExtForm] = useState({ codex_oauth_token: '', codex_oauth_refresh_token: '', codex_oauth_expires_at: '', codex_pat_key: '', codex_email: '' })
  const extQ = useQuery({
    queryKey: ['account-ext', extTarget?.ID],
    queryFn: async () => {
      try {
        return await api.getAccountExt(extTarget!.ID!)
      } catch (e) {
        if (e instanceof ApiError && e.status === 404) return null // 无 ext 行 = 空表单（null 是合法数据，undefined 会触发 react-query 数据未定义错误）
        throw e
      }
    },
    enabled: !!extTarget,
  })
  const openExt = (a: AccountView) => {
    setExtTarget(a)
    setExtForm({ codex_oauth_token: '', codex_oauth_refresh_token: '', codex_oauth_expires_at: '', codex_pat_key: '', codex_email: '' })
  }
  // 读回显填充（404/无数据 = 保持空表单）
  useEffect(() => {
    const d = extQ.data
    if (extTarget && !extQ.isLoading && d) {
      setExtForm({
        codex_oauth_token: d.codex_oauth_token ?? '',
        codex_oauth_refresh_token: d.codex_oauth_refresh_token ?? '',
        codex_oauth_expires_at: d.codex_oauth_expires_at ? toLocalDT(d.codex_oauth_expires_at) : '',
        codex_pat_key: d.codex_pat_key ?? '',
        codex_email: d.codex_email ?? '',
      })
    }
  }, [extTarget, extQ.isLoading, extQ.data])
  const extCredentialType: AccountExt['credential_type'] = extTarget?.Template?.CredentialType === 'codex-pat' ? 'codex-pat' : 'codex-oauth'
  const extSave = useMutation({
    mutationFn: () => {
      const a = extTarget!
      const ct: AccountExt['credential_type'] = extCredentialType
      if (ct === 'codex-oauth' && !extForm.codex_oauth_token.trim()) throw new Error(t('accounts.ext.oauthTokenRequired'))
      const cur = extQ.data
      const body: AccountExt = {
        account_id: a.ID,
        credential_type: ct,
        codex_email: extForm.codex_email.trim() || null,
        // 类型-列组约束（service 校验）：oauth 只允许 codex_oauth_* 列组；pat 只允许 codex_pat_key（其余置 NULL）
        ...(ct === 'codex-oauth'
          ? {
              codex_oauth_token: extForm.codex_oauth_token.trim() || null,
              codex_oauth_refresh_token: extForm.codex_oauth_refresh_token.trim() || null,
              codex_oauth_expires_at: toRFC3339(extForm.codex_oauth_expires_at) ?? null,
              codex_pat_key: null,
            }
          : {
              codex_pat_key: extForm.codex_pat_key.trim() || null,
              codex_oauth_token: null,
              codex_oauth_refresh_token: null,
              codex_oauth_expires_at: null,
            }),
        // 身份四元组只读展示：回显原对象，防 PUT 全列更新清空（首次写入由 service 自动生成）
        ...(cur?.codex_identity ? { codex_identity: cur.codex_identity } : {}),
      }
      return api.putAccountExt(a.ID!, body)
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['accounts'] })
      setExtTarget(null)
      toast.add({ title: t('accounts.ext.saveSuccess'), type: 'success' })
    },
  })

  const errMsg = (e: unknown) => (e instanceof ApiUnauthorized ? null : (e as Error)?.message)
  const templateName = (a: AccountView) => a.Template?.Name ?? (a.TemplateID ? `#${a.TemplateID}` : '—')
  const selTemplate = templates.find(tp => String(tp.ID) === form.template_id)

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-semibold tracking-tight">{t('accounts.title')}</h1>
          <p className="text-sm text-muted-foreground">{t('accounts.subtitle')}</p>
        </div>
        <div className="flex items-center gap-2">
        <Button variant="outline" onClick={() => setImportOpen(true)} disabled={templates.length === 0} title={templates.length === 0 ? '无可用模板，请先创建 codex-oauth/pat 模板' : undefined}>
          <Upload /> {t('accounts.import.button')}
        </Button>
        <Button onClick={openCreate} disabled={templates.length === 0} title={templates.length === 0 ? t('accounts.noTemplate') : undefined}>
          <Plus /> {t('accounts.new')}
        </Button>
        </div>
      </div>

      <ListToolbar
        name={name}
        onNameChange={changeName}
      >
        {/* status 多选筛选（逗号拼接传参） */}
        <Popover>
          <PopoverTrigger render={<Button variant="outline" size="lg" />}>
            <Filter />
            {statusFilter.length > 0
              ? statusFilter.map(s => t(`status.${s}`)).join(', ')
              : t('accounts.filterStatus')}
          </PopoverTrigger>
          <PopoverContent className="w-48 p-2">
            <div className="space-y-0.5">
              {STATUSES.map(s => (
                <label key={s} className="flex cursor-pointer items-center gap-2.5 rounded-md px-2 py-1.5 hover:bg-muted">
                  <Checkbox checked={statusFilter.includes(s)} onCheckedChange={() => toggleStatusFilter(s)} />
                  <span className="text-sm">{t(`status.${s}`)}</span>
                </label>
              ))}
            </div>
            {statusFilter.length > 0 && (
              <Button variant="ghost" size="sm" className="mt-1 w-full" onClick={clearFilters}>
                {t('list.reset')}
              </Button>
            )}
          </PopoverContent>
        </Popover>
        {/* template 精确筛选 */}
        <Select
          items={Object.fromEntries([['all', t('accounts.allTemplates')], ...templates.map(tp => [String(tp.ID), tp.Name ?? `#${tp.ID}`])])}
          value={templateId}
          onValueChange={changeTemplate}
        >
          <SelectTrigger size="default" className="w-44 data-[size=default]:h-9" aria-label={t('accounts.filterTemplate')}>
            <SelectValue placeholder={t('accounts.filterTemplate')} />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="all" label={t('accounts.allTemplates')}>{t('accounts.allTemplates')}</SelectItem>
            {templates.map(tp => (
              <SelectItem key={tp.ID} value={String(tp.ID)} label={tp.Name ?? `#${tp.ID}`}>{tp.Name ?? `#${tp.ID}`}</SelectItem>
            ))}
          </SelectContent>
        </Select>
      </ListToolbar>

      {/* 列设置（usage 列开关控制懒加载查询） */}
      <div className="flex justify-end">
        <DropdownMenu>
          <DropdownMenuTrigger render={<Button variant="outline" size="sm"><SlidersHorizontal className="size-4" />{t('accounts.columnSettings')}</Button>} />
          <DropdownMenuContent align="end" className="max-h-80 w-48">
            <DropdownMenuGroup>
              <DropdownMenuLabel>{t('accounts.columnSettings')}</DropdownMenuLabel>
              {ACCOUNTS_HIDDENABLE_COLS.map(key => (
                <DropdownMenuCheckboxItem key={key} checked={isColVisible(key)} onCheckedChange={() => toggleCol(key)}>
                  {t(`accounts.table.${key}`)}
                </DropdownMenuCheckboxItem>
              ))}
            </DropdownMenuGroup>
          </DropdownMenuContent>
        </DropdownMenu>
      </div>

      <BatchBar
        selected={selected}
        onClear={() => setSelected([])}
        onDelete={async () => {
          await batchDelete.mutateAsync(selected)
        }}
        onUpdate={() => new Promise<'cancelled' | 'submitted'>(resolve => {
          batchResolve.current = resolve
          openBatchUpdate()
        })}
        onResetCooldown={async () => {
          await batchResetCooldown.mutateAsync(selected)
        }}
      />

      {isError ? (
        <p className="text-sm text-destructive">{t('common.loadFailed', { message: (error as Error).message })}</p>
      ) : isLoading ? (
        <div className="space-y-2">
          {Array.from({ length: 4 }).map((_, i) => <Skeleton key={i} className="h-12" />)}
        </div>
      ) : rows.length === 0 ? (
        <motion.div initial={{ opacity: 0, y: 12 }} animate={{ opacity: 1, y: 0 }} transition={{ duration: 0.25 }}>
          <Card className="flex flex-col items-center gap-2 py-12 text-muted-foreground">
            <Users className="size-10" />
            <p className="font-medium">{hasFilters ? t('accounts.filterEmpty') : t('accounts.emptyTitle')}</p>
            {!hasFilters && <p className="text-sm">{t('accounts.emptyDesc')}</p>}
            {hasFilters ? (
              <Button className="mt-2" variant="outline" onClick={clearFilters}><Filter /> {t('list.reset')}</Button>
            ) : (
              <Button className="mt-2" onClick={openCreate} disabled={templates.length === 0}><Plus /> {t('accounts.new')}</Button>
            )}
          </Card>
        </motion.div>
      ) : (
        <>
          <Card className="bg-transparent border-0 shadow-none backdrop-blur-none p-0 gap-0">
          <ScrollArea className="rounded-[14px] border border-transparent bg-[color:var(--glass-card-light)] shadow-[inset_0_1px_0_rgba(255,255,255,0.5),0_10px_36px_rgba(19,45,83,0.16)] backdrop-blur-[var(--glass-blur)] after:pointer-events-none after:absolute after:inset-0 after:z-20 after:rounded-[14px] after:border after:border-[rgba(19,45,83,0.26)] dark:bg-[color:var(--glass-card-dark)] dark:shadow-[inset_0_1px_0_rgba(255,255,255,0.07),0_10px_36px_rgba(2,6,14,0.5)] dark:after:border-[rgba(148,180,220,0.32)]" showHorizontal>
            <Table ref={tableRef} containerClassName="overflow-x-visible border-0 shadow-none rounded-none bg-transparent backdrop-blur-none" className="min-w-[1080px]">
              <TableHeader>
                <TableRow>
                  <TableHead className="w-10">
                    <Checkbox
                      checked={allChecked}
                      indeterminate={someChecked && !allChecked}
                      onCheckedChange={c => toggleAll(c === true)}
                    />
                  </TableHead>
                  <SortableHeader field="id" label="ID" active={activeSort === 'id'} order={order} onToggle={onColumnToggle} />
                  <SortableHeader field="name" label={t('accounts.table.name')} active={activeSort === 'name'} order={order} onToggle={onColumnToggle} />
                  <SortableHeader field="template_id" label={t('accounts.table.template')} active={activeSort === 'template_id'} order={order} onToggle={onColumnToggle} />
                  <SortableHeader field="status" label={t('accounts.table.status')} active={activeSort === 'status'} order={order} onToggle={onColumnToggle} />
                  <SortableHeader field="weight" label={t('accounts.table.weight')} active={activeSort === 'weight'} order={order} onToggle={onColumnToggle} className="text-right [&_button]:justify-end" />
                  <SortableHeader field="max_concurrency" label={t('accounts.table.maxConcurrency')} active={activeSort === 'max_concurrency'} order={order} onToggle={onColumnToggle} className="text-right [&_button]:justify-end" />
                  <TableHead className="text-right">{t('accounts.table.curConcurrency')}</TableHead>
                  <TableHead className="text-right">{t('accounts.table.errRate')}</TableHead>
                  <TableHead className="text-right">{t('accounts.table.errCount')}</TableHead>
                  <TableHead>{t('accounts.table.lastError')}</TableHead>
                  {isColVisible('usage') && <TableHead className="text-center">{t('accounts.table.usage')}</TableHead>}
                  <TableHead className="text-right">{t('accounts.table.actions')}</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody className="[&_td]:py-3">
                {rows.map(a => (
                  <TableRow key={a.ID} data-state={selected.includes(a.ID!) ? 'selected' : undefined}>
                    <TableCell>
                      <Checkbox checked={selected.includes(a.ID!)} onCheckedChange={() => toggleRow(a.ID!)} />
                    </TableCell>
                    <TableCell className="tabular-nums">{a.ID}</TableCell>
                    <TableCell className="max-w-32 truncate" title={a.Name}>{a.Name}</TableCell>
                    <TableCell className="max-w-32 truncate" title={templateName(a)}>{templateName(a)}</TableCell>
                    <TableCell>
                      <div className="flex items-center gap-1.5">
                        <StatusBadge status={a.Status} />
                        {/* A-4：冷却标识（CooldownUntil 未过期即标出，status=active 也显示） */}
                        <CooldownBadge cooldownUntil={a.CooldownUntil} />
                      </div>
                    </TableCell>
                    <TableCell className="text-right tabular-nums">{a.Weight ?? 0}</TableCell>
                    <TableCell className="text-right tabular-nums">{a.MaxConcurrency ?? 8}</TableCell>
                    <TableCell className="text-right tabular-nums">{a.concurrency ?? 0}</TableCell>
                    <TableCell className="text-right tabular-nums">{formatPercent(a.err_rate)}</TableCell>
                    <TableCell className="text-right tabular-nums">{a.err_count ?? 0}</TableCell>
                    <TableCell className="max-w-40">
                      {a.LastError ? (
                        <Tooltip>
                          <TooltipTrigger render={<span className="block cursor-help truncate text-xs text-muted-foreground" />}>
                            {truncate(a.LastError, 20)}
                          </TooltipTrigger>
                          <TooltipContent>{a.LastError}</TooltipContent>
                        </Tooltip>
                      ) : (
                        <span className="text-xs text-muted-foreground">—</span>
                      )}
                    </TableCell>
                    {isColVisible('usage') && (
                      <TableCell
                        className="cursor-pointer text-center"
                        title={t('accounts.usageDetail.hint')}
                        onClick={() => setUsageDetail(a)}
                      >
                        {/* 懒加载蒙版：所属块首载 pending 时 Skeleton 动画盖住，不出占位/空值 */}
                        {blockLoadingById.get(a.ID!) ? (
                          <Skeleton className="mx-auto h-10 w-24" />
                        ) : (
                          <UsageCell item={usageById.get(a.ID!)} />
                        )}
                      </TableCell>
                    )}
                    <TableCell className="text-right">
                      <div className="flex justify-end gap-1">
                        <Button
                          variant="ghost"
                          size="icon-sm"
                          title={a.Status === 'disabled' ? t('accounts.enable') : t('accounts.disable')}
                          onClick={() => toggle.mutate(a)}
                          disabled={toggle.isPending}
                        >
                          {a.Status === 'disabled' ? <CircleCheck /> : <Ban />}
                        </Button>
                        {isCodexTemplate(a) && (
                          <Button variant="ghost" size="icon-sm" title={t('accounts.ext.button')} onClick={() => openExt(a)}><Settings2 /></Button>
                        )}
                        <Button variant="ghost" size="icon-sm" title={t('common.edit')} onClick={() => openEdit(a)}><Pencil /></Button>
                        <Button variant="ghost" size="icon-sm" className="text-destructive" title={t('common.delete')} onClick={() => setDeleting(a)}><Trash2 /></Button>
                      </div>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </ScrollArea>
          </Card>
          <Pagination total={data?.total ?? 0} limit={limit} offset={offset} onOffsetChange={setOffset} onLimitChange={changeLimit} />
        </>
      )}

      {/* —— 创建/编辑对话框 —— */}
      <Dialog open={dialogOpen} onOpenChange={setDialogOpen}>
        <DialogContent className="sm:max-w-md">
          <DialogHeader>
            <DialogTitle>{editing ? t('accounts.editTitle', { id: editing.ID }) : t('accounts.newTitle')}</DialogTitle>
            <DialogDescription>{t('accounts.dialogDesc')}</DialogDescription>
          </DialogHeader>
          <div className="space-y-3">
            <div className="space-y-1.5">
              <Label htmlFor="acc-name">{t('accounts.nameLabel')}</Label>
              <Input id="acc-name" value={form.name} placeholder={t('accounts.namePlaceholder')} onChange={e => setForm(f => ({ ...f, name: e.target.value }))} />
            </div>
            <div className="space-y-1.5">
              <Label>{t('accounts.templateLabel')}</Label>
              <Select
                items={Object.fromEntries(templates.map(tp => [String(tp.ID), tp.Name ?? `#${tp.ID}`]))}
                value={form.template_id || null}
                onValueChange={v => setForm(f => ({ ...f, template_id: String(v) }))}
              >
                <SelectTrigger className="w-full"><SelectValue /></SelectTrigger>
                <SelectContent>
                  {templates.map(tp => <SelectItem key={tp.ID} value={String(tp.ID)} label={tp.Name ?? `#${tp.ID}`}>{tp.Name ?? `#${tp.ID}`}</SelectItem>)}
                </SelectContent>
              </Select>
              {selTemplate && CODE_CREDENTIAL_TYPES.includes(selTemplate.CredentialType as typeof CODE_CREDENTIAL_TYPES[number]) && (
                <p className="text-xs text-muted-foreground">{t('accounts.ext.credHint')}</p>
              )}
            </div>
            {/* 凭据字段按模板类型分流：codex-oauth → OAuth 列组；codex-pat → PAT Key；
                api_key/responses-special → 上游 Key。codex 凭据保存时链式写入 account_ext */}
            {selTemplate?.CredentialType === 'codex-oauth' ? (
              <div className="space-y-3">
                <div className="space-y-1.5">
                  <Label htmlFor="acc-oauth-token">{t('accounts.ext.oauthToken')}</Label>
                  <Input
                    id="acc-oauth-token"
                    type="password"
                    value={form.codex_oauth_token}
                    placeholder={t('accounts.ext.oauthTokenPlaceholder')}
                    onChange={e => setForm(f => ({ ...f, codex_oauth_token: e.target.value }))}
                  />
                </div>
                <div className="space-y-1.5">
                  <Label htmlFor="acc-oauth-refresh">{t('accounts.ext.oauthRefreshToken')}</Label>
                  <Input
                    id="acc-oauth-refresh"
                    type="password"
                    value={form.codex_oauth_refresh_token}
                    placeholder={t('accounts.ext.oauthRefreshPlaceholder')}
                    onChange={e => setForm(f => ({ ...f, codex_oauth_refresh_token: e.target.value }))}
                  />
                </div>
                <div className="space-y-1.5">
                  <Label>{t('accounts.ext.oauthExpiresAt')}</Label>
                  <DateTimePicker
                    id="acc-oauth-expires"
                    value={form.codex_oauth_expires_at}
                    onChange={v => setForm(f => ({ ...f, codex_oauth_expires_at: v }))}
                  />
                </div>
                <div className="space-y-1.5">
                  <Label htmlFor="acc-ext-email">{t('accounts.ext.email')}</Label>
                  <Input
                    id="acc-ext-email"
                    value={form.codex_email}
                    placeholder={t('accounts.ext.emailPlaceholder')}
                    onChange={e => setForm(f => ({ ...f, codex_email: e.target.value }))}
                  />
                </div>
              </div>
            ) : selTemplate?.CredentialType === 'codex-pat' ? (
              <div className="space-y-3">
                <div className="space-y-1.5">
                  <Label htmlFor="acc-pat">{t('accounts.ext.patKey')}</Label>
                  <Input
                    id="acc-pat"
                    type="password"
                    value={form.codex_pat_key}
                    placeholder={t('accounts.ext.patKeyPlaceholder')}
                    onChange={e => setForm(f => ({ ...f, codex_pat_key: e.target.value }))}
                  />
                </div>
                <div className="space-y-1.5">
                  <Label htmlFor="acc-ext-email">{t('accounts.ext.email')}</Label>
                  <Input
                    id="acc-ext-email"
                    value={form.codex_email}
                    placeholder={t('accounts.ext.emailPlaceholder')}
                    onChange={e => setForm(f => ({ ...f, codex_email: e.target.value }))}
                  />
                </div>
              </div>
            ) : (
              <div className="space-y-1.5">
                <Label htmlFor="acc-key">Upstream Key</Label>
                <Input id="acc-key" type="password" value={form.upstream_key} placeholder="sk-..." onChange={e => setForm(f => ({ ...f, upstream_key: e.target.value }))} />
              </div>
            )}
            {/* 账号级 base_url 覆盖（所有凭据类型显示——codex 可覆盖 SDK 默认端点；
                api_key/responses-special 为模板留空时的兜底）；留空 = 继承模板 */}
            <div className="space-y-1.5">
              <Label htmlFor="acc-base-url">Base URL</Label>
              <Input
                id="acc-base-url"
                value={form.base_url}
                placeholder="https://..."
                onChange={e => setForm(f => ({ ...f, base_url: e.target.value }))}
              />
              <p className="text-xs text-muted-foreground">{t('accounts.baseUrlHint')}</p>
            </div>
            <div className="grid grid-cols-3 gap-3">
              <div className="space-y-1.5">
                <Label>{t('accounts.statusLabel')}</Label>
                <Select
                  items={Object.fromEntries(STATUSES.map(s => [s, t(`status.${s}`)]))}
                  value={form.status}
                  onValueChange={v => setForm(f => ({ ...f, status: v as AccountStatus }))}
                >
                  <SelectTrigger className="w-full"><SelectValue /></SelectTrigger>
                  <SelectContent>
                    {STATUSES.map(s => <SelectItem key={s} value={s} label={t(`status.${s}`)}>{t(`status.${s}`)}</SelectItem>)}
                  </SelectContent>
                </Select>
              </div>
              <div className="space-y-1.5">
                <Label htmlFor="acc-weight">{t('accounts.weightLabel')}</Label>
                <Input id="acc-weight" type="number" min={0} value={form.weight} onChange={e => setForm(f => ({ ...f, weight: e.target.value }))} />
              </div>
              <div className="space-y-1.5">
                <Label htmlFor="acc-max">{t('accounts.maxLabel')}</Label>
                <Input id="acc-max" type="number" min={1} value={form.max_concurrency} onChange={e => setForm(f => ({ ...f, max_concurrency: e.target.value }))} />
              </div>
            </div>
            <div className="space-y-1.5">
              <Label>{t('accounts.groupLabel')}</Label>
              <GroupMultiSelect
                groups={groups}
                value={form.group_ids}
                onChange={v => setForm(f => ({ ...f, group_ids: v }))}
                disabled={editing && !groupsLoaded}
              />
              <p className="text-xs text-muted-foreground">{t('accounts.groupHint')}</p>
              {editing && groupsEcho.isError && (
                <p className="text-sm text-destructive">{t('accounts.loadGroupsFailed')}</p>
              )}
            </div>
            {save.isError && errMsg(save.error) && (
              <p className="text-sm text-destructive">{errMsg(save.error)}</p>
            )}
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setDialogOpen(false)}>{t('common.cancel')}</Button>
            <Button
              onClick={submit}
              disabled={
                save.isPending ||
                !form.name.trim() ||
                !form.template_id ||
                (selTemplate?.CredentialType === 'codex-oauth' && !form.codex_oauth_token.trim()) ||
                (selTemplate?.CredentialType === 'codex-pat' && !form.codex_pat_key.trim()) ||
                (selTemplate?.CredentialType !== 'codex-oauth' && selTemplate?.CredentialType !== 'codex-pat' && form.upstream_key === '') ||
                (editing && !groupsLoaded)
              }
            >
              {save.isPending ? t('common.saving') : editing ? t('common.saveChanges') : t('common.create')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* —— 删除确认（单行） —— */}
      <Dialog open={!!deleting} onOpenChange={o => { if (!o && !remove.isPending) setDeleting(null) }}>
        <DialogContent className="sm:max-w-sm">
          <DialogHeader>
            <DialogTitle>{t('accounts.deleteTitle')}</DialogTitle>
            <DialogDescription>
              {t('accounts.deleteDesc', { name: deleting?.Name })}
            </DialogDescription>
          </DialogHeader>
          {remove.isError && errMsg(remove.error) && (
            <p className="text-sm text-destructive">{errMsg(remove.error)}</p>
          )}
          <DialogFooter>
            <Button variant="outline" onClick={() => setDeleting(null)} disabled={remove.isPending}>{t('common.cancel')}</Button>
            <Button variant="destructive" onClick={() => deleting && remove.mutate(deleting.ID!)} disabled={remove.isPending}>
              {remove.isPending ? t('common.deleting') : t('common.confirmDelete')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* —— 批量更新对话框：AccountPatch 字段子集（空 = 保持原值） —— */}
      <Dialog open={batchUpdateOpen} onOpenChange={o => { if (!o) closeBatchUpdate() }}>
        <DialogContent className="sm:max-w-md">
          <DialogHeader>
            <DialogTitle>{t('accounts.batchUpdateTitle')}</DialogTitle>
            <DialogDescription>{t('accounts.batchUpdateDesc')}</DialogDescription>
          </DialogHeader>
          <div className="space-y-3">
            <div className="space-y-1.5">
              <Label htmlFor="ba-name">{t('accounts.nameLabel')}</Label>
              <Input id="ba-name" value={batchForm.name} placeholder={t('accounts.namePlaceholder')} onChange={e => setBatchForm(f => ({ ...f, name: e.target.value }))} />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="ba-key">Upstream Key</Label>
              <Input id="ba-key" type="password" value={batchForm.upstream_key} placeholder="sk-..." onChange={e => setBatchForm(f => ({ ...f, upstream_key: e.target.value }))} />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="ba-base-url">Base URL</Label>
              <Input
                id="ba-base-url"
                value={batchForm.base_url}
                placeholder="https://..."
                onChange={e => setBatchForm(f => ({ ...f, base_url: e.target.value }))}
              />
              <p className="text-xs text-muted-foreground">{t('accounts.batchBaseUrlHint')}</p>
              <label className="flex cursor-pointer items-center gap-2.5 py-0.5">
                <Checkbox checked={batchForm.clearBaseURL} onCheckedChange={c => setBatchForm(f => ({ ...f, clearBaseURL: c === true }))} />
                <span className="text-sm">{t('accounts.clearBaseUrl')}</span>
              </label>
            </div>
            <div className="grid grid-cols-2 gap-3">
              <div className="space-y-1.5">
                <Label>{t('accounts.statusLabel')}</Label>
                <Select
                  items={Object.fromEntries([['all', t('list.unchanged')], ...STATUSES.map(s => [s, t(`status.${s}`)])])}
                  value={batchForm.status}
                  onValueChange={v => setBatchForm(f => ({ ...f, status: v as BatchStatus }))}
                >
                  <SelectTrigger className="w-full"><SelectValue /></SelectTrigger>
                  <SelectContent>
                    <SelectItem value="all" label={t('list.unchanged')}>{t('list.unchanged')}</SelectItem>
                    {STATUSES.map(s => <SelectItem key={s} value={s} label={t(`status.${s}`)}>{t(`status.${s}`)}</SelectItem>)}
                  </SelectContent>
                </Select>
              </div>
              <div className="space-y-1.5">
                <Label>{t('accounts.templateLabel')}</Label>
                <Select
                  items={Object.fromEntries([['all', t('list.unchanged')], ...templates.map(tp => [String(tp.ID), tp.Name ?? `#${tp.ID}`])])}
                  value={batchForm.template_id}
                  onValueChange={v => setBatchForm(f => ({ ...f, template_id: v }))}
                >
                  <SelectTrigger className="w-full"><SelectValue /></SelectTrigger>
                  <SelectContent>
                    <SelectItem value="all" label={t('list.unchanged')}>{t('list.unchanged')}</SelectItem>
                    {templates.map(tp => <SelectItem key={tp.ID} value={String(tp.ID)} label={tp.Name ?? `#${tp.ID}`}>{tp.Name ?? `#${tp.ID}`}</SelectItem>)}
                  </SelectContent>
                </Select>
              </div>
            </div>
            <div className="grid grid-cols-2 gap-3">
              <div className="space-y-1.5">
                <Label htmlFor="ba-weight">{t('accounts.weightLabel')}</Label>
                <Input id="ba-weight" type="number" min={0} value={batchForm.weight} placeholder="0" onChange={e => setBatchForm(f => ({ ...f, weight: e.target.value }))} />
              </div>
              <div className="space-y-1.5">
                <Label htmlFor="ba-max">{t('accounts.maxLabel')}</Label>
                <Input id="ba-max" type="number" min={1} value={batchForm.max_concurrency} placeholder="8" onChange={e => setBatchForm(f => ({ ...f, max_concurrency: e.target.value }))} />
              </div>
            </div>
            <div className="space-y-1.5">
              <Label>{t('accounts.batchGroupLabel')}</Label>
              <GroupMultiSelect
                groups={groups}
                value={batchForm.group_ids.map(Number)}
                onChange={v => setBatchForm(f => ({ ...f, group_ids: v.map(String) }))}
                disabled={batchForm.clearGroups}
              />
              <p className="text-xs text-muted-foreground">{t('accounts.batchGroupHint')}</p>
              <label className="flex cursor-pointer items-center gap-2.5 py-0.5">
                <Checkbox checked={batchForm.clearGroups} onCheckedChange={c => setBatchForm(f => ({ ...f, clearGroups: c === true }))} />
                <span className="text-sm">{t('accounts.clearGroups')}</span>
              </label>
            </div>
            {batchFormErr && <p className="text-sm text-destructive">{batchFormErr}</p>}
            {batchUpdate.isError && errMsg(batchUpdate.error) && (
              <p className="text-sm text-destructive">{errMsg(batchUpdate.error)}</p>
            )}
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => closeBatchUpdate()} disabled={batchUpdate.isPending}>{t('common.cancel')}</Button>
            <Button onClick={submitBatchUpdate} disabled={batchUpdate.isPending}>
              {batchUpdate.isPending ? t('common.saving') : t('list.batchUpdate')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* —— 扩展配置（codex 生态账号；credential_type 分流 codex_oauth_* 列组 / codex_pat_key；身份四元组只读） —— */}
      <Dialog open={!!extTarget} onOpenChange={o => { if (!o && !extSave.isPending) setExtTarget(null) }}>
        <DialogContent className="sm:max-w-md">
          <DialogHeader>
            <DialogTitle>{t('accounts.ext.title', { id: extTarget?.ID })}</DialogTitle>
            <DialogDescription>{t('accounts.ext.desc')}</DialogDescription>
          </DialogHeader>
          <div className="space-y-3">
            <div className="space-y-1.5">
              <Label>{t('accounts.ext.credentialType')}</Label>
              <Badge variant="outline" className="font-mono">{extQ.data?.credential_type ?? extCredentialType}</Badge>
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="acc-ext-email">{t('accounts.ext.email')}</Label>
              <Input
                id="acc-ext-email"
                value={extForm.codex_email}
                placeholder={t('accounts.ext.emailPlaceholder')}
                onChange={e => setExtForm(f => ({ ...f, codex_email: e.target.value }))}
              />
            </div>
            {extCredentialType === 'codex-oauth' ? (
              <>
                <div className="space-y-1.5">
                  <Label htmlFor="acc-ext-oauth-token">{t('accounts.ext.oauthToken')}</Label>
                  <Input
                    id="acc-ext-oauth-token"
                    type="password"
                    value={extForm.codex_oauth_token}
                    onChange={e => setExtForm(f => ({ ...f, codex_oauth_token: e.target.value }))}
                  />
                  <p className="text-xs text-muted-foreground">{t('accounts.ext.oauthTokenRequired')}</p>
                </div>
                <div className="space-y-1.5">
                  <Label htmlFor="acc-ext-oauth-refresh">{t('accounts.ext.oauthRefreshToken')}</Label>
                  <Input
                    id="acc-ext-oauth-refresh"
                    type="password"
                    value={extForm.codex_oauth_refresh_token}
                    onChange={e => setExtForm(f => ({ ...f, codex_oauth_refresh_token: e.target.value }))}
                  />
                </div>
                <div className="space-y-1.5">
                  <Label>{t('accounts.ext.oauthExpiresAt')}</Label>
                  <DateTimePicker
                    id="acc-ext-oauth-expires"
                    value={extForm.codex_oauth_expires_at}
                    onChange={v => setExtForm(f => ({ ...f, codex_oauth_expires_at: v }))}
                  />
                </div>
              </>
            ) : (
              <div className="space-y-1.5">
                <Label htmlFor="acc-ext-pat">{t('accounts.ext.patKey')}</Label>
                <Input
                  id="acc-ext-pat"
                  type="password"
                  value={extForm.codex_pat_key}
                  onChange={e => setExtForm(f => ({ ...f, codex_pat_key: e.target.value }))}
                />
              </div>
            )}
            {/* 身份四元组：只读展示（首次写入自动生成、恒等约束——不提供编辑） */}
            <div className="space-y-1">
              <p className="text-xs text-muted-foreground">{t('accounts.ext.identityNote')}</p>
              <div className="space-y-0.5 font-mono text-xs text-muted-foreground">
                <p>{t('accounts.ext.installationId')}: {extQ.data?.codex_identity?.installation_id ?? '—'}</p>
                <p>{t('accounts.ext.sessionId')}: {extQ.data?.codex_identity?.session_id ?? '—'}</p>
                <p>{t('accounts.ext.threadId')}: {extQ.data?.codex_identity?.thread_id ?? '—'}</p>
                <p>{t('accounts.ext.windowId')}: {extQ.data?.codex_identity?.window_id ?? '—'}</p>
              </div>
            </div>
            {extQ.isError && (
              <p className="text-sm text-destructive">{t('accounts.ext.loadFailed', { message: (extQ.error as Error).message })}</p>
            )}
            {extSave.isError && errMsg(extSave.error) && (
              <p className="text-sm text-destructive">{errMsg(extSave.error)}</p>
            )}
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setExtTarget(null)} disabled={extSave.isPending}>{t('common.cancel')}</Button>
            <Button
              onClick={() => extSave.mutate()}
              disabled={extSave.isPending || extQ.isLoading || (extCredentialType === 'codex-oauth' && !extForm.codex_oauth_token.trim())}
            >
              {extSave.isPending ? t('common.saving') : t('common.save')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* —— 用量明细弹窗（B-2）：汇总 = usage_logs 实时全窗；分桶 = stats-agg 离线
          聚合（watermark 滞后）——末桶（进行中部分桶）被「当前」尾窗补行原位替代；
          弹窗内数据不轮询（打开时点快照，切换范围手动刷新） —— */}
      <Dialog open={!!usageDetail} onOpenChange={o => { if (!o) setUsageDetail(null) }}>
        <DialogContent className="flex max-h-[85vh] w-[calc(100vw-2rem)] flex-col overflow-hidden p-0 sm:max-w-2xl">
          <DialogHeader className="shrink-0 px-6 pt-6">
            <DialogTitle>{t('accounts.usageDetail.title', { name: usageDetail?.Name ?? '—', id: usageDetail?.ID })}</DialogTitle>
            <DialogDescription>{t(`accounts.usageDetail.range.${rangeKey}`)}</DialogDescription>
          </DialogHeader>
          <div className="flex-1 overflow-y-auto px-6 py-4">
            <div className="space-y-4">
            <div className="flex justify-end">
              <Select
                items={Object.fromEntries(USAGE_RANGES.map(r => [r.key, t(`accounts.usageDetail.range.${r.key}`)]))}
                value={rangeKey}
                onValueChange={setRangeKey}
              >
                <SelectTrigger size="sm" className="w-36"><SelectValue /></SelectTrigger>
                <SelectContent>
                  {USAGE_RANGES.map(r => (
                    <SelectItem key={r.key} value={r.key} label={t(`accounts.usageDetail.range.${r.key}`)}>
                      {t(`accounts.usageDetail.range.${r.key}`)}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
            {/* 汇总卡片（A/U 金额口径与单元格一致：≥$0.01 两位、更小四位，0 → $0.00） */}
            {detailQ.isError ? (
              <p className="text-sm text-destructive">{t('common.loadFailed', { message: (detailQ.error as Error).message })}</p>
            ) : detailQ.isPending ? (
              <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
                {Array.from({ length: 4 }).map((_, i) => <Skeleton key={i} className="h-20" />)}
              </div>
            ) : (
              <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
                <UsageSummary label={t('accounts.usageDetail.rawCost')} value={fmtUsd(detailQ.data?.items?.[0]?.gateway?.raw_cost_usd)} />
                <UsageSummary label={t('accounts.usageDetail.cost')} value={fmtUsd(detailQ.data?.items?.[0]?.gateway?.cost_usd)} />
                <UsageSummary label={t('accounts.usageDetail.requests')} value={String(detailQ.data?.items?.[0]?.gateway?.requests ?? 0)} />
                <UsageSummary label={t('accounts.usageDetail.tokens')} value={fmtTokens(detailQ.data?.items?.[0]?.gateway?.total_tokens ?? 0)} />
              </div>
            )}
            {/* 分桶表：A 列来自 raw_cost_usd（与汇总同口径）；末桶被尾窗行原位替代；
                尾窗失败 → 恢复完整 buckets 渲染（末桶保留）+ 错误行提示（O 级裁决） */}
            {statsQ.isError ? (
              <p className="text-sm text-destructive">{t('common.loadFailed', { message: (statsQ.error as Error).message })}</p>
            ) : statsQ.isPending ? (
              <div className="space-y-2">
                {Array.from({ length: 4 }).map((_, i) => <Skeleton key={i} className="h-9" />)}
              </div>
            ) : statsBuckets.length === 0 ? (
              <p className="py-8 text-center text-sm text-muted-foreground">{t('accounts.usageDetail.empty')}</p>
            ) : (
              <ScrollArea className="max-h-80 rounded-lg border" showHorizontal>
                <Table containerClassName="overflow-x-visible border-0 shadow-none rounded-none bg-transparent backdrop-blur-none" className="min-w-[720px]">
                  <TableHeader>
                    <TableRow>
                      <TableHead>{t('accounts.usageDetail.col.time')}</TableHead>
                      <TableHead className="text-right">{t('accounts.usageDetail.rawCost')}</TableHead>
                      <TableHead className="text-right">{t('accounts.usageDetail.col.cost')}</TableHead>
                      <TableHead className="text-right">{t('accounts.usageDetail.col.requests')}</TableHead>
                      <TableHead className="text-right">{t('accounts.usageDetail.col.errors')}</TableHead>
                      <TableHead className="text-right">{t('accounts.usageDetail.col.input')}</TableHead>
                      <TableHead className="text-right">{t('accounts.usageDetail.col.output')}</TableHead>
                      <TableHead className="text-right">{t('accounts.usageDetail.col.cacheRead')}</TableHead>
                      <TableHead className="text-right">{t('accounts.usageDetail.col.cacheWrite')}</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody className="[&_td]:py-2.5">
                    {shownBuckets.map((b, i) => (
                      <TableRow key={b.BucketTime ?? i}>
                        <TableCell className="whitespace-nowrap tabular-nums">{fmtBucketTime(b.BucketTime, granularity)}</TableCell>
                        <TableCell className="text-right tabular-nums">{fmtUsd(b.RawCost)}</TableCell>
                        <TableCell className="text-right tabular-nums">{fmtUsd(b.Cost)}</TableCell>
                        <TableCell className="text-right tabular-nums">{b.RequestCount ?? 0}</TableCell>
                        <TableCell className="text-right tabular-nums">{b.ErrorCount ?? 0}</TableCell>
                        <TableCell className="text-right tabular-nums">{fmtTokens(b.InputTokens ?? 0)}</TableCell>
                        <TableCell className="text-right tabular-nums">{fmtTokens(b.OutputTokens ?? 0)}</TableCell>
                        <TableCell className="text-right tabular-nums">{fmtTokens(b.CacheReadTokens ?? 0)}</TableCell>
                        <TableCell className="text-right tabular-nums">{fmtTokens(b.CacheCreationTokens ?? 0)}</TableCell>
                      </TableRow>
                    ))}
                    {tailItem && (
                      <TableRow className="text-muted-foreground">
                        <TableCell className="whitespace-nowrap">{t('accounts.usageDetail.current')}</TableCell>
                        <TableCell className="text-right tabular-nums">{fmtUsd(tailItem.gateway?.raw_cost_usd)}</TableCell>
                        <TableCell className="text-right tabular-nums">{fmtUsd(tailItem.gateway?.cost_usd)}</TableCell>
                        <TableCell className="text-right tabular-nums">{tailItem.gateway?.requests ?? 0}</TableCell>
                        <TableCell className="text-right" colSpan={5}>—</TableCell>
                      </TableRow>
                    )}
                    {tailFailed && (
                      <TableRow>
                        <TableCell colSpan={9} className="text-sm text-destructive">
                          {t('common.loadFailed', { message: (tailQ.error as Error).message })}
                        </TableCell>
                      </TableRow>
                    )}
                  </TableBody>
                </Table>
              </ScrollArea>
            )}
            </div>
          </div>
          <DialogFooter className="shrink-0 rounded-b-[14px] border-t bg-muted/10 px-6 py-5">
            <Button variant="outline" onClick={() => setUsageDetail(null)}>{t('common.cancel')}</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
      <CodexImportDialog
        open={importOpen}
        onOpenChange={setImportOpen}
        templates={templates}
        groups={groups}
        onDone={() => qc.invalidateQueries({ queryKey: ['accounts'] })}
      />
    </div>
  )
}
