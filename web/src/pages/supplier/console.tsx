// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 供应商控制台（spec 2026-10-09 §6.1）：概览（可提领/冻结/累计/生效分成率与冻结小时/
// 最晚解冻时刻/桶行数）+ 收益明细 + 冻结中明细 + 结算单 + 申请结算。
// 端点全部走 supplierApi（/api/user/supplier/*，JWT 作用域=本人）；申请结算带
// 稳定 request_key（I3：重试复用，避免「提交成功但响应丢失」双单）。
import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Coins, Snowflake, Wallet, TrendingUp, Plus } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { useScopedApi } from '@/lib/api/scope'
import { ApiUnauthorized } from '@/lib/api/client'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Skeleton } from '@/components/ui/skeleton'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { toast } from '@/components/ui/toast'
import { formatDateTime, formatQuotaMillis, parseQuotaUSD } from '@/components/fmt'
import type { components } from '@/lib/api/schema'

type SupplierSettlement = components['schemas']['SupplierSettlement']
type SettlementStatus = SupplierSettlement['status']

const PAGE_SIZE = 20

// 结算五态徽章：pending/approved/paying 为在途（不同色调），paid/rejected 为终态。
const STATUS_VARIANT: Record<SettlementStatus, 'default' | 'secondary' | 'destructive' | 'outline'> = {
  pending: 'secondary',
  approved: 'default',
  paying: 'outline',
  paid: 'default',
  rejected: 'destructive',
}

function StatusBadge({ status }: { status: SettlementStatus }) {
  const { t } = useTranslation()
  return <Badge variant={STATUS_VARIANT[status]}>{t(`supplier.settlement.status.${status}`)}</Badge>
}

// 概览卡：标签 + 主值（金额卡用等宽/较大字号）
function StatCard({ icon: Icon, label, value, hint }: { icon: typeof Coins; label: string; value: string; hint?: string }) {
  return (
    <Card>
      <CardHeader className="flex flex-row items-center justify-between gap-2 pb-2">
        <CardTitle className="text-sm font-medium text-muted-foreground">{label}</CardTitle>
        <Icon className="size-4 text-muted-foreground" />
      </CardHeader>
      <CardContent>
        <div className="text-2xl font-semibold tabular-nums tracking-tight">{value}</div>
        {hint && <p className="mt-1 text-xs text-muted-foreground">{hint}</p>}
      </CardContent>
    </Card>
  )
}

