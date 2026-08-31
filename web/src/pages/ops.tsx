// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

import { useQuery } from '@tanstack/react-query'
import { useTranslation } from 'react-i18next'
import { RefreshCw, Cpu } from 'lucide-react'
import { api } from '@/App'
import type { components } from '@/lib/api/schema'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Badge } from '@/components/ui/badge'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { Skeleton } from '@/components/ui/skeleton'
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert'
import { fmtTTFT } from '@/components/fmt'

// stats 为契约自由 schema（unknown）：各 worker 异构观测字段。通用渲染分支：
// 数字（unix_ms 时间戳字段转时间显示）、布尔、字符串，其余 JSON 摘要。
function fmtStatValue(v: unknown, key: string): string {
  if (typeof v === 'number') {
    // last_redis_attempt_ms 是毫秒时间戳（契约命名遗留 _ms 后缀，非时长）
    if ((key.endsWith('unix_ms') || key === 'last_redis_attempt_ms') && v > 0) {
      return new Date(v).toLocaleString()
    }
    return v.toLocaleString()
  }
  if (typeof v === 'boolean') return v ? 'true' : 'false'
  if (typeof v === 'string') return v || '—'
  if (v === null || v === undefined) return '—'
  const s = JSON.stringify(v)
  return s.length > 60 ? `${s.slice(0, 60)}…` : s
}

