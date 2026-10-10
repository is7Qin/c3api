// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 管理面结算工作台（spec 2026-10-09 §6.3）：结算单列表 + 五态 CAS 迁移
// （approve / reject 退还 / claim 认领 / paid 付款确认 / confirm-failed 回退）
// + 代申请（admin_request）+ 供应商余额列表与逐供应商配置（仅 share_bp / freeze_hours）。
// 全部走 adminApi（/api/admin/supplier/*）；资金命令要求具名 JWT（I5，后端强卡）。
import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Plus, RefreshCw } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { api } from '@/App'
import { ApiError, ApiUnauthorized } from '@/lib/api/client'
import { useFundsErrHandler } from '@/lib/api/funds-error'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { Skeleton } from '@/components/ui/skeleton'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { toast } from '@/components/ui/toast'
import { SettlementActionDialog, type ActionKind } from '@/components/supplier/settlement-action-dialog'
import { SupplierPicker } from '@/components/supplier/supplier-picker'
import { formatDateTime, formatQuotaMillis, parseQuotaUSD } from '@/components/fmt'
import type { components } from '@/lib/api/schema'

type SupplierSettlement = components['schemas']['SupplierSettlement']
type SettlementStatus = SupplierSettlement['status']
type SupplierBalance = components['schemas']['SupplierBalance']

const PAGE_SIZE = 20
const STATUSES: SettlementStatus[] = ['pending', 'approved', 'paying', 'paid', 'rejected']
const KINDS = ['supplier_request', 'admin_request'] as const

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

