// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

import { useMemo, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { BarChart3, Workflow } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { Area, AreaChart, Bar, BarChart, CartesianGrid, Line, Scatter, ScatterChart, XAxis, YAxis, ZAxis } from 'recharts'
import { api } from '@/App'
import type { components } from '@/lib/api/schema'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { ChartContainer, ChartLegend, ChartLegendContent, ChartTooltip, ChartTooltipContent, type ChartConfig } from '@/components/ui/chart'
import { DateRangePicker } from '@/components/date-range-picker'
import { Label } from '@/components/ui/label'
import { Badge } from '@/components/ui/badge'
import { Skeleton } from '@/components/ui/skeleton'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert'
import { Combobox, ComboboxContent, ComboboxEmpty, ComboboxInput, ComboboxItem, ComboboxList } from '@/components/ui/combobox'
import { fmtTTFT, formatCost, formatDateTime, toRFC3339, truncate } from '@/components/fmt'

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
    () => ({ from: toRFC3339(range.from)!, to: toRFC3339(range.to)!, granularity }),
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
  const labeledRows = useMemo(() => rows.map(r => {
    const d = r.BucketTime ? new Date(r.BucketTime) : null
    const label = d && !Number.isNaN(d.getTime())
      ? granularity === 'hour'
        ? `${pad2(d.getMonth() + 1)}-${pad2(d.getDate())} ${pad2(d.getHours())}:${pad2(d.getMinutes())}`
        : `${pad2(d.getMonth() + 1)}-${pad2(d.getDate())}`
      : r.BucketTime ?? '—'
    return { ...r, label, time: r.BucketTime ?? '' }
  }), [rows, granularity])

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
                  <ChartLegend content={<ChartLegendContent onItemClick={toggleSeries} hiddenKeys={hidden} />} />
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
          <Table>
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
                      <TableCell className="text-xs text-muted-foreground whitespace-nowrap tabular-nums">{formatDateTime(r.time)}</TableCell>
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

// —— Routing Flow tab（Todo 22）：渲染服务端 rollup 聚合与当前计划投影。
// 铁律：不在浏览器侧重算 Wilson 区间 / frontier 支配 / 计划编译——所有数值
// 直出 API；守恒失败（first≠terminal）时降级为表格并显式告警，不画无效图。

type PlanRoute = components['schemas']['RoutingPlanRoute']
type FlowEdge = components['schemas']['RoutingFlowEdge']

const OUTCOME_PALETTE = ['var(--chart-1)', 'var(--chart-2)', 'var(--chart-3)', 'var(--chart-4)', 'var(--chart-5)']

const routeLabel = (r: PlanRoute) => `${r.ref.model} · ${r.ref.format} · ${r.ref.operation_tag} · g${r.ref.group_id}`

function RoutingPanel({ range, setRange }: {
  range: { from: string; to: string }
  setRange: (r: { from: string; to: string }) => void
}) {
  const { t } = useTranslation()
  const planQ = useQuery({ queryKey: ['routing-plan'], queryFn: () => api.getRoutingPlan() })
  const routes = useMemo(() => planQ.data?.routes ?? [], [planQ.data])
  const [picked, setPicked] = useState<string | undefined>(undefined)
  // 选中项失效（计划换代/路由消失）→ 回落首条；空目录 = 未发布计划
  const routeId = picked && routes.some(r => r.ref.route_class_id === picked)
    ? picked
    : (routes[0]?.ref.route_class_id ?? '')
  const from = toRFC3339(range.from) ?? ''
  const to = toRFC3339(range.to) ?? ''

  const flowQ = useQuery({
    queryKey: ['routing-flow', routeId, from, to],
    queryFn: () => api.getRoutingFlow({ route: routeId, from, to }),
    enabled: routeId !== '',
  })
  const frontierQ = useQuery({
    queryKey: ['routing-frontier', routeId, from, to],
    queryFn: () => api.getRoutingFrontier({ route: routeId, from, to }),
    enabled: routeId !== '',
  })

  if (planQ.isLoading) {
    return <div className="grid grid-cols-1 gap-5 lg:grid-cols-2">{Array.from({ length: 4 }).map((_, i) => <Skeleton key={i} className="h-48" />)}</div>
  }
  if (planQ.isError) {
    return <p className="text-sm text-destructive">{t('common.loadFailed', { message: (planQ.error as Error).message })}</p>
  }
  if (routes.length === 0) {
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

  const labels = new Map(routes.map(r => [r.ref.route_class_id, routeLabel(r)]))
  const route = routes.find(r => r.ref.route_class_id === routeId) ?? routes[0]

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
              <ComboboxInput placeholder={t('stats.routing.routePlaceholder')} showClear={false} />
              <ComboboxContent>
                <ComboboxEmpty>{t('logs.filter.noMatch')}</ComboboxEmpty>
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
            <span className="text-xs text-muted-foreground">{t('stats.routing.routesCount', { count: routes.length })}</span>
          </div>
        </div>
      </Card>

      <FlowCard flowQ={flowQ} />
      <FrontierCard frontierQ={frontierQ} />
      <PlanCard route={route} generation={planQ.data?.generation ?? 0} />
    </div>
  )
}

// flow 卡：守恒成立才画 (ordinal,lane)×outcome 堆叠柱；违例 → 告警 + 边表格。
type FlowQuery = { data?: components['schemas']['RoutingFlowResponse']; isLoading: boolean; isError: boolean; error: unknown }

function FlowCard({ flowQ }: { flowQ: FlowQuery }) {
  const { t } = useTranslation()
  const data = flowQ.data
  const lanes = useMemo(() => data?.lanes ?? [], [data])
  const edges = useMemo(() => lanes.flatMap(l => l.edges), [lanes])
  const conserved = !!data && data.first_dispatch_chains === data.terminal_chains
  const staleEdges = data ? edges.filter(e => e.generation !== data.plan_generation).length : 0

  const outcomes = useMemo(() => {
    const s = new Set<string>()
    for (const e of edges) s.add(e.outcome)
    return [...s].sort()
  }, [edges])
  const flowConfig = useMemo(() => {
    const c: ChartConfig = {}
    outcomes.forEach((o, i) => {
      c[o] = { label: t(`stats.routing.outcome.${o}`, { defaultValue: o }), color: OUTCOME_PALETTE[i % OUTCOME_PALETTE.length] }
    })
    return c
  }, [outcomes, t])
  const chartData = useMemo(() => lanes.map(l => {
    const row: Record<string, number | string> = { label: `#${l.ordinal} ${t(`stats.routing.lane.${l.lane}`, { defaultValue: l.lane })}` }
    for (const e of l.edges) row[e.outcome] = ((row[e.outcome] as number | undefined) ?? 0) + e.chain_count
    return row
  }), [lanes, t])

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex flex-wrap items-center gap-2">
          {t('stats.routing.flowTitle')}
          {data && (
            <>
              <Badge variant="secondary" className="font-mono">{t('stats.routing.generation', { gen: data.plan_generation })}</Badge>
              {staleEdges > 0 && <Badge variant="outline">{t('stats.routing.staleEdges', { count: staleEdges })}</Badge>}
            </>
          )}
        </CardTitle>
        <CardDescription>{t('stats.routing.flowDesc')}</CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        {flowQ.isError ? (
          <p className="text-sm text-destructive">{t('common.loadFailed', { message: (flowQ.error as Error).message })}</p>
        ) : flowQ.isLoading ? (
          <Skeleton className="h-[280px] w-full" />
        ) : lanes.length === 0 ? (
          <div className="flex flex-col items-center gap-2 py-10 text-muted-foreground">
            <BarChart3 className="size-10" />
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
            {conserved ? (
              <ChartContainer config={flowConfig} className="h-[280px] w-full">
                <BarChart accessibilityLayer data={chartData}>
                  <CartesianGrid vertical={false} />
                  <XAxis dataKey="label" tickLine={false} tickMargin={10} axisLine={false} fontSize={12} />
                  <YAxis tickLine={false} axisLine={false} tickMargin={8} fontSize={12} allowDecimals={false} />
                  <ChartTooltip content={<ChartTooltipContent />} />
                  <ChartLegend content={<ChartLegendContent />} />
                  {outcomes.map(o => (
                    <Bar key={o} dataKey={o} stackId="chains" fill={`var(--color-${o})`} radius={2} maxBarSize={48} />
                  ))}
                </BarChart>
              </ChartContainer>
            ) : (
              <FlowEdgeTable edges={edges} />
            )}
          </>
        )}
      </CardContent>
    </Card>
  )
}

