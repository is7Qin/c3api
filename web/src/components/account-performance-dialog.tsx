// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

import { useMemo } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Activity, Clock3, Coins, Gauge, Timer, Zap } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { api } from '@/App'
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Skeleton } from '@/components/ui/skeleton'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import type { components } from '@/lib/api/schema'

type AccountView = components['schemas']['AccountView']
type UsageLog = components['schemas']['UsageLog']
type StatBucket = components['schemas']['StatBucket']

function percentile(values: number[], ratio: number): number {
  if (values.length === 0) return 0
  const sorted = [...values].sort((a, b) => a - b)
  return sorted[Math.max(0, Math.ceil(sorted.length * ratio) - 1)]
}

function formatDuration(ms: number): string {
  if (!Number.isFinite(ms) || ms <= 0) return '-'
  return ms >= 1000 ? `${(ms / 1000).toFixed(ms >= 10_000 ? 1 : 2)}s` : `${Math.round(ms)}ms`
}

function formatNumber(value: number): string {
  return new Intl.NumberFormat(undefined, {
    notation: value >= 1_000_000 ? 'compact' : 'standard',
    maximumFractionDigits: 1,
  }).format(value)
}

function summarize(stats: StatBucket[], logs: UsageLog[]) {
  let requests = 0
  let errors = 0
  let tokens = 0
  let cost = 0
  let ttftWeighted = 0
  let ttftCount = 0
  const models = new Map<string, { requests: number; tokens: number; cost: number }>()

  for (const row of stats) {
    requests += row.RequestCount ?? 0
    errors += row.ErrorCount ?? 0
    tokens += row.TotalTokens ?? 0
    cost += row.Cost ?? 0
    ttftWeighted += (row.TTFTAvgMS ?? 0) * (row.TTFTCount ?? 0)
    ttftCount += row.TTFTCount ?? 0

    const model = row.Model || '-'
    const current = models.get(model) ?? { requests: 0, tokens: 0, cost: 0 }
    current.requests += row.RequestCount ?? 0
    current.tokens += row.TotalTokens ?? 0
    current.cost += row.Cost ?? 0
    models.set(model, current)
  }

  const latency = logs.map(row => row.LatencyMS ?? 0).filter(value => value > 0)
  const ttft = logs.map(row => row.TTFTMS ?? 0).filter(value => value > 0)
  return {
    requests,
    errors,
    errorRate: requests > 0 ? errors / requests : 0,
    tokens,
    cost,
    ttftAverage: ttftCount > 0 ? ttftWeighted / ttftCount : 0,
    latencyP50: percentile(latency, 0.5),
    latencyP95: percentile(latency, 0.95),
    ttftP50: percentile(ttft, 0.5),
    ttftP95: percentile(ttft, 0.95),
    sampleCount: logs.length,
    models: [...models.entries()]
      .map(([model, values]) => ({ model, ...values }))
      .sort((a, b) => b.requests - a.requests)
      .slice(0, 5),
  }
}

