// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

import { useState } from 'react'
import { useTranslation } from 'react-i18next'
import { Trash2, X } from 'lucide-react'
import { Button } from '@/components/ui/button'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { toast } from '@/components/ui/toast'

// 列表页批量操作条：已选计数 + 批量删除（带确认弹窗）+ 可选批量更新 + 清除选择。
// onDelete/onUpdate 可为异步；resolve 后就地显示短暂成功反馈（batch.deleted / batch.updated），reject 时就地显示错误（2s 自动消失）。
// confirmTitle/confirmDesc/successTitle 用于覆盖「删除」语义（如批量失效场景），缺省保持默认文案。
export function BatchBar({
  selected,
  onClear,
  onDelete,
  onUpdate,
  deleteLabel,
  updateLabel,
  confirmTitle,
  confirmDesc,
  successTitle,
}: {
  selected: number[]
  onClear: () => void
  onDelete: () => void | Promise<void>
  onUpdate?: () => void | Promise<void | 'cancelled' | 'submitted'>
  deleteLabel?: string
  updateLabel?: string
  confirmTitle?: string
  confirmDesc?: string
  successTitle?: string
}) {
  const { t } = useTranslation()
  const [confirmOpen, setConfirmOpen] = useState(false)
  const [pending, setPending] = useState<'delete' | 'update' | null>(null)

  if (selected.length === 0) return null

  const count = selected.length

  function run(action: 'delete' | 'update', fn: () => void | Promise<void | 'cancelled' | 'submitted'>) {
    setPending(action)
    Promise.resolve(fn())
      .then((result) => {
        if (result === 'cancelled') return
        toast.add({
          title: successTitle ?? t(action === 'delete' ? 'batch.deleted' : 'batch.updated', { count }),
          type: 'success',
        })
      })
      .catch((err: unknown) => {
        toast.add({ title: err instanceof Error ? err.message : String(err), type: 'error' })
      })
      .finally(() => setPending(null))
  }

  return (
    <div className="flex flex-wrap items-center gap-2 rounded-lg border border-[rgba(19,45,83,0.12)] bg-[color:var(--glass-card-light)] px-3 py-2 shadow-[inset_0_1px_0_rgba(255,255,255,0.45)] backdrop-blur-[var(--glass-blur)] dark:border-[rgba(148,180,220,0.18)] dark:bg-[color:var(--glass-card-dark)] dark:shadow-[inset_0_1px_0_rgba(255,255,255,0.06)]">
      {(
        <>
          <span className="rounded-md border border-[rgba(19,45,83,0.08)] bg-primary/10 px-2 py-0.5 text-sm font-medium text-primary tabular-nums dark:border-[rgba(148,180,220,0.14)] dark:bg-primary/15 dark:text-primary">
            {t('list.selected', { count })}
          </span>
          <div className="ml-auto flex flex-wrap items-center gap-2">
            <Button
              variant="destructive"
              size="sm"
              disabled={pending !== null}
              onClick={() => setConfirmOpen(true)}
            >
              <Trash2 />
              {deleteLabel ?? t('list.batchDelete')}
            </Button>
            {onUpdate && (
              <Button
                variant="secondary"
                size="sm"
                disabled={pending !== null}
                onClick={() => run('update', onUpdate)}
              >
                {pending === 'update' ? t('common.saving') : updateLabel ?? t('list.batchUpdate')}
              </Button>
            )}
            <Button variant="ghost" size="sm" disabled={pending !== null} onClick={onClear}>
              <X />
              {t('list.clearSelection')}
            </Button>
          </div>
        </>
      )}
      <Dialog open={confirmOpen} onOpenChange={setConfirmOpen}>
        <DialogContent className="sm:max-w-sm">
          <DialogHeader>
            <DialogTitle>{confirmTitle ?? t('common.confirmDelete')}</DialogTitle>
            <DialogDescription>{confirmDesc ?? t('batch.confirmDelete', { count })}</DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" size="sm" onClick={() => setConfirmOpen(false)}>
              {t('common.cancel')}
            </Button>
            <Button
              variant="destructive"
              size="sm"
              disabled={pending === 'delete'}
              onClick={() => {
                setConfirmOpen(false)
                run('delete', onDelete)
              }}
            >
              {pending === 'delete' ? t('common.deleting') : deleteLabel ?? t('list.batchDelete')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
}
