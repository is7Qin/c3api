// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

import { useEffect, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { motion } from 'framer-motion'
import { Check, Copy, KeyRound, Pencil, Plus, RefreshCcw, Trash2 } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { ApiUnauthorized, userApi } from '@/lib/api/client'
import { KeyBox, copyText } from '@/components/key-box'
import { Pagination } from '@/components/pagination'
import { StatusBadge } from '@/components/status-badge'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { Skeleton } from '@/components/ui/skeleton'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { formatDateTime } from '@/components/fmt'
import type { components } from '@/lib/api/schema'

type Key = components['schemas']['Key']
type KeyCreate = components['schemas']['KeyCreate']
type KeyUpdate = components['schemas']['KeyUpdate']
type KeyStatus = components['schemas']['KeyStatus']

const STATUSES: KeyStatus[] = ['active', 'disabled']

// 创建表单：name/group_id 必填；数值字段 '' = 不发送（后端缺省），0 = 不限。
interface CreateForm {
  name: string
  group_id: string // Select value 为字符串，提交时转 number
  max_concurrency: string
  quota: string
}

const emptyCreateForm = (): CreateForm => ({ name: '', group_id: '', max_concurrency: '', quota: '' })

// 编辑表单（KeyUpdate 全可选）：name/status 必有值总是发送（读改写幂等）；
// 数值字段 '' = 不发送（后端视为未提供，保持不变）。
interface EditForm {
  name: string
  status: KeyStatus
  max_concurrency: string
  quota: string
}

function toEditForm(k: Key): EditForm {
  return {
    name: k.Name ?? '',
    status: k.Status ?? 'active',
    max_concurrency: k.MaxConcurrency == null ? '' : String(k.MaxConcurrency),
    quota: k.Quota == null ? '' : String(k.Quota),
  }
}

// 数值字段校验：'' 或非负整数（0 = 不限）。
function validNumber(s: string): boolean {
  return s === '' || (Number.isInteger(Number(s)) && Number(s) >= 0)
}

// 列表行明文展示：短展示头 8 尾 4 省略中间（title 悬停全文）+ 行内复制按钮
// （明文长期可复制——需求核心；复用 KeyBox 同款 copyText）。
function KeyCell({ raw }: { raw?: string }) {
  const { t } = useTranslation()
  const [copied, setCopied] = useState(false)
  return (
    <div className="flex items-center gap-1.5">
      <code className="font-mono text-sm" title={raw}>{raw ? `${raw.slice(0, 8)}…${raw.slice(-4)}` : '—'}</code>
      {raw && (
        <Button
          variant="ghost"
          size="icon-sm"
          title={t('keybox.copy')}
          onClick={async () => {
            if (await copyText(raw)) {
              setCopied(true)
              setTimeout(() => setCopied(false), 2000)
            }
          }}
        >
          {copied ? <Check /> : <Copy />}
        </Button>
      )}
    </div>
  )
}

export default function UserKeys() {
  const { t } = useTranslation()
  const qc = useQueryClient()

  // —— 列表：limit/offset 分页（mutation 后 invalidate 刷新，保持当前页） ——
  const [offset, setOffset] = useState(0)
  const [limit, setLimit] = useState(20)

  const { data, isLoading, isError, error } = useQuery({
    queryKey: ['user', 'keys', { limit, offset }],
    queryFn: () => userApi.listUserKeys({ limit, offset }),
  })
  const rows = data?.rows ?? []

  // 每页条数变化 → 重置 offset。
  const changeLimit = (l: number) => { setLimit(l); setOffset(0) }
  // 末页死胡同守卫：当前页数据被清空（如删除末页最后一条）时回退到第 1 页，
  // 避免空态页无返回入口。页 1 本身为空（列表真正为空）时无需回退，不会成环。
  useEffect(() => {
    if (!isLoading && !isError && rows.length === 0 && offset > 0) setOffset(0)
  }, [isLoading, isError, rows.length, offset])

  // —— 可选分组（public 全部 + 已授予 private；key 创建时选组） ——
  const { data: groups } = useQuery({
    queryKey: ['user', 'groups'],
    queryFn: () => userApi.listUserGroups(),
    staleTime: 60_000,
  })
  const selectableGroups = (groups ?? []).filter(g => g.ID != null)
  // Select 必须用 items prop（Record<string, ReactNode>），否则 trigger 显示原始 value。
  const groupItems = Object.fromEntries(selectableGroups.map(g => [String(g.ID!), g.Name ?? String(g.ID!)]))

  // —— 创建（form → result 两阶段：成功后 KeyBox 展示明文，仅此一次） ——
  const [createOpen, setCreateOpen] = useState(false)
  const [createForm, setCreateForm] = useState<CreateForm>(emptyCreateForm())
  const [createErr, setCreateErr] = useState<string | null>(null)
  const [created, setCreated] = useState<Key | null>(null)

  const create = useMutation({
    mutationFn: (f: CreateForm) => {
      const body: KeyCreate = { name: f.name.trim(), group_id: Number(f.group_id) }
      if (f.max_concurrency !== '') body.max_concurrency = Number(f.max_concurrency)
      if (f.quota !== '') body.quota = Number(f.quota)
      return userApi.createUserKey(body)
    },
    onSuccess: res => {
      qc.invalidateQueries({ queryKey: ['user', 'keys'] })
      setCreated(res)
    },
  })
  const openCreate = () => {
    setCreateForm(emptyCreateForm())
    setCreateErr(null)
    setCreated(null)
    create.reset()
    setCreateOpen(true)
  }
  const updateCreate = (patch: Partial<CreateForm>) => {
    setCreateForm(f => ({ ...f, ...patch }))
    setCreateErr(null)
  }
  const submitCreate = () => {
    const f = createForm
    if (!f.name.trim() || !f.group_id || !validNumber(f.max_concurrency) || !validNumber(f.quota)) {
      setCreateErr(t('user.keys.formInvalid'))
      return
    }
    create.mutate(f)
  }

  // —— 编辑（name/status/max_concurrency/quota） ——
  const [editOpen, setEditOpen] = useState(false)
  const [editing, setEditing] = useState<Key | null>(null)
  const [editForm, setEditForm] = useState<EditForm>(toEditForm({}))
  const [editErr, setEditErr] = useState<string | null>(null)

  const update = useMutation({
    mutationFn: (p: { id: number; body: KeyUpdate }) => userApi.updateUserKey(p.id, p.body),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['user', 'keys'] })
      setEditOpen(false)
    },
  })
  const openEdit = (k: Key) => {
    setEditing(k)
    setEditForm(toEditForm(k))
    setEditErr(null)
    update.reset()
    setEditOpen(true)
  }
  const submitEdit = () => {
    if (!editForm.name.trim() || !validNumber(editForm.max_concurrency) || !validNumber(editForm.quota)) {
      setEditErr(t('user.keys.formInvalid'))
      return
    }
    const body: KeyUpdate = { name: editForm.name.trim(), status: editForm.status }
    if (editForm.max_concurrency !== '') body.max_concurrency = Number(editForm.max_concurrency)
    if (editForm.quota !== '') body.quota = Number(editForm.quota)
    update.mutate({ id: editing!.ID!, body })
  }

  // —— 删除（确认弹窗） ——
  const [deleting, setDeleting] = useState<Key | null>(null)
  const del = useMutation({
    mutationFn: (id: number) => userApi.deleteUserKey(id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['user', 'keys'] })
      setDeleting(null)
    },
  })
  const openDelete = (k: Key) => {
    del.reset()
    setDeleting(k)
  }

  // —— 轮换（confirm → result 两阶段：成功后 KeyBox 展示新明文） ——
  const [rotating, setRotating] = useState<Key | null>(null)
  const [rotated, setRotated] = useState<Key | null>(null)
  const rotate = useMutation({
    mutationFn: (id: number) => userApi.rotateUserKey(id),
    onSuccess: res => {
      qc.invalidateQueries({ queryKey: ['user', 'keys'] })
      setRotated(res)
    },
  })
  const openRotate = (k: Key) => {
    setRotating(k)
    setRotated(null)
    rotate.reset()
  }
  const closeRotate = () => {
    if (!rotate.isPending) {
      setRotating(null)
      setRotated(null)
    }
  }

  const errMsg = (e: unknown) => (e instanceof ApiUnauthorized ? null : (e as Error)?.message)

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-semibold tracking-tight">{t('user.keys.title')}</h1>
          <p className="text-sm text-muted-foreground">{t('user.keys.subtitle')}</p>
        </div>
        <Button onClick={openCreate}><Plus /> {t('user.keys.new')}</Button>
      </div>

      {isError ? (
        <p className="text-sm text-destructive">{t('common.loadFailed', { message: (error as Error).message })}</p>
      ) : isLoading ? (
        <div className="space-y-2">
          {Array.from({ length: 4 }).map((_, i) => <Skeleton key={i} className="h-12" />)}
        </div>
      ) : rows.length === 0 ? (
        <motion.div initial={{ opacity: 0, y: 12 }} animate={{ opacity: 1, y: 0 }} transition={{ duration: 0.25 }}>
          <Card className="flex flex-col items-center gap-2 py-12 text-muted-foreground">
            <KeyRound className="size-10" />
            <p className="font-medium">{t('user.keys.emptyTitle')}</p>
            <p className="text-sm">{t('user.keys.emptyDesc')}</p>
            <Button className="mt-2" onClick={openCreate}><Plus /> {t('user.keys.new')}</Button>
          </Card>
        </motion.div>
      ) : (
        <>
          <div className="overflow-hidden rounded-lg border bg-card">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>ID</TableHead>
                  <TableHead>{t('user.keys.table.name')}</TableHead>
                  <TableHead>{t('user.keys.table.keyPrefix')}</TableHead>
                  <TableHead>{t('user.keys.table.status')}</TableHead>
                  <TableHead>{t('user.keys.table.groupId')}</TableHead>
                  <TableHead className="text-right">{t('user.keys.table.maxConcurrency')}</TableHead>
                  <TableHead className="text-right">{t('user.keys.table.quota')}</TableHead>
                  <TableHead>{t('user.keys.table.createdAt')}</TableHead>
                  <TableHead className="text-right">{t('user.keys.table.actions')}</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody className="[&_td]:py-3">
                {rows.map(k => (
                  <TableRow key={k.ID}>
                    <TableCell className="tabular-nums">{k.ID}</TableCell>
                    <TableCell className="max-w-40 truncate" title={k.Name}>{k.Name ?? '—'}</TableCell>
                    <TableCell><KeyCell raw={k.key} /></TableCell>
                    <TableCell><StatusBadge status={k.Status} /></TableCell>
                    <TableCell className="tabular-nums">{k.GroupID ?? '—'}</TableCell>
                    <TableCell className="text-right tabular-nums">
                      {k.MaxConcurrency == null ? '—' : k.MaxConcurrency === 0 ? t('user.overview.unlimited') : k.MaxConcurrency}
                    </TableCell>
                    <TableCell className="text-right tabular-nums">
                      {k.Quota ? `${k.QuotaUsed ?? 0} / ${k.Quota}` : t('user.keys.unlimited')}
                    </TableCell>
                    <TableCell className="text-xs text-muted-foreground whitespace-nowrap">{formatDateTime(k.CreatedAt)}</TableCell>
                    <TableCell className="text-right">
                      <div className="flex justify-end gap-1">
                        <Button variant="ghost" size="icon-sm" title={t('common.edit')} onClick={() => openEdit(k)}><Pencil /></Button>
                        <Button variant="ghost" size="icon-sm" title={t('user.keys.rotate')} onClick={() => openRotate(k)} disabled={rotate.isPending}><RefreshCcw /></Button>
                        <Button variant="ghost" size="icon-sm" className="text-destructive" title={t('common.delete')} onClick={() => openDelete(k)} disabled={del.isPending}><Trash2 /></Button>
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

      {/* —— 创建对话框（form → result 两阶段） —— */}
      <Dialog open={createOpen} onOpenChange={setCreateOpen}>
        <DialogContent className="sm:max-w-md">
          <DialogHeader>
            <DialogTitle>{created ? t('user.keys.createdTitle') : t('user.keys.createTitle')}</DialogTitle>
            <DialogDescription>{created ? t('user.keys.createdDesc') : t('user.keys.createDesc')}</DialogDescription>
          </DialogHeader>
          {created ? (
            <>
              <KeyBox title={t('user.keys.secretTitle')} value={created.key} hint={t('user.keys.secretHint')} />
              <DialogFooter>
                <Button onClick={() => setCreateOpen(false)}>{t('common.done')}</Button>
              </DialogFooter>
            </>
          ) : (
            <>
              <div className="space-y-3">
                <div className="space-y-1.5">
                  <Label htmlFor="uk-name">{t('user.keys.nameLabel')}</Label>
                  <Input id="uk-name" value={createForm.name} placeholder={t('user.keys.namePlaceholder')} onChange={e => updateCreate({ name: e.target.value })} />
                </div>
                <div className="space-y-1.5">
                  <Label>{t('user.keys.groupLabel')}</Label>
                  <Select items={groupItems} value={createForm.group_id} onValueChange={v => updateCreate({ group_id: v })}>
                    <SelectTrigger className="w-full"><SelectValue placeholder={t('user.keys.groupPlaceholder')} /></SelectTrigger>
                    <SelectContent>
                      {selectableGroups.map(g => (
                        <SelectItem key={g.ID} value={String(g.ID)} label={g.Name ?? String(g.ID)}>{g.Name ?? String(g.ID)}</SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                  {selectableGroups.length === 0 && <p className="text-xs text-muted-foreground">{t('user.keys.groupEmpty')}</p>}
                </div>
                <div className="grid grid-cols-2 gap-3">
                  <div className="space-y-1.5">
                    <Label htmlFor="uk-max">{t('user.keys.maxLabel')}</Label>
                    <Input id="uk-max" type="number" min={0} value={createForm.max_concurrency} onChange={e => updateCreate({ max_concurrency: e.target.value })} />
                    <p className="text-xs text-muted-foreground">{t('user.keys.maxHint')}</p>
                  </div>
                  <div className="space-y-1.5">
                    <Label htmlFor="uk-quota">{t('user.keys.quotaLabel')}</Label>
                    <Input id="uk-quota" type="number" min={0} value={createForm.quota} onChange={e => updateCreate({ quota: e.target.value })} />
                    <p className="text-xs text-muted-foreground">{t('user.keys.quotaHint')}</p>
                  </div>
                </div>
                {createErr && <p className="text-sm text-destructive">{createErr}</p>}
                {create.isError && errMsg(create.error) && <p className="text-sm text-destructive">{errMsg(create.error)}</p>}
              </div>
              <DialogFooter>
                <Button variant="outline" onClick={() => setCreateOpen(false)} disabled={create.isPending}>{t('common.cancel')}</Button>
                <Button onClick={submitCreate} disabled={create.isPending}>
                  {create.isPending ? t('common.creating') : t('user.keys.new')}
                </Button>
              </DialogFooter>
            </>
          )}
        </DialogContent>
      </Dialog>

      {/* —— 编辑对话框 —— */}
      <Dialog open={editOpen} onOpenChange={setEditOpen}>
        <DialogContent className="sm:max-w-md">
          <DialogHeader>
            <DialogTitle>{t('user.keys.editTitle', { id: editing?.ID })}</DialogTitle>
            <DialogDescription>{t('user.keys.editDesc')}</DialogDescription>
          </DialogHeader>
          <div className="space-y-3">
            <div className="space-y-1.5">
              <Label htmlFor="uk-ename">{t('user.keys.nameLabel')}</Label>
              <Input id="uk-ename" value={editForm.name} onChange={e => { setEditForm(f => ({ ...f, name: e.target.value })); setEditErr(null) }} />
            </div>
            <div className="space-y-1.5">
              <Label>{t('user.keys.statusLabel')}</Label>
              <Select items={Object.fromEntries(STATUSES.map(s => [s, t(`status.${s}`)]))} value={editForm.status} onValueChange={v => setEditForm(f => ({ ...f, status: v as KeyStatus }))}>
                <SelectTrigger className="w-full"><SelectValue /></SelectTrigger>
                <SelectContent>
                  {STATUSES.map(s => <SelectItem key={s} value={s} label={t(`status.${s}`)}>{t(`status.${s}`)}</SelectItem>)}
                </SelectContent>
              </Select>
            </div>
            <div className="grid grid-cols-2 gap-3">
              <div className="space-y-1.5">
                <Label htmlFor="uk-emax">{t('user.keys.maxLabel')}</Label>
                <Input id="uk-emax" type="number" min={0} value={editForm.max_concurrency} onChange={e => setEditForm(f => ({ ...f, max_concurrency: e.target.value }))} />
                <p className="text-xs text-muted-foreground">{t('user.keys.maxHint')}</p>
              </div>
              <div className="space-y-1.5">
                <Label htmlFor="uk-equota">{t('user.keys.quotaLabel')}</Label>
                <Input id="uk-equota" type="number" min={0} value={editForm.quota} onChange={e => setEditForm(f => ({ ...f, quota: e.target.value }))} />
                <p className="text-xs text-muted-foreground">{t('user.keys.quotaHint')}</p>
              </div>
            </div>
            {editErr && <p className="text-sm text-destructive">{editErr}</p>}
            {update.isError && errMsg(update.error) && <p className="text-sm text-destructive">{errMsg(update.error)}</p>}
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setEditOpen(false)} disabled={update.isPending}>{t('common.cancel')}</Button>
            <Button onClick={submitEdit} disabled={update.isPending || !editForm.name.trim()}>
              {update.isPending ? t('common.saving') : t('common.saveChanges')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* —— 删除确认 —— */}
      <Dialog open={!!deleting} onOpenChange={o => { if (!o && !del.isPending) setDeleting(null) }}>
        <DialogContent className="sm:max-w-sm">
          <DialogHeader>
            <DialogTitle>{t('user.keys.deleteTitle')}</DialogTitle>
            <DialogDescription>{t('user.keys.deleteDesc', { name: deleting?.Name })}</DialogDescription>
          </DialogHeader>
          {del.isError && errMsg(del.error) && <p className="text-sm text-destructive">{errMsg(del.error)}</p>}
          <DialogFooter>
            <Button variant="outline" onClick={() => setDeleting(null)} disabled={del.isPending}>{t('common.cancel')}</Button>
            <Button variant="destructive" onClick={() => deleting && del.mutate(deleting.ID!)} disabled={del.isPending}>
              {del.isPending ? t('common.deleting') : t('common.delete')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* —— 轮换（confirm → result 两阶段） —— */}
      <Dialog open={!!rotating} onOpenChange={o => { if (!o) closeRotate() }}>
        <DialogContent className="sm:max-w-md">
          {rotated ? (
            <>
              <DialogHeader>
                <DialogTitle>{t('user.keys.rotatedTitle')}</DialogTitle>
                <DialogDescription>{t('user.keys.rotatedDesc')}</DialogDescription>
              </DialogHeader>
              <KeyBox title={t('user.keys.secretTitle')} value={rotated.key} hint={t('user.keys.secretHint')} />
              <DialogFooter>
                <Button onClick={() => { setRotating(null); setRotated(null) }}>{t('common.done')}</Button>
              </DialogFooter>
            </>
          ) : (
            <>
              <DialogHeader>
                <DialogTitle>{t('user.keys.rotateTitle')}</DialogTitle>
                <DialogDescription>{t('user.keys.rotateDesc', { name: rotating?.Name })}</DialogDescription>
              </DialogHeader>
              {rotate.isError && errMsg(rotate.error) && <p className="text-sm text-destructive">{errMsg(rotate.error)}</p>}
              <DialogFooter>
                <Button variant="outline" onClick={closeRotate} disabled={rotate.isPending}>{t('common.cancel')}</Button>
                <Button onClick={() => rotating && rotate.mutate(rotating.ID!)} disabled={rotate.isPending}>
                  {rotate.isPending ? t('user.keys.rotating') : t('user.keys.rotateConfirm')}
                </Button>
              </DialogFooter>
            </>
          )}
        </DialogContent>
      </Dialog>
    </div>
  )
}
