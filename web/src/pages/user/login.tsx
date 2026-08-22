// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

import { useState } from 'react'
import { Link, useNavigate } from 'react-router-dom'
import { motion } from 'framer-motion'
import { LogIn } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { ModeToggle } from '@/components/mode-toggle'
import { ApiError, userApi } from '@/lib/api/client'
import { userAuth } from '@/lib/auth'
import { setLang, type AppLang } from '@/lib/i18n'
import { cn } from '@/lib/utils'

const LANGS: { code: AppLang; label: string }[] = [
  { code: 'zh-CN', label: '中' },
  { code: 'en', label: 'EN' },
]

// 唯一登录页（原管理端 /login 已并入）：POST /user/auth/login 成功后统一进 /user；
// 角色随 token 存入同一 userAuth 槽——platform_admin 凭侧边栏管理菜单进入 /app。
export default function UserLogin() {
  const { t, i18n } = useTranslation()
  const lang: AppLang = i18n.resolvedLanguage?.startsWith('zh') ? 'zh-CN' : 'en'
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [err, setErr] = useState('')
  const [loading, setLoading] = useState(false)
  const nav = useNavigate()

  const submit = async () => {
    if (!email.trim() || !password) { setErr(t('user.auth.errorGeneric')); return }
    setErr('')
    setLoading(true)
    try {
      const res = await userApi.login({ email: email.trim(), password })
      userAuth.setToken(res.token)
      userAuth.setRole(res.user.Role)
      nav('/user')
    } catch (e) {
      // 服务端 error 字段直接展示；网络异常等统一兜底文案
      setErr(e instanceof ApiError ? e.message : t('user.auth.errorGeneric'))
    } finally {
      setLoading(false)
    }
  }

  return (
    <div className="flex min-h-screen items-center justify-center bg-gradient-to-br from-muted via-background to-background">
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
      <motion.div initial={{ opacity: 0, y: 12 }} animate={{ opacity: 1, y: 0 }} transition={{ duration: 0.3 }}>
        <Card className="w-96">
          <CardHeader><CardTitle className="flex items-center gap-2"><LogIn className="h-5 w-5" /> {t('user.auth.title')}</CardTitle></CardHeader>
          <CardContent className="space-y-3">
            <p className="text-sm text-muted-foreground">{t('user.auth.subtitle')}</p>
            <Input type="email" placeholder={t('user.auth.email')} value={email} onChange={e => { setEmail(e.target.value); setErr('') }} />
            <Input type="password" placeholder={t('user.auth.password')} value={password} onChange={e => { setPassword(e.target.value); setErr('') }} onKeyDown={e => { if (e.key === 'Enter') submit() }} />
            {err && <p className="text-sm text-destructive">{err}</p>}
            <Button className="w-full" disabled={loading} onClick={submit}>{t('user.auth.loginButton')}</Button>
            <Link to="/user/forgot-password" className="block text-center text-sm text-muted-foreground transition-colors hover:text-foreground">{t('user.auth.forgotPasswordLink')}</Link>
            <Link to="/user/register" className="block text-center text-sm text-muted-foreground transition-colors hover:text-foreground">
              {t('user.auth.registerLink')}
            </Link>
          </CardContent>
        </Card>
      </motion.div>
    </div>
  )
}
