// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

import type { UseQueryResult } from '@tanstack/react-query'
import { useTranslation } from 'react-i18next'
import { Badge } from '@/components/ui/badge'
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { ScrollArea } from '@/components/ui/scroll-area'
import { Skeleton } from '@/components/ui/skeleton'
import { CommitStateBadge, ErrorTypeBadge, FORMAT_LABELS, fmtDuration } from '@/components/log-display'
import type { ErrorTypeKey } from '@/components/log-display'
import { formatDateTime } from '@/components/fmt'
import { cn } from '@/lib/utils'
import type { components } from '@/lib/api/schema'

// 请求链对话框（spec 2026-11-18 retry-observability §4/§4bis）。
//
// 入口只有日志页的 request_id 单元格（ops 页不设查询卡）。UI 纪律：复用既有
// dialog / scroll-area / badge 基元，不引图表库、不新增基元。
//
// 取数在**页面**（logs.tsx）：它已经有 api 与 queryKey 习惯，对话框只负责渲染。
// 这样组件本身是纯渲染（SSR 测试直接喂假 q，照 retry-card 的做法），也避免组件
// 反向 import App（App 在模块求值时就建 browser router，SSR 下没有 document）。
//
// 数据形状是**三源合并的既成事实**：链上每一跳带 source（attempt/err/usage），
// 本组件只负责如实展示——缺列显示 —，绝不从别的跳回填（ordinal 缺失就是缺失）。
type AttemptChainResponse = components['schemas']['AttemptChainResponse']
type AttemptChainHop = components['schemas']['AttemptChainHop']
type AttemptChainSource = components['schemas']['AttemptChainSource']

// CHAIN_WINDOW_MS 查询窗口 = 该行 created_at ±24h。**不能**用「现在」：日志页翻到的
// 历史行若用 now 建窗，链恒为空（服务端缺省是 now ∓ 24h，且保留期只有 7 天）。
// 也**不能**收窄到 ±1h：created_at 是请求**结束**（或本轮起点）的时刻，而一条请求
// 可以跨小时——长流式 + 多次换号重试，attempt 行可能落在 created_at 之前数小时。
// ±1d 不贵：err_logs 腿在 ±1d 窗口实测 3.26ms（当日 5 万行）。
const CHAIN_WINDOW_MS = 24 * 60 * 60 * 1000

// AttemptChainTarget 打开对话框的目标：request_id + 由行 created_at 推出的窗口。
// 窗口在**打开时**算一次并随身携带（页面用它做 queryKey，对话框用它回显），
// 两处不会各算一遍而漂移。
export interface AttemptChainTarget {
  requestID: string
  /** 日志行的 created_at（RFC3339）；窗口以它为中心。 */
  createdAt: string
  from: string
  to: string
}

// attemptChainTarget 行 created_at → 对话框目标（窗口 = created_at ±24h，半开）。
// 非法日期退化为空窗口（服务端按缺省 ±24h 处理，不会 400）。
export function attemptChainTarget(requestID: string, createdAt: string): AttemptChainTarget {
  const at = new Date(createdAt)
  if (Number.isNaN(at.getTime())) return { requestID, createdAt, from: '', to: '' }
  return {
    requestID,
    createdAt,
    from: new Date(at.getTime() - CHAIN_WINDOW_MS).toISOString(),
    to: new Date(at.getTime() + CHAIN_WINDOW_MS).toISOString(),
  }
}

const SOURCE_META: Record<string, string> = {
  attempt: 'bg-amber-500/10 text-amber-600 dark:bg-amber-400/10 dark:text-amber-400',
  err: 'bg-rose-500/10 text-rose-600 dark:bg-rose-400/10 dark:text-rose-400',
  usage: 'bg-emerald-500/10 text-emerald-600 dark:bg-emerald-400/10 dark:text-emerald-400',
}

// AttemptChainDialog 受控对话框：target 非空即打开；q 由页面注入（含加载/错误态）。
export function AttemptChainDialog({ target, q, onOpenChange }: {
  target: AttemptChainTarget | null
  q: UseQueryResult<AttemptChainResponse>
  onOpenChange: (open: boolean) => void
}) {
  const { t } = useTranslation()
  return (
    <Dialog open={!!target} onOpenChange={onOpenChange}>
      {target && (
        <DialogContent className="sm:max-w-3xl">
          {/* 标题/说明留在 Root 内：base-ui 的 Title/Description 依赖 DialogRootContext
              （aria-labelledby 接线也靠它），body 因此只渲染链本体。 */}
          <DialogHeader>
            <DialogTitle>{t('logs.chain.title')}</DialogTitle>
            <DialogDescription className="space-y-1">
              <span className="block break-all font-mono text-xs">{target.requestID}</span>
              <span className="block text-xs">{t('logs.chain.coverage')}</span>
              <span className="block text-xs">{t('logs.chain.window', { from: target.from, to: target.to })}</span>
            </DialogDescription>
          </DialogHeader>
          <AttemptChainBody q={q} />
        </DialogContent>
      )}
    </Dialog>
  )
}