export default function Ops() {
  const { t } = useTranslation()
  // 10s 轮询（与 dashboard 账号视图同频）；手动刷新 refetch。
  const opsQ = useQuery({
    queryKey: ['ops', 'workers'],
    queryFn: () => api.getOpsWorkers(),
    refetchInterval: 10_000,
  })

  const workers = opsQ.data?.workers ?? []
  const snapshots = opsQ.data?.snapshots ?? []

  return (
    <div className="space-y-6">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h1 className="text-2xl font-semibold tracking-tight">{t('ops.title')}</h1>
          <p className="text-sm text-pretty break-keep text-muted-foreground">{t('ops.subtitle')}</p>
        </div>
        <div className="flex items-center gap-3">
          {opsQ.data?.generated_at && (
            <span className="text-xs text-muted-foreground tabular-nums">
              {t('ops.generatedAt', { time: new Date(opsQ.data.generated_at).toLocaleTimeString() })}
            </span>
          )}
          <Button variant="outline" size="sm" onClick={() => opsQ.refetch()} disabled={opsQ.isFetching}>
            <RefreshCw className={`size-4 ${opsQ.isFetching ? 'animate-spin' : ''}`} />
            {t('ops.refresh')}
          </Button>
        </div>
      </div>

      {/* 轮询失败警示条（有旧数据——isRefetchError）：瞬态刷新失败不整页替换，
          保留上一次数据渲染 + 警示（至多一个轮询周期自愈） */}
      {opsQ.isRefetchError && (
        <Alert variant="destructive">
          <AlertTitle>{t('ops.loadFailedWarning')}</AlertTitle>
          <AlertDescription>{t('ops.loadFailedDesc')}</AlertDescription>
        </Alert>
      )}

      {opsQ.isError && !opsQ.isRefetchError ? (
        <Alert variant="destructive">
          <AlertTitle>{t('ops.loadFailedTitle')}</AlertTitle>
          <AlertDescription>{t('ops.loadFailedDesc')}</AlertDescription>
        </Alert>
      ) : opsQ.isLoading ? (
        <div className="grid grid-cols-1 gap-5 sm:grid-cols-2 xl:grid-cols-3">
          {Array.from({ length: 6 }).map((_, i) => (
            <Skeleton key={i} className="h-40" />
          ))}
        </div>
      ) : (
        <>
          {/* 路由观测四道（compiler/quality-sync/rollup/runtime-health）：新鲜度 +
              事故计数直出 worker stats 字段，前端零重算；缺道 = 未装配，不渲染占位 */}
          <RoutingLanes workers={workers} />

          {/* Workers：每 worker 一卡，stats 通用 key-value 渲染 */}
          <div className="grid grid-cols-1 gap-5 sm:grid-cols-2 xl:grid-cols-3">
            {workers.length === 0 ? (
              <p className="col-span-full py-6 text-center text-sm text-muted-foreground">{t('ops.noWorkers')}</p>
            ) : (
              workers.map(w => (
                <Card key={w.name}>
                  <CardHeader>
                    <CardDescription className="flex items-center gap-1.5">
                      {/* worker 显示名走 i18n（ops.workers.<name>，缺 key 兜底原始标识符） */}
                      <Cpu className="size-4" /> {t(`ops.workers.${w.name}`, { defaultValue: w.name })}
                    </CardDescription>
                  </CardHeader>
                  <CardContent>
                    <dl className="space-y-1.5">
                      {Object.entries(w.stats ?? {}).map(([k, v]) => (
                        <div key={k} className="flex items-center justify-between gap-3">
                          {/* 标签走 i18n（ops.stats.<字段>，缺 key 兜底显示原始字段名）；
                              title 保留原始 key（运维识别用，标签可翻译但 key 不译） */}
                          <dt className="shrink-0 text-xs text-muted-foreground" title={k}>
                            {t(`ops.stats.${k}`, { defaultValue: k })}
                          </dt>
                          <dd className="flex min-w-0 items-center justify-end gap-2" title={fmtStatValue(v, k)}>
                            {typeof v === 'boolean' ? (
                              // 布尔观测位统一徽章：是（绿）/ 否（灰）
                              <Badge variant={v ? 'default' : 'secondary'} className="text-xs">
                                {v ? t('ops.boolYes') : t('ops.boolNo')}
                              </Badge>
                            ) : (
                              <span className="min-w-0 truncate font-mono text-sm tabular-nums">{fmtStatValue(v, k)}</span>
                            )}
                          </dd>
                        </div>
                      ))}
                      {Object.keys(w.stats ?? {}).length === 0 && (
                        <dd className="text-xs text-muted-foreground">—</dd>
                      )}
                    </dl>
                  </CardContent>
                </Card>
              ))
            )}
          </div>

          {/* 快照注册表：玻璃单框表 — Card 仅作标题容器，Table 自带玻璃外框，避免双层边框 */}
          <Card className="bg-transparent border-0 shadow-none backdrop-blur-none p-0 gap-0 pt-4">
            <CardHeader className="pb-4">
              <CardTitle>{t('ops.snapshotsTitle')}</CardTitle>
              <CardDescription>{t('ops.snapshotsDesc')}</CardDescription>
            </CardHeader>
            {snapshots.length === 0 ? (
              <div className="mt-1 rounded-[14px] border border-[rgba(19,45,83,0.26)] bg-[color:var(--glass-card-light)] py-10 text-center text-sm text-muted-foreground shadow-[inset_0_1px_0_rgba(255,255,255,0.5),0_10px_36px_rgba(19,45,83,0.16)] backdrop-blur-[var(--glass-blur)] dark:border-[rgba(148,180,220,0.32)] dark:bg-[color:var(--glass-card-dark)] dark:shadow-[inset_0_1px_0_rgba(255,255,255,0.07),0_10px_36px_rgba(2,6,14,0.5)]">
                {t('ops.noSnapshots')}
              </div>
            ) : (
              <div className="mt-1">
                <Table>
                  <TableHeader>
                    <TableRow>
                      <TableHead>{t('ops.snapshotName')}</TableHead>
                      <TableHead>{t('ops.snapshotScopes')}</TableHead>
                      <TableHead>{t('ops.snapshotLastReload')}</TableHead>
                      <TableHead>{t('ops.snapshotLastError')}</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody className="[&_td]:py-3.5">
                    {snapshots.map(s => (
                      <TableRow key={s.name}>
                        <TableCell className="font-medium">{s.name}</TableCell>
                        <TableCell>
                          {s.scopes && s.scopes.length > 0 ? (
                            <div className="flex flex-wrap gap-1.5">
                              {s.scopes.map(sc => (
                                <span key={sc} className="inline-flex items-center rounded-full border border-black/10 bg-black/[0.04] px-2 py-0.5 font-mono text-xs dark:border-white/10 dark:bg-white/10">{sc}</span>
                              ))}
                            </div>
                          ) : (
                            <span className="text-xs text-muted-foreground">{t('ops.snapshotNoScope')}</span>
                          )}
                        </TableCell>
                        <TableCell className="whitespace-nowrap tabular-nums text-xs text-muted-foreground">
                          {new Date(s.last_reload).toLocaleString()}
                        </TableCell>
                        <TableCell>
                          {s.last_error ? (
                            <span className="inline-flex max-w-64 items-center gap-1.5 truncate text-xs text-destructive" title={s.last_error}>
                              <span className="size-1.5 shrink-0 rounded-full bg-destructive" />
                              <span className="truncate">{s.last_error}</span>
                            </span>
                          ) : (
                            <span className="inline-flex items-center gap-1.5 text-xs font-medium text-emerald-600 dark:text-emerald-400">
                              <span className="size-1.5 rounded-full bg-emerald-500" />
                              {t('ops.snapshotNoError')}
                            </span>
                          )}
                        </TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              </div>
            )}
          </Card>
        </>
      )}
    </div>
  )
}

// 路由观测道面板：从 workers 数组按名取 stats（unknown 契约），只挑选呈现
// 关键字段——数值/时刻全部服务端原值，前端仅做展示格式化（fmtStatValue）。
type WorkerEntry = NonNullable<components['schemas']['WorkersResponse']['workers']>[number]

const laneNum = (s: Record<string, unknown> | undefined, k: string): number | undefined =>
  typeof s?.[k] === 'number' ? (s[k] as number) : undefined
const laneStr = (s: Record<string, unknown> | undefined, k: string): string =>
  typeof s?.[k] === 'string' ? (s[k] as string) : ''

function RoutingLanes({ workers }: { workers: WorkerEntry[] }) {
  const { t } = useTranslation()
  const byName = (n: string) => {
    const w = workers.find(x => x.name === n)
    return (w?.stats ?? undefined) as Record<string, unknown> | undefined
  }
  const compiler = byName('scheduler')
  const quality = byName('quality-sync')
  const rollup = byName('stats-agg')
  const health = byName('runtime-health')
  if (!compiler && !quality && !rollup && !health) return null

  const lanes: { key: string; stats: Record<string, unknown> | undefined; rows: [string, string][]; incidents: string[] }[] = []
  if (compiler) {
    const okMs = laneNum(compiler, 'last_compile_ok_unix_ms') ?? 0
    const errMs = laneNum(compiler, 'last_compile_err_unix_ms') ?? 0
    lanes.push({
      key: 'compiler', stats: compiler,
      rows: [
        [t('ops.routing.generation'), (laneNum(compiler, 'decision_generation') ?? 0).toLocaleString()],
        [t('ops.routing.routes'), (laneNum(compiler, 'decision_routes') ?? 0).toLocaleString()],
        [t('ops.routing.lastCompileOk'), fmtStatValue(okMs, 'last_compile_ok_unix_ms')],
        [t('ops.routing.lastCompileErr'), fmtStatValue(errMs, 'last_compile_err_unix_ms')],
        [t('ops.routing.compilePending'), `${laneNum(compiler, 'compile_pending') ?? 0} / ${laneNum(compiler, 'compile_cap') ?? 0}`],
      ],
      incidents: errMs > 0 && errMs >= okMs ? [t('ops.routing.incidentCompileFailed')] : [],
    })
  }
  if (quality) {
    const dropped = (laneNum(quality, 'dropped_quality') ?? 0) + (laneNum(quality, 'dropped_flow') ?? 0)
    const poison = laneNum(quality, 'poison_dropped') ?? 0
    const redisErr = laneNum(quality, 'redis_errors') ?? 0
    const lastErr = laneStr(quality, 'last_redis_error')
    lanes.push({
      key: 'quality', stats: quality,
      rows: [
        [t('ops.routing.freshness'), fmtTTFT(laneNum(quality, 'freshness_ms') ?? 0)],
        [t('ops.routing.lag'), fmtTTFT(laneNum(quality, 'lag_ms') ?? 0)],
        [t('ops.routing.pending'), `${(laneNum(quality, 'pending_quality') ?? 0).toLocaleString()} + ${(laneNum(quality, 'pending_flow') ?? 0).toLocaleString()}`],
        [t('ops.routing.dropped'), `${dropped.toLocaleString()} / ${poison.toLocaleString()}`],
        [t('ops.routing.redisErrors'), redisErr.toLocaleString()],
      ],
      incidents: [
        ...(dropped > 0 ? [t('ops.routing.incidentDropped')] : []),
        ...(poison > 0 ? [t('ops.routing.incidentPoison')] : []),
        ...(lastErr ? [lastErr] : []),
      ],
    })
  }
  if (rollup) {
    const wm = laneNum(rollup, 'watermark_unix_ms') ?? 0
    lanes.push({
      key: 'rollup', stats: rollup,
      rows: [
        [t('ops.routing.watermark'), fmtStatValue(wm, 'watermark_unix_ms')],
        [t('ops.routing.lastBuckets'), (laneNum(rollup, 'last_buckets') ?? 0).toLocaleString()],
        [t('ops.routing.lastRows'), (laneNum(rollup, 'last_rows') ?? 0).toLocaleString()],
        [t('ops.routing.lastDuration'), fmtTTFT(laneNum(rollup, 'last_duration_ms') ?? 0)],
      ],
      incidents: wm === 0 ? [t('ops.routing.incidentWatermarkInit')] : [],
    })
  }
  if (health) {
    const tickOk = health['last_tick_ok'] === true
    const syncErr = laneNum(health, 'sync_errors') ?? 0
    lanes.push({
      key: 'health', stats: health,
      rows: [
        [t('ops.routing.generation'), (laneNum(health, 'generation') ?? 0).toLocaleString()],
        [t('ops.routing.records'), `${(laneNum(health, 'records') ?? 0).toLocaleString()} (${[laneNum(health, 'open'), laneNum(health, 'retry_after'), laneNum(health, 'probing'), laneNum(health, 'ready')].map(n => n ?? 0).join('/')})`],
        [t('ops.routing.lastSyncOk'), fmtStatValue(laneNum(health, 'last_sync_ok_unix_ms'), 'last_sync_ok_unix_ms')],
        [t('ops.routing.syncErrors'), syncErr.toLocaleString()],
        [t('ops.routing.lastTick'), tickOk ? t('ops.boolYes') : t('ops.boolNo')],
      ],
      incidents: [
        ...(!tickOk ? [t('ops.routing.incidentTickFailed')] : []),
        ...(syncErr > 0 ? [t('ops.routing.incidentSyncErrors')] : []),
      ],
    })
  }

  return (
    <Card>
      <CardHeader>
        <CardTitle>{t('ops.routing.title')}</CardTitle>
        <CardDescription>{t('ops.routing.desc')}</CardDescription>
      </CardHeader>
      <CardContent>
        <div className="grid grid-cols-1 gap-5 sm:grid-cols-2 xl:grid-cols-4">
          {lanes.map(l => (
            <div key={l.key} className="min-w-0">
              <div className="mb-1.5 text-sm font-medium">{t(`ops.routing.lane.${l.key}`)}</div>
              <dl className="space-y-1">
                {l.rows.map(([label, value]) => (
                  <div key={label} className="flex items-center justify-between gap-2">
                    <dt className="shrink-0 text-xs text-muted-foreground">{label}</dt>
                    <dd className="min-w-0 truncate font-mono text-xs tabular-nums" title={value}>{value}</dd>
                  </div>
                ))}
              </dl>
              {l.incidents.length > 0 && (
                <div className="mt-2 flex flex-wrap gap-1.5">
                  {l.incidents.map(msg => (
                    <Badge key={msg} variant="destructive" className="max-w-full truncate text-xs" title={msg}>{msg}</Badge>
                  ))}
                </div>
              )}
            </div>
          ))}
        </div>
      </CardContent>
    </Card>
  )
}
