// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

import { useMemo, useState } from 'react'
import { useMutation } from '@tanstack/react-query'
import { Check, Copy, LoaderCircle } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { useAccountSurface } from '@/lib/api/scope'
import type { components } from '@/lib/api/schema.d.ts'
import { probeComboboxValue, probeFormats, probeModelsFor, type ProbeFormat } from '@/lib/account-test'
import { copyText } from '@/components/key-box'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Combobox, ComboboxContent, ComboboxEmpty, ComboboxInput, ComboboxItem, ComboboxList } from '@/components/ui/combobox'
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'

type AccountView = components['schemas']['AccountView']
type AccountTestResult = components['schemas']['AccountTestResult']

export type AccountTestDialogProps = {
  account: AccountView | null
  onOpenChange: (open: boolean) => void
}

export function AccountTestDialog({ account, onOpenChange }: AccountTestDialogProps) {
  return (
    <Dialog open={!!account} onOpenChange={onOpenChange}>
      {account && <AccountTestDialogContent key={account.ID} account={account} />}
    </Dialog>
  )
}

function AccountTestDialogContent({ account }: { account: AccountView }) {
  const { t } = useTranslation()
  const { api } = useAccountSurface()
  const template = account.Template
  const formats = useMemo(() => probeFormats(template), [template])
  const defaultFormat = formats.includes('openai-chat') ? 'openai-chat' : (formats[0] ?? '')
  const [format, setFormat] = useState<ProbeFormat | ''>(defaultFormat)
  const [model, setModel] = useState(() => defaultFormat && template ? (probeModelsFor(template, defaultFormat).models[0] ?? '') : '')
  const candidates = useMemo(
    () => format && template ? probeModelsFor(template, format) : { models: [], wildcard: false },
    [template, format],
  )
  const [result, setResult] = useState<AccountTestResult | null>(null)
  const [requestError, setRequestError] = useState<string | null>(null)
  const [copied, setCopied] = useState(false)

  const probe = useMutation({
    mutationFn: (body: components['schemas']['AccountTestRequest']) => api.testAccount(account.ID!, body),
    onMutate: () => {
      setResult(null)
      setRequestError(null)
      setCopied(false)
    },
    onSuccess: setResult,
    onError: (error: Error) => setRequestError(error.message),
  })

  const clearOutcome = () => {
    setResult(null)
    setRequestError(null)
    setCopied(false)
  }
  const changeFormat = (next: ProbeFormat) => {
    setFormat(next)
    const nextCandidates = template ? probeModelsFor(template, next) : { models: [], wildcard: false }
    setModel(current => nextCandidates.wildcard || nextCandidates.models.includes(current) ? current : '')
    clearOutcome()
  }
  const changeModel = (next: string) => {
    setModel(next)
    clearOutcome()
  }

  const trimmed = model.trim()
  const modelValid = trimmed !== '' && (candidates.wildcard || candidates.models.includes(trimmed))
  const disabledReason = formats.length === 0
    ? t('accounts.test.noFormats')
    : !candidates.wildcard && candidates.models.length === 0
      ? t('accounts.test.noModels')
      : trimmed === ''
        ? t('accounts.test.modelRequired')
        : null

  return (
    <DialogContent className="sm:max-w-lg">
      <DialogHeader>
        <DialogTitle>{t('accounts.test.title', { name: account.Name ?? '—', id: account.ID ?? 0 })}</DialogTitle>
      </DialogHeader>
      <div className="space-y-4">
        <div className="space-y-1.5">
          <Label htmlFor="account-test-format">{t('accounts.test.format')}</Label>
          <Select
            items={Object.fromEntries(formats.map(item => [item, item]))}
            value={format || null}
            onValueChange={value => changeFormat(value as ProbeFormat)}
            disabled={formats.length === 0 || probe.isPending}
          >
            <SelectTrigger id="account-test-format" className="w-full"><SelectValue /></SelectTrigger>
            <SelectContent>
              {formats.map(item => <SelectItem key={item} value={item} label={item}>{item}</SelectItem>)}
            </SelectContent>
          </Select>
          {formats.length === 0 && <p className="text-xs text-destructive">{t('accounts.test.noFormats')}</p>}
        </div>
        <div className="space-y-1.5">
          <Label htmlFor="account-test-model">{t('accounts.test.model')}</Label>
          <Combobox
            items={candidates.models}
            value={probeComboboxValue(model)}
            inputValue={model}
            onValueChange={next => changeModel(next ?? '')}
            onInputValueChange={changeModel}
            disabled={!format || probe.isPending || (!candidates.wildcard && candidates.models.length === 0)}
          >
            <ComboboxInput
              id="account-test-model"
              placeholder={t('accounts.test.modelPlaceholder')}
              autoComplete="none"
              showClear
            />
            <ComboboxContent>
              {!candidates.wildcard && <ComboboxEmpty>{t('accounts.test.noModels')}</ComboboxEmpty>}
              <ComboboxList>
                {candidates.models.map(item => <ComboboxItem key={item} value={item}>{item}</ComboboxItem>)}
              </ComboboxList>
            </ComboboxContent>
          </Combobox>
          {candidates.wildcard && <p className="text-xs text-muted-foreground">{t('accounts.test.wildcardHint')}</p>}
          {disabledReason && formats.length > 0 && <p className="text-xs text-muted-foreground">{disabledReason}</p>}
        </div>
        {requestError && <p className="text-xs text-destructive" role="alert">{requestError}</p>}
        {result && (
          <div className="space-y-3 rounded-lg border bg-background/40 p-3" role="status">
            <div className="flex items-center justify-between gap-2">
              <Badge variant={result.success ? 'default' : 'destructive'} className={result.success ? 'bg-emerald-600 text-white hover:bg-emerald-600' : undefined}>
                {result.success ? t('accounts.test.success') : t('accounts.test.failure')}
              </Badge>
              <span className="text-xs text-muted-foreground">{t('accounts.test.latency')}: {result.latency_ms} ms</span>
            </div>
            <div className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1 text-xs">
              <span className="text-muted-foreground">{t('accounts.test.statusCode')}</span>
              <span className="font-mono">{result.status_code}</span>
              <span className="text-muted-foreground">{t('accounts.test.error')}</span>
              <span className="break-all">{result.error || '—'}</span>
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="account-test-upstream-model">{t('accounts.test.upstreamModel')}</Label>
              <Input id="account-test-upstream-model" value={result.upstream_model} readOnly className="font-mono text-xs" />
            </div>
            <div className="space-y-1.5">
              <div className="flex items-center justify-between gap-2">
                <Label>{t('accounts.test.detail')}</Label>
                <Button
                  size="xs"
                  variant="outline"
                  disabled={!result.detail}
                  onClick={async () => {
                    if (await copyText(result.detail)) setCopied(true)
                  }}
                >
                  {copied ? <Check /> : <Copy />}
                  {copied ? t('accounts.test.copied') : t('accounts.test.copy')}
                </Button>
              </div>
              <pre className="max-h-32 overflow-auto whitespace-pre-wrap break-all rounded-md bg-muted/60 p-2 font-mono text-xs">{result.detail || '—'}</pre>
            </div>
          </div>
        )}
      </div>
      <DialogFooter>
        <Button
          disabled={!format || !modelValid || probe.isPending}
          onClick={() => {
            if (!format || !modelValid) return
            probe.mutate({ model: trimmed, format })
          }}
        >
          {probe.isPending && <LoaderCircle className="animate-spin" />}
          {probe.isPending ? t('accounts.test.testing') : t('accounts.test.start')}
        </Button>
      </DialogFooter>
    </DialogContent>
  )
}
