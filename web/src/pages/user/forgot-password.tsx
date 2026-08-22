// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.
import { useState } from 'react'
import { Link } from 'react-router-dom'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { useTranslation } from 'react-i18next'
import { ApiError, userApi } from '@/lib/api/client'

export default function ForgotPassword() {
  const { t } = useTranslation()
  const [step, setStep] = useState<'email' | 'reset'>('email')
  const [email, setEmail] = useState('')
  const [code, setCode] = useState('')
  const [newPassword, setNewPassword] = useState('')
  const [msg, setMsg] = useState('')
  const [err, setErr] = useState('')
  const [loading, setLoading] = useState(false)
  const requestCode = async () => {
    if (!email.trim()) { setErr(t('user.forgot.emailRequired')); return }
    setLoading(true); setErr(''); setMsg('')
    try { await userApi.forgotPassword({ email: email.trim() }); setStep('reset'); setMsg(t('user.forgot.codeSent')) } catch (e) { setErr(e instanceof ApiError ? e.message : String(e)) } finally { setLoading(false) }
  }
  const reset = async () => {
    if (!code.trim() || !newPassword) { setErr(t('user.forgot.required')); return }
    setLoading(true); setErr('')
    try { await userApi.resetPassword({ email: email.trim(), code: code.trim(), new_password: newPassword }); setMsg(t('user.forgot.resetSuccess')) } catch (e) { setErr(e instanceof ApiError ? e.message : String(e)) } finally { setLoading(false) }
  }
  return (
    <div className="flex min-h-screen items-center justify-center bg-gradient-to-br from-muted via-background to-background">
      <Card className="w-96"><CardHeader><CardTitle>{t('user.forgot.title')}</CardTitle></CardHeader>
        <CardContent className="space-y-3">
          {step==='email' ? <>
            <Input placeholder={t('user.auth.email')} value={email} onChange={e=>setEmail(e.target.value)} />
            {err && <p className="text-sm text-destructive">{err}</p>}
            {msg && <p className="text-sm text-green-600">{msg}</p>}
            <Button className="w-full" disabled={loading} onClick={requestCode}>{t('user.forgot.sendCode')}</Button>
          </> : <>
            <Input placeholder={t('user.forgot.codePlaceholder')} value={code} onChange={e=>setCode(e.target.value)} />
            <Input type="password" placeholder={t('user.forgot.newPassword')} value={newPassword} onChange={e=>setNewPassword(e.target.value)} />
            {err && <p className="text-sm text-destructive">{err}</p>}
            {msg && <p className="text-sm text-green-600">{msg}</p>}
            <Button className="w-full" disabled={loading} onClick={reset}>{t('user.forgot.resetButton')}</Button>
            <Button variant="outline" className="w-full" onClick={()=>setStep('email')}>{t('common.back')}</Button>
          </>}
          <Link to="/user/login" className="block text-center text-sm text-muted-foreground hover:text-foreground">{t('user.auth.loginLink')}</Link>
        </CardContent>
      </Card>
    </div>
  )
}
