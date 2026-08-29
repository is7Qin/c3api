// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

import { Construction } from 'lucide-react'
import { useTranslation } from 'react-i18next'

// 占位：实现各页面时整体替换。
export function Placeholder({ title }: { title: string }) {
  const { t } = useTranslation()
  return (
    <div className="flex min-h-[60vh] items-center justify-center">
      <div className="space-y-2 text-center text-muted-foreground">
        <Construction className="mx-auto h-10 w-10" />
        <p className="text-lg font-medium">{title}</p>
        <p className="text-sm">{t('placeholder.underConstruction')}</p>
      </div>
    </div>
  )
}