// AttemptChainBody 链本体（q 由调用方注入：SSR 测试直接喂假数据；不含 Dialog 容器，
// 故在 Root 上下文之外也能渲染）。
export function AttemptChainBody({ q }: { q: UseQueryResult<AttemptChainResponse> }) {
  const { t } = useTranslation()
  const data = q.data
  return (
    <>
      {q.isError ? (
        <p className="text-sm text-destructive">{t('logs.chain.errorDesc', { message: (q.error as Error)?.message ?? '' })}</p>
      ) : q.isLoading || !data ? (
        <div className="space-y-2">
          {Array.from({ length: 3 }).map((_, i) => <Skeleton key={i} className="h-12" />)}
        </div>
      ) : data.hops.length === 0 ? (
        <div className="space-y-1 py-6 text-center">
          <p className="text-sm font-medium">{t('logs.chain.emptyTitle')}</p>
          <p className="text-xs text-muted-foreground">{t('logs.chain.emptyDesc')}</p>
        </div>
      ) : (
        <ScrollArea className="max-h-[55vh] pr-2" showHorizontal>
          <div className="space-y-2">
            {data.hops.map((hop, i) => <HopRow key={`${hop.source}-${hop.ordinal ?? 'nil'}-${i}`} hop={hop} />)}
          </div>
        </ScrollArea>
      )}

      <div className="flex flex-wrap items-center gap-x-4 gap-y-1 text-xs text-muted-foreground">
        {data?.truncated && <span className="text-amber-600 dark:text-amber-400">{t('logs.chain.truncated')}</span>}
        <span>{t('logs.chain.samplingHint')}</span>
        {data?.sampling && data.sampling.dropped_total > 0 && (
          <span className="text-amber-600 dark:text-amber-400">
            {t('logs.chain.sampling', { count: data.sampling.dropped_total })}
          </span>
        )}
      </div>
    </>
  )
}

// HopRow 一跳一行：ordinal（nil ⇒ — + 无序号徽标）/ 来源 / 账号 / lane / 格式 /
// 状态码 / 结局 / 错误类型 / 交付状态 / 耗时；错误文本单独一行（truncate + title）。
function HopRow({ hop }: { hop: AttemptChainHop }) {
  const { t } = useTranslation()
  // 三源标签用 switch 的**字面量** key（动态拼键 extractor 看不见，缺文案会直出
  // 英文标识符——与 retry-card 的 outcomeLabel 同款）。
  const sourceText = (source: AttemptChainSource): string => {
    switch (source) {
      case 'attempt': return t('logs.chain.sourceAttempt')
      case 'err': return t('logs.chain.sourceErr')
      case 'usage': return t('logs.chain.sourceUsage')
      default: return t('logs.chain.sourceUnknown')
    }
  }
  return (
    <div className="rounded-md border px-2 py-1.5 text-xs">
      <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
        <span className="font-mono font-semibold tabular-nums">{hop.ordinal ?? '—'}</span>
        {hop.ordinal_unknown && (
          <Badge variant="outline" className="text-[10px]">{t('logs.chain.ordinalUnknown')}</Badge>
        )}
        <Badge className={cn('text-[10px]', SOURCE_META[hop.source] ?? 'bg-muted text-muted-foreground')}>
          {sourceText(hop.source)}
        </Badge>
        <span className="text-muted-foreground">
          {t('logs.chain.account')} {hop.account_id > 0 ? `#${hop.account_id}` : '—'}
        </span>
        <span className="text-muted-foreground">{t('logs.chain.lane')} {hop.lane || '—'}</span>
        <span className="text-muted-foreground">
          {t('logs.chain.format')} {hop.format ? (FORMAT_LABELS[hop.format] ?? hop.format) : '—'}
        </span>
        <span className="text-muted-foreground tabular-nums">{t('logs.chain.status')} {hop.status_code || '—'}</span>
        <span className="text-muted-foreground">{t('logs.chain.outcome')} {hop.outcome || '—'}</span>
        <span className="inline-flex items-center gap-1 text-muted-foreground">
          {t('logs.chain.errorType')}
          <ErrorTypeBadge type={hop.error_type ? (hop.error_type as ErrorTypeKey) : undefined} />
        </span>
        <span className="inline-flex items-center gap-1 text-muted-foreground">
          {t('logs.chain.commitState')} <CommitStateBadge state={hop.commit_state} />
        </span>
        <span className="text-muted-foreground tabular-nums">{t('logs.chain.latency')} {fmtDuration(hop.latency_ms ?? 0)}</span>
        <span className="ml-auto text-muted-foreground">{formatDateTime(hop.created_at)}</span>
      </div>
      {hop.error_message && (
        <p className="mt-1 truncate text-muted-foreground" title={hop.error_message}>{hop.error_message}</p>
      )}
    </div>
  )
}
