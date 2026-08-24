// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

import { useEffect, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { motion } from 'framer-motion'
import { Coins, Filter, Pencil, Plus, RefreshCw, Trash2 } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { api } from '@/App'
import { ApiUnauthorized } from '@/lib/api/client'
import { ListToolbar } from '@/components/list-toolbar'
import { PagePagination } from '@/components/page-pagination'
import { SortableHeader, type SortOrder } from '@/components/sortable-header'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { Skeleton } from '@/components/ui/skeleton'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { toast } from '@/components/ui/toast'
import { cn } from '@/lib/utils'
import { formatDateTime, formatPricePerMillion } from '@/components/fmt'
import type { components } from '@/lib/api/schema'

type PriceEntry = components['schemas']['PriceEntry']
type PriceEntryUpsert = components['schemas']['PriceEntryUpsert']
type PricingSource = components['schemas']['PricingSource']

type TabKey = 'text' | 'image' | 'function'
const MODE_BY_TAB: Record<TabKey, 'token' | 'image' | 'call'> = { text: 'token', image: 'image', function: 'call' }

const SOURCES: PricingSource[] = ['litellm', 'manual']

function SourceBadge({ source }: { source: PricingSource }) {
  const { t } = useTranslation()
  const manual = source === 'manual'
  return (
    <Badge variant="secondary" className={cn('gap-1.5', manual ? 'text-blue-700 dark:text-blue-400' : 'text-muted-foreground')}>
      <span className={cn('size-1.5 shrink-0 rounded-full', manual ? 'bg-blue-500' : 'bg-muted-foreground/60')} />
      {t(`pricing.source.${source}`)}
    </Badge>
  )
}

const formatUsd = (v: number | null | undefined): string => {
  if (v == null) return '—'
  if (v === 0) return '$0'
  return Math.abs(v) >= 0.0001 ? `$${v.toFixed(4)}` : `$${v.toExponential(2)}`
}

const isNonNegNum = (v: string) => v === '' || (Number.isFinite(Number(v)) && Number(v) >= 0)

// —— token 表单（4 字段全可选）；call/image 各自对应字段
interface TokenForm {
  model: string
  inputPerM: string
  outputPerM: string
  cacheReadPerM: string
  cacheWritePerM: string
}
interface ImageForm {
  model: string
  imgInTokPerM: string
  imgOutTokPerM: string
  pricePerImage: string
}
interface CallForm {
  model: string
  pricePerCall: string
}

const emptyTokenForm = (): TokenForm => ({ model: '', inputPerM: '', outputPerM: '', cacheReadPerM: '', cacheWritePerM: '' })
const emptyImageForm = (): ImageForm => ({ model: '', imgInTokPerM: '', imgOutTokPerM: '', pricePerImage: '' })
const emptyCallForm = (): CallForm => ({ model: '', pricePerCall: '' })

function toTokenForm(p: PriceEntry): TokenForm {
  return {
    model: p.Model,
    inputPerM: p.InputPerM == null ? '' : String(p.InputPerM),
    outputPerM: p.OutputPerM == null ? '' : String(p.OutputPerM),
    cacheReadPerM: p.CacheReadPerM == null ? '' : String(p.CacheReadPerM),
    cacheWritePerM: p.CacheWritePerM == null ? '' : String(p.CacheWritePerM),
  }
}
function toImageForm(p: PriceEntry): ImageForm {
  return {
    model: p.Model,
    imgInTokPerM: p.ImgInTokPerM == null ? '' : String(p.ImgInTokPerM),
    imgOutTokPerM: p.ImgOutTokPerM == null ? '' : String(p.ImgOutTokPerM),
    pricePerImage: p.PricePerImage == null ? '' : String(p.PricePerImage),
  }
}
function toCallForm(p: PriceEntry): CallForm {
  return { model: p.Model, pricePerCall: p.PricePerCall == null ? '' : String(p.PricePerCall) }
}

function tokenBody(f: TokenForm, mode: 'token'): PriceEntryUpsert {
  const b: PriceEntryUpsert = { mode }
  if (f.inputPerM !== '') b.input_per_m = Number(f.inputPerM)
  if (f.outputPerM !== '') b.output_per_m = Number(f.outputPerM)
  if (f.cacheReadPerM !== '') b.cache_read_per_m = Number(f.cacheReadPerM)
  if (f.cacheWritePerM !== '') b.cache_write_per_m = Number(f.cacheWritePerM)
  return b
}
function imageBody(f: ImageForm): PriceEntryUpsert {
  const b: PriceEntryUpsert = { mode: 'image' }
  if (f.imgInTokPerM !== '') b.img_in_tok_per_m = Number(f.imgInTokPerM)
  if (f.imgOutTokPerM !== '') b.img_out_tok_per_m = Number(f.imgOutTokPerM)
  if (f.pricePerImage !== '') b.price_per_image = Number(f.pricePerImage)
  return b
}
function callBody(f: CallForm): PriceEntryUpsert {
  return { mode: 'call', price_per_call: Number(f.pricePerCall) }
}

export default function PricingPage() {
  const { t } = useTranslation()
  const qc = useQueryClient()
  const [tab, setTab] = useState<TabKey>('text')

  // —— 文本价（token）状态 ——
  const [page, setPage] = useState(1)
  const [pageSize, setPageSize] = useState(20)
  const [model, setModel] = useState('')
  const [sourceFilter, setSourceFilter] = useState<'all' | PricingSource>('all')
  const [activeSort, setActiveSort] = useState<string | null>(null)
  const [order, setOrder] = useState<SortOrder>('desc')
  const sort = activeSort ?? 'model'
  const ord = activeSort ? order : 'asc'

  const { data: textData, isLoading: textLoading, isError: textIsError, error: textError } = useQuery({
    queryKey: ['prices', { mode: 'token', page, page_size: pageSize, source: sourceFilter, model, sort, order: ord }],
    queryFn: () =>
      api.listPriceEntries({
        page,
        page_size: pageSize,
        mode: 'token',
        source: sourceFilter === 'all' ? undefined : sourceFilter,
        model: model || undefined,
        sort,
        order: ord,
      }),
  })
  const textRows = textData?.rows ?? []
  useEffect(() => {
    if (!textLoading && !textIsError && textRows.length === 0 && page > 1) setPage(1)
  }, [textLoading, textIsError, textRows.length, page])
  const resetPage = () => setPage(1)
  const changePageSize = (s: number) => { setPageSize(s); resetPage() }
  const changeModel = (v: string) => { setModel(v); resetPage() }
  const changeSource = (v: string) => { setSourceFilter(v as 'all' | PricingSource); resetPage() }
  const onColumnToggle = (col: string) => {
    resetPage()
    if (activeSort !== col) { setActiveSort(col); setOrder('desc') }
    else if (order === 'desc') setOrder('asc')
    else { setActiveSort(null); setOrder('desc') }
  }
  const hasFilters = model !== '' || sourceFilter !== 'all'
  const clearFilters = () => { setModel(''); setSourceFilter('all'); resetPage() }

  // —— 图片价（image）状态 ——
  const [imgPage, setImgPage] = useState(1)
  const [imgPageSize, setImgPageSize] = useState(20)
  const [imgModel, setImgModel] = useState('')
  const [imgSource, setImgSource] = useState<'all' | PricingSource>('all')
  const [imgActiveSort, setImgActiveSort] = useState<string | null>(null)
  const [imgOrder, setImgOrder] = useState<SortOrder>('desc')
  const imgSort = imgActiveSort ?? 'model'
  const imgOrd = imgActiveSort ? imgOrder : 'asc'
  const { data: imgData, isLoading: imgLoading, isError: imgIsError, error: imgError } = useQuery({
    queryKey: ['prices', { mode: 'image', page: imgPage, page_size: imgPageSize, source: imgSource, model: imgModel, sort: imgSort, order: imgOrd }],
    queryFn: () =>
      api.listPriceEntries({
        page: imgPage,
        page_size: imgPageSize,
        mode: 'image',
        source: imgSource === 'all' ? undefined : imgSource,
        model: imgModel || undefined,
        sort: imgSort,
        order: imgOrd,
      }),
  })
  const imgRows = imgData?.rows ?? []
  useEffect(() => {
    if (!imgLoading && !imgIsError && imgRows.length === 0 && imgPage > 1) setImgPage(1)
  }, [imgLoading, imgIsError, imgRows.length, imgPage])
  const imgReset = () => setImgPage(1)
  const imgSetPageSize = (s: number) => { setImgPageSize(s); imgReset() }
  const imgSetModel = (v: string) => { setImgModel(v); imgReset() }
  const imgSetSource = (v: string) => { setImgSource(v as 'all' | PricingSource); imgReset() }
  const imgToggleSort = (col: string) => {
    imgReset()
    if (imgActiveSort !== col) { setImgActiveSort(col); setImgOrder('desc') }
    else if (imgOrder === 'desc') setImgOrder('asc')
    else { setImgActiveSort(null); setImgOrder('desc') }
  }
  const imgHasFilters = imgModel !== '' || imgSource !== 'all'
  const imgClearFilters = () => { setImgModel(''); setImgSource('all'); imgReset() }

  // —— 按次价（call）状态 ——
  const [fnPage, setFnPage] = useState(1)
  const [fnPageSize, setFnPageSize] = useState(20)
  const [fnModel, setFnModel] = useState('')
  const [fnSource, setFnSource] = useState<'all' | PricingSource>('all')
  const [fnActiveSort, setFnActiveSort] = useState<string | null>(null)
  const [fnOrder, setFnOrder] = useState<SortOrder>('desc')
  const fnSort = fnActiveSort ?? 'model'
  const fnOrd = fnActiveSort ? fnOrder : 'asc'
  const { data: fnData, isLoading: fnLoading, isError: fnIsError, error: fnError } = useQuery({
    queryKey: ['prices', { mode: 'call', page: fnPage, page_size: fnPageSize, source: fnSource, model: fnModel, sort: fnSort, order: fnOrd }],
    queryFn: () =>
      api.listPriceEntries({
        page: fnPage,
        page_size: fnPageSize,
        mode: 'call',
        source: fnSource === 'all' ? undefined : fnSource,
        model: fnModel || undefined,
        sort: fnSort,
        order: fnOrd,
      }),
  })
  const fnRows = fnData?.rows ?? []
  useEffect(() => {
    if (!fnLoading && !fnIsError && fnRows.length === 0 && fnPage > 1) setFnPage(1)
  }, [fnLoading, fnIsError, fnRows.length, fnPage])
  const fnReset = () => setFnPage(1)
  const fnSetPageSize = (s: number) => { setFnPageSize(s); fnReset() }
  const fnSetModel = (v: string) => { setFnModel(v); fnReset() }
  const fnSetSource = (v: string) => { setFnSource(v as 'all' | PricingSource); fnReset() }
  const fnToggleSort = (col: string) => {
    fnReset()
    if (fnActiveSort !== col) { setFnActiveSort(col); setFnOrder('desc') }
    else if (fnOrder === 'desc') setFnOrder('asc')
    else { setFnActiveSort(null); setFnOrder('desc') }
  }
  const fnHasFilters = fnModel !== '' || fnSource !== 'all'
  const fnClearFilters = () => { setFnModel(''); setFnSource('all'); fnReset() }

  // —— 价格同步 ——
  const sync = useMutation({
    mutationFn: () => api.syncPricing(),
    onSuccess: res => {
      toast.add({ title: t('pricing.syncDone', { rows: res.rows, skipped: res.skipped, updated: res.updated }), type: 'success' })
      qc.invalidateQueries({ queryKey: ['prices'] })
    },
    onError: (e: Error) => toast.add({ title: e.message, type: 'error' }),
  })

  // —— 文本（token）对话框 ——
  const [dialogOpen, setDialogOpen] = useState(false)
  const [editing, setEditing] = useState<PriceEntry | null>(null)
  const [form, setForm] = useState<TokenForm>(emptyTokenForm())
  const [formErr, setFormErr] = useState<string | null>(null)
  const openCreate = () => {
    if (tab === 'image') openImageCreate()
    else if (tab === 'function') openCallCreate()
    else {
      setEditing(null); setForm(emptyTokenForm()); setFormErr(null); setDialogOpen(true)
    }
  }
  const openEdit = (p: PriceEntry) => {
    // route to correct dialog by mode
    if (p.Mode === 'image') { setImgEditing(p); setImgForm(toImageForm(p)); setImgFormErr(null); setImgDialogOpen(true) }
    else if (p.Mode === 'call') { setFnEditing(p); setFnForm(toCallForm(p)); setFnFormErr(null); setFnDialogOpen(true) }
    else { setEditing(p); setForm(toTokenForm(p)); setFormErr(null); setDialogOpen(true) }
  }
  const save = useMutation({
    mutationFn: (f: TokenForm) => api.upsertPriceEntry(editing ? editing.Model : f.model.trim(), tokenBody(f, 'token')),
    onSuccess: () => { toast.add({ title: t('pricing.saved'), type: 'success' }); qc.invalidateQueries({ queryKey: ['prices'] }); setDialogOpen(false) },
  })
  const submit = () => {
    const fm = form
    const valid = (editing || fm.model.trim() !== '') && isNonNegNum(fm.inputPerM) && isNonNegNum(fm.outputPerM) && isNonNegNum(fm.cacheReadPerM) && isNonNegNum(fm.cacheWritePerM)
    if (!valid) { setFormErr(t('pricing.formInvalid')); return }
    save.mutate(fm)
  }

  const [deleting, setDeleting] = useState<PriceEntry | null>(null)
  const del = useMutation({
    mutationFn: (p: PriceEntry) => api.deletePriceEntry(p.Model),
    onSuccess: () => { toast.add({ title: t('pricing.deleted'), type: 'success' }); qc.invalidateQueries({ queryKey: ['prices'] }); setDeleting(null) },
  })

  // —— 图片（image）对话框 ——
  const [imgDialogOpen, setImgDialogOpen] = useState(false)
  const [imgEditing, setImgEditing] = useState<PriceEntry | null>(null)
  const [imgForm, setImgForm] = useState<ImageForm>(emptyImageForm())
  const [imgFormErr, setImgFormErr] = useState<string | null>(null)
  const openImageCreate = () => { setImgEditing(null); setImgForm(emptyImageForm()); setImgFormErr(null); setImgDialogOpen(true) }
  const setImg = (k: keyof ImageForm, v: string) => { setImgForm(f => ({ ...f, [k]: v })); setImgFormErr(null) }
  const imgSave = useMutation({
    mutationFn: (f: ImageForm) => api.upsertPriceEntry(imgEditing ? imgEditing.Model : f.model.trim(), imageBody(f)),
    onSuccess: () => { toast.add({ title: t('pricing.saved'), type: 'success' }); qc.invalidateQueries({ queryKey: ['prices'] }); setImgDialogOpen(false) },
  })
  const imgSubmit = () => {
    const fm = imgForm
    const valid = (imgEditing || fm.model.trim() !== '') && (fm.imgInTokPerM !== '' || fm.imgOutTokPerM !== '' || fm.pricePerImage !== '') && isNonNegNum(fm.imgInTokPerM) && isNonNegNum(fm.imgOutTokPerM) && isNonNegNum(fm.pricePerImage)
    if (!valid) { setImgFormErr(t('pricing.image.formInvalid')); return }
    imgSave.mutate(fm)
  }
  const [imgDeleting, setImgDeleting] = useState<PriceEntry | null>(null)
  const imgDel = useMutation({
    mutationFn: (p: PriceEntry) => api.deletePriceEntry(p.Model),
    onSuccess: () => { toast.add({ title: t('pricing.deleted'), type: 'success' }); qc.invalidateQueries({ queryKey: ['prices'] }); setImgDeleting(null) },
  })

  // —— 按次（call）对话框 ——
  const [fnDialogOpen, setFnDialogOpen] = useState(false)
  const [fnEditing, setFnEditing] = useState<PriceEntry | null>(null)
  const [fnForm, setFnForm] = useState<CallForm>(emptyCallForm())
  const [fnFormErr, setFnFormErr] = useState<string | null>(null)
  const openCallCreate = () => { setFnEditing(null); setFnForm(emptyCallForm()); setFnFormErr(null); setFnDialogOpen(true) }
  const setFn = (k: keyof CallForm, v: string) => { setFnForm(f => ({ ...f, [k]: v })); setFnFormErr(null) }
  const fnSave = useMutation({
    mutationFn: (f: CallForm) => api.upsertPriceEntry(fnEditing ? fnEditing.Model : f.model.trim(), callBody(f)),
    onSuccess: () => { toast.add({ title: t('pricing.saved'), type: 'success' }); qc.invalidateQueries({ queryKey: ['prices'] }); setFnDialogOpen(false) },
  })
  const fnSubmit = () => {
    const fm = fnForm
    const v = Number(fm.pricePerCall)
    const valid = (fnEditing || fm.model.trim() !== '') && fm.pricePerCall !== '' && Number.isFinite(v) && v >= 0
    if (!valid) { setFnFormErr(t('pricing.function.formInvalid')); return }
    fnSave.mutate(fm)
  }
  const [fnDeleting, setFnDeleting] = useState<PriceEntry | null>(null)
  const fnDel = useMutation({
    mutationFn: (p: PriceEntry) => api.deletePriceEntry(p.Model),
    onSuccess: () => { toast.add({ title: t('pricing.deleted'), type: 'success' }); qc.invalidateQueries({ queryKey: ['prices'] }); setFnDeleting(null) },
  })

  const errMsg = (e: unknown) => (e instanceof ApiUnauthorized ? null : (e as Error)?.message)
  const sourceItems = Object.fromEntries([['all', t('pricing.all')], ...SOURCES.map(s => [s, t(`pricing.source.${s}`)])])
  const delDisabledTitle = (source: PricingSource) => source === 'litellm' ? t('pricing.deleteLitellmHint') : t('pricing.deleteTitle')
  void MODE_BY_TAB

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-semibold tracking-tight">{t('pricing.title')}</h1>
          <p className="text-sm text-muted-foreground">{t('pricing.subtitle')}</p>
        </div>
        <div className="flex items-center gap-2">
          <Button variant="outline" onClick={() => sync.mutate()} disabled={sync.isPending}>
            <RefreshCw className={sync.isPending ? 'animate-spin' : ''} />
            {sync.isPending ? t('pricing.syncing') : t('pricing.sync')}
          </Button>
          <Button onClick={openCreate}>
            <Plus />
            {tab === 'image' ? t('pricing.image.new') : tab === 'function' ? t('pricing.function.new') : t('pricing.new')}
          </Button>
        </div>
      </div>

      <Tabs value={tab} onValueChange={v => v && setTab(v as TabKey)}>
        <TabsList className="w-full">
          <TabsTrigger value="text" className="flex-1">{t('pricing.tabs.text')}</TabsTrigger>
          <TabsTrigger value="image" className="flex-1">{t('pricing.tabs.image')}</TabsTrigger>
          <TabsTrigger value="function" className="flex-1">{t('pricing.tabs.function')}</TabsTrigger>
        </TabsList>

        {/* —— Tab 1：文本价格（token mode） —— */}
        <TabsContent value="text" className="space-y-6 pt-4">
          <ListToolbar name={model} onNameChange={changeModel} placeholder={t('pricing.searchModel')}>
            <Select items={sourceItems} value={sourceFilter} onValueChange={changeSource}>
              <SelectTrigger size="default" className="w-40" aria-label={t('pricing.all')}>
                <SelectValue placeholder={t('pricing.all')} />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="all" label={t('pricing.all')}>{t('pricing.all')}</SelectItem>
                {SOURCES.map(s => <SelectItem key={s} value={s} label={t(`pricing.source.${s}`)}>{t(`pricing.source.${s}`)}</SelectItem>)}
              </SelectContent>
            </Select>
            {sourceFilter !== 'all' && (
              <Button variant="ghost" size="lg" onClick={clearFilters}><Filter /> {t('list.reset')}</Button>
            )}
          </ListToolbar>

          {textIsError ? (
            <p className="text-sm text-destructive">{t('common.loadFailed', { message: (textError as Error).message })}</p>
          ) : textLoading ? (
            <div className="space-y-2">
              {Array.from({ length: 4 }).map((_, i) => <Skeleton key={i} className="h-12" />)}
            </div>
          ) : textRows.length === 0 ? (
            <motion.div initial={{ opacity: 0, y: 12 }} animate={{ opacity: 1, y: 0 }} transition={{ duration: 0.25 }}>
              <Card className="flex flex-col items-center gap-2 py-12 text-muted-foreground">
                <Coins className="size-10" />
                <p className="font-medium">{hasFilters ? t('pricing.filterEmpty') : t('pricing.emptyTitle')}</p>
                {!hasFilters && <p className="text-sm">{t('pricing.emptyDesc')}</p>}
                {hasFilters ? (
                  <Button className="mt-2" variant="outline" onClick={clearFilters}><Filter /> {t('list.reset')}</Button>
                ) : (
                  <Button className="mt-2" onClick={openCreate}><Plus /> {t('pricing.new')}</Button>
                )}
              </Card>
            </motion.div>
          ) : (
            <>
              <div className="overflow-hidden rounded-lg">
                <Table>
                  <TableHeader>
                    <TableRow>
                      <SortableHeader field="model" label={t('pricing.table.model')} active={activeSort === 'model'} order={order} onToggle={onColumnToggle} />
                      <TableHead className="text-right">{t('pricing.table.prompt')}</TableHead>
                      <TableHead className="text-right">{t('pricing.table.completion')}</TableHead>
                      <TableHead className="text-right">{t('pricing.table.cacheRead')}</TableHead>
                      <TableHead className="text-right">{t('pricing.table.cacheWrite')}</TableHead>
                      <TableHead>{t('pricing.table.source')}</TableHead>
                      <TableHead>{t('pricing.table.provider')}</TableHead>
                      <SortableHeader field="updated_at" label={t('pricing.table.updatedAt')} active={activeSort === 'updated_at'} order={order} onToggle={onColumnToggle} />
                      <TableHead className="text-right">{t('pricing.table.actions')}</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody className="[&_td]:py-3">
                    {textRows.map(p => (
                      <TableRow key={p.Model}>
                        <TableCell className="max-w-48 truncate font-mono text-sm" title={p.Model}>{p.Model}</TableCell>
                        <TableCell className="text-right tabular-nums">{formatPricePerMillion(p.InputPerM)}</TableCell>
                        <TableCell className="text-right tabular-nums">{formatPricePerMillion(p.OutputPerM)}</TableCell>
                        <TableCell className="text-right tabular-nums">{formatPricePerMillion(p.CacheReadPerM)}</TableCell>
                        <TableCell className="text-right tabular-nums">{formatPricePerMillion(p.CacheWritePerM)}</TableCell>
                        <TableCell><SourceBadge source={p.Source} /></TableCell>
                        <TableCell className="max-w-32 truncate" title={p.Provider ?? undefined}>{p.Provider || '—'}</TableCell>
                        <TableCell className="text-xs text-muted-foreground whitespace-nowrap">{formatDateTime(p.UpdatedAt)}</TableCell>
                        <TableCell className="text-right">
                          <div className="flex justify-end gap-1">
                            <Button variant="ghost" size="icon-sm" title={t('common.edit')} onClick={() => openEdit(p)}><Pencil /></Button>
                            <Button
                              variant="ghost"
                              size="icon-sm"
                              className="text-destructive"
                              title={delDisabledTitle(p.Source)}
                              onClick={() => setDeleting(p)}
                              disabled={p.Source === 'litellm' || del.isPending}
                            >
                              <Trash2 />
                            </Button>
                          </div>
                        </TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              </div>
              <PagePagination total={textData?.total ?? 0} pageSize={pageSize} page={page} onPageChange={setPage} onPageSizeChange={changePageSize} />
            </>
          )}
        </TabsContent>

        {/* —— Tab 2：图片价格（image mode） —— */}
        <TabsContent value="image" className="space-y-6 pt-4">
          <ListToolbar name={imgModel} onNameChange={imgSetModel} placeholder={t('pricing.searchModel')}>
            <Select items={sourceItems} value={imgSource} onValueChange={imgSetSource}>
              <SelectTrigger size="default" className="w-40" aria-label={t('pricing.all')}>
                <SelectValue placeholder={t('pricing.all')} />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="all" label={t('pricing.all')}>{t('pricing.all')}</SelectItem>
                {SOURCES.map(s => <SelectItem key={s} value={s} label={t(`pricing.source.${s}`)}>{t(`pricing.source.${s}`)}</SelectItem>)}
              </SelectContent>
            </Select>
            {imgSource !== 'all' && (
              <Button variant="ghost" size="lg" onClick={imgClearFilters}><Filter /> {t('list.reset')}</Button>
            )}
          </ListToolbar>

          {imgIsError ? (
            <p className="text-sm text-destructive">{t('common.loadFailed', { message: (imgError as Error).message })}</p>
          ) : imgLoading ? (
            <div className="space-y-2">
              {Array.from({ length: 4 }).map((_, i) => <Skeleton key={i} className="h-12" />)}
            </div>
          ) : imgRows.length === 0 ? (
            <motion.div initial={{ opacity: 0, y: 12 }} animate={{ opacity: 1, y: 0 }} transition={{ duration: 0.25 }}>
              <Card className="flex flex-col items-center gap-2 py-12 text-muted-foreground">
                <Coins className="size-10" />
                <p className="font-medium">{imgHasFilters ? t('pricing.image.filterEmpty') : t('pricing.image.emptyTitle')}</p>
                {!imgHasFilters && <p className="text-sm">{t('pricing.image.emptyDesc')}</p>}
                {imgHasFilters ? (
                  <Button className="mt-2" variant="outline" onClick={imgClearFilters}><Filter /> {t('list.reset')}</Button>
                ) : (
                  <Button className="mt-2" onClick={openImageCreate}><Plus /> {t('pricing.image.new')}</Button>
                )}
              </Card>
            </motion.div>
          ) : (
            <>
              <div className="overflow-hidden rounded-lg">
                <Table>
                  <TableHeader>
                    <TableRow>
                      <SortableHeader field="model" label={t('pricing.table.model')} active={imgActiveSort === 'model'} order={imgOrder} onToggle={imgToggleSort} />
                      <TableHead className="text-right" title="USD/1M image tokens">{t('pricing.image.table.inputToken')}</TableHead>
                      <TableHead className="text-right" title="USD/1M image tokens">{t('pricing.image.table.outputToken')}</TableHead>
                      <TableHead className="text-right" title="USD/张">{t('pricing.image.table.perImage')}</TableHead>
                      <TableHead>{t('pricing.table.source')}</TableHead>
                      <TableHead>{t('pricing.table.provider')}</TableHead>
                      <SortableHeader field="updated_at" label={t('pricing.table.updatedAt')} active={imgActiveSort === 'updated_at'} order={imgOrder} onToggle={imgToggleSort} />
                      <TableHead className="text-right">{t('pricing.table.actions')}</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody className="[&_td]:py-3">
                    {imgRows.map(p => (
                      <TableRow key={p.Model}>
                        <TableCell className="max-w-48 truncate font-mono text-sm" title={p.Model}>{p.Model}</TableCell>
                        <TableCell className="text-right tabular-nums">{formatUsd(p.ImgInTokPerM)}</TableCell>
                        <TableCell className="text-right tabular-nums">{formatUsd(p.ImgOutTokPerM)}</TableCell>
                        <TableCell className="text-right tabular-nums">{formatUsd(p.PricePerImage)}</TableCell>
                        <TableCell><SourceBadge source={p.Source} /></TableCell>
                        <TableCell className="max-w-32 truncate" title={p.Provider ?? undefined}>{p.Provider || '—'}</TableCell>
                        <TableCell className="text-xs text-muted-foreground whitespace-nowrap">{formatDateTime(p.UpdatedAt)}</TableCell>
                        <TableCell className="text-right">
                          <div className="flex justify-end gap-1">
                            <Button variant="ghost" size="icon-sm" title={t('common.edit')} onClick={() => openEdit(p)}><Pencil /></Button>
                            <Button
                              variant="ghost"
                              size="icon-sm"
                              className="text-destructive"
                              title={delDisabledTitle(p.Source)}
                              onClick={() => setImgDeleting(p)}
                              disabled={p.Source === 'litellm' || imgDel.isPending}
                            >
                              <Trash2 />
                            </Button>
                          </div>
                        </TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              </div>
              <PagePagination total={imgData?.total ?? 0} pageSize={imgPageSize} page={imgPage} onPageChange={setImgPage} onPageSizeChange={imgSetPageSize} />
            </>
          )}
        </TabsContent>

        {/* —— Tab 3：按次价格（call mode） —— */}
        <TabsContent value="function" className="space-y-6 pt-4">
          <ListToolbar name={fnModel} onNameChange={fnSetModel} placeholder={t('pricing.searchModel')}>
            <Select items={sourceItems} value={fnSource} onValueChange={fnSetSource}>
              <SelectTrigger size="default" className="w-40" aria-label={t('pricing.all')}>
                <SelectValue placeholder={t('pricing.all')} />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="all" label={t('pricing.all')}>{t('pricing.all')}</SelectItem>
                {SOURCES.map(s => <SelectItem key={s} value={s} label={t(`pricing.source.${s}`)}>{t(`pricing.source.${s}`)}</SelectItem>)}
              </SelectContent>
            </Select>
            {fnSource !== 'all' && (
              <Button variant="ghost" size="lg" onClick={fnClearFilters}><Filter /> {t('list.reset')}</Button>
            )}
          </ListToolbar>

          {fnIsError ? (
            <p className="text-sm text-destructive">{t('common.loadFailed', { message: (fnError as Error).message })}</p>
          ) : fnLoading ? (
            <div className="space-y-2">
              {Array.from({ length: 4 }).map((_, i) => <Skeleton key={i} className="h-12" />)}
            </div>
          ) : fnRows.length === 0 ? (
            <motion.div initial={{ opacity: 0, y: 12 }} animate={{ opacity: 1, y: 0 }} transition={{ duration: 0.25 }}>
              <Card className="flex flex-col items-center gap-2 py-12 text-muted-foreground">
                <Coins className="size-10" />
                <p className="font-medium">{fnHasFilters ? t('pricing.function.filterEmpty') : t('pricing.function.emptyTitle')}</p>
                {!fnHasFilters && <p className="text-sm">{t('pricing.function.emptyDesc')}</p>}
                {fnHasFilters ? (
                  <Button className="mt-2" variant="outline" onClick={fnClearFilters}><Filter /> {t('list.reset')}</Button>
                ) : (
                  <Button className="mt-2" onClick={openCallCreate}><Plus /> {t('pricing.function.new')}</Button>
                )}
              </Card>
            </motion.div>
          ) : (
            <>
              <div className="overflow-hidden rounded-lg">
                <Table>
                  <TableHeader>
                    <TableRow>
                      <SortableHeader field="model" label={t('pricing.table.model')} active={fnActiveSort === 'model'} order={fnOrder} onToggle={fnToggleSort} />
                      <TableHead className="text-right" title="USD/次">{t('pricing.function.table.price')}</TableHead>
                      <TableHead>{t('pricing.table.source')}</TableHead>
                      <TableHead>{t('pricing.table.provider')}</TableHead>
                      <SortableHeader field="updated_at" label={t('pricing.table.updatedAt')} active={fnActiveSort === 'updated_at'} order={fnOrder} onToggle={fnToggleSort} />
                      <TableHead className="text-right">{t('pricing.table.actions')}</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody className="[&_td]:py-3">
                    {fnRows.map(p => (
                      <TableRow key={p.Model}>
                        <TableCell className="max-w-48 truncate font-mono text-sm" title={p.Model}>{p.Model}</TableCell>
                        <TableCell className="text-right tabular-nums">{formatUsd(p.PricePerCall)}</TableCell>
                        <TableCell><SourceBadge source={p.Source} /></TableCell>
                        <TableCell className="max-w-32 truncate" title={p.Provider ?? undefined}>{p.Provider || '—'}</TableCell>
                        <TableCell className="text-xs text-muted-foreground whitespace-nowrap">{formatDateTime(p.UpdatedAt)}</TableCell>
                        <TableCell className="text-right">
                          <div className="flex justify-end gap-1">
                            <Button variant="ghost" size="icon-sm" title={t('common.edit')} onClick={() => openEdit(p)}><Pencil /></Button>
                            <Button
                              variant="ghost"
                              size="icon-sm"
                              className="text-destructive"
                              title={delDisabledTitle(p.Source)}
                              onClick={() => setFnDeleting(p)}
                              disabled={p.Source === 'litellm' || fnDel.isPending}
                            >
                              <Trash2 />
                            </Button>
                          </div>
                        </TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              </div>
              <PagePagination total={fnData?.total ?? 0} pageSize={fnPageSize} page={fnPage} onPageChange={setFnPage} onPageSizeChange={fnSetPageSize} />
            </>
          )}
        </TabsContent>
      </Tabs>

      {/* —— 文本价对话框 —— */}
      <Dialog open={dialogOpen} onOpenChange={o => { if (!o && !save.isPending) setDialogOpen(false) }}>
        <DialogContent className="sm:max-w-lg">
          <DialogHeader>
            <DialogTitle>{editing ? t('pricing.editTitle', { model: editing.Model }) : t('pricing.newTitle')}</DialogTitle>
            <DialogDescription>{t('pricing.dialogDesc')}</DialogDescription>
          </DialogHeader>
          <div className="space-y-3">
            <div className="space-y-1.5">
              <Label htmlFor="pf-model">{t('pricing.modelLabel')} <span className="text-destructive">*</span></Label>
              <Input
                id="pf-model"
                value={form.model}
                placeholder={t('pricing.modelPlaceholder')}
                onChange={e => { setForm(f => ({ ...f, model: e.target.value })); setFormErr(null) }}
                disabled={!!editing}
              />
              {editing?.Source === 'litellm' && (
                <p className="text-xs text-muted-foreground">{t('pricing.takeoverHint')}</p>
              )}
            </div>
            <div className="grid grid-cols-2 gap-3">
              <div className="space-y-1.5">
                <Label htmlFor="pf-input">{t('pricing.promptLabel')}</Label>
                <Input id="pf-input" type="number" min={0} step="any" value={form.inputPerM} onChange={e => { setForm(f => ({ ...f, inputPerM: e.target.value })); setFormErr(null) }} />
              </div>
              <div className="space-y-1.5">
                <Label htmlFor="pf-output">{t('pricing.completionLabel')}</Label>
                <Input id="pf-output" type="number" min={0} step="any" value={form.outputPerM} onChange={e => { setForm(f => ({ ...f, outputPerM: e.target.value })); setFormErr(null) }} />
              </div>
              <div className="space-y-1.5">
                <Label htmlFor="pf-cache-read">{t('pricing.cacheReadLabel')}</Label>
                <Input id="pf-cache-read" type="number" min={0} step="any" value={form.cacheReadPerM} onChange={e => { setForm(f => ({ ...f, cacheReadPerM: e.target.value })); setFormErr(null) }} />
              </div>
              <div className="space-y-1.5">
                <Label htmlFor="pf-cache-write">{t('pricing.cacheWriteLabel')}</Label>
                <Input id="pf-cache-write" type="number" min={0} step="any" value={form.cacheWritePerM} onChange={e => { setForm(f => ({ ...f, cacheWritePerM: e.target.value })); setFormErr(null) }} />
              </div>
            </div>
          </div>
          {formErr && <p className="text-sm text-destructive">{formErr}</p>}
          {save.isError && errMsg(save.error) && (
            <p className="text-sm text-destructive">{errMsg(save.error)}</p>
          )}
          <DialogFooter>
            <Button variant="outline" onClick={() => setDialogOpen(false)} disabled={save.isPending}>{t('common.cancel')}</Button>
            <Button onClick={submit} disabled={save.isPending}>
              {save.isPending ? t('common.saving') : t('common.save')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* —— 图片价对话框 —— */}
      <Dialog open={imgDialogOpen} onOpenChange={o => { if (!o && !imgSave.isPending) setImgDialogOpen(false) }}>
        <DialogContent className="sm:max-w-md">
          <DialogHeader>
            <DialogTitle>{imgEditing ? t('pricing.image.editTitle', { model: imgEditing.Model }) : t('pricing.image.newTitle')}</DialogTitle>
            <DialogDescription>{t('pricing.image.dialogDesc')}</DialogDescription>
          </DialogHeader>
          <div className="space-y-3">
            <div className="space-y-1.5">
              <Label htmlFor="im-model">{t('pricing.modelLabel')} <span className="text-destructive">*</span></Label>
              <Input
                id="im-model"
                value={imgForm.model}
                placeholder={t('pricing.modelPlaceholder')}
                onChange={e => setImg('model', e.target.value)}
                disabled={!!imgEditing}
              />
              {imgEditing?.Source === 'litellm' && (
                <p className="text-xs text-muted-foreground">{t('pricing.takeoverHint')}</p>
              )}
            </div>
            <div className="grid grid-cols-1 gap-3">
              <div className="space-y-1.5">
                <Label htmlFor="im-input">{t('pricing.image.inputTokenLabel')}</Label>
                <Input id="im-input" type="number" min={0} step="any" value={imgForm.imgInTokPerM} onChange={e => setImg('imgInTokPerM', e.target.value)} />
              </div>
              <div className="space-y-1.5">
                <Label htmlFor="im-output">{t('pricing.image.outputTokenLabel')}</Label>
                <Input id="im-output" type="number" min={0} step="any" value={imgForm.imgOutTokPerM} onChange={e => setImg('imgOutTokPerM', e.target.value)} />
              </div>
              <div className="space-y-1.5">
                <Label htmlFor="im-per-image">{t('pricing.image.perImageLabel')}</Label>
                <Input id="im-per-image" type="number" min={0} step="any" value={imgForm.pricePerImage} onChange={e => setImg('pricePerImage', e.target.value)} />
              </div>
            </div>
          </div>
          {imgFormErr && <p className="text-sm text-destructive">{imgFormErr}</p>}
          {imgSave.isError && errMsg(imgSave.error) && (
            <p className="text-sm text-destructive">{errMsg(imgSave.error)}</p>
          )}
          <DialogFooter>
            <Button variant="outline" onClick={() => setImgDialogOpen(false)} disabled={imgSave.isPending}>{t('common.cancel')}</Button>
            <Button onClick={imgSubmit} disabled={imgSave.isPending}>
              {imgSave.isPending ? t('common.saving') : t('common.save')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* —— 按次价对话框 —— */}
      <Dialog open={fnDialogOpen} onOpenChange={o => { if (!o && !fnSave.isPending) setFnDialogOpen(false) }}>
        <DialogContent className="sm:max-w-md">
          <DialogHeader>
            <DialogTitle>{fnEditing ? t('pricing.function.editTitle', { model: fnEditing.Model }) : t('pricing.function.newTitle')}</DialogTitle>
            <DialogDescription>{t('pricing.function.dialogDesc')}</DialogDescription>
          </DialogHeader>
          <div className="space-y-3">
            <div className="space-y-1.5">
              <Label htmlFor="fn-model">{t('pricing.modelLabel')} <span className="text-destructive">*</span></Label>
              <Input
                id="fn-model"
                value={fnForm.model}
                placeholder={t('pricing.modelPlaceholder')}
                onChange={e => setFn('model', e.target.value)}
                disabled={!!fnEditing}
              />
              {fnEditing?.Source === 'litellm' && (
                <p className="text-xs text-muted-foreground">{t('pricing.takeoverHint')}</p>
              )}
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="fn-price">{t('pricing.function.priceLabel')} <span className="text-destructive">*</span></Label>
              <Input id="fn-price" type="number" min={0} step="any" value={fnForm.pricePerCall} onChange={e => setFn('pricePerCall', e.target.value)} />
            </div>
          </div>
          {fnFormErr && <p className="text-sm text-destructive">{fnFormErr}</p>}
          {fnSave.isError && errMsg(fnSave.error) && (
            <p className="text-sm text-destructive">{errMsg(fnSave.error)}</p>
          )}
          <DialogFooter>
            <Button variant="outline" onClick={() => setFnDialogOpen(false)} disabled={fnSave.isPending}>{t('common.cancel')}</Button>
            <Button onClick={fnSubmit} disabled={fnSave.isPending}>
              {fnSave.isPending ? t('common.saving') : t('common.save')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* —— 删除确认 —— */}
      <Dialog open={!!deleting} onOpenChange={o => { if (!o && !del.isPending) setDeleting(null) }}>
        <DialogContent className="sm:max-w-sm">
          <DialogHeader>
            <DialogTitle>{t('pricing.deleteTitle')}</DialogTitle>
            <DialogDescription>{t('pricing.deleteDesc', { model: deleting?.Model })}</DialogDescription>
          </DialogHeader>
          {del.isError && errMsg(del.error) && (
            <p className="text-sm text-destructive">{errMsg(del.error)}</p>
          )}
          <DialogFooter>
            <Button variant="outline" onClick={() => setDeleting(null)} disabled={del.isPending}>{t('common.cancel')}</Button>
            <Button variant="destructive" onClick={() => deleting && del.mutate(deleting)} disabled={del.isPending}>
              {del.isPending ? t('common.deleting') : t('pricing.deleteConfirm')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={!!imgDeleting} onOpenChange={o => { if (!o && !imgDel.isPending) setImgDeleting(null) }}>
        <DialogContent className="sm:max-w-sm">
          <DialogHeader>
            <DialogTitle>{t('pricing.deleteTitle')}</DialogTitle>
            <DialogDescription>{t('pricing.deleteDesc', { model: imgDeleting?.Model })}</DialogDescription>
          </DialogHeader>
          {imgDel.isError && errMsg(imgDel.error) && (
            <p className="text-sm text-destructive">{errMsg(imgDel.error)}</p>
          )}
          <DialogFooter>
            <Button variant="outline" onClick={() => setImgDeleting(null)} disabled={imgDel.isPending}>{t('common.cancel')}</Button>
            <Button variant="destructive" onClick={() => imgDeleting && imgDel.mutate(imgDeleting)} disabled={imgDel.isPending}>
              {imgDel.isPending ? t('common.deleting') : t('pricing.deleteConfirm')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={!!fnDeleting} onOpenChange={o => { if (!o && !fnDel.isPending) setFnDeleting(null) }}>
        <DialogContent className="sm:max-w-sm">
          <DialogHeader>
            <DialogTitle>{t('pricing.deleteTitle')}</DialogTitle>
            <DialogDescription>{t('pricing.deleteDesc', { model: fnDeleting?.Model })}</DialogDescription>
          </DialogHeader>
          {fnDel.isError && errMsg(fnDel.error) && (
            <p className="text-sm text-destructive">{errMsg(fnDel.error)}</p>
          )}
          <DialogFooter>
            <Button variant="outline" onClick={() => setFnDeleting(null)} disabled={fnDel.isPending}>{t('common.cancel')}</Button>
            <Button variant="destructive" onClick={() => fnDeleting && fnDel.mutate(fnDeleting)} disabled={fnDel.isPending}>
              {fnDel.isPending ? t('common.deleting') : t('pricing.deleteConfirm')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
}
