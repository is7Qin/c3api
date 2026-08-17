// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { motion } from 'framer-motion'
import { Plus, Pencil, Trash2, ScrollText, Ban, CircleCheck } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { api } from '@/App'
import { ApiUnauthorized } from '@/lib/api/client'
import { BatchBar } from '@/components/batch-bar'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { Checkbox } from '@/components/ui/checkbox'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { ScrollArea } from '@/components/ui/scroll-area'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { Skeleton } from '@/components/ui/skeleton'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import type { components } from '@/lib/api/schema'

type Rule = components['schemas']['Rule']
type RuleCreate = components['schemas']['RuleCreate']
type AccountStatus = components['schemas']['AccountStatus']

// 规则事件类型（spec：ok | 429 | 4xx | 5xx | network——error 已拆分为
// 4xx/5xx/network，连接级独立 network）。
const KINDS = ['ok', '429', '4xx', '5xx', 'network'] as const
// then.status 可选值（空 = 不设置状态）。
const STATUSES: AccountStatus[] = ['active', 'unhealthy', '429', 'disabled']

// —— 条件行模型：when = kind 锚行（Select 常驻首行）+ 动态条件行 ——
type WhenField =
  | 'http_status' | 'error_message_contains' | 'account_id' | 'template_id' | 'group_id'
  | 'model' | 'window_seconds' | 'count_total_ge'
  | 'count_429_ge' | 'ratio_429_ge' | 'count_failure_ge' | 'ratio_failure_ge' | 'count_ok_ge'

interface CondRow { field: WhenField; value: string }

interface WhenFieldMeta {
  key: WhenField
  kinds: readonly string[] // kind 归属：'any' | '429' | '4xx' | '5xx' | 'network' | 'ok'（error_message_contains 联合归属 4xx|5xx|network）
  input: 'number' | 'text'
  placeholder?: string
  min?: number
  max?: number
  step?: number
}

// 匹配器元数据表：key 与后端 when JSON 键一致；kinds 驱动"添加条件"下拉过滤；
// input/min/max/step 决定值输入控件（操作符由字段类型隐含，无操作符选择器）。
const WHEN_FIELDS: WhenFieldMeta[] = [
  { key: 'http_status', kinds: ['any'], input: 'number', placeholder: '503', min: 100, max: 599 },
  { key: 'error_message_contains', kinds: ['4xx', '5xx', 'network'], input: 'text', placeholder: 'unhealthy' },
  { key: 'account_id', kinds: ['any'], input: 'number', placeholder: '12' },
  { key: 'template_id', kinds: ['any'], input: 'number', placeholder: '3' },
  { key: 'group_id', kinds: ['any'], input: 'number', placeholder: '1' },
  { key: 'model', kinds: ['any'], input: 'text', placeholder: 'gpt-4o' },
  { key: 'window_seconds', kinds: ['any'], input: 'number', placeholder: '60', min: 1 },
  { key: 'count_total_ge', kinds: ['any'], input: 'number', placeholder: '10', min: 1 },
  { key: 'count_429_ge', kinds: ['429'], input: 'number', placeholder: '3', min: 0 },
  { key: 'ratio_429_ge', kinds: ['429'], input: 'number', placeholder: '0.5', min: 0, max: 1, step: 0.01 },
  // count_failure_ge/ratio_failure_ge 语义 = 失败事件桶（4xx/5xx/network 并入）。
  { key: 'count_failure_ge', kinds: ['4xx', '5xx', 'network'], input: 'number', placeholder: '5', min: 0 },
  { key: 'ratio_failure_ge', kinds: ['4xx', '5xx', 'network'], input: 'number', placeholder: '0.8', min: 0, max: 1, step: 0.01 },
  { key: 'count_ok_ge', kinds: ['ok'], input: 'number', placeholder: '1', min: 0 },
]
const MAX_CONDITIONS = 10

// WhenField key → locale 键（rules.whenFields.*）。
const WHEN_FIELD_LOCALE: Record<WhenField, string> = {
  http_status: 'httpStatus', error_message_contains: 'errorContains',
  account_id: 'accountId', template_id: 'templateId', group_id: 'groupId',
  model: 'model', window_seconds: 'windowSeconds', count_total_ge: 'countTotal',
  count_429_ge: 'count429', ratio_429_ge: 'ratio429',
  count_failure_ge: 'countFailure', ratio_failure_ge: 'ratioFailure', count_ok_ge: 'countOK',
}
const whenFieldLabel = (k: WhenField) => `rules.whenFields.${WHEN_FIELD_LOCALE[k]}`

