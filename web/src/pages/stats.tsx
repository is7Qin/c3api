// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

import { useEffect, useMemo, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { BarChart3, Workflow } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { Area, AreaChart, Bar, BarChart, CartesianGrid, Line, Sankey, Scatter, ScatterChart, XAxis, YAxis, ZAxis } from 'recharts'
import { api } from '@/App'
import type { components } from '@/lib/api/schema'
import { buildFoldedFlowSankey } from '@/lib/routing-sankey'
import { useDebounced } from '@/lib/use-debounced'
import { Pagination } from '@/components/pagination'
import { FlowSankeyLegend, FlowSankeyLinkShape, FlowSankeyNodeShape, FlowSankeyTooltip } from '@/components/routing-flow-sankey'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { ChartContainer, ChartLegend, ChartLegendContent, ChartTooltip, ChartTooltipContent, type ChartConfig } from '@/components/ui/chart'
import { DateRangePicker } from '@/components/date-range-picker'
import { Label } from '@/components/ui/label'
import { Badge } from '@/components/ui/badge'
import { ScrollArea } from '@/components/ui/scroll-area'
import { Skeleton } from '@/components/ui/skeleton'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert'
import { Combobox, ComboboxContent, ComboboxEmpty, ComboboxInput, ComboboxItem, ComboboxList } from '@/components/ui/combobox'
import { browserTimeZone, fmtTTFT, formatCost, formatDateTime, localOffsetSuffix, toRFC3339, truncate } from '@/components/fmt'

type Metric = 'requests' | 'tokens'
type Granularity = 'hour' | 'day'

// 默认近 24h（组件挂载时固定一次，避免渲染期时间漂移）。
function defaultRange() {
  const to = new Date()
  const from = new Date(to.getTime() - 24 * 3600 * 1000)
  const local = (d: Date) =>
    `${d.getFullYear()}-${pad2(d.getMonth() + 1)}-${pad2(d.getDate())}T${pad2(d.getHours())}:${pad2(d.getMinutes())}`
  return { from: local(from), to: local(to) }
}

const pad2 = (n: number) => String(n).padStart(2, '0')

export default function Stats() {
  const { t } = useTranslation()
  const [tab, setTab] = useState<'usage' | 'routing'>('usage')
  const [range, setRange] = useState(defaultRange)
  const [granularity, setGranularity] = useState<Granularity>('hour')
  const [metric, setMetric] = useState<Metric>('tokens')
  const [hidden, setHidden] = useState<Set<string>>(new Set())
  const toggleSeries = (key: string) => {
    setHidden(prev => {
      const next = new Set(prev)
      if (next.has(key)) next.delete(key)
      else next.add(key)
      return next
    })
  }

  const params = useMemo(
    // timezone = 浏览器 IANA 时区——服务端按本地桶界精确聚合；label 用
    // new Date 本地渲染恰一次（与请求时区一致，见 fmt.browserTimeZone）。
    () => ({ from: toRFC3339(range.from)!, to: toRFC3339(range.to)!, granularity, timezone: browserTimeZone() }),
    [range, granularity]
  )
  const { data, isLoading, isError, error } = useQuery({
    queryKey: ['stats', params],
    queryFn: () => api.getStatsTrend(params),
  })
  // 数据即时间线点，无需中间聚合层；label 本地生成
  const rows = data ?? []
  // label 必须跨桶唯一：recharts category 轴 domain 按 label 值去重，
  // 纯时分（"04:00"×5 天重复）→ domain 6-7 个 → tooltip 索引在 0-5 循环
  // （"点位置一直在前面循环"——2026-08-14 修复）；hour 粒度加日期前缀。
  // DST fall-back 重复墙钟 label（01:00 出现两次 = EDT/EST 两个绝对桶）：
  // 计数后仅对重复 label 追加数值 UTC 偏移（RFC3339 形态）消歧，唯一 label 原样。
  const labeledRows = useMemo(() => {
    const base = rows.map(r => {
      const d = r.BucketTime ? new Date(r.BucketTime) : null
      const label = d && !Number.isNaN(d.getTime())
        ? granularity === 'hour'
          ? `${pad2(d.getMonth() + 1)}-${pad2(d.getDate())} ${pad2(d.getHours())}:${pad2(d.getMinutes())}`
          : `${pad2(d.getMonth() + 1)}-${pad2(d.getDate())}`
        : r.BucketTime ?? '—'
      return { ...r, label, time: r.BucketTime ?? '', tableLabel: formatDateTime(r.BucketTime) }
    })
    const counts = new Map<string, number>()
    for (const r of base) counts.set(r.label, (counts.get(r.label) ?? 0) + 1)
    if (![...counts.values()].some(n => n > 1)) return base
    return base.map(r => {
      if ((counts.get(r.label) ?? 0) < 2) return r
      const d = new Date(r.time)
      return Number.isNaN(d.getTime())
        ? r
        : { ...r, label: r.label + localOffsetSuffix(d), tableLabel: r.tableLabel + localOffsetSuffix(d) }
    })
  }, [rows, granularity])

  // TTFT 卡片独立 query，不阻塞图表渲染
  const ttftParams = useMemo(
    () => ({ from: toRFC3339(range.from)!, to: toRFC3339(range.to)! }),
    [range]
  )
  const ttftQ = useQuery({
    queryKey: ['stats-ttft', ttftParams],
    queryFn: () => api.getStatsTTFT(ttftParams),
  })

  const chartConfig = {
    requests: { label: t('stats.metricRequests'), color: 'var(--chart-1)' },
    input: { label: t('stats.chart.seriesInput'), color: 'var(--chart-1)' },
    cacheRead: { label: t('stats.chart.seriesCacheRead'), color: 'var(--chart-2)' },
    output: { label: t('stats.chart.seriesOutput'), color: 'var(--chart-3)' },
    cacheWrite: { label: t('stats.chart.seriesCacheWrite'), color: 'var(--chart-4)' },
    hitRate: { label: t('stats.chart.seriesHitRate'), color: 'var(--chart-5)' },
  } satisfies ChartConfig

  const chartData = useMemo(
    () => labeledRows.map(r => {
      const cacheRead = r.CacheReadTokens ?? 0
      const input = r.InputTokens ?? 0
      return {
        label: r.label,
        requests: r.RequestCount ?? 0,
        input,
        cacheRead,
        output: r.OutputTokens ?? 0,
        cacheWrite: r.CacheCreationTokens ?? 0,
        hitRate: cacheRead + input > 0 ? (cacheRead / (cacheRead + input)) * 100 : 0,
      }
    }),
    [labeledRows]
  )

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">{t('stats.title')}</h1>
        <p className="text-sm text-muted-foreground">{t('stats.subtitle')}</p>
      </div>

      <Tabs value={tab} onValueChange={v => v && setTab(v as 'usage' | 'routing')}>
        <TabsList>
          <TabsTrigger value="usage">{t('stats.tabUsage')}</TabsTrigger>
          <TabsTrigger value="routing">{t('stats.tabRouting')}</TabsTrigger>
        </TabsList>
        <TabsContent value="usage" className="space-y-6">

      <Card className="p-4">
        <div className="flex flex-nowrap items-start gap-5 overflow-x-auto">
          <div className="w-[14rem] shrink-0 space-y-1.5">
            <Label>{t('dateRange.label')}</Label>
            <DateRangePicker value={range} onChange={setRange} />
          </div>
          <div className="shrink-0 space-y-1.5">
            <Label>{t('stats.granularity')}</Label>
            <Tabs value={granularity} onValueChange={v => v && setGranularity(v as Granularity)}>
              <TabsList>
                <TabsTrigger value="hour">{t('stats.granularityHour')}</TabsTrigger>
                <TabsTrigger value="day">{t('stats.granularityDay')}</TabsTrigger>
              </TabsList>
            </Tabs>
          </div>
          <div className="shrink-0 space-y-1.5">
            <Label>{t('stats.metric')}</Label>
            <Tabs value={metric} onValueChange={v => v && setMetric(v as Metric)}>
              <TabsList>
                <TabsTrigger value="requests">{t('stats.metricRequests')}</TabsTrigger>
                <TabsTrigger value="tokens">{t('stats.metricTokens')}</TabsTrigger>
              </TabsList>
            </Tabs>
          </div>
        </div>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>{metric === 'requests' ? t('stats.chartRequestsTitle') : t('stats.chartTokensTitle')}</CardTitle>
          <CardDescription>{t('stats.chartDesc')}</CardDescription>
        </CardHeader>
        <CardContent>
          {isError ? (
            <p className="text-sm text-destructive">{t('common.loadFailed', { message: (error as Error).message })}</p>
          ) : isLoading ? (
            <Skeleton className="h-[320px] w-full" />
          ) : labeledRows.length === 0 ? (
            <div className="flex flex-col items-center gap-2 py-10 text-muted-foreground">
              <BarChart3 className="size-10" />
              <p className="font-medium">{t('stats.emptyTitle')}</p>
              <p className="text-sm">{t('stats.emptyDesc')}</p>
            </div>
          ) : (
            <ChartContainer config={chartConfig} className="h-[320px] w-full">
              {metric === 'requests' ? (
                <BarChart accessibilityLayer data={chartData}>
                  <defs>
                    <linearGradient id="bar-glass-fill" x1="0" y1="0" x2="0" y2="1">
                      <stop offset="0%" stopColor="var(--color-requests)" className="bar-glass-hi" />
                      <stop offset="100%" stopColor="var(--color-requests)" className="bar-glass-lo" />
                    </linearGradient>
                  </defs>
                  <CartesianGrid vertical={false} />
                  <XAxis dataKey="label" tickCount={chartData.length} tickLine={false} tickMargin={10} axisLine={false} fontSize={12} />
                  <YAxis tickLine={false} axisLine={false} tickMargin={8} fontSize={12} allowDecimals={false} />
                  <ChartTooltip content={<ChartTooltipContent />} />
                  <Bar dataKey="requests" fill="url(#bar-glass-fill)" radius={4} maxBarSize={48} />
                </BarChart>
              ) : (
                <AreaChart accessibilityLayer data={chartData} margin={{ left: 0, right: 8 }}>
                  <CartesianGrid vertical={false} />
                  <XAxis dataKey="label" tickCount={chartData.length} tickLine={false} tickMargin={10} axisLine={false} fontSize={12} />
                  <YAxis yAxisId="left" tickLine={false} axisLine={false} tickMargin={8} fontSize={12} allowDecimals={false} />
                  <YAxis
                    yAxisId="right"
                    orientation="right"
                    domain={[0, 100]}
                    tickFormatter={(v: number) => `${v}%`}
                    tickLine={false}
                    axisLine={false}
                    tickMargin={8}
                    fontSize={12}
                  />
                  <ChartTooltip
                    content={
                      <ChartTooltipContent
                        formatter={(value, name, item) => (
                          <>
                            <div
                              className="h-2.5 w-2.5 shrink-0 rounded-[2px]"
                              style={{ backgroundColor: item?.color }}
                            />
                            <div className="flex flex-1 items-center justify-between leading-none">
                              <span className="text-muted-foreground">
                                {chartConfig[String(name) as keyof typeof chartConfig]?.label ?? String(name)}
                              </span>
                              <span className="font-mono font-medium text-foreground tabular-nums">
                                {name === 'hitRate'
                                  ? `${Number(value).toFixed(1)}%`
                                  : Number(value).toLocaleString()}
                              </span>
                            </div>
                          </>
                        )}
                      />
                    }
                  />
                  <ChartLegend content={<ChartLegendContent className="flex-wrap gap-x-4 gap-y-2 [&>div]:shrink-0 [&>div]:whitespace-nowrap" onItemClick={toggleSeries} hiddenKeys={hidden} />} />
                  <Area yAxisId="left" dataKey="input" type="linear" fill="var(--color-input)" fillOpacity={0.2} stroke="var(--color-input)" strokeWidth={2} hide={hidden.has('input')} />
                  <Area yAxisId="left" dataKey="cacheRead" type="linear" fill="var(--color-cacheRead)" fillOpacity={0.2} stroke="var(--color-cacheRead)" strokeWidth={2} hide={hidden.has('cacheRead')} />
                  <Area yAxisId="left" dataKey="output" type="linear" fill="var(--color-output)" fillOpacity={0.2} stroke="var(--color-output)" strokeWidth={2} hide={hidden.has('output')} />
                  <Area yAxisId="left" dataKey="cacheWrite" type="linear" fill="var(--color-cacheWrite)" fillOpacity={0.2} stroke="var(--color-cacheWrite)" strokeWidth={2} hide={hidden.has('cacheWrite')} />
                  <Line yAxisId="right" dataKey="hitRate" type="linear" stroke="var(--color-hitRate)" strokeWidth={2} dot={false} strokeDasharray="6 3" hide={hidden.has('hitRate')} />
                </AreaChart>
              )}
            </ChartContainer>
          )}
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>{t('stats.ttft.title')}</CardTitle>
          <CardDescription>{t('stats.ttft.desc')}</CardDescription>
        </CardHeader>
        <CardContent>
          {ttftQ.isError ? (
            <p className="text-sm text-destructive">{t('common.loadFailed', { message: (ttftQ.error as Error).message })}</p>
          ) : ttftQ.isLoading ? (
            <div className="grid grid-cols-3 gap-4">
              {Array.from({ length: 3 }).map((_, i) => <Skeleton key={i} className="h-16" />)}
            </div>
          ) : (
            <>
              <div className="grid grid-cols-3 gap-4">
                {[
                  { key: 'avg', labelKey: 'stats.ttft.avg', value: ttftQ.data?.AvgMS ?? 0 },
                  { key: 'p95', labelKey: 'stats.ttft.p95', value: ttftQ.data?.P95MS ?? 0 },
                  { key: 'p99', labelKey: 'stats.ttft.p99', value: ttftQ.data?.P99MS ?? 0 },
                ].map(({ key, labelKey, value }) => (
                  <div key={key}>
                    <div className="text-sm text-muted-foreground">{t(labelKey)}</div>
                    <div className="text-2xl font-semibold tabular-nums">{fmtTTFT(value)}</div>
                  </div>
                ))}
              </div>
              {ttftQ.data?.Source && (
                <p className="mt-3 text-xs text-muted-foreground">{t('stats.ttft.source', { source: ttftQ.data.Source })}</p>
              )}
            </>
          )}
        </CardContent>
      </Card>

      <Card className="bg-transparent border-0 shadow-none backdrop-blur-none p-0">
        {isError ? (
          <p className="p-4 text-sm text-destructive">{t('common.loadFailed', { message: (error as Error).message })}</p>
        ) : (
          <ScrollArea className="max-h-[calc(100dvh-20rem)] min-h-0 rounded-[14px] border border-transparent bg-[color:var(--glass-card-light)] shadow-[inset_0_1px_0_rgba(255,255,255,0.5),0_10px_36px_rgba(19,45,83,0.16)] backdrop-blur-[var(--glass-blur)] after:pointer-events-none after:absolute after:inset-0 after:z-20 after:rounded-[14px] after:border after:border-[rgba(19,45,83,0.26)] dark:bg-[color:var(--glass-card-dark)] dark:shadow-[inset_0_1px_0_rgba(255,255,255,0.07),0_10px_36px_rgba(2,6,14,0.5)] dark:after:border-[rgba(148,180,220,0.32)]" showHorizontal data-od-id="table-scroll-stats">
          <Table className="min-w-[1100px]" containerClassName="overflow-x-visible border-0 shadow-none rounded-none bg-transparent backdrop-blur-none">
            <TableHeader>
              <TableRow>
                <TableHead>{t('stats.table.time')}</TableHead>
                <TableHead className="text-right">{t('stats.table.requests')}</TableHead>
                <TableHead className="text-right">{t('stats.table.errors')}</TableHead>
                <TableHead className="text-right">{t('stats.table.calls')}</TableHead>
                <TableHead className="text-right">{t('stats.table.promptTokens')}</TableHead>
                <TableHead className="text-right">{t('stats.table.completionTokens')}</TableHead>
                <TableHead className="text-right">{t('stats.table.cacheReadTokens')}</TableHead>
                <TableHead className="text-right">{t('stats.table.cacheCreationTokens')}</TableHead>
                <TableHead className="text-right">{t('stats.table.totalTokens')}</TableHead>
                <TableHead className="text-right">{t('stats.table.cost')}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody className="[&_td]:py-3">
              {isLoading
                ? Array.from({ length: 5 }).map((_, i) => (
                    <TableRow key={i}>
                      {Array.from({ length: 10 }).map((_, j) => (
                        <TableCell key={j}><Skeleton className="h-4" /></TableCell>
                      ))}
                    </TableRow>
                  ))
                : labeledRows.length === 0
                  ? (
                    <TableRow>
                      <TableCell colSpan={10} className="py-10 text-center text-muted-foreground">{t('stats.emptyTitle')}</TableCell>
                    </TableRow>
                  )
                  : labeledRows.map(r => (
                    <TableRow key={r.time}>
                      <TableCell className="text-xs text-muted-foreground whitespace-nowrap tabular-nums">{r.tableLabel}</TableCell>
                      <TableCell className="text-right tabular-nums">{r.RequestCount ?? 0}</TableCell>
                      <TableCell className="text-right tabular-nums">{r.ErrorCount ?? 0}</TableCell>
                      <TableCell className="text-right tabular-nums">{r.CallCount ?? 0}</TableCell>
                      <TableCell className="text-right tabular-nums">{r.InputTokens ?? 0}</TableCell>
                      <TableCell className="text-right tabular-nums">{r.OutputTokens ?? 0}</TableCell>
                      <TableCell className="text-right tabular-nums">{r.CacheReadTokens ?? 0}</TableCell>
                      <TableCell className="text-right tabular-nums">{r.CacheCreationTokens ?? 0}</TableCell>
                      <TableCell className="text-right tabular-nums">{r.TotalTokens ?? 0}</TableCell>
                      <TableCell className="text-right tabular-nums">{`$${(r.Cost ?? 0).toFixed(4)}`}</TableCell>
                    </TableRow>
                  ))}
            </TableBody>
          </Table>
          </ScrollArea>
        )}
      </Card>
        </TabsContent>
        <TabsContent value="routing" className="space-y-6">
          <RoutingPanel range={range} setRange={setRange} />
        </TabsContent>
      </Tabs>
    </div>
  )
}