export default function SupplierConsole() {
  const api = useScopedApi()
  const { t } = useTranslation()
  const qc = useQueryClient()

  const overviewQ = useQuery({
    queryKey: ['supplier', 'overview'],
    queryFn: () => api.getSupplierOverview(),
    refetchInterval: 15_000,
  })
  const ov = overviewQ.data

  const [earningsOffset, setEarningsOffset] = useState(0)
  const [chunksOffset, setChunksOffset] = useState(0)
  const [settleOffset, setSettleOffset] = useState(0)

  const earningsQ = useQuery({
    queryKey: ['supplier', 'earnings', earningsOffset],
    queryFn: () => api.getSupplierEarnings({ limit: PAGE_SIZE, offset: earningsOffset }),
  })
  const chunksQ = useQuery({
    queryKey: ['supplier', 'chunks', chunksOffset],
    queryFn: () => api.getSupplierChunks({ limit: PAGE_SIZE, offset: chunksOffset }),
  })
  const settlementsQ = useQuery({
    queryKey: ['supplier', 'settlements', settleOffset],
    queryFn: () => api.getSupplierSettlements({ limit: PAGE_SIZE, offset: settleOffset }),
  })

  // —— 申请结算 ——（稳定 request_key：对话框打开时生成，重试复用同一 key）
  const [applyOpen, setApplyOpen] = useState(false)
  const [applyAmount, setApplyAmount] = useState('')
  const [applyKey, setApplyKey] = useState('')
  const [applyNote, setApplyNote] = useState('')
  const openApply = () => {
    setApplyAmount('')
    setApplyNote('')
    setApplyKey(crypto.randomUUID())
    setApplyOpen(true)
  }
  const apply = useMutation({
    mutationFn: async () => {
      const millis = parseQuotaUSD(applyAmount)
      if (millis == null || millis <= 0) throw new Error(t('supplier.apply.invalidAmount'))
      return api.applySupplierSettlement({ amount_millis: millis, request_key: applyKey, ...(applyNote ? { note: applyNote } : {}) })
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['supplier'] })
      setApplyOpen(false)
      toast.add({ title: t('supplier.apply.success'), type: 'success' })
    },
    onError: (e) => {
      if (e instanceof ApiUnauthorized) return
      toast.add({ title: (e as Error)?.message ?? String(e), type: 'error' })
    },
  })

  const pager = (offset: number, total: number, setOffset: (n: number) => void) => (
    <div className="mt-3 flex items-center justify-end gap-2 text-sm text-muted-foreground">
      <span>{t('supplier.pager', { from: total === 0 ? 0 : offset + 1, to: Math.min(offset + PAGE_SIZE, total), total })}</span>
      <Button variant="outline" size="sm" disabled={offset <= 0} onClick={() => setOffset(Math.max(0, offset - PAGE_SIZE))}>
        {t('supplier.prev')}
      </Button>
      <Button variant="outline" size="sm" disabled={offset + PAGE_SIZE >= total} onClick={() => setOffset(offset + PAGE_SIZE)}>
        {t('supplier.next')}
      </Button>
    </div>
  )

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-semibold tracking-tight">{t('supplier.title')}</h1>
          <p className="text-sm text-muted-foreground">{t('supplier.subtitle')}</p>
        </div>
        <Button onClick={openApply} disabled={!ov || ov.available <= 0}>
          <Plus /> {t('supplier.apply.button')}
        </Button>
      </div>

      {overviewQ.isError ? (
        <p className="text-sm text-destructive">{t('common.loadFailed', { message: (overviewQ.error as Error).message })}</p>
      ) : !ov ? (
        <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
          {Array.from({ length: 4 }).map((_, i) => <Skeleton key={i} className="h-28" />)}
        </div>
      ) : (
        <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
          <StatCard icon={Wallet} label={t('supplier.card.available')} value={formatQuotaMillis(ov.available)} />
          <StatCard icon={Snowflake} label={t('supplier.card.frozen')} value={formatQuotaMillis(ov.frozen_amount)} hint={t('supplier.card.frozenHint', { rows: ov.bucket_rows, at: ov.latest_available_at ? formatDateTime(ov.latest_available_at) : t('supplier.card.noBucket') })} />
          <StatCard icon={TrendingUp} label={t('supplier.card.lifetimeCredited')} value={formatQuotaMillis(ov.lifetime_credited)} />
          <StatCard icon={Coins} label={t('supplier.card.lifetimePaid')} value={formatQuotaMillis(ov.lifetime_paid)} />
          <StatCard icon={Coins} label={t('supplier.card.shareBp')} value={`${(ov.share_bp / 100).toFixed(2)}%`} hint={t('supplier.card.shareBpHint', { bp: ov.share_bp })} />
          <StatCard icon={Snowflake} label={t('supplier.card.freezeHours')} value={ov.freeze_hours === 0 ? t('supplier.card.noFreeze') : `${ov.freeze_hours} h`} />
        </div>
      )}

      <Tabs defaultValue="earnings">
        <TabsList>
          <TabsTrigger value="earnings">{t('supplier.tab.earnings')}</TabsTrigger>
          <TabsTrigger value="chunks">{t('supplier.tab.chunks')}</TabsTrigger>
          <TabsTrigger value="settlements">{t('supplier.tab.settlements')}</TabsTrigger>
        </TabsList>

        <TabsContent value="earnings">
          <Card>
            <CardContent className="pt-4">
              {earningsQ.isLoading ? (
                <Skeleton className="h-40" />
              ) : (earningsQ.data?.items?.length ?? 0) === 0 ? (
                <p className="py-8 text-center text-sm text-muted-foreground">{t('supplier.earnings.empty')}</p>
              ) : (
                <Table>
                  <TableHeader>
                    <TableRow>
                      <TableHead>{t('supplier.earnings.col.time')}</TableHead>
                      <TableHead>{t('supplier.earnings.col.model')}</TableHead>
                      <TableHead className="text-right">{t('supplier.earnings.col.cost')}</TableHead>
                      <TableHead className="text-right">{t('supplier.earnings.col.earn')}</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {earningsQ.data!.items.map((e, i) => (
                      <TableRow key={`${e.created_at}-${i}`}>
                        <TableCell className="whitespace-nowrap text-muted-foreground">{formatDateTime(e.created_at)}</TableCell>
                        <TableCell className="max-w-56 truncate" title={e.model}>{e.model || '—'}</TableCell>
                        <TableCell className="text-right tabular-nums">{formatQuotaMillis(e.cost)}</TableCell>
                        <TableCell className="text-right font-medium tabular-nums">{formatQuotaMillis(e.earn_millis)}</TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              )}
              {pager(earningsOffset, earningsQ.data?.total ?? 0, setEarningsOffset)}
            </CardContent>
          </Card>
        </TabsContent>

        <TabsContent value="chunks">
          <Card>
            <CardContent className="pt-4">
              {chunksQ.isLoading ? (
                <Skeleton className="h-40" />
              ) : (chunksQ.data?.items?.length ?? 0) === 0 ? (
                <p className="py-8 text-center text-sm text-muted-foreground">{t('supplier.chunks.empty')}</p>
              ) : (
                <Table>
                  <TableHeader>
                    <TableRow>
                      <TableHead>{t('supplier.chunks.col.availableAt')}</TableHead>
                      <TableHead className="text-right">{t('supplier.chunks.col.amount')}</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {chunksQ.data!.items.map((c, i) => (
                      <TableRow key={`${c.available_at}-${i}`}>
                        <TableCell className="whitespace-nowrap">{formatDateTime(c.available_at)}</TableCell>
                        <TableCell className="text-right font-medium tabular-nums">{formatQuotaMillis(c.amount)}</TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              )}
              {pager(chunksOffset, chunksQ.data?.total ?? 0, setChunksOffset)}
            </CardContent>
          </Card>
        </TabsContent>

        <TabsContent value="settlements">
          <Card>
            <CardContent className="pt-4">
              {settlementsQ.isLoading ? (
                <Skeleton className="h-40" />
              ) : (settlementsQ.data?.items?.length ?? 0) === 0 ? (
                <p className="py-8 text-center text-sm text-muted-foreground">{t('supplier.settlements.empty')}</p>
              ) : (
                <Table>
                  <TableHeader>
                    <TableRow>
                      <TableHead>ID</TableHead>
                      <TableHead className="text-right">{t('supplier.settlements.col.amount')}</TableHead>
                      <TableHead>{t('supplier.settlements.col.status')}</TableHead>
                      <TableHead>{t('supplier.settlements.col.kind')}</TableHead>
                      <TableHead>{t('supplier.settlements.col.period')}</TableHead>
                      <TableHead>{t('supplier.settlements.col.requestedAt')}</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {settlementsQ.data!.items.map((s) => (
                      <TableRow key={s.id}>
                        <TableCell className="tabular-nums">{s.id}</TableCell>
                        <TableCell className="text-right font-medium tabular-nums">{formatQuotaMillis(s.amount_millis)}</TableCell>
                        <TableCell><StatusBadge status={s.status} /></TableCell>
                        <TableCell>{t(`supplier.settlement.kind.${s.kind}`)}</TableCell>
                        <TableCell className="whitespace-nowrap text-muted-foreground">{formatDateTime(s.period_start)} – {formatDateTime(s.period_end)}</TableCell>
                        <TableCell className="whitespace-nowrap text-muted-foreground">{formatDateTime(s.requested_at)}</TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              )}
              {pager(settleOffset, settlementsQ.data?.total ?? 0, setSettleOffset)}
            </CardContent>
          </Card>
        </TabsContent>
      </Tabs>

      <Dialog open={applyOpen} onOpenChange={setApplyOpen}>
        <DialogContent className="sm:max-w-md">
          <DialogHeader>
            <DialogTitle>{t('supplier.apply.title')}</DialogTitle>
            <DialogDescription>{t('supplier.apply.desc', { available: ov ? formatQuotaMillis(ov.available) : '—' })}</DialogDescription>
          </DialogHeader>
          <div className="space-y-3">
            <div className="space-y-1.5">
              <Label htmlFor="apply-amount">{t('supplier.apply.amount')}</Label>
              <Input id="apply-amount" inputMode="decimal" value={applyAmount} placeholder="1.00000" onChange={e => setApplyAmount(e.target.value)} />
              <p className="text-xs text-muted-foreground">{t('supplier.apply.amountHint')}</p>
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="apply-note">{t('supplier.apply.note')}</Label>
              <Input id="apply-note" value={applyNote} onChange={e => setApplyNote(e.target.value)} />
            </div>
          </div>
          {apply.isError && !(apply.error instanceof ApiUnauthorized) && (
            <p className="text-sm text-destructive">{(apply.error as Error).message}</p>
          )}
          <DialogFooter>
            <Button variant="outline" onClick={() => setApplyOpen(false)}>{t('common.cancel')}</Button>
            <Button onClick={() => apply.mutate()} disabled={apply.isPending}>{t('supplier.apply.submit')}</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
}
