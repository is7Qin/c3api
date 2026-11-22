// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

import { useQuery } from '@tanstack/react-query'
import { useTranslation } from 'react-i18next'
import { asPlainKey, type DynamicKey } from '@/lib/i18n'
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
import { RetryCard } from '@/components/retry-card'
import { fetchAllRoutingPlanRoutes } from '@/lib/routing-plan'
import { formatStatValue, statTooltip } from '@/lib/ops-stats'
import { isOpsStatVisible } from '@/lib/supplier-visibility'
import { useFeatures } from '@/lib/use-features'

// 重试卡的观察窗口：固定近 24h（滚动窗口，UTC 分钟对齐由服务端仓储读面完成）。
// 比率指标窗口太短会被单次故障主导；24h 覆盖一个完整工作日，且远在观测保留
// 期之内（默认 7 天、配置地板 2 天），故不会触发服务端的保留期裁剪 400。
const RETRY_WINDOW_MS = 24 * 60 * 60 * 1000

export default function Ops() {
  const { t } = useTranslation()
  // 供应商功能开关（加载中 / 失败按关闭）：只影响供应商专属统计项的展示。
  const { supplier } = useFeatures()
  // 10s 轮询（与 dashboard 账号视图同频）；手动刷新 refetch。
  const opsQ = useQuery({
    queryKey: ['ops', 'workers'],
    queryFn: () => api.getOpsWorkers(),
    refetchInterval: 10_000,
  })
  // 发布计划事故：只取 active incident 行，渲染进编译道既有 incidents 列表，
  // 不另起面板。
  //
  // 取数必须**取全**：`/routing/plan` 的 `routes` 是服务端一页（缺省 20 条、上限
  // 200），而这里要的是整个计划里所有活跃事故——只读第一页会**静默漏报**第 20 条
  // 之后路由上的事故。故走 `fetchAllRoutingPlanRoutes` 按 `total_routes` 取全。
  //
  // 缓存键不与 stats 页共用：那边是 `['routing-plan', search]`（选择器只要 100 条
  // 轻量候选、`candidates_limit=0`），形态不同，共用会互相覆盖。
  const planQ = useQuery({
    queryKey: ['routing-plan'],
    queryFn: () =>
      fetchAllRoutingPlanRoutes<components['schemas']['RoutingPlanRoute']>(p =>
        api.getRoutingPlan({ offset: p.offset, limit: p.limit }),
      ),
    refetchInterval: 10_000,
  })

  // 重试聚合（/routing/retry）：只读聚合面，1 分钟轮询即可（24h 窗口的比率
  // 不会因为 10s 刷新而变得更新鲜）。route 缺省 = 服务端枚举窗口内有流量的
  // 全部 route（上限 200，超限 400 → 卡片显示错误态与要求指定 route 的说明）。
  const retryQ = useQuery({
    queryKey: ['ops', 'routing-retry'],
    queryFn: () => {
      const to = new Date()
      const from = new Date(to.getTime() - RETRY_WINDOW_MS)
      return api.getRoutingRetry({ from: from.toISOString(), to: to.toISOString() })
    },
    refetchInterval: 60_000,
  })

  const workers = opsQ.data?.workers ?? []
  const snapshots = opsQ.data?.snapshots ?? []
  const planIncidents = (planQ.data?.routes ?? [])
    .filter(r => r.incident?.active)
    .map(r => ({
      kind: r.incident.kind,
      group: r.ref.group_id,
      model: r.ref.model,
      degraded: r.incident.degraded,
      comparable: r.incident.comparable,
    }))

  // 砌砖布局的摆放顺序：卡片高度 ≈ 可见统计项数，升序让小卡先填、大卡沉底，
  // 比按名字排再短 71px（2560 实测 2638 → 2567）；同数时按名字稳定排序。
  const workerCards = workers
    .map(w => ({ w, entries: Object.entries(w.stats ?? {}).filter(([k]) => isOpsStatVisible(k, supplier)) }))
    .sort((a, b) => a.entries.length - b.entries.length || a.w.name.localeCompare(b.w.name))

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
          <Button variant="outline" size="sm" onClick={() => { opsQ.refetch(); planQ.refetch(); retryQ.refetch() }} disabled={opsQ.isFetching || retryQ.isFetching}>
            <RefreshCw className={`size-4 ${opsQ.isFetching || retryQ.isFetching ? 'animate-spin' : ''}`} />
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
        <div className="grid grid-cols-1 gap-5 sm:grid-cols-2 xl:grid-cols-3 items-start">
          {Array.from({ length: 6 }).map((_, i) => (
            <Skeleton key={i} className="h-40" />
          ))}
        </div>
      ) : (
        <>
          {/* 路由观测四道（compiler/quality-sync/rollup/runtime-health）：新鲜度 +
              事故计数直出 worker stats 字段，前端零重算；缺道 = 未装配，不渲染占位 */}
          <RoutingLanes workers={workers} planIncidents={planIncidents} />

          {/* 重试聚合卡（重试率 / 平均尝试数 / 耗尽率 / 重试后成功率 + 按原因
              分解的横向条）。比值 null（服务端「分母为 0」）显示 —，绝不显示 0%。 */}
          <RetryCard q={retryQ} />

          {/* Workers：每 worker 一卡，stats 通用 key-value 渲染。
              布局是**多列砌砖**（`columns-*` + `break-inside-avoid`）而不是 grid：卡片高度
              从 93px 到 1159px（stats 行数差异极大），grid 每行被该行最高的卡顶死，短卡
              下面留大片空洞（实测 2560 下三列空洞 2012 / 400 / 2122px）。砌砖把页面从
              3903px 压到 2567px（−34%），只剩列尾余量。
              摆放顺序见上面的 workerCards：按可见统计项数升序，小卡在前、大卡沉底。 */}
          {workers.length === 0 ? (
            <p className="py-6 text-center text-sm text-muted-foreground">{t('ops.noWorkers')}</p>
          ) : (
            <div className="columns-1 gap-5 sm:columns-2 xl:columns-3 [&>*]:mb-5 [&>*]:break-inside-avoid">
              {workerCards.map(({ w, entries }) => (
                <Card key={w.name}>
                  <CardHeader>
                    <CardDescription className="flex items-center gap-1.5">
                      {/* worker 显示名走 i18n（ops.workers.<name>，缺 key 兜底原始标识符） */}
                      <Cpu className="size-4" /> {t(`ops.workers.${w.name}`, { defaultValue: w.name })}
                    </CardDescription>
                  </CardHeader>
                  <CardContent>
                    <dl className="space-y-1.5">
                      {entries.map(([k, v]) => (
                        <div key={k} className="flex items-center justify-between gap-3">
                          {/* 标签走 i18n（ops.stats.<字段>，缺 key 兜底显示原始字段名）；
                              title 保留原始 key（运维识别用，标签可翻译但 key 不译） */}
                          <dt className="shrink-0 text-xs text-muted-foreground" title={k}>
                            {t(`ops.stats.${k}`, { defaultValue: k })}
                          </dt>
                          {/* title 是原始值（对象/数组为完整 JSON），单元格永不显示 JSON */}
                          <dd className="flex min-w-0 items-center justify-end gap-2" title={statTooltip(v, k)}>
                            {typeof v === 'boolean' ? (
                              // 布尔观测位统一徽章：是（绿）/ 否（灰）
                              <Badge variant={v ? 'default' : 'secondary'} className="text-xs">
                                {v ? t('ops.boolYes') : t('ops.boolNo')}
                              </Badge>
                            ) : (
                              <span className="min-w-0 truncate font-mono text-sm tabular-nums">
                                {formatStatValue(v, k, { field: kk => t(`ops.statField.${kk}`, { defaultValue: kk }), items: n => t('ops.statItems', { count: n }) })}
                              </span>
                            )}
                          </dd>
                        </div>
                      ))}
                      {entries.length === 0 && (
                        <dd className="text-xs text-muted-foreground">—</dd>
                      )}
                    </dl>
                  </CardContent>
                </Card>
              ))}
            </div>
          )}

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
                      <TableHead>{t('ops.snapshotScopeColumn')}</TableHead>
                      <TableHead>{t('ops.snapshotLastReload')}</TableHead>
                      <TableHead>{t('ops.snapshotLastError')}</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody className="[&_td]:py-3.5">
                    {snapshots.map(s => (
                      <TableRow key={s.name}>
                        <TableCell className="font-medium">
                          {t(`ops.snapshotNames.${s.name}` as DynamicKey, { defaultValue: s.name })}
                        </TableCell>
                        <TableCell>
                          {s.scopes && s.scopes.length > 0 ? (
                            <div className="flex flex-wrap gap-1.5">
                              {s.scopes.map(sc => (
                                <span key={sc} className="inline-flex items-center rounded-full border border-black/10 bg-black/[0.04] px-2 py-0.5 font-mono text-xs dark:border-white/10 dark:bg-white/10">
                                  {t(`ops.snapshotScopes.${sc}` as DynamicKey, { defaultValue: sc })}
                                </span>
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
// 关键字段——数值/时刻全部服务端原值，前端仅做展示格式化（formatStatValue）。
type WorkerEntry = NonNullable<components['schemas']['WorkersResponse']['workers']>[number]

const laneNum = (s: Record<string, unknown> | undefined, k: string): number | undefined =>
  typeof s?.[k] === 'number' ? (s[k] as number) : undefined
const laneStr = (s: Record<string, unknown> | undefined, k: string): string =>
  typeof s?.[k] === 'string' ? (s[k] as string) : ''

type PlanIncidentBadge = { kind: string; group: number; model: string; degraded: number; comparable: number }

function RoutingLanes({ workers, planIncidents }: { workers: WorkerEntry[]; planIncidents: PlanIncidentBadge[] }) {
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
    const evalMs = laneNum(compiler, 'last_incident_eval_unix_ms') ?? 0
    lanes.push({
      key: 'compiler', stats: compiler,
      rows: [
        [t('ops.routing.generation'), (laneNum(compiler, 'decision_generation') ?? 0).toLocaleString()],
        [t('ops.routing.routes'), (laneNum(compiler, 'decision_routes') ?? 0).toLocaleString()],
        [t('ops.routing.lastCompileOk'), formatStatValue(okMs, 'last_compile_ok_unix_ms')],
        [t('ops.routing.lastCompileErr'), formatStatValue(errMs, 'last_compile_err_unix_ms')],
        [t('ops.routing.compilePending'), `${laneNum(compiler, 'compile_pending') ?? 0} / ${laneNum(compiler, 'compile_cap') ?? 0}`],
        [t('ops.routing.activeIncidents'), (laneNum(compiler, 'active_incidents') ?? 0).toLocaleString()],
        [t('ops.routing.lastIncidentEval'), formatStatValue(evalMs, 'last_incident_eval_unix_ms')],
      ],
      incidents: [
        ...(errMs > 0 && errMs >= okMs ? [t('ops.routing.incidentCompileFailed')] : []),
        ...planIncidents.map(p => t('ops.routing.planIncident', {
          kind: p.kind, group: p.group, model: p.model, degraded: p.degraded, comparable: p.comparable,
        })),
      ],
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
        [t('ops.routing.watermark'), formatStatValue(wm, 'watermark_unix_ms')],
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
        [t('ops.routing.lastSyncOk'), formatStatValue(laneNum(health, 'last_sync_ok_unix_ms'), 'last_sync_ok_unix_ms')],
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
        <div className="grid grid-cols-1 gap-5 sm:grid-cols-2 xl:grid-cols-4 items-start">
          {lanes.map(l => (
            <div key={l.key} className="min-w-0">
              <div className="mb-1.5 text-sm font-medium">{t(asPlainKey(`ops.routing.lane.${l.key}`))}</div>
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
