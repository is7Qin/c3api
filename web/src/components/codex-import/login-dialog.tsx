import { useEffect, useMemo, useRef, useState } from 'react'
import { AlertTriangle, CheckCircle2, ExternalLink, Loader2 } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Label } from '@/components/ui/label'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { copyText } from '@/components/key-box'
import { useScopedApi } from '@/lib/api/scope'
import type { components } from '@/lib/api/schema'

type Template = components['schemas']['Template']
type Group = components['schemas']['Group']
type Challenge = components['schemas']['CodexDeviceLoginStart']
type Phase = 'idle' | 'starting' | 'code' | 'checking' | 'done' | 'expired' | 'denied' | 'error'

// 设备码登录（ChatGPT OAuth device flow）：**状态在前端**。
//
// 发起返回 device_auth_id + user_code；用户在自己的浏览器完成授权后点「我已完成
// 授权」，前端才调用 complete —— 服务端当场轮询一次并落库。网关不保存待授权
// 会话（无 Redis/TTL/跨实例问题），浏览器关掉即自然放弃。
//
// pending = 上游还没看到授权（通常几秒传播延迟）：保留一次性码，让用户再点一次，
// 而不是前端自动定时轮询——把节奏交还给用户，也避免后台打上游。
//
// 生命周期：发起响应的 expires_in_seconds 恒为正（SDK 按真客户端 15 分钟补齐），
// 这里据此维护本地截止时间；到期即进入 expired，不再发 complete 请求。
export function CodexLoginDialog({ open, onOpenChange, templates, groups, onDone }: { open: boolean; onOpenChange: (open: boolean) => void; templates: Template[]; groups: Group[]; onDone: () => void }) {
  const api = useScopedApi()
  const { t } = useTranslation()
  const oauthTemplates = useMemo(() => templates.filter(tp => tp.CredentialType === 'codex-oauth'), [templates])
  const [templateId, setTemplateId] = useState('')
  const [groupId, setGroupId] = useState('')
  const [challenge, setChallenge] = useState<Challenge | null>(null)
  const [phase, setPhase] = useState<Phase>('idle')
  const [error, setError] = useState('')
  const [pendingHint, setPendingHint] = useState(false)
  const [copied, setCopied] = useState(false)
  const [deadline, setDeadline] = useState<number | null>(null)
  // 请求代次：关闭/重开/重置后，上一轮的迟到响应不得改写本轮状态。
  const genRef = useRef(0)

  const reset = () => {
    genRef.current++
    setTemplateId('')
    setGroupId('')
    setChallenge(null)
    setPhase('idle')
    setError('')
    setPendingHint(false)
    setCopied(false)
    setDeadline(null)
  }

  useEffect(() => {
    if (open) reset()
    else genRef.current++
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open])

  // 本地截止时间：到点直接进 expired（服务端无会话，没有别的过期来源）。
  useEffect(() => {
    if (phase !== 'code' || deadline == null) return
    const left = deadline - Date.now()
    if (left <= 0) {
      setPhase('expired')
      return
    }
    const timer = window.setTimeout(() => setPhase('expired'), left)
    return () => window.clearTimeout(timer)
  }, [phase, deadline])

  const close = (next: boolean) => {
    if (!next) reset()
    onOpenChange(next)
  }

  const start = async () => {
    const gen = ++genRef.current
    setPhase('starting')
    setError('')
    setPendingHint(false)
    try {
      const res = await api.startCodexDeviceLogin()
      if (gen !== genRef.current) return
      setChallenge(res)
      setDeadline(res.expires_in_seconds > 0 ? Date.now() + res.expires_in_seconds * 1000 : null)
      setPhase('code')
    } catch (e) {
      if (gen !== genRef.current) return
      setPhase('error')
      setError((e as Error)?.message ?? '')
    }
  }

  const complete = async () => {
    if (!challenge || !templateId) return
    if (deadline != null && Date.now() >= deadline) {
      setPhase('expired')
      return
    }
    const gen = genRef.current
    setPhase('checking')
    setError('')
    try {
      const res = await api.completeCodexDeviceLogin({
        device_auth_id: challenge.device_auth_id,
        user_code: challenge.user_code,
        template_id: Number(templateId),
        group_id: groupId ? Number(groupId) : null,
      })
      if (res.status === 'authorized') {
        // 已落库就是事实：即使弹窗已关闭/重置也刷新列表（迟到响应不改写状态）。
        onDone()
        if (gen !== genRef.current) return
        setDeadline(null)
        setPhase('done')
        return
      }
      if (gen !== genRef.current) return
      if (res.status === 'pending') {
        setPhase('code')
        setPendingHint(true)
        return
      }
      setDeadline(null)
      setPhase(res.status)
    } catch (e) {
      // 网络/上游故障：一次性码通常仍有效，保留现场让用户重试一次。
      if (gen !== genRef.current) return
      setPhase('code')
      setError((e as Error)?.message ?? '')
    }
  }

  const copyCode = async () => {
    if (!challenge?.user_code) return
    if (await copyText(challenge.user_code)) {
      setCopied(true)
      window.setTimeout(() => setCopied(false), 2000)
    }
  }

  const pickersEditable = phase === 'idle' || phase === 'code'
  const showPickers = phase === 'idle' || phase === 'starting' || phase === 'code' || phase === 'checking'

  return (
    <Dialog open={open} onOpenChange={close}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>{t('accounts.login.title')}</DialogTitle>
          <DialogDescription>{t('accounts.login.desc')}</DialogDescription>
        </DialogHeader>

        {showPickers && (
          <div className="space-y-4">
            <div className="space-y-1.5">
              <Label>{t('accounts.login.template')}</Label>
              <Select
                items={Object.fromEntries(oauthTemplates.map(tp => [String(tp.ID), tp.Name ?? `#${tp.ID}`]))}
                value={templateId || null}
                onValueChange={v => setTemplateId(String(v ?? ''))}
                disabled={!pickersEditable}
              >
                <SelectTrigger className="h-10 w-full">
                  <SelectValue placeholder={t('accounts.login.templatePlaceholder')} />
                </SelectTrigger>
                <SelectContent>
                  {oauthTemplates.map(tp => (
                    <SelectItem key={tp.ID} value={String(tp.ID)} label={tp.Name ?? `#${tp.ID}`}>
                      {tp.Name ?? `#${tp.ID}`}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
              {oauthTemplates.length === 0 && <p className="text-xs text-destructive">{t('accounts.login.noTemplate')}</p>}
            </div>
            <div className="space-y-1.5">
              <Label>{t('accounts.login.group')}</Label>
              <Select
                items={Object.fromEntries([['__none', t('accounts.login.groupNone')], ...groups.map(g => [String(g.ID), g.Name ?? `#${g.ID}`])])}
                value={groupId || '__none'}
                onValueChange={v => setGroupId(v === '__none' ? '' : String(v ?? ''))}
                disabled={!pickersEditable}
              >
                <SelectTrigger className="h-10 w-full">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="__none" label={t('accounts.login.groupNone')}>
                    {t('accounts.login.groupNone')}
                  </SelectItem>
                  {groups.map(g => (
                    <SelectItem key={g.ID} value={String(g.ID)} label={g.Name ?? `#${g.ID}`}>
                      {g.Name ?? `#${g.ID}`}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
            <Alert>
              <AlertDescription className="text-sm">{t('accounts.login.ungroupedHint')}</AlertDescription>
            </Alert>
          </div>
        )}

        {phase === 'code' && challenge && (
          <div className="space-y-4">
            <div className="rounded-lg border bg-subtle p-4 text-center">
              <p className="text-sm text-muted-foreground">{t('accounts.login.codeHint')}</p>
              <div className="mt-2 flex items-center justify-center gap-2">
                <code className="select-all rounded border bg-surface px-4 py-2 font-mono text-2xl font-bold tracking-wider">{challenge.user_code}</code>
                <Button size="sm" variant="outline" onClick={copyCode}>
                  {copied ? t('keybox.copied') : t('keybox.copy')}
                </Button>
              </div>
              <a href={challenge.verification_uri} target="_blank" rel="noopener noreferrer" className="mt-3 inline-flex items-center gap-1 text-sm text-primary hover:underline">
                {challenge.verification_uri}
                <ExternalLink className="h-3 w-3" />
              </a>
              {challenge.expires_in_seconds > 0 && (
                <p className="mt-2 text-xs text-muted-foreground">{t('accounts.login.expiresIn', { minutes: Math.max(1, Math.round(challenge.expires_in_seconds / 60)) })}</p>
              )}
            </div>
            {pendingHint && <p className="text-sm text-muted-foreground">{t('accounts.login.stillPending')}</p>}
            {error && (
              <Alert variant="destructive">
                <AlertTriangle />
                <AlertDescription className="text-sm">{error}</AlertDescription>
              </Alert>
            )}
          </div>
        )}

        {phase === 'expired' && (
          <Alert variant="destructive">
            <AlertTriangle />
            <AlertDescription className="text-sm">{t('accounts.login.expired')}</AlertDescription>
          </Alert>
        )}

        {phase === 'denied' && (
          <Alert variant="destructive">
            <AlertTriangle />
            <AlertDescription className="text-sm">{t('accounts.login.denied')}</AlertDescription>
          </Alert>
        )}

        {phase === 'error' && (
          <Alert variant="destructive">
            <AlertTriangle />
            <AlertDescription className="text-sm">{error || t('accounts.login.unknownError')}</AlertDescription>
          </Alert>
        )}

        {phase === 'done' && (
          <Alert>
            <CheckCircle2 />
            <AlertDescription className="text-sm">{t('accounts.login.done')}</AlertDescription>
          </Alert>
        )}

        <DialogFooter>
          {(phase === 'idle' || phase === 'starting') && (
            <>
              <Button variant="outline" onClick={() => close(false)}>
                {t('common.cancel')}
              </Button>
              <Button onClick={start} disabled={!templateId || oauthTemplates.length === 0 || phase === 'starting'}>
                {phase === 'starting' ? <Loader2 className="h-4 w-4 animate-spin" /> : null}
                {t('accounts.login.start')}
              </Button>
            </>
          )}
          {phase === 'code' && (
            <>
              <Button variant="outline" onClick={() => close(false)}>
                {t('common.cancel')}
              </Button>
              <Button onClick={complete} disabled={!templateId}>
                {t('accounts.login.completeAction')}
              </Button>
            </>
          )}
          {phase === 'checking' && (
            <Button disabled>
              <Loader2 className="h-4 w-4 animate-spin" />
              {t('accounts.login.checking')}
            </Button>
          )}
          {phase === 'done' && <Button onClick={() => close(false)}>{t('common.close')}</Button>}
          {(phase === 'expired' || phase === 'denied' || phase === 'error') && (
            <>
              <Button variant="outline" onClick={() => close(false)}>
                {t('common.close')}
              </Button>
              <Button onClick={reset}>{t('accounts.login.retry')}</Button>
            </>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