// —— Routing Flow tab：渲染服务端 rollup 聚合与当前计划投影。
// 铁律：不在浏览器侧重算 Wilson 区间 / frontier 支配 / 计划编译——所有数值
// 直出 API；守恒失败（first≠terminal）时降级为表格并显式告警，不画无效图。

type PlanRoute = components['schemas']['RoutingPlanRoute']
type FlowEdge = components['schemas']['RoutingFlowEdge']

const routeLabel = (r: PlanRoute) => `${r.ref.model} · ${r.ref.format} · ${r.ref.operation_tag} · g${r.ref.group_id}`

function RoutingPanel({ range, setRange }: {
  range: { from: string; to: string }
  setRange: (r: { from: string; to: string }) => void
}) {
  const { t } = useTranslation()
  // 选择器轻量调用：只取路由列表（candidates_limit=0 不返回候选——
  // 避免 limit × candidates_limit 放大），search 服务端模糊匹配。
  const [searchInput, setSearchInput] = useState('')
  const search = useDebounced(searchInput, 300)
  const planQ = useQuery({
    queryKey: ['routing-plan', search],
    queryFn: () => api.getRoutingPlan({ search, limit: SELECTOR_ROUTE_LIMIT, candidates_limit: 0 }),
  })
  const routes = useMemo(() => planQ.data?.routes ?? [], [planQ.data])
  const [picked, setPicked] = useState<string | undefined>(undefined)
  const [labels, setLabels] = useState(() => new Map<string, string>())
  // 候选刷新后已选项仍显示名称：labels 随列表累积（不随 search 收窄清空——
  // 与 logs.tsx FilterCombobox 的 labelCache 同构惯例）。
  useEffect(() => {
    setLabels(prev => {
      const next = new Map(prev)
      for (const r of routes) next.set(r.ref.route_class_id, routeLabel(r))
      return next
    })
  }, [routes])
  // 选中项失效（计划换代/路由消失）→ 回落首条；空目录 = 未发布计划
  const routeId = picked && routes.some(r => r.ref.route_class_id === picked)
    ? picked
    : (routes[0]?.ref.route_class_id ?? '')
  // 选中路由被 search 过滤掉时：保持当前 routeId（flow/frontier/plan
  // 三卡不断流），labels 缓存保证选择框仍显示其名称。
  // 用 state + effect 记录最后一次非空 routeId（而不是 render 期写 ref——后者违反
  // react(refs) 规则且会在并发渲染下读到脏值）。effect 在提交后同步，故本渲染仍读到上一次的值。
  const [lastRouteId, setLastRouteId] = useState('')
  useEffect(() => {
    if (routeId !== '') setLastRouteId(routeId)
  }, [routeId])
  const activeRouteId = routeId !== '' ? routeId : lastRouteId
  const from = toRFC3339(range.from) ?? ''
  const to = toRFC3339(range.to) ?? ''

  // 三处表格分页（offset/limit 与服务端同构，分页状态进 queryKey）。
  const [flowOffset, setFlowOffset] = useState(0)
  const [flowLimit, setFlowLimit] = useState(20)
  const [frontierOffset, setFrontierOffset] = useState(0)
  const [frontierLimit, setFrontierLimit] = useState(20)
  const changeFlowLimit = (l: number) => { setFlowLimit(l); setFlowOffset(0) }
  const changeFrontierLimit = (l: number) => { setFrontierLimit(l); setFrontierOffset(0) }
  useEffect(() => { setFlowOffset(0); setFrontierOffset(0) }, [activeRouteId, from, to])

  const flowQ = useQuery({
    queryKey: ['routing-flow', activeRouteId, from, to, flowOffset, flowLimit],
    queryFn: () => api.getRoutingFlow({ route: activeRouteId, from, to, offset: flowOffset, limit: flowLimit }),
    enabled: activeRouteId !== '',
  })
  const frontierQ = useQuery({
    queryKey: ['routing-frontier', activeRouteId, from, to, frontierOffset, frontierLimit],
    queryFn: () => api.getRoutingFrontier({ route: activeRouteId, from, to, offset: frontierOffset, limit: frontierLimit }),
    enabled: activeRouteId !== '',
  })
  // 选中路由的候选页：route 优先（search 被忽略）+ candidates_offset/limit 取数。
  const [candOffset, setCandOffset] = useState(0)
  const [candLimit, setCandLimit] = useState(20)
  const changeCandLimit = (l: number) => { setCandLimit(l); setCandOffset(0) }
  useEffect(() => { setCandOffset(0) }, [activeRouteId])
  const pickedRouteQ = useQuery({
    queryKey: ['routing-plan-route', activeRouteId, candOffset, candLimit],
    queryFn: () => api.getRoutingPlan({ route: activeRouteId, candidates_offset: candOffset, candidates_limit: candLimit }),
    enabled: activeRouteId !== '',
  })
  const pickedRoute = useMemo(() => {
    const found = pickedRouteQ.data?.routes.find(r => r.ref.route_class_id === activeRouteId)
    return found ?? routes.find(r => r.ref.route_class_id === activeRouteId)
  }, [pickedRouteQ.data, routes, activeRouteId])

  if (planQ.isLoading) {
    return <div className="grid grid-cols-1 gap-5 lg:grid-cols-2">{Array.from({ length: 4 }).map((_, i) => <Skeleton key={i} className="h-48" />)}</div>
  }
  if (planQ.isError) {
    return <p className="text-sm text-destructive">{t('common.loadFailed', { message: (planQ.error as Error).message })}</p>
  }
  if (planQ.data && planQ.data.total_routes === 0) {
    return (
      <Card>
        <CardContent className="flex flex-col items-center gap-2 py-12 text-muted-foreground">
          <Workflow className="size-10" />
          <p className="font-medium">{t('stats.routing.planEmptyTitle')}</p>
          <p className="text-sm">{t('stats.routing.planEmptyDesc')}</p>
        </CardContent>
      </Card>
    )
  }

  const route = pickedRoute ?? routes[0]
  if (!route || activeRouteId === '') {
    // search 无命中：保持选择器可见（可改词重搜），三卡隐藏。
    return (
      <div className="space-y-6">
        <Card className="p-4">
          <div className="flex flex-wrap items-start gap-5">
            <div className="w-full min-w-0 space-y-1.5 sm:w-[22rem]">
              <Label>{t('stats.routing.route')}</Label>
              <Combobox
                items={routes.map(r => r.ref.route_class_id)}
                filter={() => true}
                autoComplete="none"
                value={null}
                onValueChange={v => setPicked(v ?? undefined)}
                itemToStringLabel={v => labels.get(v) ?? v}
              >
                <ComboboxInput
                  placeholder={t('stats.routing.routePlaceholder')}
                  showClear={false}
                  value={searchInput}
                  onChange={e => setSearchInput(e.target.value)}
                />
                <ComboboxContent>
                  <ComboboxEmpty>{planQ.isFetching ? t('logs.filter.searching') : t('logs.filter.noMatch')}</ComboboxEmpty>
                  <ComboboxList />
                </ComboboxContent>
              </Combobox>
            </div>
            <div className="w-[14rem] shrink-0 space-y-1.5">
              <Label>{t('dateRange.label')}</Label>
              <DateRangePicker value={range} onChange={setRange} />
            </div>
            <div className="flex items-center gap-2 pt-7">
              <Badge variant="secondary" className="font-mono">{t('stats.routing.generation', { gen: planQ.data?.generation ?? 0 })}</Badge>
              <span className="text-xs text-muted-foreground">{t('stats.routing.routesCount', { count: planQ.data?.total_routes ?? 0 })}</span>
            </div>
          </div>
        </Card>
        <Card>
          <CardContent className="flex flex-col items-center gap-2 py-12 text-muted-foreground">
            <Workflow className="size-10" />
            <p className="font-medium">{t('stats.routing.routeNoMatchTitle')}</p>
            <p className="text-sm">{t('stats.routing.routeNoMatchDesc')}</p>
          </CardContent>
        </Card>
      </div>
    )
  }

  return (
    <div className="space-y-6">
      <Card className="p-4">
        <div className="flex flex-wrap items-start gap-5">
          <div className="w-full min-w-0 space-y-1.5 sm:w-[22rem]">
            <Label>{t('stats.routing.route')}</Label>
            <Combobox
              items={routes.map(r => r.ref.route_class_id)}
              filter={() => true}
              autoComplete="none"
              value={routeId || null}
              onValueChange={v => setPicked(v ?? undefined)}
              itemToStringLabel={v => labels.get(v) ?? v}
            >
              {/* 服务端搜索：输入即时反映在受控框（300ms 防抖后发 search 请求——
                  与 logs.tsx 候选搜索同频）；filter 恒真关本地过滤（仓库已验证惯例）。 */}
              <ComboboxInput
                placeholder={t('stats.routing.routePlaceholder')}
                showClear={false}
                value={searchInput}
                onChange={e => setSearchInput(e.target.value)}
              />
              <ComboboxContent>
                {routes.length === 0 && (
                  <ComboboxEmpty>{planQ.isFetching ? t('logs.filter.searching') : t('logs.filter.noMatch')}</ComboboxEmpty>
                )}
                <ComboboxList>
                  {routes.map(r => (
                    <ComboboxItem key={r.ref.route_class_id} value={r.ref.route_class_id}>
                      <span className="min-w-0 truncate">{routeLabel(r)}</span>
                    </ComboboxItem>
                  ))}
                </ComboboxList>
              </ComboboxContent>
            </Combobox>
          </div>
          <div className="w-[14rem] shrink-0 space-y-1.5">
            <Label>{t('dateRange.label')}</Label>
            <DateRangePicker value={range} onChange={setRange} />
          </div>
          <div className="flex items-center gap-2 pt-7">
            <Badge variant="secondary" className="font-mono">{t('stats.routing.generation', { gen: planQ.data?.generation ?? 0 })}</Badge>
            <span className="text-xs text-muted-foreground">{t('stats.routing.routesCount', { count: planQ.data?.total_routes ?? routes.length })}</span>
          </div>
        </div>
      </Card>

      <FlowCard
        flowQ={flowQ}
        offset={flowOffset}
        limit={flowLimit}
        onOffsetChange={setFlowOffset}
        onLimitChange={changeFlowLimit}
      />
      <FrontierCard
        frontierQ={frontierQ}
        offset={frontierOffset}
        limit={frontierLimit}
        onOffsetChange={setFrontierOffset}
        onLimitChange={changeFrontierLimit}
      />
      <PlanCard
        route={route}
        generation={planQ.data?.generation ?? 0}
        candidates={pickedRoute?.candidates ?? []}
        candidatesTotal={pickedRoute?.candidates_total ?? 0}
        candidatesLoading={pickedRouteQ.isLoading}
        candidatesError={pickedRouteQ.isError ? pickedRouteQ.error : null}
        offset={candOffset}
        limit={candLimit}
        onOffsetChange={setCandOffset}
        onLimitChange={changeCandLimit}
      />
    </div>
  )
}

