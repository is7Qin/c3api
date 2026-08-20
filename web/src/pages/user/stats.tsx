// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

import { useMemo, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { BarChart3 } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { Area, AreaChart, Bar, BarChart, CartesianGrid, Line, XAxis, YAxis } from 'recharts'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { ChartContainer, ChartLegend, ChartLegendContent, ChartTooltip, ChartTooltipContent, type ChartConfig } from '@/components/ui/chart'
import { DateRangePicker } from '@/components/date-range-picker'
import { Label } from '@/components/ui/label'
import { Skeleton } from '@/components/ui/skeleton'
import { Tabs, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { formatDateTime, toRFC3339 } from '@/components/fmt'
import { userApi } from '@/lib/api/client'
import { fmtTTFT, mergeBuckets, summarizeTTFT, type Granularity } from '@/lib/stats-merge'

type Metric = 'requests' | 'tokens'

// 默认近 24h（组件挂载时固定一次，避免渲染期时间漂移）。
function defaultRange() {
  const to = new Date()
  const from = new Date(to.getTime() - 24 * 3600 * 1000)
  const local = (d: Date) =>
    `${d.getFullYear()}-${pad2(d.getMonth() + 1)}-${pad2(d.getDate())}T${pad2(d.getHours())}:${pad2(d.getMinutes())}`
  return { from: local(from), to: local(to) }
}

const pad2 = (n: number) => String(n).padStart(2, '0')

// 后端按 (bucket_time, group, account, template, model, is_error) 返回多行，
// 同一时间桶可能有多行 → 前端按 BucketTime 合并求和（图表与表格共用；
// TTFT 合并语义见 stats-merge.ts——rewrite spec 2026-08-14 评审 P1）。

export default function UserStats() {
  const { t } = useTranslation()
  const [range, setRange] = useState(defaultRange)
  // 用户端默认按日聚合（与管理端默认 hour 区分）。
  const [granularity, setGranularity] = useState<Granularity>('day')
  const [metric, setMetric] = useState<Metric>('tokens')
  // 图例点击开关序列（2026-08-14：ChartLegendContent 增强 onItemClick/hiddenKeys）。
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
    () => ({ from: toRFC3339(range.from), to: toRFC3339(range.to), granularity }),
    [range, granularity]
  )
  const { data, isLoading, isError, error } = useQuery({
    queryKey: ['user', 'stats', params],
    queryFn: () => userApi.getUserStats(params),
  })
  const rows = useMemo(() => mergeBuckets(data ?? [], granularity), [data, granularity])
  // TTFT 范围汇总（同 mergeBuckets 合并语义：avg 加权、pN 取请求量最大桶近似）。
  const ttft = useMemo(() => summarizeTTFT(rows), [rows])

  // 主题色走 --chart-* 变量（ChartStyle 注入 --color-*）；requests 柱图用 chart-1，
  // tokens 独立面积四序列 + 命中率线用 chart-1..5（spec 2026-08-14）。
  const chartConfig = {
    requests: { label: t('user.stats.metricRequests'), color: 'var(--chart-1)' },
    input: { label: t('user.stats.chart.seriesInput'), color: 'var(--chart-1)' },
    cacheRead: { label: t('user.stats.chart.seriesCacheRead'), color: 'var(--chart-2)' },
    output: { label: t('user.stats.chart.seriesOutput'), color: 'var(--chart-3)' },
    cacheWrite: { label: t('user.stats.chart.seriesCacheWrite'), color: 'var(--chart-4)' },
    hitRate: { label: t('user.stats.chart.seriesHitRate'), color: 'var(--chart-5)' },
  } satisfies ChartConfig

  // 图表只消费 label + 指标值列，dataKey 与 chartConfig 键对齐（官方示例写法）。
  // 读缓存命中率（spec 2026-08-14 钉死）：CacheRead / (CacheRead + Input)，两者均 0 → 0%。
  const chartData = useMemo(
    () => rows.map(r => {
      const cacheRead = r.CacheReadTokens
      const input = r.InputTokens
      return {
        label: r.label,
        requests: r.RequestCount,
        input,
        cacheRead,
        output: r.OutputTokens,
        cacheWrite: r.CacheCreationTokens,
        hitRate: cacheRead + input > 0 ? (cacheRead / (cacheRead + input)) * 100 : 0,
      }
    }),
    [rows]
  )

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">{t('user.stats.title')}</h1>
        <p className="text-sm text-muted-foreground">{t('user.stats.subtitle')}</p>
      </div>

      {/* 控制：时间范围 + 粒度 + 指标（管理端修复版同款：
          flex-nowrap + 子项 shrink-0 + overflow-x-auto：窄窗口横向滚动而非换行；
          items-start 顶对齐（结构性防下沉）：打开日历后左列高度变化不影响右列位置。 */}
      <Card className="p-4">
        <div className="flex flex-nowrap items-start gap-5 overflow-x-auto">
          <div className="w-[14rem] shrink-0 space-y-1.5">
            <Label>{t('dateRange.label')}</Label>
            <DateRangePicker value={range} onChange={setRange} />
          </div>
          <div className="shrink-0 space-y-1.5">
            <Label>{t('user.stats.granularity')}</Label>
            <Tabs value={granularity} onValueChange={v => v && setGranularity(v as Granularity)}>
              <TabsList>
                <TabsTrigger value="hour">{t('user.stats.granularityHour')}</TabsTrigger>
                <TabsTrigger value="day">{t('user.stats.granularityDay')}</TabsTrigger>
              </TabsList>
            </Tabs>
          </div>
          <div className="shrink-0 space-y-1.5">
            <Label>{t('user.stats.metric')}</Label>
            <Tabs value={metric} onValueChange={v => v && setMetric(v as Metric)}>
              <TabsList>
                <TabsTrigger value="requests">{t('user.stats.metricRequests')}</TabsTrigger>
                <TabsTrigger value="tokens">{t('user.stats.metricTokens')}</TabsTrigger>
              </TabsList>
            </Tabs>
          </div>
        </div>
      </Card>

      {/* 图表 */}
      <Card>
        <CardHeader>
          <CardTitle>{metric === 'requests' ? t('user.stats.chartRequestsTitle') : t('user.stats.chartTokensTitle')}</CardTitle>
          <CardDescription>{t('user.stats.chartDesc')}</CardDescription>
        </CardHeader>
        <CardContent>
          {isError ? (
            <p className="text-sm text-destructive">{t('common.loadFailed', { message: (error as Error).message })}</p>
          ) : isLoading ? (
            <Skeleton className="h-[320px] w-full" />
          ) : rows.length === 0 ? (
            <div className="flex flex-col items-center gap-2 py-10 text-muted-foreground">
              <BarChart3 className="size-10" />
              <p className="font-medium">{t('user.stats.emptyTitle')}</p>
              <p className="text-sm">{t('user.stats.emptyDesc')}</p>
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
                // Token 构成独立面积（四序列各自从 0 基线起画，直接比较绝对高度；
                // fillOpacity 防重叠遮挡，描边区分）。
                // 读缓存命中率 = CacheRead / (CacheRead + Input)，右轴 [0,100]%，虚线。
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

      {/* TTFT 卡（rewrite spec 2026-08-14：range 汇总——avg = Σ(avg×count)/Σcount
          加权、p95/p99 取请求量最大桶的 pN 近似；无样本 = 0） */}
      <Card>
        <CardHeader>
          <CardTitle>{t('user.stats.ttft.title')}</CardTitle>
          <CardDescription>{t('user.stats.ttft.desc')}</CardDescription>
        </CardHeader>
        <CardContent>
          <div className="grid grid-cols-3 gap-4">
            {[
              { key: 'avg', labelKey: 'user.stats.ttft.avg', value: ttft.avg },
              { key: 'p95', labelKey: 'user.stats.ttft.p95', value: ttft.p95 },
              { key: 'p99', labelKey: 'user.stats.ttft.p99', value: ttft.p99 },
            ].map(({ key, labelKey, value }) => (
              <div key={key}>
                <div className="text-sm text-muted-foreground">{t(labelKey)}</div>
                <div className="text-2xl font-semibold tabular-nums">{fmtTTFT(value)}</div>
              </div>
            ))}
          </div>
        </CardContent>
      </Card>

      {/* 明细表 */}
      <Card className="bg-transparent border-0 shadow-none backdrop-blur-none p-0">
        {isError ? (
          <p className="p-4 text-sm text-destructive">{t('common.loadFailed', { message: (error as Error).message })}</p>
        ) : (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>{t('user.stats.table.time')}</TableHead>
                <TableHead className="text-right">{t('user.stats.table.requests')}</TableHead>
                <TableHead className="text-right">{t('user.stats.table.errors')}</TableHead>
                <TableHead className="text-right">{t('user.stats.table.calls')}</TableHead>
                <TableHead className="text-right">{t('user.stats.table.promptTokens')}</TableHead>
                <TableHead className="text-right">{t('user.stats.table.completionTokens')}</TableHead>
                <TableHead className="text-right">{t('user.stats.table.cacheReadTokens')}</TableHead>
                <TableHead className="text-right">{t('user.stats.table.cacheCreationTokens')}</TableHead>
                <TableHead className="text-right">{t('user.stats.table.totalTokens')}</TableHead>
                <TableHead className="text-right">{t('user.stats.table.cost')}</TableHead>
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
                : rows.length === 0
                  ? (
                    <TableRow>
                      <TableCell colSpan={10} className="!py-10 text-center text-muted-foreground">{t('user.stats.emptyTitle')}</TableCell>
                    </TableRow>
                  )
                  : rows.map(r => (
                    <TableRow key={r.time}>
                      <TableCell className="text-xs text-muted-foreground whitespace-nowrap tabular-nums">{formatDateTime(r.time)}</TableCell>
                      <TableCell className="text-right tabular-nums">{r.RequestCount}</TableCell>
                      <TableCell className="text-right tabular-nums">{r.ErrorCount}</TableCell>
                      <TableCell className="text-right tabular-nums">{r.CallCount}</TableCell>
                      <TableCell className="text-right tabular-nums">{r.InputTokens}</TableCell>
                      <TableCell className="text-right tabular-nums">{r.OutputTokens}</TableCell>
                      <TableCell className="text-right tabular-nums">{r.CacheReadTokens}</TableCell>
                      <TableCell className="text-right tabular-nums">{r.CacheCreationTokens}</TableCell>
                      <TableCell className="text-right tabular-nums">{r.TotalTokens}</TableCell>
                      <TableCell className="text-right tabular-nums">{`$${r.Cost.toFixed(4)}`}</TableCell>
                    </TableRow>
                  ))}
            </TableBody>
          </Table>
        )}
      </Card>
    </div>
  )
}