// kind 相关性过滤（"添加条件"与行内字段下拉共用）。
// kind=''（不限）→ 全部字段；否则只留归属含该 kind 的字段——
// 通用字段（kinds=['any']，如 window_seconds/count_total_ge/账号/模板等）对任何 kind 放行。
function kindFilter(kind: string): WhenFieldMeta[] {
  return WHEN_FIELDS.filter(f => kind === '' || f.kinds.includes(kind) || f.kinds.includes('any'))
}

interface ThenForm {
  status: string
  cooldown: string
  weight: string
  transmit: boolean // true = 透传上游原文（响应/日志）；false = 归一固定文案
}
interface WhenForm {
  kind: string
  rows: CondRow[]
}
interface FormState {
  name: string
  priority: string
  enabled: boolean
  when: WhenForm
  then: ThenForm
}

const emptyWhen = (): WhenForm => ({ kind: '', rows: [] })
const emptyThen = (): ThenForm => ({ status: '', cooldown: '', weight: '', transmit: false })
const emptyForm = (): FormState => ({ name: '', priority: '', enabled: true, when: emptyWhen(), then: emptyThen() })

// 数字字段：空/NaN → 不发送；其他字符串化。
function num(s: string): number | undefined {
  if (s === '') return undefined
  const n = Number(s)
  return Number.isNaN(n) ? undefined : n
}

// Rule.When（[key: string]: unknown）→ 条件行（未知键忽略，编辑往返保留全部已知字段；
// kind 提出放锚行）。kind 在 when 中的位置不固定（JSON 键序），回填与提交解耦。
function whenToRows(w: Rule['When']): CondRow[] {
  const rows: CondRow[] = []
  for (const f of WHEN_FIELDS) {
    const v = w[f.key]
    if (v !== undefined && v !== null && v !== '') rows.push({ field: f.key, value: String(v) })
  }
  return rows
}
function whenToForm(w: Rule['When']): WhenForm {
  return { kind: typeof w.kind === 'string' ? w.kind : '', rows: whenToRows(w) }
}
function thenToForm(th: Rule['Then']): ThenForm {
  const f = emptyThen()
  if (typeof th.status === 'string') f.status = th.status
  if (typeof th.cooldown === 'string') f.cooldown = th.cooldown
  if (th.weight !== undefined && th.weight !== null) f.weight = String(th.weight)
  if (th.transmit === true) f.transmit = true
  return f
}
function toForm(r: Rule): FormState {
  return {
    name: r.Name,
    priority: String(r.Priority),
    enabled: r.Enabled,
    when: whenToForm(r.When ?? {}),
    then: thenToForm(r.Then ?? {}),
  }
}

// 条件行 → when JSON：number 字段 Number()（NaN 跳过）、空值跳过、未知键忽略
// （WHEN_FIELDS 表驱动白名单，与后端字段全集一致）。
function rowsToWhen(rows: CondRow[]): Record<string, unknown> {
  const w: Record<string, unknown> = {}
  for (const r of rows) {
    const meta = WHEN_FIELDS.find(f => f.key === r.field)
    if (!meta || r.value === '') continue
    if (meta.input === 'number') {
      const n = num(r.value)
      if (n !== undefined) w[r.field] = n
    } else {
      w[r.field] = r.value
    }
  }
  return w
}
function toWhen(f: WhenForm): Record<string, unknown> {
  const w = rowsToWhen(f.rows)
  if (f.kind) w.kind = f.kind
  return w
}
function toThen(f: ThenForm): Record<string, unknown> {
  const th: Record<string, unknown> = {}
  if (f.status) th.status = f.status
  if (f.cooldown) th.cooldown = f.cooldown
  const w = num(f.weight)
  if (w !== undefined) th.weight = w
  if (f.transmit) th.transmit = true
  return th
}
function toBody(f: FormState): RuleCreate {
  return { name: f.name.trim(), priority: Number(f.priority), enabled: f.enabled, when: toWhen(f.when), then: toThen(f.then) }
}

