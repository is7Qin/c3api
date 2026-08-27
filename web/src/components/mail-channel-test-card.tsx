// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.
import { useState } from 'react'
import { useMutation } from '@tanstack/react-query'
import { useTranslation } from 'react-i18next'
import { api } from '@/App'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { toast } from '@/components/ui/toast'

const EMAIL_RE = /^[^\s@]+@[^\s@]+\.[^\s@]+$/
function isValidEmail(s: string): boolean {
  if (s.length === 0 || s.includes('..')) return false
  if (!EMAIL_RE.test(s)) return false
  const at = s.indexOf('@')
  if (at <= 0) return false
  const local = s.slice(0, at)
  const domain = s.slice(at + 1)
  if (!local || !domain) return false
  if (local.startsWith('.') || local.endsWith('.') || domain.startsWith('.') || domain.endsWith('.')) return false
  if (local.startsWith('-') || local.endsWith('-') || domain.startsWith('-') || domain.endsWith('-')) return false
  return true
}

export function MailChannelTestCard() {
  const { t } = useTranslation()
  const [email, setEmail] = useState('')
  const [err, setErr] = useState<string | null>(null)

  const send = useMutation({
    mutationFn: () => api.sendMailChannelTest({ email: email.trim() }),
    onSuccess: () => {
      setErr(null)
      toast.add({ title: t('settings.mailChannelTest.sent'), type: 'success' })
    },
    onError: (e: Error) => {
      const m = e.message || t('settings.mailChannelTest.failed')
      setErr(m)
      toast.add({ title: m, type: 'error' })
    },
  })

  const trimmed = email.trim()
  const valid = isValidEmail(trimmed)
  const errId = 'mail-channel-test-error'
  const clientError = trimmed.length > 0 && !valid ? t('settings.mailChannelTest.invalidEmail') : null
  const visibleError = err ?? clientError

  return (
    <Card className="p-4 space-y-3">
      <div className="font-medium">{t('settings.mailChannelTest.title')}</div>
      <p className="text-xs text-muted-foreground">{t('settings.mailChannelTest.desc')}</p>
      <div className="space-y-1.5">
        <Label htmlFor="mail-channel-test-email">{t('settings.mailChannelTest.emailLabel')}</Label>
        <Input
          id="mail-channel-test-email"
          type="email"
          inputMode="email"
          placeholder={t('settings.mailChannelTest.emailPlaceholder')}
          value={email}
          onChange={e => {
            setEmail(e.target.value)
            setErr(null)
          }}
          onKeyDown={e => {
            if (e.key === 'Enter' && valid && !send.isPending) send.mutate()
          }}
          aria-invalid={visibleError ? true : undefined}
          aria-describedby={visibleError ? errId : undefined}
          className="w-full"
        />
        {visibleError && <p id={errId} className="text-xs text-destructive" role="alert">{visibleError}</p>}
      </div>
      <div className="flex gap-2">
        <Button disabled={!valid || send.isPending} onClick={() => send.mutate()} className="min-w-24">
          {send.isPending ? t('settings.mailChannelTest.sending') : t('settings.mailChannelTest.send')}
        </Button>
      </div>
    </Card>
  )
}
