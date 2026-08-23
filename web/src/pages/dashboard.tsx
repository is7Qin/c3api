// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

import { useQuery } from '@tanstack/react-query'
import { motion } from 'framer-motion'
import { Activity, AlertTriangle, Boxes, Coins, FolderOpen, Gauge, PowerOff, Users, Zap } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { Bar, BarChart, CartesianGrid, XAxis, YAxis } from 'recharts'
import { ChartContainer, ChartTooltip, ChartTooltipContent, type ChartConfig } from '@/components/ui/chart'
import { api } from '@/App'
import { Card, CardAction, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Badge } from '@/components/ui/badge'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { Skeleton } from '@/components/ui/skeleton'
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert'
import { StatusBadge } from '@/components/status-badge'
import { fmtTTFT, formatPercent, truncate } from '@/components/fmt'

const fadeUp = {
  initial: { opacity: 0, y: 12 },
  animate: { opacity: 1, y: 0 },
}

// 统计卡 grid 的官方 dashboard-01 处理：浅色卡片带顶部 primary 微渐变 + 细阴影，深色回退纯 card。
const cardGrid = 'grid grid-cols-1 gap-5 *:data-[slot=card]:bg-linear-to-t *:data-[slot=card]:from-primary/5 *:data-[slot=card]:to-card *:data-[slot=card]:shadow-xs dark:*:data-[slot=card]:bg-card'

// errTop 柱图数据行（账号维度——name = 账号名）。
type ErrRow = { name: string; err_rate: number; err_count: number }

// trend 日桶行（date = UTC 日 YYYY-MM-DD）。
type TrendRow = { date: string; requests: number; cost_usd: number; errors: number; tokens: number }

// 大数紧凑格式（用户裁决 2026-08-14）：≥1e9 → X.XX B、≥1e6 → X.XX M，否则千分位。
function formatCompact(n: number): string {
  if (n >= 1e9) return `${(n / 1e9).toFixed(2)} B`
  if (n >= 1e6) return `${(n / 1e6).toFixed(2)} M`
  return n.toLocaleString()
}

