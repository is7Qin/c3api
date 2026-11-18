// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 用户端钱包（wallet-topup-redeem）：余额/临时余额摘要 → 主任务 Tabs（充值/兑换）→
// 记录 Tabs（订单/兑换记录）。三个模式（支付关 / 支付开有渠道 / 支付开零渠道）由
// lib/wallet-mode.ts 决定；本页只按它给出的可用集合渲染，不自行推导。
//
// 硬性渲染边界（源码形状 tripwire 在 payment-visibility.test.ts 里守）：
//   · 支付关闭：渠道查询带 `enabled: payment` 根本不发；充值 Tab/表单/渠道/金额/汇率/
//     下单按钮/订单列表全部不渲染（渲染分支，不是 CSS 隐藏），默认落在兑换。
//   · 支付开但零渠道：充值位只有「暂时无法充值」说明卡 + 去兑换，不渲染可提交表单。
//   · 兑换侧（表单/回执/记录）不在任何支付守卫内：支付关闭时兑换仍必须可用。
//
// `?tab=` 只决定 Tab 视图（topup|redeem|orders|redemptions）：非法值或该模式下不可用的
// 值安静回落到模式默认**且不修改 URL**。`?order=<order_no>`（网关 return 的目标）优先级
// 最高：等待卡渲染在 Tabs 之上并轮询（3 s，最多 2 分钟，paid/closed 即停），不会被
// `?tab=redeem` 遮住。
import { useEffect, useMemo, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { motion } from 'framer-motion'
import { CircleCheck, CreditCard, ExternalLink, RefreshCw, Ticket, Timer, Wallet, X, Zap } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import type { TFunction } from 'i18next'
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
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { toast } from '@/components/ui/toast'
import { formatDateTime, formatRemaining } from '@/components/fmt'
import { cn } from '@/lib/utils'
import {
  activeTabs,
  isMainTab,
  isRecordTab,
  showRedeemShortcut,
  visibleTabs,
  walletMode,
  type ChannelsState,
  type WalletTab,
} from '@/lib/wallet-mode'
import { useFeatures, FEATURES_QUERY_KEY } from '@/lib/use-features'
import type { components } from '@/lib/api/schema'

type PaymentMethod = components['schemas']['PaymentMethod']
type OrderStatus = components['schemas']['OrderStatus']
type CreateOrderResponse = components['schemas']['CreateOrderResponse']
type RedemptionType = components['schemas']['RedemptionType']
type Applied = components['schemas']['RedeemResponse']['applied']

const STATUSES: OrderStatus[] = ['pending', 'paid', 'closed']

// 轮询节奏（spec 支付网关 §9：3 s，最多 2 分钟或到 paid/closed）。
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

// 面值展示（与原兑换页同规则，2026-08-15 对齐修复）：Value 已换算 USD——直接显示
// （勿再 /1e5）；concurrency → 并发数直出。
function formatValue(type: RedemptionType, value: number): string {
  return type === 'concurrency' ? String(value) : `$${value.toFixed(2)}`
}

// applied 回执差异化文案（原样迁移）：balance → 余额 +USD；concurrency → 并发上限 +N；
// temp_balance → 临时余额 +USD + 到期时间（resource_expires_at → 本地时间）。
function appliedText(a: Applied, t: TFunction): string {
  if (a.type === 'concurrency') {
    return t('user.wallet.successConcurrency', { value: a.value })
  }
  const value = formatValue(a.type, a.value)
  if (a.type === 'balance') return t('user.wallet.successBalance', { value })
  return `${t('user.wallet.successTempBalance', { value })} · ${t('user.wallet.successExpiresAt', {
    time: a.resource_expires_at ? formatDateTime(a.resource_expires_at) : '—',
  })}`
}

export default function UserWallet() {
  const { t } = useTranslation()
  const qc = useQueryClient()
  const [searchParams, setSearchParams] = useSearchParams()
  // 网关 return 带回来的单号（不是主键 id）与 Tab 参数
  const focusOrderNo = searchParams.get('order') ?? ''
  const rawTab = searchParams.get('tab')
  const { payment } = useFeatures()

  // —— 运行时开关：跨标签页切回来时重新取一次 feature 状态 ——
  // 全局 refetchOnWindowFocus=false、useFeatures 的 staleTime 无限，所以这里显式定向失效
  // （与设置页同一精确键，绝不碰 `['user']` 整前缀）：管理员在另一个标签页关掉支付后，
  // 切回钱包会立刻移除充值区块并落回兑换，不露出已关闭内容。
  useEffect(() => {
    const refresh = () => {
      if (document.visibilityState === 'hidden') return
      void qc.invalidateQueries({ queryKey: FEATURES_QUERY_KEY })
    }
    window.addEventListener('focus', refresh)
    document.addEventListener('visibilitychange', refresh)
    return () => {
      window.removeEventListener('focus', refresh)
      document.removeEventListener('visibilitychange', refresh)
    }
  }, [qc])

  // —— 余额摘要（余额 / 并发上限 / 临时余额）——
  // 数据源沿用既有 query 键：`['user','me']` 与 `['user','temp-balances']`（个人中心同键，
  // 共用缓存）。失败时 react-query 保留旧值，页面给"重试"，不清空已展示的数字。
  const meQ = useQuery({ queryKey: ['user', 'me'], queryFn: () => userApi.me() })
  const tempQ = useQuery({ queryKey: ['user', 'temp-balances'], queryFn: () => userApi.getTempBalances() })
  const tempTotal = tempQ.data?.total_usd ?? 0
  // 临时余额的"到期"只取**有效额度**的最早一笔：金额 > 0 且未过期（永久额度不参与取最早）。
  // 全部是永久额度 → permanent（渲染成"永久"）；一笔有效额度都没有 → undefined（不显示后缀，
  // 已过期的桶不该在余额卡上冒充"当前额度"的到期时间）。
  const tempExpiry = useMemo<{ kind: 'at'; iso: string } | { kind: 'permanent' } | undefined>(() => {
    const now = Date.now()
    const valid = (tempQ.data?.rows ?? []).filter(r => r.amount_usd > 0)
    if (valid.length === 0) return undefined
    const dated = valid
      .map(r => r.expires_at)
      .filter((v): v is string => typeof v === 'string' && v !== '' && new Date(v).getTime() > now)
    if (dated.length === 0) return { kind: 'permanent' }
    const iso = dated.reduce((a, b) => (new Date(a).getTime() <= new Date(b).getTime() ? a : b))
    return { kind: 'at', iso }
  }, [tempQ.data])

  // —— 渠道列表（三层开关已在服务端过滤）——
  // 支付关闭时 `enabled: false`：请求根本不发，充值相关内容全部由渲染分支移除。
  const methodsQ = useQuery({ queryKey: ['user', 'payment-methods'], queryFn: () => userApi.listPaymentMethods(), enabled: payment })
  const methods = useMemo<PaymentMethod[]>(() => methodsQ.data ?? [], [methodsQ.data])
  // 失败按"零渠道"处理（mode = noChannels）：宁可给"暂时无法充值"说明卡，也不渲染一个
  // 渠道为空、点了必然失败的表单。零渠道与加载失败对用户是同一个结论。
  const channels: ChannelsState = payment && methodsQ.isLoading ? 'loading' : methods.length > 0 ? 'ready' : 'empty'
  const mode = walletMode({ payment, channels })
  const { main: mainTabs, records: recordTabs } = visibleTabs(mode)
  const canTopup = mainTabs.includes('topup')
  const canViewOrders = recordTabs.includes('orders')

  // —— Tab 状态：URL 是权威，本地只记住"最后一次点击"（切换 Tab 不清空另一侧的输入）——
  const [pickedTab, setPickedTab] = useState<WalletTab | null>(null)
  const orderForced = payment && focusOrderNo !== ''
  const tabs = activeTabs({ raw: rawTab, picked: pickedTab, mode, orderForced })
  const switchTab = (v: WalletTab) => {
    setPickedTab(v)
    const next = new URLSearchParams(searchParams)
    next.set('tab', v)
    setSearchParams(next, { replace: true })
  }

  // —— 充值：渠道 + 金额 ——
  // 用户改过渠道就认用户的选择；渠道列表还没到/该渠道消失时回落到第一个（不再用 effect 写入）。
  const [payType, setPayType] = useState('')
  const selected = useMemo(
    () => methods.find(m => m.pay_type === payType) ?? methods[0],
    [methods, payType],
  )
  const [amount, setAmount] = useState('')
  // 下单响应 = 本次报价的**权威值**（pay_amount/rate 都是订单快照）。跳转前必须让用户
  // 看清这个金额：methods.rate 只是参考值，不许拿它当"用户已确认的价格"（spec §5-3）。
  const [created, setCreated] = useState<CreateOrderResponse | null>(null)
  const overLimit = selected ? createWouldExceedLimit(amount, selected) : false
  const submitBlocked = !selected || !amountValid(amount) || overLimit
  const create = useMutation({
    mutationFn: (v: { amount_usd: string; pay_type: string }) => userApi.createOrder(v),
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
  // 支付关闭 / 零渠道时连订单请求都不发（"绝不请求/展示订单 UI"）。
  const ordersQ = useQuery({
    queryKey: ['user', 'orders', { page, page_size: pageSize, status: statusFilter }],
    queryFn: () =>
      userApi.listMyOrders({
        page,
        page_size: pageSize,
        status: statusFilter === 'all' ? undefined : statusFilter,
      }),
    enabled: payment && canViewOrders,
  })
  const orderRows = ordersQ.data?.rows ?? []
  // 末页死胡同守卫（与其它分页页同款）
  useEffect(() => {
    if (!ordersQ.isLoading && !ordersQ.isError && orderRows.length === 0 && page > 1) setPage(1)
  }, [ordersQ.isLoading, ordersQ.isError, orderRows.length, page])

  // —— 兑换（原 /user/redemptions，文案与行为不变）——
  const [code, setCode] = useState('')
  const [applied, setApplied] = useState<Applied | null>(null)
  const [redPage, setRedPage] = useState(1)
  const [redPageSize, setRedPageSize] = useState(20)
  const redemptionsQ = useQuery({
    queryKey: ['user', 'redemptions', { page: redPage, page_size: redPageSize }],
    queryFn: () => userApi.listUserRedemptions({ page: redPage, page_size: redPageSize }),
  })
  const redemptionRows = redemptionsQ.data?.rows ?? []
  // 末页死胡同守卫：非首页的当前页数据被清空（如兑换后 total 变化）时回退到第 1 页；
  // 页 1 本身为空（列表真正为空）时无需回退，不会成环。
  useEffect(() => {
    if (!redemptionsQ.isLoading && !redemptionsQ.isError && redemptionRows.length === 0 && redPage > 1) setRedPage(1)
  }, [redemptionsQ.isLoading, redemptionsQ.isError, redemptionRows.length, redPage])

  const redeem = useMutation({
    mutationFn: (c: string) => userApi.redeem(c),
    onSuccess: res => {
      setApplied(res.applied)
      setCode('')
      // 刷新记录 + 保持当前页（queryKey 前缀失效，当前页数据重新拉取）
      qc.invalidateQueries({ queryKey: ['user', 'redemptions'] })
      // 余额/并发上限/临时余额都可能变化 → 失效余额卡的两个数据源
      qc.invalidateQueries({ queryKey: ['user', 'me'] })
      qc.invalidateQueries({ queryKey: ['user', 'temp-balances'] })
      toast.add({ title: t('user.wallet.successTitle'), description: appliedText(res.applied, t), type: 'success' })
    },
    onError: (e: unknown) => {
      // 失败 toast 区分：400 = 无效码（不存在/失效/过期/用尽，服务端统一不泄露细节）；
      // 409 = 已兑换过；其余 = 网络/服务端错误（ApiError 的 message = 服务端 error 字段）。
      if (e instanceof ApiError && e.status === 400) {
        toast.add({ title: t('user.wallet.invalidCode'), type: 'error' })
      } else if (e instanceof ApiError && e.status === 409) {
        toast.add({ title: t('user.wallet.alreadyRedeemed'), type: 'error' })
      } else {
        toast.add({
          title: t('user.wallet.redeemFailed'),
          description: e instanceof ApiError ? e.message : t('user.common.error'),
          type: 'error',
        })
      }
    },
  })

  const submitRedeem = () => {
    const c = code.trim()
    if (!c || redeem.isPending) return
    setApplied(null) // 新一次兑换开始，清掉上次的成功回执
    redeem.mutate(c)
  }

  // —— ?order= 轮询 ——
  // 单号 → id：列表按 order_no 定位（订单号不是主键；新单在前，故首页 100 条足够，
  // 找不到时明确提示而不是空转）。支付关闭时整个上下文不存在（不请求、不轮询）。
  const locateQ = useQuery({
    queryKey: ['user', 'orders', 'locate', focusOrderNo],
    queryFn: () => userApi.listMyOrders({ page: 1, page_size: 100 }),
    enabled: orderForced,
  })
  const focusId = useMemo(
    () => locateQ.data?.rows.find(r => r.order_no === focusOrderNo)?.id ?? null,
    [locateQ.data, focusOrderNo],
  )
  // 2 分钟窗口：超时后停止轮询并给出"稍后手动刷新"的出口（不无限打服务端）
  const [pollExpired, setPollExpired] = useState(false)
  useEffect(() => {
    setPollExpired(false)
    if (!orderForced) return
    const timer = setTimeout(() => setPollExpired(true), POLL_WINDOW_MS)
    return () => clearTimeout(timer)
  }, [orderForced, focusOrderNo])

  const focusQ = useQuery({
    queryKey: ['user', 'order', focusId],
    queryFn: () => userApi.getMyOrder(focusId as number),
    enabled: orderForced && focusId != null,
    refetchInterval: q => {
      if (!orderForced || pollExpired) return false
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
  const estimated = selected ? estimatePay(amount, selected.rate) : null
  const limitHint = selected ? t('user.wallet.limitHint', { min: selected.min_usd, max: selected.max_usd }) : ''
  const balanceText = meQ.data?.Balance == null ? '—' : `$${meQ.data.Balance.toFixed(2)}`
  const concurrencyText =
    meQ.data?.MaxConcurrency == null
      ? '—'
      : meQ.data.MaxConcurrency === 0
        ? t('user.overview.unlimited')
        : String(meQ.data.MaxConcurrency)

  return (
    // 表单页：底纹与兑换码输入都是单行控件，超宽屏铺满会拉成 2000px 长条 → 只在大屏居中收窄
    <div className="mx-auto w-full space-y-6 @[2000px]/main:max-w-6xl">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">{t('user.wallet.title')}</h1>
      </div>

      {/* ① 余额与临时权益摘要：余额 / 并发上限 / 临时余额（含到期）；失败保留旧值 + 重试 */}
      <motion.div initial={{ opacity: 0, y: 12 }} animate={{ opacity: 1, y: 0 }} transition={{ duration: 0.25 }}>
        <Card>
          <CardContent className="flex flex-wrap items-start justify-between gap-4 py-5">
            <div className="flex flex-wrap items-center gap-x-8 gap-y-4">
              <div className="space-y-1">
                <p className="flex items-center gap-1.5 text-xs text-muted-foreground">
                  <Wallet className="size-3.5" /> {t('user.wallet.balanceTitle')}
                </p>
                {meQ.isLoading ? (
                  <Skeleton className="h-8 w-24" />
                ) : (
                  <p className="text-2xl font-semibold tabular-nums">{balanceText}</p>
                )}
              </div>
              <div className="space-y-1">
                <p className="flex items-center gap-1.5 text-xs text-muted-foreground">
                  <Zap className="size-3.5" /> {t('user.wallet.concurrencyLabel')}
                </p>
                {meQ.isLoading ? (
                  <Skeleton className="h-8 w-12" />
                ) : (
                  <p className="text-2xl font-semibold tabular-nums">{concurrencyText}</p>
                )}
              </div>
              {tempQ.isLoading ? (
                <div className="space-y-1">
                  <p className="flex items-center gap-1.5 text-xs text-muted-foreground">
                    <Timer className="size-3.5" /> {t('user.wallet.temporaryBalanceLabel')}
                  </p>
                  <Skeleton className="h-8 w-20" />
                </div>
              ) : tempTotal > 0 ? (
                <div className="space-y-1">
                  <p className="flex items-center gap-1.5 text-xs text-muted-foreground">
                    <Timer className="size-3.5" /> {t('user.wallet.temporaryBalanceLabel')}
                  </p>
                  <p className="text-2xl font-semibold tabular-nums">
                    ${tempTotal.toFixed(2)}
                    {tempExpiry && (
                      <span className="ml-2 text-xs font-normal text-muted-foreground">
                        {tempExpiry.kind === 'at'
                          ? `${t('user.wallet.expiresAtLabel')} ${formatDateTime(tempExpiry.iso)}`
                          : t('user.profile.tempPermanent')}
                      </span>
                    )}
                  </p>
                </div>
              ) : null}
            </div>
            {(meQ.isError || tempQ.isError) && (
              <Button
                type="button"
                variant="ghost"
                size="sm"
                onClick={() => {
                  void meQ.refetch()
                  void tempQ.refetch()
                }}
              >
                <RefreshCw /> {t('user.wallet.retry')}
              </Button>
            )}
          </CardContent>
        </Card>
      </motion.div>

      {/* 余额卡下方：兑换快捷入口（支付关闭时默认已在兑换，不再重复放一个） */}
      {showRedeemShortcut(mode) && (
        <div className="flex flex-wrap items-center justify-end gap-2">
          <Button type="button" variant="outline" size="sm" onClick={() => switchTab('redeem')}>
            <Ticket /> {t('user.wallet.redeemShortcut')}
          </Button>
        </div>
      )}

      {/* 支付回跳后的等待卡（?order= 进入时显示，支付关闭时不进入该上下文） */}
      {orderForced && (
        <motion.div initial={{ opacity: 0, y: 12 }} animate={{ opacity: 1, y: 0 }} transition={{ duration: 0.25 }}>
          <Card>
            <CardHeader>
              <CardTitle className="flex items-center justify-between gap-2">
                <span>{t('user.wallet.focusTitle')}</span>
                <Button type="button" variant="ghost" size="sm" onClick={clearFocus}>
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
                  <Button type="button" variant="outline" size="sm" onClick={() => { void focusQ.refetch(); void ordersQ.refetch() }}>
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
                <Button type="button" variant="outline" onClick={() => window.location.assign(focus.pay_url as string)}>
                  <ExternalLink /> {t('user.wallet.continuePay')}
                </Button>
              )}
            </CardContent>
          </Card>
        </motion.div>
      )}

      {/* ② 主任务区：支付开时充值占主视图，兑换是第二个 Tab；支付关只剩兑换 */}
      <motion.div
        initial={{ opacity: 0, y: 12 }}
        animate={{ opacity: 1, y: 0 }}
        transition={{ duration: 0.25, delay: 0.05 }}
        className="space-y-3"
      >
        {/* 支付开但零渠道：只给事实、原因与替代路径，不给可提交表单 */}
        {mode === 'noChannels' && (
          <Card>
            <CardContent className="flex flex-wrap items-center justify-between gap-3 py-5">
              <p className="text-sm text-muted-foreground">{t('user.wallet.noChannels')}</p>
              <Button type="button" variant="outline" onClick={() => switchTab('redeem')}>
                <Ticket /> {t('user.wallet.noChannelsAction')}
              </Button>
            </CardContent>
          </Card>
        )}

        <Tabs value={tabs.main} onValueChange={v => { if (isMainTab(v)) switchTab(v) }}>
          <TabsList>
            {canTopup && <TabsTrigger value="topup">{t('user.wallet.topupTab')}</TabsTrigger>}
            <TabsTrigger value="redeem">{t('user.wallet.redeemTab')}</TabsTrigger>
          </TabsList>

          {/* 充值：整块由 payment 守卫包住。支付关闭时 Tab、表单、渠道、金额、汇率、下单
              按钮全部不渲染——是渲染分支，不是 CSS 隐藏。 */}
          {payment && canTopup && (
            <TabsContent value="topup">
              <Card>
                <CardHeader>
                  <CardTitle className="flex items-center gap-2"><CreditCard className="size-4" /> {t('user.wallet.topupTitle')}</CardTitle>
                  <CardDescription>{t('user.wallet.topupDesc')}</CardDescription>
                </CardHeader>
                <CardContent className="space-y-4">
                  {channels === 'loading' ? (
                    <div className="space-y-2">
                      {Array.from({ length: 2 }).map((_, i) => <Skeleton key={i} className="h-10" />)}
                    </div>
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
                              if (e.key === 'Enter' && !submitBlocked && !create.isPending && selected) {
                                create.mutate({ amount_usd: amount.trim(), pay_type: selected.pay_type })
                              }
                            }}
                            disabled={create.isPending}
                          />
                          {limitHint && <p className="text-xs text-muted-foreground">{limitHint}</p>}
                        </div>
                        <div className="space-y-1.5">
                          <Label>{t('user.wallet.channelLabel')}</Label>
                          <Select
                            items={methodItems}
                            value={selected?.pay_type ?? ''}
                            onValueChange={setPayType}
                            disabled={create.isPending}
                          >
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
                          {selected && (
                            <p className="text-xs text-muted-foreground">
                              {t('user.wallet.rateReferenceLabel')} {selected.rate}
                            </p>
                          )}
                        </div>
                      </div>

                      {/* 预估实付：参考汇率 × 金额（spec §5-3 说它不是报价承诺）。范围与免责文案
                          属于"说明"而不是"决策"，已经从卡片里去掉——上限在输入框下有 limitHint，
                          权威金额在跳转前的确认层再展示一次。 */}
                      {selected && (
                        <div className="rounded-[10px] border border-border/60 bg-black/[0.02] p-3 text-sm dark:bg-white/[0.03]">
                          <div className="flex flex-wrap items-center gap-x-6 gap-y-1">
                            <span className="text-muted-foreground">
                              {t('user.wallet.estimatePayLabel')}{' '}
                              <span className="font-medium text-foreground">
                                {estimated ? money(estimated, selected.currency) : '—'}
                              </span>
                            </span>
                            <span className="text-muted-foreground">
                              {t('user.wallet.rateReferenceLabel')}{' '}
                              <span className="tabular-nums text-foreground">{selected.rate}</span>
                            </span>
                          </div>
                        </div>
                      )}

                      {amount.trim() !== '' && !amountValid(amount) && (
                        <p className="text-sm text-destructive">{t('user.wallet.amountInvalid')}</p>
                      )}
                      {selected && amountValid(amount) && overLimit && (
                        <p className="text-sm text-destructive">{limitHint}</p>
                      )}

                      <Button
                        type="button"
                        onClick={() => {
                          if (selected) create.mutate({ amount_usd: amount.trim(), pay_type: selected.pay_type })
                        }}
                        disabled={submitBlocked || create.isPending}
                      >
                        <CreditCard /> {create.isPending ? t('user.wallet.creating') : t('user.wallet.payNow')}
                      </Button>
                    </>
                  )}
                </CardContent>
              </Card>
            </TabsContent>
          )}

          {/* 兑换：不在任何支付守卫内——支付关闭时它就是主视图 */}
          <TabsContent value="redeem">
            <Card>
              <CardHeader>
                <CardTitle className="flex items-center gap-2"><Ticket className="size-4" /> {t('user.wallet.codeLabel')}</CardTitle>
                <CardDescription>{t('user.wallet.codeHint')}</CardDescription>
              </CardHeader>
              <CardContent className="space-y-3">
                <div className="flex gap-3">
                  <Input
                    value={code}
                    onChange={e => setCode(e.target.value)}
                    onKeyDown={e => { if (e.key === 'Enter') submitRedeem() }}
                    placeholder={t('user.wallet.codePlaceholder')}
                    aria-label={t('user.wallet.codeLabel')}
                    autoComplete="off"
                    spellCheck={false}
                    disabled={redeem.isPending}
                  />
                  <Button type="button" className="shrink-0" onClick={submitRedeem} disabled={!code.trim() || redeem.isPending}>
                    {redeem.isPending ? t('user.wallet.redeeming') : t('user.wallet.redeem')}
                  </Button>
                </div>
                {applied && (
                  <Alert>
                    <CircleCheck className="size-4 text-emerald-700 dark:text-emerald-400" />
                    <AlertTitle>{t('user.wallet.successTitle')}</AlertTitle>
                    <AlertDescription>{appliedText(applied, t)}</AlertDescription>
                  </Alert>
                )}
              </CardContent>
            </Card>
          </TabsContent>
        </Tabs>
      </motion.div>

      {/* ③ 记录区：订单 / 兑换记录各自分页（page/page_size 1-based） */}
      <motion.div
        initial={{ opacity: 0, y: 12 }}
        animate={{ opacity: 1, y: 0 }}
        transition={{ duration: 0.25, delay: 0.1 }}
        className="space-y-3"
      >
        <Tabs value={tabs.records} onValueChange={v => { if (isRecordTab(v)) switchTab(v) }}>
          <TabsList>
            {canViewOrders && <TabsTrigger value="orders">{t('user.wallet.ordersTab')}</TabsTrigger>}
            <TabsTrigger value="redemptions">{t('user.wallet.redemptionsTab')}</TabsTrigger>
          </TabsList>

          {/* 订单：同样只在 payment 守卫内渲染（也只在支付开且非零渠道时存在） */}
          {payment && canViewOrders && (
            <TabsContent value="orders">
              <div className="space-y-3">
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
                    <Button type="button" variant="ghost" size="lg" onClick={() => { setStatusFilter('all'); setPage(1) }}>
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
                ) : orderRows.length === 0 ? (
                  <Card className="flex flex-col items-center gap-2 py-12 text-muted-foreground">
                    <CreditCard className="size-10" />
                    <p className="font-medium">{t('user.wallet.emptyOrders')}</p>
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
                          {orderRows.map(r => (
                            <TableRow key={r.id} data-state={r.order_no === focusOrderNo ? 'selected' : undefined}>
                              <TableCell><code className="font-mono text-sm">{r.order_no}</code></TableCell>
                              <TableCell><OrderStatusBadge status={r.status} /></TableCell>
                              <TableCell className="tabular-nums">{r.amount_usd} USD</TableCell>
                              <TableCell className="tabular-nums">{money(r.pay_amount, r.currency)}</TableCell>
                              <TableCell>{formatDateTime(r.created_at)}</TableCell>
                              <TableCell>{r.paid_at ? formatDateTime(r.paid_at) : '—'}</TableCell>
                              <TableCell className="text-right">
                                {r.status === 'pending' && r.pay_url ? (
                                  <Button type="button" variant="outline" size="sm" onClick={() => window.location.assign(r.pay_url as string)}>
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
              </div>
            </TabsContent>
          )}

          <TabsContent value="redemptions">
            <div className="space-y-3">
              {redemptionsQ.isError ? (
                <p className="p-4 text-sm text-destructive">{t('common.loadFailed', { message: (redemptionsQ.error as Error).message })}</p>
              ) : redemptionsQ.isLoading ? (
                <div className="space-y-2 p-4">
                  {Array.from({ length: 4 }).map((_, i) => <Skeleton key={i} className="h-10" />)}
                </div>
              ) : redemptionRows.length === 0 ? (
                <div className="flex flex-col items-center gap-2 py-12 text-muted-foreground">
                  <Ticket className="size-10" />
                  <p className="font-medium">{t('user.wallet.emptyRedemptions')}</p>
                  <p className="text-sm">{t('user.wallet.emptyRedemptionsDesc')}</p>
                </div>
              ) : (
                <ScrollArea data-od-id="table-scroll-user-redemptions" className="rounded-[14px] border border-[rgba(19,45,83,0.26)] bg-[color:var(--glass-card-light)] shadow-[inset_0_1px_0_rgba(255,255,255,0.5),0_10px_36px_rgba(19,45,83,0.16)] backdrop-blur-[var(--glass-blur)] dark:bg-[color:var(--glass-card-dark)] dark:shadow-[inset_0_1px_0_rgba(255,255,255,0.07),0_10px_36px_rgba(2,6,14,0.5)] dark:border-[rgba(148,180,220,0.32)]" showHorizontal>
                  <Table className="min-w-[900px]" containerClassName="overflow-x-visible border-0 shadow-none rounded-none bg-transparent backdrop-blur-none">
                    <TableHeader>
                      <TableRow>
                        <TableHead>{t('user.wallet.table.code')}</TableHead>
                        <TableHead>{t('user.wallet.table.type')}</TableHead>
                        <TableHead>{t('user.wallet.table.value')}</TableHead>
                        <TableHead>{t('user.wallet.table.remark')}</TableHead>
                        <TableHead>{t('user.wallet.table.expiresAt')}</TableHead>
                        <TableHead>{t('user.wallet.table.redeemedAt')}</TableHead>
                      </TableRow>
                    </TableHeader>
                    <TableBody className="[&_td]:py-3">
                      {redemptionRows.map(r => (
                        <TableRow key={r.ID}>
                          <TableCell><code className="font-mono text-sm">{r.Code}</code></TableCell>
                          <TableCell>{t(`redemptions.type.${r.CodeType}`)}</TableCell>
                          <TableCell className="tabular-nums">{formatValue(r.CodeType, r.Value)}</TableCell>
                          <TableCell className="max-w-40 truncate" title={r.Remark ?? undefined}>{r.Remark || '—'}</TableCell>
                          <TableCell>{r.ResourceExpiresAt ? formatDateTime(r.ResourceExpiresAt) : '—'}</TableCell>
                          <TableCell>{formatDateTime(r.CreatedAt)}</TableCell>
                        </TableRow>
                      ))}
                    </TableBody>
                  </Table>
                </ScrollArea>
              )}
              {!redemptionsQ.isLoading && !redemptionsQ.isError && (
                <PagePagination
                  total={redemptionsQ.data?.total ?? 0}
                  pageSize={redPageSize}
                  page={redPage}
                  onPageChange={setRedPage}
                  onPageSizeChange={s => { setRedPageSize(s); setRedPage(1) }}
                />
              )}
            </div>
          </TabsContent>
        </Tabs>
      </motion.div>

      {/* 跳转前的金额确认（spec §5-3：pay_amount 是权威值）。同样在 payment 守卫内——
          支付被关掉时不留下任何此前的充值内容。 */}
      {payment && created && (
        <Dialog open onOpenChange={o => { if (!o) setCreated(null) }}>
          <DialogContent className="sm:max-w-md">
            <DialogHeader>
              <DialogTitle>{t('user.wallet.confirmTitle')}</DialogTitle>
              <DialogDescription>{t('user.wallet.confirmDesc')}</DialogDescription>
            </DialogHeader>
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
            <DialogFooter>
              <Button type="button" variant="outline" onClick={() => setCreated(null)}>
                {t('user.wallet.confirmCancel')}
              </Button>
              <Button type="button" onClick={() => window.location.assign(created.pay_url)}>
                <ExternalLink /> {t('user.wallet.confirmGo')}
              </Button>
            </DialogFooter>
          </DialogContent>
        </Dialog>
      )}
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
