// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.
import { useEffect, useState } from 'react'
import { Link, useNavigate } from 'react-router-dom'
import { motion } from 'framer-motion'
import { UserPlus } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { ModeToggle } from '@/components/mode-toggle'
import { ApiError, userApi } from '@/lib/api/client'
import { userAuth } from '@/lib/auth'
import { setLang, type AppLang } from '@/lib/i18n'
import { cn } from '@/lib/utils'

const LANGS: { code: AppLang; label: string }[] = [{ code: 'zh-CN', label: '中' }, { code: 'en', label: 'EN' }]

export default function UserRegister() {
  const { t, i18n } = useTranslation()
  const lang: AppLang = i18n.resolvedLanguage?.startsWith('zh') ? 'zh-CN' : 'en'
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [confirm, setConfirm] = useState('')
  const [code, setCode] = useState('')
  const [step, setStep] = useState<'form' | 'code'>('form')
  const [countdown, setCountdown] = useState(0)
  const [err, setErr] = useState('')
  const [loading, setLoading] = useState(false)
  const nav = useNavigate()
  useEffect(() => { if (countdown <= 0) return; const id = setTimeout(() => setCountdown(c => c - 1), 1000); return () => clearTimeout(id) }, [countdown])
  const doRegister = async (withCode: string | undefined) => {
    const res = await userApi.register({ email: email.trim(), password, ...(withCode ? { code: withCode } : {}) })
    userAuth.setToken(res.token)
    userAuth.setRole(res.user.Role)
    nav('/user')
  }
  const submit = async () => {
    if (!email.trim() || !password || !confirm) { setErr(t('user.register.required')); return }
    if (password !== confirm) { setErr(t('user.register.passwordMismatch')); return }
    if (step === 'code' && !code.trim()) { setErr(t('user.register.codeRequired')); return }
    setErr(''); setLoading(true)
    try {
      if (step === 'code') { await doRegister(code.trim()); return }
      await doRegister(undefined)
    } catch (e) {
      if (e instanceof ApiError && e.message.includes('email verification required')) {
        try { await userApi.registerCode({ email: email.trim() }); setStep('code'); setCountdown(60); setErr('') } catch (ee) { setErr(ee instanceof ApiError ? ee.message : t('user.auth.errorGeneric')) }
      } else if (e instanceof ApiError && e.status === 403) setErr(t('user.register.signupDisabled'))
      else setErr(e instanceof ApiError ? e.message : t('user.auth.errorGeneric'))
    } finally { setLoading(false) }
  }
  const resend = async () => {
    if (countdown > 0) return
    try { await userApi.registerCode({ email: email.trim() }); setCountdown(60); setErr('') } catch (e) { setErr(e instanceof ApiError ? e.message : String(e)) }
  }
  return (
    <div className="flex min-h-screen items-center justify-center bg-gradient-to-br from-muted via-background to-background">
      <div className="absolute right-4 top-4 flex items-center gap-2"><ModeToggle /><div className="inline-flex items-center gap-1 rounded-md border bg-background p-0.5">{LANGS.map(({ code: c, label }) => <Button key={c} size="sm" variant="ghost" className={cn('h-7 min-w-9 px-2', lang === c && 'bg-secondary text-secondary-foreground')} onClick={() => setLang(c)}>{label}</Button>)}</div></div>
      <motion.div initial={{ opacity: 0, y: 12 }} animate={{ opacity: 1, y: 0 }} transition={{ duration: 0.3 }}>
        <Card className="w-96"><CardHeader><CardTitle className="flex items-center gap-2"><UserPlus className="h-5 w-5" /> {t('user.register.title')}</CardTitle></CardHeader>
          <CardContent className="space-y-3">
            <p className="text-sm text-muted-foreground">{t('user.register.subtitle')}</p>
            <Input type="email" placeholder={t('user.auth.email')} value={email} onChange={e => { setEmail(e.target.value); setErr('') }} disabled={step==='code'} />
            <Input type="password" placeholder={t('user.auth.password')} value={password} onChange={e => { setPassword(e.target.value); setErr('') }} disabled={step==='code'} />
            <Input type="password" placeholder={t('user.auth.confirmPassword')} value={confirm} onChange={e => { setConfirm(e.target.value); setErr('') }} disabled={step==='code'} />
            {step==='code' && <div className="flex gap-2"><Input placeholder={t('user.register.codePlaceholder')} value={code} onChange={e => setCode(e.target.value)} /><Button variant="outline" disabled={countdown>0} onClick={resend}>{countdown>0 ? `${countdown}s` : t('user.register.resend')}</Button></div>}
            {err && <p className="text-sm text-destructive">{err}</p>}
            <Button className="w-full" disabled={loading} onClick={submit}>{step==='code' ? t('user.register.verifyButton') : t('user.auth.registerButton')}</Button>
            <Link to="/user/login" className="block text-center text-sm text-muted-foreground hover:text-foreground">{t('user.auth.loginLink')}</Link>
          </CardContent>
        </Card>
      </motion.div>
    </div>
  )
}
