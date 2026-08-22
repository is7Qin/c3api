// SPDX-License-Identifier: AGPL-3.0-or-later
import { useRef, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { motion } from 'framer-motion'
import { Settings as SettingsIcon } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { api } from '@/App'
import { ApiUnauthorized } from '@/lib/api/client'
import { Button } from '@/components/ui/button'
import { Card, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { Skeleton } from '@/components/ui/skeleton'
import { Switch } from '@/components/ui/switch'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { toast } from '@/components/ui/toast'
import type { components } from '@/lib/api/schema'

type Setting = components['schemas']['Setting']
type MailTemplate = components['schemas']['MailTemplate']

const MILLI_PER_USD = 100_000
const USD_KEYS = new Set(['default_user_balance', 'default_user_temp_balance'])
const TIER_KEYS = new Set(['service_tier_policy_priority', 'service_tier_policy_flex', 'service_tier_policy_fast'])
const TIER_VALUES = ['passthrough', 'strip', 'reject'] as const

const GROUPS: { id: string; keys: string[] }[] = [
  { id: 'signup', keys: ['signup_enabled'] },
  { id: 'defaults', keys: ['default_user_max_concurrency', 'default_user_balance', 'default_user_temp_balance', 'default_user_temp_balance_ttl_days'] },
  { id: 'pricingSync', keys: ['price_source_url', 'price_sync_cron'] },
  { id: 'tierPolicy', keys: ['service_tier_policy_priority', 'service_tier_policy_flex', 'service_tier_policy_fast'] },
  { id: 'cluster', keys: ['cluster.instances'] },
  { id: 'mail', keys: ['mail.enabled', 'mail.register_verification', 'mail.smtp_host', 'mail.smtp_port', 'mail.smtp_username', 'mail.smtp_password', 'mail.from_address', 'mail.tls'] },
]
const GROUPED_KEYS = new Set(GROUPS.flatMap(g => g.keys))

const isPlainInt = (v: string) => /^\d+$/.test(v)
const isUsdText = (v: string) => v.trim() !== '' && Number.isFinite(Number(v)) && Number(v) >= 0

function SettingRow({ setting }: { setting: Setting }) {
  const { t } = useTranslation()
  const qc = useQueryClient()
  const key = setting.Key ?? ''
  const typ = setting.Type ?? 'string'
  const current = setting.Value ?? ''
  const isUsd = USD_KEYS.has(key)
  const isTier = TIER_KEYS.has(key)
  const isPassword = key === 'mail.smtp_password'
  const toInput = (v: string) => (isUsd ? String(Number(v) / MILLI_PER_USD) : v)
  const [draft, setDraft] = useState(() => toInput(current))
  const pendingRef = useRef<string | null>(null)
  const queuedRef = useRef<string | null>(null)
  const draftRef = useRef(draft)
  draftRef.current = draft
  const [err, setErr] = useState<string | null>(null)
  const errMsg = (e: unknown) => (e instanceof ApiUnauthorized ? null : (e as Error)?.message)
  const save = useMutation({
    mutationFn: (value: string) => api.updateSetting({ key, value }),
    onSuccess: all => {
      qc.setQueryData(['settings'], all)
      if (draftRef.current === pendingRef.current) {
        setDraft(toInput(all.find(s => s.Key === key)?.Value ?? current))
      }
      pendingRef.current = null
      if (queuedRef.current !== null) {
        const v = queuedRef.current
        queuedRef.current = null
        pendingRef.current = v
        save.mutate(v)
      }
      setErr(null)
      toast.add({ title: t('settings.saved'), type: 'success' })
    },
    onError: (e: Error) => {
      if (draftRef.current === pendingRef.current) setDraft(toInput(current))
      pendingRef.current = null
      queuedRef.current = null
      const m = errMsg(e)
      if (m) setErr(m)
    },
  })
  const submitValue = (): string | null => {
    if (typ !== 'number') return draft.trim()
    if (isUsd) {
      if (!isUsdText(draft)) { setErr(t('settings.invalidUsd')); return null }
      return String(Math.round(Number(draft) * MILLI_PER_USD))
    }
    if (!isPlainInt(draft)) { setErr(t('settings.invalidNumber')); return null }
    return draft
  }
  const doSave = (v: string | null) => {
    if (v == null) return
    if (save.isPending) { queuedRef.current = v; return }
    if (v === current) return
    pendingRef.current = v
    save.mutate(v)
  }
  const onEnter = (e: React.KeyboardEvent<HTMLInputElement>) => {
    if (e.key !== 'Enter') return
    e.preventDefault()
    doSave(submitValue())
  }
  const control =
    typ === 'switch' ? (
      <Switch checked={draft === 'true'} disabled={save.isPending} onCheckedChange={c => { setDraft(String(c)); doSave(String(c)) }} aria-label={t(`settings.labels.${key}`)} />
    ) : isTier ? (
      <Select items={{ passthrough: t('settings.policies.passthrough'), strip: t('settings.policies.strip'), reject: t('settings.policies.reject') }} value={draft} onValueChange={v => { setDraft(v); doSave(v) }} disabled={save.isPending}>
        <SelectTrigger className="w-44 bg-black/[0.04] border-black/10 dark:bg-black/20 dark:border-white/10" aria-label={t(`settings.labels.${key}`)}><SelectValue /></SelectTrigger>
        <SelectContent>{TIER_VALUES.map(v => <SelectItem key={v} value={v} label={t(`settings.policies.${v}`)}>{t(`settings.policies.${v}`)}</SelectItem>)}</SelectContent>
      </Select>
    ) : typ === 'number' ? (
      <Input type="number" min={0} step={isUsd ? 0.00001 : 1} className="w-48 bg-black/[0.04] border-black/10 dark:bg-black/20 dark:border-white/10 text-right tabular-nums" value={draft} onChange={e => { setDraft(e.target.value); setErr(null) }} onBlur={() => { if (draft !== current) doSave(submitValue()) }} onKeyDown={onEnter} />
    ) : (
      <Input type={isPassword ? 'password' : 'text'} className="w-96 max-w-full bg-black/[0.04] border-black/10 dark:bg-black/20 dark:border-white/10" value={draft} onChange={e => { setDraft(e.target.value); setErr(null) }} onKeyDown={onEnter} />
    )
  return (
    <div className="flex items-start justify-between gap-5 py-3">
      <div className="min-w-0">
        <div className="flex items-center gap-2"><span className="text-sm font-medium">{t(`settings.labels.${key}`)}</span><code className="font-mono text-xs text-muted-foreground">{key}</code></div>
        <p className="text-xs text-muted-foreground">{t(`settings.descs.${key}`)}</p>
        {isUsd && !err && <p className="text-xs text-muted-foreground">{t('settings.usdHint')}</p>}
        {err && <p className="text-xs text-destructive">{err}</p>}
      </div>
      <div className="flex shrink-0 items-center gap-2">{control}{typ === 'string' && !isTier && <Button variant="outline" size="sm" onClick={() => doSave(submitValue())} disabled={save.isPending}>{save.isPending ? t('common.saving') : t('common.save')}</Button>}</div>
    </div>
  )
}

function MailTemplateCard({ purpose }: { purpose: string }) {
  const { t } = useTranslation()
  const qc = useQueryClient()
  const { data } = useQuery({ queryKey: ['mail-templates'], queryFn: () => api.getMailTemplates() })
  const tmpl = (data ?? []).find(x => x.purpose === purpose) as MailTemplate | undefined
  const [subject, setSubject] = useState('')
  const [body, setBody] = useState('')
  const [loaded, setLoaded] = useState(false)
  if (tmpl && !loaded) {
    setSubject(tmpl.subject ?? '')
    setBody(tmpl.body_text ?? '')
    setLoaded(true)
  }
  const save = useMutation({
    mutationFn: () => api.putMailTemplate(purpose, { subject, body_text: body }),
    onSuccess: () => { qc.invalidateQueries({ queryKey: ['mail-templates'] }); setLoaded(false); toast.add({ title: t('settings.saved'), type: 'success' }) },
    onError: (e: Error) => toast.add({ title: e.message, type: 'error' }),
  })
  const restore = useMutation({
    mutationFn: () => api.putMailTemplate(purpose, { subject: '', body_text: '' }),
    onSuccess: () => { qc.invalidateQueries({ queryKey: ['mail-templates'] }); setLoaded(false); toast.add({ title: t('settings.restored'), type: 'success' }) },
  })
  return (
    <Card className="p-4 space-y-3">
      <div className="font-medium">{t(`settings.mailTemplate.${purpose}`)}</div>
      <div className="space-y-1"><label className="text-sm">{t('settings.mailTemplate.subject')}</label><Input value={subject} onChange={e => setSubject(e.target.value)} placeholder="{{app_name}} {{code}}" /></div>
      <div className="space-y-1"><label className="text-sm">{t('settings.mailTemplate.body')}</label><textarea className="w-full min-h-24 rounded-md border border-input bg-black/[0.04] px-3 py-2 text-sm" value={body} onChange={e => setBody(e.target.value)} placeholder="{{code}} {{ttl_minutes}} {{app_name}}" /></div>
      <p className="text-xs text-muted-foreground">{t('settings.mailTemplate.hint')}</p>
      <div className="flex gap-2"><Button onClick={() => save.mutate()} disabled={save.isPending}>{t('common.save')}</Button><Button variant="outline" onClick={() => restore.mutate()} disabled={restore.isPending}>{t('settings.restoreDefault')}</Button></div>
    </Card>
  )
}

export default function SettingsPage() {
  const { t } = useTranslation()
  const { data, isLoading, isError, error } = useQuery({ queryKey: ['settings'], queryFn: () => api.getSettings() })
  const byKey = new Map((data ?? []).map(s => [s.Key ?? '', s]))
  const others = (data ?? []).filter(s => s.Key && !GROUPED_KEYS.has(s.Key))
  return (
    <motion.div className="space-y-6" initial={{ opacity: 0, y: 12 }} animate={{ opacity: 1, y: 0 }} transition={{ duration: 0.25 }}>
      <div><h1 className="text-2xl font-semibold tracking-tight">{t('settings.title')}</h1><p className="text-sm text-muted-foreground">{t('settings.subtitle')}</p></div>
      {isError ? <p className="text-sm text-destructive">{t('common.loadFailed', { message: (error as Error).message })}</p> : isLoading ? <div className="space-y-2">{Array.from({ length: 4 }).map((_, i) => <Skeleton key={i} className="h-12" />)}</div> : data?.length === 0 ? <Card className="flex flex-col items-center gap-2 py-12 text-muted-foreground"><SettingsIcon className="size-10" /><p className="font-medium">{t('settings.emptyTitle')}</p></Card> : (
        <Tabs defaultValue="signup">
          <TabsList className="w-full">
            {GROUPS.map(g => <TabsTrigger key={g.id} value={g.id} className="flex-1">{t(`settings.groups.${g.id}`)}</TabsTrigger>)}
          </TabsList>
          {GROUPS.map(g => {
            const rows = g.keys.map(k => byKey.get(k)).filter((s): s is Setting => !!s)
            return (
              <TabsContent key={g.id} value={g.id} className="space-y-4 pt-4">
                {rows.length > 0 && <Card><CardHeader><CardTitle>{t(`settings.groups.${g.id}`)}</CardTitle></CardHeader><div className="divide-y divide-border px-(--card-spacing)">{rows.map(s => <SettingRow key={s.Key} setting={s} />)}</div></Card>}
                {g.id === 'mail' && <div className="space-y-4"><MailTemplateCard purpose="register_code" /><MailTemplateCard purpose="reset_code" /></div>}
              </TabsContent>
            )
          })}
          {others.length > 0 && <Card><CardHeader><CardTitle>{t('settings.otherTitle')}</CardTitle><CardDescription>{t('settings.otherDesc')}</CardDescription></CardHeader><div className="divide-y divide-border px-(--card-spacing)">{others.map(s => <SettingRow key={s.Key} setting={s} />)}</div></Card>}
        </Tabs>
      )}
    </motion.div>
  )
}