export default function SupplierAdmin() {
  const { t } = useTranslation()
  const qc = useQueryClient()

  const [status, setStatus] = useState<'all' | SettlementStatus>('pending')
  const [kind, setKind] = useState<'all' | (typeof KINDS)[number]>('all')
  const [offset, setOffset] = useState(0)

  const listQ = useQuery({
    queryKey: ['admin-supplier-settlements', status, kind, offset],
    queryFn: () => api.listAdminSupplierSettlements({ status: status === 'all' ? undefined : status, kind: kind === 'all' ? undefined : kind, limit: PAGE_SIZE, offset }),
    refetchInterval: 15_000,
  })

  const balancesQ = useQuery({
    queryKey: ['admin-supplier-balances'],
    queryFn: () => api.listSupplierBalances({ limit: 100 }),
  })

  const fundsErr = useFundsErrHandler()
  const afterMutate = (msg: string) => {
    qc.invalidateQueries({ queryKey: ['admin-supplier-settlements'] })
    qc.invalidateQueries({ queryKey: ['admin-supplier-balances'] })
    toast.add({ title: msg, type: 'success' })
  }

  const approve = useMutation({
    mutationFn: (s: SupplierSettlement) => api.approveSettlement(s.id, { expected_revision: s.revision }),
    onSuccess: () => afterMutate(t('supplierAdmin.approveSuccess')),
    onError: fundsErr,
  })

  // —— 带输入的资金命令：打开哪个动作交给对话框；字段状态/校验/提交归对话框 ——
  const [act, setAct] = useState<{ kind: ActionKind; row: SupplierSettlement } | null>(null)
  const openAct = (kind: ActionKind, row: SupplierSettlement) => setAct({ kind, row })

  // —— 代申请（admin_request）——
  const [reqOpen, setReqOpen] = useState(false)
  const [reqUid, setReqUid] = useState('')
  const [reqAmount, setReqAmount] = useState('')
  const [reqNote, setReqNote] = useState('')
  const [reqKey, setReqKey] = useState('')
  const openReq = () => {
    setReqUid(''); setReqAmount(''); setReqNote(''); setReqKey(crypto.randomUUID())
    setReqOpen(true)
  }
  const adminRequest = useMutation({
    mutationFn: async () => {
      const millis = parseQuotaUSD(reqAmount)
      if (!reqUid) throw new Error(t('supplierAdmin.request.requiredUid'))
      if (millis == null || millis <= 0) throw new Error(t('supplierAdmin.request.invalidAmount'))
      return api.adminRequestSettlement({ supplier_user_id: Number(reqUid.trim()), amount_millis: millis, request_key: reqKey, ...(reqNote ? { note: reqNote } : {}) })
    },
    onSuccess: () => { afterMutate(t('supplierAdmin.request.success')); setReqOpen(false) },
    onError: (e) => {
      if (e instanceof ApiUnauthorized) return
      if (e instanceof ApiError && e.status === 404) {
        toast.add({ title: t('supplierAdmin.request.notFound'), type: 'error' })
        return
      }
      toast.add({ title: (e as Error)?.message ?? String(e), type: 'error' })
    },
  })

  // —— 余额：PATCH 仅 share_bp / freeze_hours ——
  const [bal, setBal] = useState<SupplierBalance | null>(null)
  const [balShare, setBalShare] = useState('')
  const [balFreeze, setBalFreeze] = useState('')
  const openBal = (b: SupplierBalance) => {
    setBalShare(b.share_bp == null ? '' : String(b.share_bp))
    setBalFreeze(b.freeze_hours == null ? '' : String(b.freeze_hours))
    setBal(b)
  }
  const patchBal = useMutation({
    mutationFn: async () => {
      const body: components['schemas']['SupplierBalancePatchBody'] = {}
      if (balShare.trim() !== '') {
        if (!/^\d+$/.test(balShare.trim())) throw new Error(t('supplierAdmin.balance.invalid'))
        body.share_bp = Number(balShare.trim())
      }
      if (balFreeze.trim() !== '') {
        if (!/^\d+$/.test(balFreeze.trim())) throw new Error(t('supplierAdmin.balance.invalid'))
        body.freeze_hours = Number(balFreeze.trim())
      }
      return api.patchSupplierBalance(bal!.supplier_user_id, body)
    },
    onSuccess: () => { qc.invalidateQueries({ queryKey: ['admin-supplier-balances'] }); setBal(null); toast.add({ title: t('supplierAdmin.balance.success'), type: 'success' }) },
    onError: fundsErr,
  })

  const rows = listQ.data?.items ?? []
  const total = listQ.data?.total ?? 0

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-semibold tracking-tight">{t('supplierAdmin.title')}</h1>
          <p className="text-sm text-muted-foreground">{t('supplierAdmin.subtitle')}</p>
        </div>
        <Button onClick={openReq}><Plus /> {t('supplierAdmin.request.button')}</Button>
      </div>

      <Tabs defaultValue="settlements">
        <TabsList>
          <TabsTrigger value="settlements">{t('supplierAdmin.tab.settlements')}</TabsTrigger>
          <TabsTrigger value="balances">{t('supplierAdmin.tab.balances')}</TabsTrigger>
        </TabsList>

        <TabsContent value="settlements">
          <div className="mb-3 flex items-center gap-2">
            <Select items={{ all: t('supplierAdmin.filter.allStatus'), ...Object.fromEntries(STATUSES.map(s => [s, t(`supplier.settlement.status.${s}`)])) }} value={status} onValueChange={v => { setStatus(v as typeof status); setOffset(0) }}>
              <SelectTrigger size="default" className="w-40 data-[size=default]:h-9"><SelectValue /></SelectTrigger>
              <SelectContent>
                <SelectItem value="all" label={t('supplierAdmin.filter.allStatus')}>{t('supplierAdmin.filter.allStatus')}</SelectItem>
                {STATUSES.map(s => <SelectItem key={s} value={s} label={t(`supplier.settlement.status.${s}`)}>{t(`supplier.settlement.status.${s}`)}</SelectItem>)}
              </SelectContent>
            </Select>
            <Select items={{ all: t('supplierAdmin.filter.allKind'), supplier_request: t('supplier.settlement.kind.supplier_request'), admin_request: t('supplier.settlement.kind.admin_request') }} value={kind} onValueChange={v => { setKind(v as typeof kind); setOffset(0) }}>
              <SelectTrigger size="default" className="w-44 data-[size=default]:h-9"><SelectValue /></SelectTrigger>
              <SelectContent>
                <SelectItem value="all" label={t('supplierAdmin.filter.allKind')}>{t('supplierAdmin.filter.allKind')}</SelectItem>
                {KINDS.map(k => <SelectItem key={k} value={k} label={t(`supplier.settlement.kind.${k}`)}>{t(`supplier.settlement.kind.${k}`)}</SelectItem>)}
              </SelectContent>
            </Select>
            <Button variant="outline" size="sm" onClick={() => qc.invalidateQueries({ queryKey: ['admin-supplier-settlements'] })}><RefreshCw className="size-4" />{t('supplierAdmin.refresh')}</Button>
          </div>

          <Card>
            <CardContent className="pt-4">
              {listQ.isLoading ? (
                <Skeleton className="h-48" />
              ) : rows.length === 0 ? (
                <p className="py-8 text-center text-sm text-muted-foreground">{t('supplierAdmin.settlements.empty')}</p>
              ) : (
                <Table>
                  <TableHeader>
                    <TableRow>
                      <TableHead>ID</TableHead>
                      <TableHead>{t('supplierAdmin.col.supplier')}</TableHead>
                      <TableHead className="text-right">{t('supplierAdmin.col.amount')}</TableHead>
                      <TableHead>{t('supplierAdmin.col.status')}</TableHead>
                      <TableHead>{t('supplierAdmin.col.kind')}</TableHead>
                      <TableHead>{t('supplierAdmin.col.requestedAt')}</TableHead>
                      <TableHead className="text-right">{t('supplierAdmin.col.actions')}</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {rows.map(s => (
                      <TableRow key={s.id}>
                        <TableCell className="tabular-nums">{s.id}</TableCell>
                        <TableCell className="tabular-nums">#{s.supplier_user_id}</TableCell>
                        <TableCell className="text-right font-medium tabular-nums">{formatQuotaMillis(s.amount_millis)}</TableCell>
                        <TableCell><StatusBadge status={s.status} /></TableCell>
                        <TableCell>{t(`supplier.settlement.kind.${s.kind}`)}</TableCell>
                        <TableCell className="whitespace-nowrap text-muted-foreground">{formatDateTime(s.requested_at)}</TableCell>
                        <TableCell>
                          <div className="flex justify-end gap-1">
                            {s.status === 'pending' && <Button size="sm" variant="outline" onClick={() => approve.mutate(s)} disabled={approve.isPending}>{t('supplierAdmin.act.approve')}</Button>}
                            {(s.status === 'pending' || s.status === 'approved') && <Button size="sm" variant="outline" onClick={() => openAct('reject', s)}>{t('supplierAdmin.act.reject')}</Button>}
                            {s.status === 'approved' && <Button size="sm" variant="outline" onClick={() => openAct('claim', s)}>{t('supplierAdmin.act.claim')}</Button>}
                            {s.status === 'paying' && <Button size="sm" onClick={() => openAct('paid', s)}>{t('supplierAdmin.act.paid')}</Button>}
                            {s.status === 'paying' && <Button size="sm" variant="outline" onClick={() => openAct('confirm-failed', s)}>{t('supplierAdmin.act.confirmFailed')}</Button>}
                          </div>
                        </TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              )}
              <div className="mt-3 flex items-center justify-end gap-2 text-sm text-muted-foreground">
                <span>{t('supplier.pager', { from: total === 0 ? 0 : offset + 1, to: Math.min(offset + PAGE_SIZE, total), total })}</span>
                <Button variant="outline" size="sm" disabled={offset <= 0} onClick={() => setOffset(Math.max(0, offset - PAGE_SIZE))}>{t('supplier.prev')}</Button>
                <Button variant="outline" size="sm" disabled={offset + PAGE_SIZE >= total} onClick={() => setOffset(offset + PAGE_SIZE)}>{t('supplier.next')}</Button>
              </div>
            </CardContent>
          </Card>
        </TabsContent>

        <TabsContent value="balances">
          <Card>
            <CardContent className="pt-4">
              {balancesQ.isLoading ? (
                <Skeleton className="h-48" />
              ) : (balancesQ.data?.items?.length ?? 0) === 0 ? (
                <p className="py-8 text-center text-sm text-muted-foreground">{t('supplierAdmin.balances.empty')}</p>
              ) : (
                <Table>
                  <TableHeader>
                    <TableRow>
                      <TableHead>{t('supplierAdmin.col.supplier')}</TableHead>
                      <TableHead className="text-right">{t('supplierAdmin.col.available')}</TableHead>
                      <TableHead className="text-right">{t('supplierAdmin.col.lifetimeCredited')}</TableHead>
                      <TableHead className="text-right">{t('supplierAdmin.col.lifetimePaid')}</TableHead>
                      <TableHead className="text-right">{t('supplierAdmin.col.shareBp')}</TableHead>
                      <TableHead className="text-right">{t('supplierAdmin.col.freezeHours')}</TableHead>
                      <TableHead className="text-right">{t('supplierAdmin.col.buckets')}</TableHead>
                      <TableHead className="text-right">{t('supplierAdmin.col.actions')}</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {balancesQ.data!.items.map(b => (
                      <TableRow key={b.supplier_user_id}>
                        <TableCell className="tabular-nums">#{b.supplier_user_id}</TableCell>
                        <TableCell className="text-right tabular-nums">{formatQuotaMillis(b.available)}</TableCell>
                        <TableCell className="text-right tabular-nums">{formatQuotaMillis(b.lifetime_credited)}</TableCell>
                        <TableCell className="text-right tabular-nums">{formatQuotaMillis(b.lifetime_paid)}</TableCell>
                        <TableCell className="text-right tabular-nums">{b.share_bp == null ? t('supplierAdmin.inherit') : `${(b.share_bp / 100).toFixed(2)}%`}</TableCell>
                        <TableCell className="text-right tabular-nums">{b.freeze_hours == null ? t('supplierAdmin.inherit') : b.freeze_hours}</TableCell>
                        <TableCell className="text-right tabular-nums" title={b.latest_available_at ? formatDateTime(b.latest_available_at) : undefined}>{b.bucket_rows}</TableCell>
                        <TableCell className="text-right"><Button size="sm" variant="outline" onClick={() => openBal(b)}>{t('supplierAdmin.balance.edit')}</Button></TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              )}
            </CardContent>
          </Card>
        </TabsContent>
      </Tabs>

      {/* 带输入的资金命令对话框（状态/校验/提交归对话框；act=null 即卸载 ⇒ 状态自然清理） */}
      {act && (
        <SettlementActionDialog
          act={act}
          onClose={() => setAct(null)}
          onSuccess={() => afterMutate(t('supplierAdmin.actionSuccess'))}
        />
      )}

      {/* 代申请 */}
      <Dialog open={reqOpen} onOpenChange={setReqOpen}>
        <DialogContent className="sm:max-w-md">
          <DialogHeader>
            <DialogTitle>{t('supplierAdmin.request.title')}</DialogTitle>
            <DialogDescription>{t('supplierAdmin.request.desc')}</DialogDescription>
          </DialogHeader>
          <div className="space-y-3">
            <div className="space-y-1.5">
              <Label htmlFor="req-uid">{t('supplierAdmin.request.uid')}</Label>
              <SupplierPicker id="req-uid" value={reqUid} onChange={setReqUid} placeholder={t('supplierPicker.placeholder')} />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="req-amount">{t('supplierAdmin.request.amount')}</Label>
              <Input id="req-amount" inputMode="decimal" value={reqAmount} placeholder="1.00000" onChange={e => setReqAmount(e.target.value)} />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="req-note">{t('supplierAdmin.request.note')}</Label>
              <Input id="req-note" value={reqNote} onChange={e => setReqNote(e.target.value)} />
            </div>
          </div>
          {adminRequest.isError && !(adminRequest.error instanceof ApiUnauthorized) && <p className="text-sm text-destructive">{(adminRequest.error as Error).message}</p>}
          <DialogFooter>
            <Button variant="outline" onClick={() => setReqOpen(false)}>{t('common.cancel')}</Button>
            <Button onClick={() => adminRequest.mutate()} disabled={adminRequest.isPending || !reqUid}>{t('supplierAdmin.act.submit')}</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* 余额配置（仅 share_bp / freeze_hours） */}
      <Dialog open={!!bal} onOpenChange={v => { if (!v) setBal(null) }}>
        <DialogContent className="sm:max-w-md">
          <DialogHeader>
            <DialogTitle>{t('supplierAdmin.balance.title', { uid: bal?.supplier_user_id })}</DialogTitle>
            <DialogDescription>{t('supplierAdmin.balance.desc')}</DialogDescription>
          </DialogHeader>
          <div className="space-y-3">
            <div className="space-y-1.5">
              <Label htmlFor="bal-share">{t('supplierAdmin.balance.shareBp')}</Label>
              <Input id="bal-share" inputMode="numeric" value={balShare} placeholder={t('supplierAdmin.inheritPlaceholder')} onChange={e => setBalShare(e.target.value)} />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="bal-freeze">{t('supplierAdmin.balance.freezeHours')}</Label>
              <Input id="bal-freeze" inputMode="numeric" value={balFreeze} placeholder={t('supplierAdmin.inheritPlaceholder')} onChange={e => setBalFreeze(e.target.value)} />
            </div>
          </div>
          {patchBal.isError && !(patchBal.error instanceof ApiUnauthorized) && <p className="text-sm text-destructive">{(patchBal.error as Error).message}</p>}
          <DialogFooter>
            <Button variant="outline" onClick={() => setBal(null)}>{t('common.cancel')}</Button>
            <Button onClick={() => patchBal.mutate()} disabled={patchBal.isPending}>{t('common.save')}</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
}
