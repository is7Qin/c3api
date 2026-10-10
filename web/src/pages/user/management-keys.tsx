// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 用户面「管理 API key」页（spec 2026-10-09 §4.1/§4.8）：管理 key 唯一页面，普通页面
// 直接以 owner 身份鉴权 /api/user/*。管理 key 以 owner 身份鉴权管理面，owner 可见明文
// key_raw（长期可查看/复制——自托管权衡）。KeyCell/状态选择/弹窗均为同文件私有实现。
import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { motion } from 'framer-motion'
import { Check, Copy, KeyRound, Pencil, Plus, Trash2 } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { ApiUnauthorized, userApi } from '@/lib/api/client'
import { copyText, KeyBox } from '@/components/key-box'
import { StatusBadge } from '@/components/status-badge'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { ScrollArea } from '@/components/ui/scroll-area'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { Skeleton } from '@/components/ui/skeleton'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { toast } from '@/components/ui/toast'
import { formatDateTime } from '@/components/fmt'
import type { components } from '@/lib/api/schema'

type ManagementKey = components['schemas']['ManagementKey']
type ManagementKeyStatus = components['schemas']['ManagementKeyStatus']

// 管理 key 生命周期状态（与 user/key 的 active/disabled 同词表，复用 status.* 文案）。
const STATUSES: ManagementKeyStatus[] = ['active', 'disabled']

// 列表行明文展示：短展示头 8 尾 4 省略中间（title 悬停全文）+ 行内复制按钮。
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

