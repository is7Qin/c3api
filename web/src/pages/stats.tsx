// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

import { useMemo, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { BarChart3 } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { Area, AreaChart, Bar, BarChart, CartesianGrid, Line, XAxis, YAxis } from 'recharts'
import { api } from '@/App'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { ChartContainer, ChartLegend, ChartLegendContent, ChartTooltip, ChartTooltipContent, type ChartConfig } from '@/components/ui/chart'
import { DateRangePicker } from '@/components/date-range-picker'
import { Label } from '@/components/ui/label'
import { Skeleton } from '@/components/ui/skeleton'
import { Tabs, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { fmtTTFT, formatDateTime, toRFC3339 } from '@/components/fmt'

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
                  <CartesianGrid vertical={false} />
                  <XAxis dataKey="label" tickCount={chartData.length} tickLine={false} tickMargin={10} axisLine={false} fontSize={12} />
                  <YAxis tickLine={false} axisLine={false} tickMargin={8} fontSize={12} allowDecimals={false} />
                  <ChartTooltip content={<ChartTooltipContent />} />
                  <Bar dataKey="requests" fill="var(--color-requests)" radius={4} />
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
    </div>
  )
}
