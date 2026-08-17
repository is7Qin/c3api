// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

import { useEffect, useMemo, useRef, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { FileJson, Gauge, Loader2, RefreshCw, Upload } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { api } from '@/App'
import { ApiError } from '@/lib/api/client'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Popover, PopoverContent, PopoverTrigger } from '@/components/ui/popover'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { Skeleton } from '@/components/ui/skeleton'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { toast } from '@/components/ui/toast'
import type { components } from '@/lib/api/schema'

type Template = components['schemas']['Template']
type Group = components['schemas']['Group']
type CodexUsage = components['schemas']['CodexUsageResponse']
type ImportResult = components['schemas']['CodexOAuthImportResponse']

const MAX_IMPORT_BYTES = 2 * 1024 * 1024

function asRecord(value: unknown): Record<string, unknown> | null {
  return value !== null && typeof value === 'object' && !Array.isArray(value)
    ? value as Record<string, unknown>
    : null
}

function countCredentials(value: unknown, depth = 0): number {
  if (depth > 4) return 0
  if (Array.isArray(value)) return value.reduce((sum, item) => sum + countCredentials(item, depth + 1), 0)

  const record = asRecord(value)
  if (!record) return 0
  for (const key of ['access_token', 'accessToken', 'oauth_token', 'oauthToken', 'token']) {
    if (typeof record[key] === 'string') return 1
  }
  for (const key of ['credentials', 'accounts', 'items', 'data', 'tokens']) {
    if (record[key] !== undefined) {
      const count = countCredentials(record[key], depth + 1)
      if (count > 0) return count
    }
  }
  return 0
}

function parseImportJSON(text: string): { value?: unknown; count: number; error?: string } {
  if (!text.trim()) return { count: 0 }
  try {
    const value: unknown = JSON.parse(text)
    return { value, count: countCredentials(value) }
  } catch (error) {
    return { count: 0, error: (error as Error).message }
  }
}

function importStatusVariant(status: string): 'default' | 'secondary' | 'destructive' | 'outline' {
  if (status === 'imported') return 'default'
  if (status === 'updated') return 'secondary'
  if (status === 'failed') return 'destructive'
  return 'outline'
}

