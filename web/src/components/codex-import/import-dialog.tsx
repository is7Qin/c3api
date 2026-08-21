import { useEffect, useMemo, useRef, useState } from 'react'
import { Upload, FileText, FolderOpen, Files, CheckCircle2 } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import { Card } from '@/components/ui/card'
import { ScrollArea } from '@/components/ui/scroll-area'
import { SourceSelect } from './source-select'
import { PreviewTable } from './preview-table'
import { ResultView } from './result-view'
import { getAdapter, type SourceId } from '@/lib/codex-import/adapters'
import { readImportFile } from '@/lib/codex-import/parse'
import { importSequential } from '@/lib/codex-import/chunk'
import type { CredentialKind, NormalizedRow } from '@/lib/codex-import/normalize'
import type { components } from '@/lib/api/schema'
import { api } from '@/App'

type Template = components['schemas']['Template']
type Group = components['schemas']['Group']

export function CodexImportDialog({ open, onOpenChange, templates, groups, onDone }: { open: boolean; onOpenChange: (open: boolean) => void; templates: Template[]; groups: Group[]; onDone: () => void }) {
  const { t } = useTranslation()
  const STEP_LABELS = [
    t('accounts.import.step.kind'),
    t('accounts.import.step.source'),
    t('accounts.import.step.input'),
    t('accounts.import.step.preview'),
  ] as const
  const [step, setStep] = useState(1)
  const [kind, setKind] = useState<CredentialKind>('codex-oauth')
  const [source, setSource] = useState<SourceId>('cpa')
  const [tab, setTab] = useState('text')
  const [rawText, setRawText] = useState('')
  const [fileName, setFileName] = useState('')
  const [templateId, setTemplateId] = useState('')
  const [groupId, setGroupId] = useState('')
  const [parseState, setParseState] = useState<{ rows: NormalizedRow[]; parseError?: string }>({ rows: [] })
  const [isPending, setIsPending] = useState(false)
  const [progress, setProgress] = useState<[number, number] | null>(null)
  const [result, setResult] = useState<components['schemas']['ImportResult'] | null>(null)
  const [dragOver, setDragOver] = useState(false)
  const fileInputRef = useRef<HTMLInputElement>(null)
  const folderInputRef = useRef<HTMLInputElement>(null)

  const availableTemplates = useMemo(() => templates.filter(tp => tp.CredentialType === kind), [templates, kind])
  const validRows = parseState.rows.filter((row: any) => row.item)
  const invalidCount = parseState.rows.length - validRows.length

  useEffect(() => {
    if (!open) return
    setStep(1); setKind('codex-oauth'); setSource('cpa'); setTab('text'); setRawText(''); setFileName(''); setTemplateId(''); setGroupId(''); setParseState({ rows: [] }); setResult(null); setProgress(null)
  }, [open])
  useEffect(() => { setTemplateId(''); setSource('cpa'); setParseState({ rows: [] }) }, [kind])
  useEffect(() => {
    if (!rawText.trim()) { setParseState({ rows: [] }); return }
    const timer = window.setTimeout(() => setParseState(getAdapter(source).parse(rawText, kind)), tab === 'text' ? 300 : 0)
    return () => window.clearTimeout(timer)
  }, [rawText, source, kind, tab])

  const handleFiles = async (files: FileList | File[]) => {
    const list = Array.from(files)
    if (list.length === 0) return
    // 文件夹仅 CPA
    if (list.length > 1 && source !== 'cpa') {
      setParseState({ rows: [], parseError: t('accounts.import.folderOnlyCpa') })
      return
    }
    // 5MB 单文件校验由 readImportFile 完成；文件夹总大小校验
    const total = list.reduce((s, f) => s + f.size, 0)
    if (total > 20 * 1024 * 1024) {
      setParseState({ rows: [], parseError: t('accounts.import.input.fileTooLarge') })
      return
    }
    try {
      const texts = await Promise.all(list.map(f => f.text()))
      // 记录显示名：单文件用文件名，多文件/文件夹显示数量
      const displayName = list.length === 1 ? list[0].name : t('accounts.import.filesCount', { count: list.length })
      const combined = texts.join('\n')
      // 若为多文件，逐文件解析后合并，避免 JSON 数组拼接错误
      if (list.length > 1) {
        const allRows: unknown[] = []
        for (const text of texts) {
          const parsed = getAdapter(source).parse(text, kind)
          if (parsed.parseError && parsed.rows.length === 0) {
            // 忽略空文件
            continue
          }
          // parse 成功则收集 raw，再统一归一化已在 parse 内完成；此处直接合并 NormalizedRow
          // 为避免重复归一化，直接用 parse 结果的 rows
          // 需要重新索引
        }
        // 简化：用 combined 文本走统一 parse，兼容 JSONL 拼接；JSON 数组多文件场景 improbable，文件夹内应为单对象/JSONL
        setRawText(combined)
        setFileName(displayName)
        setTab('file')
        return
      }
      setRawText(combined)
      setFileName(displayName)
      setTab('file')
    } catch {
      setParseState({ rows: [], parseError: t('common.loadFailed', { message: '' }) })
    }
  }
  const handleFile = async (file?: File) => {
    if (!file) return
    await handleFiles([file])
  }
  const handleDrop = async (e: React.DragEvent) => {
    e.preventDefault()
    setDragOver(false)
    const files: File[] = []
    // 优先尝试 DataTransferItem.webkitGetAsEntry 以识别文件夹拖拽
    if (e.dataTransfer.items) {
      const entries: any[] = []
      for (let i = 0; i < e.dataTransfer.items.length; i++) {
        const item = e.dataTransfer.items[i] as any
        const entry = item.webkitGetAsEntry?.()
        if (entry) entries.push(entry)
      }
      if (entries.length > 0) {
        // 递归读取文件夹
        const readEntry = async (entry: any): Promise<File[]> => {
          if (entry.isFile) {
            return await new Promise(res => entry.file((f: File) => res([f])))
          }
          if (entry.isDirectory) {
            if (source !== 'cpa') return []
            const reader = entry.createReader()
            const batch: File[] = []
            const readBatch = (): Promise<File[]> => new Promise(resolve => {
              reader.readEntries(async (ents: any[]) => {
                if (ents.length === 0) resolve(batch)
                else {
                  for (const ent of ents) batch.push(...await readEntry(ent))
                  resolve(await readBatch())
                }
              })
            })
            return await readBatch()
          }
          return []
        }
        for (const ent of entries) files.push(...await readEntry(ent))
        if (files.length > 0) {
          await handleFiles(files)
          return
        }
      }
    }
    if (e.dataTransfer.files?.length) await handleFiles(e.dataTransfer.files)
  }
  const canNext = step === 1 ? true : step === 2 ? !!source : step === 3 ? validRows.length > 0 && !!templateId : false
  const doImport = async () => {
    if (!templateId || validRows.length === 0) return
    setIsPending(true); setProgress([0, Math.ceil(validRows.length / 100)])
    try {
      const items = validRows.map((row: any) => row.item)
      const result = kind === 'codex-oauth'
        ? await importSequential(items, Number(templateId), groupId ? Number(groupId) : undefined, body => api.importCodexOauthAccounts(body as components['schemas']['CodexOAuthImportBody']), (d, total) => setProgress([d, total]))
        : await importSequential(items, Number(templateId), groupId ? Number(groupId) : undefined, body => api.importCodexPatAccounts(body as components['schemas']['CodexPATImportBody']), (d, total) => setProgress([d, total]))
      setResult({ ...result, failed: result.failed.map(f => ({ ...f, index: validRows[f.index]?.index ?? f.index })) }); setStep(4)
    } catch (e) { setParseState(prev => ({ ...prev, parseError: e instanceof Error ? e.message : t('common.loadFailed', { message: '' }) })) }
    finally { setIsPending(false) }
  }
  const close = (next: boolean) => { if (!next) { setRawText(''); setResult(null); onDone() } onOpenChange(next) }

  return <Dialog open={open} onOpenChange={close}>
    <DialogContent className="flex max-h-[88vh] w-full flex-col gap-0 overflow-hidden p-0 sm:max-w-3xl lg:max-w-[860px]" data-od-id="codex-import-dialog">
      <DialogHeader className="shrink-0 border-b px-6 pt-6 pb-4">
        <DialogTitle className="text-xl">{t('accounts.import.title')}</DialogTitle>
        <DialogDescription className="text-sm">{t('accounts.import.desc')}</DialogDescription>
      </DialogHeader>

      <div className="shrink-0 border-b bg-muted/20 px-6 py-4" data-od-id="import-stepper">
        <div className="flex items-center gap-2">
          {STEP_LABELS.map((label, i) => {
            const idx = i + 1
            const active = idx === step
            const done = idx < step
            return <div key={label} className="flex flex-1 items-center gap-2">
              <div className={`flex size-8 shrink-0 items-center justify-center rounded-full border text-sm font-semibold transition-colors ${active ? 'border-primary bg-primary text-primary-foreground shadow-sm' : done ? 'border-primary/40 bg-primary/15 text-primary' : 'border-border bg-card text-muted-foreground'}`}>
                {done ? <CheckCircle2 className="size-4" /> : idx}
              </div>
              <span className={`hidden text-sm sm:inline ${active ? 'font-semibold text-foreground' : done ? 'text-foreground' : 'text-muted-foreground'}`}>{label}</span>
              {idx < STEP_LABELS.length && <div className={`mx-2 hidden h-px flex-1 sm:block ${done ? 'bg-primary/30' : 'bg-border'}`} />}
            </div>
          })}
        </div>
      </div>

      <div className="flex min-h-[460px] flex-1 flex-col overflow-hidden">
        <ScrollArea className="flex-1">
          <div className="px-6 py-6">
            {step === 1 && <div className="space-y-5" data-od-id="import-type-step">
              <div>
                <h3 className="text-base font-semibold">{t('accounts.import.step.kind')}</h3>
                <p className="mt-1 text-sm text-muted-foreground">{t('accounts.import.stepDesc.kind')}</p>
              </div>
              <div className="grid gap-4 sm:grid-cols-2">
                {(['codex-oauth', 'codex-pat'] as CredentialKind[]).map(value => <button type="button" key={value} onClick={() => setKind(value)} className="text-left">
                  <Card className={`relative flex min-h-[132px] flex-col justify-between p-5 pt-6 transition-all hover:shadow-md ${kind === value ? 'border-primary bg-primary/5 ring-1 ring-primary/20' : 'hover:border-border'}`}>
                    {kind === value && <Badge className="absolute top-3 right-3 z-10 rounded-full px-2.5 py-0.5 text-xs shadow-xs">{t('accounts.import.selected')}</Badge>}
                    <div className="pr-16">
                      <div className="text-base font-semibold">{value === 'codex-oauth' ? t('accounts.import.type.oauth') : t('accounts.import.type.pat')}</div>
                      <p className="mt-1.5 text-sm leading-relaxed text-muted-foreground">{value === 'codex-oauth' ? t('accounts.import.kindDesc.oauth') : t('accounts.import.kindDesc.pat')}</p>
                    </div>
                    <span className="mt-3 inline-flex items-center gap-1 text-xs font-medium text-primary">{value === 'codex-oauth' ? t('accounts.import.interface.oauth') : t('accounts.import.interface.pat')}</span>
                  </Card>
                </button>)}
              </div>
            </div>}
            {step === 2 && <div className="space-y-5" data-od-id="import-source-step">
              <div>
                <h3 className="text-base font-semibold">{t('accounts.import.step.source')}</h3>
                <p className="mt-1 text-sm text-muted-foreground">{t('accounts.import.stepDesc.source')}</p>
              </div>
              <SourceSelect value={source} kind={kind} onChange={setSource} />
              <Alert className="border-primary/20 bg-primary/5"><AlertDescription className="text-sm">{t('accounts.import.infer', { hint: kind === 'codex-oauth' ? t('accounts.import.inferHint.oauth') : t('accounts.import.inferHint.pat') })}</AlertDescription></Alert>
            </div>}
            {step === 3 && <div className="space-y-5" data-od-id="import-input-step">
              <div>
                <h3 className="text-base font-semibold">{t('accounts.import.step.input')}</h3>
                <p className="mt-1 text-sm text-muted-foreground">{t('accounts.import.stepDesc.input')}</p>
              </div>
              <Tabs value={tab} onValueChange={setTab}>
                <TabsList className="w-full justify-start gap-1">
                  <TabsTrigger value="file" className="gap-1.5"><Upload className="size-4" /> {t('accounts.import.input.fileTab')}</TabsTrigger>
                  <TabsTrigger value="text" className="gap-1.5"><FileText className="size-4" /> {t('accounts.import.input.textTab')}</TabsTrigger>
                </TabsList>
                <TabsContent value="file" className="mt-4 space-y-3">
                  <div
                    onDragOver={e => { e.preventDefault(); setDragOver(true) }}
                    onDragLeave={() => setDragOver(false)}
                    onDrop={handleDrop}
                    className={`group relative flex flex-col items-center justify-center gap-3 rounded-xl border-2 border-dashed bg-card px-6 py-8 text-center transition-colors ${dragOver ? 'border-primary bg-primary/5' : 'border-border hover:border-primary/30 hover:bg-muted/30'}`}
                  >
                    <div className="flex size-12 items-center justify-center rounded-full bg-muted text-muted-foreground group-hover:bg-primary/10 group-hover:text-primary">
                      {source === 'cpa' ? <FolderOpen className="size-6" /> : <Upload className="size-6" />}
                    </div>
                    <div className="space-y-1">
                      <p className="text-sm font-semibold">{dragOver ? t('accounts.import.input.dropActive') : t('accounts.import.fileDropTitle')}</p>
                      <p className="text-xs text-muted-foreground">{source === 'cpa' ? t('accounts.import.input.fileDropDescCpa') : t('accounts.import.fileDropDesc')}</p>
                      {source === 'cpa' && <p className="text-[11px] text-muted-foreground">{t('accounts.import.input.folderHint')}</p>}
                    </div>
                    <div className="mt-1 flex flex-wrap items-center justify-center gap-2">
                      <Button type="button" variant="outline" size="sm" onClick={() => fileInputRef.current?.click()}><Upload className="size-4" />{t('accounts.import.input.chooseFile')}</Button>
                      {source === 'cpa' && <Button type="button" variant="outline" size="sm" onClick={() => folderInputRef.current?.click()}><FolderOpen className="size-4" />{t('accounts.import.input.chooseFolder')}</Button>}
                    </div>
                    <input ref={fileInputRef} type="file" accept=".json,.txt,.at_txt,text/plain,application/json" onChange={e => { if (e.target.files?.length) handleFiles(e.target.files); e.target.value = '' }} className="hidden" />
                    {source === 'cpa' && <input ref={folderInputRef as any} type="file" {...({ webkitdirectory: '', directory: '' } as any)} onChange={e => { if (e.target.files?.length) handleFiles(e.target.files); e.target.value = '' }} className="hidden" />}
                  </div>
                  {fileName && <div className="flex items-center gap-2 rounded-lg border bg-muted/30 px-3 py-2 text-xs text-muted-foreground"><Files className="size-4 shrink-0" /><span className="truncate">{fileName}</span><Button variant="ghost" size="sm" className="ml-auto h-6 px-2" onClick={() => { setRawText(''); setFileName(''); setParseState({ rows: [] }) }}>{t('common.cancel')}</Button></div>}
                  {source !== 'cpa' && <p className="text-xs text-muted-foreground">{t('accounts.import.folderOnlyCpa')}</p>}
                </TabsContent>
                <TabsContent value="text" className="mt-4">
                  <textarea value={rawText} onChange={e => setRawText(e.target.value)} placeholder={t('accounts.import.pastePlaceholder')} rows={10} className="min-h-[220px] w-full resize-y rounded-lg border border-input bg-background px-4 py-3 font-mono text-sm leading-relaxed outline-none placeholder:text-muted-foreground focus-visible:border-ring focus-visible:ring-2 focus-visible:ring-ring/20" />
                  <p className="mt-2 text-xs text-muted-foreground">{t('accounts.import.pasteHint2')}</p>
                </TabsContent>
              </Tabs>
              <div className="grid gap-4 sm:grid-cols-2">
                <div className="space-y-2"><Label className="text-sm">{t('accounts.import.templateLabel')}</Label><Select items={Object.fromEntries(availableTemplates.map(tp => [String(tp.ID), tp.Name ?? `#${tp.ID}`]))} value={templateId || null} onValueChange={v => setTemplateId(String(v ?? ''))}><SelectTrigger className="h-10 w-full"><SelectValue placeholder={t('accounts.import.templatePlaceholder')} /></SelectTrigger><SelectContent>{availableTemplates.map(tp => <SelectItem key={tp.ID} value={String(tp.ID)} label={tp.Name ?? `#${tp.ID}`}>{tp.Name ?? `#${tp.ID}`}</SelectItem>)}</SelectContent></Select>{availableTemplates.length === 0 && <p className="text-xs text-destructive">{t('accounts.import.templateEmpty', { type: kind })}</p>}</div>
                <div className="space-y-2"><Label className="text-sm">{t('accounts.import.groupLabel')}</Label><Select items={Object.fromEntries([['__none', t('accounts.import.groupNone')], ...groups.map(g => [String(g.ID), g.Name ?? `#${g.ID}`])])} value={groupId ? groupId : '__none'} onValueChange={v => setGroupId(v === '__none' ? '' : String(v ?? ''))}><SelectTrigger className="h-10 w-full"><SelectValue /></SelectTrigger><SelectContent><SelectItem value="__none" label={t('accounts.import.groupNone')}>{t('accounts.import.groupNone')}</SelectItem>{groups.map(g => <SelectItem key={g.ID} value={String(g.ID)} label={g.Name ?? `#${g.ID}`}>{g.Name ?? `#${g.ID}`}</SelectItem>)}</SelectContent></Select></div>
              </div>
              {parseState.parseError && <Alert variant="destructive"><AlertDescription className="text-sm">{parseState.parseError === 'adapterComingSoon' ? t('accounts.import.source.comingSoon') : parseState.parseError}</AlertDescription></Alert>}
              <div className="flex items-center gap-2 rounded-lg border bg-card px-4 py-3 text-sm"><span className="font-medium">{t('accounts.import.stats.valid')} {validRows.length}</span><span className="text-muted-foreground">/</span><span className={invalidCount ? 'font-medium text-destructive' : 'text-muted-foreground'}>{t('accounts.import.stats.invalid')} {invalidCount}</span><span className="text-muted-foreground">/ {t('accounts.import.stats.total')} {parseState.rows.length}</span></div>
              {parseState.rows.length > 0 && <PreviewTable rows={parseState.rows} kind={kind} />}
            </div>}
            {step === 4 && (result ? <ResultView result={result} /> : <div className="space-y-5">
              <div>
                <h3 className="text-base font-semibold">{t('accounts.import.step.preview')}</h3>
                <p className="mt-1 text-sm text-muted-foreground">{t('accounts.import.stepDesc.preview')}</p>
              </div>
              <div className="flex items-center gap-2 rounded-lg border bg-card px-4 py-3 text-sm"><span className="font-medium">{t('accounts.import.stats.valid')} {validRows.length}</span><span className="text-muted-foreground">/</span><span className={invalidCount ? 'font-medium text-destructive' : 'text-muted-foreground'}>{t('accounts.import.stats.invalid')} {invalidCount}</span><span className="text-muted-foreground">/ {t('accounts.import.stats.total')} {parseState.rows.length}</span></div>
              {parseState.rows.length === 0 ? <Alert><AlertDescription>{t('accounts.import.previewEmpty')}</AlertDescription></Alert> : <PreviewTable rows={parseState.rows} kind={kind} />}
            </div>)}
          </div>
        </ScrollArea>
      </div>

      <DialogFooter className="shrink-0 rounded-b-[14px] border-t bg-muted/10 px-6 py-5">
        <Button variant="outline" onClick={() => step > 1 ? setStep(step - 1) : close(false)}>{step > 1 ? t('accounts.import.prev') : t('common.cancel')}</Button>
        {step < 3 && <Button onClick={() => setStep(step + 1)} disabled={!canNext}>{t('accounts.import.next.source')}</Button>}
        {step === 3 && <Button onClick={() => setStep(4)} disabled={!canNext}>{t('accounts.import.next.preview')}</Button>}
        {step === 4 && !result && <Button onClick={doImport} disabled={isPending || validRows.length === 0 || !templateId}>{isPending ? t('accounts.import.sending', { done: progress?.[0] ?? 0, total: progress?.[1] ?? 0 }) : t('accounts.import.importAction', { count: validRows.length })}</Button>}
        {result && <Button onClick={() => close(false)}>{t('accounts.import.done')}</Button>}
      </DialogFooter>
    </DialogContent>
  </Dialog>
}
