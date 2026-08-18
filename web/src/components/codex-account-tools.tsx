// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

import { useEffect, useMemo, useRef, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { FileJson, Gauge, Loader2, Upload } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { api } from '@/App'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Popover, PopoverContent, PopoverTrigger } from '@/components/ui/popover'
import { Select, SelectContent, SelectGroup, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { Separator } from '@/components/ui/separator'
import { Skeleton } from '@/components/ui/skeleton'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { toast } from '@/components/ui/toast'
import type { components } from '@/lib/api/schema'

type Template = components['schemas']['Template']
type Group = components['schemas']['Group']
type ImportResult = components['schemas']['CodexOAuthImportResponse']
type AccountUsage = components['schemas']['AccountUsageItem']

const MAX_IMPORT_BYTES = 1 << 20

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
  const queryClient = useQueryClient()
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
      queryClient.invalidateQueries({ queryKey: ['accounts'] })
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
    if (open && !templateID && templates.length > 0) setTemplateID(String(templates[0].ID))
  }, [open, templateID, templates])

  const setDialogOpen = (nextOpen: boolean) => {
    if (!nextOpen && mutation.isPending) return
    if (!nextOpen) {
      setTemplateID('')
      setGroupIDs([])
      setNamePrefix('codex')
      setWeight('100')
      setMaxConcurrency('8')
      setJSONText('')
      setFormError(null)
      mutation.reset()
    }
    onOpenChange(nextOpen)
  }

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
    <Dialog open={open} onOpenChange={setDialogOpen}>
      <DialogContent className="max-h-[90vh] overflow-y-auto sm:max-w-2xl">
        <DialogHeader>
          <DialogTitle>{t('accounts.codexImport.title')}</DialogTitle>
          <DialogDescription>{t('accounts.codexImport.desc')}</DialogDescription>
        </DialogHeader>

        <div className="grid gap-4 sm:grid-cols-2">
          <div className="space-y-1.5 sm:col-span-2">
            <Label>{t('accounts.codexImport.template')}</Label>
            <Select
              items={Object.fromEntries(templates.map(template => [String(template.ID), template.Name]))}
              value={templateID}
              onValueChange={setTemplateID}
            >
              <SelectTrigger className="w-full"><SelectValue placeholder={t('accounts.codexImport.templatePlaceholder')} /></SelectTrigger>
              <SelectContent>
                <SelectGroup>
                  {templates.map(template => (
                    <SelectItem key={template.ID} value={String(template.ID)} label={template.Name}>{template.Name}</SelectItem>
                  ))}
                </SelectGroup>
              </SelectContent>
            </Select>
          </div>

          <div className="space-y-1.5">
            <Label htmlFor="codex-import-prefix">{t('accounts.codexImport.namePrefix')}</Label>
            <Input id="codex-import-prefix" value={namePrefix} onChange={event => setNamePrefix(event.target.value)} />
          </div>
          <div className="grid grid-cols-2 gap-3">
            <div className="space-y-1.5">
              <Label htmlFor="codex-import-weight">{t('accounts.weightLabel')}</Label>
              <Input id="codex-import-weight" type="number" min={1} value={weight} onChange={event => setWeight(event.target.value)} />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="codex-import-concurrency">{t('accounts.maxLabel')}</Label>
              <Input id="codex-import-concurrency" type="number" min={1} value={maxConcurrency} onChange={event => setMaxConcurrency(event.target.value)} />
            </div>
          </div>

          <div className="space-y-1.5 sm:col-span-2">
            <Label>{t('accounts.groupLabel')}</Label>
            <div className="max-h-36 overflow-y-auto rounded-md border p-2">
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

          <div className="space-y-1.5 sm:col-span-2">
            <div className="flex flex-wrap items-center justify-between gap-2">
              <Label htmlFor="codex-import-json">{t('accounts.codexImport.json')}</Label>
              <div className="flex items-center gap-2">
                {jsonText.trim() && !parsed.error && (
                  <Badge variant={parsed.count > 0 && parsed.count <= 100 ? 'secondary' : 'destructive'}>
                    {t('accounts.codexImport.previewCount', { count: parsed.count })}
                  </Badge>
                )}
                <Button type="button" variant="outline" size="sm" onClick={() => fileRef.current?.click()}>
                  <Upload data-icon="inline-start" aria-hidden="true" />
                  {t('accounts.codexImport.chooseFile')}
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
              className="min-h-44 w-full resize-y rounded-md border bg-transparent px-3 py-2 font-mono text-xs outline-none transition-shadow placeholder:text-muted-foreground focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50"
            />
            {parsed.error && <p className="text-xs text-destructive">{t('accounts.codexImport.parseError', { message: parsed.error })}</p>}
          </div>
        </div>

        {(formError || mutation.isError) && (
          <p className="text-sm text-destructive">{formError ?? (mutation.error as Error).message}</p>
        )}

        {result && (
          <>
            <Separator />
            <div className="space-y-3">
              <div className="flex flex-wrap gap-2 text-xs">
                <Badge>{t('accounts.codexImport.imported', { count: result.imported })}</Badge>
                <Badge variant="secondary">{t('accounts.codexImport.updated', { count: result.updated })}</Badge>
                <Badge variant="outline">{t('accounts.codexImport.skipped', { count: result.skipped })}</Badge>
                {result.failed > 0 && <Badge variant="destructive">{t('accounts.codexImport.failed', { count: result.failed })}</Badge>}
              </div>
              <div className="max-h-52 overflow-auto rounded-md border">
                <Table>
                  <TableHeader>
                    <TableRow>
                      <TableHead className="w-12">#</TableHead>
                      <TableHead>{t('accounts.codexImport.space')}</TableHead>
                      <TableHead>{t('accounts.codexImport.email')}</TableHead>
                      <TableHead>{t('accounts.codexImport.status')}</TableHead>
                      <TableHead>{t('accounts.codexImport.message')}</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {result.items.map(item => (
                      <TableRow key={`${item.index}-${item.email ?? ''}-${item.space_id ?? ''}`}>
                        <TableCell className="tabular-nums">{item.index + 1}</TableCell>
                        <TableCell className="max-w-36 truncate" title={item.space_id}>{item.space_id ?? '-'}</TableCell>
                        <TableCell className="max-w-40 truncate" title={item.email}>{item.email ?? '-'}</TableCell>
                        <TableCell><Badge variant={importStatusVariant(item.status)}>{t(`accounts.codexImport.statuses.${item.status}`)}</Badge></TableCell>
                        <TableCell className="max-w-44 truncate text-xs text-muted-foreground" title={item.message}>{item.message ?? '-'}</TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              </div>
            </div>
          </>
        )}

        <DialogFooter>
          <Button variant="outline" onClick={() => setDialogOpen(false)}>{t('common.cancel')}</Button>
          <Button onClick={submit} disabled={mutation.isPending || templates.length === 0}>
            {mutation.isPending
              ? <Loader2 data-icon="inline-start" aria-hidden="true" className="animate-spin" />
              : <FileJson data-icon="inline-start" aria-hidden="true" />}
            {mutation.isPending ? t('accounts.codexImport.importing') : t('accounts.codexImport.submit')}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

function clampPercent(value: number): number {
  return Math.min(100, Math.max(0, value))
}

function UsageProgress({ value }: { value: number }) {
  return (
    <div className="h-1.5 overflow-hidden rounded-full bg-muted" aria-hidden="true">
      <div className="h-full bg-primary transition-[width]" style={{ width: `${clampPercent(value)}%` }} />
    </div>
  )
}

function UsageRow({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex items-center justify-between gap-4 py-1.5 text-sm">
      <span className="text-muted-foreground">{label}</span>
      <span className="text-right font-medium tabular-nums">{value}</span>
    </div>
  )
}

function formatUSD(value: number): string {
  return new Intl.NumberFormat(undefined, { style: 'currency', currency: 'USD', maximumFractionDigits: 4 }).format(value)
}

function formatCount(value: number): string {
  return new Intl.NumberFormat().format(value)
}

export function CodexUsagePopover({ accountID, accountName }: { accountID: number; accountName?: string }) {
  const { t } = useTranslation()
  const [open, setOpen] = useState(false)
  const query = useQuery({
    queryKey: ['account-usage', accountID],
    queryFn: async () => (await api.getAccountsUsage([accountID])).items[0],
    enabled: open,
    staleTime: 5 * 60_000,
    retry: false,
  })
  const usage: AccountUsage | undefined = query.data
  const upstream = usage?.upstream
  const rateLimit = upstream?.rate_limit
  const compact = rateLimit
    ? t('accounts.codexUsage.compact', { value: Math.round(100 - clampPercent(rateLimit.used_percent)) })
    : t('accounts.codexUsage.view')

  return (
    <Popover open={open} onOpenChange={setOpen}>
      <PopoverTrigger render={<Button variant="ghost" size="sm" className="h-8 min-w-24 justify-start px-2" />}>
        {query.isFetching && !usage
          ? <Loader2 data-icon="inline-start" aria-hidden="true" className="animate-spin" />
          : <Gauge data-icon="inline-start" aria-hidden="true" />}
        <span className="tabular-nums">{compact}</span>
      </PopoverTrigger>
      <PopoverContent align="end" className="w-80 p-4">
        <div className="min-w-0">
          <p className="font-medium">{t('accounts.codexUsage.title')}</p>
          <p className="truncate text-xs text-muted-foreground">{accountName || `#${accountID}`}</p>
        </div>

        {query.isLoading ? (
          <div className="space-y-3 py-4"><Skeleton className="h-12" /><Skeleton className="h-12" /><Skeleton className="h-12" /></div>
        ) : query.isError ? (
          <p className="py-4 text-sm text-destructive">{t('accounts.codexUsage.loadFailed', { message: (query.error as Error).message })}</p>
        ) : usage ? (
          <div className="mt-3 space-y-3">
            {usage.upstream_error && (
              <p className="text-sm text-destructive">{t(`accounts.codexUsage.errors.${usage.upstream_error}`)}</p>
            )}
            <div>
              <UsageRow label={t('accounts.codexUsage.plan')} value={upstream?.plan_type?.toUpperCase() || t('accounts.codexUsage.unavailable')} />
              {rateLimit ? (
                <div className="space-y-1.5 py-1.5">
                  <div className="flex items-center justify-between gap-4 text-sm">
                    <span className="text-muted-foreground">{t('accounts.codexUsage.rateLimit')}</span>
                    <span className="font-medium tabular-nums">{t('accounts.codexUsage.remaining', { value: Math.round(100 - clampPercent(rateLimit.used_percent)) })}</span>
                  </div>
                  <UsageProgress value={rateLimit.used_percent} />
                  <div className="flex justify-between gap-3 text-[11px] text-muted-foreground">
                    <span>{t('accounts.codexUsage.used', { value: Math.round(rateLimit.used_percent) })}</span>
                    <span>{rateLimit.reset_at ? t('accounts.codexUsage.resetsAt', { time: new Date(rateLimit.reset_at).toLocaleString() }) : t('accounts.codexUsage.resetUnknown')}</span>
                  </div>
                </div>
              ) : (
                <UsageRow label={t('accounts.codexUsage.rateLimit')} value={t('accounts.codexUsage.unavailable')} />
              )}
              {upstream?.credits && (
                <UsageRow label={t('accounts.codexUsage.creditBalance')} value={upstream.credits.balance ?? t('accounts.codexUsage.unavailable')} />
              )}
              {upstream?.spend_control && (
                <div className="space-y-1.5 py-1.5">
                  <UsageRow label={t('accounts.codexUsage.spend')} value={`${upstream.spend_control.used} / ${upstream.spend_control.limit}`} />
                  <UsageProgress value={upstream.spend_control.used_percent} />
                  <p className="text-right text-[11px] text-muted-foreground">
                    {t('accounts.codexUsage.spendRemaining', { value: upstream.spend_control.remaining })}
                  </p>
                </div>
              )}
            </div>

            <Separator />
            <div>
              <p className="mb-1 text-xs font-medium text-muted-foreground">{t('accounts.codexUsage.gateway')}</p>
              <UsageRow label={t('accounts.codexUsage.requests')} value={formatCount(usage.gateway.requests)} />
              <UsageRow label={t('accounts.codexUsage.tokens')} value={formatCount(usage.gateway.total_tokens)} />
              <UsageRow label={t('accounts.codexUsage.cost')} value={formatUSD(usage.gateway.cost_usd)} />
            </div>
          </div>
        ) : (
          <p className="py-4 text-sm text-muted-foreground">{t('accounts.codexUsage.unavailable')}</p>
        )}
      </PopoverContent>
    </Popover>
  )
}
