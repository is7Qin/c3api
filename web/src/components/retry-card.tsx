// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

import type { UseQueryResult } from '@tanstack/react-query'
import { useTranslation } from 'react-i18next'
import type { components } from '@/lib/api/schema'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert'
import { Skeleton } from '@/components/ui/skeleton'
import { formatAverage, formatRate, formatStatValue } from '@/lib/ops-stats'

// RetryCard 重试聚合卡：四个数字 + 一条按原因分解的横向条（复用既有 Card /
// Alert / Skeleton 原语与 bg-muted 横条样式，不引图表库）。
//
// 数值格式化一律走 lib/ops-stats（formatRate/formatAverage/formatStatValue）：
// - null = 服务端「分母为 0」⇒ —（不是 0%）；
// - 计数走千分位；原始 JSON 永不进单元格。
type RetryResponse = components['schemas']['RoutingRetryResponse']

export function RetryCard({ q }: { q: UseQueryResult<RetryResponse> }) {
  const { t } = useTranslation()
  const data = q.data

  // 结局词表是闭集（导流 token：429/5xx/network/4xx/error + 单列的 client_cancel）。
  // 用 switch 的**字面量** key 而不是 `t(`ops.retry.outcome.${k}`)`：动态拼接
  // extractor 看不见，缺文案会直出英文标识符（禁动态键）。
  const outcomeLabel = (outcome: string): string => {
    switch (outcome) {
      case '429': return t('ops.retry.outcome429')
      case '5xx': return t('ops.retry.outcome5xx')
      case 'network': return t('ops.retry.outcomeNetwork')
      case '4xx': return t('ops.retry.outcome4xx')
      case 'error': return t('ops.retry.outcomeError')
      case 'client_cancel': return t('ops.retry.outcomeClientCancel')
      default: return t('ops.retry.outcomeUnknown')
    }
  }

  const metrics = data
    ? [
        { key: 'rate', label: t('ops.retry.rate'), value: formatRate(data.retry_rate) },
        { key: 'avg', label: t('ops.retry.avgAttempts'), value: formatAverage(data.avg_attempts) },
        { key: 'exhausted', label: t('ops.retry.exhaustedRate'), value: formatRate(data.exhausted_rate) },
        { key: 'retrySuccess', label: t('ops.retry.retrySuccessRate'), value: formatRate(data.retry_success_rate) },
      ]
    : []
  // 横条以最大的一列归一（读的是相对占比，不是占全部尝试的比例——后者会把
  // 成功边也算进分母，横条看起来永远很短）。
  const barMax = data ? Math.max(0, ...data.by_reason.map(r => r.chain_count)) : 0

  return (
    <Card>
      <CardHeader>
        <CardTitle>{t('ops.retry.title')}</CardTitle>
        <CardDescription>{t('ops.retry.desc')}</CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        {q.isError && !data ? (
          // 首载失败：没有旧数据可保留，只能给错误态。
          <Alert variant="destructive">
            <AlertTitle>{t('ops.retry.errorTitle')}</AlertTitle>
            <AlertDescription>{t('ops.retry.errorDesc')}</AlertDescription>
          </Alert>
        ) : q.isLoading || !data ? (
          <div className="space-y-4">
            <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
              {Array.from({ length: 4 }).map((_, i) => (
                <Skeleton key={i} className="h-12" />
              ))}
            </div>
            <Skeleton className="h-16" />
          </div>
        ) : data.request_count === 0 ? (
          <p className="py-6 text-center text-sm text-muted-foreground">{t('ops.retry.empty')}</p>
        ) : (
          <>
            {/* 轮询失败但有旧值：保留上一次数字 + 警示（至多一个周期自愈），不整卡替换 */}
            {q.isRefetchError && (
              <Alert variant="destructive">
                <AlertTitle>{t('ops.retry.errorTitle')}</AlertTitle>
                <AlertDescription>{t('ops.retry.errorDesc')}</AlertDescription>
              </Alert>
            )}
            <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
              {metrics.map(m => (
                <div key={m.key} className="min-w-0">
                  <div className="text-xs text-muted-foreground">{m.label}</div>
                  <div className="mt-0.5 truncate font-mono text-2xl font-semibold tabular-nums" title={m.value}>{m.value}</div>
                </div>
              ))}
            </div>

            <div className="space-y-2">
              <div className="text-xs text-muted-foreground">{t('ops.retry.byReason')}</div>
              {data.by_reason.length === 0 ? (
                <p className="text-xs text-muted-foreground">{t('ops.retry.byReasonEmpty')}</p>
              ) : (
                data.by_reason.map(r => (
                  <div key={r.outcome} className="space-y-1">
                    <div className="flex items-center justify-between gap-3 text-xs">
                      <span className="shrink-0 text-muted-foreground">{outcomeLabel(r.outcome)}</span>
                      <span className="truncate font-mono tabular-nums" title={`${r.outcome}: ${r.chain_count}`}>
                        {formatStatValue(r.chain_count, 'chain_count')}
                      </span>
                    </div>
                    <div className="h-1.5 w-full overflow-hidden rounded-full bg-muted">
                      {/* 取消单独一色：它不是失败，横条颜色不能让两者看起来同源 */}
                      <div
                        className={r.outcome === 'client_cancel' ? 'h-full rounded-full bg-muted-foreground/40' : 'h-full rounded-full bg-primary'}
                        style={{ width: `${barMax > 0 ? Math.min(100, Math.max(0, (r.chain_count / barMax) * 100)) : 0}%` }}
                      />
                    </div>
                  </div>
                ))
              )}
            </div>

            <div className="flex flex-wrap items-center gap-x-4 gap-y-1 text-xs text-muted-foreground">
              <span>
                {t('ops.retry.counts', {
                  requests: formatStatValue(data.request_count, 'request_count'),
                  attempts: formatStatValue(data.attempt_total, 'attempt_total'),
                })}
              </span>
              <span>{t('ops.retry.routes', { count: data.routes })}</span>
              <span>{t('ops.retry.queriedAt', { time: new Date(data.queried_at).toLocaleTimeString() })}</span>
            </div>
          </>
        )}
      </CardContent>
    </Card>
  )
}