// —— 预设模板（点击覆盖条件行 + 动作，name/priority/enabled 保留）——
interface TemplatePreset {
  id: string
  when: { [key: string]: unknown }
  then: ThenForm
}
const TEMPLATES: TemplatePreset[] = [
  { id: 'cooldown429', when: { kind: '429' }, then: { status: '429', cooldown: '30s', weight: '', transmit: false } },
  { id: '5xxBackoff', when: { kind: '5xx' }, then: { status: 'unhealthy', cooldown: '5s', weight: '', transmit: false } },
  { id: 'escalate', when: { kind: '429', window_seconds: 60, count_429_ge: 3 }, then: { status: '429', cooldown: '5m', weight: '', transmit: false } },
  { id: 'recover', when: { kind: 'ok' }, then: { status: 'active', cooldown: '', weight: '', transmit: false } },
]

// —— 摘要渲染 ——
function WhenSummary({ w, t }: { w: Rule['When']; t: (k: string) => string }) {
  if (!w || Object.keys(w).length === 0) return <span className="text-muted-foreground">—</span>
  const parts: string[] = []
  if (typeof w.kind === 'string') parts.push(t(`rules.kind.${w.kind}`))
  if (typeof w.http_status === 'number') parts.push(`HTTP ${w.http_status}`)
  if (typeof w.error_message_contains === 'string') parts.push(t('rules.when.errorContains') + ` "${w.error_message_contains}"`)
  if (typeof w.account_id === 'number') parts.push(`acc#${w.account_id}`)
  if (typeof w.template_id === 'number') parts.push(`tpl#${w.template_id}`)
  if (typeof w.group_id === 'number') parts.push(`grp#${w.group_id}`)
  if (typeof w.model === 'string') parts.push(w.model)
  if (typeof w.window_seconds === 'number') parts.push(`${w.window_seconds}s`)
  if (typeof w.count_429_ge === 'number') parts.push(`429≥${w.count_429_ge}`)
  if (typeof w.count_failure_ge === 'number') parts.push(`fail≥${w.count_failure_ge}`)
  if (typeof w.count_ok_ge === 'number') parts.push(`ok≥${w.count_ok_ge}`)
  if (typeof w.count_total_ge === 'number') parts.push(`total≥${w.count_total_ge}`)
  if (typeof w.ratio_429_ge === 'number') parts.push(`429率≥${w.ratio_429_ge}`)
  if (typeof w.ratio_failure_ge === 'number') parts.push(`fail率≥${w.ratio_failure_ge}`)
  return <span className="block max-w-64 truncate text-xs" title={parts.join(' · ')}>{parts.join(' · ') || '—'}</span>
}

function ThenSummary({ th, t }: { th: Rule['Then']; t: (k: string) => string }) {
  if (!th || Object.keys(th).length === 0) return <span className="text-muted-foreground">—</span>
  const parts: string[] = []
  if (typeof th.status === 'string') parts.push(`→${th.status}`)
  if (typeof th.cooldown === 'string') parts.push(`⏱${th.cooldown}`)
  if (typeof th.weight === 'number') parts.push(`w=${th.weight}`)
  if (th.transmit === true) parts.push(t('rules.then.transmit'))
  return <span className="block max-w-40 truncate text-xs" title={parts.join(' · ')}>{parts.join(' · ') || '—'}</span>
}