// flow 卡：守恒成立才画 transition-aware Sankey（route → (ordinal,lane) → account →
// outcome/terminal，数据源为服务端 sankey.edges——分页与折叠解耦，翻页不改变图形）；
// 违例 → 告警 + 边表格。
type FlowQuery = { data?: components['schemas']['RoutingFlowResponse']; isLoading: boolean; isError: boolean; error: unknown }

function FlowCard({ flowQ, offset, limit, onOffsetChange, onLimitChange }: {
  flowQ: FlowQuery
  offset: number
  limit: number
  onOffsetChange: (offset: number) => void
  onLimitChange: (limit: number) => void
}) {
  const { t } = useTranslation()
  const data = flowQ.data
  const lanes = useMemo(() => data?.lanes ?? [], [data])
  const edges = useMemo(() => lanes.flatMap(l => l.edges), [lanes])
  const conserved = !!data && data.first_dispatch_chains === data.terminal_chains
  // 旧代际徽标：完整集上的服务端**精确布尔**（stale_generation_present），
  // 翻页不变；false / 未就绪 → 不渲染。合并层一行聚合多代际的链，链次占比只能
  // 给出上界，故服务端不再输出占比（见 openapi 描述）。
  const staleGeneration = data?.stale_generation_present === true

  const sankeyEdges = useMemo(() => data?.sankey.edges ?? [], [data])
  const sankeyData = useMemo(() => {
    if (!data) return null
    return buildFoldedFlowSankey(sankeyEdges, {
      route: truncate(data.route_class_id, 14),
      lane: (ordinal, lane) => `#${ordinal} ${t(`stats.routing.lane.${lane}`, { defaultValue: lane })}`,
      account: id => `A${id}`,
      foldedAccount: () => t('stats.routing.sankey.foldedNode'),
      outcome: o => t(`stats.routing.outcome.${o}`, { defaultValue: o }),
      terminal: o => `${t(`stats.routing.outcome.${o}`, { defaultValue: o })} · ${t('stats.routing.table.finalBadge')}`,
    })
  }, [data, sankeyEdges, t])

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex flex-wrap items-center gap-2">
          {t('stats.routing.flowTitle')}
          {data && (
            <>
              <Badge variant="secondary" className="font-mono">{t('stats.routing.generation', { gen: data.plan_generation })}</Badge>
              {staleGeneration && <Badge variant="outline">{t('stats.routing.staleEdges')}</Badge>}
            </>
          )}
        </CardTitle>
        <CardDescription>{t('stats.routing.flowDesc')}</CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        {flowQ.isError ? (
          <p className="text-sm text-destructive">{t('common.loadFailed', { message: (flowQ.error as Error).message })}</p>
        ) : flowQ.isLoading ? (
          <Skeleton className="h-[320px] w-full" />
        ) : lanes.length === 0 ? (
          <div className="flex flex-col items-center gap-2 py-10 text-muted-foreground">
            <Workflow className="size-10" />
            <p className="font-medium">{t('stats.routing.flowEmptyTitle')}</p>
            <p className="text-sm">{t('stats.routing.flowEmptyDesc')}</p>
          </div>
        ) : (
          <>
            <div className="flex flex-wrap items-center gap-x-6 gap-y-1 text-sm">
              <span className="text-muted-foreground">{t('stats.routing.firstDispatch')}: <span className="font-mono tabular-nums text-foreground">{data?.first_dispatch_chains.toLocaleString()}</span></span>
              <span className="text-muted-foreground">{t('stats.routing.terminal')}: <span className="font-mono tabular-nums text-foreground">{data?.terminal_chains.toLocaleString()}</span></span>
              <span className="text-muted-foreground">{t('stats.routing.incompleteDropped')}: <span className="font-mono tabular-nums text-foreground">{data?.incomplete_chain_dropped.toLocaleString()}</span></span>
              <span className="text-muted-foreground">{t('stats.routing.overflowDropped')}: <span className="font-mono tabular-nums text-foreground">{data?.flow_overflow_dropped_chains.toLocaleString()}</span></span>
              {data?.process_crash_loss_unobservable && <span className="text-xs text-muted-foreground">{t('stats.routing.crashUnobservable')}</span>}
            </div>
            {!conserved && (
              <Alert variant="destructive">
                <AlertTitle>{t('stats.routing.conservationAlertTitle')}</AlertTitle>
                <AlertDescription>{t('stats.routing.conservationAlertDesc', { first: data?.first_dispatch_chains.toLocaleString(), terminal: data?.terminal_chains.toLocaleString() })}</AlertDescription>
              </Alert>
            )}
            {conserved && sankeyData ? (
              <div className="space-y-3">
                {/* folded 标注不含计数：folded_accounts 按层重复计数（同账号在两层
                    被折叠计两次），不是去重账号数，故文案不得声称"其余 N 个账号"。 */}
                {data?.sankey.folded && (
                  <p className="text-xs text-muted-foreground">{t('stats.routing.sankey.foldedNote', { limit: data.sankey.account_limit })}</p>
                )}
                <div className="overflow-x-auto">
                  <ChartContainer config={{}} className="h-[320px] w-full min-w-[640px]">
                    <Sankey
                      accessibilityLayer
                      data={sankeyData}
                      node={FlowSankeyNodeShape}
                      link={FlowSankeyLinkShape}
                      nodeWidth={10}
                      nodePadding={14}
                      margin={{ top: 8, right: 8, bottom: 8, left: 8 }}
                      title={t('stats.routing.sankey.title')}
                      desc={t('stats.routing.sankey.desc')}
                    >
                      <ChartTooltip content={<FlowSankeyTooltip />} />
                    </Sankey>
                  </ChartContainer>
                </div>
                <FlowSankeyLegend />
                <details className="rounded-lg border border-border/50">
                  <summary className="cursor-pointer select-none px-3 py-2 text-xs font-medium text-muted-foreground">
                    {t('stats.routing.sankey.tableToggle')}
                  </summary>
                  <FlowEdgeTable
                    edges={edges}
                    total={data?.total_edges ?? 0}
                    offset={offset}
                    limit={limit}
                    onOffsetChange={onOffsetChange}
                    onLimitChange={onLimitChange}
                  />
                </details>
              </div>
            ) : (
              <FlowEdgeTable
                edges={edges}
                total={data?.total_edges ?? 0}
                offset={offset}
                limit={limit}
                onOffsetChange={onOffsetChange}
                onLimitChange={onLimitChange}
              />
            )}
          </>
        )}
      </CardContent>
    </Card>
  )
}