export function CodexImportDialog({
  open,
  onOpenChange,
  templates,
  groups,
}: {
  open: boolean
  onOpenChange: (open: boolean) => void
  templates: Template[]
  groups: Group[]
}) {
  const { t } = useTranslation()
  const qc = useQueryClient()
  const fileRef = useRef<HTMLInputElement>(null)
  const [templateID, setTemplateID] = useState('')
  const [groupIDs, setGroupIDs] = useState<number[]>([])
  const [namePrefix, setNamePrefix] = useState('codex')
  const [weight, setWeight] = useState('100')
  const [maxConcurrency, setMaxConcurrency] = useState('8')
  const [jsonText, setJSONText] = useState('')
  const [formError, setFormError] = useState<string | null>(null)

  const parsed = useMemo(() => parseImportJSON(jsonText), [jsonText])
  const mutation = useMutation({
    mutationFn: (body: components['schemas']['CodexOAuthImportRequest']) => api.importCodexOAuth(body),
    onSuccess: result => {
      qc.invalidateQueries({ queryKey: ['accounts'] })
      toast.add({
        title: t('accounts.codexImport.completed'),
        description: t('accounts.codexImport.completedDesc', {
          imported: result.imported,
          updated: result.updated,
          skipped: result.skipped,
          failed: result.failed,
        }),
        type: result.failed > 0 ? 'warning' : 'success',
      })
    },
  })

  useEffect(() => {
    if (!open || templateID || templates.length === 0) return
    setTemplateID(String(templates[0].ID))
  }, [open, templateID, templates])

  const toggleGroup = (id: number) => {
    setGroupIDs(current => current.includes(id) ? current.filter(value => value !== id) : [...current, id])
  }

  const loadFile = async (file?: File) => {
    if (!file) return
    if (file.size > MAX_IMPORT_BYTES) {
      setFormError(t('accounts.codexImport.fileTooLarge'))
      return
    }
    try {
      setJSONText(await file.text())
      setFormError(null)
      mutation.reset()
    } catch (error) {
      setFormError((error as Error).message)
    }
  }

  const submit = () => {
    setFormError(null)
    mutation.reset()
    const numericWeight = Number(weight)
    const numericConcurrency = Number(maxConcurrency)
    if (!templateID || parsed.value === undefined || parsed.error || parsed.count < 1 || parsed.count > 100) {
      setFormError(parsed.error ? t('accounts.codexImport.invalidJSON') : t('accounts.codexImport.invalidCount'))
      return
    }
    if (!Number.isInteger(numericWeight) || numericWeight < 1 || !Number.isInteger(numericConcurrency) || numericConcurrency < 1) {
      setFormError(t('accounts.codexImport.invalidDefaults'))
      return
    }
    mutation.mutate({
      template_id: Number(templateID),
      group_ids: groupIDs,
      name_prefix: namePrefix.trim() || 'codex',
      weight: numericWeight,
      max_concurrency: numericConcurrency,
      credentials: parsed.value,
    })
  }

  const result: ImportResult | undefined = mutation.data
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-h-[90vh] overflow-y-auto sm:max-w-2xl">
        <DialogHeader>
          <DialogTitle>{t('accounts.codexImport.title')}</DialogTitle>
          <DialogDescription>{t('accounts.codexImport.desc')}</DialogDescription>
        </DialogHeader>

        <div className="grid gap-4 sm:grid-cols-2">
          <div className="space-y-2 sm:col-span-2">
            <Label>{t('accounts.codexImport.template')}</Label>
            <Select
              items={Object.fromEntries(templates.map(template => [String(template.ID), template.Name]))}
              value={templateID}
              onValueChange={setTemplateID}
            >
              <SelectTrigger className="w-full"><SelectValue placeholder={t('accounts.codexImport.templatePlaceholder')} /></SelectTrigger>
              <SelectContent>
                {templates.map(template => (
                  <SelectItem key={template.ID} value={String(template.ID)} label={template.Name}>{template.Name}</SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>

          <div className="space-y-2">
            <Label htmlFor="codex-import-prefix">{t('accounts.codexImport.namePrefix')}</Label>
            <Input id="codex-import-prefix" value={namePrefix} onChange={event => setNamePrefix(event.target.value)} />
          </div>
          <div className="grid grid-cols-2 gap-3">
            <div className="space-y-2">
              <Label htmlFor="codex-import-weight">{t('accounts.weightLabel')}</Label>
              <Input id="codex-import-weight" type="number" min={1} value={weight} onChange={event => setWeight(event.target.value)} />
            </div>
            <div className="space-y-2">
              <Label htmlFor="codex-import-concurrency">{t('accounts.maxLabel')}</Label>
              <Input id="codex-import-concurrency" type="number" min={1} value={maxConcurrency} onChange={event => setMaxConcurrency(event.target.value)} />
            </div>
          </div>

          <div className="space-y-2 sm:col-span-2">
            <Label>{t('accounts.groupLabel')}</Label>
            <div className="max-h-36 overflow-y-auto rounded-lg border p-2">
              {groups.length === 0 ? (
                <p className="px-2 py-3 text-sm text-muted-foreground">{t('accounts.codexImport.noGroups')}</p>
              ) : groups.map(group => (
                <label key={group.ID} className="flex cursor-pointer items-center gap-2.5 rounded-md px-2 py-1.5 hover:bg-muted">
                  <Checkbox checked={groupIDs.includes(group.ID!)} onCheckedChange={() => toggleGroup(group.ID!)} />
                  <span className="min-w-0 flex-1 truncate text-sm">{group.Name}</span>
                  <span className="text-xs text-muted-foreground">#{group.ID}</span>
                </label>
              ))}
            </div>
          </div>

          <div className="space-y-2 sm:col-span-2">
            <div className="flex items-center justify-between gap-3">
              <Label htmlFor="codex-import-json">{t('accounts.codexImport.json')}</Label>
              <div className="flex items-center gap-2">
                {jsonText.trim() && !parsed.error && (
                  <Badge variant={parsed.count > 0 && parsed.count <= 100 ? 'secondary' : 'destructive'}>
                    {t('accounts.codexImport.previewCount', { count: parsed.count })}
                  </Badge>
                )}
                <Button type="button" variant="outline" size="sm" onClick={() => fileRef.current?.click()}>
                  <Upload /> {t('accounts.codexImport.chooseFile')}
                </Button>
                <input
                  ref={fileRef}
                  className="hidden"
                  type="file"
                  accept="application/json,.json"
                  onChange={event => {
                    void loadFile(event.target.files?.[0])
                    event.currentTarget.value = ''
                  }}
                />
              </div>
            </div>
            <textarea
              id="codex-import-json"
              value={jsonText}
              onChange={event => {
                setJSONText(event.target.value)
                setFormError(null)
                mutation.reset()
              }}
              placeholder={t('accounts.codexImport.jsonPlaceholder')}
              spellCheck={false}
              className="min-h-44 w-full resize-y rounded-lg border bg-transparent px-3 py-2 font-mono text-xs outline-none transition-shadow placeholder:text-muted-foreground focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50"
            />
            {parsed.error && <p className="text-xs text-destructive">{t('accounts.codexImport.parseError', { message: parsed.error })}</p>}
          </div>
        </div>

        {(formError || mutation.isError) && (
          <p className="text-sm text-destructive">{formError ?? (mutation.error as Error).message}</p>
        )}

        {result && (
          <div className="space-y-3 border-t pt-4">
            <div className="flex flex-wrap gap-2 text-xs">
              <Badge>{t('accounts.codexImport.imported', { count: result.imported })}</Badge>
              <Badge variant="secondary">{t('accounts.codexImport.updated', { count: result.updated })}</Badge>
              <Badge variant="outline">{t('accounts.codexImport.skipped', { count: result.skipped })}</Badge>
              {result.failed > 0 && <Badge variant="destructive">{t('accounts.codexImport.failed', { count: result.failed })}</Badge>}
            </div>
            <div className="max-h-52 overflow-auto rounded-lg border">
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead className="w-12">#</TableHead>
                    <TableHead>{t('accounts.codexImport.account')}</TableHead>
                    <TableHead>{t('accounts.codexImport.email')}</TableHead>
                    <TableHead>{t('accounts.codexImport.status')}</TableHead>
                    <TableHead>{t('accounts.codexImport.message')}</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {result.items.map(item => (
                    <TableRow key={`${item.index}-${item.codex_account_id ?? ''}`}>
                      <TableCell className="tabular-nums">{item.index + 1}</TableCell>
                      <TableCell className="max-w-36 truncate" title={item.codex_account_id}>{item.codex_account_id ?? item.account_id ?? '-'}</TableCell>
                      <TableCell className="max-w-40 truncate" title={item.email}>{item.email ?? '-'}</TableCell>
                      <TableCell><Badge variant={importStatusVariant(item.status)}>{t(`accounts.codexImport.statuses.${item.status}`)}</Badge></TableCell>
                      <TableCell className="max-w-44 truncate text-xs text-muted-foreground" title={item.message}>{item.message ?? '-'}</TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            </div>
          </div>
        )}

        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)}>{t('common.cancel')}</Button>
          <Button onClick={submit} disabled={mutation.isPending || templates.length === 0}>
            {mutation.isPending ? <Loader2 className="animate-spin" /> : <FileJson />}
            {mutation.isPending ? t('accounts.codexImport.importing') : t('accounts.codexImport.submit')}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

type QuotaWindow = { usedPercent: number; resetAt?: Date }

function readNumber(record: Record<string, unknown> | null, key: string): number | undefined {
  const value = record?.[key]
  if (typeof value === 'number' && Number.isFinite(value)) return value
  if (typeof value === 'string' && value.trim() !== '') {
    const parsed = Number(value)
    if (Number.isFinite(parsed)) return parsed
  }
  return undefined
}

function readString(record: Record<string, unknown> | null, key: string): string | undefined {
  const value = record?.[key]
  return typeof value === 'string' && value.trim() ? value.trim() : undefined
}

function parseResetAt(window: Record<string, unknown> | null): Date | undefined {
  const direct = window?.reset_at
  if (typeof direct === 'number' && Number.isFinite(direct)) {
    const date = new Date(direct < 1e12 ? direct * 1000 : direct)
    if (!Number.isNaN(date.getTime())) return date
  }
  if (typeof direct === 'string') {
    const numeric = Number(direct)
    const date = Number.isFinite(numeric)
      ? new Date(numeric < 1e12 ? numeric * 1000 : numeric)
      : new Date(direct)
    if (!Number.isNaN(date.getTime())) return date
  }
  const afterSeconds = readNumber(window, 'reset_after_seconds')
  if (afterSeconds !== undefined && afterSeconds >= 0) return new Date(Date.now() + afterSeconds * 1000)
  return undefined
}

function parseQuotaWindow(value: unknown): QuotaWindow | undefined {
  const window = asRecord(value)
  const used = readNumber(window, 'used_percent')
  if (used === undefined) return undefined
  return { usedPercent: Math.min(100, Math.max(0, used)), resetAt: parseResetAt(window) }
}

function quotaSnapshot(data?: CodexUsage) {
  const usage = asRecord(data?.usage)
  const rateLimit = asRecord(usage?.rate_limit)
  const credits = asRecord(usage?.credits)
  const resetCredits = asRecord(data?.reset_credits)
  const availableCredits = readNumber(resetCredits, 'available_count')
    ?? readNumber(resetCredits, 'remaining')
    ?? readNumber(resetCredits, 'balance')
    ?? readNumber(credits, 'balance')
  return {
    plan: readString(usage, 'plan_type'),
    primary: parseQuotaWindow(rateLimit?.primary_window),
    secondary: parseQuotaWindow(rateLimit?.secondary_window),
    availableCredits,
  }
}

function QuotaLine({ label, window }: { label: string; window?: QuotaWindow }) {
  const { t } = useTranslation()
  if (!window) return (
    <div className="flex items-center justify-between gap-4 py-2 text-sm">
      <span className="text-muted-foreground">{label}</span>
      <span>{t('accounts.codexQuota.unavailable')}</span>
    </div>
  )
  const remaining = Math.max(0, 100 - window.usedPercent)
  return (
    <div className="space-y-1.5 py-2">
      <div className="flex items-center justify-between gap-4 text-sm">
        <span className="text-muted-foreground">{label}</span>
        <span className="font-medium tabular-nums">{t('accounts.codexQuota.remaining', { value: Math.round(remaining) })}</span>
      </div>
      <div className="h-1.5 overflow-hidden rounded-full bg-muted">
        <div className="h-full bg-primary transition-[width]" style={{ width: `${window.usedPercent}%` }} />
      </div>
      <div className="flex justify-between text-[11px] text-muted-foreground">
        <span>{t('accounts.codexQuota.used', { value: Math.round(window.usedPercent) })}</span>
        <span>{window.resetAt ? t('accounts.codexQuota.resetsAt', { time: window.resetAt.toLocaleString() }) : t('accounts.codexQuota.resetUnknown')}</span>
      </div>
    </div>
  )
}

export function CodexQuotaPopover({ accountID }: { accountID: number }) {
  const { t } = useTranslation()
  const qc = useQueryClient()
  const [open, setOpen] = useState(false)
  const query = useQuery({
    queryKey: ['codex-usage', accountID],
    queryFn: () => api.getCodexUsage(accountID),
    enabled: open,
    staleTime: 5 * 60_000,
    retry: false,
  })
  const refresh = useMutation({
    mutationFn: () => api.refreshCodexUsage(accountID),
    onSuccess: data => {
      qc.setQueryData(['codex-usage', accountID], data)
      toast.add({ title: t('accounts.codexQuota.refreshed'), type: 'success' })
    },
  })
  const snapshot = quotaSnapshot(query.data)
  const compact = snapshot.primary
    ? t('accounts.codexQuota.compact', { value: Math.round(100 - snapshot.primary.usedPercent) })
    : t('accounts.codexQuota.view')

  return (
    <Popover open={open} onOpenChange={setOpen}>
      <PopoverTrigger render={<Button variant="ghost" size="sm" className="h-8 min-w-24 justify-start px-2" />}>
        {query.isFetching && !query.data ? <Loader2 className="animate-spin" /> : <Gauge />}
        <span className="tabular-nums">{compact}</span>
      </PopoverTrigger>
      <PopoverContent align="end" className="w-80 p-4">
        <div className="flex items-start justify-between gap-3 border-b pb-3">
          <div className="min-w-0">
            <p className="font-medium">{t('accounts.codexQuota.title')}</p>
            <p className="truncate text-xs text-muted-foreground">{query.data?.email ?? query.data?.codex_account_id ?? `#${accountID}`}</p>
          </div>
          <Button
            variant="ghost"
            size="icon-sm"
            title={t('accounts.codexQuota.refresh')}
            onClick={() => refresh.mutate()}
            disabled={refresh.isPending}
          >
            <RefreshCw className={refresh.isPending ? 'animate-spin' : ''} />
          </Button>
        </div>

        {query.isLoading ? (
          <div className="space-y-3 py-4"><Skeleton className="h-12" /><Skeleton className="h-12" /></div>
        ) : query.isError ? (
          <p className="py-4 text-sm text-destructive">
            {t('accounts.codexQuota.loadFailed', { message: query.error instanceof ApiError ? query.error.message : (query.error as Error).message })}
          </p>
        ) : (
          <div>
            <div className="flex items-center justify-between py-2 text-sm">
              <span className="text-muted-foreground">{t('accounts.codexQuota.plan')}</span>
              <span className="font-medium uppercase">{snapshot.plan ?? t('accounts.codexQuota.unavailable')}</span>
            </div>
            <QuotaLine label={t('accounts.codexQuota.fiveHour')} window={snapshot.primary} />
            <QuotaLine label={t('accounts.codexQuota.weekly')} window={snapshot.secondary} />
            <div className="flex items-center justify-between border-t pt-3 text-sm">
              <span className="text-muted-foreground">{t('accounts.codexQuota.resetCredits')}</span>
              <span className="font-medium tabular-nums">{snapshot.availableCredits ?? t('accounts.codexQuota.unavailable')}</span>
            </div>
            {query.data?.reset_credits_error && <p className="mt-2 text-xs text-destructive">{query.data.reset_credits_error}</p>}
            {query.data?.fetched_at && (
              <p className="mt-3 text-[11px] text-muted-foreground">{t('accounts.codexQuota.updatedAt', { time: new Date(query.data.fetched_at).toLocaleString() })}</p>
            )}
          </div>
        )}
      </PopoverContent>
    </Popover>
  )
}
