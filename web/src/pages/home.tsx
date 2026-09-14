// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

import { Link } from 'react-router-dom'
import { motion } from 'framer-motion'
import { ArrowRight, BarChart3, Coins, Network, Ticket } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { ModeToggle } from '@/components/mode-toggle'
import { setLang, type AppLang } from '@/lib/i18n'
import { cn } from '@/lib/utils'

const LANGS: { code: AppLang; label: string }[] = [
  { code: 'zh-CN', label: '中' },
  { code: 'en', label: 'EN' },
]

const FEATURES = [
  { key: 'feature1', icon: Network },
  { key: 'feature2', icon: Coins },
  { key: 'feature3', icon: BarChart3 },
  { key: 'feature4', icon: Ticket },
] as const

// 首页 landing（登录前展示）：品牌 + 标语 + 功能卡片 + 单一登录入口。
export default function Home() {
  const { t, i18n } = useTranslation()
  const lang: AppLang = i18n.resolvedLanguage?.startsWith('zh') ? 'zh-CN' : 'en'
  return (
    <div className="flex min-h-screen flex-col items-center bg-gradient-to-br from-muted via-background to-background">
      <div className="absolute right-4 top-4 flex items-center gap-2">
        <ModeToggle />
        <div className="inline-flex items-center gap-1 rounded-md border bg-background p-0.5">
          {LANGS.map(({ code, label }) => (
            <Button
              key={code}
              size="sm"
              variant="ghost"
              className={cn('h-7 min-w-9 px-2', lang === code && 'bg-secondary text-secondary-foreground')}
              onClick={() => setLang(code)}
            >
              {label}
            </Button>
          ))}
        </div>
      </div>
      <main className="flex w-full max-w-4xl flex-1 flex-col items-center justify-center gap-8 px-4 py-24">
        <motion.div initial={{ opacity: 0, y: 12 }} animate={{ opacity: 1, y: 0 }} transition={{ duration: 0.3 }} className="text-center">
          <h1 className="text-4xl font-bold tracking-tight">{t('common.appTitle')}</h1>
          <p className="mt-3 text-muted-foreground">{t('home.subtitle')}</p>
        </motion.div>
        <motion.div initial={{ opacity: 0, y: 12 }} animate={{ opacity: 1, y: 0 }} transition={{ duration: 0.3, delay: 0.1 }} className="w-full">
          <h2 className="mb-4 text-center text-lg font-semibold">{t('home.title')}</h2>
          <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-4">
            {FEATURES.map(({ key, icon: Icon }) => (
              <Card key={key}>
                <CardHeader>
                  <CardTitle className="flex items-center gap-2"><Icon className="h-5 w-5" /> {t(`home.${key}.title`)}</CardTitle>
                </CardHeader>
                <CardContent>
                  <p className="text-sm text-muted-foreground">{t(`home.${key}.desc`)}</p>
                </CardContent>
              </Card>
            ))}
          </div>
        </motion.div>
        <motion.div initial={{ opacity: 0, y: 12 }} animate={{ opacity: 1, y: 0 }} transition={{ duration: 0.3, delay: 0.2 }} className="flex items-center justify-center">
          <Button nativeButton={false} render={<Link to="/user/login" />}>
            {t('home.loginEntry')} <ArrowRight className="h-4 w-4" />
          </Button>
        </motion.div>
      </main>
    </div>
  )
}
