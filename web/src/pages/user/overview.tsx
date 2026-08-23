// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 用户端总览（/user 默认落地页）：me() 余额卡（Balance/MaxConcurrency/Status/注册时间）
// + 近况（可用 keys 数 + 最近 7 天用量摘要）。卡片与动画延续管理端 dashboard 模式。
// 单位语义：User.Balance 为 USD 浮点直显（$ + 2 位小数）；MaxConcurrency 0 = 不限。
import { useMemo } from 'react'
import { useQuery } from '@tanstack/react-query'
import { motion } from 'framer-motion'
import { BarChart3, CalendarDays, KeyRound, Wallet, Zap } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { userApi } from '@/lib/api/client'
import { Card, CardAction, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { StatusBadge } from '@/components/status-badge'
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert'
import { Skeleton } from '@/components/ui/skeleton'
import { formatDateTime, formatUSD } from '@/components/fmt'

const fadeUp = {
  initial: { opacity: 0, y: 12 },
  animate: { opacity: 1, y: 0 },
}

// 统计卡 grid（与 dashboard 同款）：浅色卡片顶部 primary 微渐变 + 细阴影，深色回退纯 card。
const cardGrid = 'grid grid-cols-1 gap-5 *:data-[slot=card]:bg-linear-to-t *:data-[slot=card]:from-primary/5 *:data-[slot=card]:to-card *:data-[slot=card]:shadow-xs dark:*:data-[slot=card]:bg-card'

export default function UserOverview() {
  const { t } = useTranslation()

  // 近 7 天窗口（挂载时固定，避免 queryKey 每次渲染变化导致无限 refetch）
  const [from, to] = useMemo(() => {
    const end = new Date()
    const start = new Date(end.getTime() - 7 * 86_400_000)
    return [start.toISOString(), end.toISOString()]
  }, [])

  const meQ = useQuery({ queryKey: ['user', 'me'], queryFn: () => userApi.me() })
  // limit 1 仅取 total（KeyListResponse.total），不用拉全量
  const keysQ = useQuery({ queryKey: ['user', 'keys'], queryFn: () => userApi.listUserKeys({ limit: 1 }) })
  const statsQ = useQuery({
    queryKey: ['user', 'stats', { from, to, granularity: 'day' }],
    queryFn: () => userApi.getMyStats({ from, to, granularity: 'day' }),
  })

  // 最近 7 天汇总：请求数 / 总 token / 成本（/user/stats 的 Cost 已 USD → formatUSD）
  const buckets = statsQ.data ?? []
  const totalReq = buckets.reduce((s, b) => s + (b.RequestCount ?? 0), 0)
  const totalTokens = buckets.reduce((s, b) => s + (b.TotalTokens ?? 0), 0)
  const totalCost = buckets.reduce((s, b) => s + (b.Cost ?? 0), 0)

  if (meQ.isError) {
    return (
      <Alert variant="destructive">
        <AlertTitle>{t('user.common.error')}</AlertTitle>
        <AlertDescription>{(meQ.error as Error).message}</AlertDescription>
      </Alert>
    )
  }

  const u = meQ.data
  const loading = meQ.isLoading || keysQ.isLoading || statsQ.isLoading

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">{t('user.overview.title')}</h1>
        <p className="text-sm text-muted-foreground">{t('user.overview.subtitle')}</p>
      </div>

      {loading ? (
        <div className="grid grid-cols-1 gap-5 sm:grid-cols-2 xl:grid-cols-3">
          {Array.from({ length: 3 }).map((_, i) => <Skeleton key={i} className="h-28" />)}
        </div>
      ) : (
        <div className={`${cardGrid} sm:grid-cols-2 xl:grid-cols-3`}>
          {/* 余额卡：余额大数字 + 状态徽章（top-right）+ 并发上限/注册时间 */}
          <motion.div {...fadeUp} transition={{ duration: 0.25 }}>
            <Card className="@container/card h-full">
              <CardHeader>
                <CardDescription className="flex items-center gap-1.5">
                  <Wallet className="size-4" /> {t('user.overview.balanceTitle')}
                </CardDescription>
                <CardTitle className="text-2xl font-semibold tabular-nums @[250px]/card:text-3xl">
                  {u?.Balance == null ? '—' : `$${u.Balance.toFixed(2)}`}
                </CardTitle>
                <CardAction>
                  <StatusBadge status={u?.Status} />
                </CardAction>
              </CardHeader>
              <CardContent className="space-y-1.5 text-sm">
                <div className="flex items-center justify-between gap-3">
                  <span className="flex items-center gap-1.5 text-muted-foreground">
                    <Zap className="size-4" /> {t('user.overview.maxConcurrency')}
                  </span>
                  <span className="tabular-nums">
                    {u?.MaxConcurrency == null ? '—' : u.MaxConcurrency === 0 ? t('user.overview.unlimited') : u.MaxConcurrency}
                  </span>
                </div>
                <div className="flex items-center justify-between gap-3">
                  <span className="flex items-center gap-1.5 text-muted-foreground">
                    <CalendarDays className="size-4" /> {t('user.overview.createdAt')}
                  </span>
                  <span className="text-xs">{u ? formatDateTime(u.CreatedAt) : '—'}</span>
                </div>
              </CardContent>
            </Card>
          </motion.div>

          {/* keys 数卡（listUserKeys total） */}
          <motion.div {...fadeUp} transition={{ duration: 0.25, delay: 0.06 }}>
            <Card className="@container/card h-full">
              <CardHeader>
                <CardDescription className="flex items-center gap-1.5">
                  <KeyRound className="size-4" /> {t('user.overview.keysTitle')}
                </CardDescription>
                <CardTitle className="text-2xl font-semibold tabular-nums @[250px]/card:text-3xl">
                  {keysQ.isError ? '—' : (keysQ.data?.total ?? 0)}
                </CardTitle>
              </CardHeader>
              <CardContent>
                <p className="text-sm text-muted-foreground">
                  {keysQ.isError ? (keysQ.error as Error).message : t('user.overview.keysDesc')}
                </p>
              </CardContent>
            </Card>
          </motion.div>

          {/* 最近 7 天用量摘要（getUserStats 汇总） */}
          <motion.div {...fadeUp} transition={{ duration: 0.25, delay: 0.12 }}>
            <Card className="@container/card h-full">
              <CardHeader>
                <CardDescription className="flex items-center gap-1.5">
                  <BarChart3 className="size-4" /> {t('user.overview.recentTitle')}
                </CardDescription>
              </CardHeader>
              <CardContent>
                {statsQ.isError ? (
                  <p className="text-sm text-destructive">{(statsQ.error as Error).message}</p>
                ) : totalReq === 0 ? (
                  <p className="text-sm text-muted-foreground">{t('user.overview.recentEmpty')}</p>
                ) : (
                  <div className="grid grid-cols-3 gap-3 text-center">
                    <div>
                      <div className="text-lg font-semibold tabular-nums">{totalReq.toLocaleString()}</div>
                      <div className="text-xs text-muted-foreground">{t('user.overview.requests')}</div>
                    </div>
                    <div>
                      <div className="text-lg font-semibold tabular-nums">{totalTokens.toLocaleString()}</div>
                      <div className="text-xs text-muted-foreground">{t('user.overview.tokens')}</div>
                    </div>
                    <div>
                      <div className="text-lg font-semibold tabular-nums">{formatUSD(totalCost)}</div>
                      <div className="text-xs text-muted-foreground">{t('user.overview.cost')}</div>
                    </div>
                  </div>
                )}
              </CardContent>
            </Card>
          </motion.div>
        </div>
      )}
    </div>
  )
}