// edges = 边表当前页（lanes 由全量改为一页）；total 驱动翻页器（完整边行数）。
function FlowEdgeTable({ edges, total, offset, limit, onOffsetChange, onLimitChange }: {
  edges: FlowEdge[]
  total: number
  offset: number
  limit: number
  onOffsetChange: (offset: number) => void
  onLimitChange: (limit: number) => void
}) {
  const { t } = useTranslation()
  return (
    <div>
    <div className="overflow-x-auto">
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead className="text-right">{t('stats.routing.table.ordinal')}</TableHead>
            <TableHead>{t('stats.routing.table.lane')}</TableHead>
            <TableHead className="text-right">{t('stats.routing.table.account')}</TableHead>
            <TableHead className="text-right">{t('stats.routing.table.previous')}</TableHead>
            <TableHead>{t('stats.routing.table.prevOutcome')}</TableHead>
            <TableHead>{t('stats.routing.table.reason')}</TableHead>
            <TableHead>{t('stats.routing.table.outcome')}</TableHead>
            <TableHead>{t('stats.routing.table.terminal')}</TableHead>
            <TableHead className="text-right">{t('stats.routing.table.minGeneration')}</TableHead>
            <TableHead className="text-right">{t('stats.routing.table.chains')}</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody className="[&_td]:py-2.5">
          {edges.map((e, i) => (
            <TableRow key={i}>
              <TableCell className="text-right tabular-nums">{e.ordinal}</TableCell>
              <TableCell className="font-mono text-xs">{t(`stats.routing.lane.${e.lane}`, { defaultValue: e.lane })}</TableCell>
              <TableCell className="text-right font-mono tabular-nums">{e.account_id}</TableCell>
              <TableCell className="text-right font-mono tabular-nums">{e.previous_account_id ?? '—'}</TableCell>
              <TableCell className="text-xs">{e.previous_outcome || '—'}</TableCell>
              <TableCell className="text-xs">{e.transition_reason || '—'}</TableCell>
              <TableCell className="text-xs">{t(`stats.routing.outcome.${e.outcome}`, { defaultValue: e.outcome })}</TableCell>
              <TableCell>{e.is_terminal ? <Badge variant="outline" className="text-xs">{t('stats.routing.table.finalBadge')}</Badge> : '—'}</TableCell>
              <TableCell className="text-right font-mono tabular-nums text-xs">{e.min_generation}</TableCell>
              <TableCell className="text-right tabular-nums">{e.chain_count.toLocaleString()}</TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </div>
    <Pagination total={total} limit={limit} offset={offset} onOffsetChange={onOffsetChange} onLimitChange={onLimitChange} pageSizes={ROUTING_PAGE_SIZES} />
    </div>
  )
}

// frontier 卡：散点（x=每次成功成本，y=成功率 Wilson LCB——均为服务端值）+
// 全候选表（unknown/成本不可知者只呈现观测事实，不上前沿）。
type FrontierQuery = { data?: components['schemas']['RoutingFrontierResponse']; isLoading: boolean; isError: boolean; error: unknown }

function FrontierCard({ frontierQ, offset, limit, onOffsetChange, onLimitChange }: {
  frontierQ: FrontierQuery
  offset: number
  limit: number
  onOffsetChange: (offset: number) => void
  onLimitChange: (limit: number) => void
}) {
  const { t } = useTranslation()
  const candidates = useMemo(() => frontierQ.data?.candidates ?? [], [frontierQ.data])
  const plotted = useMemo(() => candidates.filter(c => c.known && c.cost_known), [candidates])
  const scatterConfig = {
    frontier: { label: t('stats.routing.frontierOn'), color: 'var(--chart-1)' },
    dominated: { label: t('stats.routing.frontierDominated'), color: 'var(--chart-2)' },
  } satisfies ChartConfig
  const toPoint = (c: components['schemas']['RoutingFrontierCandidate']) => ({
    cost: c.cost_per_success,
    lcb: c.success_lcb * 100,
    attempts: c.attempts,
  })
  const frontierPts = useMemo(() => plotted.filter(c => c.on_frontier).map(toPoint), [plotted])
  const dominatedPts = useMemo(() => plotted.filter(c => !c.on_frontier).map(toPoint), [plotted])

  return (
    <Card>
      <CardHeader>
        <CardTitle>{t('stats.routing.frontierTitle')}</CardTitle>
        <CardDescription>{t('stats.routing.frontierDesc')}</CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        {frontierQ.isError ? (
          <p className="text-sm text-destructive">{t('common.loadFailed', { message: (frontierQ.error as Error).message })}</p>
        ) : frontierQ.isLoading ? (
          <Skeleton className="h-[280px] w-full" />
        ) : candidates.length === 0 ? (
          <p className="py-8 text-center text-sm text-muted-foreground">{t('stats.routing.frontierEmpty')}</p>
        ) : (
          <>
            {plotted.length > 0 && (
              <>
                <ChartContainer config={scatterConfig} className="h-[280px] w-full">
                  <ScatterChart>
                    <CartesianGrid />
                    <XAxis type="number" dataKey="cost" name={t('stats.routing.table.costPerSuccess')} tickFormatter={(v: number) => (v > 0 ? formatCost(v) : '$0')} tickLine={false} axisLine={false} fontSize={12} />
                    <YAxis type="number" dataKey="lcb" name={t('stats.routing.frontierLcb')} domain={[0, 100]} tickFormatter={(v: number) => `${v}%`} tickLine={false} axisLine={false} tickMargin={8} fontSize={12} />
                    <ZAxis type="number" dataKey="attempts" range={[40, 240]} name={t('stats.routing.table.attempts')} />
                    <ChartTooltip cursor={{ strokeDasharray: '3 3' }} content={<ChartTooltipContent />} />
                    <Scatter name={t('stats.routing.frontierOn')} data={frontierPts} fill="var(--color-frontier)" />
                    <Scatter name={t('stats.routing.frontierDominated')} data={dominatedPts} fill="var(--color-dominated)" fillOpacity={0.55} />
                  </ScatterChart>
                </ChartContainer>
                {/* 静态两项图例（系列固定；ChartLegend 的 config 查键按显示名，
                    散点系列名对不上 config key 会渲染空标签） */}
                <div className="flex items-center justify-center gap-4 pt-1 text-xs text-muted-foreground">
                  <span className="flex items-center gap-1.5"><span className="size-2 shrink-0 rounded-[2px] bg-(--chart-1)" />{t('stats.routing.frontierOn')}</span>
                  <span className="flex items-center gap-1.5"><span className="size-2 shrink-0 rounded-[2px] bg-(--chart-2) opacity-55" />{t('stats.routing.frontierDominated')}</span>
                </div>
              </>
            )}
            <div className="overflow-x-auto">
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>{t('stats.routing.table.fingerprint')}</TableHead>
                    <TableHead className="text-right">{t('stats.routing.table.account')}</TableHead>
                    <TableHead>{t('stats.routing.table.model')}</TableHead>
                    <TableHead className="text-right">{t('stats.routing.table.attempts')}</TableHead>
                    <TableHead className="text-right">{t('stats.routing.table.successRange')}</TableHead>
                    <TableHead className="text-right">{t('stats.routing.table.ttftRange')}</TableHead>
                    <TableHead className="text-right">{t('stats.routing.table.costPerSuccess')}</TableHead>
                    <TableHead>{t('stats.routing.table.flags')}</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody className="[&_td]:py-2.5">
                  {candidates.map(c => (
                    <TableRow key={c.candidate_fingerprint}>
                      <TableCell className="font-mono text-xs" title={c.candidate_fingerprint}>{truncate(c.candidate_fingerprint, 12)}</TableCell>
                      <TableCell className="text-right font-mono tabular-nums">{c.known ? c.account_id : '—'}</TableCell>
                      <TableCell className="text-xs">{c.known ? c.mapped_model : <span className="text-muted-foreground">{t('stats.routing.flagUnknown')}</span>}</TableCell>
                      <TableCell className="text-right tabular-nums">{c.attempts.toLocaleString()} / {c.successes.toLocaleString()}</TableCell>
                      <TableCell className="text-right font-mono text-xs tabular-nums">{`${(c.success_lcb * 100).toFixed(1)}–${(c.success_ucb * 100).toFixed(1)}%`}</TableCell>
                      <TableCell className="text-right font-mono text-xs tabular-nums">{c.ttft_known ? `${fmtTTFT(c.ttft_lcb)}–${fmtTTFT(c.ttft_ucb)}` : '—'}</TableCell>
                      <TableCell className="text-right font-mono text-xs tabular-nums">{c.cost_known ? formatCost(c.cost_per_success) : '—'}</TableCell>
                      <TableCell>
                        <div className="flex flex-wrap gap-1">
                          {c.on_frontier && <Badge variant="default" className="text-xs">{t('stats.routing.frontierOn')}</Badge>}
                          {c.insufficient && <Badge variant="outline" className="text-xs">{t('stats.routing.flagInsufficient')}</Badge>}
                          {!c.known && <Badge variant="secondary" className="text-xs">{t('stats.routing.flagUnknown')}</Badge>}
                          {c.known && !c.cost_known && <Badge variant="secondary" className="text-xs">{t('stats.routing.flagCostUnknown')}</Badge>}
                        </div>
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            </div>
            <Pagination total={frontierQ.data?.total_candidates ?? 0} limit={limit} offset={offset} onOffsetChange={onOffsetChange} onLimitChange={onLimitChange} pageSizes={ROUTING_PAGE_SIZES} />
          </>
        )}
      </CardContent>
    </Card>
  )
}

// plan 卡：当前 generation 的选中路由直通序（primary/explore/degraded）+ 候选目录。
// 芯片不做分页（无行结构）：每组客户端封顶 CHIP_LIMIT 个 + 展开/收起，四组各自
// 独立计数（一组超限不影响其他组）。候选表走服务端分页（candidates_offset/limit）。
type PlanCandidate = components['schemas']['RoutingPlanCandidate']

const CHIP_LIMIT = 20

// 路由观测三处表格的服务端每页上限（openapi: `maximum: 200`）。Pagination 的共享
// 列表含 1000，超过服务端上限会造成「本地页码按 1000 算、服务端只回 200 行」的不一致，
// 故三处均显式传入本列表。
const ROUTING_PAGE_SIZES = [10, 20, 50, 100, 200]

// 选择器轻量调用一次取回的路由数上限（服务端上限 200；`candidates_limit=0` 不返回候选，
// 避免 limit × candidates_limit 放大）。目录超过此数时被截断，由 `search` 收窄。
const SELECTOR_ROUTE_LIMIT = 100

function ChipGroup({ ids, weights, expanded, onToggle }: {
  ids: number[]
  weights?: Record<string, number>
  expanded: boolean
  onToggle: () => void
}) {
  const { t } = useTranslation()
  const visible = expanded ? ids : ids.slice(0, CHIP_LIMIT)
  return (
    <div className="flex flex-wrap items-center gap-1.5">
      {ids.length === 0 && <span className="text-xs text-muted-foreground">—</span>}
      {visible.map((id, i) => (
        <span key={`${id}-${i}`} className="inline-flex items-center gap-1 rounded-full border border-black/10 bg-black/[0.04] px-2 py-0.5 font-mono text-xs dark:border-white/10 dark:bg-white/10">
          {i + 1}. {id}{weights ? ` ×${weights[String(id)] ?? 0}` : ''}
        </span>
      ))}
      {ids.length > CHIP_LIMIT && (
        <button
          type="button"
          onClick={onToggle}
          aria-expanded={expanded}
          className="inline-flex items-center rounded-full border border-dashed border-black/20 px-2 py-0.5 text-xs text-muted-foreground transition-colors hover:border-black/40 hover:text-foreground dark:border-white/20 dark:hover:border-white/40"
        >
          {expanded
            ? t('stats.routing.chipsCollapse')
            : t('stats.routing.chipsExpand', { count: ids.length - CHIP_LIMIT })}
        </button>
      )}
    </div>
  )
}

function PlanCard({ route, generation, candidates, candidatesTotal, candidatesLoading, candidatesError, offset, limit, onOffsetChange, onLimitChange }: {
  route: PlanRoute
  generation: number
  candidates: PlanCandidate[]
  candidatesTotal: number
  candidatesLoading: boolean
  candidatesError: unknown
  offset: number
  limit: number
  onOffsetChange: (offset: number) => void
  onLimitChange: (limit: number) => void
}) {
  const { t } = useTranslation()
  // 四组展开态各自独立：key = primary / explore / degraded / fallback。
  const [chipExpanded, setChipExpanded] = useState<Set<string>>(new Set())
  const toggleChips = (key: string) => {
    setChipExpanded(prev => {
      const next = new Set(prev)
      if (next.has(key)) next.delete(key)
      else next.add(key)
      return next
    })
  }
  const chipProps = (key: string) => ({
    expanded: chipExpanded.has(key),
    onToggle: () => toggleChips(key),
  })
  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex flex-wrap items-center gap-2">
          {t('stats.routing.planTitle')}
          <Badge variant="secondary" className="font-mono">{t('stats.routing.generation', { gen: generation })}</Badge>
        </CardTitle>
        <CardDescription>{t('stats.routing.planDesc')}</CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        <div className="grid grid-cols-1 gap-4 sm:grid-cols-3">
          <div>
            <div className="mb-1.5 text-xs font-medium text-muted-foreground">{t('stats.routing.planPrimary')}</div>
            <ChipGroup ids={route.primary} {...chipProps('primary')} />
          </div>
          <div>
            <div className="mb-1.5 text-xs font-medium text-muted-foreground">{t('stats.routing.planExplore')}</div>
            <ChipGroup ids={route.explore.ids} weights={route.explore.weights} {...chipProps('explore')} />
            <div className="mt-1 text-xs text-muted-foreground">{t('stats.routing.planExploreTotal', { total: route.explore.total.toLocaleString() })}</div>
          </div>
          <div>
            <div className="mb-1.5 text-xs font-medium text-muted-foreground">{t('stats.routing.planDegraded')}</div>
            <ChipGroup ids={route.degraded} {...chipProps('degraded')} />
            {route.explore.fallback.length > 0 && (
              <div className="mt-1.5 text-xs text-muted-foreground">{t('stats.routing.planFallback')}</div>
            )}
            <ChipGroup ids={route.explore.fallback} {...chipProps('fallback')} />
          </div>
        </div>
        {candidatesError ? (
          <p className="text-sm text-destructive">{t('common.loadFailed', { message: (candidatesError as Error).message })}</p>
        ) : candidatesLoading ? (
          <Skeleton className="h-32 w-full" />
        ) : (
          <div>
          <div className="overflow-x-auto">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead className="text-right">{t('stats.routing.table.account')}</TableHead>
                <TableHead className="text-right">{t('stats.routing.table.template')}</TableHead>
                <TableHead className="text-right">{t('stats.routing.table.revision')}</TableHead>
                <TableHead>{t('stats.routing.table.model')}</TableHead>
                <TableHead>{t('stats.routing.table.qualityClass')}</TableHead>
                <TableHead className="text-right">{t('stats.routing.table.multiplier')}</TableHead>
                <TableHead>{t('stats.routing.table.fingerprint')}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody className="[&_td]:py-2.5">
              {candidates.map(c => (
                <TableRow key={`${c.account_id}-${c.identity_fingerprint}`}>
                  <TableCell className="text-right font-mono tabular-nums">{c.account_id}</TableCell>
                  <TableCell className="text-right font-mono tabular-nums">{c.template_id}</TableCell>
                  <TableCell className="text-right font-mono tabular-nums">{c.identity_revision}</TableCell>
                  <TableCell className="text-xs">{c.mapped_model || '—'}</TableCell>
                  <TableCell className="font-mono text-xs" title={c.quality_class_id}>{truncate(c.quality_class_id, 12)}</TableCell>
                  <TableCell className="text-right font-mono tabular-nums">{`${(c.upstream_cost_multiplier_bp / 100).toFixed(2)}×`}</TableCell>
                  <TableCell className="font-mono text-xs" title={c.fingerprint || c.identity_fingerprint}>{truncate(c.fingerprint || c.identity_fingerprint, 12)}</TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
          </div>
          <Pagination total={candidatesTotal} limit={limit} offset={offset} onOffsetChange={onOffsetChange} onLimitChange={onLimitChange} pageSizes={ROUTING_PAGE_SIZES} />
          </div>
        )}
      </CardContent>
    </Card>
  )
}
