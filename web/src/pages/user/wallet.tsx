// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 用户端钱包页（spec 支付网关 §9）：渠道列表 → 金额 + 渠道（显示实付/汇率/生效
// 上下限）→ 建单 → 跳网关；下方我的订单列表。
//
// `?order=<order_no>` 进入（网关 return 302 的目标）时进入**轮询**：定位该单
// （列表里按 order_no 找 id，单号不是主键）→ 每 3 s 查一次详情，最多 2 分钟，
// 或状态到 paid/closed 立即停。轮询用 react-query 的 refetchInterval，不用自建
// 定时器：卸载即自动停，不会泄漏。
import { useEffect, useMemo, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { motion } from 'framer-motion'
import { CircleCheck, CreditCard, ExternalLink, RefreshCw, Wallet, X } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { useSearchParams } from 'react-router-dom'
import { ApiError, userApi } from '@/lib/api/client'
import { PagePagination } from '@/components/page-pagination'
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
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
import { formatDateTime, formatRemaining } from '@/components/fmt'
import { cn } from '@/lib/utils'
import type { components } from '@/lib/api/schema'

type PaymentMethod = components['schemas']['PaymentMethod']
type OrderStatus = components['schemas']['OrderStatus']
type CreateOrderResponse = components['schemas']['CreateOrderResponse']

const STATUSES: OrderStatus[] = ['pending', 'paid', 'closed']

// 轮询节奏（spec §9：3 s，最多 2 分钟或到 paid/closed）。
const POLL_INTERVAL_MS = 3_000
const POLL_WINDOW_MS = 120_000

// 金额形状与服务端同源（openapi CreateOrderRequest）：`^\d{1,7}(\.\d{1,2})?$` 且 > 0。
// 前端只做"明显非法"的拦截，权威判定在服务端（400/503）。
const AMOUNT_RE = /^\d{1,7}(\.\d{1,2})?$/
const amountValid = (v: string) => AMOUNT_RE.test(v.trim()) && Number(v) > 0

// 快照金额展示：服务端已按 %d.%02d 格式化，**不再**二次换算（禁 float 往返）。
const money = (amount: string, currency: string) => `${amount} ${currency}`

// 状态徽章：pending 灰 / paid 绿 / closed 灰（与兑换码页同款点状徽章）。
function OrderStatusBadge({ status }: { status: OrderStatus }) {
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
      {t(`user.wallet.status.${status}`)}
    </Badge>
  )
}

// 预估实付：rate 是定点串（"7.200000"），**仅供参考**（methods.rate 不构成报价承诺，
// spec §5-3）——权威值是建单响应的 pay_amount，提交后必须在跳转前展示并要求确认。
function estimatePay(amount: string, rate: string): string | null {
  const a = Number(amount)
  const r = Number(rate)
  if (!Number.isFinite(a) || !Number.isFinite(r) || a <= 0 || r <= 0) return null
  return (Math.round(a * r * 100) / 100).toFixed(2)
}