export default function UserManagementKeys() {
  const { t } = useTranslation()
  const qc = useQueryClient()
  // 唯一页面：查询键不含 surface 维度。
  const qk = ['management-keys', 'list'] as const

  const { data, isLoading, isError, error } = useQuery({
    queryKey: qk,
    queryFn: () => userApi.listManagementKeys(),
  })
  const rows = data?.rows ?? []

  // —— 创建（form → result 两阶段：成功后 KeyBox 展示明文） ——
  const [createOpen, setCreateOpen] = useState(false)
  const [createName, setCreateName] = useState('')
  const [createErr, setCreateErr] = useState<string | null>(null)
  const [created, setCreated] = useState<ManagementKey | null>(null)
  const create = useMutation({
    mutationFn: (name: string) => userApi.createManagementKey(name),
    onSuccess: res => {
      qc.invalidateQueries({ queryKey: qk })
      setCreated(res)
    },
  })
  const openCreate = () => {
    setCreateName('')
    setCreateErr(null)
    setCreated(null)
    create.reset()
    setCreateOpen(true)
  }
  const submitCreate = () => {
    if (!createName.trim()) {
      setCreateErr(t('managementKeys.formInvalid'))
      return
    }
    create.mutate(createName.trim())
  }

  // —— 编辑（name/status；name 必有值总是发送，status 必送——读改写幂等） ——
  const [editOpen, setEditOpen] = useState(false)
  const [editing, setEditing] = useState<ManagementKey | null>(null)
  const [editName, setEditName] = useState('')
  const [editStatus, setEditStatus] = useState<ManagementKeyStatus>('active')
  const [editErr, setEditErr] = useState<string | null>(null)
  const update = useMutation({
    mutationFn: (p: { id: number; name: string; status: ManagementKeyStatus }) =>
      userApi.updateManagementKey(p.id, { name: p.name, status: p.status }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: qk })
      setEditOpen(false)
    },
  })
  const openEdit = (k: ManagementKey) => {
    setEditing(k)
    setEditName(k.name ?? '')
    setEditStatus(k.status ?? 'active')
    setEditErr(null)
    update.reset()
    setEditOpen(true)
  }
  const submitEdit = () => {
    if (!editName.trim()) {
      setEditErr(t('managementKeys.formInvalid'))
      return
    }
    update.mutate({ id: editing!.id!, name: editName.trim(), status: editStatus })
  }

  // —— 删除（确认弹窗；软删 + 本实例快照即时移除） ——
  const [deleting, setDeleting] = useState<ManagementKey | null>(null)
  const del = useMutation({
    mutationFn: (id: number) => userApi.deleteManagementKey(id),
    onSuccess: () => {
      toast.add({ title: t('managementKeys.deletedToast'), type: 'success' })
      qc.invalidateQueries({ queryKey: qk })
      setDeleting(null)
    },
  })

  const errMsg = (e: unknown) => (e instanceof ApiUnauthorized ? null : (e as Error)?.message)

  const renderActions = (k: ManagementKey) => (
    <div className="flex justify-end gap-1">
      <Button variant="ghost" size="icon-sm" title={t('common.edit')} onClick={() => openEdit(k)}><Pencil /></Button>
      <Button variant="ghost" size="icon-sm" className="text-destructive" title={t('common.delete')} onClick={() => { del.reset(); setDeleting(k) }} disabled={del.isPending}><Trash2 /></Button>
    </div>
  )

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-semibold tracking-tight">{t('managementKeys.title')}</h1>
          <p className="text-sm text-muted-foreground">{t('managementKeys.subtitle')}</p>
        </div>
        <Button onClick={openCreate}><Plus /> {t('managementKeys.new')}</Button>
      </div>

      {isError ? (
        <p className="text-sm text-destructive">{t('common.loadFailed', { message: (error as Error).message })}</p>
      ) : isLoading ? (
        <div className="space-y-2">
          {Array.from({ length: 3 }).map((_, i) => <Skeleton key={i} className="h-12" />)}
        </div>
      ) : rows.length === 0 ? (
        <motion.div initial={{ opacity: 0, y: 12 }} animate={{ opacity: 1, y: 0 }} transition={{ duration: 0.25 }}>
          <Card className="flex flex-col items-center gap-2 py-12 text-muted-foreground">
            <KeyRound className="size-10" />
            <p className="font-medium">{t('managementKeys.emptyTitle')}</p>
            <p className="text-sm">{t('managementKeys.emptyDesc')}</p>
            <Button className="mt-2" onClick={openCreate}><Plus /> {t('managementKeys.new')}</Button>
          </Card>
        </motion.div>
      ) : (
        <>
          <ScrollArea data-od-id="table-scroll-management-keys" className="max-sm:hidden rounded-[14px] border border-[rgba(19,45,83,0.26)] bg-[color:var(--glass-card-light)] shadow-[inset_0_1px_0_rgba(255,255,255,0.5),0_10px_36px_rgba(19,45,83,0.16)] backdrop-blur-[var(--glass-blur)] dark:bg-[color:var(--glass-card-dark)] dark:shadow-[inset_0_1px_0_rgba(255,255,255,0.07),0_10px_36px_rgba(2,6,14,0.5)] dark:border-[rgba(148,180,220,0.32)]" showHorizontal>
            <Table className="min-w-[880px]" containerClassName="overflow-x-visible border-0 shadow-none rounded-none bg-transparent backdrop-blur-none">
              <TableHeader>
                <TableRow>
                  <TableHead>ID</TableHead>
                  <TableHead>{t('managementKeys.table.name')}</TableHead>
                  <TableHead>{t('managementKeys.table.keyPrefix')}</TableHead>
                  <TableHead>{t('managementKeys.table.status')}</TableHead>
                  <TableHead>{t('managementKeys.table.createdAt')}</TableHead>
                  <TableHead className="text-right">{t('managementKeys.table.actions')}</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody className="[&_td]:py-3">
                {rows.map(k => (
                  <TableRow key={k.id}>
                    <TableCell className="tabular-nums">{k.id}</TableCell>
                    <TableCell className="max-w-40 truncate" title={k.name}>{k.name ?? '—'}</TableCell>
                    <TableCell><KeyCell raw={k.key_raw} /></TableCell>
                    <TableCell><StatusBadge status={k.status} /></TableCell>
                    <TableCell className="text-xs text-muted-foreground whitespace-nowrap">{formatDateTime(k.created_at)}</TableCell>
                    <TableCell className="text-right">{renderActions(k)}</TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </ScrollArea>
          {/* 移动端（<sm）紧凑卡片行：名称/状态/Key/操作全可见，无横向滚动。 */}
          <div className="space-y-2 sm:hidden">
            {rows.map(k => (
              <Card key={k.id} size="sm" className="px-3">
                <div className="flex items-start justify-between gap-2">
                  <div className="min-w-0 space-y-1">
                    <p className="truncate text-sm font-medium" title={k.name}>{k.name ?? '—'}</p>
                    <KeyCell raw={k.key_raw} />
                    <p className="text-xs text-muted-foreground">{formatDateTime(k.created_at)}</p>
                  </div>
                  <div className="flex shrink-0 flex-col items-end gap-1">
                    <StatusBadge status={k.status} />
                    {renderActions(k)}
                  </div>
                </div>
              </Card>
            ))}
          </div>
        </>
      )}

      {/* —— 创建对话框（form → result 两阶段） —— */}
      <Dialog open={createOpen} onOpenChange={o => { if (!o && !create.isPending) { setCreateOpen(false); setCreated(null) } }}>
        <DialogContent className="sm:max-w-md">
          {created ? (
            <>
              <DialogHeader>
                <DialogTitle>{t('managementKeys.createdTitle')}</DialogTitle>
                <DialogDescription>{t('managementKeys.createdDesc')}</DialogDescription>
              </DialogHeader>
              <KeyBox title={t('managementKeys.secretTitle')} value={created.key_raw ?? ''} hint={t('managementKeys.secretHint')} />
              <DialogFooter>
                <Button onClick={() => { setCreateOpen(false); setCreated(null) }}>{t('common.done')}</Button>
              </DialogFooter>
            </>
          ) : (
            <>
              <DialogHeader>
                <DialogTitle>{t('managementKeys.createTitle')}</DialogTitle>
                <DialogDescription>{t('managementKeys.createDesc')}</DialogDescription>
              </DialogHeader>
              <div className="space-y-3">
                <div className="space-y-1.5">
                  <Label htmlFor="mk-cname">{t('managementKeys.nameLabel')}</Label>
                  <Input id="mk-cname" value={createName} placeholder={t('managementKeys.namePlaceholder')} onChange={e => { setCreateName(e.target.value); setCreateErr(null) }} />
                </div>
                {createErr && <p className="text-sm text-destructive">{createErr}</p>}
                {create.isError && errMsg(create.error) && <p className="text-sm text-destructive">{errMsg(create.error)}</p>}
              </div>
              <DialogFooter>
                <Button variant="outline" onClick={() => setCreateOpen(false)} disabled={create.isPending}>{t('common.cancel')}</Button>
                <Button onClick={submitCreate} disabled={create.isPending || !createName.trim()}>
                  {create.isPending ? t('common.creating') : t('managementKeys.new')}
                </Button>
              </DialogFooter>
            </>
          )}
        </DialogContent>
      </Dialog>

      {/* —— 编辑对话框（name/status） —— */}
      <Dialog open={editOpen} onOpenChange={setEditOpen}>
        <DialogContent className="sm:max-w-md">
          <DialogHeader>
            <DialogTitle>{t('managementKeys.editTitle', { id: editing?.id ?? 0 })}</DialogTitle>
            <DialogDescription>{t('managementKeys.editDesc')}</DialogDescription>
          </DialogHeader>
          <div className="space-y-3">
            <div className="space-y-1.5">
              <Label htmlFor="mk-ename">{t('managementKeys.nameLabel')}</Label>
              <Input id="mk-ename" value={editName} onChange={e => { setEditName(e.target.value); setEditErr(null) }} />
            </div>
            <div className="space-y-1.5">
              <Label>{t('managementKeys.statusLabel')}</Label>
              <Select items={Object.fromEntries(STATUSES.map(s => [s, t(`status.${s}`)]))} value={editStatus} onValueChange={v => setEditStatus(v as ManagementKeyStatus)}>
                <SelectTrigger className="w-full"><SelectValue /></SelectTrigger>
                <SelectContent>
                  {STATUSES.map(s => <SelectItem key={s} value={s} label={t(`status.${s}`)}>{t(`status.${s}`)}</SelectItem>)}
                </SelectContent>
              </Select>
              <p className="text-xs text-muted-foreground">{t('managementKeys.statusHint')}</p>
            </div>
            {editErr && <p className="text-sm text-destructive">{editErr}</p>}
            {update.isError && errMsg(update.error) && <p className="text-sm text-destructive">{errMsg(update.error)}</p>}
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setEditOpen(false)} disabled={update.isPending}>{t('common.cancel')}</Button>
            <Button onClick={submitEdit} disabled={update.isPending || !editName.trim()}>
              {update.isPending ? t('common.saving') : t('common.saveChanges')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* —— 删除确认 —— */}
      <Dialog open={!!deleting} onOpenChange={o => { if (!o && !del.isPending) setDeleting(null) }}>
        <DialogContent className="sm:max-w-sm">
          <DialogHeader>
            <DialogTitle>{t('managementKeys.deleteTitle')}</DialogTitle>
            <DialogDescription>{t('managementKeys.deleteDesc', { name: deleting?.name ?? '' })}</DialogDescription>
          </DialogHeader>
          {del.isError && errMsg(del.error) && <p className="text-sm text-destructive">{errMsg(del.error)}</p>}
          <DialogFooter>
            <Button variant="outline" onClick={() => setDeleting(null)} disabled={del.isPending}>{t('common.cancel')}</Button>
            <Button variant="destructive" onClick={() => deleting && del.mutate(deleting.id!)} disabled={del.isPending}>
              {del.isPending ? t('common.deleting') : t('common.delete')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
}
