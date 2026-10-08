// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 管理面结算资金命令的共享错误映射（spec 2026-10-09 §6.3）：陈旧 CAS → 409 提示 +
// 刷新结算列表；401 交给全局拦截；其余 toast 报错。供结算工作台与其动作对话框共用。
import { useQueryClient } from '@tanstack/react-query'
import { useTranslation } from 'react-i18next'
import { ApiError, ApiUnauthorized } from '@/lib/api/client'
import { toast } from '@/components/ui/toast'

export function useFundsErrHandler() {
  const { t } = useTranslation()
  const qc = useQueryClient()
  return (e: unknown) => {
    if (e instanceof ApiUnauthorized) return
    if (e instanceof ApiError && e.status === 409) {
      toast.add({ title: t('supplierAdmin.conflict'), type: 'error' })
      qc.invalidateQueries({ queryKey: ['admin-supplier-settlements'] })
      return
    }
    toast.add({ title: (e as Error)?.message ?? String(e), type: 'error' })
  }
}