export default function Dashboard() {
  const { t } = useTranslation()
  // 总览聚合面 30s 轮询（服务端 TTL 30s 缓存）+ 实时并发排行 10s 轮询（服务端
  // TTL 2s 缓存）——两端点轮询频率各自独立（spec 2026-08-14 §3）。
  const overviewQ = useQuery({ queryKey: ['overview'], queryFn: () => api.getOverview(), refetchInterval: 30_000 })
  const usersTopQ = useQuery({ queryKey: ['users-top'], queryFn: () => api.getUsersTop(), refetchInterval: 10_000 })

  const ov = overviewQ.data
  const accounts = ov?.accounts
  const statusCounts = {
    active: accounts?.active ?? 0,
    unhealthy: accounts?.unhealthy ?? 0,
    '429': accounts?.['429'] ?? 0,
    disabled: accounts?.disabled ?? 0,
  } as const

  // 并发水位：concurrency / max_concurrency 合计（服务端聚合）。
  const totalCur = accounts?.concurrency ?? 0
  const totalMax = accounts?.max_concurrency ?? 0
  const water = totalMax > 0 ? Math.min(totalCur / totalMax, 1) : 0

  // err_rate Top 5（账号维度，服务端排序）。
  const errData: ErrRow[] = (ov?.err_top ?? []).map(e => ({
    name: e.name,
    err_rate: e.err_rate,
    err_count: e.err_count,
  }))

  // 近 N 天日桶（趋势图；X 轴 MM-DD，tooltip 带费用/错误/token）。
  const trend: TrendRow[] = (ov?.trend ?? []).map(d => ({
    date: d.date,
    requests: d.requests,
    cost_usd: d.cost_usd,
    errors: d.errors,
    tokens: d.tokens,
  }))

  // 实时并发排行（本实例视角）+ other 归并。
  const usersTop = usersTopQ.data?.users ?? []
  const otherConc = usersTopQ.data?.other_concurrency ?? 0

  const loading = overviewQ.isLoading

  // 图表配置（ChartContainer 注入 --color-*，随主题翻转）。
  const errChartConfig = {
    err_rate: { label: t('dashboard.tableErrRate'), color: 'var(--primary)' },
  } satisfies ChartConfig
  const trendChartConfig = {
    requests: { label: t('dashboard.tableRequests'), color: 'var(--primary)' },
  } satisfies ChartConfig

  const statusCards: { key: keyof typeof statusCounts; icon: typeof Activity; descKey: string }[] = [
    { key: 'active', icon: Activity, descKey: 'dashboard.statusCards.active' },
    { key: 'unhealthy', icon: AlertTriangle, descKey: 'dashboard.statusCards.unhealthy' },
    { key: '429', icon: Gauge, descKey: 'dashboard.statusCards.429' },
    { key: 'disabled', icon: PowerOff, descKey: 'dashboard.statusCards.disabled' },
  ]

  // 今日汇总卡（USD 口径——API 边界已 /1e5 换算；spec 2026-08-14：call_count
  // 按次调用与 requests 并列——图片生成 = 张数、search = 1）。
  const summaryCards = [
    { key: 'requests', icon: Activity, labelKey: 'dashboard.summaryCards.requests', value: ov?.summary.requests ?? 0 },
    { key: 'callCount', icon: Boxes, labelKey: 'dashboard.summaryCards.callCount', value: ov?.summary.call_count ?? 0 },
    { key: 'cost', icon: Coins, labelKey: 'dashboard.summaryCards.cost', value: `$${(ov?.summary.cost_usd ?? 0).toFixed(4)}` },
    { key: 'tokens', icon: Zap, labelKey: 'dashboard.summaryCards.tokens', value: formatCompact(ov?.summary.total_tokens ?? 0) },
  ] as const

  // TTFT 指标（今日汇总；仅含首 token 流式请求样本——无样本 = 0；avg 查询侧
  // Go 除，分位直方图插值）。
  const ttftMetrics = [
    { key: 'avg', labelKey: 'dashboard.ttft.avg', value: ov?.summary.ttft_avg_ms ?? 0 },
    { key: 'p50', labelKey: 'dashboard.ttft.p50', value: ov?.summary.ttft_p50_ms ?? 0 },
    { key: 'p90', labelKey: 'dashboard.ttft.p90', value: ov?.summary.ttft_p90_ms ?? 0 },
    { key: 'p95', labelKey: 'dashboard.ttft.p95', value: ov?.summary.ttft_p95_ms ?? 0 },
    { key: 'p99', labelKey: 'dashboard.ttft.p99', value: ov?.summary.ttft_p99_ms ?? 0 },
    { key: 'max', labelKey: 'dashboard.ttft.max', value: ov?.summary.ttft_max_ms ?? 0 },
  ] as const

  // 资源计数（服务端 count；模板/分组排除软删）。
  const totalCards = [
    { key: 'templates', labelKey: 'dashboard.totalCards.templates', value: ov?.resources.templates ?? 0, icon: Boxes },
    { key: 'groups', labelKey: 'dashboard.totalCards.groups', value: ov?.resources.groups ?? 0, icon: FolderOpen },
    { key: 'users', labelKey: 'dashboard.totalCards.users', value: ov?.resources.users ?? 0, icon: Users },
  ] as const

  // 首次加载失败（无数据）仍整页错误；轮询失败（有数据——isRefetchError）保留
  // 上一次数据继续渲染 + 顶部警示条（瞬态刷新失败不整页替换，至多一个轮询周期自愈）。
  if (overviewQ.isError && !overviewQ.isRefetchError) {
    return (
      <Alert variant="destructive">
        <AlertTitle>{t('dashboard.loadFailedTitle')}</AlertTitle>
        <AlertDescription>{(overviewQ.error as Error).message}</AlertDescription>
      </Alert>
    )
  }

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">{t('dashboard.title')}</h1>
        <p className="text-sm text-muted-foreground">{t('dashboard.subtitle')}</p>
      </div>

      {/* 轮询失败警示条：保留上一次数据继续渲染（isRefetchError = 已有数据 + 本次刷新失败） */}
      {overviewQ.isRefetchError && (
        <Alert variant="destructive">
          <AlertTitle>{t('dashboard.loadFailedWarning')}</AlertTitle>
          <AlertDescription>{(overviewQ.error as Error).message}</AlertDescription>
        </Alert>
      )}

      {loading ? (
        <div className="grid grid-cols-2 gap-5 xl:grid-cols-4">
          {Array.from({ length: 4 }).map((_, i) => (
            <Skeleton key={i} className="h-28" />
          ))}
        </div>
      ) : (
        <>
          {/* 今日汇总卡（summary：请求/按次调用/费用/token——今日 UTC 日界） */}
          <div className={`${cardGrid} sm:grid-cols-2 xl:grid-cols-4`}>
            {summaryCards.map(({ key, labelKey, value, icon: Icon }, i) => (
              <motion.div key={key} {...fadeUp} transition={{ duration: 0.25, delay: i * 0.06 }}>
                <Card className="@container/card h-full">
                  <CardHeader>
                    <CardDescription className="flex items-center gap-1.5">
                      <Icon className="size-4" /> {t(labelKey)}
                    </CardDescription>
                    <CardTitle className="text-2xl font-semibold tabular-nums @[250px]/card:text-3xl">
                      {value}
                    </CardTitle>
                  </CardHeader>
                </Card>
              </motion.div>
            ))}
          </div>

          {/* TTFT 指标卡（今日汇总；仅含首 token 流式请求样本——无样本 = 0） */}
          <div className={`${cardGrid} sm:grid-cols-3 xl:grid-cols-6`}>
            {ttftMetrics.map(({ key, labelKey, value }, i) => (
              <motion.div key={key} {...fadeUp} transition={{ duration: 0.25, delay: 0.12 + i * 0.05 }}>
                <Card className="h-full">
                  <CardHeader>
                    <CardDescription>{t(labelKey)}</CardDescription>
                    <CardTitle className="text-2xl font-semibold tabular-nums">
                      {fmtTTFT(value)}
                    </CardTitle>
                  </CardHeader>
                </Card>
              </motion.div>
            ))}
          </div>

          {/* 状态计数卡片（dashboard-01 section-cards 结构：描述 + 大数字 + 状态徽章） */}
          <div className={`${cardGrid} sm:grid-cols-2 xl:grid-cols-4`}>
            {statusCards.map(({ key, icon: Icon, descKey }, i) => (
              <motion.div key={key} {...fadeUp} transition={{ duration: 0.25, delay: 0.18 + i * 0.06 }}>
                <Card className="@container/card h-full">
                  <CardHeader>
                    <CardDescription className="flex items-center gap-1.5">
                      <Icon className="size-4" /> {t(descKey)}
                    </CardDescription>
                    <CardTitle className="text-2xl font-semibold tabular-nums @[250px]/card:text-3xl">
                      {statusCounts[key]}
                    </CardTitle>
                    <CardAction>
                      <StatusBadge status={key} />
                    </CardAction>
                  </CardHeader>
                </Card>
              </motion.div>
            ))}
          </div>

          {/* 资源总数卡片（同款结构，无徽章位） */}
          <div className={`${cardGrid} sm:grid-cols-3`}>
            {totalCards.map(({ key, labelKey, value, icon: Icon }, i) => (
              <motion.div key={key} {...fadeUp} transition={{ duration: 0.25, delay: 0.42 + i * 0.06 }}>
                <Card className="@container/card h-full">
                  <CardHeader>
                    <CardDescription className="flex items-center gap-1.5">
                      <Icon className="size-4" /> {t(labelKey)}
                    </CardDescription>
                    <CardTitle className="text-2xl font-semibold tabular-nums @[250px]/card:text-3xl">
                      {value}
                    </CardTitle>
                  </CardHeader>
                </Card>
              </motion.div>
            ))}
          </div>

          <div className="grid grid-cols-1 gap-5 xl:grid-cols-3">
            {/* 趋势柱图（日桶请求量；tooltip 带费用/错误/token） */}
            <motion.div {...fadeUp} transition={{ duration: 0.25, delay: 0.6 }} className="xl:col-span-2">
              <Card className="h-full">
                <CardHeader>
                  <CardTitle>{t('dashboard.trendTitle')}</CardTitle>
                  <CardDescription>{t('dashboard.trendDesc')}</CardDescription>
                </CardHeader>
                <CardContent className="px-2 pt-4 sm:px-6 sm:pt-6">
                  {trend.length === 0 ? (
                    <p className="flex h-[320px] items-center justify-center text-sm text-muted-foreground">
                      {t('dashboard.trendEmpty')}
                    </p>
                  ) : (
                    <ChartContainer config={trendChartConfig} className="aspect-auto h-[320px] w-full">
                      <BarChart accessibilityLayer data={trend} margin={{ left: 0, right: 8 }}>
                        <defs>
                          <linearGradient id="c3api-trendbar-fill" x1="0" y1="0" x2="0" y2="1">
                            <stop offset="0%" stopColor="var(--color-requests)" stopOpacity={0.9} />
                            <stop offset="100%" stopColor="var(--color-requests)" stopOpacity={0.35} />
                          </linearGradient>
                        </defs>
                        <CartesianGrid vertical={false} />
                        <XAxis
                          dataKey="date"
                          tickLine={false}
                          axisLine={false}
                          tickMargin={8}
                          interval={0}
                          tickFormatter={(v: string) => v.slice(5)}
                        />
                        <YAxis width={44} tickLine={false} axisLine={false} />
                        <ChartTooltip
                          content={
                            <ChartTooltipContent
                              indicator="dot"
                              labelFormatter={(label, payload) => {
                                const row = payload?.[0]?.payload as TrendRow | undefined
                                return row?.date ?? String(label ?? '')
                              }}
                              formatter={(value, _name, item) => {
                                const row = item?.payload as TrendRow | undefined
                                return (
                                  <>
                                    <span className="text-muted-foreground">{t('dashboard.tableRequests')}</span>
                                    <span className="font-mono font-medium text-foreground tabular-nums">
                                      {Number(value).toLocaleString()}
                                    </span>
                                    {row && (
                                      <>
                                        <span className="flex w-full justify-between text-muted-foreground">
                                          <span>{t('dashboard.tableCost')}</span>
                                          <span className="font-mono font-medium text-foreground tabular-nums">
                                            ${row.cost_usd.toFixed(4)}
                                          </span>
                                        </span>
                                        <span className="flex w-full justify-between text-muted-foreground">
                                          <span>{t('dashboard.tableErrors')}</span>
                                          <span className="font-mono font-medium text-foreground tabular-nums">
                                            {row.errors}
                                          </span>
                                        </span>
                                        <span className="flex w-full justify-between text-muted-foreground">
                                          <span>{t('dashboard.tableTokens')}</span>
                                          <span className="font-mono font-medium text-foreground tabular-nums">
                                            {row.tokens.toLocaleString()}
                                          </span>
                                        </span>
                                      </>
                                    )}
                                  </>
                                )
                              }}
                            />
                          }
                        />
                        <Bar dataKey="requests" fill="url(#c3api-trendbar-fill)" radius={[4, 4, 0, 0]} maxBarSize={36} />
                      </BarChart>
                    </ChartContainer>
                  )}
                </CardContent>
              </Card>
            </motion.div>

            {/* 实时并发排行（本实例视角；TopN + other 归并） */}
            <motion.div {...fadeUp} transition={{ duration: 0.25, delay: 0.66 }}>
              <Card className="h-full">
                <CardHeader>
                  <CardTitle>{t('dashboard.usersTopTitle')}</CardTitle>
                  <CardDescription>{t('dashboard.usersTopDesc')}</CardDescription>
                </CardHeader>
                <CardContent>
                  {/* 轮询失败警示条（有旧数据）：不替换排行数据，保留上次渲染 */}
                  {usersTopQ.isRefetchError && (
                    <Alert variant="destructive" className="mb-3">
                      <AlertTitle>{t('dashboard.loadFailedWarning')}</AlertTitle>
                      <AlertDescription>{(usersTopQ.error as Error).message}</AlertDescription>
                    </Alert>
                  )}
                  {usersTopQ.isError && !usersTopQ.isRefetchError ? (
                    <Alert variant="destructive">
                      <AlertTitle>{t('dashboard.usersTopLoadFailed')}</AlertTitle>
                      <AlertDescription>{(usersTopQ.error as Error).message}</AlertDescription>
                    </Alert>
                  ) : usersTop.length === 0 ? (
                    <p className="flex h-[280px] items-center justify-center text-sm text-muted-foreground">
                      {t('dashboard.usersTopEmpty')}
                    </p>
                  ) : (
                    <div className="overflow-hidden rounded-lg">
                      <Table>
                        <TableHeader className="bg-muted">
                          <TableRow>
                            <TableHead className="w-10">#</TableHead>
                            <TableHead>{t('dashboard.tableUser')}</TableHead>
                            <TableHead className="text-right">{t('dashboard.tableConcurrency')}</TableHead>
                          </TableRow>
                        </TableHeader>
                        <TableBody>
                          {usersTop.map((u, i) => (
                            <TableRow key={u.user_id}>
                              <TableCell className="text-muted-foreground tabular-nums">{i + 1}</TableCell>
                              <TableCell className="max-w-40 truncate" title={u.email}>
                                {truncate(u.email, 20)}
                              </TableCell>
                              <TableCell className="text-right tabular-nums">{u.concurrency}</TableCell>
                            </TableRow>
                          ))}
                          {otherConc > 0 && (
                            <TableRow>
                              <TableCell className="text-muted-foreground">…</TableCell>
                              <TableCell className="text-muted-foreground">{t('dashboard.otherRow')}</TableCell>
                              <TableCell className="text-right tabular-nums text-muted-foreground">{otherConc}</TableCell>
                            </TableRow>
                          )}
                        </TableBody>
                      </Table>
                    </div>
                  )}
                </CardContent>
              </Card>
            </motion.div>
          </div>

          <div className="grid grid-cols-1 gap-5 xl:grid-cols-3">
            {/* err_rate Top 5 柱图（单序列，颜色走语义 primary，深浅色自适应） */}
            <motion.div {...fadeUp} transition={{ duration: 0.25, delay: 0.72 }} className="xl:col-span-2">
              <Card className="h-full">
                <CardHeader>
                  <CardTitle>{t('dashboard.errTopTitle')}</CardTitle>
                  <CardDescription>{t('dashboard.errTopDesc')}</CardDescription>
                </CardHeader>
                <CardContent className="px-2 pt-4 sm:px-6 sm:pt-6">
                  {errData.length === 0 ? (
                    <p className="flex h-[320px] items-center justify-center text-sm text-muted-foreground">
                      {t('dashboard.errTopEmpty')}
                    </p>
                  ) : (
                    <ChartContainer config={errChartConfig} className="aspect-auto h-[320px] w-full">
                      <BarChart accessibilityLayer data={errData} margin={{ left: 0, right: 8 }}>
                        <defs>
                          <linearGradient id="c3api-errbar-fill" x1="0" y1="0" x2="0" y2="1">
                            <stop offset="0%" stopColor="var(--color-err_rate)" stopOpacity={0.9} />
                            <stop offset="100%" stopColor="var(--color-err_rate)" stopOpacity={0.35} />
                          </linearGradient>
                        </defs>
                        <CartesianGrid vertical={false} />
                        <XAxis
                          dataKey="name"
                          tickLine={false}
                          axisLine={false}
                          tickMargin={8}
                          interval={0}
                          tickFormatter={(v: string) => truncate(v, 8)}
                        />
                        <YAxis
                          width={44}
                          tickLine={false}
                          axisLine={false}
                          tickFormatter={(v: number) => formatPercent(v)}
                        />
                        <ChartTooltip
                          content={
                            <ChartTooltipContent
                              indicator="dot"
                              labelFormatter={(label, payload) => {
                                const row = payload?.[0]?.payload as ErrRow | undefined
                                return row?.name ?? String(label ?? '')
                              }}
                              formatter={(value, _name, item) => {
                                const row = item?.payload as ErrRow | undefined
                                return (
                                  <>
                                    <span className="text-muted-foreground">{t('dashboard.tableErrRate')}</span>
                                    <span className="font-mono font-medium text-foreground tabular-nums">
                                      {formatPercent(Number(value))}
                                    </span>
                                    {row && (
                                      <span className="flex w-full justify-between text-muted-foreground">
                                        <span>{t('dashboard.tableErrCount')}</span>
                                        <span className="font-mono font-medium text-foreground tabular-nums">
                                          {row.err_count}
                                        </span>
                                      </span>
                                    )}
                                  </>
                                )
                              }}
                            />
                          }
                        />
                        <Bar dataKey="err_rate" fill="url(#c3api-errbar-fill)" radius={[4, 4, 0, 0]} maxBarSize={36} />
                      </BarChart>
                    </ChartContainer>
                  )}
                </CardContent>
              </Card>
            </motion.div>

            {/* 并发水位（聚合标量：单值进度条） */}
            <motion.div {...fadeUp} transition={{ duration: 0.25, delay: 0.78 }}>
              <Card className="h-full">
                <CardHeader>
                  <CardTitle>{t('dashboard.waterTitle')}</CardTitle>
                  <CardDescription>{t('dashboard.waterDesc', { cur: totalCur, max: totalMax })}</CardDescription>
                </CardHeader>
                <CardContent className="flex h-[320px] flex-col justify-center">
                  <div className="h-3 w-full overflow-hidden rounded-full bg-foreground/10 dark:bg-white/10 border border-foreground/5">
                    <motion.div
                      className="h-full rounded-full bg-primary shadow-sm"
                      initial={{ width: 0 }}
                      animate={{ width: `${Math.max(water * 100, water > 0 ? 2 : 0)}%` }}
                      transition={{ duration: 0.5 }}
                    />
                  </div>
                  <p className="mt-2 text-xs text-muted-foreground">
                    {t('dashboard.waterValue', { percent: formatPercent(water) })} {water > 0.8 && t('dashboard.waterWarning')}
                  </p>
                </CardContent>
              </Card>
            </motion.div>
          </div>

          {/* 最近错误账号明细（dashboard-01 data-table 容器样式：圆角边框 + muted 表头） */}
          <motion.div {...fadeUp} transition={{ duration: 0.25, delay: 0.84 }}>
            <Card>
              <CardHeader>
                <CardTitle>{t('dashboard.errTopDetailTitle')}</CardTitle>
                <CardDescription>{t('dashboard.errTopDetailDesc')}</CardDescription>
              </CardHeader>
              <CardContent>
                {errData.length === 0 ? (
                  <p className="py-6 text-center text-sm text-muted-foreground">{t('dashboard.errTopEmpty')}</p>
                ) : (
                  <div className="overflow-hidden rounded-lg">
                    <Table>
                      <TableHeader className="bg-muted">
                        <TableRow>
                          <TableHead>{t('dashboard.tableAccount')}</TableHead>
                          <TableHead className="text-right">{t('dashboard.tableErrRate')}</TableHead>
                          <TableHead className="text-right">{t('dashboard.tableErrCount')}</TableHead>
                        </TableRow>
                      </TableHeader>
                      <TableBody>
                        {errData.map(a => (
                          <TableRow key={a.name}>
                            <TableCell className="max-w-40 truncate" title={a.name}>{truncate(a.name, 14)}</TableCell>
                            <TableCell className="text-right">
                              <Badge variant="destructive">{formatPercent(a.err_rate)}</Badge>
                            </TableCell>
                            <TableCell className="text-right tabular-nums">{a.err_count}</TableCell>
                          </TableRow>
                        ))}
                      </TableBody>
                    </Table>
                  </div>
                )}
              </CardContent>
            </Card>
          </motion.div>
        </>
      )}
    </div>
  )
}
