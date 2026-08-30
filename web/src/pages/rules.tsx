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
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert'
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
import { cn } from '@/lib/utils'
import type { components } from '@/lib/api/schema'

type Rule = components['schemas']['Rule']
type RuleCreate = components['schemas']['RuleCreate']

// 规则事件类型（spec：ok | 429 | 4xx | 5xx | network——error 已拆分为
// 4xx/5xx/network，连接级独立 network）。
const KINDS = ['ok', '429', '4xx', '5xx', 'network'] as const

// —— typed 路由动作（后端 domain.RuleThen 严格反序列化 + ValidateThen 语义校验）——
// then.throttle = {scope: account|account_route, mode: retry_after|open, duration_ms?, use_reset}
//   retry_after 强制 use_reset=true（上游 reset 窗口优先，duration_ms 为兜底）；
//   open 强制 use_reset=false 且 duration_ms>0。
// then.fail_account = true：终态摘除（lifecycle CAS），与 throttle/legacy 互斥。
// use_reset 由 mode 唯一决定（校验不变量），表单不单独编辑，序列化时按 mode 派生。
type RuleAction = 'none' | 'throttle' | 'fail_account'
type ThrottleScope = 'account' | 'account_route'
type ThrottleMode = 'retry_after' | 'open'
interface ThrottleForm { scope: ThrottleScope; mode: ThrottleMode; durationMs: string }
const ACTIONS: RuleAction[] = ['none', 'throttle', 'fail_account']

// —— 条件行模型：when = kind 锚行（Select 常驻首行）+ 动态条件行 ——
type WhenField =
  | 'http_status' | 'http_status_in' | 'error_message_contains' | 'error_message_contains_in' | 'account_id' | 'template_id' | 'group_id'
  | 'model' | 'model_in' | 'window_seconds' | 'count_total_ge'
  | 'count_429_ge' | 'ratio_429_ge' | 'count_failure_ge' | 'ratio_failure_ge' | 'count_ok_ge'

interface CondRow { field: WhenField; value: string | string[] }

interface WhenFieldMeta {
  key: WhenField
  kinds: readonly string[] // kind 归属：'any' | '429' | '4xx' | '5xx' | 'network' | 'ok'（error_message_contains 联合归属 4xx|5xx|network）
  input: 'number' | 'text' | 'tags'
  placeholder?: string
  min?: number
  max?: number
  step?: number
}

