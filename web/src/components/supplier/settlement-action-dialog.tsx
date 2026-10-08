// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 管理面结算动作对话框（spec 2026-10-09 §6.3）：需要额外输入的资金命令
// （reject / claim / paid / confirm-failed）的字段状态、校验与提交都归本组件；
// 父组件只负责打开哪个动作与刷新列表。组件随 act 为 null 卸载 ⇒ 状态自然清理
// （无需成串 setter 复位）。approve（无输入）不在此列，仍由父组件单列。
import { useState } from 'react'
import { useMutation } from '@tanstack/react-query'
import { useTranslation } from 'react-i18next'
import { api } from '@/App'
import { ApiUnauthorized } from '@/lib/api/client'
import { useFundsErrHandler } from '@/lib/api/funds-error'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { formatQuotaMillis } from '@/components/fmt'
import type { components } from '@/lib/api/schema'

type SupplierSettlement = components['schemas']['SupplierSettlement']

// ActionKind 需要额外输入的资金命令。
export type ActionKind = 'reject' | 'claim' | 'paid' | 'confirm-failed'

export function SettlementActionDialog({
  act,
  onClose,
  onSuccess,
}: {
  act: { kind: ActionKind; row: SupplierSettlement }
  onClose: () => void
  onSuccess: () => void
}) {
  const { t } = useTranslation()
  const fundsErr = useFundsErrHandler()
  const [reason, setReason] = useState('')
  const [payeeName, setPayeeName] = useState('')
  const [payeeAccount, setPayeeAccount] = useState('')
  const [payeeUnit, setPayeeUnit] = useState('')
  const [risk, setRisk] = useState('')
  const [riskSummary, setRiskSummary] = useState('')
  const [evidence, setEvidence] = useState('')
  const [confirmedNotPaid, setConfirmedNotPaid] = useState(false)
  const [oldExecutionStopped, setOldExecutionStopped] = useState(false)
  const [ref, setRef] = useState('')

  const runAct = useMutation({
    mutationFn: async () => {
      const s = act.row
      const rev = { expected_revision: s.revision }
      switch (act.kind) {
        case 'reject': return api.rejectSettlement(s.id, { ...rev, ...(reason ? { reason } : {}) })
        case 'claim':
          if (!payeeName.trim() || !payeeAccount.trim() || !payeeUnit.trim() || !risk.trim() || !riskSummary.trim()) throw new Error(t('supplierAdmin.claim.required'))
          return api.claimSettlement(s.id, {
            ...rev,
            amount_millis: s.amount_millis,
            payee_snapshot: { payee_name: payeeName.trim(), account: payeeAccount.trim(), unit: payeeUnit.trim() },
            risk_evidence: { reference: risk.trim(), summary: riskSummary.trim(), approved_revision: s.revision },
          })
        case 'paid':
          if (!ref.trim()) throw new Error(t('supplierAdmin.paid.required'))
          return api.paidSettlement(s.id, { ...rev, external_ref: ref.trim() })
        case 'confirm-failed':
          if (!reason.trim() || !evidence.trim() || !confirmedNotPaid || !oldExecutionStopped) throw new Error(t('supplierAdmin.confirmFailed.required'))
          return api.confirmFailedSettlement(s.id, {
            ...rev,
            reason: reason.trim(),
            evidence: evidence.trim(),
            confirmed_not_paid: confirmedNotPaid,
            old_execution_stopped: oldExecutionStopped,
          })
      }
    },
    onSuccess: () => { onSuccess(); onClose() },
    onError: fundsErr,
  })

  return (
    <Dialog open onOpenChange={v => { if (!v) onClose() }}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>{t(`supplierAdmin.act.${act.kind === 'confirm-failed' ? 'confirmFailed' : act.kind}`)}</DialogTitle>
          <DialogDescription>{t('supplierAdmin.act.idDesc', { id: act.row.id, amount: formatQuotaMillis(act.row.amount_millis) })}</DialogDescription>
        </DialogHeader>
        <div className="space-y-3">
          {act.kind === 'reject' && (
            <div className="space-y-1.5">
              <Label htmlFor="act-reason">{t('supplierAdmin.field.reason')}</Label>
              <Input id="act-reason" value={reason} onChange={e => setReason(e.target.value)} />
            </div>
          )}
          {act.kind === 'claim' && (
            <>
              <div className="space-y-1.5">
                <Label htmlFor="act-payee-name">{t('supplierAdmin.field.payeeName')}</Label>
                <Input id="act-payee-name" value={payeeName} onChange={e => setPayeeName(e.target.value)} />
              </div>
              <div className="space-y-1.5">
                <Label htmlFor="act-payee-account">{t('supplierAdmin.field.payeeAccount')}</Label>
                <Input id="act-payee-account" value={payeeAccount} onChange={e => setPayeeAccount(e.target.value)} />
              </div>
              <div className="space-y-1.5">
                <Label htmlFor="act-payee-unit">{t('supplierAdmin.field.payeeUnit')}</Label>
                <Input id="act-payee-unit" value={payeeUnit} onChange={e => setPayeeUnit(e.target.value)} />
              </div>
              <div className="space-y-1.5">
                <Label htmlFor="act-risk">{t('supplierAdmin.field.risk')}</Label>
                <Input id="act-risk" value={risk} onChange={e => setRisk(e.target.value)} />
              </div>
              <div className="space-y-1.5">
                <Label htmlFor="act-risk-summary">{t('supplierAdmin.field.riskSummary')}</Label>
                <Input id="act-risk-summary" value={riskSummary} onChange={e => setRiskSummary(e.target.value)} />
                <p className="text-xs text-muted-foreground">{t('supplierAdmin.claim.hint')}</p>
              </div>
            </>
          )}
          {act.kind === 'confirm-failed' && (
            <>
              <div className="space-y-1.5">
                <Label htmlFor="act-reason">{t('supplierAdmin.field.reason')}</Label>
                <Input id="act-reason" value={reason} onChange={e => setReason(e.target.value)} />
              </div>
              <div className="space-y-1.5">
                <Label htmlFor="act-evidence">{t('supplierAdmin.field.evidence')}</Label>
                <Input id="act-evidence" value={evidence} onChange={e => setEvidence(e.target.value)} />
              </div>
              <label className="flex items-center gap-2 text-sm">
                <Checkbox checked={confirmedNotPaid} onCheckedChange={c => setConfirmedNotPaid(c === true)} />
                {t('supplierAdmin.field.confirmedNotPaid')}
              </label>
              <label className="flex items-center gap-2 text-sm">
                <Checkbox checked={oldExecutionStopped} onCheckedChange={c => setOldExecutionStopped(c === true)} />
                {t('supplierAdmin.field.oldExecutionStopped')}
              </label>
              <p className="text-xs text-muted-foreground">{t('supplierAdmin.confirmFailed.hint')}</p>
            </>
          )}
          {act.kind === 'paid' && (
            <div className="space-y-1.5">
              <Label htmlFor="act-ref">{t('supplierAdmin.field.externalRef')}</Label>
              <Input id="act-ref" value={ref} onChange={e => setRef(e.target.value)} />
            </div>
          )}
        </div>
        {runAct.isError && !(runAct.error instanceof ApiUnauthorized) && <p className="text-sm text-destructive">{(runAct.error as Error).message}</p>}
        <DialogFooter>
          <Button variant="outline" onClick={onClose}>{t('common.cancel')}</Button>
          <Button onClick={() => runAct.mutate()} disabled={runAct.isPending}>{t('supplierAdmin.act.submit')}</Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
