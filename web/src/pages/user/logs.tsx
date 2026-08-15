// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

import { useMemo, useState } from 'react'
import { ArrowDown, ArrowUp, FileText, RotateCcw } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { DateRangePicker } from '@/components/date-range-picker'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { LogPagination } from '@/components/log-pagination'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { Skeleton } from '@/components/ui/skeleton'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { Tabs, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/ui/tooltip'
import { useCursorLogs } from '@/components/use-cursor-logs'
import { defaultLogRange, formatCost, formatDateTime, toRFC3339 } from '@/components/fmt'
import { userApi } from '@/lib/api/client'
import type { MyErrLogParams, MyUsageLogParams } from '@/lib/api/client'
import { cn } from '@/lib/utils'
import type { components } from '@/lib/api/schema'

type ErrorType = components['schemas']['ErrorType']
// 用户面行类型（无 AccountID/TemplateID——用户级契约已删上游拓扑字段）。
type UsageLog = components['schemas']['UserUsageLog']
type ErrLog = components['schemas']['UserErrLog']

// 错误类型全值域（err_logs 完整错误面：拒绝 + 异常双轨）。
const ERROR_TYPES: ErrorType[] = ['none', '429', '4xx', '5xx', 'network', 'auth', 'no_account', 'abort', 'billing']
// usage_logs 放行面只有 none/abort 两种错误类型。
const USAGE_ERROR_TYPES: ErrorType[] = ['none', 'abort']

// 格式标签（管理端 logs.tsx 同款）。
const FORMAT_LABELS: Record<string, string> = {
  'openai-chat': 'OpenAI Chat',
  'openai-responses': 'OpenAI Responses',
  'openai-responses-ws': 'OpenAI Responses (WS)',
  'openai-images': 'OpenAI Images',
  'openai-search': 'OpenAI Search',
  anthropic: 'Anthropic',
}

// 与管理端 logs.tsx 同款色板：none 绿 / 4xx 黄 / 5xx、network、abort 红 / 429 橙 / auth、no_account 灰 / billing 紫。
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

// 表头样式（与管理端 logs.tsx 一致）：uppercase 小字 + sticky（位于纵向滚动容器内）。
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

// 延迟健康色（管理端同款）：<1s 绿 / <5s 黄 / <15s 橙 / 以上红——色点与数字同色。
function latencyColor(ms: number): { dot: string; text: string } {
  if (ms < 1000) return { dot: 'bg-emerald-500', text: 'text-emerald-500' }
  if (ms < 5000) return { dot: 'bg-amber-500', text: 'text-amber-500' }
  if (ms < 15000) return { dot: 'bg-orange-500', text: 'text-orange-500' }
  return { dot: 'bg-red-500', text: 'text-red-500' }
}

// —— 格式化工具（管理端 logs.tsx 同款实现） ——
// 耗时 ≥1000ms 用 s（一位小数），否则 ms。
const fmtDuration = (ms: number): string => (ms >= 1000 ? `${(ms / 1000).toFixed(1)}s` : `${ms}ms`)
// 单价：毫分/M → USD/M（API 边界换算 1 USD = 100,000 毫分）。
const fmtPricePerM = (millis: number): string => {
  const usd = millis / 1e5
  if (usd >= 1) return `$${usd.toFixed(4)}/M`
  if (usd >= 0.001) return `$${usd.toFixed(4)}/M`
  return `$${usd.toPrecision(2)}/M`
}
// token 缩写：≥1000 用 K（1.2K/27.5K），否则原样。
const fmtTokens = (n: number): string => (n >= 1000 ? `${(n / 1000).toFixed(1)}K` : String(n))

// base-ui Select 不接受空串值，用哨兵表示「全部」。
const ERROR_ALL = '__all__'
const FORMAT_ALL = '__all__'

interface LogFilters {
  key_id: string
  group_id: string
  model: string
  format: string
  error_type: string
  status_code: string
  from: string
  to: string
}

const emptyFilters = (): LogFilters => ({
  key_id: '', group_id: '', model: '', format: '', error_type: '', status_code: '', ...defaultLogRange(),
})

export default function UserLogs() {
  const { t } = useTranslation()
  const [tab, setTab] = useState<'usage' | 'errors'>('usage')
  const [filters, setFilters] = useState<LogFilters>(emptyFilters)
  const [limit, setLimit] = useState(20)

  // 过滤条件 / 每页条数变化 → hook 参数键变化自动重置回第 1 页（游标链自持，调用方只传派生值）。
  const set = (patch: Partial<LogFilters>) => setFilters(f => ({ ...f, ...patch }))
  const changeLimit = (v: number) => setLimit(v)
  // Tab 切换：各自独立游标链（hook 按参数变化重置）；usage 面错误类型值域收窄为
  // none/abort，超出值重置（收窄逻辑属调用方，hook 只收归一后参数）。
  const switchTab = (v: string) => {
    setTab(v as 'usage' | 'errors')
    if (v === 'usage' && filters.error_type && !USAGE_ERROR_TYPES.includes(filters.error_type as ErrorType)) {
      setFilters(f => ({ ...f, error_type: '' }))
    }
  }

  // 参数对象随 filter/limit/tab 派生（游标由 hook 注入）。
  // 服务端强制 user_id=当前用户，客户端不传；status_code 仅错误面契约支持。
  const { usageParams, errParams } = useMemo(() => {
    const base: MyUsageLogParams = {
      key_id: filters.key_id ? Number(filters.key_id) : undefined,
      group_id: filters.group_id ? Number(filters.group_id) : undefined,
      model: filters.model || undefined,
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
        status_code: filters.status_code && Number.isFinite(Number(filters.status_code)) ? Number(filters.status_code) : undefined,
      } satisfies MyErrLogParams,
    }
  }, [filters, limit])

  // 游标链分页：替代 useQuery + 自计页号；fetchPage 注入（hook 不感知 API 层）。
  const { page, rows, loadedPages, hasNext, isLoading, isFetching, isError, error, goNext, goPrev, goLatest, goToPage } = useCursorLogs<UsageLog | ErrLog>(
    [tab, usageParams, errParams],
    (cursor: number | null) =>
      tab === 'errors'
        ? userApi.getMyErrLogs({ ...errParams, cursor: cursor ?? undefined })
        : userApi.getMyUsageLogs({ ...usageParams, cursor: cursor ?? undefined }),
  )

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">{t('user.logs.title')}</h1>
        <p className="text-sm text-muted-foreground">{t('user.logs.subtitle')}</p>
      </div>

      {/* Tab 切换：用量日志 / 错误日志（两表独立游标） */}
      <Tabs value={tab} onValueChange={v => v && switchTab(v)}>
        <TabsList>
          <TabsTrigger value="usage">{t('user.logs.tab.usage')}</TabsTrigger>
          <TabsTrigger value="errors">{t('user.logs.tab.errors')}</TabsTrigger>
        </TabsList>
      </Tabs>

      {/* 过滤栏：分组/模型/错误类型（+错误面状态码）+ 时间范围（参数与管理端同构，无 user_id/account_id——用户级契约删该两参数） */}
      <Card className="p-4">
        <div className="grid grid-cols-2 gap-3 md:grid-cols-4 xl:grid-cols-8">
          <div className="space-y-1.5">
            <Label htmlFor="user-log-key">{t('user.logs.filter.keyId')}</Label>
            <Input id="user-log-key" type="number" min={0} placeholder="1" value={filters.key_id} onChange={e => set({ key_id: e.target.value })} />
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="user-log-group">{t('user.logs.filter.groupId')}</Label>
            <Input id="user-log-group" type="number" min={0} placeholder="1" value={filters.group_id} onChange={e => set({ group_id: e.target.value })} />
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="user-log-model">{t('user.logs.filter.model')}</Label>
            <Input id="user-log-model" placeholder="gpt-4o" value={filters.model} onChange={e => set({ model: e.target.value })} />
          </div>
          <div className="space-y-1.5">
            <Label>{t('user.logs.filter.format')}</Label>
            <Select
              items={Object.fromEntries([[FORMAT_ALL, t('user.logs.filter.all')], ...Object.keys(FORMAT_LABELS).map(f => [f, FORMAT_LABELS[f]])])}
              value={filters.format || FORMAT_ALL}
              onValueChange={v => set({ format: v === FORMAT_ALL ? '' : v })}
            >
              <SelectTrigger className="w-full"><SelectValue /></SelectTrigger>
              <SelectContent>
                <SelectItem value={FORMAT_ALL} label={t('user.logs.filter.all')}>{t('user.logs.filter.all')}</SelectItem>
                {Object.keys(FORMAT_LABELS).map(f => <SelectItem key={f} value={f} label={FORMAT_LABELS[f]}>{FORMAT_LABELS[f]}</SelectItem>)}
              </SelectContent>
            </Select>
          </div>
          <div className="space-y-1.5">
            <Label>{t('user.logs.filter.errorType')}</Label>
            <Select
              items={Object.fromEntries([[ERROR_ALL, t('user.logs.filter.all')], ...(tab === 'errors' ? ERROR_TYPES : USAGE_ERROR_TYPES).map(et => [et, t(`errorType.${et}`)])])}
              value={filters.error_type || ERROR_ALL}
              onValueChange={v => set({ error_type: v === ERROR_ALL ? '' : v })}
            >
              <SelectTrigger className="w-full"><SelectValue /></SelectTrigger>
              <SelectContent>
                <SelectItem value={ERROR_ALL} label={t('user.logs.filter.all')}>{t('user.logs.filter.all')}</SelectItem>
                {(tab === 'errors' ? ERROR_TYPES : USAGE_ERROR_TYPES).map(et => <SelectItem key={et} value={et} label={t(`errorType.${et}`)}>{t(`errorType.${et}`)}</SelectItem>)}
              </SelectContent>
            </Select>
          </div>
          {tab === 'errors' && (
            <div className="space-y-1.5">
              <Label htmlFor="user-log-status">{t('user.logs.filter.statusCode')}</Label>
              <Input id="user-log-status" type="number" min={0} placeholder="429" value={filters.status_code} onChange={e => set({ status_code: e.target.value })} />
            </div>
          )}
          <div className="space-y-1.5">
            <Label>{t('dateRange.label')}</Label>
            <DateRangePicker value={{ from: filters.from, to: filters.to }} onChange={v => set(v)} />
          </div>
          <div className="flex items-end">
            <Button variant="outline" className="w-full" onClick={() => setFilters(emptyFilters())}>
              <RotateCcw /> {t('user.logs.filter.reset')}
            </Button>
          </div>
        </div>
      </Card>

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
          <p className="font-medium">{t('user.logs.emptyTitle')}</p>
          <p className="text-sm">{t('user.logs.emptyDesc')}</p>
        </Card>
      ) : (
        <>
        <Card className="overflow-hidden">
          <Table containerClassName="max-h-[calc(100vh-16rem)] overflow-y-auto">
            <TableHeader>
              {/* 列顺序与管理端 logs.tsx 对齐：Key→model→format→statusCode(errors)→errorType→
                  errorMessage(errors)→Token(usage)→费用(usage)→耗时→计费档(errors) */}
              <TableRow>
                <Th>{t('user.logs.table.createdAt')}</Th>
                <Th className="text-right">{t('logs.table.key')}</Th>
                <Th>{t('user.logs.table.model')}</Th>
                <Th>{t('user.logs.table.format')}</Th>
                {tab === 'errors' && <Th className="text-right">{t('user.logs.table.statusCode')}</Th>}
                <Th>{t('user.logs.table.errorType')}</Th>
                {tab === 'errors' && <Th>{t('user.logs.table.errorMessage')}</Th>}
                {tab === 'usage' && <Th className="text-right">{t('user.logs.table.tokens')}</Th>}
                {tab === 'usage' && <Th className="text-right">{t('user.logs.table.cost')}</Th>}
                <Th className="text-right">{t('user.logs.table.latency')}</Th>
                {tab === 'errors' && <Th>{t('user.logs.table.billingTier')}</Th>}
              </TableRow>
            </TableHeader>
            <TableBody className="[&_td]:py-3">
              {tab === 'usage'
                ? (rows as UsageLog[]).map(l => (
                <TableRow key={l.ID}>
                  <TableCell className="text-xs text-muted-foreground whitespace-nowrap">{formatDateTime(l.CreatedAt)}</TableCell>
                  {/* 鉴权归属 Key（管理端同款）：#KeyID；0 = 无鉴权 */}
                  <TableCell className="text-right tabular-nums">{l.KeyID ? `#${l.KeyID}` : '—'}</TableCell>
                  {/* 模型链式（管理端同款）：请求模型加粗 + 映射模型缩进灰（有值才显示 ↳） */}
                  <TableCell>
                    <div className="space-y-0.5 text-xs">
                      <div className="max-w-40 truncate font-medium" title={l.Model}>{l.Model ?? '—'}</div>
                      {l.MappedModel && (
                        <div className="max-w-40 truncate pl-3 text-muted-foreground" title={l.MappedModel}>↳{l.MappedModel}</div>
                      )}
                    </div>
                  </TableCell>
                  <TableCell>
                    {l.Format ? <Badge variant="outline">{FORMAT_LABELS[l.Format]}</Badge> : <span className="text-xs text-muted-foreground">—</span>}
                  </TableCell>
                  <TableCell><ErrorTypeBadge type={l.ErrorType} /></TableCell>
                  {/* token 合并列（管理端同款）：↓输入 ↑输出 + cache 第二行 + ⓘ 悬停大卡 */}
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
                        <Tooltip>
                          <TooltipTrigger delay={0} render={<span className="inline-flex -m-1 size-4 shrink-0 cursor-help items-center justify-center rounded-full bg-muted p-1 text-muted-foreground text-[10px] leading-none" />}>
                            i
                          </TooltipTrigger>
                          <TooltipContent className="max-w-xs border bg-popover p-0 text-popover-foreground shadow-lg">
                            <div className="space-y-1.5 p-3 text-xs">
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
                                <span className="font-semibold tabular-nums">
                                  {((l.InputTokens ?? 0) + (l.OutputTokens ?? 0) + (l.CacheReadTokens ?? 0) + (l.CacheCreationTokens ?? 0)).toLocaleString()}
                                </span>
                              </div>
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
                  {/* 计费列：Cost 毫分 → USD（0/空显示 —） */}
                  <TableCell className="text-right tabular-nums">{formatCost(l.Cost)}</TableCell>
                  {/* 耗时列（管理端同款）：上行 TTFT（色点按 ttft 着色 + ≥1000ms 用 s）+ 下行总耗时；ttft 无值只显示总耗时 */}
                  <TableCell className="text-right tabular-nums">
                    {l.TTFTMS != null ? (
                      <div className="space-y-0.5 text-right text-xs">
                        <div className="inline-flex items-center justify-end gap-1.5">
                          <span className={cn('size-2 rounded-full', latencyColor(l.TTFTMS).dot)} />
                          <span className="text-muted-foreground">{t('logs.latency.ttft')} {fmtDuration(l.TTFTMS)}</span>
                        </div>
                        <div className="text-muted-foreground/60">{t('logs.latency.total')} {fmtDuration(l.LatencyMS ?? 0)}</div>
                      </div>
                    ) : l.LatencyMS != null ? (
                      <span className="text-muted-foreground">{fmtDuration(l.LatencyMS)}</span>
                    ) : (
                      <span className="text-xs text-muted-foreground">—</span>
                    )}
                  </TableCell>
                </TableRow>
                ))
                : (rows as ErrLog[]).map(l => (
                <TableRow key={l.ID}>
                  <TableCell className="text-xs text-muted-foreground whitespace-nowrap">{formatDateTime(l.CreatedAt)}</TableCell>
                  {/* 鉴权归属 Key（管理端同款）：#KeyID；0 = 无鉴权 */}
                  <TableCell className="text-right tabular-nums">{l.KeyID ? `#${l.KeyID}` : '—'}</TableCell>
                  {/* 错误面模型无映射链（ErrLog 无 MappedModel）：单行 truncate + title 悬停 */}
                  <TableCell>
                    <div className="max-w-40 truncate text-xs font-medium" title={l.Model}>{l.Model ?? '—'}</div>
                  </TableCell>
                  <TableCell>
                    {l.Format ? <Badge variant="outline">{FORMAT_LABELS[l.Format] ?? l.Format}</Badge> : <span className="text-xs text-muted-foreground">—</span>}
                  </TableCell>
                  {/* 状态码：0 = 连接级错误（无 HTTP 码）显示 — */}
                  <TableCell className="text-right tabular-nums">
                    {l.StatusCode ? <Badge variant="outline">{l.StatusCode}</Badge> : <span className="text-xs text-muted-foreground">—</span>}
                  </TableCell>
                  <TableCell><ErrorTypeBadge type={l.ErrorType} /></TableCell>
                  {/* 错误信息：max-w truncate + title 悬停全文（域内已截断 500 字符） */}
                  <TableCell className="max-w-72">
                    {l.ErrorMessage ? (
                      <span className="block truncate text-xs text-muted-foreground" title={l.ErrorMessage}>{l.ErrorMessage}</span>
                    ) : (
                      <span className="text-xs text-muted-foreground">—</span>
                    )}
                  </TableCell>
                  {/* 延迟：错误面无 TTFT，仅总耗时（管理端同款：健康色点 + fmtDuration ≥1000ms 用 s） */}
                  <TableCell className="text-right tabular-nums">
                    {l.LatencyMS != null ? (
                      <span className="inline-flex items-center justify-end gap-1.5">
                        <span className={cn('size-2 rounded-full', latencyColor(l.LatencyMS).dot)} />
                        <span className="text-xs text-muted-foreground">{fmtDuration(l.LatencyMS)}</span>
                      </span>
                    ) : (
                      <span className="text-xs text-muted-foreground">—</span>
                    )}
                  </TableCell>
                  {/* 计费档：service_tier 归一化值；null = 未计费路径 */}
                  <TableCell>
                    {l.BillingTier ? <Badge variant="outline">{l.BillingTier}</Badge> : <span className="text-xs text-muted-foreground">—</span>}
                  </TableCell>
                </TableRow>
                ))}
            </TableBody>
          </Table>
        </Card>
        {/* 分页底栏：游标链（无 total/offset）——条数 Select + 页码按钮组 + 跳转 + 翻页/回最新；
            isFetching（翻页/补链中）禁用全部控件，防连点重复请求 */}
        <LogPagination
          ns="user.logs.pagination"
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