export default function Rules() {
  const { t } = useTranslation()
  const qc = useQueryClient()

  const { data, isLoading, isError, error } = useQuery({
    queryKey: ['rules'],
    queryFn: () => api.listRules({}),
    refetchInterval: 10_000,
  })
  const rows = data?.rows ?? []

  // —— 行勾选（规则表全量无分页，pageIds = 全部行）——
  const [selected, setSelected] = useState<number[]>([])
  const pageIds = rows.map(r => r.ID)
  const allChecked = rows.length > 0 && pageIds.every(id => selected.includes(id))
  const someChecked = pageIds.some(id => selected.includes(id))
  const toggleRow = (id: number) => setSelected(s => (s.includes(id) ? s.filter(x => x !== id) : [...s, id]))
  const toggleAll = (c: boolean) =>
    setSelected(s => (c ? Array.from(new Set([...s, ...pageIds])) : s.filter(x => !pageIds.includes(x))))

  // —— 创建/编辑/删除/批量删除 ——
  const [dialogOpen, setDialogOpen] = useState(false)
  const [editing, setEditing] = useState<Rule | null>(null)
  const [form, setForm] = useState<FormState>(emptyForm())
  const [deleting, setDeleting] = useState<Rule | null>(null)
  const [whenErr, setWhenErr] = useState<string | null>(null)

  const openCreate = () => {
    setEditing(null)
    setForm(emptyForm())
    setWhenErr(null)
    setDialogOpen(true)
  }
  const openEdit = (r: Rule) => {
    setEditing(r)
    setForm(toForm(r))
    setWhenErr(null)
    setDialogOpen(true)
  }

  const save = useMutation({
    mutationFn: (f: FormState) =>
      editing ? api.updateRule(editing.ID, toBody(f)) : api.createRule(toBody(f)),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['rules'] })
      setDialogOpen(false)
    },
  })
  const toggle = useMutation({
    mutationFn: (r: Rule) =>
      api.updateRule(r.ID, { enabled: !r.Enabled }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['rules'] }),
  })
  const remove = useMutation({
    mutationFn: (id: number) => api.deleteRule(id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['rules'] })
      setDeleting(null)
    },
  })
  const batchDelete = useMutation({
    mutationFn: (ids: number[]) => api.deleteRulesBatch(ids),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['rules'] })
      setSelected([])
    },
  })

  // —— 条件行操作（kind 锚行常驻，rows 动态增删）——
  const addRow = (field: WhenField) => {
    setForm(f => (f.when.rows.length >= MAX_CONDITIONS
      ? f
      : { ...f, when: { ...f.when, rows: [...f.when.rows, { field, value: '' }] } }))
    setWhenErr(null)
  }
  const removeRow = (i: number) => {
    setForm(f => ({ ...f, when: { ...f.when, rows: f.when.rows.filter((_, j) => j !== i) } }))
    setWhenErr(null)
  }
  const setRowField = (i: number, field: WhenField) => {
    setForm(f => ({ ...f, when: { ...f.when, rows: f.when.rows.map((r, j) => (j === i ? { ...r, field } : r)) } }))
    setWhenErr(null)
  }
  const setRowValue = (i: number, value: string) => {
    setForm(f => ({ ...f, when: { ...f.when, rows: f.when.rows.map((r, j) => (j === i ? { ...r, value } : r)) } }))
  }
  const applyTemplate = (tp: TemplatePreset) => {
    setForm(f => ({ ...f, when: whenToForm(tp.when), then: tp.then }))
    setWhenErr(null)
  }

  // 添加下拉：kindFilter - 已用字段；kind 切换不丢越界行（渲染不过滤）。
  const used = new Set(form.when.rows.map(r => r.field))
  const addOptions = kindFilter(form.when.kind).filter(f => !used.has(f.key))
  const addDisabled = form.when.rows.length >= MAX_CONDITIONS || addOptions.length === 0
  const [addField, setAddField] = useState<WhenField | null>(null)

  // 提交校验（仅检查实际会发送的条件；越界行是合法观察者语义，不阻止）：
  // kind=ok + error_message_contains → 确定死配置（ok 事件错误信息恒空）；比例无总次数 → 缺失依赖。
  const submit = () => {
    if (!form.name.trim() || form.priority === '') return
    const when = toWhen(form.when)
    if (when.kind === 'ok' && when.error_message_contains !== undefined) {
      setWhenErr(t('rules.whenErrOkContains'))
      return
    }
    if ((when.ratio_429_ge !== undefined || when.ratio_failure_ge !== undefined) && when.count_total_ge === undefined) {
      setWhenErr(t('rules.whenErrRatio'))
      return
    }
    setWhenErr(null)
    save.mutate(form)
  }
  const errMsg = (e: unknown) => (e instanceof ApiUnauthorized ? null : (e as Error)?.message)
  const setThen = (k: keyof ThenForm, v: string) => setForm(f => ({ ...f, then: { ...f.then, [k]: v } }))

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-semibold tracking-tight">{t('rules.title')}</h1>
          <p className="text-sm text-muted-foreground">{t('rules.subtitle')}</p>
        </div>
        <Button onClick={openCreate}><Plus /> {t('rules.new')}</Button>
      </div>

      <BatchBar
        selected={selected}
        onClear={() => setSelected([])}
        onDelete={async () => {
          await batchDelete.mutateAsync(selected)
        }}
      />

      {isError ? (
        <p className="text-sm text-destructive">{t('common.loadFailed', { message: (error as Error).message })}</p>
      ) : isLoading ? (
        <div className="space-y-2">
          {Array.from({ length: 3 }).map((_, i) => <Skeleton key={i} className="h-12" />)}
        </div>
      ) : rows.length === 0 ? (
        <motion.div initial={{ opacity: 0, y: 12 }} animate={{ opacity: 1, y: 0 }} transition={{ duration: 0.25 }}>
          <Card className="flex flex-col items-center gap-2 py-12 text-muted-foreground">
            <ScrollText className="size-10" />
            <p className="font-medium">{t('rules.emptyTitle')}</p>
            <p className="text-sm">{t('rules.emptyDesc')}</p>
            <Button className="mt-2" onClick={openCreate}><Plus /> {t('rules.new')}</Button>
          </Card>
        </motion.div>
      ) : (
        <div className="overflow-hidden rounded-lg border bg-card">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead className="w-10">
                  <Checkbox
                    checked={allChecked}
                    indeterminate={someChecked && !allChecked}
                    onCheckedChange={c => toggleAll(c === true)}
                  />
                </TableHead>
                <TableHead className="w-14">ID</TableHead>
                <TableHead>{t('rules.table.name')}</TableHead>
                <TableHead className="w-16 text-right">{t('rules.table.priority')}</TableHead>
                <TableHead className="w-20">{t('rules.table.enabled')}</TableHead>
                <TableHead className="w-64">{t('rules.table.when')}</TableHead>
                <TableHead className="w-44">{t('rules.table.then')}</TableHead>
                <TableHead className="w-24 text-right">{t('rules.table.actions')}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody className="[&_td]:py-3">
              {rows.map(r => (
                <TableRow key={r.ID} data-state={selected.includes(r.ID) ? 'selected' : undefined}>
                  <TableCell>
                    <Checkbox checked={selected.includes(r.ID)} onCheckedChange={() => toggleRow(r.ID)} />
                  </TableCell>
                  <TableCell className="tabular-nums">{r.ID}</TableCell>
                  <TableCell className="max-w-40 truncate" title={r.Name}>{r.Name}</TableCell>
                  <TableCell className="text-right tabular-nums">{r.Priority}</TableCell>
                  <TableCell>
                    <Button
                      variant="ghost"
                      size="icon-sm"
                      title={r.Enabled ? t('rules.disable') : t('rules.enable')}
                      onClick={() => toggle.mutate(r)}
                      disabled={toggle.isPending}
                    >
                      {r.Enabled ? <CircleCheck className="text-emerald-500" /> : <Ban />}
                    </Button>
                  </TableCell>
                  <TableCell><WhenSummary w={r.When} t={t} /></TableCell>
                  <TableCell><ThenSummary th={r.Then} t={t} /></TableCell>
                  <TableCell className="text-right">
                    <div className="flex justify-end gap-1">
                      <Button variant="ghost" size="icon-sm" title={t('common.edit')} onClick={() => openEdit(r)}><Pencil /></Button>
                      <Button variant="ghost" size="icon-sm" className="text-destructive" title={t('common.delete')} onClick={() => setDeleting(r)}><Trash2 /></Button>
                    </div>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </div>
      )}

      {/* —— 创建/编辑对话框 —— */}
      <Dialog open={dialogOpen} onOpenChange={setDialogOpen}>
        <DialogContent className="sm:max-w-2xl">
          <DialogHeader>
            <DialogTitle>{editing ? t('rules.editTitle', { id: editing.ID }) : t('rules.newTitle')}</DialogTitle>
            <DialogDescription>{t('rules.dialogDesc')}</DialogDescription>
          </DialogHeader>
          <ScrollArea className="max-h-[70vh] pr-1">
          <div className="space-y-4">
            {/* 预设模板：一键填充条件 + 动作（name/priority/enabled 保留） */}
            <div className="space-y-2">
              <p className="text-xs font-medium uppercase tracking-wide text-muted-foreground">{t('rules.templates.title')}</p>
              <div className="flex flex-wrap gap-2">
                {TEMPLATES.map(tp => (
                  <Button key={tp.id} variant="outline" size="sm" onClick={() => applyTemplate(tp)}>
                    {t(`rules.templates.${tp.id}`)}
                  </Button>
                ))}
              </div>
            </div>

            {/* 基础 */}
            <div className="grid grid-cols-3 gap-3">
              <div className="col-span-2 space-y-1.5">
                <Label htmlFor="rl-name">{t('rules.nameLabel')}</Label>
                <Input id="rl-name" value={form.name} placeholder={t('rules.namePlaceholder')} onChange={e => setForm(f => ({ ...f, name: e.target.value }))} />
              </div>
              <div className="space-y-1.5">
                <Label htmlFor="rl-priority">{t('rules.priorityLabel')}</Label>
                <Input id="rl-priority" type="number" min={0} value={form.priority} placeholder="10" onChange={e => setForm(f => ({ ...f, priority: e.target.value }))} />
              </div>
            </div>
            <label className="flex cursor-pointer items-center gap-2 text-sm">
              <Checkbox checked={form.enabled} onCheckedChange={c => setForm(f => ({ ...f, enabled: c === true }))} />
              {t('rules.enabledLabel')}
            </label>

            {/* 匹配 when：kind 锚行 + 动态条件行（行渲染不过滤，仅添加下拉防呆） */}
            <div className="space-y-2">
              <p className="text-xs font-medium uppercase tracking-wide text-muted-foreground">{t('rules.whenTitle')}</p>
              <div className="flex items-end gap-2">
                <div className="space-y-1.5">
                  <Label>{t('rules.when.kind')}</Label>
                  <Select
                    // 哨兵 'any' 表示"不限"（状态存 ''，UI 显示"不限"——'' 作为 value 时 SelectValue 无内容显示空白）
                    items={Object.fromEntries([['any', t('rules.any')], ...KINDS.map(k => [k, t(`rules.kind.${k}`)])])}
                    value={form.when.kind === '' ? 'any' : form.when.kind}
                    onValueChange={v => {
                      setForm(f => ({ ...f, when: { ...f.when, kind: v === 'any' ? '' : v } }))
                      setWhenErr(null)
                    }}
                  >
                    <SelectTrigger className="w-44"><SelectValue /></SelectTrigger>
                    <SelectContent>
                      <SelectItem value="any" label={t('rules.any')}>{t('rules.any')}</SelectItem>
                      {KINDS.map(k => <SelectItem key={k} value={k} label={t(`rules.kind.${k}`)}>{t(`rules.kind.${k}`)}</SelectItem>)}
                    </SelectContent>
                  </Select>
                </div>
                <p className="pb-2 text-xs text-muted-foreground">{t('rules.whenHint')}</p>
              </div>

              {form.when.rows.map((r, i) => {
                const meta = WHEN_FIELDS.find(f => f.key === r.field)
                // 行内下拉按 kind 过滤（用户反馈：选事件类型后不该出现无关条件）；
                // 越界行（kind 切换后字段不在过滤集）附加为额外选项保留——不丢行（评审 I-1）。
                const rowOptions = kindFilter(form.when.kind)
                if (!rowOptions.some(f => f.key === r.field)) {
                  const cur = WHEN_FIELDS.find(f => f.key === r.field)
                  if (cur) rowOptions.push(cur)
                }
                return (
                  <div key={i} className="flex items-center gap-2">
                    <span className="w-6 shrink-0 text-sm text-muted-foreground">{t('rules.condOf')}</span>
                    <Select
                      items={Object.fromEntries(rowOptions.map(f => [f.key, t(whenFieldLabel(f.key))]))}
                      value={r.field}
                      onValueChange={v => v && setRowField(i, v as WhenField)}
                    >
                      <SelectTrigger className="w-48"><SelectValue /></SelectTrigger>
                      <SelectContent>
                        {rowOptions.map(f => (
                          <SelectItem key={f.key} value={f.key} label={t(whenFieldLabel(f.key))}>{t(whenFieldLabel(f.key))}</SelectItem>
                        ))}
                      </SelectContent>
                    </Select>
                    {meta && meta.input === 'number' ? (
                      <Input type="number" min={meta.min} max={meta.max} step={meta.step} placeholder={meta.placeholder}
                        value={r.value} onChange={e => setRowValue(i, e.target.value)} className="w-28" />
                    ) : (
                      <Input type="text" placeholder={meta?.placeholder} value={r.value} onChange={e => setRowValue(i, e.target.value)} className="w-44" />
                    )}
                    <Button variant="ghost" size="icon-sm" className="text-destructive" title={t('rules.removeCondition')} onClick={() => removeRow(i)}>
                      <Trash2 />
                    </Button>
                  </div>
                )
              })}

              {/* 添加条件：kindFilter - 已用字段；行数上限 10 */}
              <div className="flex items-center gap-2">
                <Select
                  items={Object.fromEntries(addOptions.map(f => [f.key, t(whenFieldLabel(f.key))]))}
                  value={addField}
                  onValueChange={v => {
                    setAddField(null)
                    if (v) addRow(v as WhenField)
                  }}
                >
                  <SelectTrigger size="sm" className="w-48" disabled={addDisabled}>
                    <SelectValue placeholder={t('rules.addCondition')} />
                  </SelectTrigger>
                  <SelectContent>
                    {addOptions.map(f => (
                      <SelectItem key={f.key} value={f.key} label={t(whenFieldLabel(f.key))}>{t(whenFieldLabel(f.key))}</SelectItem>
                    ))}
                  </SelectContent>
                </Select>
                {form.when.rows.length >= MAX_CONDITIONS && (
                  <span className="text-xs text-muted-foreground">{t('rules.condLimit', { max: MAX_CONDITIONS })}</span>
                )}
              </div>

              {whenErr && (
                <p className="text-sm text-destructive">{whenErr}</p>
              )}
            </div>

            {/* 动作 then（可选组合） */}
            <div className="space-y-2">
              <p className="text-xs font-medium uppercase tracking-wide text-muted-foreground">{t('rules.thenTitle')}</p>
              <div className="grid grid-cols-3 gap-3">
                <div className="space-y-1.5">
                  <Label>{t('rules.then.status')}</Label>
                  <Select
                    items={Object.fromEntries([['', t('rules.any')], ...STATUSES.map(s => [s, t(`status.${s}`)])])}
                    value={form.then.status || null}
                    onValueChange={v => setThen('status', v)}
                  >
                    <SelectTrigger className="w-full"><SelectValue /></SelectTrigger>
                    <SelectContent>
                      <SelectItem value="" label={t('rules.any')}>{t('rules.any')}</SelectItem>
                      {STATUSES.map(s => <SelectItem key={s} value={s} label={t(`status.${s}`)}>{t(`status.${s}`)}</SelectItem>)}
                    </SelectContent>
                  </Select>
                </div>
                <div className="space-y-1.5">
                  <Label htmlFor="rl-cd">{t('rules.then.cooldown')}</Label>
                  <Input id="rl-cd" placeholder="30s / 5m / 1h" value={form.then.cooldown} onChange={e => setThen('cooldown', e.target.value)} />
                </div>
                <div className="space-y-1.5">
                  <Label htmlFor="rl-w">{t('rules.then.weight')}</Label>
                  <Input id="rl-w" type="number" min={0} max={100} placeholder="0" value={form.then.weight} onChange={e => setThen('weight', e.target.value)} />
                </div>
              </div>
              <label className="flex cursor-pointer items-center gap-2 text-sm">
                <Checkbox
                  checked={form.then.transmit}
                  onCheckedChange={c => setForm(f => ({ ...f, then: { ...f.then, transmit: c === true } }))}
                />
                {t('rules.then.transmit')}
              </label>
              <p className="text-xs text-muted-foreground">{t('rules.thenHint')}</p>
            </div>

            {save.isError && errMsg(save.error) && (
              <p className="text-sm text-destructive">{errMsg(save.error)}</p>
            )}
          </div>
          </ScrollArea>
          <DialogFooter>
            <Button variant="outline" onClick={() => setDialogOpen(false)} disabled={save.isPending}>{t('common.cancel')}</Button>
            <Button onClick={submit} disabled={save.isPending || !form.name.trim() || form.priority === ''}>
              {save.isPending ? t('common.saving') : editing ? t('common.saveChanges') : t('common.create')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* —— 删除确认 —— */}
      <Dialog open={!!deleting} onOpenChange={o => { if (!o && !remove.isPending) setDeleting(null) }}>
        <DialogContent className="sm:max-w-sm">
          <DialogHeader>
            <DialogTitle>{t('rules.deleteTitle')}</DialogTitle>
            <DialogDescription>
              {t('rules.deleteDesc', { name: deleting?.Name })}
            </DialogDescription>
          </DialogHeader>
          {remove.isError && errMsg(remove.error) && (
            <p className="text-sm text-destructive">{errMsg(remove.error)}</p>
          )}
          <DialogFooter>
            <Button variant="outline" onClick={() => setDeleting(null)} disabled={remove.isPending}>{t('common.cancel')}</Button>
            <Button variant="destructive" onClick={() => deleting && remove.mutate(deleting.ID)} disabled={remove.isPending}>
              {remove.isPending ? t('common.deleting') : t('common.confirmDelete')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
}
