// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

import { useMemo, useState } from 'react'
import { useMutation } from '@tanstack/react-query'
import { Check, Copy, LoaderCircle } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { useAccountSurface } from '@/lib/api/scope'
import type { components } from '@/lib/api/schema.d.ts'
import { ApiError } from '@/lib/api/client'
import {
  buildProbeRequest,
  PROBE_MAX_IMAGE_URL_BYTES,
  PROBE_MAX_INPUT_BYTES,
  probeComboboxValue,
  probeImageURLByteLength,
  probeImageURLShapeValid,
  probeInputByteLength,
  probeModelsFor,
  probeTargetFormat,
  probeTargets,
  type ProbeTarget,
} from '@/lib/account-test'
import { copyText } from '@/components/key-box'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Combobox, ComboboxContent, ComboboxEmpty, ComboboxInput, ComboboxItem, ComboboxList } from '@/components/ui/combobox'
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { Textarea } from '@/components/ui/textarea'

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
  const targets = useMemo(() => probeTargets(template), [template])
  const defaultTarget = targets.includes('openai-chat') ? 'openai-chat' : (targets[0] ?? '')
  const [target, setTarget] = useState<ProbeTarget | ''>(defaultTarget)
  const targetFormat = target ? probeTargetFormat(target) : undefined
  const [model, setModel] = useState(() => defaultTarget && template ? (probeModelsFor(template, probeTargetFormat(defaultTarget)).models[0] ?? '') : '')
  const candidates = useMemo(
    () => targetFormat && template ? probeModelsFor(template, targetFormat) : { models: [], wildcard: false },
    [template, targetFormat],
  )
  const [inputText, setInputText] = useState('')
  const [imageURL, setImageURL] = useState('')
  const [fileError, setFileError] = useState<string | null>(null)
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
    onError: (error: Error) => {
      // 不显示服务端错误体原文：仅按状态区分“输入被拒”和“请求失败”。
      setRequestError(error instanceof ApiError && error.status === 400
        ? t('accounts.test.requestInvalid')
        : t('accounts.test.requestFailed'))
    },
  })

  const clearOutcome = () => {
    setResult(null)
    setRequestError(null)
    setCopied(false)
  }
  const changeTarget = (next: ProbeTarget) => {
    setTarget(next)
    const nextCandidates = template ? probeModelsFor(template, probeTargetFormat(next)) : { models: [], wildcard: false }
    setModel(current => nextCandidates.wildcard || nextCandidates.models.includes(current) ? current : '')
    if (next !== 'openai-images.edits') {
      setImageURL('')
      setFileError(null)
    }
    clearOutcome()
  }
  const changeModel = (next: string) => {
    setModel(next)
    clearOutcome()
  }
  const changeImageURL = (next: string) => {
    setImageURL(next)
    setFileError(null)
    clearOutcome()
  }
  const onImageFile = (file: File | undefined) => {
    setFileError(null)
    clearOutcome()
    if (!file) return
    if (file.type && !file.type.startsWith('image/')) {
      setFileError(t('accounts.test.imageFileType'))
      return
    }
    if (file.size > PROBE_MAX_IMAGE_URL_BYTES) {
      setFileError(t('accounts.test.imageUrlTooLarge'))
      return
    }
    const reader = new FileReader()
    reader.onerror = () => setFileError(t('accounts.test.imageFileRead'))
    reader.onload = () => {
      const dataURL = typeof reader.result === 'string' ? reader.result : ''
      if (dataURL === '' || probeImageURLByteLength(dataURL) > PROBE_MAX_IMAGE_URL_BYTES) {
        setFileError(t('accounts.test.imageUrlTooLarge'))
        return
      }
      setImageURL(dataURL)
      setFileError(null)
      clearOutcome()
    }
    reader.readAsDataURL(file)
  }

  const trimmed = model.trim()
  const modelValid = trimmed !== '' && (candidates.wildcard || candidates.models.includes(trimmed))
  const edits = target === 'openai-images.edits'
  const inputTooLarge = probeInputByteLength(inputText) > PROBE_MAX_INPUT_BYTES
  const imageURLBytes = probeImageURLByteLength(imageURL)
  const imageTooLarge = edits && imageURLBytes > PROBE_MAX_IMAGE_URL_BYTES
  const imageMissing = edits && imageURL.trim() === ''
  const imageInvalid = edits && !imageMissing && !imageTooLarge && !probeImageURLShapeValid(imageURL)
  // 远端 URL 不在浏览器里预取（避免管理员浏览器成为第二个取图方）；只预览
  // 本地文件/粘贴产生的 data URL。
  const imageSourcePreview = edits && !imageTooLarge && imageURL.trim().toLowerCase().startsWith('data:image/') && probeImageURLShapeValid(imageURL)
  const imageError = imageMissing
    ? t('accounts.test.imageUrlRequired')
    : imageTooLarge
      ? t('accounts.test.imageUrlTooLarge')
      : imageInvalid
        ? t('accounts.test.imageUrlInvalid')
        : null
  const disabledReason = targets.length === 0
    ? t('accounts.test.noFormats')
    : !candidates.wildcard && candidates.models.length === 0
      ? t('accounts.test.noModels')
      : trimmed === ''
        ? t('accounts.test.modelRequired')
        : null

  return (
    <DialogContent className="sm:max-w-xl">
      <DialogHeader>
        <DialogTitle>{t('accounts.test.title', { name: account.Name ?? '—', id: account.ID ?? 0 })}</DialogTitle>
      </DialogHeader>
      <div className="space-y-4">
        <div className="space-y-1.5">
          <Label htmlFor="account-test-format">{t('accounts.test.format')}</Label>
          <Select
            items={Object.fromEntries(targets.map(item => [item, item]))}
            value={target || null}
            onValueChange={value => changeTarget(value as ProbeTarget)}
            disabled={targets.length === 0 || probe.isPending}
          >
            <SelectTrigger id="account-test-format" className="w-full"><SelectValue /></SelectTrigger>
            <SelectContent>
              {targets.map(item => <SelectItem key={item} value={item} label={item}>{item}</SelectItem>)}
            </SelectContent>
          </Select>
          {targets.length === 0 && <p className="text-xs text-destructive">{t('accounts.test.noFormats')}</p>}
        </div>
        <div className="space-y-1.5">
          <Label htmlFor="account-test-model">{t('accounts.test.model')}</Label>
          <Combobox
            items={candidates.models}
            value={probeComboboxValue(model)}
            inputValue={model}
            onValueChange={next => changeModel(next ?? '')}
            onInputValueChange={changeModel}
            disabled={!target || probe.isPending || (!candidates.wildcard && candidates.models.length === 0)}
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
          {disabledReason && targets.length > 0 && <p className="text-xs text-muted-foreground">{disabledReason}</p>}
        </div>
        <div className="space-y-1.5">
          <Label htmlFor="account-test-input">{t('accounts.test.input')}</Label>
          <Textarea
            id="account-test-input"
            value={inputText}
            onChange={event => {
              setInputText(event.target.value)
              clearOutcome()
            }}
            placeholder={t('accounts.test.inputPlaceholder')}
            aria-invalid={inputTooLarge}
            rows={3}
            disabled={probe.isPending}
          />
          <p className="text-xs text-muted-foreground">{t('accounts.test.inputHint')}</p>
          {inputTooLarge && <p className="text-xs text-destructive">{t('accounts.test.inputTooLarge')}</p>}
        </div>
        {edits && (
          <div className="space-y-3 rounded-lg border bg-background/40 p-3">
            <div className="space-y-1.5">
              <Label htmlFor="account-test-image-url">{t('accounts.test.imageUrl')}</Label>
              <Input
                id="account-test-image-url"
                value={imageURL}
                onChange={event => changeImageURL(event.target.value)}
                placeholder={t('accounts.test.imageUrlPlaceholder')}
                aria-invalid={!!imageError}
                autoComplete="off"
                disabled={probe.isPending}
              />
              <p className="text-xs text-muted-foreground">{t('accounts.test.imageUrlHint')}</p>
              {imageError && <p className="text-xs text-destructive">{imageError}</p>}
            </div>
            {imageSourcePreview && (
              <div className="space-y-1.5">
                <Label>{t('accounts.test.imageSourcePreview')}</Label>
                <img
                  key={imageURL.trim()}
                  src={imageURL.trim()}
                  alt={t('accounts.test.imageSourcePreviewAlt')}
                  className="max-h-40 max-w-full rounded-md border object-contain"
                  loading="lazy"
                  decoding="async"
                />
              </div>
            )}
            <div className="space-y-1.5">
              <Label htmlFor="account-test-image-file">{t('accounts.test.imageFile')}</Label>
              <Input
                id="account-test-image-file"
                type="file"
                accept="image/*"
                onChange={event => {
                  const input = event.currentTarget
                  onImageFile(input.files?.[0])
                  input.value = ''
                }}
                disabled={probe.isPending}
              />
              {fileError && <p className="text-xs text-destructive">{fileError}</p>}
            </div>
          </div>
        )}
        {requestError && <p className="text-xs text-destructive" role="alert">{requestError}</p>}
        {result && (
          <div className="max-h-[55vh] space-y-3 overflow-y-auto rounded-lg border bg-background/40 p-3" role="status">
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
            {result.success && result.output_preview && (
              <div className="space-y-1.5">
                <Label>{t('accounts.test.outputPreview')}</Label>
                <pre className="max-h-48 overflow-auto whitespace-pre-wrap break-words rounded-md bg-muted/60 p-2 font-mono text-xs">{result.output_preview}</pre>
              </div>
            )}
            {result.success && (result.image_previews?.length ?? 0) > 0 && (
              <div className="space-y-1.5">
                <Label>{t('accounts.test.imagePreviews')}</Label>
                <div className="grid grid-cols-2 gap-2">
                  {result.image_previews.map((src, index) => (
                    <img
                      key={index}
                      src={src}
                      alt={t('accounts.test.imagePreviewAlt', { index: index + 1 })}
                      className="max-h-48 w-full rounded-md border object-contain"
                    />
                  ))}
                </div>
              </div>
            )}
            {result.success && !result.output_preview && (result.image_previews?.length ?? 0) === 0 && (
              <p className="text-xs text-muted-foreground">{t('accounts.test.noPreview')}</p>
            )}
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
          disabled={!target || !modelValid || probe.isPending || inputTooLarge || !!imageError}
          onClick={() => {
            if (!target || !modelValid || inputTooLarge || imageError) return
            probe.mutate(buildProbeRequest({ target, model: trimmed, input: inputText, imageURL }))
          }}
        >
          {probe.isPending && <LoaderCircle className="animate-spin" />}
          {probe.isPending ? t('accounts.test.testing') : t('accounts.test.start')}
        </Button>
      </DialogFooter>
    </DialogContent>
  )
}