function FlowEdgeTable({ edges }: { edges: FlowEdge[] }) {
  const { t } = useTranslation()
  return (
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
            <TableHead className="text-right">{t('stats.routing.table.generation')}</TableHead>
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
              <TableCell className="text-right font-mono tabular-nums text-xs">{e.generation}</TableCell>
              <TableCell className="text-right tabular-nums">{e.chain_count.toLocaleString()}</TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </div>
  )
}

// frontier 卡：散点（x=每次成功成本，y=成功率 Wilson LCB——均为服务端值）+
// 全候选表（unknown/成本不可知者只呈现观测事实，不上前沿）。
type FrontierQuery = { data?: components['schemas']['RoutingFrontierResponse']; isLoading: boolean; isError: boolean; error: unknown }

function FrontierCard({ frontierQ }: { frontierQ: FrontierQuery }) {
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
          </>
        )}
      </CardContent>
    </Card>
  )
}

// plan 卡：当前 generation 的选中路由直通序（primary/explore/degraded）+ 候选目录。
function PlanCard({ route, generation }: { route: PlanRoute; generation: number }) {
  const { t } = useTranslation()
  const chips = (ids: number[], weights?: Record<string, number>) => (
    <div className="flex flex-wrap gap-1.5">
      {ids.length === 0 && <span className="text-xs text-muted-foreground">—</span>}
      {ids.map((id, i) => (
        <span key={`${id}-${i}`} className="inline-flex items-center gap-1 rounded-full border border-black/10 bg-black/[0.04] px-2 py-0.5 font-mono text-xs dark:border-white/10 dark:bg-white/10">
          {i + 1}. {id}{weights ? ` ×${weights[String(id)] ?? 0}` : ''}
        </span>
      ))}
    </div>
  )
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
            {chips(route.primary)}
          </div>
          <div>
            <div className="mb-1.5 text-xs font-medium text-muted-foreground">{t('stats.routing.planExplore')}</div>
            {chips(route.explore.ids, route.explore.weights)}
            <div className="mt-1 text-xs text-muted-foreground">{t('stats.routing.planExploreTotal', { total: route.explore.total.toLocaleString() })}</div>
          </div>
          <div>
            <div className="mb-1.5 text-xs font-medium text-muted-foreground">{t('stats.routing.planDegraded')}</div>
            {chips(route.degraded)}
            {route.explore.fallback.length > 0 && (
              <div className="mt-1.5 text-xs text-muted-foreground">{t('stats.routing.planFallback')}</div>
            )}
            {chips(route.explore.fallback)}
          </div>
        </div>
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
              {route.candidates.map(c => (
                <TableRow key={`${c.account_id}-${c.identity_fingerprint}`}>
                  <TableCell className="text-right font-mono tabular-nums">{c.account_id}</TableCell>
                  <TableCell className="text-right font-mono tabular-nums">{c.template_id}</TableCell>
                  <TableCell className="text-right font-mono tabular-nums">{c.lifecycle_revision}</TableCell>
                  <TableCell className="text-xs">{c.mapped_model || '—'}</TableCell>
                  <TableCell className="font-mono text-xs" title={c.quality_class_id}>{truncate(c.quality_class_id, 12)}</TableCell>
                  <TableCell className="text-right font-mono tabular-nums">{`${(c.upstream_cost_multiplier_bp / 100).toFixed(2)}×`}</TableCell>
                  <TableCell className="font-mono text-xs" title={c.fingerprint || c.identity_fingerprint}>{truncate(c.fingerprint || c.identity_fingerprint, 12)}</TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </div>
      </CardContent>
    </Card>
  )
}
