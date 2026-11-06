// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 出口代理池页面（spec proxy-pool §7）：**管理面与供应商面复用同一组件**，
// 差异只由作用域（lib/api/scope 的 kind）决定——与账号页（pages/accounts.tsx）同款惯例，
// 不复制页面、不建平行字段清单：
// - 管理面（/app/proxies，base /api/admin）：多一列 `owner_uid`（null = 平台代理），
//   删除文案按归属分流（平台代理解绑所有引用；私池代理只解绑该供应商自己的账号）。
// - 供应商面（/user/supplier/proxies，base /api/user/supplier）：没有归属列（都是自己的）、
//   没有「分配给账号」动作；出口地址旁只挂一句**固定说明**（不得用内网/回环/链路本地，
//   后端会拒绝），前端**不判定**某个地址是不是私网——判据只有后端一份（C3）。
//   失败时把后端 400 的 message 原样透出（错误体没有字段级错误清单）。
//
// 三态纪律（与后端同口径，唯一实现在 lib/proxy-form 的 buildProxyPayload）：
// 未触碰的 username/password/enabled/remark 一律不出现在请求体里；「清除」是显式 null。
import { useMemo, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { motion } from 'framer-motion'
import { Plus, Pencil, Trash2, Radar, Lock, Network, CircleCheck, Ban } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import type { DynamicKey } from '@/lib/i18n'
import { useAccountSurface } from '@/lib/api/scope'
import { ApiError, ApiUnauthorized } from '@/lib/api/client'
import {
  PROXY_SCHEMES,
  buildProxyPayload,
  emptyProxyDraft,
  proxyDraftOf,
  proxyEndpoint,
  validateProxyForm,
  type ProxyField,
  type ProxyFormErrors,
  type ProxyView,
} from '@/lib/proxy-form'
import { Badge } from '@/components/ui/badge'
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
import { toast } from '@/components/ui/toast'
import { formatDateTime, truncate } from '@/components/fmt'

export default function ProxyPoolPage() {
  const { kind, api } = useAccountSurface()
  const isSupplier = kind === 'supplier'
  const { t } = useTranslation()
  const qc = useQueryClient()

  // 查询键带作用域：同一浏览器里管理面/供应商面切换不得互相污染缓存。
  const q = useQuery({ queryKey: ['proxies', kind], queryFn: () => api.listProxies() })
  const rows = q.data?.rows ?? []
  const invalidate = () => qc.invalidateQueries({ queryKey: ['proxies', kind] })
  const errMsg = (e: unknown) => (e instanceof ApiUnauthorized ? null : (e as Error)?.message)

  // —— 对话框（创建/编辑共用一个）——
  const [dialogOpen, setDialogOpen] = useState(false)
  const [editing, setEditing] = useState<ProxyView | null>(null)
  const [draft, setDraft] = useState(emptyProxyDraft())
  const [clearPassword, setClearPassword] = useState(false)
  const [clearUsername, setClearUsername] = useState(false)
  const [errors, setErrors] = useState<ProxyFormErrors>({})
  const [deleting, setDeleting] = useState<ProxyView | null>(null)

  const openCreate = () => {
    setEditing(null)
    setDraft(emptyProxyDraft())
    setClearPassword(false)
    setClearUsername(false)
    setErrors({})
    setDialogOpen(true)
  }
  const openEdit = (p: ProxyView) => {
    setEditing(p)
    setDraft(proxyDraftOf(p))
    setClearPassword(false)
    setClearUsername(false)
    setErrors({})
    setDialogOpen(true)
  }

  const save = useMutation({
    mutationFn: () => {
      const payload = buildProxyPayload(editing, draft, { clearPassword, clearUsername })
      return editing ? api.updateProxy(editing.id, payload) : api.createProxy(payload)
    },
    onSuccess: () => {
      void invalidate()
      setDialogOpen(false)
      toast.add({ title: t(editing ? 'proxies.updateSuccess' : 'proxies.createSuccess'), type: 'success' })
    },
  })
  const remove = useMutation({
    mutationFn: (id: number) => api.deleteProxy(id),
    onSuccess: () => {
      void invalidate()
      setDeleting(null)
      toast.add({ title: t('proxies.deleteSuccess'), type: 'success' })
    },
  })
  // 探测：拨不通**不是** API 错误（200 + last_probe_ok=false），所以这里不按失败处理——
  // 只把服务端写回的结论刷新到行内。
  const probe = useMutation({
    mutationFn: (id: number) => api.probeProxy(id),
    onSuccess: () => { void invalidate() },
  })

  const submit = () => {
    const errs = validateProxyForm(draft)
    setErrors(errs)
    if (Object.keys(errs).length > 0) return
    save.mutate()
  }

  const fieldErr = (k: ProxyField) => {
    const code = errors[k]
    if (!code) return null
    return <p className="text-xs text-destructive">{t(`proxies.errors.${code}` as DynamicKey)}</p>
  }
  const schemeItems = useMemo(() => Object.fromEntries(PROXY_SCHEMES.map(s => [s, s])), [])
  const schemaErr = save.isError && save.error instanceof ApiError && save.error.status === 409

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between">
        <div className="min-w-0">
          <h1 className="text-2xl font-semibold tracking-tight">{t('proxies.title')}</h1>
          <p className="text-sm text-muted-foreground text-pretty">
            {t(isSupplier ? 'proxies.subtitleSupplier' : 'proxies.subtitleAdmin')}
          </p>
        </div>
        <Button onClick={openCreate}><Plus /> {t('proxies.new')}</Button>
      </div>

      {q.isError ? (
        <p className="text-sm text-destructive">{t('common.loadFailed', { message: (q.error as Error).message })}</p>
      ) : q.isLoading ? (
        <div className="space-y-2">
          {Array.from({ length: 3 }).map((_, i) => <Skeleton key={i} className="h-12" />)}
        </div>
      ) : rows.length === 0 ? (
        <motion.div initial={{ opacity: 0, y: 12 }} animate={{ opacity: 1, y: 0 }} transition={{ duration: 0.25 }}>
          <Card className="flex flex-col items-center gap-2 py-12 text-muted-foreground">
            <Network className="size-10" />
            <p className="font-medium">{t('proxies.emptyTitle')}</p>
            <p className="text-sm text-center text-pretty">{t(isSupplier ? 'proxies.emptyDescSupplier' : 'proxies.emptyDescAdmin')}</p>
            <Button className="mt-2" onClick={openCreate}><Plus /> {t('proxies.new')}</Button>
          </Card>
        </motion.div>
      ) : (
        <ScrollArea data-od-id="table-scroll-proxies" className="rounded-[14px] border border-[rgba(19,45,83,0.26)] bg-[color:var(--glass-card-light)] shadow-[inset_0_1px_0_rgba(255,255,255,0.5),0_10px_36px_rgba(19,45,83,0.16)] backdrop-blur-[var(--glass-blur)] dark:bg-[color:var(--glass-card-dark)] dark:shadow-[inset_0_1px_0_rgba(255,255,255,0.07),0_10px_36px_rgba(2,6,14,0.5)] dark:border-[rgba(148,180,220,0.32)]" showHorizontal>
          <Table className="min-w-[980px]" containerClassName="overflow-x-visible border-0 shadow-none rounded-none bg-transparent backdrop-blur-none">
            <TableHeader>
              <TableRow>
                <TableHead className="w-12">ID</TableHead>
                <TableHead className="w-48">{t('proxies.table.name')}</TableHead>
                <TableHead className="w-64">{t('proxies.table.endpoint')}</TableHead>
                <TableHead className="w-44">{t('proxies.table.auth')}</TableHead>
                <TableHead className="w-20">{t('proxies.table.enabled')}</TableHead>
                <TableHead className="w-64">{t('proxies.table.probe')}</TableHead>
                {!isSupplier && <TableHead className="w-28">{t('proxies.table.owner')}</TableHead>}
                <TableHead className="w-28 text-right">{t('proxies.table.actions')}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody className="[&_td]:py-3">
              {rows.map(p => (
                <TableRow key={p.id}>
                  <TableCell className="tabular-nums">{p.id}</TableCell>
                  <TableCell className="max-w-48">
                    <div className="truncate" title={p.name}>{p.name}</div>
                    {p.remark ? <div className="truncate text-xs text-muted-foreground" title={p.remark}>{truncate(p.remark, 60)}</div> : null}
                  </TableCell>
                  <TableCell className="font-mono text-xs">{proxyEndpoint(p)}</TableCell>
                  <TableCell>
                    {p.username
                      ? (
                        <span className="inline-flex items-center gap-1.5 text-xs">
                          {p.has_password && <Lock className="size-3.5 text-muted-foreground" aria-label={t('proxies.authPasswordSet')} />}
                          <span className="truncate" title={p.username}>{p.username}</span>
                        </span>
                      )
                      : <span className="text-xs text-muted-foreground">{t('proxies.authNone')}</span>}
                  </TableCell>
                  <TableCell>
                    <span className="inline-flex items-center gap-1.5 text-xs">
                      {p.enabled
                        ? <CircleCheck className="size-4 text-emerald-500" aria-label={t('accounts.state.enabled')} />
                        : <Ban className="size-4 text-muted-foreground" aria-label={t('accounts.state.disabled')} />}
                      {p.enabled ? t('accounts.state.enabled') : t('accounts.state.disabled')}
                    </span>
                  </TableCell>
                  <TableCell className="text-xs">
                    {/* 探测结论只显示在行内：拨不通 = 200 + last_probe_ok=false，不是请求失败 */}
                    {p.last_probe_at == null ? (
                      <span className="text-muted-foreground">{t('proxies.probeNever')}</span>
                    ) : (
                      <div className="space-y-0.5">
                        <div className={p.last_probe_ok ? 'text-emerald-600 dark:text-emerald-400' : 'text-destructive'}>
                          {p.last_probe_ok ? t('proxies.probeOK') : t('proxies.probeFailed')}
                          <span className="ml-1.5 text-muted-foreground">{formatDateTime(p.last_probe_at)}</span>
                        </div>
                        {!p.last_probe_ok && p.last_probe_error && (
                          <div className="truncate text-muted-foreground" title={p.last_probe_error}>{truncate(p.last_probe_error, 80)}</div>
                        )}
                      </div>
                    )}
                  </TableCell>
                  {!isSupplier && (
                    <TableCell className="text-xs">
                      {p.owner_uid == null
                        ? <Badge variant="outline">{t('proxies.ownerPlatform')}</Badge>
                        : <span className="tabular-nums">{p.owner_uid}</span>}
                    </TableCell>
                  )}
                  <TableCell className="text-right">
                    <div className="inline-flex items-center gap-1">
                      <Button variant="ghost" size="icon-sm" title={t('common.edit')} onClick={() => openEdit(p)}>
                        <Pencil />
                      </Button>
                      <Button
                        variant="ghost"
                        size="icon-sm"
                        title={t('proxies.probeAction')}
                        onClick={() => probe.mutate(p.id)}
                        disabled={probe.isPending}
                      >
                        <Radar />
                      </Button>
                      <Button variant="ghost" size="icon-sm" title={t('common.delete')} onClick={() => setDeleting(p)}>
                        <Trash2 className="text-destructive" />
                      </Button>
                    </div>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </ScrollArea>
      )}

      {probe.isError && errMsg(probe.error) && (
        <p className="text-sm text-destructive">{errMsg(probe.error)}</p>
      )}

      {/* —— 创建/编辑对话框 —— */}
      <Dialog open={dialogOpen} onOpenChange={setDialogOpen}>
        <DialogContent className="sm:max-w-md">
          <DialogHeader>
            <DialogTitle>{editing ? t('proxies.editTitle', { id: editing.id }) : t('proxies.newTitle')}</DialogTitle>
            <DialogDescription>{t('proxies.dialogDesc')}</DialogDescription>
          </DialogHeader>
          <div className="space-y-3">
            <div className="space-y-1.5">
              <Label htmlFor="px-name">{t('proxies.nameLabel')}</Label>
              <Input id="px-name" value={draft.name} placeholder={t('proxies.namePlaceholder')} onChange={e => setDraft(d => ({ ...d, name: e.target.value }))} />
              {fieldErr('name')}
            </div>
            <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
              <div className="space-y-1.5">
                <Label>{t('proxies.schemeLabel')}</Label>
                <Select items={schemeItems} value={draft.scheme} onValueChange={v => setDraft(d => ({ ...d, scheme: v }))}>
                  <SelectTrigger className="w-full"><SelectValue /></SelectTrigger>
                  <SelectContent>
                    {PROXY_SCHEMES.map(s => <SelectItem key={s} value={s} label={s}>{s}</SelectItem>)}
                  </SelectContent>
                </Select>
                {fieldErr('scheme')}
              </div>
              <div className="space-y-1.5">
                <Label htmlFor="px-port">{t('proxies.portLabel')}</Label>
                <Input id="px-port" inputMode="numeric" value={draft.port} placeholder="1080" onChange={e => setDraft(d => ({ ...d, port: e.target.value }))} />
                {fieldErr('port')}
              </div>
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="px-host">{t('proxies.hostLabel')}</Label>
              <Input id="px-host" value={draft.host} placeholder={t('proxies.hostPlaceholder')} onChange={e => setDraft(d => ({ ...d, host: e.target.value }))} />
              {fieldErr('host')}
              {/* 固定说明，不随输入变化：地址是否可用由后端判（见下方 save 错误位）。 */}
              {isSupplier && (
                <p className="text-xs text-muted-foreground">{t('proxies.hostPolicyHint')}</p>
              )}
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="px-user">{t('proxies.usernameLabel')}</Label>
              <Input
                id="px-user"
                value={draft.username}
                disabled={clearUsername}
                placeholder={t('proxies.usernamePlaceholder')}
                onChange={e => setDraft(d => ({ ...d, username: e.target.value }))}
              />
              {editing && (
                <label className="flex cursor-pointer items-center gap-2.5 py-0.5">
                  <Checkbox checked={clearUsername} onCheckedChange={c => setClearUsername(c === true)} />
                  <span className="text-sm">{t('proxies.clearUsername')}</span>
                </label>
              )}
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="px-pass">{t('proxies.passwordLabel')}</Label>
              <Input
                id="px-pass"
                type="password"
                value={draft.password}
                disabled={clearPassword}
                placeholder={t('proxies.passwordPlaceholder')}
                onChange={e => setDraft(d => ({ ...d, password: e.target.value }))}
              />
              <p className="text-xs text-muted-foreground">{t(editing && editing.has_password ? 'proxies.passwordKeepHint' : 'proxies.passwordHint')}</p>
              {editing && (
                <label className="flex cursor-pointer items-center gap-2.5 py-0.5">
                  <Checkbox checked={clearPassword} onCheckedChange={c => setClearPassword(c === true)} />
                  <span className="text-sm">{t('proxies.clearPassword')}</span>
                </label>
              )}
            </div>
            <label className="flex cursor-pointer items-center gap-2.5 py-0.5">
              <Checkbox checked={draft.enabled} onCheckedChange={c => setDraft(d => ({ ...d, enabled: c === true }))} />
              <span className="text-sm">{t('proxies.enabledLabel')}</span>
            </label>
            <p className="text-xs text-muted-foreground">{t('proxies.enabledHint')}</p>
            <div className="space-y-1.5">
              <Label htmlFor="px-remark">{t('proxies.remarkLabel')}</Label>
              <Input id="px-remark" value={draft.remark} placeholder={t('proxies.remarkPlaceholder')} onChange={e => setDraft(d => ({ ...d, remark: e.target.value }))} />
            </div>
            {/* 提交失败：409 有专门的冲突文案；其余（含 400 的内网/回环地址拒绝）直接
                展示后端 message —— 错误体没有字段级错误清单，前端不猜是哪个字段。 */}
            {save.isError && (
              <p className="text-sm text-destructive">
                {schemaErr ? t('proxies.nameConflict') : errMsg(save.error)}
              </p>
            )}
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setDialogOpen(false)}>{t('common.cancel')}</Button>
            <Button onClick={submit} disabled={save.isPending}>
              {save.isPending ? t('common.saving') : editing ? t('common.saveChanges') : t('common.create')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* —— 删除确认：文案随归属分流（spec §3.6 的连坐范围） —— */}
      <Dialog open={!!deleting} onOpenChange={o => { if (!o && !remove.isPending) setDeleting(null) }}>
        <DialogContent className="sm:max-w-sm">
          <DialogHeader>
            <DialogTitle>{t('proxies.deleteTitle')}</DialogTitle>
            <DialogDescription>
              {isSupplier || deleting?.owner_uid != null
                ? t('proxies.deleteDescPrivate', { name: deleting?.name ?? '' })
                : t('proxies.deleteDescPlatform', { name: deleting?.name ?? '' })}
            </DialogDescription>
          </DialogHeader>
          {remove.isError && errMsg(remove.error) && (
            <p className="text-sm text-destructive">{errMsg(remove.error)}</p>
          )}
          <DialogFooter>
            <Button variant="outline" onClick={() => setDeleting(null)} disabled={remove.isPending}>{t('common.cancel')}</Button>
            <Button variant="destructive" onClick={() => deleting && remove.mutate(deleting.id)} disabled={remove.isPending}>
              {remove.isPending ? t('common.deleting') : t('common.confirmDelete')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
}