export default function UserWallet() {
  const { t } = useTranslation()
  const qc = useQueryClient()
  const [searchParams, setSearchParams] = useSearchParams()
  // 网关 return 带回来的单号（不是主键 id）
  const focusOrderNo = searchParams.get('order') ?? ''

  // —— 渠道列表（三层开关已在服务端过滤；空 = 支付未启用/未装配协议）——
  const methodsQ = useQuery({ queryKey: ['user', 'payment-methods'], queryFn: () => userApi.listPaymentMethods() })
  const methods = methodsQ.data ?? []
  const [payType, setPayType] = useState('')
  // 渠道首次到达后默认选第一个（用户改过就不覆盖）
  useEffect(() => {
    if (!payType && methods.length > 0) setPayType(methods[0].pay_type)
  }, [methods, payType])
  const method: PaymentMethod | undefined = useMemo(
    () => methods.find(m => m.pay_type === payType),
    [methods, payType],
  )

  const [amount, setAmount] = useState('')
  // 下单响应 = 本次报价的**权威值**（pay_amount/rate 都是订单快照）。跳转前必须让用户
  // 看清这个金额：methods.rate 只是参考值，不许拿它当"用户已确认的价格"（spec §5-3）。
  const [created, setCreated] = useState<CreateOrderResponse | null>(null)
  const submitBlocked = !method || !amountValid(amount) || createWouldExceedLimit(amount, method)
  const create = useMutation({
    mutationFn: () => userApi.createOrder({ amount_usd: amount.trim(), pay_type: payType }),
    onSuccess: res => {
      qc.invalidateQueries({ queryKey: ['user', 'orders'] })
      // 不在这里跳：先弹确认层展示**权威实付**，点"去支付"才整页跳（网关非 SPA 路由）。
      setCreated(res)
    },
    onError: (e: unknown) => {
      toast.add({
        title: t('user.wallet.createFailed'),
        description: e instanceof ApiError ? e.message : t('user.common.error'),
        type: 'error',
      })
    },
  })

  // —— 我的订单（page/page_size 1-based + status 筛选）——
  const [page, setPage] = useState(1)
  const [pageSize, setPageSize] = useState(20)
  const [statusFilter, setStatusFilter] = useState<'all' | OrderStatus>('all')
  const ordersQ = useQuery({
    queryKey: ['user', 'orders', { page, page_size: pageSize, status: statusFilter }],
    queryFn: () =>
      userApi.listMyOrders({
        page,
        page_size: pageSize,
        status: statusFilter === 'all' ? undefined : statusFilter,
      }),
  })
  const rows = ordersQ.data?.rows ?? []
  // 末页死胡同守卫（与其它分页页同款）
  useEffect(() => {
    if (!ordersQ.isLoading && !ordersQ.isError && rows.length === 0 && page > 1) setPage(1)
  }, [ordersQ.isLoading, ordersQ.isError, rows.length, page])

  // —— ?order= 轮询 ——
  // 单号 → id：列表按 order_no 定位（订单号不是主键；新单在前，故首页 100 条足够，
  // 找不到时明确提示而不是空转）。
  const locateQ = useQuery({
    queryKey: ['user', 'orders', 'locate', focusOrderNo],
    queryFn: () => userApi.listMyOrders({ page: 1, page_size: 100 }),
    enabled: !!focusOrderNo,
  })
  const focusId = useMemo(
    () => locateQ.data?.rows.find(r => r.order_no === focusOrderNo)?.id ?? null,
    [locateQ.data, focusOrderNo],
  )
  // 2 分钟窗口：超时后停止轮询并给出"稍后手动刷新"的出口（不无限打服务端）
  const [pollExpired, setPollExpired] = useState(false)
  useEffect(() => {
    setPollExpired(false)
    if (!focusOrderNo) return
    const timer = setTimeout(() => setPollExpired(true), POLL_WINDOW_MS)
    return () => clearTimeout(timer)
  }, [focusOrderNo])

  const focusQ = useQuery({
    queryKey: ['user', 'order', focusId],
    queryFn: () => userApi.getMyOrder(focusId as number),
    enabled: focusId != null,
    refetchInterval: q => {
      if (!focusOrderNo || pollExpired) return false
      const status = q.state.data?.status
      // 终态即停（paid 到账 / closed 关单）；pending 继续 3 s 一次
      if (status === 'paid' || status === 'closed') return false
      return POLL_INTERVAL_MS
    },
  })
  const focus = focusQ.data
  // 到账即刷新订单列表与余额（overview 与侧边栏共用 ['user','me'] 缓存）
  useEffect(() => {
    if (focus?.status !== 'paid') return
    qc.invalidateQueries({ queryKey: ['user', 'orders'] })
    qc.invalidateQueries({ queryKey: ['user', 'me'] })
  }, [focus?.status, qc])

  const clearFocus = () => {
    const next = new URLSearchParams(searchParams)
    next.delete('order')
    setSearchParams(next, { replace: true })
  }

  const statusItems = Object.fromEntries([
    ['all', t('user.wallet.allStatus')],
    ...STATUSES.map(s => [s, t(`user.wallet.status.${s}`)]),
  ])
  const methodItems = Object.fromEntries(methods.map(m => [m.pay_type, `${m.pay_type} · ${m.currency}`]))
  const estimated = method ? estimatePay(amount, method.rate) : null
  const limitHint = method
    ? t('user.wallet.limitHint', { min: method.min_usd, max: method.max_usd })
    : ''

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">{t('user.wallet.title')}</h1>
        <p className="text-sm text-muted-foreground">{t('user.wallet.subtitle')}</p>
      </div>

      {/* —— 支付回跳后的等待卡（?order= 进入时显示） —— */}
      {focusOrderNo && (
        <motion.div initial={{ opacity: 0, y: 12 }} animate={{ opacity: 1, y: 0 }} transition={{ duration: 0.25 }}>
          <Card>
            <CardHeader>
              <CardTitle className="flex items-center justify-between gap-2">
                <span>{t('user.wallet.focusTitle')}</span>
                <Button variant="ghost" size="sm" onClick={clearFocus}>
                  <X /> {t('user.wallet.backToWallet')}
                </Button>
              </CardTitle>
              <CardDescription className="break-all font-mono">{focusOrderNo}</CardDescription>
            </CardHeader>
            <CardContent className="space-y-3">
              {locateQ.isError ? (
                <p className="text-sm text-destructive">{t('common.loadFailed', { message: (locateQ.error as Error).message })}</p>
              ) : locateQ.isLoading || (focusId != null && focusQ.isLoading) ? (
                <div className="space-y-2">
                  {Array.from({ length: 3 }).map((_, i) => <Skeleton key={i} className="h-6" />)}
                </div>
              ) : focusId == null ? (
                <Alert>
                  <AlertDescription>{t('user.wallet.focusNotFound')}</AlertDescription>
                </Alert>
              ) : focus ? (
                <div className="flex flex-wrap items-center gap-x-6 gap-y-2 text-sm">
                  <span className="flex items-center gap-2">
                    <OrderStatusBadge status={focus.status} />
                  </span>
                  <span className="text-muted-foreground">
                    {t('user.wallet.amountLabel')} <span className="font-medium text-foreground">{focus.amount_usd} USD</span>
                  </span>
                  <span className="text-muted-foreground">
                    {t('user.wallet.payAmountLabel')}{' '}
                    <span className="font-medium text-foreground">{money(focus.pay_amount, focus.currency)}</span>
                  </span>
                  <span className="text-muted-foreground">
                    {t('user.wallet.expiresAtLabel')} {formatDateTime(focus.expires_at)}
                  </span>
                  {focus.status === 'pending' && !pollExpired && (
                    <span className="text-xs text-muted-foreground">{t('user.wallet.polling')}</span>
                  )}
                  {focus.status === 'pending' && pollExpired && (
                    <span className="text-xs text-amber-700 dark:text-amber-400">{t('user.wallet.pollExpired')}</span>
                  )}
                  <Button variant="outline" size="sm" onClick={() => { void focusQ.refetch(); void ordersQ.refetch() }}>
                    <RefreshCw /> {t('user.wallet.refresh')}
                  </Button>
                </div>
              ) : (
                <p className="text-sm text-muted-foreground">{t('user.common.loading')}</p>
              )}
              {focus?.status === 'paid' && (
                <Alert>
                  <CircleCheck className="size-4 text-emerald-700 dark:text-emerald-400" />
                  <AlertTitle>{t('user.wallet.paidTitle')}</AlertTitle>
                  <AlertDescription>{t('user.wallet.paidDesc', { amount: focus.amount_usd })}</AlertDescription>
                </Alert>
              )}
              {focus?.status === 'closed' && (
                <Alert>
                  <AlertDescription>{t('user.wallet.closedDesc')}</AlertDescription>
                </Alert>
              )}
              {focus?.status === 'pending' && focus.pay_url && (
                <Button variant="outline" onClick={() => window.location.assign(focus.pay_url as string)}>
                  <ExternalLink /> {t('user.wallet.continuePay')}
                </Button>
              )}
            </CardContent>
          </Card>
        </motion.div>
      )}

      {/* —— 充值区 —— */}
      <motion.div initial={{ opacity: 0, y: 12 }} animate={{ opacity: 1, y: 0 }} transition={{ duration: 0.25, delay: 0.05 }}>
        <Card>
          <CardHeader>
            <CardTitle className="flex items-center gap-2"><Wallet className="size-4" /> {t('user.wallet.topupTitle')}</CardTitle>
            <CardDescription>{t('user.wallet.topupDesc')}</CardDescription>
          </CardHeader>
          <CardContent className="space-y-4">
            {methodsQ.isError ? (
              <p className="text-sm text-destructive">{t('common.loadFailed', { message: (methodsQ.error as Error).message })}</p>
            ) : methodsQ.isLoading ? (
              <div className="space-y-2">
                {Array.from({ length: 2 }).map((_, i) => <Skeleton key={i} className="h-10" />)}
              </div>
            ) : methods.length === 0 ? (
              <Alert>
                <AlertDescription>{t('user.wallet.noChannels')}</AlertDescription>
              </Alert>
            ) : (
              <>
                <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
                  <div className="space-y-1.5">
                    <Label htmlFor="wallet-amount">{t('user.wallet.amountLabel')}</Label>
                    <Input
                      id="wallet-amount"
                      inputMode="decimal"
                      autoComplete="off"
                      placeholder={t('user.wallet.amountPlaceholder')}
                      value={amount}
                      onChange={e => setAmount(e.target.value)}
                      onKeyDown={e => {
                        if (e.key === 'Enter' && !submitBlocked && !create.isPending) create.mutate()
                      }}
                      disabled={create.isPending}
                    />
                    {limitHint && <p className="text-xs text-muted-foreground">{limitHint}</p>}
                  </div>
                  <div className="space-y-1.5">
                    <Label>{t('user.wallet.channelLabel')}</Label>
                    <Select items={methodItems} value={payType} onValueChange={setPayType} disabled={create.isPending}>
                      <SelectTrigger className="w-full" aria-label={t('user.wallet.channelLabel')}>
                        <SelectValue />
                      </SelectTrigger>
                      <SelectContent>
                        {methods.map(m => (
                          <SelectItem key={m.pay_type} value={m.pay_type} label={`${m.pay_type} · ${m.currency}`}>
                            {m.pay_type} · {m.currency}
                          </SelectItem>
                        ))}
                      </SelectContent>
                    </Select>
                    {method && (
                      <p className="text-xs text-muted-foreground">
                        {t('user.wallet.rateReferenceLabel')} {method.rate}
                      </p>
                    )}
                  </div>
                </div>

                {/* 预估实付：参考汇率 × 金额（spec §5-3 说它不是报价承诺）。范围与免责文案
                    属于"说明"而不是"决策"，已经从卡片里去掉——上限在输入框下有 limitHint，
                    权威金额在跳转前的确认层再展示一次。 */}
                {method && (
                  <div className="rounded-[10px] border border-border/60 bg-black/[0.02] p-3 text-sm dark:bg-white/[0.03]">
                    <div className="flex flex-wrap items-center gap-x-6 gap-y-1">
                      <span className="text-muted-foreground">
                        {t('user.wallet.estimatePayLabel')}{' '}
                        <span className="font-medium text-foreground">
                          {estimated ? money(estimated, method.currency) : '—'}
                        </span>
                      </span>
                      <span className="text-muted-foreground">
                        {t('user.wallet.rateReferenceLabel')}{' '}
                        <span className="tabular-nums text-foreground">{method.rate}</span>
                      </span>
                    </div>
                  </div>
                )}

                {amount.trim() !== '' && !amountValid(amount) && (
                  <p className="text-sm text-destructive">{t('user.wallet.amountInvalid')}</p>
                )}
                {method && amountValid(amount) && createWouldExceedLimit(amount, method) && (
                  <p className="text-sm text-destructive">{limitHint}</p>
                )}

                <Button onClick={() => create.mutate()} disabled={submitBlocked || create.isPending}>
                  <CreditCard /> {create.isPending ? t('user.wallet.creating') : t('user.wallet.payNow')}
                </Button>
              </>
            )}
          </CardContent>
        </Card>
      </motion.div>

      {/* —— 我的订单 —— */}
      <motion.div initial={{ opacity: 0, y: 12 }} animate={{ opacity: 1, y: 0 }} transition={{ duration: 0.25, delay: 0.1 }} className="space-y-3">
        <div className="flex flex-wrap items-center gap-2 rounded-lg border p-3">
          <Select
            items={statusItems}
            value={statusFilter}
            onValueChange={v => { setStatusFilter(v as 'all' | OrderStatus); setPage(1) }}
          >
            <SelectTrigger size="default" className="w-40" aria-label={t('user.wallet.filterStatus')}>
              <SelectValue placeholder={t('user.wallet.filterStatus')} />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="all" label={t('user.wallet.allStatus')}>{t('user.wallet.allStatus')}</SelectItem>
              {STATUSES.map(s => (
                <SelectItem key={s} value={s} label={t(`user.wallet.status.${s}`)}>{t(`user.wallet.status.${s}`)}</SelectItem>
              ))}
            </SelectContent>
          </Select>
          {statusFilter !== 'all' && (
            <Button variant="ghost" size="lg" onClick={() => { setStatusFilter('all'); setPage(1) }}>
              <X /> {t('list.reset')}
            </Button>
          )}
        </div>

        {ordersQ.isError ? (
          <p className="text-sm text-destructive">{t('common.loadFailed', { message: (ordersQ.error as Error).message })}</p>
        ) : ordersQ.isLoading ? (
          <div className="space-y-2">
            {Array.from({ length: 4 }).map((_, i) => <Skeleton key={i} className="h-12" />)}
          </div>
        ) : rows.length === 0 ? (
          <Card className="flex flex-col items-center gap-2 py-12 text-muted-foreground">
            <CreditCard className="size-10" />
            <p className="font-medium">{t('user.wallet.emptyTitle')}</p>
            <p className="text-sm">{t('user.wallet.emptyDesc')}</p>
          </Card>
        ) : (
          <>
            <ScrollArea
              data-od-id="table-scroll-user-orders"
              className="rounded-[14px] border border-[rgba(19,45,83,0.26)] bg-[color:var(--glass-card-light)] shadow-[inset_0_1px_0_rgba(255,255,255,0.5),0_10px_36px_rgba(19,45,83,0.16)] backdrop-blur-[var(--glass-blur)] dark:bg-[color:var(--glass-card-dark)] dark:shadow-[inset_0_1px_0_rgba(255,255,255,0.07),0_10px_36px_rgba(2,6,14,0.5)] dark:border-[rgba(148,180,220,0.32)]"
              showHorizontal
            >
              <Table className="min-w-[980px]" containerClassName="overflow-x-visible border-0 shadow-none rounded-none bg-transparent backdrop-blur-none">
                <TableHeader>
                  <TableRow>
                    <TableHead>{t('user.wallet.table.orderNo')}</TableHead>
                    <TableHead>{t('user.wallet.table.status')}</TableHead>
                    <TableHead>{t('user.wallet.table.amount')}</TableHead>
                    <TableHead>{t('user.wallet.table.payAmount')}</TableHead>
                    <TableHead>{t('user.wallet.table.createdAt')}</TableHead>
                    <TableHead>{t('user.wallet.table.paidAt')}</TableHead>
                    <TableHead className="text-right">{t('user.wallet.table.actions')}</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody className="[&_td]:py-3">
                  {rows.map(r => (
                    <TableRow key={r.id} data-state={r.order_no === focusOrderNo ? 'selected' : undefined}>
                      <TableCell><code className="font-mono text-sm">{r.order_no}</code></TableCell>
                      <TableCell><OrderStatusBadge status={r.status} /></TableCell>
                      <TableCell className="tabular-nums">{r.amount_usd} USD</TableCell>
                      <TableCell className="tabular-nums">{money(r.pay_amount, r.currency)}</TableCell>
                      <TableCell>{formatDateTime(r.created_at)}</TableCell>
                      <TableCell>{r.paid_at ? formatDateTime(r.paid_at) : '—'}</TableCell>
                      <TableCell className="text-right">
                        {r.status === 'pending' && r.pay_url ? (
                          <Button variant="outline" size="sm" onClick={() => window.location.assign(r.pay_url as string)}>
                            <ExternalLink /> {t('user.wallet.continuePay')}
                          </Button>
                        ) : r.status === 'pending' ? (
                          // 过期/签名不可用时 pay_url=null：只能重新下单，不给死按钮
                          <span className="text-xs text-muted-foreground">{formatRemaining(r.expires_at) ?? t('user.wallet.expired')}</span>
                        ) : (
                          <span className="text-xs text-muted-foreground">—</span>
                        )}
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            </ScrollArea>
            <PagePagination
              total={ordersQ.data?.total ?? 0}
              pageSize={pageSize}
              page={page}
              onPageChange={setPage}
              onPageSizeChange={s => { setPageSize(s); setPage(1) }}
            />
          </>
        )}
      </motion.div>

      {/* —— 跳转前的金额确认（spec §5-3：pay_amount 是权威值，必须让用户看到并确认）—— */}
      <Dialog open={!!created} onOpenChange={o => { if (!o) setCreated(null) }}>
        <DialogContent className="sm:max-w-md">
          <DialogHeader>
            <DialogTitle>{t('user.wallet.confirmTitle')}</DialogTitle>
            <DialogDescription>{t('user.wallet.confirmDesc')}</DialogDescription>
          </DialogHeader>
          {created && (
            <div className="space-y-3">
              <div className="rounded-[10px] border border-border/60 bg-black/[0.02] p-3 dark:bg-white/[0.03]">
                <p className="text-xs text-muted-foreground">{t('user.wallet.confirmPayAmount')}</p>
                <p className="text-2xl font-semibold tabular-nums">{money(created.pay_amount, created.currency)}</p>
                <p className="mt-1 text-xs text-muted-foreground">{t('user.wallet.confirmAuthoritative')}</p>
              </div>
              <div className="grid grid-cols-1 gap-2 text-sm sm:grid-cols-2">
                <div className="space-y-0.5">
                  <p className="text-xs text-muted-foreground">{t('user.wallet.amountLabel')}</p>
                  <p className="tabular-nums">{created.amount_usd} USD</p>
                </div>
                <div className="space-y-0.5">
                  <p className="text-xs text-muted-foreground">{t('user.wallet.confirmRate')}</p>
                  <p className="tabular-nums">{created.rate}</p>
                </div>
                <div className="space-y-0.5">
                  <p className="text-xs text-muted-foreground">{t('user.wallet.table.orderNo')}</p>
                  <p className="break-all font-mono text-xs">{created.order_no}</p>
                </div>
                <div className="space-y-0.5">
                  <p className="text-xs text-muted-foreground">{t('user.wallet.expiresAtLabel')}</p>
                  <p className="text-xs">{formatDateTime(created.expires_at)}</p>
                </div>
              </div>
              <p className="text-xs text-muted-foreground">{t('user.wallet.confirmKeepHint')}</p>
            </div>
          )}
          <DialogFooter>
            <Button variant="outline" onClick={() => setCreated(null)}>
              {t('user.wallet.confirmCancel')}
            </Button>
            <Button onClick={() => created && window.location.assign(created.pay_url)}>
              <ExternalLink /> {t('user.wallet.confirmGo')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
}

// createWouldExceedLimit 金额是否越过该渠道的**生效**上限（min/max 都是 USD 整数）。
// 与服务端一致：低于下限/高于上限由服务端 400，这里只提前挡掉明显的越界。
function createWouldExceedLimit(amount: string, method: PaymentMethod): boolean {
  const a = Number(amount)
  if (!Number.isFinite(a)) return false
  return a < method.min_usd || a > method.max_usd
}
