import { useEffect, useMemo, useRef, useState } from 'react'
import { AlertTriangle, CheckCircle2, ExternalLink, Loader2 } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Label } from '@/components/ui/label'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { Textarea } from '@/components/ui/textarea'
import { copyText } from '@/components/key-box'
import { useScopedApi } from '@/lib/api/scope'
import type { components } from '@/lib/api/schema'
import { deviceLoginCompleteFields, deviceLoginProvider, type DeviceLoginProvider } from '@/lib/device-login'

type Template = components['schemas']['Template']
type Group = components['schemas']['Group']
type Challenge = components['schemas']['CodexDeviceLoginStart']

type LoginTarget = { templateId: number; provider: DeviceLoginProvider; label: string }
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
  const loginTargets = useMemo(() => templates.flatMap(tp => {
    const provider = deviceLoginProvider(tp.CredentialType)
    if (!provider) return []
    const suffix = provider === 'bps' ? t('accounts.login.bpsTemplateSuffix') : t('accounts.login.codexTemplateSuffix')
    return [{ templateId: tp.ID, provider, label: `${tp.Name ?? `#${tp.ID}`} · ${suffix}` }]
  }), [templates, t])
  const [templateId, setTemplateId] = useState('')
  const [profileText, setProfileText] = useState('')
  const [target, setTarget] = useState<LoginTarget | null>(null)
  const [groupId, setGroupId] = useState('')
  const [challenge, setChallenge] = useState<Challenge | null>(null)
  const [phase, setPhase] = useState<Phase>('idle')
  const [error, setError] = useState('')
  const [pendingHint, setPendingHint] = useState(false)
  const [copied, setCopied] = useState(false)
  const [deadline, setDeadline] = useState<number | null>(null)
  // 请求代次：关闭/重开/重置后，上一轮的迟到响应不得改写本轮状态。
  const genRef = useRef(0)
  const inFlight = useRef(false)

  const reset = () => {
    genRef.current++
    inFlight.current = false
    setTemplateId('')
    setGroupId('')
    setProfileText('')
    setTarget(null)
    setChallenge(null)
    setPhase('idle')
    setError('')
    setPendingHint(false)
    setCopied(false)
    setDeadline(null)
  }

  useEffect(() => {
    reset()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open])

  // 本地截止时间：到点直接进 expired（服务端无会话，没有别的过期来源）。
  useEffect(() => {
    if (phase !== 'code' || deadline == null) return
    const left = deadline - Date.now()
    const expire = () => {
      setProfileText('')
      setChallenge(null)
      setPhase('expired')
    }
    if (left <= 0) {
      expire()
      return
    }
    const timer = window.setTimeout(expire, left)
    return () => window.clearTimeout(timer)
  }, [phase, deadline])

  const close = (next: boolean) => {
    if (!next) reset()
    onOpenChange(next)
  }

  const selectedTarget = loginTargets.find(tp => String(tp.templateId) === templateId)
  const activeProvider = (target ?? selectedTarget)?.provider
  const templateOptions = target ? [target] : loginTargets

  const start = async () => {
    if (phase !== 'idle' || inFlight.current || !selectedTarget) return
    const gen = ++genRef.current
    inFlight.current = true
    setTarget(selectedTarget)
    setPhase('starting')
    setError('')
    setPendingHint(false)
    try {
      const res = await api.startCodexDeviceLogin(selectedTarget.provider === 'bps' ? 'bps' : undefined)
      if (gen !== genRef.current) return
      setChallenge(res)
      setDeadline(res.expires_in_seconds > 0 ? Date.now() + res.expires_in_seconds * 1000 : null)
      setPhase('code')
    } catch (e) {
      if (gen !== genRef.current) return
      setProfileText('')
      setPhase('error')
      setError((e as Error)?.message ?? '')
    } finally {
      if (gen === genRef.current) inFlight.current = false
    }
  }

  const complete = async () => {
    if (phase !== 'code' || inFlight.current || !challenge || !target) return
    if (deadline != null && Date.now() >= deadline) {
      setProfileText('')
      setChallenge(null)
      setPhase('expired')
      return
    }
    const fields = deviceLoginCompleteFields(target.provider, profileText)
    if (!fields) {
      setError(t('accounts.login.profileInvalid'))
      return
    }
    const gen = genRef.current
    inFlight.current = true
    setPhase('checking')
    setPendingHint(false)
    setError('')
    try {
      const res = await api.completeCodexDeviceLogin({
        device_auth_id: challenge.device_auth_id,
        user_code: challenge.user_code,
        template_id: target.templateId,
        group_id: groupId ? Number(groupId) : null,
        ...fields,
      })
      if (res.status === 'authorized') {
        // 已落库就是事实：即使弹窗已关闭/重置也刷新列表（迟到响应不改写状态）。
        onDone()
        if (gen !== genRef.current) return
        setDeadline(null)
        setProfileText('')
        setChallenge(null)
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
      setProfileText('')
      setChallenge(null)
      setPhase(res.status)
    } catch (e) {
      // 网络/上游故障：一次性码通常仍有效，保留现场让用户重试一次。
      if (gen !== genRef.current) return
      setPhase('code')
      setError((e as Error)?.message ?? '')
    } finally {
      if (gen === genRef.current) inFlight.current = false
    }
  }

  const copyCode = async () => {
    if (!challenge?.user_code) return
    if (await copyText(challenge.user_code)) {
      setCopied(true)
      window.setTimeout(() => setCopied(false), 2000)
    }
  }

  // Template/provider are frozen once the upstream device grant has started.
  const templateEditable = phase === 'idle'
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
              <Label htmlFor="login-template">{t('accounts.login.template')}</Label>
              <Select
                items={Object.fromEntries(templateOptions.map(tp => [String(tp.templateId), tp.label]))}
                value={templateId || null}
                onValueChange={v => {
                  if (!templateEditable || inFlight.current) return
                  setTemplateId(String(v ?? ''))
                  setProfileText('')
                }}
                disabled={!templateEditable}
              >
                <SelectTrigger id="login-template" className="h-10 w-full">
                  <SelectValue placeholder={t('accounts.login.templatePlaceholder')} />
                </SelectTrigger>
                <SelectContent>
                  {templateOptions.map(tp => (
                    <SelectItem key={tp.templateId} value={String(tp.templateId)} label={tp.label}>{tp.label}</SelectItem>
                  ))}
                </SelectContent>
              </Select>
              {templateOptions.length === 0 && <p className="text-xs text-destructive">{t('accounts.login.noTemplate')}</p>}
            </div>
            {activeProvider === 'bps' && (
              <div className="space-y-1.5">
                <Label htmlFor="bps-profile">{t('accounts.login.profileLabel')}</Label>
                <Textarea
                  id="bps-profile"
                  value={profileText}
                  onChange={e => setProfileText(e.target.value)}
                  disabled={phase === 'starting' || phase === 'checking'}
                  placeholder={t('accounts.login.profilePlaceholder')}
                  spellCheck={false}
                  autoComplete="off"
                  aria-describedby="bps-profile-hint"
                  className="min-h-40 font-mono text-xs"
                />
                <p id="bps-profile-hint" className="text-xs text-muted-foreground">{t('accounts.login.profileHint')}</p>
              </div>
            )}
            <div className="space-y-1.5">
              <Label>{t('accounts.login.group')}</Label>
              <Select
                items={Object.fromEntries([['__none', t('accounts.login.groupNone')], ...groups.map(g => [String(g.ID), g.Name ?? `#${g.ID}`])])}
                value={groupId || '__none'}
                onValueChange={v => setGroupId(v === '__none' ? '' : String(v ?? ''))}
                disabled={phase === 'starting' || phase === 'checking'}
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
              <Button onClick={start} disabled={!selectedTarget || phase === 'starting'}>
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
              <Button onClick={complete} disabled={!target}>
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
