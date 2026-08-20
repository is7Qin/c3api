// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

import { useMemo, useRef, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { motion } from 'framer-motion'
import { Plus, Pencil, Trash2, FolderOpen, Filter, UserPlus, X } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { api } from '@/App'
import { ApiUnauthorized } from '@/lib/api/client'
import { BatchBar } from '@/components/batch-bar'
import { ListToolbar } from '@/components/list-toolbar'
import { Pagination } from '@/components/pagination'
import { SortableHeader, type SortOrder } from '@/components/sortable-header'
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
import { Badge } from '@/components/ui/badge'
import { toast } from '@/components/ui/toast'
import { formatDateTime } from '@/components/fmt'
import { cn } from '@/lib/utils'
import type { TFunction } from 'i18next'
import type { components } from '@/lib/api/schema'

type Group = components['schemas']['Group']
type GroupVisibility = components['schemas']['GroupVisibility']
type GroupProtocolConvert = components['schemas']['GroupProtocolConvert']
type GroupAssignmentsBody = components['schemas']['GroupAssignmentsBody']

// 协议转换方向（W5 网关 internal/protoconv 消费）：4 方向可多选，全不勾 = off =
// 不转换（off 不进数组，空勾选表达）
const PROTOCOL_CONVERTS: GroupProtocolConvert[] = ['chat_to_resp', 'mess_to_resp', 'resp_to_mess', 'chat_to_mess']

// 协议转换多选切换：勾选 = 加入方向集合（同方向去重），取消 = 移除。
const toggleConvert = (on: boolean, v: GroupProtocolConvert, cur: GroupProtocolConvert[]) =>
  on ? (cur.includes(v) ? cur : [...cur, v]) : cur.filter(x => x !== v)

// 协议转换多选 checkbox 组（创建/编辑共用）：4 方向勾选，全不勾 = off。
function ProtocolConvertCheckboxes({ value, onChange }: {
  value: GroupProtocolConvert[]
  onChange: (v: GroupProtocolConvert[]) => void
}) {
  const { t } = useTranslation()
  return (
    <div className="space-y-1">
      {PROTOCOL_CONVERTS.map(v => (
        <label key={v} className="flex cursor-pointer items-center gap-2.5 rounded-md border px-2 py-1.5 text-sm">
          <Checkbox checked={value.includes(v)} onCheckedChange={c => onChange(toggleConvert(c === true, v, value))} />
          {t(`groups.protocolConvert.${v}`)}
        </label>
      ))}
    </div>
  )
}

// 授予弹窗行内专属倍率态：mult = 输入框文本（'' = 未填）；cleared = 用户显式点过
// 「清除为未设置」（提交 null）；勾选留空且未清除 = 省略键（沿用当前值）。
interface AssignRowMult { mult: string; cleared: boolean }

// 价格倍率（正常值，API 边界已换算）→ 展示：null = 未设置（—）；0 = 免费；
// 其余 ×N（1 = ×1.0，1.5 = ×1.5）。
const formatMultiplier = (m: number | null | undefined, t: TFunction): string => {
  if (m == null) return '—'
  if (m === 0) return t('groups.free')
  return `×${m.toFixed(1)}`
}

// 倍率输入归一（创建/编辑共用）：空 = undefined（省略键）；非数字/越界抛错；
// 正常值返回数字（0 = 免费组，1 = ×1，上限 10）。
const normalizeMultiplierInput = (v: string, invalidMsg: string): number | undefined => {
  const m = v.trim()
  if (m === '') return undefined
  const n = Number(m)
  if (!Number.isFinite(n) || n < 0 || n > 10) throw new Error(invalidMsg)
  return n
}

// 可见性徽章：public 绿点 / private 灰点（与 StatusBadge 同风格）。
function VisibilityBadge({ visibility }: { visibility?: GroupVisibility }) {
  const { t } = useTranslation()
  const isPublic = visibility === 'public'
  return (
    <Badge variant="secondary" className={cn('gap-1.5', isPublic ? 'text-emerald-700 dark:text-emerald-400' : 'text-muted-foreground')}>
      <span className={cn('size-1.5 shrink-0 rounded-full', isPublic ? 'bg-emerald-500' : 'bg-muted-foreground/60')} />
      {t(isPublic ? 'groups.visibilityPublic' : 'groups.visibilityPrivate')}
    </Badge>
  )
}

// 授予弹窗用户行：勾选 + 用户标识 + 专属倍率三态输入（public 默认列表与搜索列表共用）。
function AssignUserRow({ uid, label, checked, row, onToggle, onMult, onClear, t }: {
  uid: number
  label: string
  checked: boolean
  row: AssignRowMult | undefined
  onToggle: (uid: number, on: boolean) => void
  onMult: (uid: number, v: string) => void
  onClear: (uid: number) => void
  t: TFunction
}) {
  return (
    <div className="flex items-center gap-2.5 rounded-md border px-2 py-1.5">
      <Checkbox checked={checked} onCheckedChange={c => onToggle(uid, c === true)} />
      <span className="min-w-0 flex-1 truncate text-sm" title={label}>{label}</span>
      {checked && (
        <>
          <Input
            type="number"
            min={0}
            max={10}
            step={0.1}
            value={row?.mult ?? ''}
            placeholder={t('groups.assignMultiplierPlaceholder')}
            onChange={e => onMult(uid, e.target.value)}
            className="h-7 w-24 text-xs"
          />
          <Button
            variant="ghost"
            size="icon-sm"
            title={t('groups.assignMultiplierClear')}
            disabled={!row?.mult && !row?.cleared}
            onClick={() => onClear(uid)}
          >
            <X />
          </Button>
          {row?.cleared && (
            <span className="w-24 text-xs text-muted-foreground">{t('groups.assignMultiplierUnset')}</span>
          )}
        </>
      )}
    </div>
  )
}

export default function Groups() {
  const { t } = useTranslation()
  const qc = useQueryClient()

  // —— 列表：筛选/分页状态归 queryKey ——
  const [name, setName] = useState('')
  const [activeSort, setActiveSort] = useState<string | null>(null) // null = 无主动排序（默认 id desc）
  const [order, setOrder] = useState<SortOrder>('desc')
  const [offset, setOffset] = useState(0)
  const [limit, setLimit] = useState(20)

  const { data, isLoading, isError, error } = useQuery({
    queryKey: ['groups', { limit, offset, name, sort: activeSort ?? 'id', order }],
    queryFn: () => api.listGroups({ limit, offset, name: name || undefined, sort: activeSort ?? 'id', order }),
  })
  const rows = data?.rows ?? []

  // —— 行勾选（跨页保留，筛选/翻页后清空）——
  const [selected, setSelected] = useState<number[]>([])
  const pageIds = rows.map(r => r.ID!)
  const allChecked = rows.length > 0 && pageIds.every(id => selected.includes(id))
  const someChecked = pageIds.some(id => selected.includes(id))
  const toggleRow = (id: number) => setSelected(s => (s.includes(id) ? s.filter(x => x !== id) : [...s, id]))
  const toggleAll = (c: boolean) =>
    setSelected(s => (c ? Array.from(new Set([...s, ...pageIds])) : s.filter(x => !pageIds.includes(x))))

  const resetPage = () => {
    setOffset(0)
    setSelected([])
  }
  // 每页条数变化 → 重置 offset 并清勾选。
  const changeLimit = (l: number) => { setLimit(l); resetPage() }
  const changeName = (v: string) => { setName(v); resetPage() }
  // 列头三态：新列 → 降序；同列降序 → 升序；同列升序 → 取消（回默认 id desc）
  const onColumnToggle = (col: string) => {
    resetPage()
    if (activeSort !== col) {
      setActiveSort(col)
      setOrder('desc')
    } else if (order === 'desc') {
      setOrder('asc')
    } else {
      setActiveSort(null)
      setOrder('desc')
    }
  }
  const hasFilters = name !== ''
  const clearFilters = () => {
    setName('')
    resetPage()
  }

  // —— 批量删除/重命名 ——
  const batchDelete = useMutation({
    mutationFn: (ids: number[]) => api.deleteGroupsBatch(ids),
    onSuccess: (_res, ids) => {
      qc.invalidateQueries({ queryKey: ['groups'] })
      setSelected([])
      // 当前页被删空时回到最后有效页（templates 同款守卫，不再一律回第 1 页）
      const after = (data?.total ?? 0) - ids.length
      if (offset > 0 && offset >= after) setOffset(Math.max(0, after - (after % limit)))
    },
  })
  const batchRename = useMutation({
    mutationFn: (p: { ids: number[]; name: string }) => api.updateGroupsBatch(p.ids, { name: p.name }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['groups'] })
      setSelected([])
      closeBatchRename('submitted')
    },
  })
  // BatchBar 的 onUpdate 返回 promise：对话框关闭（提交成功/取消）时 resolve。
  const [batchRenameOpen, setBatchRenameOpen] = useState(false)
  const [batchRenameValue, setBatchRenameValue] = useState('')
  const [batchRenameErr, setBatchRenameErr] = useState<string | null>(null)
  const batchResolve = useRef<((r: 'cancelled' | 'submitted') => void) | null>(null)
  const closeBatchRename = (r: 'cancelled' | 'submitted' = 'cancelled') => {
    setBatchRenameOpen(false)
    batchResolve.current?.(r)
    batchResolve.current = null
  }
  const openBatchRename = () => {
    setBatchRenameValue('')
    setBatchRenameErr(null)
    setBatchRenameOpen(true)
  }
  const submitBatchRename = () => {
    if (!batchRenameValue.trim()) {
      setBatchRenameErr(t('groups.batchUpdateEmpty'))
      return
    }
    batchRename.mutate({ ids: selected, name: batchRenameValue.trim() })
  }

  // —— 授予用户（替换语义：勾选 = 授予，未勾选 = 撤销；打开时并行预填充当前授予）——
  const [assignTarget, setAssignTarget] = useState<Group | null>(null)
  const [assignChecked, setAssignChecked] = useState<number[]>([])
  const [assignMult, setAssignMult] = useState<Record<number, AssignRowMult>>({})
  const [assignQuery, setAssignQuery] = useState('')
  const [assignOffset, setAssignOffset] = useState(0)
  const [assignLimit, setAssignLimit] = useState(20)
  // 预填充态：prefilled = 已成功回显当前授予；prefillFailed = 读端点不可用（空预填充 + toast，不阻塞弹窗）
  const [assignPrefilled, setAssignPrefilled] = useState(false)
  const [assignPrefillFailed, setAssignPrefillFailed] = useState(false)
  // 批量倍率输入值（勾选 2+ 用户时显示批量行）
  const [assignBatchMult, setAssignBatchMult] = useState('')
  // public 组预填充的「已配置专属倍率」用户（multipliers 非 null 键）：默认列表数据源
  const [assignPrefillUids, setAssignPrefillUids] = useState<number[]>([])
  // 预填充请求代际：弹窗关闭/换组后丢弃过期响应，防止旧组数据串入新组
  const assignFetchId = useRef(0)

  const openAssign = (g: Group) => {
    const isPublic = g.Visibility === 'public'
    setAssignTarget(g)
    setAssignChecked([])
    setAssignMult({})
    setAssignPrefillUids([])
    setAssignQuery('')
    setAssignOffset(0)
    setAssignPrefilled(false)
    setAssignPrefillFailed(false)
    setAssignBatchMult('')
    assign.reset() // 清掉上次提交失败的就地错误，换组重开不残留
    // 预填充：并行读当前授予 → 勾选态 = 已授予 user_ids，倍率输入框初值 = multipliers 正常值。
    // public 分支：公开组天然所有用户可用，无「授予」概念 → 初始勾选 = multipliers 非 null
    // 键（已配置专属倍率的用户），不用 user_ids（可能含清除残留行，不代表配置态）。
    // 读端点不可用（后端未实现 404/网络）→ 优雅降级：空预填充 + toast，弹窗正常打开。
    const gid = g.ID!
    const fetchId = ++assignFetchId.current
    api.getGroupAssignments(gid)
      .then(resp => {
        if (assignFetchId.current !== fetchId) return
        const muls: Record<number, AssignRowMult> = {}
        const prefilledUids: number[] = []
        for (const [uid, m] of Object.entries(resp.multipliers ?? {})) {
          const id = Number(uid)
          if (isPublic) {
            // public：只取非 null 键（null = 已清除 → 不勾选不显示）
            if (typeof m === 'number') {
              prefilledUids.push(id)
              muls[id] = { mult: String(m), cleared: false }
            }
          } else if (typeof m === 'number' && resp.user_ids.includes(id)) {
            // private：仅回显已授予用户的数值倍率；null = 未设置 → 留空（省略键沿用当前值，语义不变）
            muls[id] = { mult: String(m), cleared: false }
          }
        }
        prefilledUids.sort((a, b) => a - b)
        setAssignChecked(isPublic ? prefilledUids : resp.user_ids)
        setAssignMult(muls)
        // 默认列表数据源：public = 已配置专属倍率的用户；private = 已授予权限的用户全量
        setAssignPrefillUids(isPublic ? prefilledUids : resp.user_ids)
        setAssignPrefilled(true)
      })
      .catch(() => {
        if (assignFetchId.current !== fetchId) return
        setAssignPrefillFailed(true)
        toast.add({ title: t('groups.assignPrefillFailed'), type: 'error' })
      })
  }
  const toggleAssignUser = (id: number, on: boolean) =>
    setAssignChecked(s => (on ? (s.includes(id) ? s : [...s, id]) : s.filter(x => x !== id)))
  const assignIsPublic = assignTarget?.Visibility === 'public'
  // 空搜索时默认列表 = 预填充的已授予/已配置用户 ∪ 当前勾选（搜索新增的也保留）：
  // public = 已配置专属倍率的用户；private = 已授予权限的用户。不拉全量用户列表，
  // 只有搜索才显示用户列表（勾选 = 新增）。
  const assignDefaultIds = Array.from(new Set([...assignPrefillUids, ...assignChecked])).sort((a, b) => a - b)
  // 默认列表的邮箱尽力解析：预填充响应只有 uid（读端点不返回邮箱），
  // 用全量用户查询按 id 匹配（accounts 弹窗 listGroups(100) 同款先例）；未命中回退 #uid。
  const assignUsersLookup = useQuery({
    queryKey: ['users', 'assign-lookup'],
    queryFn: () => api.listUsers({ limit: 100 }),
    enabled: !!assignTarget && assignPrefillUids.length > 0,
  })
  const assignEmailMap = useMemo(() => {
    const m = new Map<number, string>()
    for (const u of assignUsersLookup.data?.rows ?? []) m.set(u.ID!, u.Email ?? '')
    return m
  }, [assignUsersLookup.data])
  const assignUsers = useQuery({
    queryKey: ['users', 'assign', { limit: assignLimit, offset: assignOffset, email: assignQuery }],
    queryFn: () => api.listUsers({ limit: assignLimit, offset: assignOffset, email: assignQuery || undefined }),
    // 空搜索时不显示用户列表（默认列表用预填充），跳过无谓的全量拉取（输入搜索词后自动启用）
    enabled: !!assignTarget && assignQuery !== '',
  })
  const assignRows = assignUsers.data?.rows ?? []
  const assignTotal = assignUsers.data?.total ?? 0

  // multipliers 三态：勾选且填值 → 数字；勾选留空（未清除）→ 省略键（沿用当前值）；
  // 勾选且显式清除 → null（回退组倍率）。
  const assign = useMutation({
    mutationFn: () => {
      const body: GroupAssignmentsBody = { user_ids: assignChecked }
      const muls: Record<string, number | null> = {}
      for (const uid of assignChecked) {
        const row = assignMult[uid]
        const v = row?.mult.trim()
        if (v !== undefined && v !== '') {
          const n = Number(v)
          if (!Number.isFinite(n) || n < 0 || n > 10) throw new Error(t('groups.multiplierInvalid'))
          muls[String(uid)] = n
        } else if (row?.cleared) {
          muls[String(uid)] = null
        }
      }
      if (Object.keys(muls).length > 0) body.multipliers = muls
      return api.setGroupAssignments(assignTarget!.ID!, body)
    },
    onSuccess: (resp) => {
      setAssignTarget(null)
      // 空勾选提交 = 清空（契约语义）：toast 用清空文案，避免「已授予 0 个用户」歧义。
      // public 组文案用「配置专属倍率」语义，private 组用「授予」语义。
      toast.add({
        title: t(assignIsPublic ? 'groups.assignPublicSuccess' : 'groups.assignSuccess'),
        description: resp.user_ids.length > 0
          ? t(assignIsPublic ? 'groups.assignConfiguredCount' : 'groups.assignSuccessDesc', { count: resp.user_ids.length })
          : t(assignIsPublic ? 'groups.assignConfiguredClearedDesc' : 'groups.assignClearedDesc'),
        type: 'success',
      })
    },
  })
  const setRowMult = (uid: number, mult: string) => setAssignMult(m => ({ ...m, [uid]: { mult, cleared: false } }))
  const clearRowMult = (uid: number) => setAssignMult(m => ({ ...m, [uid]: { mult: '', cleared: true } }))
  // 批量倍率：勾选 2+ 用户时整行统一设置/清除（对所有勾选用户生效，覆盖其行内值）
  const applyBatchMult = () => {
    const v = assignBatchMult.trim()
    if (v === '' || assignChecked.length < 2) return
    const n = Number(v)
    if (!Number.isFinite(n) || n < 0 || n > 10) {
      toast.add({ title: t('groups.multiplierInvalid'), type: 'error' })
      return
    }
    setAssignMult(m => {
      const next = { ...m }
      for (const uid of assignChecked) next[uid] = { mult: String(n), cleared: false }
      return next
    })
  }
  const clearBatchMult = () => {
    setAssignMult(m => {
      const next = { ...m }
      for (const uid of assignChecked) next[uid] = { mult: '', cleared: true }
      return next
    })
  }

  // —— 创建（表单：name + visibility + protocol_convert 多选 + price_multiplier；
  //     倍率留空 = 省略键，后端按 ×1）——
  const [createOpen, setCreateOpen] = useState(false)
  const [createName, setCreateName] = useState('')
  const [createVisibility, setCreateVisibility] = useState<GroupVisibility>('public')
  const [createProtocols, setCreateProtocols] = useState<GroupProtocolConvert[]>([])
  const [createMultiplier, setCreateMultiplier] = useState('')
  const openCreate = () => {
    setCreateName('')
    setCreateVisibility('public')
    setCreateProtocols([])
    setCreateMultiplier('')
    setCreateOpen(true)
  }
  const create = useMutation({
    mutationFn: (n: string) => {
      const body: components['schemas']['GroupCreate'] = {
        name: n,
        visibility: createVisibility,
        protocol_convert: createProtocols, // 空数组 = off = 不转换
      }
      const m = normalizeMultiplierInput(createMultiplier, t('groups.multiplierInvalid'))
      if (m !== undefined) body.price_multiplier = m // 正常值直接提交；输入为空则省略键（后端按 ×1）
      return api.createGroup(body)
    },
    onSuccess: (_g, name) => {
      qc.invalidateQueries({ queryKey: ['groups'] })
      setCreateOpen(false)
      toast.add({ title: t('groups.createdSuccess'), description: name, type: 'success' })
    },
  })
  // —— 编辑（name + visibility + protocol_convert 多选；PUT 缺省字段保持原值，
  //      此处总是显式提交——空数组 = 清空既有方向）——
  const [editTarget, setEditTarget] = useState<Group | null>(null)
  const [editName, setEditName] = useState('')
  const [editVisibility, setEditVisibility] = useState<GroupVisibility>('public')
  const [editProtocols, setEditProtocols] = useState<GroupProtocolConvert[]>([])
  // 倍率用字符串态：空 = 不修改（PUT 省略键，后端保持原值）
  const [editMultiplier, setEditMultiplier] = useState('')
  // —— 删除 ——
  const [deleting, setDeleting] = useState<Group | null>(null)

  const rename = useMutation({
    mutationFn: () => {
      const body: components['schemas']['GroupCreate'] = { name: editName.trim(), visibility: editVisibility, protocol_convert: editProtocols }
      const m = normalizeMultiplierInput(editMultiplier, t('groups.multiplierInvalid'))
      if (m !== undefined) body.price_multiplier = m // 正常值直接提交；输入为空则省略键（后端保持原值）
      return api.updateGroup(editTarget!.ID!, body)
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['groups'] })
      setEditTarget(null)
    },
  })
  const remove = useMutation({
    mutationFn: (id: number) => api.deleteGroup(id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['groups'] })
      setDeleting(null)
      // 删除的是当前页最后一行时回退一页（templates 同款「最后有效页」守卫）
      if (rows.length === 1 && offset > 0) setOffset(offset - limit)
    },
  })

  const errMsg = (e: unknown) => (e instanceof ApiUnauthorized ? null : (e as Error)?.message)

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-semibold tracking-tight">{t('groups.title')}</h1>
          <p className="text-sm text-muted-foreground">{t('groups.subtitle')}</p>
        </div>
        <Button onClick={openCreate}><Plus /> {t('groups.new')}</Button>
      </div>

      <ListToolbar
        name={name}
        onNameChange={changeName}
      />

      <BatchBar
        selected={selected}
        onClear={() => setSelected([])}
        onDelete={async () => {
          await batchDelete.mutateAsync(selected)
        }}
        onUpdate={() => new Promise<'cancelled' | 'submitted'>(resolve => {
          batchResolve.current = resolve
          openBatchRename()
        })}
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
            <FolderOpen className="size-10" />
            <p className="font-medium">{hasFilters ? t('groups.filterEmpty') : t('groups.emptyTitle')}</p>
            {!hasFilters && <p className="text-sm">{t('groups.emptyDesc')}</p>}
            {hasFilters ? (
              <Button className="mt-2" variant="outline" onClick={clearFilters}><Filter /> {t('list.reset')}</Button>
            ) : (
              <Button className="mt-2" onClick={openCreate}><Plus /> {t('groups.new')}</Button>
            )}
          </Card>
        </motion.div>
      ) : (
        <>
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
                  <SortableHeader field="id" label="ID" active={activeSort === 'id'} order={order} onToggle={onColumnToggle} />
                  <SortableHeader field="name" label={t('groups.table.name')} active={activeSort === 'name'} order={order} onToggle={onColumnToggle} />
                  <TableHead>{t('groups.table.visibility')}</TableHead>
                  <TableHead>{t('groups.table.priceMultiplier')}</TableHead>
                  <TableHead>{t('groups.table.protocolConvert')}</TableHead>
                  <SortableHeader field="created_at" label={t('groups.table.createdAt')} active={activeSort === 'created_at'} order={order} onToggle={onColumnToggle} />
                  <TableHead className="text-right">{t('groups.table.actions')}</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody className="[&_td]:py-3">
                {rows.map(g => (
                  <TableRow key={g.ID} data-state={selected.includes(g.ID!) ? 'selected' : undefined}>
                    <TableCell>
                      <Checkbox checked={selected.includes(g.ID!)} onCheckedChange={() => toggleRow(g.ID!)} />
                    </TableCell>
                    <TableCell className="tabular-nums">{g.ID}</TableCell>
                    <TableCell className="max-w-36 truncate" title={g.Name}>{g.Name}</TableCell>
                    <TableCell><VisibilityBadge visibility={g.Visibility} /></TableCell>
                    <TableCell className="tabular-nums">{formatMultiplier(g.PriceMultiplier, t)}</TableCell>
                    <TableCell>
                      {g.ProtocolConvert && g.ProtocolConvert.length > 0 ? (
                        <div className="flex flex-wrap gap-1">
                          {g.ProtocolConvert.map(pc => (
                            <Badge key={pc} variant="secondary" className="font-mono text-xs">{t(`groups.protocolConvertShort.${pc}`)}</Badge>
                          ))}
                        </div>
                      ) : (
                        <span className="text-xs text-muted-foreground">{t('groups.protocolConvertShort.off')}</span>
                      )}
                    </TableCell>
                    <TableCell className="text-xs text-muted-foreground">{formatDateTime(g.CreatedAt)}</TableCell>
                    <TableCell className="text-right">
                      <div className="flex justify-end gap-1">
                        <Button variant="ghost" size="icon-sm" title={t('groups.assignButton')} onClick={() => openAssign(g)}><UserPlus /></Button>
                        <Button variant="ghost" size="icon-sm" title={t('common.edit')} onClick={() => { setEditTarget(g); setEditName(g.Name ?? ''); setEditVisibility(g.Visibility ?? 'public'); setEditProtocols(g.ProtocolConvert ?? []); setEditMultiplier(g.PriceMultiplier != null ? String(g.PriceMultiplier) : '') }}><Pencil /></Button>
                        <Button variant="ghost" size="icon-sm" className="text-destructive" title={t('common.delete')} onClick={() => setDeleting(g)}><Trash2 /></Button>
                      </div>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </div>
          <Pagination total={data?.total ?? 0} limit={limit} offset={offset} onOffsetChange={setOffset} onLimitChange={changeLimit} />
        </>
      )}

      {/* —— 创建分组：表单（name + visibility）；创建成功仅提示，不再返回 key 明文 —— */}
      <Dialog open={createOpen} onOpenChange={setCreateOpen}>
        <DialogContent className="sm:max-w-sm">
          <DialogHeader>
            <DialogTitle>{t('groups.newTitle')}</DialogTitle>
            <DialogDescription>{t('groups.newDesc')}</DialogDescription>
          </DialogHeader>
          <div className="space-y-3">
            <div className="space-y-1.5">
              <Label htmlFor="grp-name">{t('groups.nameLabel')}</Label>
              <Input
                id="grp-name"
                value={createName}
                placeholder={t('groups.namePlaceholder')}
                onChange={e => setCreateName(e.target.value)}
                onKeyDown={e => { if (e.key === 'Enter' && createName.trim() && !create.isPending) create.mutate(createName.trim()) }}
              />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="grp-visibility">{t('groups.visibilityLabel')}</Label>
              <Select
                items={Object.fromEntries([['public', t('groups.visibilityPublic')], ['private', t('groups.visibilityPrivate')]])}
                value={createVisibility}
                onValueChange={v => setCreateVisibility(v as GroupVisibility)}
              >
                <SelectTrigger id="grp-visibility" className="w-full"><SelectValue /></SelectTrigger>
                <SelectContent>
                  <SelectItem value="public" label={t('groups.visibilityPublic')}>{t('groups.visibilityPublic')}</SelectItem>
                  <SelectItem value="private" label={t('groups.visibilityPrivate')}>{t('groups.visibilityPrivate')}</SelectItem>
                </SelectContent>
              </Select>
            </div>
            <div className="space-y-1.5">
              <Label>{t('groups.protocolConvertLabel')}</Label>
              <ProtocolConvertCheckboxes value={createProtocols} onChange={setCreateProtocols} />
              <p className="text-xs text-muted-foreground">{t('groups.protocolConvertHint')}</p>
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="grp-create-multiplier">{t('groups.multiplierLabel')}</Label>
              <Input
                id="grp-create-multiplier"
                type="number"
                min={0}
                max={10}
                step={0.1}
                value={createMultiplier}
                placeholder="1"
                onChange={e => setCreateMultiplier(e.target.value)}
                onKeyDown={e => { if (e.key === 'Enter' && createName.trim() && !create.isPending) create.mutate(createName.trim()) }}
              />
              <p className="text-xs text-muted-foreground">{t('groups.createMultiplierHint')}</p>
            </div>
            {create.isError && errMsg(create.error) && (
              <p className="text-sm text-destructive">{errMsg(create.error)}</p>
            )}
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setCreateOpen(false)} disabled={create.isPending}>{t('common.cancel')}</Button>
            <Button onClick={() => create.mutate(createName.trim())} disabled={create.isPending || !createName.trim()}>
              {create.isPending ? t('common.creating') : t('common.create')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* —— 编辑（name + visibility） —— */}
      <Dialog open={!!editTarget} onOpenChange={o => { if (!o && !rename.isPending) setEditTarget(null) }}>
        <DialogContent className="sm:max-w-sm">
          <DialogHeader>
            <DialogTitle>{t('groups.editTitle', { id: editTarget?.ID })}</DialogTitle>
          </DialogHeader>
          <div className="space-y-3">
            <div className="space-y-1.5">
              <Label htmlFor="grp-edit-name">{t('groups.nameLabel')}</Label>
              <Input id="grp-edit-name" value={editName} onChange={e => setEditName(e.target.value)} />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="grp-edit-visibility">{t('groups.visibilityLabel')}</Label>
              <Select
                items={Object.fromEntries([['public', t('groups.visibilityPublic')], ['private', t('groups.visibilityPrivate')]])}
                value={editVisibility}
                onValueChange={v => setEditVisibility(v as GroupVisibility)}
              >
                <SelectTrigger id="grp-edit-visibility" className="w-full"><SelectValue /></SelectTrigger>
                <SelectContent>
                  <SelectItem value="public" label={t('groups.visibilityPublic')}>{t('groups.visibilityPublic')}</SelectItem>
                  <SelectItem value="private" label={t('groups.visibilityPrivate')}>{t('groups.visibilityPrivate')}</SelectItem>
                </SelectContent>
              </Select>
            </div>
            <div className="space-y-1.5">
              <Label>{t('groups.protocolConvertLabel')}</Label>
              <ProtocolConvertCheckboxes value={editProtocols} onChange={setEditProtocols} />
              <p className="text-xs text-muted-foreground">{t('groups.protocolConvertHint')}</p>
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="grp-edit-multiplier">{t('groups.multiplierLabel')}</Label>
              <Input
                id="grp-edit-multiplier"
                type="number"
                min={0}
                max={10}
                step={0.1}
                value={editMultiplier}
                onChange={e => setEditMultiplier(e.target.value)}
                onKeyDown={e => { if (e.key === 'Enter' && editName.trim() && !rename.isPending) rename.mutate() }}
              />
              <p className="text-xs text-muted-foreground">{t('groups.multiplierHint')}</p>
            </div>
            {rename.isError && errMsg(rename.error) && (
              <p className="text-sm text-destructive">{errMsg(rename.error)}</p>
            )}
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setEditTarget(null)} disabled={rename.isPending}>{t('common.cancel')}</Button>
            <Button onClick={() => rename.mutate()} disabled={rename.isPending || !editName.trim()}>
              {rename.isPending ? t('common.saving') : t('common.save')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* —— 删除确认（单行） —— */}
      <Dialog open={!!deleting} onOpenChange={o => { if (!o && !remove.isPending) setDeleting(null) }}>
        <DialogContent className="sm:max-w-sm">
          <DialogHeader>
            <DialogTitle>{t('groups.deleteTitle')}</DialogTitle>
            <DialogDescription>
              {t('groups.deleteDesc', { name: deleting?.Name })}
            </DialogDescription>
          </DialogHeader>
          {remove.isError && errMsg(remove.error) && (
            <p className="text-sm text-destructive">{errMsg(remove.error)}</p>
          )}
          <DialogFooter>
            <Button variant="outline" onClick={() => setDeleting(null)} disabled={remove.isPending}>{t('common.cancel')}</Button>
            <Button variant="destructive" onClick={() => deleting && remove.mutate(deleting.ID!)} disabled={remove.isPending}>
              {remove.isPending ? t('common.deleting') : t('common.confirmDelete')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* —— 批量更新对话框：仅 name —— */}
      <Dialog open={batchRenameOpen} onOpenChange={o => { if (!o) closeBatchRename() }}>
        <DialogContent className="sm:max-w-sm">
          <DialogHeader>
            <DialogTitle>{t('groups.batchUpdateTitle')}</DialogTitle>
            <DialogDescription>{t('groups.batchUpdateDesc', { count: selected.length })}</DialogDescription>
          </DialogHeader>
          <div className="space-y-3">
            <div className="space-y-1.5">
              <Label htmlFor="grp-batch-name">{t('groups.nameLabel')}</Label>
              <Input
                id="grp-batch-name"
                value={batchRenameValue}
                placeholder={t('groups.namePlaceholder')}
                onChange={e => setBatchRenameValue(e.target.value)}
                onKeyDown={e => { if (e.key === 'Enter' && batchRenameValue.trim() && !batchRename.isPending) submitBatchRename() }}
              />
            </div>
            {batchRenameErr && <p className="text-sm text-destructive">{batchRenameErr}</p>}
            {batchRename.isError && errMsg(batchRename.error) && (
              <p className="text-sm text-destructive">{errMsg(batchRename.error)}</p>
            )}
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => closeBatchRename()} disabled={batchRename.isPending}>{t('common.cancel')}</Button>
            <Button onClick={submitBatchRename} disabled={batchRename.isPending || !batchRenameValue.trim()}>
              {batchRename.isPending ? t('common.saving') : t('list.batchUpdate')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* —— 授予用户：替换语义（勾选 = 授予，未勾选 = 撤销）+ 专属倍率三态；
             public 组 = 专属倍率管理（默认列表只显示已配置用户，新增只能走搜索） —— */}
      <Dialog open={!!assignTarget} onOpenChange={o => { if (!o && !assign.isPending) setAssignTarget(null) }}>
        <DialogContent className="sm:max-w-lg">
          <DialogHeader>
            {/* public：公开组无「授予」概念，弹窗 = 专属倍率管理；private：授予权限语义 */}
            <DialogTitle>{t(assignIsPublic ? 'groups.assignPublicTitle' : 'groups.assignTitle', { name: assignTarget?.Name })}</DialogTitle>
            <DialogDescription>{t(assignIsPublic ? 'groups.assignPublicDesc' : 'groups.assignDesc')}</DialogDescription>
            <p className="text-xs text-muted-foreground">{t('groups.assignMultiplierHint')}</p>
            {assignPrefilled && assignChecked.length > 0 && (
              <p className="text-xs text-muted-foreground">
                {t(assignIsPublic ? 'groups.assignConfiguredCount' : 'groups.assignCount', { count: assignChecked.length })}
              </p>
            )}
            {/* 私有组：勾选 = 授予访问权（user_ids 全量替换天然含授权语义） */}
            {assignTarget?.Visibility === 'private' && (
              <p className="text-xs text-muted-foreground">{t('groups.assignPrivateHint')}</p>
            )}
            {/* 读端点不可用时才显示「无法回显」提示：正常路径已由预填充替代 */}
            {assignPrefillFailed && (
              <p className="text-xs text-amber-600 dark:text-amber-400">{t('groups.assignEchoNote')}</p>
            )}
          </DialogHeader>
          <div className="space-y-3">
            <Input
              value={assignQuery}
              placeholder={t('groups.assignSearchPlaceholder')}
              onChange={e => { setAssignQuery(e.target.value); setAssignOffset(0) }}
            />
            {assignChecked.length >= 2 && (
              <div className="flex items-center gap-2">
                <Input
                  type="number"
                  min={0}
                  max={10}
                  step={0.1}
                  value={assignBatchMult}
                  placeholder={t('groups.assignBatchPlaceholder')}
                  onChange={e => setAssignBatchMult(e.target.value)}
                  onKeyDown={e => { if (e.key === 'Enter') applyBatchMult() }}
                  className="h-7 w-40 text-xs"
                />
                <Button variant="outline" size="sm" onClick={applyBatchMult} disabled={!assignBatchMult.trim()}>
                  {t('groups.assignBatchApply')}
                </Button>
                <Button variant="ghost" size="sm" onClick={clearBatchMult}>
                  <X /> {t('groups.assignBatchClear')}
                </Button>
              </div>
            )}
            {assignQuery === '' ? (
              /* 空搜索默认列表：只显示预填充的已授予/已配置用户（∪ 搜索新增勾选）；
                 取消勾选 = 移除（行保留显示未勾选态，提交后消失）；新增只能走搜索 */
              <ScrollArea className="max-h-72 pr-1">
                <div className="space-y-1.5">
                {assignDefaultIds.length === 0 ? (
                  <p className="py-6 text-center text-sm text-muted-foreground">
                    {t(assignIsPublic ? 'groups.assignPublicSearchHint' : 'groups.assignPrivateSearchHint')}
                  </p>
                ) : (
                  assignDefaultIds.map(uid => (
                    <AssignUserRow
                      key={uid}
                      uid={uid}
                      label={assignEmailMap.get(uid) ?? `#${uid}`}
                      checked={assignChecked.includes(uid)}
                      row={assignMult[uid]}
                      onToggle={toggleAssignUser}
                      onMult={setRowMult}
                      onClear={clearRowMult}
                      t={t}
                    />
                  ))
                )}
                </div>
              </ScrollArea>
            ) : assignUsers.isLoading ? (
              <div className="space-y-1.5">
                {Array.from({ length: 4 }).map((_, i) => <Skeleton key={i} className="h-9" />)}
              </div>
            ) : assignUsers.isError ? (
              <p className="text-sm text-destructive">{t('common.loadFailed', { message: (assignUsers.error as Error).message })}</p>
            ) : assignRows.length === 0 ? (
              <p className="py-6 text-center text-sm text-muted-foreground">{t('groups.assignEmpty')}</p>
            ) : (
              <>
                <ScrollArea className="max-h-72 pr-1">
                  <div className="space-y-1.5">
                  {assignRows.map(u => (
                    <AssignUserRow
                      key={u.ID}
                      uid={u.ID!}
                      label={u.Email ?? ''}
                      checked={assignChecked.includes(u.ID!)}
                      row={assignMult[u.ID!]}
                      onToggle={toggleAssignUser}
                      onMult={setRowMult}
                      onClear={clearRowMult}
                      t={t}
                    />
                  ))}
                  </div>
                </ScrollArea>
                <Pagination total={assignTotal} limit={assignLimit} offset={assignOffset} onOffsetChange={setAssignOffset} onLimitChange={l => { setAssignLimit(l); setAssignOffset(0) }} />
              </>
            )}
            {assign.isError && errMsg(assign.error) && (
              <p className="text-sm text-destructive">{errMsg(assign.error)}</p>
            )}
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setAssignTarget(null)} disabled={assign.isPending}>{t('common.cancel')}</Button>
            <Button onClick={() => assign.mutate()} disabled={assign.isPending}>
              {assign.isPending ? t('common.saving') : t('common.save')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
}
