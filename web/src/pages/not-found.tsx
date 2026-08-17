// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

import { Link } from 'react-router-dom'
import { motion } from 'framer-motion'
import { FileQuestion } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { buttonVariants } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { cn } from '@/lib/utils'

// 404 界面：路由 * 兜底（无匹配路径时渲染），替代 React Router 默认
// "Unexpected Application Error" 开发页——与 403 页同构（Card + 图标 + 返回入口）。
export default function NotFound() {
  const { t } = useTranslation()
  return (
    <div className="flex min-h-screen items-center justify-center bg-gradient-to-br from-muted via-background to-background">
      <motion.div initial={{ opacity: 0, y: 12 }} animate={{ opacity: 1, y: 0 }} transition={{ duration: 0.3 }}>
        <Card className="w-96">
          <CardHeader><CardTitle className="flex items-center gap-2"><FileQuestion className="h-5 w-5" /> {t('error404.title')}</CardTitle></CardHeader>
          <CardContent className="space-y-4">
            <p className="text-sm text-muted-foreground">{t('error404.desc')}</p>
            <Link to="/" className={cn(buttonVariants({ variant: 'outline' }), 'w-full')}>
              {t('error404.backHome')}
            </Link>
          </CardContent>
        </Card>
      </motion.div>
    </div>
  )
}
