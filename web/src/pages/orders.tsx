// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 管理面订单页（spec 支付网关 §9）：全量表格 + user_id/status/out_trade_no 过滤 +
// 详情抽屉（raw_notify / fail_reason / fail_count / last_notify_at / resolved_at）+
// sync / close / resolve。
//
// 这里是「已付款未到账」的唯一运维闭环：fail_reason 说明卡在哪一步，last_notify_at
// 说明网关还在不在重试，resolved_at 记录人工已处置（防同一单被补两次）。因此详情
// 用抽屉而不是跳页——处置动作要能看着信号做，并且做完就地刷新。
import { useEffect, useMemo, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { AlertTriangle, ArrowDownUp, CircleCheck, Filter, Lock, RefreshCw, X } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import type { DynamicKey } from '@/lib/i18n'
import { api } from '@/App'
import { ApiError } from '@/lib/api/client'
import { Pagination } from '@/components/pagination'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { Checkbox } from '@/components/ui/checkbox'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { ScrollArea } from '@/components/ui/scroll-area'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { Skeleton } from '@/components/ui/skeleton'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { toast } from '@/components/ui/toast'
import { formatDateTime } from '@/components/fmt'
import { cn } from '@/lib/utils'
import type { components } from '@/lib/api/schema'

type OrderDetail = components['schemas']['OrderDetail']
type OrderStatus = components['schemas']['OrderStatus']

const STATUSES: OrderStatus[] = ['pending', 'paid', 'closed']
// 服务端上限 100（openapi /orders 的 limit maximum）——分页条必须传同一个集合，
// 否则选中 1000 时服务端钳制、本地页码与总数对不上。
const PAGE_SIZES = [10, 20, 50, 100]

// 状态徽章（与用户钱包页同款点状徽章，颜色语义一致）。
function StatusBadge({ status }: { status: OrderStatus }) {
  const { t } = useTranslation()
  const tone =
    status === 'paid'
      ? 'text-emerald-700 dark:text-emerald-400'
      : status === 'pending'
        ? 'text-amber-700 dark:text-amber-400'
        : 'text-muted-foreground'
  const dot = status === 'paid' ? 'bg-emerald-500' : status === 'pending' ? 'bg-amber-500' : 'bg-muted-foreground/60'
  return (
    <Badge variant="secondary" className={cn('gap-1.5', tone)}>
      <span className={cn('size-1.5 shrink-0 rounded-full', dot)} />
      {t(`orders.status.${status}`)}
    </Badge>
  )
}

// fail_reason 徽章：closed（已付款却收到关单/关单后成功）与 internal（网关应重试）
// 是最需要人介入的两类，单独着色；null = 无异常。
function FailReasonBadge({ reason }: { reason?: string | null }) {
  const { t } = useTranslation()
  if (!reason) return <span className="text-xs text-muted-foreground">—</span>
  const urgent = reason === 'closed' || reason === 'internal'
  return (
    <Badge
      variant="secondary"
      className={cn('gap-1', urgent ? 'text-red-700 dark:text-red-400' : 'text-amber-700 dark:text-amber-400')}
    >
      {urgent && <AlertTriangle className="size-3" />}
      {t(`orders.failReason.${reason}` as DynamicKey)}
    </Badge>
  )
}

// prettyRawJSON 把 raw_notify 折成缩进 JSON；解析失败（历史脏数据/截断前缀）原文直出。
function prettyRawJSON(raw?: string | null): string {
  if (!raw) return ''
  try {
    return JSON.stringify(JSON.parse(raw), null, 2)
  } catch {
    return raw
  }
}

// errMsg 统一取服务端 error 文案（401 由全局 handleAuthError 处理，页面不再弹）。
const errMsg = (e: unknown) => (e instanceof ApiError ? e.message : (e as Error)?.message)

export default function Orders() {
  const { t } = useTranslation()
  const qc = useQueryClient()

  // —— 过滤（草稿 + 已应用：只有点查询/回车才落条件，避免输入过程中打服务端）——
  const [userIdDraft, setUserIdDraft] = useState('')
  const [outTradeNoDraft, setOutTradeNoDraft] = useState('')
  const [statusDraft, setStatusDraft] = useState<'all' | OrderStatus>('all')
  const [userId, setUserId] = useState<number | undefined>(undefined)
  const [outTradeNo, setOutTradeNo] = useState('')
  const [status, setStatus] = useState<'all' | OrderStatus>('all')
  const [offset, setOffset] = useState(0)
  const [limit, setLimit] = useState(20)

  const applyFilters = () => {
    const uid = userIdDraft.trim() === '' ? undefined : Number(userIdDraft)
    setUserId(uid != null && Number.isFinite(uid) ? uid : undefined)
    setOutTradeNo(outTradeNoDraft.trim())
    setStatus(statusDraft)
    setOffset(0)
  }
  const resetFilters = () => {
    setUserIdDraft('')
    setOutTradeNoDraft('')
    setStatusDraft('all')
    setUserId(undefined)
    setOutTradeNo('')
    setStatus('all')
    setOffset(0)
  }
  const hasFilters = userId !== undefined || outTradeNo !== '' || status !== 'all'

  const ordersQ = useQuery({
    queryKey: ['orders', { user_id: userId, status, out_trade_no: outTradeNo, limit, offset }],
    queryFn: () =>
      api.listOrders({
        user_id: userId,
        status: status === 'all' ? undefined : status,
        out_trade_no: outTradeNo || undefined,
        limit,
        offset,
      }),
  })
  const rows = ordersQ.data?.rows ?? []
  const total = ordersQ.data?.total ?? 0

  // 末页死胡同守卫：过滤后当前页被清空 → 回到第一页（页 1 空 = 真的没有数据）
  useEffect(() => {
    if (!ordersQ.isLoading && !ordersQ.isError && rows.length === 0 && offset > 0) setOffset(0)
  }, [ordersQ.isLoading, ordersQ.isError, rows.length, offset])

  // —— 详情抽屉 ——
  const [detailId, setDetailId] = useState<number | null>(null)
  const detailQ = useQuery({
    queryKey: ['order', detailId],
    queryFn: () => api.getOrder(detailId as number),
    enabled: detailId != null,
  })
  const detail = detailQ.data
  // 动作返回的权威订单：直接回写缓存，抽屉里的 fail_reason/fail_count 立即反映结果
  const applyOrder = (o: OrderDetail) => {
    qc.setQueryData(['order', o.id], o)
    qc.invalidateQueries({ queryKey: ['orders'] })
  }

  const sync = useMutation({
    mutationFn: (id: number) => api.syncOrder(id),
    onSuccess: res => {
      applyOrder(res.order)
      toast.add({
        title: res.credited ? t('orders.syncCredited') : t('orders.syncDone'),
        description: res.already_paid ? t('orders.syncAlreadyPaid') : undefined,
        type: 'success',
      })
    },
    onError: (e: unknown) =>
      toast.add({ title: t('orders.syncFailed'), description: errMsg(e), type: 'error' }),
  })

  // close 需要确认：它是"关单"这个不可逆处置；force 只在无查单能力/查询失败时生效
  // （服务端优先级写死），所以默认不勾。
  const [closing, setClosing] = useState<OrderDetail | null>(null)
  const [force, setForce] = useState(false)
  const close = useMutation({
    mutationFn: (id: number) => api.closeOrder(id, force),
    onSuccess: res => {
      applyOrder(res.order)
      setClosing(null)
      if (res.status === 'paid') {
        toast.add({
          title: res.credited ? t('orders.closeCredited') : t('orders.syncAlreadyPaid'),
          type: 'success',
        })
      } else {
        toast.add({ title: t('orders.closed'), type: 'success' })
      }
    },
    onError: (e: unknown) => {
      // 502 = 查到 paid 但入账失败：订单**保持 pending**，fail_reason 已落库
      toast.add({ title: t('orders.closeFailed'), description: errMsg(e), type: 'error' })
    },
  })

  const resolve = useMutation({
    mutationFn: (id: number) => api.resolveOrder(id),
    onSuccess: res => {
      applyOrder(res.order)
      toast.add({ title: t('orders.resolved'), type: 'success' })
    },
    onError: (e: unknown) => {
      // 409 = 已处置过（防同一单补两次），不是故障
      if (e instanceof ApiError && e.status === 409) {
        toast.add({ title: t('orders.alreadyResolved'), type: 'error' })
        return
      }
      toast.add({ title: t('orders.resolveFailed'), description: errMsg(e), type: 'error' })
    },
  })

  // 抽屉里的动作是否可用：非 pending 的订单不能 sync/close（服务端 409，别给死按钮）；
  // resolve 只在「有异常信号且未处置」时才有意义。
  const canSync = detail?.status === 'pending'
  const canClose = detail?.status === 'pending'
  const canResolve = !!detail && !detail.resolved_at && !!detail.fail_reason
  const busy = sync.isPending || close.isPending || resolve.isPending

  const statusItems = useMemo(
    () => Object.fromEntries([['all', t('orders.all')], ...STATUSES.map(s => [s, t(`orders.status.${s}`)])]),
    [t],
  )
  const rawText = useMemo(() => prettyRawJSON(detail?.raw_notify), [detail?.raw_notify])

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">{t('orders.title')}</h1>
        <p className="text-sm text-muted-foreground">{t('orders.subtitle')}</p>
      </div>

      {/* —— 过滤工具栏 —— */}
      <div className="flex flex-wrap items-end gap-2 rounded-lg border p-3">
        <div className="space-y-1">
          <Label htmlFor="orders-user-id" className="text-xs">{t('orders.filterUserId')}</Label>
          <Input
            id="orders-user-id"
            inputMode="numeric"
            className="w-32"
            value={userIdDraft}
            onChange={e => setUserIdDraft(e.target.value)}
            onKeyDown={e => { if (e.key === 'Enter') applyFilters() }}
          />
        </div>
        <div className="space-y-1">
          <Label className="text-xs">{t('orders.filterStatus')}</Label>
          <Select items={statusItems} value={statusDraft} onValueChange={v => setStatusDraft(v as 'all' | OrderStatus)}>
            <SelectTrigger size="default" className="w-40" aria-label={t('orders.filterStatus')}>
              <SelectValue placeholder={t('orders.filterStatus')} />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="all" label={t('orders.all')}>{t('orders.all')}</SelectItem>
              {STATUSES.map(s => (
                <SelectItem key={s} value={s} label={t(`orders.status.${s}`)}>{t(`orders.status.${s}`)}</SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>
        <div className="space-y-1">
          <Label htmlFor="orders-out-trade-no" className="text-xs">{t('orders.filterOutTradeNo')}</Label>
          <Input
            id="orders-out-trade-no"
            className="w-64 font-mono text-sm"
            value={outTradeNoDraft}
            onChange={e => setOutTradeNoDraft(e.target.value)}
            onKeyDown={e => { if (e.key === 'Enter') applyFilters() }}
            placeholder="P2026…"
            spellCheck={false}
            autoComplete="off"
          />
        </div>
        <Button onClick={applyFilters}><Filter /> {t('orders.apply')}</Button>
        {hasFilters && <Button variant="ghost" onClick={resetFilters}><X /> {t('list.reset')}</Button>}
        <Button variant="ghost" onClick={() => ordersQ.refetch()} disabled={ordersQ.isFetching}>
          <RefreshCw className={cn(ordersQ.isFetching && 'animate-spin')} /> {t('orders.refresh')}
        </Button>
      </div>

      {ordersQ.isError ? (
        <p className="text-sm text-destructive">{t('common.loadFailed', { message: errMsg(ordersQ.error) })}</p>
      ) : ordersQ.isLoading ? (
        <div className="space-y-2">
          {Array.from({ length: 5 }).map((_, i) => <Skeleton key={i} className="h-12" />)}
        </div>
      ) : rows.length === 0 ? (
        <Card className="flex flex-col items-center gap-2 py-12 text-muted-foreground">
          <ArrowDownUp className="size-10" />
          <p className="font-medium">{hasFilters ? t('orders.filterEmpty') : t('orders.emptyTitle')}</p>
          {!hasFilters && <p className="text-sm">{t('orders.emptyDesc')}</p>}
        </Card>
      ) : (
        <>
          <ScrollArea
            data-od-id="table-scroll-orders"
            className="rounded-[14px] border border-[rgba(19,45,83,0.26)] bg-[color:var(--glass-card-light)] shadow-[inset_0_1px_0_rgba(255,255,255,0.5),0_10px_36px_rgba(19,45,83,0.16)] backdrop-blur-[var(--glass-blur)] dark:bg-[color:var(--glass-card-dark)] dark:shadow-[inset_0_1px_0_rgba(255,255,255,0.07),0_10px_36px_rgba(2,6,14,0.5)] dark:border-[rgba(148,180,220,0.32)]"
            showHorizontal
          >
            <Table className="min-w-[1240px]" containerClassName="overflow-x-visible border-0 shadow-none rounded-none bg-transparent backdrop-blur-none">
              <TableHeader>
                <TableRow>
                  <TableHead>ID</TableHead>
                  <TableHead>{t('orders.table.orderNo')}</TableHead>
                  <TableHead>{t('orders.table.userId')}</TableHead>
                  <TableHead>{t('orders.table.status')}</TableHead>
                  <TableHead>{t('orders.table.amount')}</TableHead>
                  <TableHead>{t('orders.table.payAmount')}</TableHead>
                  <TableHead>{t('orders.table.channel')}</TableHead>
                  {/* 上游账户快照（account_key）：按上游对账的索引键，也是「哪个账户收的钱」 */}
                  <TableHead>{t('orders.table.accountKey')}</TableHead>
                  <TableHead>{t('orders.table.failReason')}</TableHead>
                  <TableHead>{t('orders.table.createdAt')}</TableHead>
                  <TableHead className="text-right">{t('orders.table.actions')}</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody className="[&_td]:py-3">
                {rows.map(o => (
                  <TableRow key={o.id} data-state={detailId === o.id ? 'selected' : undefined}>
                    <TableCell className="tabular-nums">{o.id}</TableCell>
                    <TableCell><code className="font-mono text-sm">{o.order_no}</code></TableCell>
                    <TableCell className="tabular-nums">{o.user_id}</TableCell>
                    <TableCell><StatusBadge status={o.status} /></TableCell>
                    <TableCell className="tabular-nums">{o.amount_usd} USD</TableCell>
                    <TableCell className="tabular-nums">{o.pay_amount} {o.currency}</TableCell>
                    <TableCell className="text-xs text-muted-foreground">{o.pay_type} · {o.provider}</TableCell>
                    <TableCell><code className="font-mono text-xs">{o.account_key}</code></TableCell>
                    <TableCell><FailReasonBadge reason={o.fail_reason} /></TableCell>
                    <TableCell>{formatDateTime(o.created_at)}</TableCell>
                    <TableCell className="text-right">
                      <Button variant="outline" size="sm" onClick={() => setDetailId(o.id)}>
                        {t('orders.detail')}
                      </Button>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </ScrollArea>
          <Pagination
            total={total}
            limit={limit}
            offset={offset}
            onOffsetChange={setOffset}
            onLimitChange={l => { setLimit(l); setOffset(0) }}
            pageSizes={PAGE_SIZES}
          />
        </>
      )}

      {/* —— 详情抽屉（右侧整高）：运维闭环的唯一信号面 —— */}
      <Dialog open={detailId != null} onOpenChange={o => { if (!o) setDetailId(null) }}>
        <DialogContent className="top-0 right-0 left-auto h-dvh max-h-dvh translate-x-0 translate-y-0 rounded-none rounded-l-[14px] sm:max-w-xl">
          <DialogHeader>
            <DialogTitle>{t('orders.detailTitle')}</DialogTitle>
            <DialogDescription className="break-all font-mono">{detail?.order_no ?? ''}</DialogDescription>
          </DialogHeader>

          {detailQ.isError ? (
            <p className="text-sm text-destructive">{errMsg(detailQ.error)}</p>
          ) : detailQ.isLoading || !detail ? (
            <div className="space-y-2">
              {Array.from({ length: 6 }).map((_, i) => <Skeleton key={i} className="h-8" />)}
            </div>
          ) : (
            <div className="space-y-4">
              {/* 关键信号：状态 + fail_reason + fail_count + last_notify_at + resolved_at */}
              <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
                <div className="space-y-1">
                  <p className="text-xs text-muted-foreground">{t('orders.table.status')}</p>
                  <StatusBadge status={detail.status} />
                </div>
                <div className="space-y-1">
                  <p className="text-xs text-muted-foreground">{t('orders.table.failReason')}</p>
                  <FailReasonBadge reason={detail.fail_reason} />
                </div>
                <div className="space-y-1">
                  <p className="text-xs text-muted-foreground">{t('orders.field.failCount')}</p>
                  <p className="tabular-nums text-sm">{detail.fail_count}</p>
                </div>
                <div className="space-y-1">
                  <p className="text-xs text-muted-foreground">{t('orders.field.lastNotifyAt')}</p>
                  <p className="text-sm">{detail.last_notify_at ? formatDateTime(detail.last_notify_at) : '—'}</p>
                </div>
                <div className="space-y-1">
                  <p className="text-xs text-muted-foreground">{t('orders.field.resolvedAt')}</p>
                  <p className="text-sm">
                    {detail.resolved_at ? (
                      <span className="inline-flex items-center gap-1 text-emerald-700 dark:text-emerald-400">
                        <CircleCheck className="size-3.5" /> {formatDateTime(detail.resolved_at)}
                      </span>
                    ) : '—'}
                  </p>
                </div>
                <div className="space-y-1">
                  <p className="text-xs text-muted-foreground">{t('orders.field.tradeNo')}</p>
                  <p className="break-all font-mono text-sm">{detail.trade_no || '—'}</p>
                </div>
                <div className="space-y-1">
                  <p className="text-xs text-muted-foreground">{t('orders.table.userId')}</p>
                  <p className="tabular-nums text-sm">{detail.user_id}</p>
                </div>
                <div className="space-y-1">
                  <p className="text-xs text-muted-foreground">{t('orders.field.accountKey')}</p>
                  <p className="break-all font-mono text-sm">{detail.account_key}</p>
                </div>
                <div className="space-y-1">
                  <p className="text-xs text-muted-foreground">{t('orders.table.amount')}</p>
                  <p className="tabular-nums text-sm">{detail.amount_usd} USD</p>
                </div>
                <div className="space-y-1">
                  <p className="text-xs text-muted-foreground">{t('orders.table.payAmount')}</p>
                  <p className="tabular-nums text-sm">{detail.pay_amount} {detail.currency}</p>
                </div>
                <div className="space-y-1">
                  <p className="text-xs text-muted-foreground">{t('orders.field.rate')}</p>
                  <p className="tabular-nums text-sm">{detail.rate}</p>
                </div>
                <div className="space-y-1">
                  <p className="text-xs text-muted-foreground">{t('orders.table.createdAt')}</p>
                  <p className="text-sm">{formatDateTime(detail.created_at)}</p>
                </div>
                <div className="space-y-1">
                  <p className="text-xs text-muted-foreground">{t('orders.field.paidAt')}</p>
                  <p className="text-sm">{detail.paid_at ? formatDateTime(detail.paid_at) : '—'}</p>
                </div>
                <div className="space-y-1">
                  <p className="text-xs text-muted-foreground">{t('orders.field.expiresAt')}</p>
                  <p className="text-sm">{formatDateTime(detail.expires_at)}</p>
                </div>
                <div className="space-y-1">
                  <p className="text-xs text-muted-foreground">{t('orders.field.redemptionCodeId')}</p>
                  <p className="tabular-nums text-sm">{detail.redemption_code_id ?? '—'}</p>
                </div>
              </div>

              {/* raw_notify：含买家信息，仅管理面可见；超长已由服务端以
                  {"truncated":true,"prefix":…} 形式截断，这里只做展示 */}
              <div className="space-y-1">
                <p className="text-xs text-muted-foreground">{t('orders.field.rawNotify')}</p>
                {rawText ? (
                  <ScrollArea className="max-h-56 rounded-lg border bg-muted/40 p-3">
                    <pre className="whitespace-pre-wrap break-all font-mono text-xs leading-5">{rawText}</pre>
                  </ScrollArea>
                ) : (
                  <p className="text-sm text-muted-foreground">{t('orders.rawNotifyEmpty')}</p>
                )}
              </div>

              {(sync.isError || close.isError || resolve.isError) && (
                <Alert>
                  <AlertDescription className="text-destructive">
                    {errMsg(sync.error ?? close.error ?? resolve.error)}
                  </AlertDescription>
                </Alert>
              )}
            </div>
          )}

          <DialogFooter className="sm:justify-between">
            <Button variant="outline" onClick={() => setDetailId(null)} disabled={busy}>
              {t('common.close')}
            </Button>
            <div className="flex flex-wrap gap-2">
              <Button
                variant="outline"
                onClick={() => detail && sync.mutate(detail.id)}
                disabled={!canSync || busy}
                title={canSync ? undefined : t('orders.onlyPending')}
              >
                <RefreshCw className={cn(sync.isPending && 'animate-spin')} /> {t('orders.sync')}
              </Button>
              <Button
                variant="outline"
                onClick={() => { if (detail) { setForce(false); setClosing(detail) } }}
                disabled={!canClose || busy}
                title={canClose ? undefined : t('orders.onlyPending')}
              >
                <Lock /> {t('orders.close')}
              </Button>
              <Button
                onClick={() => detail && resolve.mutate(detail.id)}
                disabled={!canResolve || busy}
                title={canResolve ? undefined : t('orders.resolveHint')}
              >
                <CircleCheck /> {t('orders.resolve')}
              </Button>
            </div>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* —— 关单确认（三态语义：查单报 paid 时会转入账而非关单，force 无效）—— */}
      <Dialog open={!!closing} onOpenChange={o => { if (!o && !close.isPending) setClosing(null) }}>
        <DialogContent className="sm:max-w-md">
          <DialogHeader>
            <DialogTitle>{t('orders.closeTitle')}</DialogTitle>
            <DialogDescription>{t('orders.closeDesc', { orderNo: closing?.order_no ?? '' })}</DialogDescription>
          </DialogHeader>
          <div className="flex items-start gap-2">
            <Checkbox id="orders-close-force" checked={force} onCheckedChange={c => setForce(c === true)} />
            <div className="space-y-1">
              <Label htmlFor="orders-close-force">{t('orders.forceLabel')}</Label>
              <p className="text-xs text-muted-foreground">{t('orders.forceHint')}</p>
            </div>
          </div>
          {close.isError && <p className="text-sm text-destructive">{errMsg(close.error)}</p>}
          <DialogFooter>
            <Button variant="outline" onClick={() => setClosing(null)} disabled={close.isPending}>
              {t('common.cancel')}
            </Button>
            <Button
              variant="destructive"
              onClick={() => closing && close.mutate(closing.id)}
              disabled={close.isPending}
            >
              {close.isPending ? t('orders.closing') : t('orders.closeConfirm')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

    </div>
  )
}