// 匹配器元数据表：key 与后端 when JSON 键一致；kinds 驱动"添加条件"下拉过滤；
// input/min/max/step 决定值输入控件（操作符由字段类型隐含，无操作符选择器）。
const WHEN_FIELDS: WhenFieldMeta[] = [
  { key: 'http_status', kinds: ['any'], input: 'number', placeholder: '503', min: 400, max: 599 },
  { key: 'http_status_in', kinds: ['any'], input: 'tags', placeholder: '[500,502]' },
  { key: 'error_message_contains', kinds: ['4xx', '5xx', 'network'], input: 'text', placeholder: 'unhealthy' },
  { key: 'error_message_contains_in', kinds: ['4xx', '5xx', 'network'], input: 'tags', placeholder: '[overload,busy]' },
  { key: 'account_id', kinds: ['any'], input: 'number', placeholder: '12' },
  { key: 'template_id', kinds: ['any'], input: 'number', placeholder: '3' },
  { key: 'group_id', kinds: ['any'], input: 'number', placeholder: '1' },
  { key: 'model', kinds: ['any'], input: 'text', placeholder: 'gpt-4o' },
  { key: 'model_in', kinds: ['any'], input: 'tags', placeholder: '[gpt-5-0611,gpt-4o]' },
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
  http_status: 'httpStatus', http_status_in: 'httpStatusIn',
  error_message_contains: 'errorContains', error_message_contains_in: 'errorContainsIn',
  account_id: 'accountId', template_id: 'templateId', group_id: 'groupId',
  model: 'model', model_in: 'modelIn', window_seconds: 'windowSeconds', count_total_ge: 'countTotal',
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
  action: RuleAction
  throttle: ThrottleForm
  responseCode: string // empty = 透传；非空 = 400-599
  customMessage: string // empty = 透传
  failConfirmed: boolean // FailAccount 终态确认闸（编辑回显已含 fail_account 时预置 true）
  legacy: Record<string, unknown> // 旧规则 status/cooldown/weight 只读往返（不编辑不丢弃；选 typed 动作即替换）
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
const emptyThrottle = (): ThrottleForm => ({ scope: 'account', mode: 'retry_after', durationMs: '' })
const emptyThen = (): ThenForm => ({ action: 'none', throttle: emptyThrottle(), responseCode: '', customMessage: '', failConfirmed: false, legacy: {} })
const emptyForm = (): FormState => ({ name: '', priority: '', enabled: true, when: emptyWhen(), then: emptyThen() })

// 数字字段：空/NaN → 不发送；其他字符串化。
function num(s: string): number | undefined {
  if (s === '') return undefined
  const n = Number(s)
  return Number.isNaN(n) ? undefined : n
}

// legacy 动作键（Wave 5 cutover 后后端 strict 400；此处只读往返不编辑）。
const LEGACY_THEN_KEYS = ['status', 'cooldown', 'weight'] as const

// then.throttle（unknown）→ 表单值；结构不合（缺 scope/mode 或枚举外）视为无 typed 动作。
function parseThrottle(v: unknown): ThrottleForm | null {
  if (typeof v !== 'object' || v === null || Array.isArray(v)) return null
  const o = v as Record<string, unknown>
  const scope: ThrottleScope | null = o.scope === 'account' || o.scope === 'account_route' ? o.scope : null
  const mode: ThrottleMode | null = o.mode === 'retry_after' || o.mode === 'open' ? o.mode : null
  if (!scope || !mode) return null
  return { scope, mode, durationMs: typeof o.duration_ms === 'number' ? String(o.duration_ms) : '' }
}

// Rule.When（[key: string]: unknown）→ 条件行（未知键忽略，编辑往返保留全部已知字段；
// kind 提出放锚行）。kind 在 when 中的位置不固定（JSON 键序），回填与提交解耦。
function whenToRows(w: Rule['When']): CondRow[] {
  const rows: CondRow[] = []
  for (const f of WHEN_FIELDS) {
    const v = w[f.key]
    if (Array.isArray(v)) {
      if (v.length > 0) rows.push({ field: f.key, value: v.join(',') })
      continue
    }
    if (v !== undefined && v !== null && v !== '') rows.push({ field: f.key, value: String(v) })
  }
  return rows
}
function whenToForm(w: Rule['When']): WhenForm {
  return { kind: typeof w.kind === 'string' ? w.kind : '', rows: whenToRows(w) }
}
function thenToForm(th: Rule['Then']): ThenForm {
  const f = emptyThen()
  const t = parseThrottle(th.throttle)
  if (t) {
    f.action = 'throttle'
    f.throttle = t
  } else if (th.fail_account === true) {
    f.action = 'fail_account'
    f.failConfirmed = true // 回显即已确认（避免编辑无关字段被确认闸卡死）
  } else {
    for (const k of LEGACY_THEN_KEYS) {
      if (th[k] !== undefined) f.legacy[k] = th[k]
    }
  }
  if (th.response_code !== undefined && th.response_code !== null) f.responseCode = String(th.response_code)
  if (typeof th.custom_message === 'string') f.customMessage = th.custom_message
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

// 条件行 → when JSON：number 字段 Number()（NaN 跳过）、tags 按逗号/空格切数组、空值跳过、未知键忽略
// （WHEN_FIELDS 表驱动白名单，与后端字段全集一致）。
function rowsToWhen(rows: CondRow[]): Record<string, unknown> {
  const w: Record<string, unknown> = {}
  for (const r of rows) {
    const meta = WHEN_FIELDS.find(f => f.key === r.field)
    const raw = Array.isArray(r.value) ? r.value.join(',') : String(r.value ?? '')
    if (!meta || raw.trim() === '') continue
    if (meta.input === 'tags') {
      const parts = raw.split(/[,\s]+/).map(s => s.trim()).filter(Boolean)
      if (parts.length === 0) continue
      if (r.field === 'http_status_in') {
        const nums = parts.map(Number).filter(n => !Number.isNaN(n))
        if (nums.length > 0) w[r.field] = nums
      } else {
        w[r.field] = parts
      }
      continue
    }
    if (meta.input === 'number') {
      const n = num(raw)
      if (n !== undefined) w[r.field] = n
    } else {
      w[r.field] = raw
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
  if (f.action === 'throttle') {
    // use_reset 由 mode 派生（ValidateThen 不变量：retry_after=true / open=false）
    const t: Record<string, unknown> = { scope: f.throttle.scope, mode: f.throttle.mode, use_reset: f.throttle.mode === 'retry_after' }
    const d = num(f.throttle.durationMs)
    if (d !== undefined) t.duration_ms = d
    th.throttle = t
  } else if (f.action === 'fail_account') {
    th.fail_account = true
  } else {
    Object.assign(th, f.legacy) // 无 typed 动作时旧键原样往返（不静默丢语义）
  }
  if (f.responseCode !== '') {
    const rc = Number(f.responseCode)
    if (!Number.isNaN(rc)) th.response_code = rc
  }
  if (f.customMessage !== '') th.custom_message = f.customMessage
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
const presetThen = (over: Partial<ThenForm>): ThenForm => ({ ...emptyThen(), ...over })
const TEMPLATES: TemplatePreset[] = [
  { id: 'throttle429', when: { kind: '429' }, then: presetThen({ action: 'throttle', throttle: { scope: 'account', mode: 'retry_after', durationMs: '' } }) },
  { id: 'throttleRoute429', when: { kind: '429' }, then: presetThen({ action: 'throttle', throttle: { scope: 'account_route', mode: 'retry_after', durationMs: '' } }) },
  { id: 'escalate', when: { kind: '429', window_seconds: 60, count_429_ge: 3 }, then: presetThen({ action: 'throttle', throttle: { scope: 'account', mode: 'open', durationMs: '300000' } }) },
  { id: 'failFatal', when: { kind: '4xx', http_status: 401, error_message_contains: 'invalid_api_key' }, then: presetThen({ action: 'fail_account' }) },
  { id: 'overload503', when: { kind: '5xx', http_status: 503, error_message_contains: 'overload' }, then: presetThen({}) },
]

// —— 摘要渲染 ——
function WhenSummary({ w, t }: { w: Rule['When']; t: (k: string) => string }) {
  if (!w || Object.keys(w).length === 0) return <span className="text-muted-foreground">—</span>
  const parts: string[] = []
  if (typeof w.kind === 'string') parts.push(t(`rules.kind.${w.kind}`))
  if (typeof w.http_status === 'number') parts.push(`HTTP ${w.http_status}`)
  if (Array.isArray((w as Record<string, unknown>).http_status_in)) parts.push(`HTTP [${((w as Record<string, unknown>).http_status_in as unknown[]).join(',')}]`)
  if (typeof w.error_message_contains === 'string') parts.push(t('rules.when.errorContains') + ` "${w.error_message_contains}"`)
  if (Array.isArray((w as Record<string, unknown>).error_message_contains_in)) parts.push(`err∈[${((w as Record<string, unknown>).error_message_contains_in as string[]).join(',')}]`)
  if (typeof w.account_id === 'number') parts.push(`acc#${w.account_id}`)
  if (typeof w.template_id === 'number') parts.push(`tpl#${w.template_id}`)
  if (typeof w.group_id === 'number') parts.push(`grp#${w.group_id}`)
  if (typeof w.model === 'string') parts.push(w.model)
  if (Array.isArray((w as Record<string, unknown>).model_in)) parts.push(`model∈[${((w as Record<string, unknown>).model_in as string[]).join(',')}]`)
  if (typeof w.window_seconds === 'number') parts.push(`${w.window_seconds}s`)
  if (typeof w.count_429_ge === 'number') parts.push(`429≥${w.count_429_ge}`)
  if (typeof w.count_failure_ge === 'number') parts.push(`fail≥${w.count_failure_ge}`)
  if (typeof w.count_ok_ge === 'number') parts.push(`ok≥${w.count_ok_ge}`)
  if (typeof w.count_total_ge === 'number') parts.push(`total≥${w.count_total_ge}`)
  if (typeof w.ratio_429_ge === 'number') parts.push(`429率≥${w.ratio_429_ge}`)
  if (typeof w.ratio_failure_ge === 'number') parts.push(`fail率≥${w.ratio_failure_ge}`)
  return <span className="block max-w-64 truncate text-xs" title={parts.join(' · ')}>{parts.join(' · ') || '—'}</span>
}

function ThenSummary({ th, t }: { th: Rule['Then']; t: (k: string, opts?: Record<string, unknown>) => string }) {
  if (!th || Object.keys(th).length === 0) return <span className="text-muted-foreground">—</span>
  const parts: string[] = []
  const t5 = parseThrottle(th.throttle)
  if (t5) parts.push(`${t('rules.then.summaryThrottle')} ${t5.scope}/${t5.mode}${t5.durationMs ? ` ${t5.durationMs}ms` : ''}`)
  if (th.fail_account === true) parts.push(t('rules.then.summaryFail'))
  // legacy 只读回显（编辑对话框不再提供控件；保存时原样往返）
  if (typeof th.status === 'string') parts.push(`→${th.status}`)
  if (typeof th.cooldown === 'string') parts.push(`⏱${th.cooldown}`)
  if (typeof th.weight === 'number') parts.push(`w=${th.weight}`)
  if (typeof th.response_code === 'number') parts.push(t('rules.then.overrideSummary', { code: th.response_code } as unknown as Record<string, unknown>))
  if (typeof th.custom_message === 'string' && th.custom_message !== '') parts.push(t('rules.then.fixedMessage'))
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
    if (form.then.action === 'throttle') {
      const d = num(form.then.throttle.durationMs)
      if (form.then.throttle.durationMs !== '' && (d === undefined || d <= 0)) {
        setWhenErr(t('rules.then.errDurationPositive'))
        return
      }
      if (form.then.throttle.mode === 'open' && (d === undefined || d <= 0)) {
        setWhenErr(t('rules.then.errDurationRequired'))
        return
      }
    }
    if (form.then.action === 'fail_account' && !form.then.failConfirmed) return
    if (form.then.responseCode !== '') {
      const rc = Number(form.then.responseCode)
      if (Number.isNaN(rc) || rc < 400 || rc > 599) {
        setWhenErr(t('rules.then.errResponseRange'))
        return
      }
    }
    const when = toWhen(form.when) as Record<string, unknown>
    const exclusivePairs: [string, string][] = [
      ['http_status', 'http_status_in'],
      ['model', 'model_in'],
      ['error_message_contains', 'error_message_contains_in'],
    ]
    for (const [a, b] of exclusivePairs) {
      if (when[a] !== undefined && when[b] !== undefined) {
        setWhenErr(t('rules.whenErrMutual', { a, b } as unknown as Record<string, unknown>) || `${a} and ${b} are mutually exclusive`)
        return
      }
    }
    if (when.kind === 'ok' && (when.error_message_contains !== undefined || when.error_message_contains_in !== undefined)) {
      setWhenErr(t('rules.whenErrOkContains'))
      return
    }
    if ((when.ratio_429_ge !== undefined || when.ratio_failure_ge !== undefined) && when.count_total_ge === undefined) {
      setWhenErr(t('rules.whenErrRatio'))
      return
    }
    const httpSingle = when.http_status as number | undefined
    if (httpSingle !== undefined && (httpSingle < 400 || httpSingle > 599)) {
      setWhenErr(t('rules.whenErrRange') || 'http_status must be between 400 and 599')
      return
    }
    const dupSpecs: { key: string; arr: unknown }[] = [
      { key: 'http_status_in', arr: when.http_status_in },
      { key: 'model_in', arr: when.model_in },
      { key: 'error_message_contains_in', arr: when.error_message_contains_in },
    ]
    for (const s of dupSpecs) {
      const arr = s.arr as unknown[] | undefined
      if (!Array.isArray(arr) || arr.length === 0) continue
      if (s.key === 'http_status_in') {
        for (const v of arr as number[]) {
          if (v < 400 || v > 599) { setWhenErr(t('rules.whenErrRangeIn') || 'http_status_in must be between 400 and 599'); return }
        }
      } else {
        for (const v of arr as string[]) {
          if (v === '') { setWhenErr(t('rules.whenErrEmpty') || `${s.key} must not contain empty string`); return }
        }
      }
      if (new Set(arr).size !== arr.length) {
        setWhenErr(t('rules.whenErrDuplicate', { field: s.key } as unknown as Record<string, unknown>) || `${s.key} contains duplicate value`)
        return
      }
    }
    setWhenErr(null)
    save.mutate(form)
  }
  const errMsg = (e: unknown) => (e instanceof ApiUnauthorized ? null : (e as Error)?.message)
  const setThen = (k: 'responseCode' | 'customMessage', v: string) => setForm(f => ({ ...f, then: { ...f.then, [k]: v } }))
  const setAction = (a: RuleAction) => setForm(f => ({ ...f, then: { ...f.then, action: a } }))
  const setThrottle = (patch: Partial<ThrottleForm>) => setForm(f => ({ ...f, then: { ...f.then, throttle: { ...f.then.throttle, ...patch } } }))

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
        <div className="overflow-hidden rounded-lg">
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
            <div className="grid grid-cols-1 gap-3 sm:grid-cols-3">
              <div className="space-y-1.5 sm:col-span-2">
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
              <div className="grid gap-1 sm:flex sm:items-end sm:gap-2">
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

            {/* 动作 then：typed 互斥动作（none|throttle|fail_account）+ 独立 response shaping */}
            <div className="space-y-3">
              <p className="text-xs font-medium uppercase tracking-wide text-muted-foreground">{t('rules.thenTitle')}</p>
              <p className="text-xs text-muted-foreground">{t('rules.then.asyncHint')}</p>

              <div role="radiogroup" aria-label={t('rules.then.actionTitle')} className="grid gap-2 sm:grid-cols-3">
                {ACTIONS.map(a => {
                  const selected = form.then.action === a
                  return (
                    <label
                      key={a}
                      htmlFor={`rl-act-${a}`}
                      className={cn(
                        'flex cursor-pointer items-start gap-2 rounded-lg border p-3 text-sm transition-colors',
                        selected ? 'border-primary bg-primary/5' : 'border-border hover:bg-accent/50',
                      )}
                    >
                      <input
                        id={`rl-act-${a}`}
                        type="radio"
                        name="rl-action"
                        value={a}
                        checked={selected}
                        onChange={() => setAction(a)}
                        className="mt-0.5 size-4 shrink-0 accent-primary"
                      />
                      <span className="grid gap-0.5">
                        <span className="font-medium">{t(`rules.then.action.${a}`)}</span>
                        <span className="text-xs leading-snug text-muted-foreground">{t(`rules.then.action.${a}Hint`)}</span>
                      </span>
                    </label>
                  )
                })}
              </div>

              {form.then.action === 'throttle' && (
                <div className="grid gap-3 sm:grid-cols-3">
                  <div className="space-y-1.5">
                    <Label>{t('rules.then.throttle.scope')}</Label>
                    <Select
                      items={{ account: t('rules.then.throttle.scopeAccount'), account_route: t('rules.then.throttle.scopeAccountRoute') }}
                      value={form.then.throttle.scope}
                      onValueChange={v => v && setThrottle({ scope: v as ThrottleScope })}
                    >
                      <SelectTrigger className="w-full"><SelectValue /></SelectTrigger>
                      <SelectContent>
                        <SelectItem value="account" label={t('rules.then.throttle.scopeAccount')}>{t('rules.then.throttle.scopeAccount')}</SelectItem>
                        <SelectItem value="account_route" label={t('rules.then.throttle.scopeAccountRoute')}>{t('rules.then.throttle.scopeAccountRoute')}</SelectItem>
                      </SelectContent>
                    </Select>
                  </div>
                  <div className="space-y-1.5">
                    <Label>{t('rules.then.throttle.mode')}</Label>
                    <Select
                      items={{ retry_after: t('rules.then.throttle.modeRetryAfter'), open: t('rules.then.throttle.modeOpen') }}
                      value={form.then.throttle.mode}
                      onValueChange={v => v && setThrottle({ mode: v as ThrottleMode })}
                    >
                      <SelectTrigger className="w-full"><SelectValue /></SelectTrigger>
                      <SelectContent>
                        <SelectItem value="retry_after" label={t('rules.then.throttle.modeRetryAfter')}>{t('rules.then.throttle.modeRetryAfter')}</SelectItem>
                        <SelectItem value="open" label={t('rules.then.throttle.modeOpen')}>{t('rules.then.throttle.modeOpen')}</SelectItem>
                      </SelectContent>
                    </Select>
                  </div>
                  <div className="space-y-1.5">
                    <Label htmlFor="rl-th-dur">{t('rules.then.throttle.duration')}</Label>
                    <Input
                      id="rl-th-dur"
                      type="number"
                      min={1}
                      step={1000}
                      placeholder="30000"
                      value={form.then.throttle.durationMs}
                      onChange={e => setThrottle({ durationMs: e.target.value })}
                    />
                  </div>
                  <p className="text-xs text-muted-foreground sm:col-span-3">
                    {form.then.throttle.mode === 'open' ? t('rules.then.throttle.durationOpenHint') : t('rules.then.throttle.durationRetryHint')}
                  </p>
                  <p className="text-xs text-muted-foreground sm:col-span-3">
                    {form.then.throttle.scope === 'account' ? t('rules.then.throttle.scopeAccountHint') : t('rules.then.throttle.scopeAccountRouteHint')}
                  </p>
                </div>
              )}

              {form.then.action === 'fail_account' && (
                <Alert variant="destructive">
                  <AlertTitle>{t('rules.then.fail.title')}</AlertTitle>
                  <AlertDescription className="space-y-2">
                    <p>{t('rules.then.fail.desc')}</p>
                    <label className="flex cursor-pointer items-center gap-2 text-sm font-medium">
                      <Checkbox checked={form.then.failConfirmed} onCheckedChange={c => setForm(f => ({ ...f, then: { ...f.then, failConfirmed: c === true } }))} />
                      {t('rules.then.fail.confirm')}
                    </label>
                  </AlertDescription>
                </Alert>
              )}

              {form.then.action === 'none' && Object.keys(form.then.legacy).length > 0 && (
                <p className="text-xs text-muted-foreground">{t('rules.then.legacyEcho', { keys: Object.keys(form.then.legacy).join(', ') })}</p>
              )}

              <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
                <div className="space-y-1.5">
                  <Label htmlFor="rl-rc">{t('rules.then.responseCode')}</Label>
                  <Input id="rl-rc" type="number" min={400} max={599} step={1} placeholder="503" value={form.then.responseCode} onChange={e => setThen('responseCode', e.target.value)} />
                </div>
                <div className="space-y-1.5">
                  <Label htmlFor="rl-cm">{t('rules.then.customMessage')}</Label>
                  <Input id="rl-cm" placeholder={t('rules.then.customMessagePlaceholder')} value={form.then.customMessage} onChange={e => setThen('customMessage', e.target.value)} />
                </div>
              </div>
              <p className="text-xs text-muted-foreground">{t('rules.then.responseHint')}</p>
              {form.then.responseCode !== '' && (() => { const n = Number(form.then.responseCode); return !Number.isNaN(n) && (n < 400 || n > 599) })() && (
                <p className="text-sm text-destructive">{t('rules.then.errResponseRange')}</p>
              )}
            </div>

            {save.isError && errMsg(save.error) && (
              <p className="text-sm text-destructive">{errMsg(save.error)}</p>
            )}
          </div>
          </ScrollArea>
          <DialogFooter>
            <Button variant="outline" onClick={() => setDialogOpen(false)} disabled={save.isPending}>{t('common.cancel')}</Button>
            <Button
              onClick={submit}
              disabled={save.isPending || !form.name.trim() || form.priority === '' || (form.then.action === 'fail_account' && !form.then.failConfirmed)}
            >
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