export function AccountPerformanceDialog({ target, onOpenChange }: {
  target: AccountView | null
  onOpenChange: (open: boolean) => void
}) {
  const { t } = useTranslation()
  const window = useMemo(() => {
    if (!target?.ID) return { from: '', to: '' }
    const to = new Date()
    const from = new Date(to.getTime() - 24 * 60 * 60 * 1000)
    return { from: from.toISOString(), to: to.toISOString() }
  }, [target?.ID])
  const query = useQuery({
    queryKey: ['account-performance', target?.ID, window.from, window.to],
    queryFn: async () => {
      const [stats, logs] = await Promise.all([
        api.getStats({ from: window.from, to: window.to, granularity: 'hour', account_id: target!.ID! }),
        api.getUsageLogs({ from: window.from, to: window.to, limit: 200, account_id: target!.ID! }),
      ])
      return { stats, logs: logs.rows }
    },
    enabled: !!target?.ID,
    staleTime: 30_000,
  })
  const summary = useMemo(() => summarize(query.data?.stats ?? [], query.data?.logs ?? []), [query.data])
  const metrics = [
    { key: 'requests', icon: Activity, label: t('accounts.performance.requests'), value: formatNumber(summary.requests) },
    { key: 'tokens', icon: Zap, label: t('accounts.performance.tokens'), value: formatNumber(summary.tokens) },
    { key: 'errors', icon: Gauge, label: t('accounts.performance.errorRate'), value: `${(summary.errorRate * 100).toFixed(1)}%` },
    { key: 'cost', icon: Coins, label: t('accounts.performance.cost'), value: `$${summary.cost.toFixed(4)}` },
    { key: 'latencyP50', icon: Clock3, label: t('accounts.performance.latencyP50'), value: formatDuration(summary.latencyP50) },
    { key: 'latencyP95', icon: Clock3, label: t('accounts.performance.latencyP95'), value: formatDuration(summary.latencyP95) },
    { key: 'ttftAverage', icon: Timer, label: t('accounts.performance.ttftAverage'), value: formatDuration(summary.ttftAverage) },
    { key: 'ttftP95', icon: Timer, label: t('accounts.performance.ttftP95'), value: formatDuration(summary.ttftP95) },
  ]

  return (
    <Dialog open={!!target} onOpenChange={onOpenChange}>
      <DialogContent className="max-h-[90vh] overflow-y-auto sm:max-w-3xl">
        <DialogHeader>
          <DialogTitle>{t('accounts.performance.title', { name: target?.Name ?? `#${target?.ID ?? ''}` })}</DialogTitle>
          <DialogDescription>{t('accounts.performance.desc')}</DialogDescription>
        </DialogHeader>

        {query.isLoading ? (
          <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
            {Array.from({ length: 8 }).map((_, index) => <Skeleton key={index} className="h-20" />)}
          </div>
        ) : query.isError ? (
          <p className="text-sm text-destructive">{t('common.loadFailed', { message: (query.error as Error).message })}</p>
        ) : (
          <>
            <div className="grid grid-cols-2 overflow-hidden rounded-lg border sm:grid-cols-4">
              {metrics.map(({ key, icon: Icon, label, value }) => (
                <div key={key} className="min-w-0 border-r border-b p-3 last:border-r-0 sm:[&:nth-child(4n)]:border-r-0 [&:nth-last-child(-n+2)]:border-b-0 sm:[&:nth-last-child(-n+4)]:border-b-0">
                  <div className="mb-2 flex items-center gap-1.5 text-xs text-muted-foreground"><Icon className="size-3.5" /> {label}</div>
                  <p className="truncate text-lg font-semibold tabular-nums" title={value}>{value}</p>
                </div>
              ))}
            </div>
            <p className="text-xs text-muted-foreground">{t('accounts.performance.sample', { count: summary.sampleCount })}</p>

            <div className="space-y-2 border-t pt-4">
              <h3 className="text-sm font-medium">{t('accounts.performance.models')}</h3>
              {summary.models.length === 0 ? (
                <p className="py-6 text-center text-sm text-muted-foreground">{t('accounts.performance.empty')}</p>
              ) : (
                <div className="overflow-hidden rounded-lg border">
                  <Table>
                    <TableHeader>
                      <TableRow>
                        <TableHead>{t('accounts.performance.model')}</TableHead>
                        <TableHead className="text-right">{t('accounts.performance.requests')}</TableHead>
                        <TableHead className="text-right">{t('accounts.performance.tokens')}</TableHead>
                        <TableHead className="text-right">{t('accounts.performance.cost')}</TableHead>
                      </TableRow>
                    </TableHeader>
                    <TableBody>
                      {summary.models.map(model => (
                        <TableRow key={model.model}>
                          <TableCell className="max-w-56 truncate" title={model.model}>{model.model}</TableCell>
                          <TableCell className="text-right tabular-nums">{formatNumber(model.requests)}</TableCell>
                          <TableCell className="text-right tabular-nums">{formatNumber(model.tokens)}</TableCell>
                          <TableCell className="text-right tabular-nums">${model.cost.toFixed(4)}</TableCell>
                        </TableRow>
                      ))}
                    </TableBody>
                  </Table>
                </div>
              )}
            </div>
          </>
        )}
      </DialogContent>
    </Dialog>
  )
}
