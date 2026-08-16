// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

import { useEffect, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { motion } from 'framer-motion'
import { Plus, Pencil, Ban, CircleCheck, UserCog, Filter, UsersRound, X, Coins } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { api } from '@/App'
import { ApiUnauthorized } from '@/lib/api/client'
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
import { StatusBadge } from '@/components/status-badge'
import { Badge } from '@/components/ui/badge'
import { toast } from '@/components/ui/toast'
import { cn } from '@/lib/utils'
import { formatDateTime, formatUSD } from '@/components/fmt'
import type { TFunction } from 'i18next'
import type { components } from '@/lib/api/schema'

type User = components['schemas']['User']
type UserCreate = components['schemas']['UserCreate']
type UserUpdate = components['schemas']['UserUpdate']
type UserRole = components['schemas']['UserRole']
type UserStatus = components['schemas']['UserStatus']
type GroupVisibility = components['schemas']['GroupVisibility']
type UserGroupsBody = components['schemas']['UserGroupsBody']

const ROLES: UserRole[] = ['platform_admin', 'user']
const STATUSES: UserStatus[] = ['active', 'disabled']

// 余额（USD 浮点，已由 API 边界换算）→ $N.NN；空 → —。
const formatBalance = (b?: number): string => (b == null ? '—' : `$${b.toFixed(2)}`)

// 角色徽章：platform_admin 蓝点（管理面）/ user 灰点（普通用户，与 groups
// VisibilityBadge 同风格）。
function RoleBadge({ role }: { role?: UserRole }) {
  const { t } = useTranslation()
  const isAdmin = role === 'platform_admin'
  return (
    <Badge variant="secondary" className={cn('gap-1.5', isAdmin ? 'text-blue-700 dark:text-blue-400' : 'text-muted-foreground')}>
      <span className={cn('size-1.5 shrink-0 rounded-full', isAdmin ? 'bg-blue-500' : 'bg-muted-foreground/60')} />
      {t(isAdmin ? 'users.role.platform_admin' : 'users.role.user')}
    </Badge>
  )
}

// 分组管理行内专属倍率态：mult = 输入框文本（'' = 未填）；cleared = 用户显式点过
// 「清除为未设置」（提交 null）；勾选留空且未清除 = 省略键（沿用当前值）。
interface AssignRowMult { mult: string; cleared: boolean }

// 组价格倍率（正常值，API 边界已换算）→ 展示：null = 未设置（—）；0 = 免费；
// 其余 ×N（1 = ×1.0，1.5 = ×1.5）。
const formatMultiplier = (m: number | null | undefined, t: TFunction): string => {
  if (m == null) return '—'
  if (m === 0) return t('groups.free')
  return `×${m.toFixed(1)}`
}

// 组可见性小标：public 绿点 / private 灰点（与 RoleBadge 同风格）。
function GroupVisibilityDot({ visibility }: { visibility?: GroupVisibility }) {
  const { t } = useTranslation()
  const isPublic = visibility === 'public'
  return (
    <span className={cn('flex shrink-0 items-center gap-1 text-xs', isPublic ? 'text-emerald-700 dark:text-emerald-400' : 'text-muted-foreground')}>
      <span className={cn('size-1.5 shrink-0 rounded-full', isPublic ? 'bg-emerald-500' : 'bg-muted-foreground/60')} />
      {t(isPublic ? 'groups.visibilityPublic' : 'groups.visibilityPrivate')}
    </span>
  )
}

// 创建/编辑共用一个表单态：email/password 仅创建时使用（email 不可变）。
interface UserForm {
  email: string
  password: string
  role: UserRole
  status: UserStatus
  max_concurrency: string
  balance: string
}

const emptyForm = (): UserForm => ({
  email: '',
  password: '',
  role: 'user',
  status: 'active',
  max_concurrency: '',
  balance: '',
})

function toForm(u: User): UserForm {
  return {
    email: u.Email ?? '',
    password: '',
    role: u.Role ?? 'user',
    status: u.Status ?? 'active',
    max_concurrency: u.MaxConcurrency == null ? '' : String(u.MaxConcurrency),
    balance: u.Balance == null ? '' : String(u.Balance),
  }
}

// 创建体：email/password 必填；数值字段空 = 不发送（后端 settings 缺省）。
function toCreateBody(f: UserForm): UserCreate {
  const body: UserCreate = {
    email: f.email.trim(),
    password: f.password,
    role: f.role,
    status: f.status,
  }
  if (f.max_concurrency !== '') body.max_concurrency = Number(f.max_concurrency)
  if (f.balance !== '') body.balance = Number(f.balance)
  return body
}

// 更新体（UserUpdate 全可选）：role/status 为 Select 必有值，总是发送
// （读改写幂等）；数值字段空 = 不发送（后端视为未提供，保持不变）。
function toUpdateBody(f: UserForm): UserUpdate {
  const body: UserUpdate = {
    role: f.role,
    status: f.status,
  }
  if (f.max_concurrency !== '') body.max_concurrency = Number(f.max_concurrency)
  if (f.balance !== '') body.balance = Number(f.balance)
  return body
}

export default function Users() {
  const { t } = useTranslation()
  const qc = useQueryClient()

  // —— 列表：筛选/分页状态归 queryKey（排序白名单：id/email/role/status/
  // max_concurrency/created_at/updated_at——balance 不可排）——
  const [email, setEmail] = useState('')
  const [activeSort, setActiveSort] = useState<string | null>(null) // null = 无主动排序（默认 id desc）
  const [order, setOrder] = useState<SortOrder>('desc')
  const [offset, setOffset] = useState(0)
  const [limit, setLimit] = useState(20)

  const { data, isLoading, isError, error } = useQuery({
    queryKey: ['users', { limit, offset, email, sort: activeSort ?? 'id', order }],
    queryFn: () => api.listUsers({ limit, offset, email: email || undefined, sort: activeSort ?? 'id', order }),
  })
  const rows = data?.rows ?? []

  const resetPage = () => setOffset(0)
  // 每页条数变化 → 重置 offset。
  const changeLimit = (l: number) => { setLimit(l); resetPage() }
  const changeEmail = (v: string) => { setEmail(v); resetPage() }
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
  const hasFilters = email !== ''
  const clearFilters = () => { setEmail(''); resetPage() }

  // —— 创建/编辑 ——
  const [dialogOpen, setDialogOpen] = useState(false)
  const [editing, setEditing] = useState<User | null>(null)
  const [form, setForm] = useState<UserForm>(emptyForm())

  const openCreate = () => {
    setEditing(null)
    setForm(emptyForm())
    setDialogOpen(true)
  }
  const openEdit = (u: User) => {
    setEditing(u)
    setForm(toForm(u))
    setDialogOpen(true)
  }

  // —— 临时额度查看（行按钮 → 弹窗；全量视角，前端过滤有效行算合计）——
  const [tempUser, setTempUser] = useState<User | null>(null)
  const tempBalancesQ = useQuery({
    queryKey: ['admin', 'temp-balances', tempUser?.ID],
    // page_size 取后端上限 1000 拉全量（额度行通常个位数——避免截断致合计少算，评审 I-1）
    queryFn: () => api.getAdminTempBalances({ user_id: tempUser!.ID!, page_size: 1000 }),
    enabled: tempUser != null,
  })
  const tempRows = tempBalancesQ.data?.rows ?? []
  // 有效合计：仅未过期（expires_at null = 永久）且正余额行——与 /user/temp-balances 同口径
  const tempActiveTotal = tempRows
    .filter((r) => r.amount_usd > 0 && (r.expires_at == null || new Date(r.expires_at) > new Date()))
    .reduce((s, r) => s + r.amount_usd, 0)

  const save = useMutation({
    mutationFn: (f: UserForm) =>
      editing ? api.updateUser(editing.ID!, toUpdateBody(f)) : api.createUser(toCreateBody(f)),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['users'] })
      setDialogOpen(false)
    },
  })
  // 启用/禁用 quick action（与 accounts 同款；无 DELETE API，不提供删除）。
  const toggleStatus = useMutation({
    mutationFn: (u: User) =>
      api.updateUser(u.ID!, { status: u.Status === 'disabled' ? 'active' : 'disabled' }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['users'] }),
  })

  // —— 分组管理（替换语义：勾选 = 授予，未勾选 = 撤销）——
  const [groupsTarget, setGroupsTarget] = useState<User | null>(null)
  const [groupsChecked, setGroupsChecked] = useState<number[]>([])
  const [groupsMult, setGroupsMult] = useState<Record<number, AssignRowMult>>({})
  const [groupsReadFailed, setGroupsReadFailed] = useState(false) // 每次打开只 toast 一次
  const [clearGroupsOpen, setClearGroupsOpen] = useState(false)

  // 全部组（组倍率/可见性展示；账号弹窗同款 limit:100 取全量）。
  const groupsAll = useQuery({
    queryKey: ['groups', 'user-groups-dialog'],
    queryFn: () => api.listGroups({ limit: 100 }),
    enabled: !!groupsTarget,
  })
  const allGroups = groupsAll.data?.rows ?? []
  // 公开组天然属于所有用户（无授予概念）：固定勾选、不进提交 body、无专属倍率。
  const publicGroupIds = new Set(allGroups.filter(g => g.Visibility === 'public').map(g => g.ID!))
  // 授予计数仅统计私有组勾选（公开组不计）。
  const grantedCount = groupsChecked.filter(id => !publicGroupIds.has(id)).length
  // 回显该用户的授予：后端读端点可能尚未实现（404）→ retry:false 快速降级，
  // 空预填充 + toast，弹窗照常可用（不崩溃）。
  const userGroupsEcho = useQuery({
    queryKey: ['user-groups', groupsTarget?.ID],
    queryFn: () => api.getUserGroups(groupsTarget!.ID!),
    enabled: !!groupsTarget,
    retry: false,
  })
  // 回显加载中（尚无数据）：禁行交互，防止数据到达后覆盖用户已做的勾选
  // （accounts 弹窗同款守卫；失败即降级为可交互）。
  const groupsEchoLoading = !!groupsTarget && userGroupsEcho.data === undefined && userGroupsEcho.isFetching

  // 回显成功 → 预填充勾选态 + 专属倍率初值（null/缺省 = 未设置）。
  useEffect(() => {
    if (!groupsTarget || !userGroupsEcho.data) return
    setGroupsChecked(userGroupsEcho.data.group_ids ?? [])
    const m: Record<number, AssignRowMult> = {}
    for (const [k, v] of Object.entries(userGroupsEcho.data.multipliers ?? {})) {
      const id = Number(k)
      m[id] = v == null ? { mult: '', cleared: true } : { mult: String(v), cleared: false }
    }
    setGroupsMult(m)
  }, [groupsTarget, userGroupsEcho.data])

  // 回显失败 → 空预填充不阻塞 + 一次性提示。
  useEffect(() => {
    if (groupsTarget && userGroupsEcho.isError && !groupsReadFailed) {
      setGroupsReadFailed(true)
      toast.add({ title: t('users.groups.readFailed'), type: 'error' })
    }
  }, [groupsTarget, userGroupsEcho.isError, groupsReadFailed, t])

  const openGroups = (u: User) => {
    setGroupsTarget(u)
    setGroupsChecked([])
    setGroupsMult({})
    setGroupsReadFailed(false)
  }
  const toggleGroup = (id: number, on: boolean) =>
    setGroupsChecked(s => (on ? (s.includes(id) ? s : [...s, id]) : s.filter(x => x !== id)))
  const setGroupMult = (id: number, mult: string) => {
    // 填值即操作该组：private 未勾选时自动授予（专属倍率必然伴随组成员身份）。
    if (!publicGroupIds.has(id) && !groupsChecked.includes(id)) toggleGroup(id, true)
    setGroupsMult(m => ({ ...m, [id]: { mult, cleared: false } }))
  }
  const clearGroupMult = (id: number) => {
    if (!publicGroupIds.has(id) && !groupsChecked.includes(id)) toggleGroup(id, true)
    setGroupsMult(m => ({ ...m, [id]: { mult: '', cleared: true } }))
  }

  // multipliers 三态：勾选且填值 → 数字；勾选留空（未清除）→ 省略键（沿用当前值）；
  // 勾选且显式清除 → null（回退组倍率）。未列出组沿用当前值 ✓（契约 PUT 语义）。
  const saveUserGroups = useMutation({
    mutationFn: () => {
      // 有效组 = private 授予（勾选全量）∪ public 组中做了倍率操作的（专属倍率
      // 也存于授予表，需进 group_ids；无倍率操作的 public 组不进——天然属于所有用户）。
      const effectiveIds = groupsChecked.filter(id => !publicGroupIds.has(id))
      const muls: Record<string, number | null> = {}
      for (const pid of publicGroupIds) {
        const row = groupsMult[pid]
        if (row && (row.mult.trim() !== '' || row.cleared)) effectiveIds.push(pid)
      }
      for (const gid of effectiveIds) {
        const row = groupsMult[gid]
        const v = row?.mult.trim()
        if (v !== undefined && v !== '') {
          const n = Number(v)
          if (!Number.isFinite(n) || n < 0 || n > 10) throw new Error(t('groups.multiplierInvalid'))
          muls[String(gid)] = n
        } else if (row?.cleared) {
          muls[String(gid)] = null
        }
      }
      const body: UserGroupsBody = { group_ids: effectiveIds }
      if (Object.keys(muls).length > 0) body.multipliers = muls
      return api.setUserGroups(groupsTarget!.ID!, body)
    },
    onSuccess: (resp) => {
      qc.invalidateQueries({ queryKey: ['groups'] })
      qc.invalidateQueries({ queryKey: ['user-groups'] })
      // 空勾选提交 = 清空（契约语义）：toast 用清空文案，避免「已授予 0 组」歧义
      toast.add({
        title: t('users.groups.saved'),
        description: resp.group_ids.length > 0 ? t('users.groups.savedDesc', { count: resp.group_ids.length }) : t('users.groups.clearedDesc'),
        type: 'success',
      })
      setGroupsTarget(null)
    },
  })
  // 清空全部（确认后 group_ids=[] 直接提交）。
  const clearUserGroups = useMutation({
    mutationFn: () => api.setUserGroups(groupsTarget!.ID!, { group_ids: [] }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['groups'] })
      qc.invalidateQueries({ queryKey: ['user-groups'] })
      toast.add({ title: t('users.groups.saved'), description: t('users.groups.clearedDesc'), type: 'success' })
      setGroupsTarget(null)
      setClearGroupsOpen(false)
    },
  })

  const submit = () => {
    if (!form.email.trim() || (!editing && !form.password)) return
    save.mutate(form)
  }

  const errMsg = (e: unknown) => (e instanceof ApiUnauthorized ? null : (e as Error)?.message)

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-semibold tracking-tight">{t('users.title')}</h1>
          <p className="text-sm text-muted-foreground">{t('users.subtitle')}</p>
        </div>
        <Button onClick={openCreate}><Plus /> {t('users.new')}</Button>
      </div>

      <ListToolbar
        name={email}
        onNameChange={changeEmail}
        placeholder={t('users.searchEmail')}
      />

      {isError ? (
        <p className="text-sm text-destructive">{t('common.loadFailed', { message: (error as Error).message })}</p>
      ) : isLoading ? (
        <div className="space-y-2">
          {Array.from({ length: 4 }).map((_, i) => <Skeleton key={i} className="h-12" />)}
        </div>
      ) : rows.length === 0 ? (
        <motion.div initial={{ opacity: 0, y: 12 }} animate={{ opacity: 1, y: 0 }} transition={{ duration: 0.25 }}>
          <Card className="flex flex-col items-center gap-2 py-12 text-muted-foreground">
            <UserCog className="size-10" />
            <p className="font-medium">{hasFilters ? t('users.filterEmpty') : t('users.emptyTitle')}</p>
            {!hasFilters && <p className="text-sm">{t('users.emptyDesc')}</p>}
            {hasFilters ? (
              <Button className="mt-2" variant="outline" onClick={clearFilters}><Filter /> {t('list.reset')}</Button>
            ) : (
              <Button className="mt-2" onClick={openCreate}><Plus /> {t('users.new')}</Button>
            )}
          </Card>
        </motion.div>
      ) : (
        <>
          <div className="overflow-hidden rounded-lg border bg-card">
            <Table>
              <TableHeader>
                <TableRow>
                  <SortableHeader field="id" label="ID" active={activeSort === 'id'} order={order} onToggle={onColumnToggle} />
                  <SortableHeader field="email" label={t('users.table.email')} active={activeSort === 'email'} order={order} onToggle={onColumnToggle} />
                  <SortableHeader field="role" label={t('users.table.role')} active={activeSort === 'role'} order={order} onToggle={onColumnToggle} />
                  <SortableHeader field="status" label={t('users.table.status')} active={activeSort === 'status'} order={order} onToggle={onColumnToggle} />
                  <SortableHeader field="max_concurrency" label={t('users.table.maxConcurrency')} active={activeSort === 'max_concurrency'} order={order} onToggle={onColumnToggle} className="text-right [&_button]:justify-end" />
                  <TableHead className="text-right">{t('users.table.balance')}</TableHead>
                  <SortableHeader field="created_at" label={t('users.table.createdAt')} active={activeSort === 'created_at'} order={order} onToggle={onColumnToggle} />
                  <TableHead className="text-right">{t('users.table.actions')}</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody className="[&_td]:py-3">
                {rows.map(u => (
                  <TableRow key={u.ID}>
                    <TableCell className="tabular-nums">{u.ID}</TableCell>
                    <TableCell className="max-w-52 truncate" title={u.Email}>{u.Email}</TableCell>
                    <TableCell><RoleBadge role={u.Role} /></TableCell>
                    <TableCell><StatusBadge status={u.Status} /></TableCell>
                    <TableCell className="text-right tabular-nums">
                      {u.MaxConcurrency == null ? '—' : u.MaxConcurrency === 0 ? t('user.overview.unlimited') : u.MaxConcurrency}
                    </TableCell>
                    <TableCell className="text-right tabular-nums">{formatBalance(u.Balance)}</TableCell>
                    <TableCell className="text-xs text-muted-foreground whitespace-nowrap">{formatDateTime(u.CreatedAt)}</TableCell>
                    <TableCell className="text-right">
                      <div className="flex justify-end gap-1">
                        <Button variant="ghost" size="icon-sm" title={t('users.tempBalances.button')} onClick={() => setTempUser(u)}><Coins /></Button>
                        <Button variant="ghost" size="icon-sm" title={t('users.groups.button')} onClick={() => openGroups(u)}><UsersRound /></Button>
                        <Button
                          variant="ghost"
                          size="icon-sm"
                          title={u.Status === 'disabled' ? t('users.enable') : t('users.disable')}
                          onClick={() => toggleStatus.mutate(u)}
                          disabled={toggleStatus.isPending}
                        >
                          {u.Status === 'disabled' ? <CircleCheck /> : <Ban />}
                        </Button>
                        <Button variant="ghost" size="icon-sm" title={t('common.edit')} onClick={() => openEdit(u)}><Pencil /></Button>
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

      {/* —— 创建/编辑对话框 —— */}
      <Dialog open={dialogOpen} onOpenChange={setDialogOpen}>
        <DialogContent className="sm:max-w-md">
          <DialogHeader>
            <DialogTitle>{editing ? t('users.editTitle', { id: editing.ID }) : t('users.newTitle')}</DialogTitle>
            <DialogDescription>{t('users.dialogDesc')}</DialogDescription>
          </DialogHeader>
          <div className="space-y-3">
            <div className="space-y-1.5">
              <Label htmlFor="usr-email">{t('users.emailLabel')}</Label>
              <Input
                id="usr-email"
                type="email"
                value={form.email}
                placeholder={t('users.emailPlaceholder')}
                onChange={e => setForm(f => ({ ...f, email: e.target.value }))}
                disabled={!!editing}
              />
              {editing && <p className="text-xs text-muted-foreground">{t('users.emailImmutable')}</p>}
            </div>
            {!editing && (
              <div className="space-y-1.5">
                <Label htmlFor="usr-password">{t('users.passwordLabel')}</Label>
                <Input
                  id="usr-password"
                  type="password"
                  value={form.password}
                  onChange={e => setForm(f => ({ ...f, password: e.target.value }))}
                />
                <p className="text-xs text-muted-foreground">{t('users.passwordHint')}</p>
              </div>
            )}
            <div className="grid grid-cols-2 gap-3">
              <div className="space-y-1.5">
                <Label>{t('users.roleLabel')}</Label>
                <Select value={form.role} items={Object.fromEntries(ROLES.map(r => [r, t(`users.role.${r}`)]))} onValueChange={v => setForm(f => ({ ...f, role: v as UserRole }))}>
                  <SelectTrigger className="w-full"><SelectValue /></SelectTrigger>
                  <SelectContent>
                    {ROLES.map(r => <SelectItem key={r} value={r} label={t(`users.role.${r}`)}>{t(`users.role.${r}`)}</SelectItem>)}
                  </SelectContent>
                </Select>
              </div>
              <div className="space-y-1.5">
                <Label>{t('users.statusLabel')}</Label>
                <Select value={form.status} items={Object.fromEntries(STATUSES.map(s => [s, t(`status.${s}`)]))} onValueChange={v => setForm(f => ({ ...f, status: v as UserStatus }))}>
                  <SelectTrigger className="w-full"><SelectValue /></SelectTrigger>
                  <SelectContent>
                    {STATUSES.map(s => <SelectItem key={s} value={s} label={t(`status.${s}`)}>{t(`status.${s}`)}</SelectItem>)}
                  </SelectContent>
                </Select>
              </div>
            </div>
            <div className="grid grid-cols-2 gap-3">
              <div className="space-y-1.5">
                <Label htmlFor="usr-max">{t('users.maxLabel')}</Label>
                <Input id="usr-max" type="number" min={0} value={form.max_concurrency} onChange={e => setForm(f => ({ ...f, max_concurrency: e.target.value }))} />
                <p className="text-xs text-muted-foreground">{t('users.maxHint')}</p>
              </div>
              <div className="space-y-1.5">
                <Label htmlFor="usr-balance">{t('users.balanceLabel')}</Label>
                <Input id="usr-balance" type="number" min={0} step={0.01} value={form.balance} onChange={e => setForm(f => ({ ...f, balance: e.target.value }))} />
              </div>
            </div>
            {save.isError && errMsg(save.error) && (
              <p className="text-sm text-destructive">{errMsg(save.error)}</p>
            )}
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setDialogOpen(false)} disabled={save.isPending}>{t('common.cancel')}</Button>
            <Button onClick={submit} disabled={save.isPending || !form.email.trim() || (!editing && !form.password)}>
              {save.isPending ? t('common.saving') : editing ? t('common.saveChanges') : t('common.create')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* —— 临时额度查看：有效合计 + 全量列表（管理视角含过期/用尽） —— */}
      <Dialog open={!!tempUser} onOpenChange={o => { if (!o) { setTempUser(null) } }}>
        <DialogContent className="sm:max-w-lg">
          <DialogHeader>
            <DialogTitle>{t('users.tempBalances.title', { name: tempUser?.Email })}</DialogTitle>
            <DialogDescription>{t('users.tempBalances.desc')}</DialogDescription>
            {/* 空态不渲染合计（评审 M-1——对齐 profile 参考形态） */}
            {tempRows.length > 0 && (
              <p className="text-sm font-medium">{t('users.tempBalances.total', { amount: formatUSD(tempActiveTotal) })}</p>
            )}
          </DialogHeader>
          {tempBalancesQ.isLoading ? (
            <div className="space-y-1.5">
              {Array.from({ length: 3 }).map((_, i) => <Skeleton key={i} className="h-9" />)}
            </div>
          ) : tempBalancesQ.isError ? (
            <p className="text-sm text-destructive">{t('common.loadFailed', { message: (tempBalancesQ.error as Error).message })}</p>
          ) : tempRows.length === 0 ? (
            <p className="py-6 text-center text-sm text-muted-foreground">{t('users.tempBalances.empty')}</p>
          ) : (
            <ScrollArea className="max-h-72 rounded-md border">
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead className="text-right">{t('users.tempBalances.amount')}</TableHead>
                    <TableHead>{t('users.tempBalances.expiresAt')}</TableHead>
                    <TableHead>{t('users.tempBalances.note')}</TableHead>
                    <TableHead>{t('users.tempBalances.createdAt')}</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {tempRows.map(r => (
                    <TableRow key={r.id}>
                      <TableCell className="text-right tabular-nums">{formatUSD(r.amount_usd)}</TableCell>
                      <TableCell className="whitespace-nowrap text-xs text-muted-foreground">
                        {r.expires_at == null ? t('user.profile.tempPermanent') : formatDateTime(r.expires_at)}
                      </TableCell>
                      <TableCell className="max-w-40 truncate text-xs text-muted-foreground" title={r.note ?? undefined}>{r.note ?? '—'}</TableCell>
                      <TableCell className="whitespace-nowrap text-xs text-muted-foreground">{formatDateTime(r.created_at)}</TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            </ScrollArea>
          )}
        </DialogContent>
      </Dialog>

      {/* —— 分组管理：勾选 = 授予，未勾选 = 撤销（完整列表替换）+ 专属倍率三态 —— */}
      <Dialog open={!!groupsTarget} onOpenChange={o => { if (!o && !saveUserGroups.isPending && !clearUserGroups.isPending) { setGroupsTarget(null); setClearGroupsOpen(false) } }}>
        <DialogContent className="sm:max-w-lg">
          <DialogHeader>
            <DialogTitle>{t('users.groups.title', { name: groupsTarget?.Email })}</DialogTitle>
            <DialogDescription>{t('users.groups.desc')}</DialogDescription>
            <p className="text-sm font-medium">{t('users.groups.count', { count: grantedCount })}</p>
            {groupsEchoLoading && <p className="text-xs text-muted-foreground">{t('users.groups.echoLoading')}</p>}
            <p className="text-xs text-muted-foreground">{t('users.groups.multiplierHint')}</p>
          </DialogHeader>
          <div className="space-y-3">
            {groupsAll.isLoading ? (
              <div className="space-y-1.5">
                {Array.from({ length: 4 }).map((_, i) => <Skeleton key={i} className="h-9" />)}
              </div>
            ) : groupsAll.isError ? (
              <p className="text-sm text-destructive">{t('common.loadFailed', { message: (groupsAll.error as Error).message })}</p>
            ) : allGroups.length === 0 ? (
              <p className="py-6 text-center text-sm text-muted-foreground">{t('users.groups.empty')}</p>
            ) : (
              <ScrollArea className={cn('max-h-72 pr-1', groupsEchoLoading && 'pointer-events-none opacity-50')}>
                <div className="space-y-1.5">
                {allGroups.map(g => {
                  const isPublic = g.Visibility === 'public'
                  const checked = groupsChecked.includes(g.ID!)
                  const row = groupsMult[g.ID!]
                  return (
                    <div key={g.ID} className="flex items-center gap-2.5 rounded-md border px-2 py-1.5">
                      <Checkbox
                        checked={checked || isPublic}
                        disabled={isPublic}
                        onCheckedChange={c => toggleGroup(g.ID!, c === true)}
                        aria-label={g.Name}
                        className={isPublic ? 'data-checked:border-muted-foreground/40 data-checked:bg-muted data-checked:text-muted-foreground' : undefined}
                      />
                      <span className="min-w-0 flex-1 truncate text-sm" title={g.Name}>{g.Name}</span>
                      <GroupVisibilityDot visibility={g.Visibility} />
                      <span className="w-14 shrink-0 text-right text-xs tabular-nums text-muted-foreground" title={t('groups.table.priceMultiplier')}>
                        {formatMultiplier(g.PriceMultiplier, t)}
                      </span>
                      <Input
                        type="number"
                        min={0}
                        max={10}
                        step={0.1}
                        value={row?.mult ?? ''}
                        placeholder={t('groups.assignMultiplierPlaceholder')}
                        onChange={e => setGroupMult(g.ID!, e.target.value)}
                        className="h-7 w-24 text-xs"
                      />
                      <Button
                        variant="ghost"
                        size="icon-sm"
                        title={t('groups.assignMultiplierClear')}
                        disabled={!row?.mult && !row?.cleared}
                        onClick={() => clearGroupMult(g.ID!)}
                      >
                        <X />
                      </Button>
                      {row?.cleared && (
                        <span className="w-24 text-xs text-muted-foreground">{t('groups.assignMultiplierUnset')}</span>
                      )}
                    </div>
                  )
                })}
                </div>
              </ScrollArea>
            )}
            {saveUserGroups.isError && errMsg(saveUserGroups.error) && (
              <p className="text-sm text-destructive">{errMsg(saveUserGroups.error)}</p>
            )}
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setClearGroupsOpen(true)} disabled={saveUserGroups.isPending || groupsEchoLoading}>
              {t('users.groups.clearAll')}
            </Button>
            <Button variant="outline" onClick={() => setGroupsTarget(null)} disabled={saveUserGroups.isPending}>{t('common.cancel')}</Button>
            <Button onClick={() => saveUserGroups.mutate()} disabled={saveUserGroups.isPending || groupsEchoLoading}>
              {saveUserGroups.isPending ? t('common.saving') : t('common.save')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* —— 清空全部组授予（确认后直接提交 group_ids: []） —— */}
      <Dialog open={clearGroupsOpen} onOpenChange={o => { if (!o && !clearUserGroups.isPending) setClearGroupsOpen(false) }}>
        <DialogContent className="sm:max-w-sm">
          <DialogHeader>
            <DialogTitle>{t('users.groups.clearAll')}</DialogTitle>
            <DialogDescription>{t('users.groups.clearAllDesc', { name: groupsTarget?.Email })}</DialogDescription>
          </DialogHeader>
          {clearUserGroups.isError && errMsg(clearUserGroups.error) && (
            <p className="text-sm text-destructive">{errMsg(clearUserGroups.error)}</p>
          )}
          <DialogFooter>
            <Button variant="outline" onClick={() => setClearGroupsOpen(false)} disabled={clearUserGroups.isPending}>{t('common.cancel')}</Button>
            <Button variant="destructive" onClick={() => clearUserGroups.mutate()} disabled={clearUserGroups.isPending}>
              {clearUserGroups.isPending ? t('common.saving') : t('users.groups.clearAllConfirm')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
}
